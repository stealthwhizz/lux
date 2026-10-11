# ADR 0004: Runtime interface

**Status:** accepted

## Context
gitagent already loads OpenGAP repositories (agent.yaml, SOUL.md, RULES.md, skills, tools, workflows, memory, hooks) and exposes `query()` with streaming events, `abort()`, `maxTurns` and a `preToolUse` hook. Ira (Rust) implements the same agent format. Lux must not define a competing agent format.

## Decision
- The runtime is gitagent, driven by `runtime/gitagent-runner`, a small Node program that Lux owns.
- Contract between `luxd` and any runtime ("Lux runtime protocol v1"):
  - Input: `spec.json` in the run directory (task id, prompt, agent directory, model string, gateway URL, allowed tools, limits).
  - Output: append-only `events.jsonl` (one JSON object per line, with `seq`) and `exit.json` written last.
  - Control: `SIGTERM` means cancel; the runtime must stop and still write `exit.json`.
  - Policy: before every tool call the runtime asks `POST /internal/v1/runs/{id}/tool-calls` and obeys the answer. Any error is treated as "deny" (fail closed).
  - Model access only through the gateway, using the run token. The runtime never receives provider keys.
- File-based output (instead of a pipe) lets a run keep going while `luxd` restarts; `luxd` re-attaches and continues reading from its saved offset.
- Ira can become a second runtime by implementing the same protocol. Selection is per agent (`runtime: gitagent | ira`).

## Consequences
- Repo-declared plugins, MCP servers and `extends` run code inside the runner process. The runner refuses them unless the workspace policy allows them (see threat model).
- Multi-turn conversations are passed as context to a new run per message; gitagent's in-process multi-turn API is not relied on.
