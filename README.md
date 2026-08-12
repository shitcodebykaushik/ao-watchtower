# AO Watchtower

**Give Agent Orchestrator reflexes.**

AO Watchtower is a local-first automation companion for Agent Orchestrator. It
turns trusted engineering events into supervised AO agent workflows. The first
workflow is intentionally narrow and demonstrable:

```text
GitHub CI failure
  -> verify and deduplicate the event
  -> detect whether an AO worker already owns the PR
  -> spawn a focused AO investigator when needed
  -> collect an evidence-backed diagnosis
  -> require human approval before requesting a fix
  -> retain a durable audit trail
```

Watchtower is an independent project. It integrates through the public `ao` CLI
and never reads or writes AO's private SQLite database. Its packages are shaped
so the policy engine, durable trigger facts, and UI can later be proposed for
upstream inclusion in AO.

## Hackathon MVP

- One self-hosted Go binary
- Verified GitHub `check_suite.completed` webhook intake
- Repository-to-AO-project configuration
- Durable SQLite event and action ledger
- Idempotent `CI failed -> AO investigator` rule
- AO session discovery and spawning through public AO CLI contracts
- Structured diagnosis with category, confidence, summary, evidence, and action
- Human-controlled `Fix with AO` action
- Server-rendered local dashboard with live status updates
- Global automation kill switch

The detailed contract and acceptance criteria live in [docs/MVP.md](docs/MVP.md).

## Core constraint

Watchtower owns automation policy and auditing. AO owns coding-agent sessions,
isolated workspaces, provider authentication, and agent interaction.


## Run locally

Create a JSON configuration file (secrets stay in the environment):

```json
{"repositoryProjects":[{"repository":{"owner":"octo","name":"repo"},"aoProjectId":"your-ao-project"}],"sqlitePath":"./watchtower.db","callbackBaseURL":"https://public.example/watchtower"}
```

```sh
export WT_WEBHOOK_SECRET='github-webhook-secret'
export WT_CALLBACK_SECRET='separate-random-callback-secret'
export WT_ADMIN_TOKEN='local-admin-token'
go run ./cmd/watchtower -config ./watchtower.json
```

The server defaults to `127.0.0.1:8787`; override it with `WT_LISTEN`. Other
explicit overrides are `WT_SQLITE_PATH`, `WT_AO_EXECUTABLE`, and `WT_CONFIG`.
Production startup fails if any secret, callback URL, or repository mapping is
missing. The webhook is `POST /webhooks/github`; `GET /healthz` is liveness.
The dashboard is at `/`. Configuration is JSON plus environment overrides; no
AO database or private API is accessed.

Dashboard mutations use `Authorization: Bearer $WT_ADMIN_TOKEN`:

```sh
curl -X POST -H "Authorization: Bearer $WT_ADMIN_TOKEN" -H 'content-type: application/json' \
  -d '{"actor":"local-user","disabled":true}' http://127.0.0.1:8787/api/automation
curl -X POST -H "Authorization: Bearer $WT_ADMIN_TOKEN" -H 'content-type: application/json' \
  -d '{"actor":"local-user"}' 'http://127.0.0.1:8787/api/triggers?action=approve&trigger=TRIGGER_KEY'
curl -X POST -H "Authorization: Bearer $WT_ADMIN_TOKEN" \
  'http://127.0.0.1:8787/api/triggers?action=fix&trigger=TRIGGER_KEY'
```

Investigators receive a per-trigger callback bearer token in their prompt and
can only `POST` a diagnosis to the scoped callback URL. It never authorizes
approval, sending, or any dashboard mutation.

## Demo

```sh
go run ./cmd/watchtower demo
```

Demo mode visibly labels the UI, creates a unique temporary SQLite ledger, uses
a fake AO lifecycle boundary, and sends one signed fixture through the real HTTP
webhook handler. It is explicit and never selected as a fallback for real AO.
Open `http://127.0.0.1:8787/` after startup. The binary remains a single Go
process: intake normalizes trusted GitHub events, the ledger stores durable
facts, lifecycle invokes the public AO CLI, and `html/template` derives the
local dashboard from ledger reads.
