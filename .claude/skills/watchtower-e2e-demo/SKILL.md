---
name: watchtower-e2e-demo
description: Repeatable end-to-end exercise of AO Watchtower — offline checks, then `watchtower demo` against the fake AO boundary, then the full calculator-repo walkthrough with a live AO daemon. Use when verifying a change end to end, preparing or rehearsing the demo, or diagnosing "CI failed but nothing happened". Includes the ledger tables to inspect at each stage and the common failure modes with their causes.
---

# Watchtower end-to-end exercise

Three stages, cheapest first. Stage A and Stage B need no AO daemon, no GitHub account,
and no network. Only Stage C needs a live AO and a real repository.

Citations name files and symbols rather than line numbers; this tree changes frequently.

---

## Stage A — offline verification (no external services)

```sh
gofmt -l .
go vet ./...
go test -race ./...
go build -o watchtower ./cmd/watchtower
```

`gofmt -l .` must print nothing. `go test -race ./...` is the documented contributor
command in `README.md` and covers signature verification, policy, idempotency, AO argv
construction, approval gating, and the kill switch using fake runners and `httptest`
(`AGENTS.md`).

Confirm the offline suite still proves the acceptance criteria in `docs/MVP.md`:

- `internal/intake/handler_test.go` — `TestHandlerRejectsInvalidSignatureWithoutDurableAction`,
  `TestHandlerDispatchesOnlyNewCommittedReservation`.
- `internal/service/lifecycle_test.go` — `TestProcessReservationSpawnsOnlyOnce`,
  `TestFixRequiresApprovalAndRechecksKillSwitch`,
  `TestInvalidDiagnosisIsRetainedButUnusable`.
- `internal/ledger/verification_test.go` — `TestSuccessfulSendOpensExactlyOneVerification`,
  `TestSendRetryRequiresAFailedDispatch`,
  `TestKillSwitchStillBlocksAfterARetryAuthorization`.

---

## Stage B — `watchtower demo` (fake AO boundary)

Demo mode substitutes the `demoAO` type for the real client, uses a unique temporary
database, and pushes one signed fixture through the **real** HTTP intake handler — see
`demoConfig`, `demoDelivery`, and `newDemoRequest` in `cmd/watchtower/main.go`. It is
visibly labeled and is never selected as a fallback for production automation.

```sh
./watchtower demo
```

### B1 — startup

Expected log lines:

```text
Watchtower listening on http://127.0.0.1:8787 (DEMO MODE)
demo intake status: 202 Accepted
```

`202` proves signature verification, normalization, policy evaluation, and the durable
write all succeeded. Anything else means the fixture or the intake path is broken.

Fixed demo values, from `demoConfig` and `newDemoRequest`:

| | |
| --- | --- |
| repository | `demo/repo`, PR `1`, head `abcdef0123456789` |
| AO project | `demo-project`, session `demo-session` |
| admin token | `demo-admin-token` |
| callback secret | `demo-callback-secret` |
| trigger key | `github:demo/repo:pull:1:head:abcdef0123456789:rule:investigate-ci-failure` |
| database | `$TMPDIR/watchtower-demo-<unixnano>.db` (a new file every run) |

### B2 — dashboard

Open `http://127.0.0.1:8787`. The HTML shell deliberately carries no ledger content
(`TestShellCarriesNoLedgerContent`); it is marked `data-demo="true"` and then fetches
`GET /api/state`, which **requires the admin token** (`TestStateRequiresTheAdminToken`).
Enter `demo-admin-token`.

Check the state payload directly too:

```sh
curl -sS -H 'Authorization: Bearer demo-admin-token' http://127.0.0.1:8787/api/state
```

Expect `demo: true`, `automationDisabled: false`, and one row for `demo/repo` #1 with
project `demo-project`, session `demo-session`, spawn outcome `spawned`, and a derived
status of `investigating` (from `deriveStatus` in `internal/web/status.go`; it becomes
`stalled` after `StaleInvestigation`, 20 minutes, without a diagnosis).

