# ADR 0002: Local service architecture

**Status:** accepted

## Context
Local development must not need a cluster or a hosted account, and a task must keep running when the desktop app closes.

## Decision
- A single Go binary, `luxd`, contains the API, orchestrator, scheduler, model gateway and execution-provider coordination as internal packages. No microservices.
- In local mode `luxd` binds to `127.0.0.1:7878`, runs as a background process started by `lux server start` (or the desktop app), and survives the desktop app closing.
- On first start in local mode it bootstraps an owner account and writes an API token to the user's credentials file. In server mode the operator creates the first token with `luxd bootstrap`.
- Only one orchestrator is active per database, guarded by a PostgreSQL advisory lock. A second `luxd` against the same database serves the API but does not dispatch tasks.

## Consequences
- Simple to run and debug. Horizontal scaling of the orchestrator is out of scope for the first release.
- Requires PostgreSQL even locally (a container or a native install; see `docs/development.md`).
