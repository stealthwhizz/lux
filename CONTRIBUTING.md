# Contributing to Lux

Thanks for helping. This page says how to get set up, what we expect from a change, and how to get it merged.

## Set up

Follow [docs/development.md](docs/development.md): Go, Node.js, git and a PostgreSQL you may create databases in. Then:

```bash
(cd runtime/gitagent-runner && npm ci) && (cd desktop && npm ci)
```

## The repository

| Path | What it is |
|---|---|
| `cmd/luxd`, `cmd/lux` | The server and the command line |
| `internal/api` | HTTP API, connectors' endpoints, the runtime endpoints |
| `internal/orchestrator` | Runs tasks: claiming, executors, events, routines, agent workspaces |
| `internal/gateway` | The model gateway |
| `internal/connectors` | Google, Microsoft 365 and Slack clients and their tools |
| `internal/policy` | Workspace rules and approvals |
| `internal/store`, `internal/db/migrations` | PostgreSQL access and schema |
| `internal/executor` | Local and bubblewrap sandbox executors |
| `internal/agentrepo` | Importing, validating and versioning agents |
| `runtime/gitagent-runner` | Runs one gitagent task for luxd (Node) |
| `desktop` | The Electron app (React, TypeScript) |
| `docs`, `docs/adr` | Guides and design decisions |

## Before you open a pull request

Run the tests for what you changed. All of them, for anything that crosses packages:

```bash
LUX_TEST_DATABASE_URL='postgres://lux:lux-dev@127.0.0.1:55432/lux?sslmode=disable' LUX_REQUIRE_DB=1 LUX_REQUIRE_RUNNER=1 go test -race ./...
```

```bash
cd runtime/gitagent-runner && npm test
```

```bash
cd desktop && npm test
```

The desktop end-to-end tests start a real luxd against an empty database:

```bash
cd desktop && LUX_E2E_DATABASE_URL='postgres://lux:lux-dev@127.0.0.1:55432/lux_desktop_e2e?sslmode=disable' LUX_E2E_BIN="$PWD/../bin" npm run test:e2e
```

Build `bin/` first with `go build -o bin/ ./cmd/...`, and create an empty `lux_desktop_e2e` database for each run.

Also:
- Format Go with `gofmt`. `go vet ./...` must pass. The desktop must typecheck (`npm run typecheck` in `desktop`).
- Add a test for new behaviour. Tests talk to real PostgreSQL and real processes. Stand-in servers are fine for outside services (see the connector and gateway tests), but don't fake success.
- Schema changes are new numbered files in `internal/db/migrations`. Never edit one that has shipped.
- Update the docs your change makes wrong, and add an ADR in `docs/adr` for a decision others will need to know the reason for.

## What we care about

- **Agents ask before they act.** Anything that changes the outside world goes through policy and approvals, checked in luxd, not only in the runtime.
- **Secrets stay in luxd.** Provider keys, account tokens and app secrets never reach an agent's run, its logs or the app. Keep them encrypted at rest.
- **No hardcoded credentials, domains or signing keys.** Everything is configured per server.
- **Plain words in the app.** People who are not developers use it: "Reading planner.md", not "tool.call read".
- **Small changes.** One concern per pull request, with a description of what changed and why, and how you tested it.

## Reporting bugs and asking for features

Open an issue with what you did, what you expected and what happened, plus `lux status` and `lux doctor` output if the server is involved. Remove tokens and keys before you paste logs.

Security problems: please don't open an issue. Follow [SECURITY.md](SECURITY.md).

## License

By contributing you agree that your contribution is licensed under the [MIT License](LICENSE).
