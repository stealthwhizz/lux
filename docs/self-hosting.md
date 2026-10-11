# Self-hosting luxd

This guide runs `luxd` as a shared server for a team. For a single-user install on your own computer, use `lux server start` instead (see [cli.md](cli.md)).

What exists today:

- A container image (`Dockerfile`) with `luxd`, the `lux` command and the gitagent runner.
- A Compose file (`deploy/compose.yaml`) with PostgreSQL.
- A systemd unit (`deploy/systemd/luxd.service`) for running without containers.

What does not exist yet: published images or release binaries, and built-in TLS. Agents run in a bubblewrap sandbox on Linux hosts that support it, but not inside the container image (see [Security](#security)). Read that section before you enable agents on a shared server.

After the server is up, an administrator finishes setup in the desktop app: model keys and the model new agents use under **… → Models** ([models.md](models.md)), and the Google, Microsoft 365 and Slack apps under **… → Connectors** ([connectors.md](connectors.md)). Set `LUX_PUBLIC_URL` to the server's `https://` address first: the connectors' sign-in return addresses are built from it, and Slack only signs in to `https://` addresses.

## With Compose

You need Docker Compose or podman-compose, and a checkout of this repository.

```bash
cd deploy
cp env.example .env
chmod 600 .env
```

Set `LUX_DB_PASSWORD` in `.env`. Use letters and digits only, because it is placed in a URL. `openssl rand -hex 24` gives a suitable value. Then build and start the stack:

```bash
docker compose up -d --build
```

luxd applies database migrations on start. When `curl -s http://127.0.0.1:7878/readyz` reports `"ready":true`, create the first owner. The token is printed once:

```bash
docker compose exec luxd luxd bootstrap --username admin
```

Connect the command line (the desktop app uses the same settings):

```bash
lux env add team http://127.0.0.1:7878
lux env use team
lux login
lux status
```

`lux login` reads the token from standard input.

### Ports and TLS

By default, Compose publishes port 7878 on loopback only. luxd speaks plain HTTP. Before other machines connect to it, put it behind a reverse proxy that terminates TLS, such as Caddy, nginx or your load balancer, and then set `LUX_BIND_ADDRESS` if the proxy runs on another host. Tokens travel in the `Authorization` header, so never expose port 7878 over plain HTTP outside a trusted network.

Event streams use long-lived responses (`text/event-stream`). Turn off response buffering and allow idle connections of at least two minutes on the proxy.

## Configuration

luxd reads its settings from the environment. In Compose they come from `deploy/.env`.

| Variable | Default | Meaning |
|---|---|---|
| `LUX_DATABASE_URL` | (required) | PostgreSQL URL. Compose builds it from `LUX_DB_PASSWORD`. |
| `LUX_MODE` | `local` (`server` in the image) | `server` allows non-loopback listening, disables local agent sources and the local executor unless allowed, and hides server details from anonymous callers. |
| `LUX_LISTEN` | `127.0.0.1:7878` (`0.0.0.0:7878` in the image) | Listen address. |
| `LUX_PUBLIC_URL` | `http://<listen address>` | URL agent runtimes use to reach the API and model gateway. Runtimes run next to luxd, so the default is usually right. |
| `LUX_DATA_DIR` | `$XDG_DATA_HOME/lux` (`/var/lib/lux/data` in the image) | Run directories and agent repository mirrors. |
| `LUX_ALLOW_LOCAL_EXECUTOR` | `false` | Let agents run without isolation in server mode. See [Security](#security). |
| `LUX_ALLOW_LOCAL_SOURCES` | `false` | Allow importing agents from paths on the server. |
| `LUX_MAX_CONCURRENT_RUNS` | `4` | Runs started at the same time (1 to 256). |
| `LUX_AUTO_MIGRATE` | `true` | `false` makes luxd refuse to start on an old schema; then run `luxd migrate` yourself. |
| `LUX_LOG_FORMAT` | `json` in server mode | `json` or `text`. |
| `LUX_RUNNER_CMD` | the bundled runner | Command that starts the agent runtime. |
| `LUX_OPENGAP_CMD` | unset | Optional OpenGAP validator command used during imports. |
| `LUX_ANTHROPIC_API_KEY`, `LUX_OPENAI_API_KEY`, `LUX_LYZR_API_KEY` | unset | Model provider keys. They stay in luxd; agents get a run token instead. An administrator can also add keys in the app (Models); those are stored encrypted with luxd's secret key, and a key set here always wins. With several luxd replicas, others load an in-app key on restart. |
| `LUX_<PROVIDER>_BASE_URL` | the provider's public API | Override an endpoint, for example to use a corporate proxy. |
| `LUX_GIT_CREDENTIAL_<NAME>` | unset | Git token (or `username:token`) that repositories and agents refer to as `<name>`. See [git-workflows.md](git-workflows.md). |
| `LUX_GOOGLE_CLIENT_ID`, `LUX_GOOGLE_CLIENT_SECRET` | unset | Google OAuth client for connectors (Gmail, Calendar, Drive). The redirect address is `<LUX_PUBLIC_URL>/v1/connectors/google/callback`. An administrator can instead enter it in the app; Microsoft 365 and Slack are set up in the app only. See [connectors.md](connectors.md). |
| `LUX_SECRET_KEY` | a key luxd creates in its data folder | 32 bytes, base64. Encrypts connector tokens, app secrets and model keys entered in the app. Back it up with the database: without it, people must connect again and administrators must re-enter keys. |
| `LUX_SANDBOX` | on when bubblewrap works | `off` turns the sandbox executor off. |
| `LUX_GITHUB_API_URL` | `https://api.github.com` | Where pull requests are opened (must be https). |
| `LUX_ANTHROPIC_WORKSPACE_ID` | unset | Shown to operators only. |
| `LUX_OLLAMA_BASE_URL`, `LUX_OLLAMA_DISABLED` | `http://127.0.0.1:11434/v1`, `false` | Ollama endpoint. Inside a container, 127.0.0.1 is the container itself: use the host's address, for example `http://host.containers.internal:11434/v1` with Podman. |

Health endpoints are unauthenticated:

- `/healthz` reports that the process is alive.
- `/readyz` reports that the database answers, the schema matches and the event listener is connected.

In server mode, `/v1/status` shows only the version, mode and whether an owner exists until the caller presents a valid token.

## Security

**Agents run in a bubblewrap sandbox when the host supports it.** On Linux with bubblewrap (`bwrap`) installed and unprivileged user namespaces allowed, luxd offers a `sandbox` executor and new agents use it:
- An agent sees its own run folder (read-write), the system's programs and libraries, and the runner's files (read-only).
- It does not see home folders, other runs, or luxd's data folder.
- It has its own process namespace, so it cannot read other processes' environments.
- It still shares the host's network (a known limit).

Inside a container, bubblewrap usually cannot create namespaces, so the image falls back to the `local` executor below. Set `LUX_SANDBOX=off` to turn the sandbox off. The local executor, without isolation, is off in server mode until you set `LUX_ALLOW_LOCAL_EXECUTOR=true`.

What is protected:

- Provider keys and the database password never enter a runtime's environment. Runtimes get a run token that only works for their own run and stops working when the run ends.
- On Linux, luxd marks itself non-dumpable at start. Processes of the same user can then no longer read its environment or memory through `/proc` or ptrace. This is why the Compose file runs luxd as PID 1 without an init process: an init would hold the same environment unprotected.
- Tool calls go through the workspace policy and approvals (see [policy.md](policy.md)), checked by the runner.

What is not protected while an agent runs:

- Agent code can do anything the luxd user can. That includes reading the data directory, which holds every imported agent repository and other runs' working directories.
- It can read the environment of other runtimes running at the same time, including their run tokens.
- It can use the network, and stop luxd.
- The policy check runs in the same environment, so code that a tool already runs (an approved shell command, a hook) is not checked again.
- `docker compose exec` sessions carry the container's full environment while they run.

Enable the local executor only for agent repositories you trust as much as the server itself. Network isolation for the sandbox and container-based executors are planned (see [ADR 0005](adr/0005-execution-isolation.md)).

## Backups

Back up two things: the PostgreSQL database and, if you need run logs and working directories, the data volume. Agent repositories can be imported again from their sources.

```bash
docker compose exec -T postgres pg_dump -U lux -Fc lux > lux-$(date +%F).dump
```

To restore into a fresh stack, start only PostgreSQL, restore, then start luxd:

```bash
docker compose up -d postgres
docker compose exec -T postgres pg_restore -U lux -d lux --clean --if-exists --no-owner < lux-2026-01-31.dump
docker compose up -d
```

The audit log is append-only (a database trigger rejects updates and deletes). `pg_restore --clean` drops and recreates the table, which is expected.

## Upgrades

1. Back up the database.
2. Update the checkout and rebuild: `docker compose up -d --build`.
3. luxd applies new migrations on start, under a database lock, so only one instance migrates.

Downgrades are not supported. Restore the backup instead.

Restarting the luxd container stops the agents running inside it. When luxd comes back, it treats those runs as lost: it retries them if the agent allows more attempts, and otherwise marks them failed. Queued tasks are not affected.

## Without containers (systemd)

Requirements:

- Linux with systemd.
- PostgreSQL 15 or newer.
- git.
- Node.js 20 or newer.

Install the files:

```bash
go build -o bin/ ./cmd/luxd ./cmd/lux
sudo install -m 0755 bin/luxd bin/lux /usr/local/bin/
sudo install -d /usr/local/lib/lux
sudo cp -r runtime/gitagent-runner /usr/local/lib/lux/
(cd /usr/local/lib/lux/gitagent-runner && sudo npm ci --omit=dev --ignore-scripts)
sudo useradd --system --home-dir /var/lib/lux --shell /usr/sbin/nologin lux
sudo install -d -m 0700 /etc/lux
sudo install -m 0600 /dev/null /etc/lux/luxd.env   # then add LUX_DATABASE_URL and the other settings
sudo cp deploy/systemd/luxd.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now luxd
```

Create the first owner with the same settings. The token is printed once:

```bash
sudo systemd-run --uid=lux -p EnvironmentFile=/etc/lux/luxd.env --pipe --wait /usr/local/bin/luxd bootstrap
```

`/etc/lux/luxd.env` is read by systemd as root, so it can stay readable by root only.

The unit uses `KillMode=process`: stopping or restarting luxd leaves running agents alone, and the next luxd re-attaches to them.
