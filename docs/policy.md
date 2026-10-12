# Workspace policy and approvals

Each workspace has a policy document, `workspaces.policy` (JSON). `luxd` enforces it on the server. The desktop app and CLI only display it.

Unknown keys are rejected. If the stored policy is ever invalid, Lux refuses task creation, tool calls and model calls in that workspace until an administrator fixes it; it never falls back to permissive defaults.

## Settings

| Key | Default | Meaning |
|---|---|---|
| `tool_rules` | built-in rules (below) | How each tool call is decided. `[]` denies every tool call |
| `require_distinct_approver` | `true` | Maker-checker: whoever created a task cannot approve its actions. A fresh **local** install sets this to `false` in its default workspace, because there is only one person |
| `approval_timeout_seconds` | `3600` | Pending approvals expire and deny after this |
| `allow_code_execution_on_load` | `false` | Allow agents whose repository runs code when loaded (plugins, hooks, `extends`, MCP commands) |
| `require_opengap` | `false` | A failed `opengap validate` blocks import |
| `max_task_timeout_seconds` | `86400` | Upper bound for task timeouts |
| `allow_model_fallback` | `false` | Let the gateway use an agent's next declared model on provider failure |
| `monthly_token_budget` | none | Workspace tokens per calendar month (UTC) |
| `agent_monthly_token_budget` | none | Same, per agent |
| `model_requests_per_minute` | none | Per-agent model request rate |

## Tool rules
Before every tool call the runtime asks `luxd`. Rules are checked in order and the first match decides. **If no rule matches, the call is denied.**

```json
{"tool_rules": [
  {"tool": "read", "when": "path_outside_agent_dir", "action": "require_approval", "reason": "reading outside the agent's directory"},
  {"tool": "read", "action": "allow"},
  {"tool": "cli", "match": "^git (status|diff|log)$", "action": "allow"},
  {"tool": "cli", "action": "require_approval"},
  {"tool": "memory", "action": "allow"}
]}
```

| Field | Meaning |
|---|---|
| `tool` | Tool name, or `*` |
| `match` | Optional regular expression (RE2), tested against the `command` argument, else `path`, else the JSON arguments |
| `when` | Optional condition. `path_outside_agent_dir` matches when `path` is absolute, starts with `~`, or climbs out with `..`. `path_in_workspace` matches when `path` stays inside the agent's `workspace/` folder. An absolute path into the run's own agent folder is judged as the relative path it is. |
| `action` | `allow`, `deny` or `require_approval` |
| `reason` | Shown to the agent and to approvers |

**Built-in rules**, used when `tool_rules` is absent:
- `write` and `edit` inside `workspace/` are allowed: that is the agent's own folder, kept between runs and shown to its owner.
- `cli`, other `write` and `edit` calls, and `capture_photo` need approval.
- `read` needs approval outside the agent's directory and is allowed inside it.
- `memory`, `task_tracker` and `skill_learner` are allowed.
- Connector tools, named `lux__<tool>` (see [connectors.md](connectors.md)): reading is allowed (`lux__gmail_search`, `lux__drive_read`, `lux__outlook_events`, `lux__slack_read` and the like). Sending and creating need approval: `lux__gmail_draft`, `lux__gmail_send`, `lux__calendar_create`, `lux__outlook_send`, `lux__outlook_create_event` and `lux__slack_send`.
- Lux's own tools: `lux__update_status` and `lux__follow_up` are allowed. The browser: `lux__browser_open`, `lux__browser_read` and `lux__browser_back` are allowed; `lux__browser_click` and `lux__browser_type` need approval, because they can submit forms.
- Every other tool, including other MCP tools, is denied.

luxd checks connector calls itself when it serves them, so these rules hold even for a runtime that skips the check. An approval for a connector call matches its exact arguments and can be used once.

These are strict because the local executor does not isolate agents (ADR 0005).

**Be careful with `allow` on `cli`.** A pattern sees the whole command string, and shells chain commands (`;`, `&&`, `|`, `$(...)`). Anchor patterns with `^…$`, keep them narrow, and prefer `require_approval` for shell access.

### Agent settings that tighten policy
From `agent.yaml` (OpenGAP compliance section):
- `compliance.supervision.human_in_the_loop: always`: every tool call needs approval, whatever the workspace rules say.
- `compliance.segregation_of_duties.enforcement: strict`: maker-checker applies even when the workspace turns it off.

## Approval flow
1. A rule returns `require_approval`. `luxd` creates an approval, moves the task to `waiting_approval`, records `lux.approval_requested` and audits the request.
2. The runtime waits (long-poll), and runs the tool only on an explicit approval.
3. Someone with the `approver`, `admin` or `owner` role decides with `POST /v1/w/{ws}/approvals/{id}/decision` (`{"decision": "approve" | "reject", "note": "…"}`). Maker-checker is checked here. The decision is audited and recorded as `lux.approval_decided`.
4. When nothing else is pending, the task returns to `running`.

**Everything else denies:**
- rejection;
- expiry;
- cancelling the task;
- the task ending (its pending approvals become `cancelled`);
- an invalid policy;
- `luxd` being unreachable from the runtime after a few retries;
- any unexpected answer.

## Limits
With the `local` executor the agent runs as the same OS user as `luxd`. The tool-call check is enforced by the Lux runner, which runs in that same environment. Code the agent runs (an approved shell command, a hook script) can reach anything that user can, including the network, without asking Lux again. Real enforcement needs an isolating executor (not implemented yet; see ADR 0005).
