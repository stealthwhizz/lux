# Lux runtime protocol, version 1

How `luxd` runs an agent. The protocol is file-based so a run can outlive a `luxd` restart, and runtime-neutral so gitagent (TypeScript) and Ira (Rust) can both implement it. The reference implementation is `runtime/gitagent-runner`.

## Run directory
For each attempt `luxd` creates `$LUX_DATA_DIR/runs/<task-id>/<attempt>/`:

| Path | Written by | Contents |
|---|---|---|
| `agent/` | luxd | Fresh checkout of the agent repository at the task's pinned commit, with no git remote. luxd then restores the agent's saved `workspace/` (and, for agents made in Lux, `memory/`) into it |
| `spec.json` | luxd | What to run (below) |
| `home/`, `tmp/` | luxd | Private HOME and TMPDIR for the runtime |
| `events.jsonl` | runtime | Append-only progress events, one JSON object per line |
| `exit.json` | runtime | Written last, atomically (write to a temporary file, then rename) |
| `runtime.log` | luxd | The runtime's stdout and stderr, for diagnostics |

When the run ends, luxd saves `agent/workspace/` (and `agent/memory/` for agents made in Lux) as the agent's new state, skipping symbolic links and special files, and appends a `lux.files` event listing the files that were added, modified or deleted. Everything else in the run directory is not carried over. Agents imported from git keep memory changes as reviewable proposals instead (see [git-workflows.md](git-workflows.md)). If two runs of one agent overlap, the later one to finish wins.

## spec.json

```json
{
  "protocol": 1,
  "run_id": "run_…",
  "task_id": "task_…",
  "attempt": 1,
  "prompt": "Summarise this repository",
  "agent_dir": "/…/runs/task_…/1/agent",
  "events_path": "/…/events.jsonl",
  "exit_path": "/…/exit.json",
  "model": "lux:anthropic/claude-sonnet-4-5@http://127.0.0.1:7878/gateway/v1",
  "api_url": "http://127.0.0.1:7878",
  "max_turns": 50,
  "timeout_seconds": 3600
}
```

The run token is **not** in the file. It is passed in the environment as `LUX_RUN_TOKEN` (and as `OPENAI_API_KEY`, which gitagent sends to custom endpoints). The token only works for this run's internal and gateway endpoints and stops working when the run ends.

## Environment
The runtime starts with a constructed environment, not luxd's:
- `PATH` and locale variables;
- `HOME` and `TMPDIR` pointing into the run directory;
- `LUX_RUN_TOKEN` and `OPENAI_API_KEY` (the run token);
- `NO_COLOR=1`.

Provider API keys, Git credentials and Lux user tokens are never passed.

## events.jsonl
One JSON object per line:

```json
{"seq": 3, "ts": "2026-10-09T10:00:00.000Z", "type": "tool.call", "data": {"id": "call_1", "name": "cli", "args": {"command": "ls"}}}
```

`seq` starts at 1 and increases by 1. `luxd` reads only complete lines and records its byte offset in the same transaction as the events, so after a restart it continues exactly where it stopped.

| type | data |
|---|---|
| `run.started` | `runtime`, `runtime_version`, `agent`, `model` |
| `assistant.delta` | `text` (coalesced chunks of the reply as it streams) |
| `thinking.delta` | `text` |
| `assistant.message` | `content`, `model`, `stop_reason`, `usage`, `error` |
| `tool.call` | `id`, `name`, `args` |
| `tool.result` | `id`, `name`, `content`, `is_error` |
| `tool.blocked` | `name`, `reason` (denied by Lux policy) |
| `system` | `subtype`, `content` (gitagent lifecycle messages) |
| `run.error` | `message` |

Large fields are truncated by the runtime (`truncated: true`). `luxd` drops lines over 1 MB and events whose type is not a lowercase dotted name.

## exit.json

```json
{"reason": "completed", "summary": "The repository contains…", "error": ""}
```

`reason` is one of `completed`, `error` or `cancelled`. `luxd` decides the task's final state:
1. Cancellation was requested: `cancelled`.
2. The deadline passed and luxd stopped the run: `timed_out`.
3. `completed`: `succeeded`. Anything else: `failed`.
4. The process is gone and there is no `exit.json`: `failed` with reason `runtime_lost`. Only this case is retried, up to the task's `max_attempts`; agent errors are never retried because the agent may already have had side effects.

## Control
- **Cancel:** `luxd` sends `SIGTERM` to the runtime's process group. The runtime must abort and still write `exit.json` with `reason: cancelled`. After 10 seconds `luxd` sends `SIGKILL`.
- **Tool policy:** before every tool call the runtime sends `POST {api_url}/internal/v1/runs/{run_id}/tool-calls` with `{"tool": "...", "args": {...}}` and the run token, and receives `{"decision": "allow" | "deny", "reason": "..."}`. Any error, timeout or unexpected answer must be treated as `deny`.
- **Models:** only through `{api_url}/gateway/v1` (OpenAI Chat Completions) with the run token.

## Trust
`events.jsonl` and `exit.json` are written by code that also runs the agent, so `luxd` treats them as untrusted display data. They never authorize anything. With the `local` executor the agent runs as the same OS user as luxd, so this protocol is not a security boundary there (see ADR 0005).
