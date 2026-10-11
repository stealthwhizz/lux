# ADR 0006: Persistence and durable execution

**Status:** accepted

## Decision
- PostgreSQL is the only source of truth. Schema changes are numbered SQL files embedded in `luxd` and applied in a transaction at startup (`internal/db/migrations`). Applied versions are recorded in `schema_migrations`.
- IDs are prefixed random identifiers (`task_…`, `agt_…`) generated in Go.
- Tasks follow an explicit state machine (`internal/task`). Every transition is a conditional `UPDATE … WHERE state = $expected`, so two actors cannot both move a task.
- Dispatch: the orchestrator claims `queued` tasks with `FOR UPDATE SKIP LOCKED`, records a run row (attempt, provider, handle), and starts the run.
- Recovery after a `luxd` restart: every task in `starting`, `running`, `waiting_approval` or `cancelling` is reconciled with its provider. A live run is re-attached and its events are read from the saved offset; a finished run is finalized from `exit.json`; a lost run is marked `failed` with reason `runtime_lost`, then retried if its retry policy allows.
- Temporal is **not** used yet. Our runs are single long processes whose progress is already persisted as events, and the recovery rules above cover the failure modes we can test today. We will revisit Temporal for multi-step SkillsFlow workflows where step-level replay matters.
- No event bus or object store: events live in PostgreSQL; artifacts live in the run directory on the `luxd` host and are served through the API.

## Consequences
- A crash of `luxd` does not lose a task; a crash of the machine running a `local` run does.
- Artifact storage is host-local; multi-host deployments will need an object store later.
