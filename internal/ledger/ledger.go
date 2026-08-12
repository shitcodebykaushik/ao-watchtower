// Package ledger persists auditable webhook facts and atomic trigger reservations.
package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/agent-orchestrator/ao-watchtower/internal/domain"
	"github.com/agent-orchestrator/ao-watchtower/internal/policy"
	_ "modernc.org/sqlite"
)

type Ledger struct{ db *sql.DB }

type Result struct {
	Reserved   bool
	TriggerKey domain.TriggerKey
}

func Open(path string) (*Ledger, error) {
	if path == "" {
		return nil, fmt.Errorf("SQLite path is required")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open SQLite ledger: %w", err)
	}
	// A single writer connection makes SQLite reservations deterministic even under concurrent intake.
	db.SetMaxOpenConns(1)
	ledger := &Ledger{db: db}
	if err := ledger.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return ledger, nil
}
func (l *Ledger) Close() error {
	if l == nil || l.db == nil {
		return nil
	}
	return l.db.Close()
}

func (l *Ledger) migrate(ctx context.Context) error {
	_, err := l.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS webhook_deliveries (
 delivery_id TEXT PRIMARY KEY, payload_digest TEXT NOT NULL, received_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS check_suite_facts (
 delivery_id TEXT PRIMARY KEY REFERENCES webhook_deliveries(delivery_id), suite_id INTEGER NOT NULL, owner TEXT NOT NULL, repo TEXT NOT NULL, pull_number INTEGER NOT NULL, head_sha TEXT NOT NULL, conclusion TEXT NOT NULL, details_url TEXT NOT NULL, check_suite_url TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS evaluations (
 delivery_id TEXT PRIMARY KEY REFERENCES webhook_deliveries(delivery_id), rule_id TEXT NOT NULL, outcome TEXT NOT NULL, trigger_key TEXT, ao_project_id TEXT
);
CREATE TABLE IF NOT EXISTS triggers (
 trigger_key TEXT PRIMARY KEY, rule_id TEXT NOT NULL, outcome TEXT NOT NULL, created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS spawn_reservations (
 trigger_key TEXT PRIMARY KEY REFERENCES triggers(trigger_key), ao_project_id TEXT NOT NULL, reserved_at TEXT NOT NULL
);`)
	if err != nil {
		return fmt.Errorf("migrate SQLite ledger: %w", err)
	}
	return nil
}

// RecordEvaluation atomically records a verified delivery and obtains the sole spawn reservation for a trigger.
func (l *Ledger) RecordEvaluation(ctx context.Context, delivery domain.WebhookDelivery, facts domain.CheckSuiteFacts, evaluation policy.Evaluation) (Result, error) {
	if l == nil || l.db == nil {
		return Result{}, fmt.Errorf("ledger is nil")
	}
	if delivery.ID == "" || delivery.PayloadDigest == "" || delivery.ReceivedAt.IsZero() {
		return Result{}, fmt.Errorf("complete webhook delivery is required")
	}
	if err := facts.Validate(); err != nil {
		return Result{}, err
	}
	if evaluation.RuleID != domain.InvestigateCIFailureRule {
		return Result{}, fmt.Errorf("unsupported rule")
	}
	if evaluation.Outcome == policy.OutcomeReserved && (evaluation.TriggerKey == "" || evaluation.AOProjectID == "") {
		return Result{}, fmt.Errorf("reserved evaluation requires trigger key and AO project")
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, err
	}
	defer tx.Rollback()
	var digest string
	err = tx.QueryRowContext(ctx, `SELECT payload_digest FROM webhook_deliveries WHERE delivery_id=?`, delivery.ID).Scan(&digest)
	if err == nil {
		if digest != delivery.PayloadDigest {
			return Result{}, fmt.Errorf("webhook delivery id conflicts with a different payload")
		}
		var key sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT trigger_key FROM evaluations WHERE delivery_id=?`, delivery.ID).Scan(&key); err != nil {
			return Result{}, fmt.Errorf("load existing evaluation: %w", err)
		}
		return Result{TriggerKey: domain.TriggerKey(key.String)}, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Result{}, err
	}
	at := delivery.ReceivedAt.UTC().Format(time.RFC3339Nano)
	if _, err = tx.ExecContext(ctx, `INSERT INTO webhook_deliveries(delivery_id,payload_digest,received_at) VALUES(?,?,?)`, delivery.ID, delivery.PayloadDigest, at); err != nil {
		return Result{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO check_suite_facts(delivery_id,suite_id,owner,repo,pull_number,head_sha,conclusion,details_url,check_suite_url) VALUES(?,?,?,?,?,?,?,?,?)`, delivery.ID, facts.ProviderID, facts.Repository.Owner, facts.Repository.Name, facts.PullNumber, facts.HeadSHA, facts.Conclusion, facts.DetailsURL, facts.CheckSuiteURL); err != nil {
		return Result{}, err
	}
	var reserved bool
	if evaluation.Outcome == policy.OutcomeReserved {
		if _, err = tx.ExecContext(ctx, `INSERT INTO triggers(trigger_key,rule_id,outcome,created_at) VALUES(?,?,?,?) ON CONFLICT(trigger_key) DO NOTHING`, evaluation.TriggerKey, evaluation.RuleID, evaluation.Outcome, at); err != nil {
			return Result{}, err
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO spawn_reservations(trigger_key,ao_project_id,reserved_at) VALUES(?,?,?) ON CONFLICT(trigger_key) DO NOTHING`, evaluation.TriggerKey, evaluation.AOProjectID, at)
		if err != nil {
			return Result{}, err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return Result{}, err
		}
		reserved = rows == 1
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO evaluations(delivery_id,rule_id,outcome,trigger_key,ao_project_id) VALUES(?,?,?,?,?)`, delivery.ID, evaluation.RuleID, evaluation.Outcome, nullableString(string(evaluation.TriggerKey)), nullableString(evaluation.AOProjectID)); err != nil {
		return Result{}, err
	}
	if err = tx.Commit(); err != nil {
		return Result{}, err
	}
	return Result{Reserved: reserved, TriggerKey: evaluation.TriggerKey}, nil
}
func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// Count is a read-only test and dashboard support query for durable fact counts.
func (l *Ledger) Count(ctx context.Context, table string) (int, error) {
	switch table {
	case "webhook_deliveries", "spawn_reservations":
	default:
		return 0, fmt.Errorf("unsupported ledger table")
	}
	var count int
	err := l.db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&count)
	return count, err
}
