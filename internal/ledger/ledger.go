// Package ledger persists auditable webhook facts and lifecycle outcomes.
package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shitcodebykaushik/ao-watchtower/internal/domain"
	"github.com/shitcodebykaushik/ao-watchtower/internal/policy"
	_ "modernc.org/sqlite"
)

const MaxDiagnosisRaw = 64 << 10

type Ledger struct{ db *sql.DB }

type Result struct {
	Reserved    bool
	TriggerKey  domain.TriggerKey
	AOProjectID string
}

type SpawnStart struct {
	Started bool
	Blocked bool
}

type SendStart struct {
	Started bool
	Blocked bool
}

func Open(path string) (*Ledger, error) {
	if path == "" {
		return nil, fmt.Errorf("SQLite path is required")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open SQLite ledger: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("enable SQLite foreign keys: %w", err)
	}
	// `watchtower stats` opens the same file a running monitor is writing to.
	// Without write-ahead logging and a busy timeout, either process can fail
	// with "database is locked" — including the monitor, which would drop a
	// durable audit fact because someone asked for a read-only report.
	var journalMode string
	if err := db.QueryRow(`PRAGMA journal_mode = WAL`).Scan(&journalMode); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("enable SQLite write-ahead logging: %w", err)
	}
	if _, err := db.Exec(`PRAGMA busy_timeout = 5000`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("set SQLite busy timeout: %w", err)
	}
	ledger := &Ledger{db: db}
	if err := ledger.migrate(context.Background()); err != nil {
		_ = db.Close()
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

// migrate is deliberately additive. Existing facts are never rewritten or
// discarded; CREATE IF NOT EXISTS makes every application restart safe.
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
);
CREATE TABLE IF NOT EXISTS automation_settings (
 singleton INTEGER PRIMARY KEY CHECK(singleton = 1), disabled INTEGER NOT NULL CHECK(disabled IN (0, 1)), changed_at TEXT NOT NULL, actor TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS spawn_attempts (
 trigger_key TEXT PRIMARY KEY REFERENCES triggers(trigger_key), ao_project_id TEXT NOT NULL, started_at TEXT NOT NULL, completed_at TEXT, outcome TEXT NOT NULL, ao_session_id TEXT, detail TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS diagnoses (
 id INTEGER PRIMARY KEY AUTOINCREMENT, trigger_key TEXT NOT NULL REFERENCES triggers(trigger_key), raw BLOB NOT NULL, raw_truncated INTEGER NOT NULL CHECK(raw_truncated IN (0, 1)), structured_json BLOB, valid INTEGER NOT NULL CHECK(valid IN (0, 1)), recorded_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS diagnoses_by_trigger ON diagnoses(trigger_key, id DESC);
CREATE TABLE IF NOT EXISTS human_approvals (
 id INTEGER PRIMARY KEY AUTOINCREMENT, trigger_key TEXT NOT NULL REFERENCES triggers(trigger_key), actor TEXT NOT NULL, approved_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS approvals_by_trigger ON human_approvals(trigger_key, id DESC);
CREATE TABLE IF NOT EXISTS send_attempts (
 id INTEGER PRIMARY KEY AUTOINCREMENT, trigger_key TEXT NOT NULL REFERENCES triggers(trigger_key), ao_session_id TEXT NOT NULL, started_at TEXT NOT NULL, completed_at TEXT, outcome TEXT NOT NULL, detail TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS fix_verifications (
 trigger_key TEXT PRIMARY KEY REFERENCES triggers(trigger_key), owner TEXT NOT NULL, repo TEXT NOT NULL, pull_number INTEGER NOT NULL, dispatched_head_sha TEXT NOT NULL, observed_head_sha TEXT, outcome TEXT NOT NULL, started_at TEXT NOT NULL, resolved_at TEXT, detail TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS verifications_by_outcome ON fix_verifications(outcome);
CREATE TABLE IF NOT EXISTS send_retries (
 id INTEGER PRIMARY KEY AUTOINCREMENT, trigger_key TEXT NOT NULL REFERENCES triggers(trigger_key), actor TEXT NOT NULL, authorized_at TEXT NOT NULL, superseded_attempt_id INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS retries_by_trigger ON send_retries(trigger_key, id DESC);`)
	if err != nil {
		return fmt.Errorf("migrate SQLite ledger: %w", err)
	}
	return nil
}

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
		var project sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT trigger_key,ao_project_id FROM evaluations WHERE delivery_id=?`, delivery.ID).Scan(&key, &project); err != nil {
			return Result{}, fmt.Errorf("load existing evaluation: %w", err)
		}
		return Result{TriggerKey: domain.TriggerKey(key.String), AOProjectID: project.String}, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Result{}, err
	}
	at := timestamp(delivery.ReceivedAt)
	if _, err = tx.ExecContext(ctx, `INSERT INTO webhook_deliveries(delivery_id,payload_digest,received_at) VALUES(?,?,?)`, delivery.ID, delivery.PayloadDigest, at); err != nil {
		return Result{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO check_suite_facts(delivery_id,suite_id,owner,repo,pull_number,head_sha,conclusion,details_url,check_suite_url) VALUES(?,?,?,?,?,?,?,?,?)`, delivery.ID, facts.ProviderID, facts.Repository.Owner, facts.Repository.Name, facts.PullNumber, facts.HeadSHA, facts.Conclusion, facts.DetailsURL, facts.CheckSuiteURL); err != nil {
		return Result{}, err
	}
	reserved := false
	if evaluation.Outcome == policy.OutcomeReserved {
		if _, err = tx.ExecContext(ctx, `INSERT INTO triggers(trigger_key,rule_id,outcome,created_at) VALUES(?,?,?,?) ON CONFLICT(trigger_key) DO NOTHING`, evaluation.TriggerKey, evaluation.RuleID, evaluation.Outcome, at); err != nil {
			return Result{}, err
		}
		result, execErr := tx.ExecContext(ctx, `INSERT INTO spawn_reservations(trigger_key,ao_project_id,reserved_at) VALUES(?,?,?) ON CONFLICT(trigger_key) DO NOTHING`, evaluation.TriggerKey, evaluation.AOProjectID, at)
		if execErr != nil {
			return Result{}, execErr
		}
		rows, execErr := result.RowsAffected()
		if execErr != nil {
			return Result{}, execErr
		}
		reserved = rows == 1
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO evaluations(delivery_id,rule_id,outcome,trigger_key,ao_project_id) VALUES(?,?,?,?,?)`, delivery.ID, evaluation.RuleID, evaluation.Outcome, nullableString(string(evaluation.TriggerKey)), nullableString(evaluation.AOProjectID)); err != nil {
		return Result{}, err
	}
	if err = tx.Commit(); err != nil {
		return Result{}, err
	}
	return Result{Reserved: reserved, TriggerKey: evaluation.TriggerKey, AOProjectID: evaluation.AOProjectID}, nil
}

func (l *Ledger) SetAutomationDisabled(ctx context.Context, disabled bool, actor string, at time.Time) error {
	if strings.TrimSpace(actor) == "" || at.IsZero() {
		return fmt.Errorf("actor and change time are required")
	}
	_, err := l.db.ExecContext(ctx, `INSERT INTO automation_settings(singleton,disabled,changed_at,actor) VALUES(1,?,?,?) ON CONFLICT(singleton) DO UPDATE SET disabled=excluded.disabled,changed_at=excluded.changed_at,actor=excluded.actor`, boolInt(disabled), timestamp(at), actor)
	return err
}
func (l *Ledger) AutomationDisabled(ctx context.Context) (bool, error) {
	var disabled int
	err := l.db.QueryRowContext(ctx, `SELECT disabled FROM automation_settings WHERE singleton=1`).Scan(&disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return disabled == 1, err
}

// StartSpawnAttempt atomically checks the kill switch and reserves exactly one
// external spawn mutation for a trigger, including across restarts.
func (l *Ledger) StartSpawnAttempt(ctx context.Context, key domain.TriggerKey, projectID string, at time.Time) (SpawnStart, error) {
	if key == "" || strings.TrimSpace(projectID) == "" || at.IsZero() {
		return SpawnStart{}, fmt.Errorf("trigger key, AO project id, and time are required")
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return SpawnStart{}, err
	}
	defer tx.Rollback()
	disabled, err := automationDisabledTx(ctx, tx)
	if err != nil {
		return SpawnStart{}, err
	}
	if disabled {
		err = insertSpawnAttempt(ctx, tx, key, projectID, at, "blocked_kill_switch", "automation disabled", "")
		if err != nil && !isUnique(err) {
			return SpawnStart{}, err
		}
		if err = tx.Commit(); err != nil {
			return SpawnStart{}, err
		}
		return SpawnStart{Blocked: true}, nil
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO spawn_attempts(trigger_key,ao_project_id,started_at,outcome,detail) VALUES(?,?,?,?,?) ON CONFLICT(trigger_key) DO NOTHING`, key, projectID, timestamp(at), "started", "")
	if err != nil {
		return SpawnStart{}, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return SpawnStart{}, err
	}
	if err = tx.Commit(); err != nil {
		return SpawnStart{}, err
	}
	return SpawnStart{Started: rows == 1}, nil
}
func (l *Ledger) CompleteSpawnAttempt(ctx context.Context, key domain.TriggerKey, outcome, sessionID, detail string, at time.Time) error {
	if key == "" || at.IsZero() || !oneOf(outcome, "spawned", "claim_conflict", "failed") {
		return fmt.Errorf("invalid spawn completion")
	}
	result, err := l.db.ExecContext(ctx, `UPDATE spawn_attempts SET completed_at=?,outcome=?,ao_session_id=?,detail=? WHERE trigger_key=? AND outcome='started'`, timestamp(at), outcome, nullableString(sessionID), boundedDetail(detail), key)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("spawn attempt is not in progress")
	}
	return nil
}
func (l *Ledger) SessionForTrigger(ctx context.Context, key domain.TriggerKey) (string, bool, error) {
	var id sql.NullString
	err := l.db.QueryRowContext(ctx, `SELECT ao_session_id FROM spawn_attempts WHERE trigger_key=? AND outcome='spawned'`, key).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return id.String, err == nil && id.Valid, err
}

func (l *Ledger) SpawnAttemptForTrigger(ctx context.Context, key domain.TriggerKey) (domain.SpawnAttempt, bool, error) {
	var project, started string
	var completed, session sql.NullString
	var outcome string
	err := l.db.QueryRowContext(ctx, `SELECT ao_project_id,started_at,completed_at,outcome,ao_session_id FROM spawn_attempts WHERE trigger_key=?`, key).Scan(&project, &started, &completed, &outcome, &session)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.SpawnAttempt{}, false, nil
	}
	if err != nil {
		return domain.SpawnAttempt{}, false, err
	}
	startedAt, err := time.Parse(time.RFC3339Nano, started)
	if err != nil {
		return domain.SpawnAttempt{}, false, fmt.Errorf("parse spawn start time: %w", err)
	}
	attempt := domain.SpawnAttempt{TriggerKey: key, AOProjectID: project, AOSessionID: session.String, StartedAt: startedAt, Outcome: outcome}
	if completed.Valid {
		attempt.CompletedAt, err = time.Parse(time.RFC3339Nano, completed.String)
		if err != nil {
			return domain.SpawnAttempt{}, false, fmt.Errorf("parse spawn completion time: %w", err)
		}
	}
	return attempt, true, nil
}

func (l *Ledger) RecordDiagnosis(ctx context.Context, key domain.TriggerKey, raw []byte, diagnosis *domain.Diagnosis, valid bool, at time.Time) error {
	if key == "" || at.IsZero() {
		return fmt.Errorf("trigger key and record time are required")
	}
	if valid && (diagnosis == nil || diagnosis.Validate() != nil) {
		return fmt.Errorf("valid diagnosis is required")
	}
	if !valid {
		diagnosis = nil
	}
	raw, truncated := boundRaw(raw)
	var structured any
	if diagnosis != nil {
		encoded, err := json.Marshal(diagnosis)
		if err != nil {
			return err
		}
		structured = encoded
	}
	_, err := l.db.ExecContext(ctx, `INSERT INTO diagnoses(trigger_key,raw,raw_truncated,structured_json,valid,recorded_at) VALUES(?,?,?,?,?,?)`, key, raw, boolInt(truncated), structured, boolInt(valid), timestamp(at))
	return err
}
func (l *Ledger) LatestValidDiagnosis(ctx context.Context, key domain.TriggerKey) (domain.Diagnosis, bool, error) {
	var encoded []byte
	err := l.db.QueryRowContext(ctx, `SELECT structured_json FROM diagnoses WHERE trigger_key=? AND valid=1 ORDER BY id DESC LIMIT 1`, key).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Diagnosis{}, false, nil
	}
	if err != nil {
		return domain.Diagnosis{}, false, err
	}
	var diagnosis domain.Diagnosis
	if err := json.Unmarshal(encoded, &diagnosis); err != nil || diagnosis.Validate() != nil {
		return domain.Diagnosis{}, false, fmt.Errorf("stored diagnosis is invalid")
	}
	return diagnosis, true, nil
}

// LatestDiagnosisRecord returns the most recently submitted raw evidence and
// whether it was usable. It is intentionally read-only.
func (l *Ledger) LatestDiagnosisRecord(ctx context.Context, key domain.TriggerKey) (domain.DiagnosisRecord, bool, error) {
	var raw, structured []byte
	var valid int
	var recorded string
	err := l.db.QueryRowContext(ctx, `SELECT raw,structured_json,valid,recorded_at FROM diagnoses WHERE trigger_key=? ORDER BY id DESC LIMIT 1`, key).Scan(&raw, &structured, &valid, &recorded)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.DiagnosisRecord{}, false, nil
	}
	if err != nil {
		return domain.DiagnosisRecord{}, false, err
	}
	at, err := time.Parse(time.RFC3339Nano, recorded)
	if err != nil {
		return domain.DiagnosisRecord{}, false, fmt.Errorf("parse diagnosis time: %w", err)
	}
	record := domain.DiagnosisRecord{TriggerKey: key, Raw: raw, Valid: valid == 1, RecordedAt: at}
	if record.Valid {
		var diagnosis domain.Diagnosis
		if err := json.Unmarshal(structured, &diagnosis); err != nil || diagnosis.Validate() != nil {
			return domain.DiagnosisRecord{}, false, fmt.Errorf("stored diagnosis is invalid")
		}
		record.Diagnosis = &diagnosis
	}
	return record, true, nil
}
func (l *Ledger) RecordHumanApproval(ctx context.Context, key domain.TriggerKey, actor string, at time.Time) error {
	if key == "" || strings.TrimSpace(actor) == "" || at.IsZero() {
		return fmt.Errorf("trigger key, actor, and approval time are required")
	}
	_, err := l.db.ExecContext(ctx, `INSERT INTO human_approvals(trigger_key,actor,approved_at) VALUES(?,?,?)`, key, actor, timestamp(at))
	return err
}
func (l *Ledger) HasHumanApproval(ctx context.Context, key domain.TriggerKey) (bool, error) {
	var one int
	err := l.db.QueryRowContext(ctx, `SELECT 1 FROM human_approvals WHERE trigger_key=? LIMIT 1`, key).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// StartSendAttempt repeats the kill-switch check inside the durable attempt
// transaction, immediately before the caller invokes AO send.
func (l *Ledger) StartSendAttempt(ctx context.Context, key domain.TriggerKey, sessionID string, at time.Time) (SendStart, error) {
	if key == "" || strings.TrimSpace(sessionID) == "" || at.IsZero() {
		return SendStart{}, fmt.Errorf("trigger key, AO session id, and time are required")
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return SendStart{}, err
	}
	defer tx.Rollback()
	// A send attempt after the most recent retry authorization blocks another
	// one. Attempts recorded before that authorization are deliberately
	// superseded so an operator can retry a failed dispatch without losing the
	// original audit row.
	var existing string
	err = tx.QueryRowContext(ctx, `SELECT outcome FROM send_attempts WHERE trigger_key=? AND outcome<>'blocked_kill_switch' AND id > COALESCE((SELECT superseded_attempt_id FROM send_retries WHERE trigger_key=? ORDER BY id DESC LIMIT 1),0) ORDER BY id DESC LIMIT 1`, key, key).Scan(&existing)
	if err == nil {
		if err = tx.Commit(); err != nil {
			return SendStart{}, err
		}
		return SendStart{}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return SendStart{}, err
	}
	disabled, err := automationDisabledTx(ctx, tx)
	if err != nil {
		return SendStart{}, err
	}
	outcome, detail := "started", ""
	if disabled {
		outcome, detail = "blocked_kill_switch", "automation disabled"
	}
	completedAt := ""
	if disabled {
		completedAt = timestamp(at)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO send_attempts(trigger_key,ao_session_id,started_at,completed_at,outcome,detail) VALUES(?,?,?,?,?,?)`, key, sessionID, timestamp(at), nullableString(completedAt), outcome, detail); err != nil {
		return SendStart{}, err
	}
	if err = tx.Commit(); err != nil {
		return SendStart{}, err
	}
	return SendStart{Started: !disabled, Blocked: disabled}, nil
}

// CompleteSendAttempt closes the in-progress dispatch and, when the fix
// actually reached AO, opens the verification that will later record whether CI
// went green. Both facts commit together so a dispatched fix is never left
// unwatched.
func (l *Ledger) CompleteSendAttempt(ctx context.Context, key domain.TriggerKey, sessionID, outcome, detail string, at time.Time) error {
	if key == "" || sessionID == "" || at.IsZero() || !oneOf(outcome, "sent", "failed") {
		return fmt.Errorf("invalid send completion")
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE send_attempts SET completed_at=?,outcome=?,detail=? WHERE id=(SELECT id FROM send_attempts WHERE trigger_key=? AND ao_session_id=? AND outcome='started' ORDER BY id DESC LIMIT 1)`, timestamp(at), outcome, boundedDetail(detail), key, sessionID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("send attempt is not in progress")
	}
	if outcome == "sent" {
		if err := openVerificationTx(ctx, tx, key, at); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func automationDisabledTx(ctx context.Context, tx *sql.Tx) (bool, error) {
	var disabled int
	err := tx.QueryRowContext(ctx, `SELECT disabled FROM automation_settings WHERE singleton=1`).Scan(&disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return disabled == 1, err
}
func insertSpawnAttempt(ctx context.Context, tx *sql.Tx, key domain.TriggerKey, project string, at time.Time, outcome, detail, session string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO spawn_attempts(trigger_key,ao_project_id,started_at,completed_at,outcome,ao_session_id,detail) VALUES(?,?,?,?,?,?,?)`, key, project, timestamp(at), timestamp(at), outcome, nullableString(session), detail)
	return err
}
func timestamp(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }
func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
func boundRaw(value []byte) ([]byte, bool) {
	if len(value) <= MaxDiagnosisRaw {
		return append([]byte(nil), value...), false
	}
	return append([]byte(nil), value[:MaxDiagnosisRaw]...), true
}
func boundedDetail(value string) string {
	const max = 4096
	if len(value) > max {
		return value[:max]
	}
	return value
}
func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}
func isUnique(err error) bool { return strings.Contains(err.Error(), "UNIQUE constraint failed") }

// Count is a read-only test and future dashboard support query for durable facts.
func (l *Ledger) Count(ctx context.Context, table string) (int, error) {
	switch table {
	case "webhook_deliveries", "triggers", "spawn_reservations", "spawn_attempts", "diagnoses", "human_approvals", "send_attempts":
	default:
		return 0, fmt.Errorf("unsupported ledger table")
	}
	var count int
	err := l.db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&count)
	return count, err
}

// DashboardRow is a read model assembled solely from durable ledger facts.
type DashboardRow struct {
	TriggerKey, Repository, HeadSHA, Evaluation, ProjectID, SessionID, SpawnOutcome, Diagnosis, Approval, SendOutcome string
	// Conclusion is the reported check-suite result, DetailsURL links to the
	// provider run, and Verification carries the post-repair CI outcome. They
	// let the dashboard answer "why did this fail" and "did the fix work"
	// without leaving the page.
	Conclusion, DetailsURL, Verification, SendDetail string
	PullNumber                                       int64
	CreatedAt                                        time.Time
	// SpawnCompletedAt is zero until AO answered the spawn. Staleness is derived
	// from it at read time rather than stored as its own source of truth.
	SpawnCompletedAt time.Time
}
type Dashboard struct {
	AutomationDisabled                                bool
	Deliveries, Triggers, Diagnoses, Approvals, Sends int
	Rows                                              []DashboardRow
}

func (l *Ledger) Dashboard(ctx context.Context) (Dashboard, error) {
	d := Dashboard{}
	var err error
	if d.AutomationDisabled, err = l.AutomationDisabled(ctx); err != nil {
		return d, err
	}
	for _, x := range []struct {
		n *int
		t string
	}{{&d.Deliveries, "webhook_deliveries"}, {&d.Triggers, "triggers"}, {&d.Diagnoses, "diagnoses"}, {&d.Approvals, "human_approvals"}, {&d.Sends, "send_attempts"}} {
		if *x.n, err = l.Count(ctx, x.t); err != nil {
			return d, err
		}
	}
	rows, err := l.db.QueryContext(ctx, `SELECT COALESCE(e.trigger_key,''),f.owner||'/'||f.repo,f.pull_number,f.head_sha,f.conclusion,f.details_url,e.outcome,COALESCE(e.ao_project_id,''),COALESCE(s.ao_session_id,''),COALESCE(s.outcome,''),s.completed_at,
COALESCE((SELECT structured_json FROM diagnoses d WHERE d.trigger_key=e.trigger_key AND d.valid=1 ORDER BY d.id DESC LIMIT 1),''),
COALESCE((SELECT actor FROM human_approvals a WHERE a.trigger_key=e.trigger_key ORDER BY a.id DESC LIMIT 1),''),
COALESCE((SELECT outcome FROM send_attempts z WHERE z.trigger_key=e.trigger_key ORDER BY z.id DESC LIMIT 1),''),
COALESCE((SELECT detail FROM send_attempts z WHERE z.trigger_key=e.trigger_key ORDER BY z.id DESC LIMIT 1),''),
COALESCE((SELECT outcome FROM fix_verifications v WHERE v.trigger_key=e.trigger_key),''),
w.received_at FROM evaluations e JOIN check_suite_facts f ON f.delivery_id=e.delivery_id JOIN webhook_deliveries w ON w.delivery_id=e.delivery_id LEFT JOIN spawn_attempts s ON s.trigger_key=e.trigger_key ORDER BY w.received_at DESC LIMIT 100`)
	if err != nil {
		return d, err
	}
	defer rows.Close()
	for rows.Next() {
		var r DashboardRow
		var created string
		var spawnCompleted sql.NullString
		if err = rows.Scan(&r.TriggerKey, &r.Repository, &r.PullNumber, &r.HeadSHA, &r.Conclusion, &r.DetailsURL, &r.Evaluation, &r.ProjectID, &r.SessionID, &r.SpawnOutcome, &spawnCompleted, &r.Diagnosis, &r.Approval, &r.SendOutcome, &r.SendDetail, &r.Verification, &created); err != nil {
			return d, err
		}
		r.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
		if err != nil {
			return d, err
		}
		if spawnCompleted.Valid {
			r.SpawnCompletedAt, err = time.Parse(time.RFC3339Nano, spawnCompleted.String)
			if err != nil {
				return d, err
			}
		}
		d.Rows = append(d.Rows, r)
	}
	return d, rows.Err()
}
