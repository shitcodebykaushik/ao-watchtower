---
name: watchtower-ao-contract
description: The exact AO CLI surface Watchtower depends on — argv, JSON envelopes, the spawn text success line, and the claim-conflict code — plus the procedure for adding a new AO operation. Use when changing internal/ao, when an AO command or its output shape changes, when a spawn or send fails to parse, or when deciding how Watchtower should learn something about AO.
---

# Watchtower ↔ AO CLI contract

Watchtower integrates with Agent Orchestrator only through the `ao` executable and its
`--json` contracts (`AGENTS.md`). The executable path is configurable and never assumed
to be on `PATH` — see `config.Config.AOExecutable`, the `--ao` flag, and
`resolveAOExecutable` with the per-platform `aoInstallCandidates` in
`cmd/watchtower/main.go`.

Citations name files and symbols, not line numbers; this tree changes frequently.

## The complete surface

| Command (argv) | Call site | Output consumed |
| --- | --- | --- |
| `status --json` | `onboarding.checkAOReady` | `{"state":"ready"}` — only `state`; anything but `ready` is a hard stop |
| `project ls --json` | `ao.Client.ListProjects`, `onboarding.ensureAOProject` | `{"projects":[{"id":"…","name":"…"}]}` |
| `project get <id> --json` | `onboarding.verifyExisting`, `onboarding.ensureAOProject` | `{"project":{"path":"…"}}` — only `path`, compared with `samePath` |
| `project add --id <id> --name <owner/repo> --path <root> --worker-agent codex --orchestrator-agent codex` | `onboarding.ensureAOProject` | text; only success/failure is used. **No `--json`.** |
| `session ls --project <id> --json` | `ao.Client.ListLiveSessions` | `{"data":[{"id","projectId","status"}],"meta":{…}}` — `meta` deliberately ignored |
| `session get <sessionID> --project <id> --json` | `ao.Client.InspectSession` | `{"session":{"id","projectId","status"}}` |
| `spawn --project <id> --name ci-investigator --claim-pr <n> --no-takeover --harness codex --prompt <text>` | `ao.Client.SpawnInvestigator`, `SpawnInvestigatorSession` | one text line, see below |
| `send --session <id> --message <text>` | `ao.Client.SendApprovedFollowup` | text; only exit status and truncation flags are used |

Nothing else is called. `--json` responses decode into the narrow DTOs at the top of
`internal/ao/client.go` (`Project`, `Session`, `projectListResponse`,
`sessionListResponse`, `sessionGetResponse`), never into `map[string]any`.

The investigator name is the package constant `investigatorName` (`ci-investigator`),
and the harness is the literal `codex`.

### Envelope rejection rules

Absence is an error, never a default (`AGENTS.md`):

- `projects` nil → `decode AO JSON: missing projects envelope` (`ListProjects`).
- Any project with a blank `id` → error naming the index.
- `data` nil → `decode AO JSON: missing session data envelope` (`ListLiveSessions`).
- Any session with a blank `id` → error naming the index.
- `session` nil or blank `id` → `AO session has no id` (`InspectSession`).

Note the deliberate duplication: `internal/onboarding` decodes `project ls` and
`project get` with its own anonymous structs, because onboarding runs on the generic
`onboarding.Commander` interface before an `ao.Runner` exists. **If an envelope changes,
both places must change.**

## Spawn success line

`parseSpawnedSession` accepts AO's documented text line and no other output shape:

```text
spawned session <id> (<status>)<arbitrary suffix>
```

- One line only. A single trailing `\n` and `\r` are trimmed; any remaining `\r` or
  `\n` is rejected.
- Literal prefix `spawned session `.
- The id runs to the first ` (`; the status runs to the following `)`.
- Both id and status must satisfy `safeSessionToken`: 1–128 characters from
  `[A-Za-z0-9_-]`.
- Any suffix after `)` is ignored but must be at most `maxSpawnSuffix` (4096) bytes.

A real accepted line, from `TestSpawnInvestigatorSessionParsesTextContractAndClaimConflict`
in `internal/ao/client_test.go`:

```text
spawned session ao-123 (idle) (claimed https://github.com/o/r/pull/42) [prompt 100 B, system 200 B]
```

The parsed `Session` is cross-checked before it is trusted:
`Lifecycle.ProcessReservation` records `failed` when the id is blank or the project does
not match the reservation.

## Claim conflict

