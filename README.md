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
- AO session discovery and spawning through JSON CLI contracts
- Structured diagnosis with category, confidence, summary, evidence, and action
- Human-controlled `Fix with AO` action
- Server-rendered local dashboard with live status updates
- Global automation kill switch

The detailed contract and acceptance criteria live in [docs/MVP.md](docs/MVP.md).

## Core constraint

Watchtower owns automation policy and auditing. AO owns coding-agent sessions,
isolated workspaces, provider authentication, and agent interaction.

