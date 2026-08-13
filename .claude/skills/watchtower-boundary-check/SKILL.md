---
name: watchtower-boundary-check
description: Audit a Watchtower change against every engineering boundary in AGENTS.md. Use before committing, before opening a PR, or when reviewing a diff that touches any package under internal/ or cmd/watchtower — and always when a change adds an external command, a durable state transition, a prompt, an HTTP route, or a new config field.
---

# Watchtower boundary check

The boundaries in `AGENTS.md` are the product's safety contract, not style
preferences. Violating one silently converts an auditable supervisor into an
unsupervised actor. Work through every section below and record each boundary as
checked or as a finding. Do not skip one because the diff "looks unrelated".

Citations here name files and symbols rather than line numbers: this tree is under
active development and line numbers rot. Locate a symbol with `rg -n '<symbol>'`.

Scope the audit first:

```sh
git diff --stat
git diff
git status --porcelain      # untracked packages count as part of the change
```

---

## 1. No AO private-database access

`AGENTS.md` — never open, mutate, copy, or depend on AO's SQLite database or private
cloud code. Watchtower owns exactly one database: its own ledger.

```sh
rg -n 'modernc.org/sqlite|sql\.Open|database/sql' internal cmd
rg -ni 'application support|appdata|\.aoagents|aoagents\.dev|~/\.ao|ao\.db|orchestrator\.db' internal cmd
```

**Expected:** `database/sql` appears only inside `internal/ledger` (`ledger.go`,
`verification.go`); the driver is imported once, blank, in `internal/ledger/ledger.go`;
the only `sql.Open` is in `Open` against `Config.SQLitePath`. The second grep returns
nothing.

**Failure signature:** any file read, `sql.Open`, or path join that reaches an AO-owned
directory or database; `database/sql` imported outside `internal/ledger`; reading AO
state to answer a question the `ao` CLI already answers.

---

## 2. AO reached only through the `ao` executable and its `--json` contracts

`AGENTS.md` — integrate only via the executable, and treat its path as configurable
(never assume `PATH`).

```sh
rg -n 'runner\.Run\(ctx' internal/ao
rg -n '"project"|"session"|"spawn"|"send"|"status"' internal/ao/client.go internal/onboarding/onboarding.go
rg -n 'AOExecutable|aoExecutableName|aoInstallCandidates' internal cmd
```

**Expected:** the only AO invocations are in `internal/ao/client.go` (`ListProjects`,
`ListLiveSessions`, `SpawnInvestigator`, `SpawnInvestigatorSession`, `InspectSession`,
`SendApprovedFollowup`) and `internal/onboarding/onboarding.go` (`checkAOReady`,
`verifyExisting`, `ensureAOProject`). The executable comes from configuration, `--ao`,
or `resolveAOExecutable` in `cmd/watchtower/main.go` with the per-platform
`aoInstallCandidates` — never a bare literal handed to `exec.Command`.

**Failure signature:** a subcommand outside `status` / `project ls|get|add` /
`session ls|get` / `spawn` / `send`; a JSON read that omits `--json`; decoding into
`map[string]any` or `any` instead of a narrow DTO; defaulting silently when an envelope
key is absent instead of erroring the way `ListProjects` and `ListLiveSessions` do.

See the `watchtower-ao-contract` skill for the full surface.

---

## 3. argv-only execution, never a shell string

`AGENTS.md`.

```sh
rg -n 'exec\.Command|exec\.CommandContext|exec\.LookPath' internal cmd
rg -n '"sh"|"bash"|"-c"|"cmd"|"/c"|strings\.Join\(.*" "\)' internal cmd
```

**Expected:** four argv-only process paths — `internal/ao/runner.go`
(`execProcessRunner.Run`), `internal/onboarding/onboarding.go` (`ExecCommander.Run`),
`internal/polling/polling.go` (`ExecRunner.Run`), and `internal/prcomment/prcomment.go`
(`ExecRunner.Run`) — plus `exec.LookPath` in `cmd/watchtower/main.go`. The second grep
returns nothing.

**Failure signature:** `exec.Command("sh", "-c", …)`; a single argument built by
concatenating a repository name, branch, PR title, prompt, or diagnosis field;
`strings.Join(args, " ")` handed to any runner; `Env` or `Dir` derived from untrusted
input.

Note that `internal/prcomment` composes a whole Markdown comment into one `-f body=…`
argument. That is safe **only** because it stays a single argv element and every
untrusted field passes through `neutralize`/`neutralizeInline`. Preserve both
properties.

---

