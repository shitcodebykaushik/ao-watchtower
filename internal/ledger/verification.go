package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/shitcodebykaushik/ao-watchtower/internal/domain"
)

// Verification outcomes. A dispatched fix stays Awaiting until GitHub reports a
// completed check suite for a head commit that is not the one that failed.
const (
	VerificationAwaiting     = "awaiting"
	VerificationGreen        = "verified_green"
	VerificationStillFailing = "still_failing"
	VerificationAbandoned    = "abandoned"
)

// PendingVerification is a dispatched fix whose CI result is not yet known.
type PendingVerification struct {
	TriggerKey        domain.TriggerKey
	Repository        domain.Repository
	PullNumber        int64
	DispatchedHeadSHA string
	StartedAt         time.Time
}

// openVerificationTx derives the pull request under repair from the durable
// check-suite facts already linked to the trigger, so verification needs no
// caller-supplied state that could disagree with the audit trail.
func openVerificationTx(ctx context.Context, tx *sql.Tx, key domain.TriggerKey, at time.Time) error {
	// Inserting nothing is not an error: it means a verification already exists for
	// this trigger, which is the correct outcome for an authorized retry.
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO fix_verifications(trigger_key,owner,repo,pull_number,dispatched_head_sha,outcome,started_at,detail)
SELECT e.trigger_key,f.owner,f.repo,f.pull_number,f.head_sha,?,?,'' FROM evaluations e JOIN check_suite_facts f ON f.delivery_id=e.delivery_id WHERE e.trigger_key=? LIMIT 1`,
		VerificationAwaiting, timestamp(at), key); err != nil {
		return fmt.Errorf("open fix verification: %w", err)
	}
	return nil
}

// PendingVerifications lists dispatched fixes still waiting for a CI result.
func (l *Ledger) PendingVerifications(ctx context.Context) ([]PendingVerification, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT trigger_key,owner,repo,pull_number,dispatched_head_sha,started_at FROM fix_verifications WHERE outcome=? ORDER BY started_at`, VerificationAwaiting)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var pending []PendingVerification
	for rows.Next() {
		var entry PendingVerification
		var owner, repo, started string
		if err := rows.Scan(&entry.TriggerKey, &owner, &repo, &entry.PullNumber, &entry.DispatchedHeadSHA, &started); err != nil {
			return nil, err
		}
		entry.Repository = domain.Repository{Owner: owner, Name: repo}
		if err := entry.Repository.Validate(); err != nil {
			return nil, fmt.Errorf("verification repository: %w", err)
		}
		entry.StartedAt, err = time.Parse(time.RFC3339Nano, started)
		if err != nil {
			return nil, fmt.Errorf("parse verification start time: %w", err)
		}
		pending = append(pending, entry)
	}
	return pending, rows.Err()
}

// ResolveVerification records the terminal CI result for a dispatched fix. It
// only ever moves a verification out of the awaiting state, so a replayed
// observation cannot rewrite a settled fact.
func (l *Ledger) ResolveVerification(ctx context.Context, key domain.TriggerKey, observedHeadSHA, outcome, detail string, at time.Time) error {
	if key == "" || at.IsZero() || !oneOf(outcome, VerificationGreen, VerificationStillFailing, VerificationAbandoned) {
		return fmt.Errorf("invalid verification resolution")
	}
	_, err := l.db.ExecContext(ctx, `UPDATE fix_verifications SET observed_head_sha=?,outcome=?,resolved_at=?,detail=? WHERE trigger_key=? AND outcome=?`,
		nullableString(strings.ToLower(strings.TrimSpace(observedHeadSHA))), outcome, timestamp(at), boundedDetail(detail), key, VerificationAwaiting)
	return err
}

// VerificationOutcome reports the recorded result for one trigger.
func (l *Ledger) VerificationOutcome(ctx context.Context, key domain.TriggerKey) (string, bool, error) {
	var outcome string
	err := l.db.QueryRowContext(ctx, `SELECT outcome FROM fix_verifications WHERE trigger_key=?`, key).Scan(&outcome)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return outcome, err == nil, err
}

