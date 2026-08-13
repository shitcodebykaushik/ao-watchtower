# AO Watchtower

**Turn failed GitHub pull-request checks into supervised AO repairs.**

AO Watchtower is a self-hosted companion for [Agent Orchestrator](https://aoagents.dev/download/). Run it inside a GitHub repository and continue working normally. When CI fails on an open pull request, Watchtower asks AO to investigate the failure, validates the agent's diagnosis, and either waits for your approval or dispatches a narrowly scoped repair automatically.

```text
Pull request CI fails
        ↓
Watchtower detects and deduplicates the failure
        ↓
AO starts one investigator for that PR and commit
        ↓
The investigator submits structured evidence
        ↓
Human review mode ── or ── explicit auto-fix policy
        ↓
AO tests, commits, and non-force pushes the scoped fix
        ↓
GitHub reruns CI; Watchtower watches the repair commit
        ↓
The outcome is recorded: verified green, still failing, or unverified
```

Watchtower does not call an LLM API directly. AO owns coding-agent authentication, isolated worktrees, PR ownership, and agent execution. Watchtower owns event intake, policy, approval, idempotency, and auditing. It uses only AO's public `ao` CLI and never reads or modifies AO's private database.

## What is implemented

- One self-hosted Go binary with no frontend build step, on Linux, macOS, and Windows
- One-command setup for an existing GitHub repository
- GitHub check polling through the user's authenticated `gh` CLI
- Optional verified `check_suite.completed` webhook intake
- Automatic AO project discovery or registration
- Exactly one AO investigator per repository, PR, head SHA, and rule
- Strict diagnosis schema with bounded evidence
- Review mode with explicit approval
- Opt-in auto-fix for high-confidence code diagnoses
- Scoped test, commit, and non-force push to the existing PR branch
- **Repair verification**: the repair commit is watched until CI settles, and the
  outcome is recorded as verified green, still failing, or unverified
- **Repository-owned policy** in `.watchtower.json`: path allowlists, denylists,
  a confidence floor, and a blast-radius limit that automation cannot loosen
- **Runaway guardrails**: a concurrent-investigation limit and a rolling daily
  fix budget, with held-back triggers replayed rather than dropped
- **Pull request comments** (opt-in) carrying the diagnosis and the final outcome
- Closed, merged, moved-head, ownership, and duplicate-dispatch safeguards
- Auditable single-shot retry for a dispatch that failed to reach AO
- Durable SQLite audit ledger, a local dashboard, and `watchtower stats`
- Global automation kill switch

The detailed behavioral contract is in [docs/MVP.md](docs/MVP.md).

## Requirements

Before running Watchtower, you need:

1. Agent Orchestrator installed and running.
2. A coding agent such as Codex authenticated inside AO.
3. Git installed.
4. GitHub CLI installed and authenticated.
5. A local clone of a GitHub repository with CI configured for pull requests.
6. Go 1.23 or newer when building from source.

Check the important dependencies:

```sh
ao status
gh auth status
git --version
go version
```

If `ao` is not on `PATH`, Watchtower also checks common Agent Orchestrator desktop installation paths. You can always provide it explicitly with `--ao /absolute/path/to/ao`.

## Install

### Install from the public repository

```sh
go install github.com/shitcodebykaushik/ao-watchtower/cmd/watchtower@latest
```

If the shell reports `watchtower: command not found`, add Go's binary directory to `PATH`:

```sh
export PATH="$PATH:$(go env GOPATH)/bin"
```

Add the same export to your shell configuration if you want it available after restarting the terminal.

### Build from a clone

```sh
git clone https://github.com/shitcodebykaushik/ao-watchtower.git
cd ao-watchtower
go build -o watchtower ./cmd/watchtower
```

Run that local binary as `./watchtower`. It is a file, not a directory.

## Quick start

Enter the repository you want to monitor:

```sh
cd /path/to/your/repository
watchtower up --auto-fix
```

Or use a locally built binary:

```sh
/path/to/ao-watchtower/watchtower up --auto-fix
```

The first run automatically:

1. Reads the GitHub repository from `origin`.
2. Verifies the existing `gh` login.
3. Verifies AO is ready.
4. Finds the AO project for the checkout or registers one.
5. Generates independent webhook, callback, and dashboard secrets.
6. Stores private state with mode `0600` under the user config directory.
7. Creates or reuses a durable SQLite ledger.
8. Starts the dashboard on `127.0.0.1:8787`.
9. Checks open PR results every five seconds.

Example startup:

```text
Created Watchtower setup for octocat/calculator
AO project: calculator
Dashboard: http://127.0.0.1:8787
Admin token: ...
Mode: AUTO-FIX (code diagnoses at or above 80%)
Monitoring GitHub every 5s; keep this command running.
```

Keep this terminal running. Watchtower is a foreground local process and stops when you press Ctrl+C.

## Choose a safety mode

### Review mode

```sh
watchtower up
```

Watchtower investigates automatically but waits before requesting a code change. Open the printed dashboard, enter the admin token, review the diagnosis, and use **Approve** followed by **Fix with AO**.

Use this mode for unfamiliar, important, or production repositories.

### Auto-fix mode

```sh
watchtower up --auto-fix
```

The flag is an explicit repository-scoped approval policy. Watchtower acts automatically only when all of these are true:

- The diagnosis passes the strict schema.
- Its category is `code`.
- Its recommended action is `fix_code`.
- It contains concrete evidence.
- Confidence is at least 80% by default.
- AO successfully owns the PR without takeover.
- The global kill switch is not enabled.
- The PR remains open and its head has not unexpectedly moved.
- No fix was previously dispatched for the same trigger.

Infrastructure, flaky, test-only, configuration, dependency, unknown, malformed, and low-confidence diagnoses remain visible for human review. Auto-fix never merges the PR and never force-pushes.

Change the threshold when needed:

```sh
watchtower up --auto-fix --auto-fix-confidence 0.9
```

## Copy-paste local test

The following small Go repository provides an easy end-to-end test.

### 1. Create a working calculator repository

```sh
mkdir calculator-watchtower
cd calculator-watchtower
git init -b main
go mod init example.com/calculator-watchtower
mkdir -p .github/workflows
```

Create `calculator.go`:

```go
package calculator

func Add(a, b int) int {
	return a + b
}
```

Create `calculator_test.go`:

```go
package calculator

import "testing"

func TestAdd(t *testing.T) {
	if Add(2, 3) != 5 {
		t.Fatal("Add(2, 3) should equal 5")
	}
}
```

Create `.github/workflows/ci.yml`:

```yaml
name: CI

on:
  pull_request:

jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - run: go test ./...
```

Verify and publish the working base branch:

```sh
go test ./...
git add .
git commit -m "chore: create calculator test project"
gh repo create calculator-watchtower --public --source=. --remote=origin --push
```

The workflow must be committed to the base branch before creating the broken PR.

### 2. Start Watchtower

In terminal A:

```sh
cd /path/to/calculator-watchtower
watchtower up --auto-fix
```

Keep terminal A open.

### 3. Open an intentionally broken PR

In terminal B:

```sh
cd /path/to/calculator-watchtower
git switch -c demo/broken-addition
```

Change `calculator.go` from:

```go
return a + b
```

to:

```go
return a - b
```

Confirm the failure, commit it, and create the PR:

```sh
go test ./...
git add calculator.go
git commit -m "demo: introduce addition regression"
git push -u origin demo/broken-addition
gh pr create \
  --base main \
  --head demo/broken-addition \
  --title "Demo: broken calculator addition" \
  --body "Intentional CI failure for testing AO Watchtower."
```

The local `go test` failure is expected. GitHub Actions will run after the PR is created. Once it finishes with `failure`, Watchtower should detect it within approximately five seconds.

### 4. Observe the result

Expected flow:

```text
GitHub test fails
→ Watchtower reserves one trigger
→ AO creates a ci-investigator session
→ AO reports calculator.go and TestAdd as evidence
→ Watchtower validates and auto-approves the diagnosis
→ AO restores addition and runs go test ./...
→ AO commits and pushes to demo/broken-addition
→ GitHub Actions becomes green
```

Check the PR from a terminal:

```sh
gh pr checks --watch
```

The PR remains open for human review and merging.

## Commands

```sh
watchtower up [options]       # initialize if necessary, then monitor
watchtower init [options]     # initialize without starting the server
watchtower status [options]   # show repository, AO project, URL, and health
watchtower stats [options]    # show durable repair outcomes for this repository
watchtower serve -config FILE # run continuously reachable webhook mode
watchtower demo               # run a fake-AO UI demonstration
watchtower version
watchtower help
```

Useful `up` options:

```text
--repo PATH                 repository to monitor; default is current directory
--listen HOST:PORT          dashboard address; default is 127.0.0.1:8787
--poll-interval DURATION    GitHub check interval; default is 5s
--auto-fix                  enable repository-scoped automatic repair
--auto-fix-confidence N     required confidence from 0 through 1
--auto-fix-actor NAME       audit identity for automatic approvals
--comment                   publish the diagnosis and outcome on the pull request
--max-investigations N      concurrent AO investigators; default 3, 0 disables the limit
--daily-fix-budget N        automatic fixes per rolling 24h; default 20, 0 disables
--verify-timeout DURATION   how long to watch a repair before recording it unverified
--ao PATH                   explicit AO executable
--gh PATH                   explicit GitHub CLI executable
--state-dir PATH            explicit private state directory
```

## Did the fix actually work?

A dispatched repair is not the end of the story. When AO pushes to the PR branch,
Watchtower keeps watching that pull request and settles the repair against the new
head commit:

| Outcome | Meaning |
| --- | --- |
| `verified_green` | CI completed successfully on the repair commit |
| `still_failing` | CI still failed on the repair commit |
| `abandoned` | `--verify-timeout` expired first: the PR was closed, no repair commit arrived, or the conclusion never settled |

The dashboard shows this as the row's status, and `watchtower stats` aggregates it:

```sh
watchtower stats
```

```text
Repository: octocat/calculator
Failures seen:        14
Investigated:         14 (0 claim conflicts)
Valid diagnoses:      12
Approved:             9
Fixes dispatched:     9
Verified green:       7
Still failing:        1
Unverified:           1 (0 awaiting)
Repair success rate:  88%
Median time to green: 6m12s
```

Verification reuses the poll the monitor already performs, so it costs no extra
GitHub API budget.

## Repository policy

Auto-fix lets an agent modify code. A repository can constrain that itself by
committing `.watchtower.json` at its root:

```json
{
  "version": 1,
  "autoFix": {
    "minimumConfidence": 0.9,
    "allowedPaths": ["internal/**", "cmd/**"],
    "deniedPaths": [".github/**", "**/*.tf", "internal/billing/**"],
    "allowedCategories": ["code"],
    "requireEvidenceFile": true,
    "maxEvidenceFiles": 5
  }
}
```

Rules:

- Every field is optional; a missing file means no repository constraint at all.
- A policy may only **tighten** the operator's flags, never loosen them. The
  effective confidence floor is the stricter of the file and `--auto-fix-confidence`.
- `deniedPaths` beats `allowedPaths`. Patterns support `**` for any depth and `*`
  within one path segment.
- Evidence paths come from an agent and are treated as hostile: absolute paths,
  `..` traversal, and backslash paths are denied rather than normalized.
- A malformed policy is a startup error. A repository that clearly intended a
  policy is never silently downgraded to permissive.

Refusals are logged and the diagnosis stays in the dashboard for a human to
approve deliberately.

## Limits

Twenty failing pull requests should not become twenty coding-agent sessions.

- `--max-investigations` (default 3) bounds concurrent AO investigators. A trigger
  held back keeps its durable reservation and is replayed automatically once
  capacity frees up — the same mechanism also recovers a reservation whose process
  stopped before it reached AO.
- `--daily-fix-budget` (default 20) bounds how many automatic fixes may be
  dispatched in a rolling 24 hours. It is checked immediately before each dispatch,
  so a burst of eligible diagnoses cannot overshoot it.

## Pull request comments

```sh
watchtower up --auto-fix --comment
```

Watchtower posts the validated diagnosis to the pull request and later edits in
its own outcome comment, through the same `gh` login it already uses. Comments are
idempotent: rerunning never produces a duplicate. Agent-authored text is
neutralized before publication, so a diagnosis cannot forge Watchtower's own
comment markers.

## Local state

By default, Linux state is stored at:

```text
~/.config/ao-watchtower/<github-owner>/<repository>/
├── state.json       # mode 0600; contains local secrets and AO mapping
└── watchtower.db    # durable event and action ledger
```

macOS uses `~/Library/Application Support/ao-watchtower/…` and Windows uses
`%AppData%\ao-watchtower\…`. Windows has no POSIX mode bits, so the equivalent
protection is an explicit access control list granting only the current user;
Watchtower applies it on write and refuses to load state that any other account
can reach.

Do not commit or share `state.json`. The dashboard admin token is printed on startup. Investigator callback tokens are scoped to one trigger and authorize diagnosis submission only; they cannot approve fixes, change the kill switch, or invoke other dashboard mutations.

## Troubleshooting

### `watchtower: command not found`

Run the binary by absolute path or add the Go binary directory to `PATH`:

```sh
export PATH="$PATH:$(go env GOPATH)/bin"
```

When using a binary in the current directory, include `./`:

```sh
./watchtower help
```

### `cd: not a directory: watchtower`

`watchtower` is an executable file. Run it; do not `cd` into it.

### `bind: address already in use`

Usually another Watchtower instance is already running. Check:

```sh
watchtower status
```

Use the existing dashboard or stop the old process with Ctrl+C. If another application owns the port, choose another address:

```sh
watchtower up --listen 127.0.0.1:8888 --auto-fix
```

The explicit address is saved for later runs.

### GitHub Actions does not start

Confirm that:

- `.github/workflows/ci.yml` is committed to the base branch.
- The workflow contains `on: pull_request`.
- Actions are enabled in the GitHub repository settings.
- The PR targets the branch containing the workflow.

Inspect the PR:

```sh
gh pr view --web
gh pr checks
```

### CI fails but Watchtower does nothing

Keep `watchtower up` running until CI completes. Then check:

```sh
watchtower status
gh auth status
ao status
gh pr list --state open
```

Watchtower monitors open PRs only. A closed or merged PR is not repaired.

### AO does not create an investigator

Verify AO and the project mapping:

```sh
ao status
ao project ls
watchtower status
```

If another AO session already owns the PR, Watchtower records a claim conflict rather than taking over.

### The fix is not automatic

Plain `watchtower up` is review mode. Use the explicit flag:

```sh
watchtower up --auto-fix
```

Even in auto-fix mode, unsupported or low-confidence diagnoses intentionally wait for review.

## Webhook/server mode

Local polling is the simplest path and requires no tunnel, GitHub App, or webhook configuration. A continuously reachable deployment can instead receive signed GitHub webhooks.

Create `watchtower.json`:

```json
{
  "repositoryProjects": [
    {
      "repository": {"owner": "octo", "name": "repo"},
      "aoProjectId": "your-ao-project"
    }
  ],
  "sqlitePath": "./watchtower.db",
  "callbackBaseURL": "https://watchtower.example"
}
```

Keep secrets in the environment:

```sh
export WT_WEBHOOK_SECRET='github-webhook-secret'
export WT_CALLBACK_SECRET='separate-random-callback-secret'
export WT_ADMIN_TOKEN='local-admin-token'
watchtower serve -config ./watchtower.json
```

Configure GitHub to send `check_suite` events to:

```text
POST https://watchtower.example/webhooks/github
```

Other overrides are `WT_LISTEN`, `WT_SQLITE_PATH`, `WT_AO_EXECUTABLE`, `WT_CALLBACK_BASE_URL`, and `WT_CONFIG`. `GET /healthz` is the liveness endpoint and `/` is the dashboard.

## Demo mode

```sh
watchtower demo
```

Demo mode uses a fake AO boundary, a unique temporary database, and one signed fixture sent through the real HTTP intake handler. It is visibly labeled and is never selected as a fallback for production automation.

## Architecture

```text
GitHub CLI poller or signed webhook
                 ↓
        authenticated intake
                 ↓
 normalization → policy → SQLite ledger
                 ↓
        AO public CLI adapter
                 ↓
 isolated investigator/fixer session
                 ↓
     existing GitHub pull request
```

Important packages:

| Package | Responsibility |
| --- | --- |
| `internal/polling` | Reads narrow open-PR check facts through `gh` |
| `internal/github` | Verifies and normalizes GitHub check-suite events |
| `internal/policy` | Evaluates the CI-failure investigator rule |
| `internal/repopolicy` | Parses and applies the committed `.watchtower.json` policy |
| `internal/ledger` | Stores idempotent events, diagnoses, approvals, sends, and outcomes |
| `internal/ao` | Executes the public `ao` CLI using bounded argv-only calls |
| `internal/service` | Coordinates investigation, validation, approval, and repair |
| `internal/scheduler` | Replays reservations that capacity or a restart left unspawned |
| `internal/verification` | Settles whether a dispatched repair turned CI green |
| `internal/automation` | Applies the explicit high-confidence auto-fix policy |
| `internal/prcomment` | Publishes findings to the pull request through `gh` |
| `internal/notify` | Decides what reaches the pull request and when |
| `internal/onboarding` | Discovers GitHub/AO state and creates private local setup |
| `internal/web` | Serves the dashboard shell, state API, approval API, and kill switch |

## Contributing

Clone the repository and run:

```sh
git clone https://github.com/shitcodebykaushik/ao-watchtower.git
cd ao-watchtower
go test -race ./...
go vet ./...
git diff --check
go build -o watchtower ./cmd/watchtower
```

Tests use fake command runners and `httptest`; they do not require a live AO daemon, GitHub account, network, or model API.

Engineering boundaries are documented in [AGENTS.md](AGENTS.md). In particular:

- Never access AO's private database.
- Never turn repository or CI text into shell syntax.
- Keep external mutations idempotent.
- Never take over an already claimed PR.
- Preserve the explicit approval or auto-fix policy boundary.
- Add focused tests for every new external command or durable state transition.

## Current scope

Watchtower intentionally focuses on one high-value workflow: **failed GitHub PR CI → AO investigation → validated diagnosis → supervised repair → verified outcome**.

It does not yet provide GitLab support, hosted multi-tenancy, a general workflow language, automatic merging, monitoring several repositories from one process, or direct model-provider integration. Keeping this boundary narrow makes the demo understandable and the automation auditable.