### B3 — diagnosis through the scoped callback token

Demo mode does not fabricate a diagnosis. Submit one the way a real investigator would.
The token is `HMAC-SHA256(callbackSecret, "diagnosis:" + triggerKey)` in hex —
`Lifecycle.CallbackToken`:

```sh
KEY='github:demo/repo:pull:1:head:abcdef0123456789:rule:investigate-ci-failure'
TOKEN=$(printf 'diagnosis:%s' "$KEY" | openssl dgst -sha256 -hmac 'demo-callback-secret' -hex | awk '{print $NF}')
curl -sS -X POST \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  --data '{"category":"code","confidence":0.93,"summary":"Add returns a difference","evidence":[{"file":"calculator.go","line":4,"check":"TestAdd"}],"recommendedAction":"fix_code"}' \
  "http://127.0.0.1:8787/api/triggers?action=diagnosis&trigger=$KEY"
```

Expect `{"valid":true}`. `:` and `/` are legal in a query value; URL-encode the key if
you script this.

Verify the two negative paths as well:

- Reuse `$TOKEN` on `action=approve`. Expect **401** — a diagnosis credential cannot
  authorize an approval (`TestCallbackTokenIsScoped`, and the `approve` case requires
  `adminAuth`).
- Post `{"category":"not-allowed"}` with `$TOKEN`. Expect `{"valid":false}`; the raw
  payload is still retained (`Lifecycle.SubmitDiagnosis`).

The derived status should now be `awaiting_approval`.

### B4 — approve, fix, retry, kill switch

With the admin token:

```sh
A='Authorization: Bearer demo-admin-token'
curl -sS -o /dev/null -w '%{http_code}\n' -X POST -H "$A" -H 'Content-Type: application/json' \
  --data '{"actor":"me"}' "http://127.0.0.1:8787/api/triggers?action=approve&trigger=$KEY"   # 204
curl -sS -o /dev/null -w '%{http_code}\n' -X POST -H "$A" \
  "http://127.0.0.1:8787/api/triggers?action=fix&trigger=$KEY"                               # 202
curl -sS -o /dev/null -w '%{http_code}\n' -X POST -H "$A" \
  "http://127.0.0.1:8787/api/triggers?action=fix&trigger=$KEY"                               # 409 (ErrFixAlreadySent)
```

Status becomes `verifying`: `CompleteSendAttempt` opens a `fix_verifications` row in the
same transaction as the successful send, so a dispatched fix is never left unwatched.
In demo mode no poller ever resolves it, so it stays `awaiting` — that is correct.

`action=retry` (`AuthorizeSendRetry` then `FixWithAO`) must return **409** here, because
retry only supersedes a **failed** dispatch, never a successful one
(`TestRetryOnlyFollowsAFailedDispatch`).

Kill switch: `POST /api/automation` with `{"disabled":true,"actor":"me"}` → `204`; the
state payload flips `automationDisabled` and rows derive `held_by_kill_switch`. Restart
`demo` (fresh database), repeat B3, disable automation, then `action=fix`: expect `409`
from `ErrAutomationDisabled` with a durable `blocked_kill_switch` row. Intake and audit
visibility must keep working while the switch is on (`docs/MVP.md`).

### B5 — inspect the demo ledger

```sh
DB=$(ls -t "${TMPDIR:-/tmp}"/watchtower-demo-*.db | head -1)
sqlite3 "$DB" 'select delivery_id, received_at from webhook_deliveries;'
sqlite3 "$DB" 'select owner, repo, pull_number, head_sha, conclusion from check_suite_facts;'
sqlite3 "$DB" 'select delivery_id, outcome, trigger_key, ao_project_id from evaluations;'
sqlite3 "$DB" 'select trigger_key, outcome, ao_session_id, detail from spawn_attempts;'
sqlite3 "$DB" 'select id, valid, raw_truncated, length(raw) from diagnoses;'
sqlite3 "$DB" 'select trigger_key, actor from human_approvals;'
sqlite3 "$DB" 'select id, trigger_key, ao_session_id, outcome, detail from send_attempts;'
sqlite3 "$DB" 'select trigger_key, dispatched_head_sha, observed_head_sha, outcome from fix_verifications;'
sqlite3 "$DB" 'select trigger_key, actor, superseded_attempt_id from send_retries;'
```