// AuthorizeSendRetry permits exactly one further dispatch for a trigger whose
// previous fix send failed. It refuses to supersede a successful send, so an
// approved fix can never be delivered twice.
func (l *Ledger) AuthorizeSendRetry(ctx context.Context, key domain.TriggerKey, actor string, at time.Time) error {
	if key == "" || strings.TrimSpace(actor) == "" || at.IsZero() {
		return fmt.Errorf("trigger key, actor, and authorization time are required")
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var latestID int64
	var latestOutcome string
	err = tx.QueryRowContext(ctx, `SELECT id,outcome FROM send_attempts WHERE trigger_key=? AND outcome<>'blocked_kill_switch' ORDER BY id DESC LIMIT 1`, key).Scan(&latestID, &latestOutcome)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("no dispatch to retry")
	}
	if err != nil {
		return err
	}
	if latestOutcome != "failed" {
		return fmt.Errorf("only a failed dispatch can be retried")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO send_retries(trigger_key,actor,authorized_at,superseded_attempt_id) VALUES(?,?,?,?)`, key, actor, timestamp(at), latestID); err != nil {
		return err
	}
	return tx.Commit()
}

// Stats is the read model behind `watchtower stats` and the dashboard header.
// Every number is derived from durable facts at read time.
type Stats struct {
	Deliveries           int
	Triggers             int
	Spawned              int
	ClaimConflicts       int
	ValidDiagnoses       int
	Approvals            int
	Dispatched           int
	AwaitingVerification int
	VerifiedGreen        int
	StillFailing         int
	Abandoned            int
	MedianTimeToGreen    time.Duration
	AutomationDisabled   bool
}

// AutoFixRate reports the share of investigated failures that reached a
// dispatched repair.
func (s Stats) AutoFixRate() float64 {
	if s.Triggers == 0 {
		return 0
	}
	return float64(s.Dispatched) / float64(s.Triggers)
}

// RepairSuccessRate reports the share of verified repairs that turned CI green.
func (s Stats) RepairSuccessRate() float64 {
	settled := s.VerifiedGreen + s.StillFailing
	if settled == 0 {
		return 0
	}
	return float64(s.VerifiedGreen) / float64(settled)
}

// Stats assembles the aggregate read model from the ledger's durable facts.
func (l *Ledger) Stats(ctx context.Context) (Stats, error) {
	stats := Stats{}
	var err error
	if stats.AutomationDisabled, err = l.AutomationDisabled(ctx); err != nil {
		return stats, err
	}
	counts := []struct {
		target *int
		query  string
		args   []any
	}{
		{&stats.Deliveries, `SELECT count(*) FROM webhook_deliveries`, nil},
		{&stats.Triggers, `SELECT count(*) FROM triggers`, nil},
		{&stats.Spawned, `SELECT count(*) FROM spawn_attempts WHERE outcome='spawned'`, nil},
		{&stats.ClaimConflicts, `SELECT count(*) FROM spawn_attempts WHERE outcome='claim_conflict'`, nil},
		{&stats.ValidDiagnoses, `SELECT count(DISTINCT trigger_key) FROM diagnoses WHERE valid=1`, nil},
		{&stats.Approvals, `SELECT count(DISTINCT trigger_key) FROM human_approvals`, nil},
		{&stats.Dispatched, `SELECT count(DISTINCT trigger_key) FROM send_attempts WHERE outcome='sent'`, nil},
		{&stats.AwaitingVerification, `SELECT count(*) FROM fix_verifications WHERE outcome=?`, []any{VerificationAwaiting}},
		{&stats.VerifiedGreen, `SELECT count(*) FROM fix_verifications WHERE outcome=?`, []any{VerificationGreen}},
		{&stats.StillFailing, `SELECT count(*) FROM fix_verifications WHERE outcome=?`, []any{VerificationStillFailing}},
		{&stats.Abandoned, `SELECT count(*) FROM fix_verifications WHERE outcome=?`, []any{VerificationAbandoned}},
	}
	for _, count := range counts {
		if err := l.db.QueryRowContext(ctx, count.query, count.args...).Scan(count.target); err != nil {
			return stats, err
		}
	}
	stats.MedianTimeToGreen, err = l.medianTimeToGreen(ctx)
	return stats, err
}

// medianTimeToGreen measures from the moment the failing check suite was
// recorded to the moment its repair verified green. The median resists the long
// tail produced by a pull request left open overnight.
func (l *Ledger) medianTimeToGreen(ctx context.Context) (time.Duration, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT w.received_at,v.resolved_at FROM fix_verifications v JOIN evaluations e ON e.trigger_key=v.trigger_key JOIN webhook_deliveries w ON w.delivery_id=e.delivery_id WHERE v.outcome=? AND v.resolved_at IS NOT NULL`, VerificationGreen)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var durations []time.Duration
	for rows.Next() {
		var failedAt, greenAt string
		if err := rows.Scan(&failedAt, &greenAt); err != nil {
			return 0, err
		}
		start, startErr := time.Parse(time.RFC3339Nano, failedAt)
		end, endErr := time.Parse(time.RFC3339Nano, greenAt)
		if startErr != nil || endErr != nil || !end.After(start) {
			continue
		}
		durations = append(durations, end.Sub(start))
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(durations) == 0 {
		return 0, nil
	}
	sort.Slice(durations, func(first, second int) bool { return durations[first] < durations[second] })
	middle := len(durations) / 2
	if len(durations)%2 == 1 {
		return durations[middle], nil
	}
	return (durations[middle-1] + durations[middle]) / 2, nil
}