The stable code is `PR_CLAIMED_BY_ACTIVE_SESSION`, matched by the package regexp
`stableClaimConflictCode`, which requires a non-word character or a string boundary on
both sides — `XPR_CLAIMED_BY_ACTIVE_SESSION` must not match
(`TestClaimConflictRequiresExactStableCode`). Both stdout and stderr are searched by
`isClaimConflict`, because AO emits surrounding rollback prose that is deliberately
ignored.

Flow: `Runner.Run` returns a `*CommandError`; `SpawnInvestigatorSession` wraps it in
`*ClaimConflictError`; callers classify with `ao.IsClaimConflict` rather than string
matching; `Lifecycle.ProcessReservation` records the spawn attempt as `claim_conflict`.
There is no retry and no takeover.

## Runner invariants

`internal/ao/runner.go`:

- argv only, never a shell (`execProcessRunner.Run`); `ProcessRunner` is injectable so
  tests need no live daemon.
- Context timeout applied per call in `Runner.Run`.
- stdout and stderr captured into `boundedBuffer` with truncation flags.
- Typed error kinds on `CommandError`: `ErrorTimeout`, `ErrorExecution`,
  `ErrorOutputLimit`. Truncation is a failure, not a partial success — and
  `Lifecycle.FixWithAO` separately records `failed` when a send result is truncated.
- `CommandError.Error()` never includes process output. Its `Args` field *does* hold the
  `--prompt` value, which carries the scoped callback token — never format a
  `CommandError` with `%+v` or `%#v`.

## Ownership authority

**Ownership is established only by `--claim-pr <n> --no-takeover` on `spawn`.** AO either
grants the claim or refuses with `PR_CLAIMED_BY_ACTIVE_SESSION`. That refusal is the
answer; Watchtower records it and stops.

Ownership is **never** inferred from `session ls`. `ListLiveSessions` exists for display
and audit links only. Do not add code that scans a session listing for a PR number, a
branch, or a name and concludes that Watchtower may act. Do not add `--takeover`, and do
not make `--no-takeover` conditional.

## Adding a new AO operation

1. Confirm the product needs it. `docs/MVP.md` "AO adapter contract" lists the five
   operations the adapter is allowed to need. Anything beyond that is scope expansion
   and needs a stated reason.
2. Extend `internal/ao/client.go` with one method. Validate arguments before running —
   blank project or session ids are rejected up front by the existing methods.
3. Prefer `--json`. Add a narrow DTO beside the existing ones with only the fields the
   product uses. No `map[string]any`, no `any`, no catch-all `Extra` field.
4. Route JSON reads through `runJSON` so decoding failures become `decode AO JSON: …`.
5. Reject ambiguity explicitly: nil envelope, blank identifier, wrong element count, or
   an unexpected shape must return an error rather than a zero value.
6. If the command returns text rather than JSON, constrain what you retain the way
   `parseSpawnedSession` and `safeSessionToken` do — one line, allowlisted characters,
   bounded length.
7. If the operation is an external mutation, gate it on a durable ledger reservation
   first (see `watchtower-boundary-check`, boundary 4) and record the outcome.
8. Add a fake-runner test in `internal/ao/client_test.go`. Reuse `fakeProcess`,
   `newTestClient`, and `assertCommand` to assert the exact executable and the exact
   argv slice, plus one test per rejection path you added. Tests must not require a live
   AO daemon.
9. If onboarding also needs the operation, update its anonymous decoder and extend
   `fakeCommander` in `internal/onboarding/onboarding_test.go`, which fails the test on
   any unexpected command.
10. Update this skill's surface table, then run `gofmt -l .`, `go vet ./...`,
    `go test -race ./...`.

## Related: the second external CLI

Watchtower also shells out to `gh`, through three separate argv-only runners —
`polling.ExecRunner` (`gh pr list --repo … --state open --limit 100 --json …`),
`onboarding.ExecCommander` (`gh auth status`, plus `git`), and `prcomment.ExecRunner`
(`gh api --method GET|POST|PATCH …`). Those are governed by the same argv, bounding, and
untrusted-input rules, but they are **not** part of the AO contract. Do not route AO
calls through them, and do not route `gh` calls through `ao.Runner`.

## Do not

- Read AO's database or any AO private file.
- Build an `ao` invocation through a shell string.
- Retry a claim conflict, or convert one into a takeover.
- Treat missing JSON keys, truncated output, or an unparsable spawn line as success.
- Widen a DTO "just in case". Narrowness is the contract.