## 4. Every external mutation idempotent

`AGENTS.md` and `docs/MVP.md` ("Idempotency key", acceptance criteria). A replayed
webhook must not create a second session for the same repository, PR, head SHA, and
rule.

```sh
rg -n 'INSERT INTO|INSERT OR|UPDATE |DELETE|DROP' internal/ledger
rg -n 'RowsAffected' internal/ledger
rg -n 'NewCIFailureTriggerKey' internal cmd
```

**Expected:** the trigger key is constructed only by `domain.NewCIFailureTriggerKey`.
Each external mutation is gated by a durable reservation keyed on the trigger:

- `spawn_reservations` — claimed in `RecordEvaluation` with `ON CONFLICT DO NOTHING`
  plus `RowsAffected() == 1`, which is what sets `Result.Reserved`.
- `spawn_attempts` — claimed in `StartSpawnAttempt` the same way;
  `Lifecycle.ProcessReservation` calls AO only when `start.Started` is true and
  `start.Blocked` is false.
- `send_attempts` — guarded in `StartSendAttempt`.
- `fix_verifications` — opened by `openVerificationTx` with `INSERT OR IGNORE`, inside
  the same transaction as `CompleteSendAttempt`.
- Comment publication — `prcomment.Publisher.Upsert` finds its own hidden marker and
  edits in place, so republishing never creates a second comment.

**Failure signature:** an external call not preceded by a `RowsAffected()==1`
reservation; `INSERT OR REPLACE` over a durable fact; a discarded `RowsAffected`; a
reservation committed in a different transaction from the fact it guards; a retry loop
around a spawn, a send, or a comment.

---

## 5. Untrusted text never becomes shell syntax or overrides the investigator role

`AGENTS.md`. Repository text, CI logs, PR metadata, evidence file paths, and agent prose
are all untrusted.

```sh
rg -n 'untrusted-ci-event|validated-diagnosis' internal/service/lifecycle.go
rg -n 'safeSessionToken|parseSpawnedSession' internal/ao/client.go
rg -n 'neutralize|sanitize|normalizeEvidencePath' internal/prcomment internal/repopolicy
rg -n 'DisallowUnknownFields' internal
```

**Expected:**

- `Lifecycle.investigatorPrompt` states the role and the refusal *before* any external
  data, then encloses that data in `<untrusted-ci-event>` and declares it
  non-authoritative. `fixPrompt` encloses only the already-validated structured
  diagnosis in `<validated-diagnosis>`.
- The prompt travels as one argv element (`--prompt`), so it can never become syntax.
- Values retained from AO process output pass an allowlist: `safeSessionToken` limits
  session id and status to `[A-Za-z0-9_-]{1,128}`, and `parseSpawnedSession` rejects
  embedded newlines and oversized suffixes.
- Submitted diagnoses are strict: `decodeDiagnosis` uses `DisallowUnknownFields` plus a
  trailing-data check, then `domain.Diagnosis.Validate`.
- Published comment text passes `prcomment.neutralize` / `neutralizeInline`, which strip
  control characters, escape `<!--` and `-->` so untrusted prose can neither forge nor
  terminate Watchtower's marker, and bound every field.
- Model-authored evidence paths pass `repopolicy.normalizeEvidencePath`, which refuses
  absolute, backslash-separated, drive-qualified, non-printable, and `..` paths outright
  rather than cleaning them into something that merely looks safe.

**Failure signature:** external data interpolated ahead of the role sentence or outside
its labeled block; a diagnosis field used to build a path, a command, or a branch name;
an identifier retained from process output without an allowlist; a new prompt assembled
with `fmt.Sprintf` from CI log text; rendered comment text that skips `neutralize`.

---

## 6. Investigation is advisory; code modification requires durable approval

