// Package notify publishes Watchtower's findings where the people who care
// about them already are: on the pull request. A diagnosis that only exists on
// a localhost dashboard is invisible to reviewers, and the verification result
// is the answer to the only question that matters — did the repair work.
package notify

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/shitcodebykaushik/ao-watchtower/internal/domain"
	"github.com/shitcodebykaushik/ao-watchtower/internal/ledger"
	"github.com/shitcodebykaushik/ao-watchtower/internal/prcomment"
	"github.com/shitcodebykaushik/ao-watchtower/internal/verification"
)

// DefaultInterval is how often pending diagnoses are published.
const DefaultInterval = 10 * time.Second

// Publisher is the pull request comment surface. Keeping it an interface lets
// the controller be tested without invoking the GitHub CLI.
type Publisher interface {
	Upsert(context.Context, domain.Repository, int64, prcomment.Body) error
}

// Store is the durable subset of the ledger the controller reads.
type Store interface {
	Dashboard(context.Context) (ledger.Dashboard, error)
}

type Controller struct {
	store     Store
	publisher Publisher
	mode      string
	interval  time.Duration
	logger    *log.Logger
	published map[string]bool
}

// Options carries the optional controller settings.
type Options struct {
	Interval time.Duration
	Logger   *log.Logger
}

// New builds a controller. Mode is the operating mode shown in the comment so a
// reader can tell an automatic repair from one a human approved.
func New(store Store, publisher Publisher, mode string, options ...Options) (*Controller, error) {
	if store == nil || publisher == nil {
		return nil, fmt.Errorf("notify store and publisher are required")
	}
	if mode == "" {
		return nil, fmt.Errorf("operating mode is required")
	}
	if len(options) > 1 {
		return nil, fmt.Errorf("only one notify option is supported")
	}
	var option Options
	if len(options) == 1 {
		option = options[0]
	}
	if option.Interval == 0 {
		option.Interval = DefaultInterval
	}
	if option.Interval < 0 {
		return nil, fmt.Errorf("notify interval must be positive")
	}
	if option.Logger == nil {
		option.Logger = log.Default()
	}
	return &Controller{store: store, publisher: publisher, mode: mode, interval: option.Interval, logger: option.Logger, published: make(map[string]bool)}, nil
}

func (c *Controller) Run(ctx context.Context) {
	c.runOnce(ctx)
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.runOnce(ctx)
		}
	}
}

// RunOnce publishes a comment for every validated diagnosis not yet published
// in this process. Republishing is harmless because Upsert edits its own
// comment in place, but the in-memory set keeps the GitHub call rate low.
func (c *Controller) RunOnce(ctx context.Context) error {
	dashboard, err := c.store.Dashboard(ctx)
	if err != nil {
		return err
	}
	for _, row := range dashboard.Rows {
		if row.TriggerKey == "" || row.Diagnosis == "" || c.published["diagnosis:"+row.TriggerKey] {
			continue
		}
		diagnosis, ok := decodeDiagnosis(row.Diagnosis)
		if !ok {
			continue
		}
		repository, err := domain.ParseRepository(row.Repository)
		if err != nil {
			continue
		}
		key := domain.TriggerKey(row.TriggerKey)
		if err := c.publisher.Upsert(ctx, repository, row.PullNumber, prcomment.RenderDiagnosis(key, diagnosis, row.HeadSHA, c.mode)); err != nil {
			return fmt.Errorf("publish diagnosis %s: %w", row.TriggerKey, err)
		}
		c.published["diagnosis:"+row.TriggerKey] = true
	}
	return nil
}

// VerificationSettled implements verification.Notifier. Only a settled repair
// is reported, so a pull request never receives a comment saying a fix is still
// being watched.
func (c *Controller) VerificationSettled(ctx context.Context, result verification.Result) error {
	marker := "outcome:" + string(result.TriggerKey)
	if c.published[marker] {
		return nil
	}
	verified := result.Outcome == ledger.VerificationGreen
	body := prcomment.RenderOutcome(result.TriggerKey, verified, result.ObservedHeadSHA, result.Detail)
	if err := c.publisher.Upsert(ctx, result.Repository, result.PullNumber, body); err != nil {
		return fmt.Errorf("publish outcome %s: %w", result.TriggerKey, err)
	}
	c.published[marker] = true
	return nil
}

func (c *Controller) runOnce(ctx context.Context) {
	if err := c.RunOnce(ctx); err != nil && ctx.Err() == nil {
		c.logger.Printf("Pull request comment: %v", err)
	}
}
