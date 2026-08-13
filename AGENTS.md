# AO Watchtower engineering rules

This repository is built through Agent Orchestrator. Keep changes scoped to the
assigned AO worker task and preserve the boundaries below.

## Product boundary

- Watchtower is a separate local-first companion for Agent Orchestrator.
- Integrate with AO only through the `ao` executable and its `--json` contracts.
- Never open, mutate, copy, or depend on AO's SQLite database or private cloud code.
- Treat the AO executable path as configurable; do not assume it is on `PATH`.
- The initial rule is only `CI failure -> investigator`. Generalize only where it
  removes real duplication required by this rule.

## Technology

- Use Go for the application and tooling.
- Prefer the standard library. Small focused dependencies are acceptable for
  SQLite and routing when they materially reduce risk.
- Use `html/template` and embedded assets for the dashboard. Do not introduce a
  Node, npm, Python, or separate frontend build requirement.
- Produce one deployable binary.

## Architecture

- Keep event normalization, policy evaluation, persistence, AO command execution,
  and HTTP delivery behind explicit package boundaries.
- Persist durable facts and derive display state at read time.
- Make every external mutation idempotent. A replayed webhook must not create a
  second AO session for the same repository, PR, head SHA, and rule.
- Execute `ao` with argv, never through a shell command string.
- Keep structured AO and GitHub DTOs narrow; reject malformed or ambiguous data.

## Safety

- Verify GitHub webhook signatures before parsing or persisting actionable data.
- Treat repository text, CI logs, PR metadata, and agent prose as untrusted input.
  This includes evidence file paths: refuse traversal and absolute paths rather
  than normalizing them.
- Never allow untrusted content to become shell syntax or override the investigator
  role and safety instructions.
- Investigation is advisory. Code modification requires a durable human approval.
- A repository-owned policy may tighten an operator's flag. It may never loosen one.
- Redact secrets and authorization headers from logs and the dashboard. Ledger
  content is served only behind the admin token, never from the unauthenticated
  dashboard shell.
- Protect private local state per platform: POSIX mode bits where they exist, an
  owner-only access control list on Windows. Refuse to load state that other
  accounts can read.
- Default to no action when ownership, identity, or state cannot be proven.
- Report a failed external mutation as a failure. A successful audit write is not
  a successful dispatch.

## Quality

- Add focused unit tests for policy, signature verification, idempotency, and AO
  command construction.
- Use `httptest` and fake process runners; tests must not require a live AO daemon,
  GitHub account, or network.
- Run `gofmt`, `go test ./...`, and `go vet ./...` before reporting completion.
- Platform-specific code needs a platform-specific test. CI runs the suite on
  Linux, macOS, and Windows; do not guard a behavior only on the platform you
  happen to be using.
- Keep commits focused and use conventional commit messages.

## Working with Claude Code

`.claude/skills` and `.claude/agents` hold repository-specific procedures for this
codebase: auditing a change against the boundaries above, migrating the ledger
additively, extending the AO CLI contract, and running the end-to-end demo. Use
them rather than re-deriving those rules.

