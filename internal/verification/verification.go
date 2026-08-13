// Package verification closes the repair loop. Once a fix has been dispatched
// to AO, Watchtower keeps watching the pull request until GitHub reports a
// completed check suite for a head commit other than the one that failed, then
// durably records whether the repair actually worked.
package verification

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/shitcodebykaushik/ao-watchtower/internal/domain"
	"github.com/shitcodebykaushik/ao-watchtower/internal/ledger"
	"github.com/shitcodebykaushik/ao-watchtower/internal/polling"
)

// DefaultTimeout bounds how long a dispatched fix may stay unverified before
// Watchtower stops waiting and records the attempt as abandoned.
const DefaultTimeout = 45 * time.Minute

// Store is the durable subset of the ledger this package needs. Keeping it an
// interface lets the recorder be tested without SQLite.
type Store interface {
	PendingVerifications(context.Context) ([]ledger.PendingVerification, error)
	ResolveVerification(ctx context.Context, key domain.TriggerKey, observedHeadSHA, outcome, detail string, at time.Time) error
}

// Result is one settled verification, handed to the optional notifier.
type Result struct {
	TriggerKey      domain.TriggerKey
	Repository      domain.Repository
	PullNumber      int64
	ObservedHeadSHA string
	Outcome         string
	Detail          string
}

// Notifier receives settled verifications so they can be surfaced outside the
// ledger, for example as a pull request comment. A notifier failure is logged
// and never rolls back the durable record.
type Notifier interface {
	VerificationSettled(context.Context, Result) error
}

// Recorder turns poll observations into durable verification outcomes. It is
// wired as a polling observer so it reuses the poller's existing GitHub reads
// rather than spending additional API budget of its own.
type Recorder struct {
	store    Store
	timeout  time.Duration
	now      func() time.Time
	logger   *log.Logger
	notifier Notifier
}

// Options carries the optional recorder settings.
type Options struct {
	Timeout  time.Duration
	Notifier Notifier
	Logger   *log.Logger
}

func NewRecorder(store Store, options ...Options) (*Recorder, error) {
	if store == nil {
		return nil, fmt.Errorf("verification store is required")
	}
	if len(options) > 1 {
		return nil, fmt.Errorf("only one recorder option is supported")
	}
	var option Options
	if len(options) == 1 {
		option = options[0]
	}
	if option.Timeout == 0 {
		option.Timeout = DefaultTimeout
	}
	if option.Timeout < 0 {
		return nil, fmt.Errorf("verification timeout must not be negative")
	}
	if option.Logger == nil {
		option.Logger = log.Default()
	}
	return &Recorder{store: store, timeout: option.Timeout, now: time.Now, logger: option.Logger, notifier: option.Notifier}, nil
}

// Observe implements polling.Observer. It receives every open pull request the
// poller saw with a completed check rollup, for one repository, each cycle.
func (r *Recorder) Observe(ctx context.Context, repository domain.Repository, observations []polling.Observation) error {
	pending, err := r.store.PendingVerifications(ctx)
	if err != nil {
		return err
	}
	current := make(map[int64]polling.Observation, len(observations))
	for _, observation := range observations {
		current[observation.PullNumber] = observation
	}
	at := r.now()
	for _, entry := range pending {
		if entry.Repository != repository {
			continue
		}
		outcome, observedSHA, detail := classify(entry, current, at, r.timeout)
		if outcome == "" {
			continue
		}
		if err := r.store.ResolveVerification(ctx, entry.TriggerKey, observedSHA, outcome, detail, at); err != nil {
			return err
		}
		r.logger.Printf("Fix verification %s: %s (%s)", entry.TriggerKey, outcome, detail)
		r.notify(ctx, Result{TriggerKey: entry.TriggerKey, Repository: entry.Repository, PullNumber: entry.PullNumber, ObservedHeadSHA: observedSHA, Outcome: outcome, Detail: detail})
	}
	return nil
}

func (r *Recorder) notify(ctx context.Context, result Result) {
	if r.notifier == nil {
		return
	}
	if err := r.notifier.VerificationSettled(ctx, result); err != nil {
		r.logger.Printf("Fix verification notice %s: %v", result.TriggerKey, err)
	}
}

// classify decides the terminal outcome for one pending verification, or
// returns an empty outcome to keep waiting. It is pure so the decision table is
// directly testable.
func classify(entry ledger.PendingVerification, current map[int64]polling.Observation, at time.Time, timeout time.Duration) (string, string, string) {
	observation, open := current[entry.PullNumber]
	expired := timeout > 0 && at.Sub(entry.StartedAt) >= timeout
	if !open {
		// The pull request is closed, merged, or its checks have not finished.
		// None of those prove the repair worked, so only the timeout settles it.
		if expired {
			return ledger.VerificationAbandoned, "", "pull request left the open set before a new check suite completed"
		}
		return "", "", ""
	}
	if strings.EqualFold(observation.HeadSHA, entry.DispatchedHeadSHA) {
		if expired {
			return ledger.VerificationAbandoned, observation.HeadSHA, "no repair commit was pushed before the verification timeout"
		}
		return "", "", ""
	}
	switch observation.Conclusion {
	case "success":
		return ledger.VerificationGreen, observation.HeadSHA, "CI completed successfully on the repair commit"
	case "failure":
		return ledger.VerificationStillFailing, observation.HeadSHA, "CI still failed on the repair commit"
	}
	if expired {
		return ledger.VerificationAbandoned, observation.HeadSHA, "check conclusion never settled before the verification timeout"
	}
	return "", "", ""
}