`sqlite3` is optional — `/api/state` exposes the same facts. Note that `Count`'s
allowlist deliberately excludes `evaluations`, `check_suite_facts`,
`automation_settings`, `fix_verifications`, and `send_retries`; query those directly.

---

## Stage C — full calculator walkthrough (live AO)

Follows `README.md` "Copy-paste local test". Preconditions:

```sh
ao status
gh auth status
git --version
go version
```

### C1 — create the base repository

Build `calculator-watchtower` exactly as `README.md` describes: `calculator.go` with
`return a + b`, `calculator_test.go` asserting `Add(2,3) == 5`, and
`.github/workflows/ci.yml` with `on: pull_request`. Commit and publish with
`gh repo create … --push`.

**Check before continuing:** the workflow must already be on the base branch. If it is
not, the PR will never run CI and Watchtower will correctly do nothing.

### C2 — start Watchtower

Terminal A, from the repository root:

```sh
watchtower up --auto-fix
```

Expected output shape (produced by `runInit` in `cmd/watchtower/main.go`):

```text
Created Watchtower setup for <owner>/calculator-watchtower
AO project: calculator-watchtower
State: <user config dir>/ao-watchtower/<owner>/calculator-watchtower/state.json
Dashboard: http://127.0.0.1:8787
Admin token: …
Mode: AUTO-FIX (code diagnoses at or above 80%)
Monitoring GitHub every 5s; keep this command running. Press Ctrl+C to stop.
```

Checks:

- `state.json` exists and is private — `onboarding.Save` writes it through
  `protectFile`/`protectDirectory`, and `onboarding.Load` refuses it otherwise via
  `verifyPrivate`. Do not commit or share it.
- `watchtower.db` was created beside it.
- `ao project ls --json` shows the project and `ao project get <id> --json` reports a
  `path` equal to the checkout.
- Re-running `watchtower up` prints `Reused`, not `Created`, and registers no second AO
  project (`TestSetupCreatesProtectedReusableState`).

Optionally commit a `.watchtower.json` at the repository root to tighten automation —
see `internal/repopolicy` for the schema (`version`, `autoFix.minimumConfidence`,
`allowedPaths`, `deniedPaths`, `allowedCategories`, `requireEvidenceFile`,
`maxEvidenceFiles`). A repository policy can only tighten the operator's flags, never
loosen them, and a malformed file is an error rather than a silent fallback to
permissive.

### C3 — open a deliberately broken PR

Terminal B, per `README.md`: branch `demo/broken-addition`, change `return a + b` to
`return a - b`, commit, push, `gh pr create --base main`. The local `go test` failure is
expected.

### C4 — trigger

Within roughly one poll interval of GitHub reporting `failure`:

```sh
gh pr checks
DB="$HOME/.config/ao-watchtower/<owner>/calculator-watchtower/watchtower.db"
sqlite3 "$DB" 'select delivery_id, received_at from webhook_deliveries;'
sqlite3 "$DB" 'select delivery_id, outcome, trigger_key, ao_project_id from evaluations;'
sqlite3 "$DB" 'select * from triggers;'
sqlite3 "$DB" 'select * from spawn_reservations;'
```

Expect evaluation outcome `reserved`, exactly one `triggers` row, exactly one
`spawn_reservations` row. The key format is
`github:<owner>/<repo>:pull:<n>:head:<sha>:rule:investigate-ci-failure`
(`domain.NewCIFailureTriggerKey`).

`polling.aggregate` only reports a PR once **every** check has completed, so an
in-progress run produces no delivery at all — that is not a failure.

