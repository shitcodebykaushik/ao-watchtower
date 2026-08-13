# AO Watchtower MVP contract

## User story

When GitHub reports that CI failed for a pull request, Watchtower verifies the
event, checks its durable ledger, determines whether an AO worker already owns the
problem, and creates at most one focused AO investigator. The investigator returns
an evidence-backed diagnosis. Watchtower displays the result and waits for a human
before asking the AO worker to modify code.

## Demonstration

1. Open the Watchtower dashboard and AO Kanban board side by side.
2. Push a deliberately broken commit to a demo pull request.
3. GitHub reports a failed check suite.
4. Watchtower records the verified event and evaluates the rule.
5. A new `ci-investigator` worker appears on the AO board without a terminal command.
6. The dashboard links the GitHub event to the AO session and shows its diagnosis.
7. The user selects **Fix with AO**.
8. Watchtower records approval and sends a scoped fix instruction to that session.
9. The agent pushes a fix and CI becomes green.

## Local onboarding

The default local experience is `watchtower up` from a GitHub checkout. It
discovers the repository and public AO project through CLI contracts, stores
private installation state with owner-only permissions, and polls PR check facts
through the user's existing `gh` authentication. Poll results are converted into
signed local deliveries and pass through the same normalization, durable ledger,
policy, and idempotency boundary as remote GitHub webhooks. This removes tunnel
and webhook setup without weakening the downstream automation controls.

`watchtower up --auto-fix` records an explicit repository-scoped operator choice
and automatically approves only schema-valid, evidence-bearing `code` diagnoses
at or above the configured confidence threshold. The scoped AO instruction may
test, commit, and non-force push to the already claimed PR branch, but may not
create or merge a PR, take over ownership, or alter unrelated files.

## Structured diagnosis

```json
{
  "category": "code",
  "confidence": 0.93,
  "summary": "The parser rejects requests without an optional header",
  "evidence": [
    {
      "file": "internal/parser/parser.go",
      "line": 84,
      "check": "TestOptionalHeader"
    }
  ],
  "recommendedAction": "fix_code"
}
```

Allowed categories: `code`, `test`, `infrastructure`, `flaky`, `configuration`,
`dependency`, and `unknown`.

## Durable facts

- Webhook delivery identifier and payload digest
- Repository, pull request number, and head SHA
- Check-suite identifier, conclusion, and relevant provider URLs
- Rule identifier and evaluation outcome
- AO project and session identifiers
- Spawn attempt and command outcome
- Diagnosis payload and validation status
- Human approval actor/time and subsequent send outcome
- Repair verification: the dispatched head SHA, the head SHA later observed, the
  terminal outcome, and when it settled
- Retry authorization: actor, time, and the send attempt it supersedes

Dashboard labels such as `investigating`, `awaiting_approval`, and `fixed` are
derived from those facts rather than stored as an independent source of truth.

## Repair verification

A dispatched fix is not an outcome. `CompleteSendAttempt` opens a verification in
the same transaction as a successful send, so a fix that reached AO is never left
unwatched. The verification settles only when GitHub reports a completed check
suite for a head commit **other** than the one that failed:

- `verified_green` — CI completed successfully on the repair commit.
- `still_failing` — CI still failed on the repair commit.
- `abandoned` — the timeout expired first: the pull request left the open set, no
  repair commit was pushed, or the conclusion never settled.

Verification is driven by the observations the poller already collects, so it
consumes no additional GitHub API budget. Resolution is one-way; a replayed
observation cannot rewrite a settled outcome.

## Repository policy

A repository may commit `.watchtower.json` to constrain automatic repair with a
confidence floor, path allow/deny globs, category filtering, an evidence-file
requirement, and a blast-radius bound. A policy may only tighten the operator's
flags. A malformed policy is a startup error rather than a silent fall back to
permissive, and evidence paths are refused rather than normalized when they are
absolute or contain traversal.

## Limits

- A concurrent-investigation limit is checked before the durable spawn attempt, so
  a held-back trigger keeps its reservation. A scheduler replays reservations that
  have no spawn attempt, which also recovers a process that stopped mid-flight.
- A rolling 24-hour fix budget is checked immediately before each automatic
  dispatch, so a burst of eligible diagnoses cannot overshoot it.

## Idempotency key

The initial trigger key is:

```text
github:<owner>/<repo>:pull:<number>:head:<sha>:rule:investigate-ci-failure
```

Webhook redelivery, service restart, and concurrent evaluation must resolve to the
same trigger record and at most one AO spawn.

## AO adapter contract

The adapter needs only these product operations:

- List projects as JSON
- List live sessions for one project as JSON
- Spawn a named Codex investigator for a mapped project
- Inspect one session as JSON
- Send a human-approved follow-up message

The command runner receives an executable plus discrete argv, captures bounded
stdout/stderr, enforces a context timeout, and exposes typed errors. It never invokes
a shell.

## Acceptance criteria

- Invalid webhook signatures return an error and cause no durable action.
- A successful non-failure check-suite event is recorded but does not spawn.
- A failed event for an unmapped repository is visible as skipped.
- Two deliveries for the same PR/head/rule create one trigger and no more than one
  spawn attempt.
- An existing live owner causes the event to be linked/skipped, not duplicated.
- A successful spawn records the AO session id.
- An invalid diagnosis is retained as raw bounded evidence but never treated as an
  approved action.
- A fix message cannot be sent without a durable human approval.
- The global kill switch prevents new spawns and sends while leaving intake/audit
  visibility operational.
- A successful send opens exactly one verification; a failed send opens none.
- A settled verification is never rewritten by a later observation.
- A dispatch that failed is reported to the caller as a failure, never as success,
  and becomes retryable exactly once per explicit authorization.
- A successful send can never be superseded by a retry authorization.
- A trigger held back by the concurrency limit keeps its reservation and is
  replayed, and intake reports it as accepted rather than failed.
- A repository policy can tighten but never loosen the operator's confidence floor.
- The unauthenticated dashboard shell carries no ledger content; `/api/state`
  requires the admin token, and a diagnosis callback token does not unlock it.
- Private installation state is unreadable by other accounts on Linux, macOS, and
  Windows, and refuses to load when it is not.
- `go test ./...` and `go vet ./...` pass without live external services.

## Explicit non-goals

- General workflow-language design
- Automatic code mutation or pushing without approval
- Direct LLM-provider integration
- Direct AO database/API coupling
- Supporting every GitHub event
- GitLab, Gitea, Jira, or Linear
- Hosted multi-tenant operation
- Monitoring several repositories from one process
- Automatic merging, even after a repair verifies green
