---
name: watchtower-ledger-migration
description: Safe procedure for changing the SQLite ledger schema in internal/ledger — adding a table, adding a column, or extending the Dashboard/Stats read models. Use whenever a change touches migrate(), adds a durable fact, or would alter an existing CREATE TABLE statement. A user's watchtower.db holds audit facts that must survive every upgrade.
---

# Watchtower ledger migration

`migrate()` in `internal/ledger/ledger.go` is deliberately additive and
`CREATE ... IF NOT EXISTS`-only. It runs inside `Open()` on every process start,
against a database that lives with the user at
`<user config dir>/ao-watchtower/<owner>/<repo>/watchtower.db` (created by
`onboarding.Setup`, documented in `README.md` under "Local state"). That file is the
audit record of every event, spawn, diagnosis, approval, send, and verification. It is
never regenerated.

Line numbers are omitted throughout: this tree is under active development. Locate a
symbol with `rg -n '<symbol>' internal/ledger`.

## Non-negotiable rules

1. **Additive only.** Append new statements to the end of the batch string in
   `migrate()`. Never edit the body of an existing `CREATE TABLE`. SQLite does not
   re-apply a changed `CREATE TABLE IF NOT EXISTS` to a database that already has the
   table, so an edited statement produces a binary that disagrees silently with every
   existing file.
2. **Never rewrite or discard facts.** No `DROP`, no `DELETE`, no `INSERT OR REPLACE`
   over a recorded fact, no `UPDATE` that overwrites a settled outcome. The package's
   `UPDATE`s are all narrowly guarded: `CompleteSpawnAttempt` and `CompleteSendAttempt`
   require `outcome='started'` and exactly one affected row, and
   `ResolveVerification` only ever moves a row out of `awaiting`. The one upsert is the
   single-row kill switch in `SetAutomationDisabled`.
3. **Never renumber.** `diagnoses`, `human_approvals`, `send_attempts`, and
   `send_retries` use `INTEGER PRIMARY KEY AUTOINCREMENT` and their ordering is
   load-bearing — `LatestValidDiagnosis`, `LatestDiagnosisRecord`, the
   `StartSendAttempt` supersession guard, and `AuthorizeSendRetry` all depend on
   `ORDER BY id DESC`.
4. **Restart must be a no-op.** `Open()` on an existing database must not change any row
   count or any existing value. Prove it with a test that opens twice.
5. **Respect foreign keys.** `Open` sets `PRAGMA foreign_keys = ON`. A new referencing
   table must point at a parent row that reliably exists. `triggers` is the safe parent
   for anything keyed by trigger; `spawn_attempts` only has a row once a spawn was
   reserved.
6. **Derive display state, do not store it.** `AGENTS.md` and `docs/MVP.md` both require
   it, and `internal/web/status.go` is where derivation belongs — it computes
   `investigating`, `awaiting_approval`, `fixed`, `stalled`, and the rest from durable
   facts on every read. Do not add a `status`, `state`, or `label` column.
7. **Bound anything unbounded.** Follow `boundRaw` (`MaxDiagnosisRaw`, 64 KiB) and
   `boundedDetail` (4096 bytes) for any new text or blob column that can carry agent or
   CI output.

## In-repo precedent

`fix_verifications` and `send_retries` were added exactly this way: appended to the end
of the `migrate()` batch, each with its own index, each referencing
`triggers(trigger_key)`, with their write paths in `internal/ledger/verification.go`
(`openVerificationTx`, `ResolveVerification`, `AuthorizeSendRetry`) rather than by
widening an existing table. Read that file before writing your own migration — it is the
best available model.

## Adding a column to an existing table

You cannot edit the existing `CREATE TABLE`. Two options, in order of preference:

**Preferred — a side table** keyed by the same primary key with a foreign key to its
parent. It keeps `migrate()` a single idempotent statement batch, leaves existing rows
byte-identical, and makes "the fact was not recorded" distinguishable from "the fact is
empty".

