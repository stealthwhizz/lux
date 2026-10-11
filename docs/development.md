# Development

## Prerequisites
- Go 1.27 or newer
- Git
- Node.js 20 or newer (runs gitagent agents)
- PostgreSQL 15 or newer, either in a container (below) or installed natively

## Database
With Podman or Docker:

```bash
podman run -d --name lux-dev-db -e POSTGRES_USER=lux -e POSTGRES_PASSWORD=lux-dev -e POSTGRES_DB=lux -p 127.0.0.1:55432:5432 docker.io/library/postgres:17-alpine
```

## Install the agent runtime
`luxd` runs agents with the gitagent runner in `runtime/gitagent-runner`:

```bash
cd runtime/gitagent-runner && npm ci
```

`luxd` finds it automatically when started from the repository root, or set `LUX_RUNNER_CMD` (for example `node /opt/lux/gitagent-runner/src/runner.mjs`). `/v1/status` reports `runtime: ok` or why it is missing.

## Run luxd locally

```bash
export LUX_DATABASE_URL='postgres://lux:lux-dev@127.0.0.1:55432/lux?sslmode=disable'
go run ./cmd/luxd
```

On first start in local mode, `luxd`:
- applies migrations;
- creates an owner named after `$USER` and a `default` workspace;
- writes an API token to `~/.config/lux/credentials.yaml` (mode 0600) under the `local` environment.

It listens on `127.0.0.1:7878` and refuses to listen on other interfaces unless `LUX_MODE=server`.

Check it:

```bash
curl -s localhost:7878/readyz
```

```bash
curl -s localhost:7878/v1/status
```

The API is described in `internal/api/openapi.yaml`, also served at `/v1/openapi.yaml`.

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `LUX_DATABASE_URL` | (required) | PostgreSQL connection URL |
| `LUX_MODE` | `local` | `local` (loopback only, auto-bootstrap, local paths and executor allowed) or `server` |
| `LUX_LISTEN` | `127.0.0.1:7878` | Listen address |
| `LUX_PUBLIC_URL` | derived from `LUX_LISTEN` | URL runtimes use to reach luxd |
| `LUX_DATA_DIR` | `~/.local/share/lux` | Repository mirrors, temporary checkouts, run directories |
| `LUX_CONFIG_DIR` | `~/.config/lux` | Client config and credentials |
| `LUX_ALLOW_LOCAL_SOURCES` | `false` in server mode | Allow importing agents from paths on the server |
| `LUX_ALLOW_LOCAL_EXECUTOR` | `false` in server mode | Allow the non-isolated local executor |
| `LUX_OPENGAP_CMD` | unset | Reference validator, e.g. `npx --no-install opengap` |
| `LUX_RUNNER_CMD` | auto-detected | Command that runs the agent runtime; the spec path is appended |
| `LUX_MAX_CONCURRENT_RUNS` | `4` | Runs this luxd executes at once |
| `LUX_ANTHROPIC_API_KEY` / `LUX_OPENAI_API_KEY` / `LUX_LYZR_API_KEY` | unset | Provider keys for the model gateway (the unprefixed `ANTHROPIC_API_KEY` etc. also work). Never passed to agents |
| `LUX_<PROVIDER>_BASE_URL` | provider default | Override a provider endpoint (e.g. a corporate proxy) |
| `LUX_ANTHROPIC_WORKSPACE_ID` | unset | Sent as `anthropic-workspace-id` for keys that span workspaces |
| `LUX_OLLAMA_DISABLED` | `false` | Turn off the keyless local Ollama provider |
| `LUX_AUTO_MIGRATE` | `true` | Set `false` to require `luxd migrate` before start |
| `LUX_LOG_FORMAT` | `text` locally, `json` in server mode | Log format |

In server mode, create the first owner with `luxd bootstrap`. It prints the token once on stdout.

## Tests
Unit tests need nothing else:

```bash
go test ./...
```

Runner tests (Node, no database):

```bash
cd runtime/gitagent-runner && npm test
```

Integration tests (store, migrations, API, event hub, orchestrator) need a PostgreSQL role that may create databases. Each test creates its own database and drops it afterwards. Without `LUX_TEST_DATABASE_URL` they are skipped; with `LUX_REQUIRE_DB=1` they fail instead, which CI must set. The end-to-end test (`internal/e2e`) also needs the runner installed; `LUX_REQUIRE_RUNNER=1` makes its absence a failure.

```bash
LUX_TEST_DATABASE_URL='postgres://lux:lux-dev@127.0.0.1:55432/lux?sslmode=disable' LUX_REQUIRE_DB=1 LUX_REQUIRE_RUNNER=1 go test -race ./...
```

Desktop unit and end-to-end tests are described in [CONTRIBUTING.md](../CONTRIBUTING.md#before-you-open-a-pull-request).

To check the validator against real agent repositories:

```bash
LUX_TEST_AGENT_DIRS=/path/to/agent-a:/path/to/agent-b go test -run TestValidateRealAgents -v ./internal/agentrepo
```

## What exists today
- **Server (`luxd`):** API with role checks and workspace isolation; agents (imported from git or made in Lux, versioned and validated); tasks with live events (Server-Sent Events); conversations, one per agent and person; approvals with maker-checker; routines; per-agent workspace folders kept between runs; attachments; deleting and turning off agents; audit log.
- **Orchestrator:** claims queued tasks, runs them with the bubblewrap sandbox or the local executor and the gitagent runner, ingests events, enforces cancellation, timeouts and the concurrency limit, retries only lost runtimes, and re-attaches to running tasks after a restart. See [runtime-protocol.md](runtime-protocol.md).
- **Model gateway:** Lyzr, Anthropic, OpenAI and Ollama; declared-models-only routing, server-side keys (from settings or added in the app), budgets, rate limits, opt-in fallback, usage recording. See [models.md](models.md) and [ADR 0009](adr/0009-model-gateway.md).
- **Connectors:** Google, Microsoft 365 and Slack, served to runs over MCP. See [connectors.md](connectors.md).
- **Clients:** the `lux` command ([cli.md](cli.md)) and the Electron app ([using-lux.md](using-lux.md)).
- **Verified against real services:** Lyzr and Ollama. Anthropic, OpenAI, Google, Microsoft and Slack are tested against scripted stand-ins only.
- **Not yet:** releases and an installer, sign-in by email or phone (people use API tokens), network isolation for sandboxed runs, per-person private agents. Gateway events (`lux.model_call`) are written immediately, while runtime events are read from the run's event file every 250 ms, so the timeline is ordered by arrival time.

## Known gitagent issues worked around in the runner
The runner pins `@open-gitagent/gitagent` 2.2.0 (the latest release). Two problems in it are handled on the Lux side; both should be fixed upstream:
- **No `runtime:` block crashes it.** `sdk.ts` and `index.ts` read `manifest.runtime.timeout` without a guard, so an agent.yaml without a `runtime:` block (valid per the OpenGAP spec) crashes gitagent. The runner adds `runtime: {}` to the run's private copy only and records a `runtime.compat` event.
- **`max_turns` is not enforced.** The fix exists in the gitagent repository but is unreleased. The runner counts turns and stops the agent at the limit.
