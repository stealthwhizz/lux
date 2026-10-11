# The `lux` command

`lux` talks to a Lux server (`luxd`) through the same REST API as the desktop app. It shares the app's configuration: environments live in `~/.config/lux/config.yaml` and tokens in `credentials.yaml`. Both files are mode 0600.

```bash
go build -o ~/.local/bin/lux ./cmd/lux
```

## First run, local

```bash
lux server start --database-url 'postgres://lux:lux-dev@127.0.0.1:55432/lux?sslmode=disable'
lux doctor
lux agent import ./my-agent
lux agent run my-agent "Summarise this repository" --follow
```

`lux server start` runs `luxd` in the background (found next to `lux` or on `PATH`). It writes its pid and log to `~/.local/share/lux/`. On first start, `luxd` creates your account and saves its token for the `local` environment, so no login is needed. The database URL is saved after the first use.

`lux server stop` stops `luxd`. Agent runs keep going and are picked up when it starts again.

## Remote servers

```bash
lux env add work https://lux.example.com
lux login --env work
lux env use work
```

`lux login` reads the token from stdin, so it never appears in your shell history or the process list:

```bash
lux login --env work < token.txt
```

## Commands

| Command | What it does |
|---|---|
| `status` | Server version, components, executors, model providers and who you are |
| `doctor` | Checks git, Node, the server, the runtime, providers and your sign-in; exits 1 on failure |
| `login`, `logout`, `env`, `config` | Manage environments and tokens |
| `server start\|stop\|status` | Local `luxd`. `start` loads settings and API keys from `~/.config/lux/luxd.env` (copy `.env.example`), or from `--env-file PATH`; the file must be private (mode 600) |
| `agent list\|show\|import\|validate\|sync\|enable\|disable` | Agents. `import` and `validate` take a directory (local mode) or a git URL, with `--ref`, `--slug` and `--credential` |
| `agent run AGENT "PROMPT"` | Start a task. `--follow` streams progress; `--wait` waits silently; `-` reads the prompt from stdin; `--idempotency-key` makes retries safe; `--repo NAME` gives the agent a repository |
| `repo [list]`, `repo add NAME URL`, `repo remove NAME` | Repositories tasks can work on (see [git-workflows.md](git-workflows.md)) |
| `changes [list] [--all]`, `changes show\|publish\|discard TASK [--memory]` | Review and publish what a task changed; `publish --pr` opens a GitHub pull request |
| `task list\|show\|cancel`, `logs TASK [--follow]` | Tasks and their event log |
| `approvals [list] [--all]`, `approvals approve\|reject ID [--note]` | Approvals (maker-checker applies) |
| `chat AGENT` | Interactive conversation; each line is a message, `/exit` quits. `--conversation ID` continues one |

Global flags: `--env NAME`, `--workspace SLUG`, `--json`. JSON output is for scripts; human output may change.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | Success |
| 1 | Error (server error, failed check, invalid agent) |
| 2 | Usage error |
| 3 | A task the command waited for (`--wait`, `--follow`) did not succeed |
| 4 | Not signed in, or the token was rejected |

## Platform support
`lux` builds for Linux, macOS and Windows. `lux server` is not available on Windows yet: run `luxd` as a service there, or connect to a remote server.