`AGENTS.md`, `docs/MVP.md` ("A fix message cannot be sent without a durable human
approval").

```sh
rg -n 'SendApprovedFollowup' internal cmd
rg -n 'HasHumanApproval|RecordHumanApproval|ApproveFix|FixWithAO|AuthorizeSendRetry' internal
```

**Expected:** `SendApprovedFollowup` is defined on `ao.Client`, declared on the narrow
`service.AOClient` interface, and called from exactly one production site —
`Lifecycle.FixWithAO`. That call runs after, in order: a valid stored diagnosis, a
recorded approval (`HasHumanApproval`), a spawned session (`SessionForTrigger`), and a
claimed send attempt (`StartSendAttempt`). `ApproveFix` itself refuses without a valid
diagnosis. Auto-fix goes through the same `ApproveFix` before `FixWithAO`
(`automation.Controller.RunOnce`) and only for diagnoses passing `eligible`, the
optional repository `Gate`, and the daily budget.

`ledger.AuthorizeSendRetry` is the only way a second dispatch becomes possible, and it
**refuses to supersede a successful send** — only a `failed` attempt is retryable. That
property is what stops an approved fix from being delivered twice.

**Failure signature:** any new `ao send` path; approval recorded after dispatch;
`FixWithAO` reordered so the send precedes the approval check; the investigator prompt
losing its "do not modify code, commit, push, send messages, or change ownership"
clause; an auto-fix eligibility rule admitting a category other than `code` or an
empty-evidence diagnosis; a repository policy that *loosens* an operator flag rather
than tightening it (`repopolicy.EvaluateAutoFix` must keep taking the stricter of the
two confidence floors); `AuthorizeSendRetry` allowing a `sent` attempt to be superseded.

---

## 7. Secrets redacted from logs and the dashboard

`AGENTS.md`.

```sh
rg -n 'Secret|Token' internal/config/config.go internal/onboarding/onboarding.go internal/web
rg -n 'log\.Printf|log\.Print|fmt\.Fprintf' internal cmd
rg -n '%\+v|%#v' internal cmd
```

**Expected:** `WebhookSecret`, `AdminToken`, and `CallbackSecret` carry `json:"-"` in
`config.Config` and are environment- or state-file-only. `ao.CommandError.Error()`
prints only the executable and the error kind, never captured output. `onboarding`,
`polling`, and `prcomment` all bound and trim process stderr before embedding it. The
dashboard renders only ledger read-model fields plus the derived labels in
`internal/web/status.go`.

**Failure signature:** logging an `*ao.CommandError` with `%+v` or `%#v` — its `Args`
field holds the `--prompt` value, which contains the scoped callback bearer token;
adding a prompt, message, or raw command line to a dashboard row or to `web.Row`;
a new secret-bearing config field without `json:"-"`; printing `onboarding.State`;
echoing an `Authorization` header.

---

## 8. Default to no action when ownership, identity, or state cannot be proven

`AGENTS.md`.

```sh
rg -n 'ClaimConflict|no-takeover|claim-pr' internal
rg -n 'VerifySignature' internal
rg -n 'Truncated' internal/ao internal/service
```

**Expected:**

- Ownership authority is `--claim-pr <n> --no-takeover` on spawn. A refusal becomes a
  typed `*ao.ClaimConflictError`, is classified with `ao.IsClaimConflict` rather than
  string matching, and is audited as `claim_conflict` with no retry and no takeover.
  Ownership is never inferred from `ListLiveSessions`.
- `intake.Handler.ServeHTTP` verifies the signature before validating the delivery id,
  before `NormalizeCheckSuiteCompleted`, and before any durable write.
- A spawn whose session id is empty or whose project does not match the reservation is
  recorded as `failed`, not accepted.
- Truncated AO output is a failure, not a partial success — in `Runner.Run` and again in
  `FixWithAO`.
- `github.NormalizeCheckSuiteCompleted` requires exactly one associated pull request and
  matching head SHAs.
- `verification.classify` settles a repair only on a *different* head SHA with a
  completed conclusion; a closed PR, an unchanged head, or an unsettled conclusion keeps
  waiting and only the timeout resolves it, as `abandoned` rather than as success.
- `repopolicy.Policy`'s zero value is deliberately unusable, so an ignored `Load` error
  can never be mistaken for a permissive policy.

**Failure signature:** a zero value or `""` substituted for a missing field instead of
an error; ownership derived from a session listing; a claim conflict retried; a
truncated result treated as success; parsing before verifying a signature; a
verification resolved green on the same head SHA that failed.

---

## Before reporting completion

`AGENTS.md`:

```sh
gofmt -l .
go vet ./...
go test -race ./...
```

`go test -race` is the documented contributor command in `README.md`. Tests must pass
without a live AO daemon, GitHub account, network, or model API.

Also confirm any new package is actually reachable. A boundary enforced only in
unreachable code protects nothing, and a package with a full test suite but no
production importer reads as a working feature while doing nothing. Check with:

```sh
rg -n 'internal/(scheduler|verification|prcomment|repopolicy|notify)' cmd internal
```

Every one of those must appear in `cmd/watchtower/main.go`. They are wired
there today: the scheduler and the verification recorder start unconditionally
in the `up` ready hook, the repository policy gates `--auto-fix`, and the
comment publisher starts only under `--comment`.
