# Architecture decision records

| ADR | Decision |
|---|---|
| [0001](0001-desktop-architecture.md) | Desktop app: Electron + React; API tokens stay in the main process |
| [0002](0002-local-service.md) | One Go daemon (`luxd`) serves local and self-hosted modes |
| [0003](0003-remote-api.md) | Versioned REST + Server-Sent Events, described by OpenAPI |
| [0004](0004-runtime-interface.md) | Runtime = gitagent driven by a Lux runner over a JSONL file protocol |
| [0005](0005-execution-isolation.md) | Execution providers with an explicit, documented isolation level |
| [0006](0006-persistence.md) | PostgreSQL as the only source of truth; durable task state machine with leases |
| [0007](0007-installer-distribution.md) | Versioned release manifest with SHA-256 verification; user-level install |
| [0008](0008-deployment-modes.md) | Local, self-hosted single instance, enterprise; managed service optional |
| [0009](0009-model-gateway.md) | Model gateway is an OpenAI-compatible proxy inside `luxd` |
| [0010](0010-connectors.md) | Connectors: each server brings its own OAuth apps; per-person tokens stay in luxd and reach agents only as MCP tools |
