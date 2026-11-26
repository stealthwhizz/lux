# Lux

**A personal AI agent for everyone at your company. Self-hosted, always on, and it asks before it acts.**

[![Lux launch video: a personal AI agent for everyone at your company](docs/assets/lux-launch.jpg)](docs/assets/lux-launch.mp4)

<p align="center"><a href="docs/assets/lux-launch.mp4">▶ Watch the 23-second launch video</a></p>

Lux gives each person agents that work for them: they plan the day from the calendar, keep on top of email and Slack, prepare documents and run routines on a schedule. The agents run on your Lux server, not on a laptop, so they keep working when the laptop is closed. Anything that changes the outside world, like sending an email or posting in Slack, waits for the person's approval.

Every Lux agent is a [gitagent](https://github.com/open-gitagent/gitagent): a folder with an `agent.yaml` and instructions, versioned in git and checked against the OpenGAP spec. Make one in the app with one click, or import one from a repository.

> Lux is early software. It works end to end but has not been released, and some parts have only been tested against stand-in services. Read [What works and what doesn't](#what-works-and-what-doesnt) before you rely on it.

| Asks before it acts | Keeps its own Work panel | Take over its browser |
|---|---|---|
| ![An approval card in the chat](screenshots/01-asks-before-acting.png) | ![The Work panel](screenshots/02-work-panel.png) | ![The live browser view](screenshots/13-browser-live-view.png) |

## What it does

**Works for you, with your OK**
- **Asks before it acts.** Workspace rules decide what an agent may do alone and what needs a person. Approvals show up in the chat and in **Needs you**. A second approver (maker-checker) can be required. See [docs/policy.md](docs/policy.md).
- **Always on.** Tasks, routines and approvals live on the server. Closing the app or the laptop does not stop them.
- **Owns the work, not just the chat.** Each agent keeps a **Work** panel up to date: its job, what it's doing, what it needs from you, decisions and deadlines. It schedules its own follow-ups and keeps a **Memory** of your preferences that you can read and edit.
- **Routines.** "Every weekday at 8:00, summarise my inbox", posted into the agent's chat.

**Easy to start**
- **One click to a new agent.** Press **+**, then **Create an agent**. The agent greets you, asks what it should do and suggests first jobs as buttons. After your first message it names itself, for example "Calendar Planner".
- **A chat for each agent.** Agents ask questions with answer buttons, show what they are doing as they work, and save their work as files you can open.
- **Reach it where you are.** Desktop notifications when an agent needs you or replies, and a **Lux bot in Slack**: talk to your agents in a DM and approve with a button.

**Connected to your tools**
- **Connectors.** Each person connects their own Google (Gmail, Calendar, Drive), Microsoft 365 (Outlook, calendar, OneDrive) and Slack. Agents act as that person and see only what that person can see. Tokens are encrypted on the server and never reach the agent. See [docs/connectors.md](docs/connectors.md).
- **Its own browser and web search.** Agents search the web (through a self-hosted SearXNG or the Brave Search API), open and read sites, and click or type with your approval. Open **Browser** to watch it live or take over, for example to sign in for it. The agent waits until you give it back. The browser can only reach the public internet.
- **Any model.** Lyzr, Anthropic, OpenAI, or a local model through Ollama. Administrators add keys in the app and pick the model new agents use. Keys stay on the server. See [docs/models.md](docs/models.md).

**Safe to run**
- **Sandboxed runs.** On Linux, agents run inside bubblewrap, with no access to home folders, other agents or the server's data.
- **Files and attachments.** Attach documents to a message (Word and PDF are turned into text for the agent). Each agent keeps a workspace folder between conversations.
- **Git workflows.** Agents can work on repositories. Their changes go to review and can be published as pull requests. See [docs/git-workflows.md](docs/git-workflows.md).

## How it fits together

```
 Desktop app (Electron)        lux (command line)
          │                          │
          └──────── HTTPS / SSE ─────┘
                       │
                   luxd (Go)  ── PostgreSQL (tasks, events, approvals, settings)
     ┌──────────────┬──┴───────────┬─────────────────┬──────────────┐
 orchestrator   model gateway   connectors (MCP)   browser + search   policy
     │              │              │                 │
 sandboxed run  Lyzr / Anthropic  Google / M365 /   headless Chrome,
 (gitagent)     OpenAI / Ollama   Slack             SearXNG / Brave
```

- **luxd** is the server: API, orchestrator, model gateway, connectors, browser and policy in one Go binary.
- **The runner** (`runtime/gitagent-runner`) runs one gitagent task. It reaches models only through luxd's gateway and tools only through luxd's MCP endpoint, with a short-lived run token. It never holds a provider key or an account token.
- **The desktop app** (`desktop/`) and **`lux`** are clients of the same API.

Design decisions are recorded in [docs/adr](docs/adr/README.md).

## Quick start (on your own computer)

You need Go 1.27+, Node.js 20+, git and PostgreSQL 15+ (a container is fine). Chromium is optional; it gives agents a browser.

1. Start a database:

   ```bash
   podman run -d --name lux-dev-db -e POSTGRES_USER=lux -e POSTGRES_PASSWORD=lux-dev -e POSTGRES_DB=lux -p 127.0.0.1:55432:5432 docker.io/library/postgres:17-alpine
   ```

2. Build the server, the command line and the agent runner:

   ```bash
   go build -o bin/ ./cmd/... && (cd runtime/gitagent-runner && npm ci)
   ```

3. Start the server. The database address is saved, so later starts are just `bin/lux server start`:

   ```bash
   bin/lux server start --database-url 'postgres://lux:lux-dev@127.0.0.1:55432/lux?sslmode=disable'
   ```

4. Start the desktop app:

   ```bash
   cd desktop && npm ci && npm start
   ```

5. In the app, press **+** and **Create an agent**. The first time, pick the model agents use under **… → Models** and add its API key there. With no API key at all, use a local model through [Ollama](docs/models.md#ollama).

You can also keep model keys and other settings in a private file that `lux server start` reads. Fill in what you use, then restart with `bin/lux server stop` and `bin/lux server start`:

```bash
install -m 600 .env.example ~/.config/lux/luxd.env
```

To run Lux for a team, see [docs/self-hosting.md](docs/self-hosting.md).

## Documentation

| Guide | What's in it |
|---|---|
| [Using Lux](docs/using-lux.md) | The desktop app: agents, chats, approvals, files, routines, the browser |
| [Connectors](docs/connectors.md) | Connecting Google, Microsoft 365 and Slack, and the Lux bot in Slack |
| [Models](docs/models.md) | Providers, keys, the default model, Lyzr agents, Ollama |
| [Policy and approvals](docs/policy.md) | What agents may do alone, and how approvals work |
| [Self-hosting](docs/self-hosting.md) | Running luxd for a team: Compose, systemd, TLS, backups, settings |
| [The `lux` command](docs/cli.md) | Every command |
| [Git workflows](docs/git-workflows.md) | Agents that work on repositories |
| [Agent validation](docs/agent-validation.md) | How Lux checks an agent before it runs |
| [Runtime protocol](docs/runtime-protocol.md) | How luxd and the runner talk |
| [Development](docs/development.md) | Building and testing Lux |

## What works and what doesn't

Tested end to end with real services:
- Agents on Lyzr (a manager agent with sub-agents): multi-turn chats, files kept between turns, attachments, routines and approvals. Agents kept their Work status, scheduled follow-ups, saved to memory, read websites in their browser and asked a person to sign in for them.
- A local llama3.2 model through Ollama runs and names agents. A 3B model is too small to use agent tools well, so use a larger one.

Tested only against stand-in services (no real accounts yet):
- The Google, Microsoft 365 and Slack connectors, and the Slack bot.
- Web search through SearXNG and Brave.
- The Anthropic and OpenAI providers.

Not built yet:
- Signed releases and a one-line installer. For now, build from source.
- Sign-in with Google or email. People sign in with an API token.
- A mobile app and push notifications to phones.
- Network isolation for sandboxed runs. They share the host's network.
- Private agents. Agents are shared within a workspace.

## Contributing and security

Contributions are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md). To report a security issue, follow [SECURITY.md](SECURITY.md) and don't open a public issue.

## License

[MIT](LICENSE)