### C5 — investigator

```sh
sqlite3 "$DB" 'select trigger_key, ao_project_id, outcome, ao_session_id, detail from spawn_attempts;'
ao session ls --project <project-id> --json
```

Expect one row with outcome `spawned` and a non-empty `ao_session_id`. On the AO board a
`ci-investigator` worker appears with no terminal command typed by a human
(`docs/MVP.md`). The session was created with `--claim-pr <n> --no-takeover`.

If a reservation exists with **no** `spawn_attempts` row, the trigger is deferred —
either the concurrency limit held it back (`Lifecycle.HasCapacity`, `ErrAtCapacity`) or
the process stopped mid-flight. `ledger.DeferredSpawns` lists that backlog and
`scheduler.Controller` replays it. Intake treats
`domain.ErrInvestigationDeferred` as an accepted event, not an error.

### C6 — diagnosis

```sh
sqlite3 "$DB" 'select id, valid, raw_truncated, length(raw), recorded_at from diagnoses;'
sqlite3 "$DB" "select structured_json from diagnoses where valid=1 order by id desc limit 1;"
```

A usable diagnosis has `valid=1`, category `code`, `recommendedAction` `fix_code`, and
non-empty evidence pointing at `calculator.go` / `TestAdd`. An invalid submission is
still retained as bounded raw evidence and is never treated as an approved action.

### C7 — approval

- Review mode (`watchtower up`): open the dashboard, enter the admin token, press
  **Approve** then **Fix with AO**.
- Auto-fix mode: `automation.Controller` approves and dispatches on its own, logging
  `Auto-fix approved …` and `Auto-fix dispatched …`. The recorded actor is
  `local-auto-fix` unless `--auto-fix-actor` overrides it.

```sh
sqlite3 "$DB" 'select trigger_key, actor, approved_at from human_approvals;'
```

### C8 — send, verification, green CI

```sh
sqlite3 "$DB" 'select trigger_key, ao_session_id, outcome, detail from send_attempts;'
sqlite3 "$DB" 'select trigger_key, dispatched_head_sha, observed_head_sha, outcome, detail from fix_verifications;'
gh pr checks --watch
```

Expect one `send_attempts` row with outcome `sent`, and one `fix_verifications` row
opened in the same transaction, starting at `awaiting`.

AO then verifies the PR is still open, makes the scoped change, runs the tests, commits,
and non-force pushes to the existing head branch — it must not create a PR, merge, or
touch unrelated files (`fixPrompt`).

After the push the head SHA changes, which produces a **new** trigger key. That is
correct and is the clearest demonstration that the idempotency key includes the head
SHA. If the new run passes, the next evaluation is recorded as `non_failure` and no
spawn occurs.

The verification settles from `verification.classify`, which needs an open PR, a head
SHA **different** from the dispatched one, and a completed conclusion:

| `fix_verifications.outcome` | Meaning |
| --- | --- |
| `awaiting` | still waiting; nothing has proven the repair either way |
| `verified_green` | CI completed successfully on the repair commit |
| `still_failing` | CI still failed on the repair commit |
| `abandoned` | the timeout (`verification.DefaultTimeout`, 45 minutes) expired first — PR closed, no repair commit pushed, or the conclusion never settled |

The PR stays open for a human to review and merge.

---

## Common failure modes

