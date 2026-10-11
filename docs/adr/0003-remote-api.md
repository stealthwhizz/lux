# ADR 0003: Remote API

**Status:** accepted

## Decision
- REST over HTTPS under `/v1`, JSON bodies, bearer-token auth. Described in `api/openapi.yaml`, served at `/v1/openapi.yaml`.
- Live updates use Server-Sent Events (`/v1/events`, `/v1/tasks/{id}/events`) with a numeric cursor, so a client that reconnects resumes from the last event it saw. Events are persisted first and then fanned out with PostgreSQL `LISTEN/NOTIFY`.
- Task creation accepts an `Idempotency-Key` header; a retry with the same key returns the original task.
- Errors use one shape: `{"error": {"code": "...", "message": "..."}}`.
- The runtime talks to separate, run-scoped endpoints under `/internal/v1` and `/gateway/v1` with a short-lived run token that cannot call the user API.
- gRPC is not used. It would add a second contract without a concrete need yet.

## Consequences
- The CLI, the desktop app and third-party tools all use the same API.
- SSE is one-directional; all writes are normal REST calls.