**Fallback — a guarded `ALTER TABLE`.** SQLite has no `ADD COLUMN IF NOT EXISTS`, so
probe first. A `NOT NULL` column additionally needs a `DEFAULT` so existing rows stay
valid. Run it as a separate step after the batch `ExecContext` inside `migrate()`:

```go
func addColumnIfMissing(ctx context.Context, db *sql.DB, table, column, definition string) error {
	var present int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info(?) WHERE name=?`, table, column).Scan(&present); err != nil {
		return fmt.Errorf("inspect %s columns: %w", table, err)
	}
	if present > 0 {
		return nil
	}
	_, err := db.ExecContext(ctx, "ALTER TABLE "+table+" ADD COLUMN "+column+" "+definition)
	return err
}
```

`table`, `column`, and `definition` must be compile-time constants, never values derived
from input.

## Procedure

1. Confirm the fact belongs in the ledger rather than being derivable from existing
   facts. Check `docs/MVP.md` "Durable facts" first, and check whether
   `internal/web/status.go` can already derive it.
2. Append the `CREATE TABLE IF NOT EXISTS` (and any `CREATE INDEX IF NOT EXISTS`) to the
   end of the batch in `migrate()`.
3. Add a narrow write method on `*Ledger`. Validate arguments the way the existing
   methods do, timestamp with `timestamp()`, and make the insert idempotent —
   `ON CONFLICT(<key>) DO NOTHING`, or `INSERT OR IGNORE` as `openVerificationTx` does —
   when the fact must be recorded at most once. If the fact must commit atomically with
   another fact, take a `*sql.Tx` parameter and let the caller own the transaction, the
   way `openVerificationTx` is called from inside `CompleteSendAttempt`.
4. If the new table should be countable, add its name to the explicit allowlist in
   `Count`. That allowlist is a deliberate SQL-injection barrier — never build a table
   name from an argument. Note it currently omits `evaluations`, `check_suite_facts`,
   `automation_settings`, `fix_verifications`, and `send_retries`; those are queried
   directly by their own methods instead.
5. Extend the read models that should show the fact:
   - `Dashboard()` — add the field to `DashboardRow`, add a
     `COALESCE((SELECT … ), '')` correlated subquery or a `LEFT JOIN` to the query, and
     add the scan target to `rows.Scan` **in the same position** as the new expression
     in the SELECT list. Never an inner `JOIN`: it would hide every older row that
     predates the new fact. Scan-order mismatch fails at runtime, not compile time, and
     is the most common bug here.
   - `Stats()` in `internal/ledger/verification.go` — add an entry to its `counts`
     table if the fact is an aggregate.
   - `internal/web/status.go` — if the fact changes a derived lifecycle label.
6. If the value should be visible, extend `internal/web/status.go` and the dashboard
   template together.
7. Add the migration test below to `internal/ledger/ledger_test.go`.
8. Run `gofmt -l .`, `go vet ./...`, `go test -race ./...`.

## Worked example: record the AO harness used for each spawn

The harness is hardcoded to `codex` in `ao.Client.SpawnInvestigator` and
`SpawnInvestigatorSession`. Suppose it should become a durable audit fact. That is
logically a column on `spawn_attempts` — precisely the case where you must not edit an
existing `CREATE TABLE`. Use a side table.

**Step 2 — append to the batch in `migrate()`**, after the `send_retries` index that
currently ends the string:

```sql
CREATE TABLE IF NOT EXISTS spawn_harnesses (
 trigger_key TEXT PRIMARY KEY REFERENCES triggers(trigger_key), harness TEXT NOT NULL, recorded_at TEXT NOT NULL
);
```

The parent is `triggers`, not `spawn_attempts`: `RecordEvaluation` inserts the trigger
row before the reservation, so the foreign key can never reject a legitimate write.

**Step 3 — narrow, idempotent write method:**

```go
// RecordSpawnHarness stores which AO harness performed one spawn. It is additive
// and never overwrites an existing fact.
func (l *Ledger) RecordSpawnHarness(ctx context.Context, key domain.TriggerKey, harness string, at time.Time) error {
	if key == "" || strings.TrimSpace(harness) == "" || at.IsZero() {
		return fmt.Errorf("trigger key, harness, and record time are required")
	}
	_, err := l.db.ExecContext(ctx,
		`INSERT INTO spawn_harnesses(trigger_key,harness,recorded_at) VALUES(?,?,?) ON CONFLICT(trigger_key) DO NOTHING`,
		key, harness, timestamp(at))
	return err
}
```

**Step 4 — add `"spawn_harnesses"` to the `Count` allowlist.**

**Step 5 — read model.** Add `Harness` to the `DashboardRow` string block. In the
`Dashboard()` query, insert immediately before the final `w.received_at`:

```sql
COALESCE((SELECT harness FROM spawn_harnesses h WHERE h.trigger_key=e.trigger_key),''),
```

and insert `&r.Harness` immediately before `&created` in the `rows.Scan` call. Rows
written by an older binary scan as `""`, which correctly represents "not recorded".

**Step 7 — the test that proves an old database still opens and keeps its rows.**
Add it to `internal/ledger/ledger_test.go`. No existing test covers this: the closest,
`TestLifecycleFactsSurviveReopenAndBoundRawDiagnosis`, only reopens a database that the
*current* code created, so it cannot catch a destructive schema edit. This test builds a
database containing only the pre-change schema, using raw SQL, then opens it with the
new code. The `sqlite` driver is already registered by `ledger.go`'s blank import, so the
test file needs only `database/sql`:

```go
func TestMigrateIsAdditiveForAnOlderDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// Exactly the pre-change schema and rows this test depends on.
	if _, err := legacy.Exec(`
CREATE TABLE webhook_deliveries (delivery_id TEXT PRIMARY KEY, payload_digest TEXT NOT NULL, received_at TEXT NOT NULL);
CREATE TABLE triggers (trigger_key TEXT PRIMARY KEY, rule_id TEXT NOT NULL, outcome TEXT NOT NULL, created_at TEXT NOT NULL);
INSERT INTO webhook_deliveries VALUES('legacy-delivery','legacy-digest','2024-01-01T00:00:00Z');
INSERT INTO triggers VALUES('legacy-key','investigate-ci-failure','reserved','2024-01-01T00:00:00Z');`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	// Two passes: the first migrates, the second must be a complete no-op.
	for pass := 0; pass < 2; pass++ {
		l, err := Open(path)
		if err != nil {
			t.Fatalf("pass %d: open legacy database: %v", pass, err)
		}
		for table, want := range map[string]int{"webhook_deliveries": 1, "triggers": 1} {
			got, err := l.Count(context.Background(), table)
			if err != nil || got != want {
				t.Fatalf("pass %d: %s=%d want %d err=%v", pass, table, got, want, err)
			}
		}
		if err := l.RecordSpawnHarness(context.Background(), "legacy-key", "codex", time.Now()); err != nil {
			t.Fatalf("pass %d: new table unusable on a migrated database: %v", pass, err)
		}
		harnesses, err := l.Count(context.Background(), "spawn_harnesses")
		if err != nil || harnesses != 1 {
			t.Fatalf("pass %d: harnesses=%d err=%v", pass, harnesses, err)
		}
		if err := l.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
```

The second pass proves three things at once: `migrate()` is safe to re-run, the
pre-existing rows are untouched, and the new write is idempotent under
`ON CONFLICT DO NOTHING`.

Then run:

```sh
go test -race ./internal/ledger/...
go test -race ./...
```

## Compatibility note

Appending a table is safe in both directions. A newer binary opening an older file
creates the missing table during `Open()`. An older binary opening a newer file ignores
the extra table entirely. Editing an existing table is safe in neither direction — which
is why the rule is append-only.
