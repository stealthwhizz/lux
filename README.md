# Lux

**A personal AI agent for everyone at your company: self-hosted, always on, and it asks before it acts.**

Lux gives each person agents that work for them around the clock: planning their day from their calendar, keeping on top of email and Slack, preparing documents, running routines on a schedule. The agents run on your own Lux server, not on a laptop, so they keep working when the laptop is closed. Anything that changes the outside world, like sending an email or posting in Slack, waits for the person's approval.

Every Lux agent is a [gitagent](https://github.com/open-gitagent/gitagent): a folder with an `agent.yaml` and instructions, versioned in git and validated against the OpenGAP spec. You can make one in the app with one click, or import one from a repository.

> Lux is early software. It works end to end, but it has not been released yet, and some parts are marked "not verified" below. Read [What works and what doesn't](#what-works-and-what-doesnt) before you rely on it.

## What it does

- **One click to a new agent.** Press **+**, then **Create an agent**. The agent greets you, asks what it should do, and suggests first jobs as buttons. After your first message it names itself (for example "Calendar Planner").
- **Chat-first desktop app.** Each agent is one ongoing conversation. Agents ask questions with answer buttons, show what they are doing as they work, and put their work in files you can open and save.
- **Asks before it acts.** Workspace rules decide what an agent may do alone and what needs a person. Approvals appear inline in the chat and in **Needs you**, and maker-checker (a second person approves) is available. See [docs/policy.md](docs/policy.md).
- **Always on.** Tasks, routines and approvals live on the server. Closing the app or the laptop does not stop them.
- **Routines.** "Every weekday at 8:00, summarise my inbox", posted into the agent's thread.
- **Connectors.** People connect their own Google (Gmail, Calendar, Drive), Microsoft 365 (Outlook, calendar, OneDrive) and Slack accounts. Agents act as that person and see only what that person can see. Tokens are encrypted on the server and never reach the agent. See [docs/connectors.md](docs/connectors.md).
- **Any model.** Lyzr, Anthropic, OpenAI or a local model through Ollama. Administrators add keys in the app and choose the model new agents use. Keys stay in the server. See [docs/models.md](docs/models.md).
- **Attachments and files.** Attach documents to a message (Word and PDF are converted to text for the agent). Each agent keeps a workspace folder between conversations.
- **Sandboxed runs.** On Linux, agents run inside bubblewrap: no access to home directories, other agents or the server's data.
- **Git workflows.** Agents can work on repositories; their changes go to review and can be published as pull requests. See [docs/git-workflows.md](docs/git-workflows.md).

## How it fits together

```
 Desktop app (Electron)        lux (command line)
          │                          │
          └──────── HTTPS / SSE ─────┘
                       │
                   luxd (Go)  ── PostgreSQL (tasks, events, approvals, settings)
          ┌────────────┼──────────────┬───────────────────┐
     orchestrator   model gateway   connectors (MCP)    policy
          │            │               │
   sandboxed run   Lyzr / Anthropic   Google / Microsoft 365 / Slack
   (gitagent)      OpenAI / Ollama
```

- **luxd** is the server: API, orchestrator, model gateway, connectors and policy, in one Go binary.
- **The runner** (`runtime/gitagent-runner`) runs one gitagent task. It reaches models only through luxd's gateway, and connector tools only through luxd's MCP endpoint, with a short-lived run token. It never holds a provider key or an account token.
- **The desktop app** (`desktop/`) and **`lux`** are clients of the same API.

Design decisions are recorded in [docs/adr](docs/adr/README.md).

## Quick start (on your own computer)

You need Go 1.27+, Node.js 20+, git, and PostgreSQL 15+ (a container is fine).

```bash
podman run -d --name lux-dev-db -e POSTGRES_USER=lux -e POSTGRES_PASSWORD=lux-dev -e POSTGRES_DB=lux -p 127.0.0.1:55432:5432 docker.io/library/postgres:17-alpine
```

Build the server, the command line and the agent runner:

```bash
go build -o bin/ ./cmd/... && (cd runtime/gitagent-runner && npm ci)
```

Start the server. The database address is saved, so later starts are just `bin/lux server start`:

```bash
bin/lux server start --database-url 'postgres://lux:lux-dev@127.0.0.1:55432/lux?sslmode=disable'
```

Model keys can be added in the app later, or kept in a private settings file that `lux server start` reads (fill in the keys you use, then restart the server with `bin/lux server stop` and `bin/lux server start`):

```bash
install -m 600 .env.example ~/.config/lux/luxd.env
```

Start the desktop app:

```bash
cd desktop && npm ci && npm start
```

In the app, press **+** and **Create an agent**. The first time, an administrator (you, on your own server) picks the model agents use under **… → Models**. With no API key at all, you can use a local model through [Ollama](docs/models.md#ollama).

To run Lux for a team, see [docs/self-hosting.md](docs/self-hosting.md).

## Documentation

| Guide | What's in it |
|---|---|
| [Using Lux](docs/using-lux.md) | The desktop app: agents, chats, approvals, files, routines |
| [Connectors](docs/connectors.md) | Connecting Google, Microsoft 365 and Slack, and what agents can do with them |
| [Models](docs/models.md) | Providers, keys, the default model, Lyzr agents, Ollama |
| [Policy and approvals](docs/policy.md) | What agents may do alone, and how approvals work |
| [Self-hosting](docs/self-hosting.md) | Running luxd for a team: Compose, systemd, TLS, backups, configuration |
| [The `lux` command](docs/cli.md) | Every command |
| [Git workflows](docs/git-workflows.md) | Agents that work on repositories |
| [Agent validation](docs/agent-validation.md) | How Lux checks an agent before it runs |
| [Runtime protocol](docs/runtime-protocol.md) | How luxd and the runner talk |
| [Development](docs/development.md) | Building and testing Lux |

## What works and what doesn't

Verified end to end with real services:
- Agents running on Lyzr (a manager agent with sub-agents): multi-turn conversations, files kept between turns, attachments, routines, approvals.
- A local llama3.2 model through Ollama runs and names agents. A 3B model is too small to use agent tools reliably, so use a larger one.

Tested only against stand-in services (no real accounts were available):
- The Google, Microsoft 365 and Slack connectors.
- The Anthropic and OpenAI providers (Lyzr and Ollama were tested for real).

Not built yet:
- Signed releases and a one-line installer. For now, build from source.
- Sign-in by email or phone. People sign in with an API token.
- Push notifications for approvals.
- Network isolation for sandboxed runs. They share the host's network.
- Agents are shared within a workspace. Per-person private agents are not built yet.

## Contributing and security

Contributions are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md). To report a vulnerability, follow [SECURITY.md](SECURITY.md); please don't open a public issue.

## License

[MIT](LICENSE)