| Symptom | Cause | Where to look |
| --- | --- | --- |
| No `webhook_deliveries` row at all | CI never ran, or checks are still in progress | workflow must be on the base branch with `on: pull_request`; Actions enabled (`README.md` troubleshooting). `polling.aggregate` skips incomplete rollups |
| Nothing happens on a failing PR | The PR is closed or merged | `polling.Client.ListCompleted` lists `--state open` only |
| Evaluation `unmapped_repository` | The mapping does not match the normalized `owner/name` | names are lowercased by `domain.ParseRepository`; resolved by `config.ProjectFor` via `policy.EvaluateCheckSuite` |
| Evaluation `non_failure` on a red PR | The rollup aggregated to `success` | `polling.aggregate` |
| Reservation exists but no `spawn_attempts` row | Deferred by the concurrency limit, or the process stopped mid-flight | `Lifecycle.HasCapacity` / `ErrAtCapacity`; `ledger.DeferredSpawns`; `scheduler.Controller.RunOnce` |
| `spawn_attempts.outcome = claim_conflict` | Another AO session already owns the PR | `PR_CLAIMED_BY_ACTIVE_SESSION`; Watchtower records it and never takes over. Inspect with `ao session ls --project <id> --json` |
| `spawn_attempts.outcome = blocked_kill_switch` | Kill switch on | `StartSpawnAttempt` audits the blocked attempt; status derives `held_by_kill_switch` |
| `spawn_attempts.outcome = failed`, detail mentions `output_limit` | AO output exceeded `Config.AOOutputLimit` | `ao.Runner.Run`, `ErrorOutputLimit` |
| Diagnosis rows all `valid=0` | Unknown field, trailing data, bad category, or evidence with no observation | `service.decodeDiagnosis`, `domain.Diagnosis.Validate`. Read the `raw` column |
| Auto-fix silent on a valid diagnosis | Ineligible, refused by repository policy, or the daily budget is spent | `automation.eligible`; `repopolicy.EvaluateAutoFix` (log line `Auto-fix refused by repository policy …` with a `Reason` code); `Controller.withinBudget` (log line `Auto-fix paused: daily budget …`) |
| **Approve** returns 409 | No valid diagnosis yet | `Lifecycle.ApproveFix` → `ErrNoValidDiagnosis` |
| **Fix with AO** returns 409 | No approval, already dispatched, no spawned session, or kill switch on | `Lifecycle.FixWithAO` |
| **Retry** returns 409 | The last dispatch was not a `failed` one | `ledger.AuthorizeSendRetry` — a successful send is never superseded |
| Verification stuck at `awaiting` | No new head SHA yet, or the conclusion has not settled | `verification.classify`; it only resolves on a different head SHA, or on timeout as `abandoned` |
| Verification `abandoned` | Timeout expired first | `verification.DefaultTimeout` |
| `/api/state` returns 401 | The admin token is missing or wrong | `Server.state` requires `adminAuth` |
| Diagnosis POST returns 401 | Wrong scoped token, or the admin token was used | `Lifecycle.CallbackToken` / `VerifyCallbackToken` |
| `bind: address already in use` | Another Watchtower instance, or a foreign process | `watchtower status`; or `--listen 127.0.0.1:8888`, which is persisted |
| Startup fails `AO is not ready` | `ao status --json` did not report `state: ready` | `onboarding.checkAOReady` |
| Startup fails `cannot find AO` | Not on `PATH` and not at a known install location | `resolveAOExecutable`, `aoInstallCandidates`; pass `--ao /absolute/path/to/ao` |

## Resetting between runs

`watchtower demo` allocates a fresh temporary database each run, so it needs no reset.

For Stage C, deleting `watchtower.db` discards the audit record permanently — only do
that in a throwaway demo repository. To re-run the flow without deleting anything, push
another broken commit: the new head SHA yields a new trigger key and a new investigator.

## What Stage C exercises, and what it does not

`watchtower up` starts the deferred-spawn scheduler and the verification recorder
unconditionally, applies `.watchtower.json` to `--auto-fix`, and starts the pull request
comment publisher only under `--comment`. Confirm the wiring is intact before trusting
any of those stages:

```sh
rg -n 'internal/(scheduler|verification|prcomment|repopolicy|notify)' cmd internal
```

`watchtower demo` deliberately runs none of them: it has no poller, so nothing ever
resolves a `fix_verifications` row, and a demo verification correctly stays `awaiting`
forever. Use Stage C, not the demo, to exercise verification, deferred-spawn replay, or
comment publishing.
