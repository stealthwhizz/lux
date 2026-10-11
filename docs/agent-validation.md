# Agent import and validation

Lux runs agents defined as git repositories in the [OpenGAP](https://github.com/open-gitagent/opengap) format. It adds no agent format of its own. The code is in `internal/agentrepo` and has no dependency on the desktop app, the HTTP API or the database.

## Import

1. **Source check.** An agent comes from an `https://`, `ssh://` or `git@host:path` URL, or from an absolute local path. Anything else is refused, including `ext::`, `fd::`, `git://`, `file://`, plain `http://`, and values starting with `-`. Git itself is also run with only the https, ssh and local-file transports enabled, so a URL that slipped past the check still cannot run a command.
2. **Mirror.** The repository is cloned once into `$LUX_DATA_DIR/mirrors/<hash>.git` and updated on later imports.
3. **Pin.** The branch or tag (default: the repository's default branch) is resolved to a full commit id. Lux runs committed revisions only; uncommitted changes in a local repository are ignored.
4. **Inspect.** That commit is checked out into a temporary directory with no remote, validated, and deleted.

Git always runs with an argument list (never a shell), with repository hooks, prompts and the system git config disabled.

## Checks

Errors make the revision invalid; warnings are shown but do not block it.

| Area | Error | Warning |
|---|---|---|
| `agent.yaml` | missing, not YAML, not a mapping, wrong field types; `name`, `version`, `description` missing or not matching the OpenGAP schema patterns; malformed `spec_version` | `spec_version` missing; keys outside the spec and the runtime's extensions (`plugins` for gitagent) |
| `SOUL.md` | missing, a symbolic link, or headings only | — |
| Skills | listed skill has no `skills/<name>/` folder; bad or duplicate names | folder has no `SKILL.md` |
| Tools | listed tool has no `tools/<name>.yaml` and is not a built-in of the runtime; bad or duplicate names | — |
| Model | preferred model unparseable, or its provider is not one the gateway supports | no model (workspace default used); provider inferred from a bare model id; endpoint in the model string ignored; unusable fallback dropped |
| `runtime` | `max_turns` or `timeout` below 1 | — |
| Segregation of duties | fewer than 2 roles, duplicate roles, conflicts or assignments naming undefined roles, a role conflicting with itself | — |
| Links | a symbolic link that resolves outside the repository | a broken link |
| Code at load time | — | `extends`, `dependencies`, `plugins`, command-based `mcp_servers`, `hooks/`. These need workspace approval to run |
| Schedules (`schedules/*.yaml`) | — | invalid id, prompt, cron or `runAt`, or a duplicate id. That schedule will not run |

### Model names
Repositories name models three ways. Lux accepts all three and normalizes them to `provider:model`:

| Written as | Example | Result |
|---|---|---|
| gitagent `provider:model` | `anthropic:claude-sonnet-4-5` | as written |
| `provider/model` | `ollama/qwen3-coder:latest` | `ollama:qwen3-coder:latest` |
| bare model id (OpenGAP spec) | `claude-opus-4-6` | `anthropic:claude-opus-4-6`, with a warning |

Bare ids are mapped with the same prefixes as opengap's Lyzr adapter: `claude*` to Anthropic; `gpt*`, `chatgpt*`, `o1*`, `o3*` and `o4*` to OpenAI. Other bare ids are an error, because Lux cannot guess the provider.

Supported providers: `anthropic`, `lyzr`, `ollama`, `openai`. A base URL inside a model string (`…@https://host/v1`) is never used: model traffic always goes through the Lux gateway to the endpoint the operator configured, so a repository cannot redirect requests or API keys.

### Reading untrusted files
Files Lux reads itself (`agent.yaml`, `SOUL.md`, `SKILL.md`, schedules) must be regular files inside the repository. Symbolic links are refused, and sizes are capped (256 KB for `agent.yaml`, 1 MB for the others). The whole tree is scanned for links that point outside the repository, because the runtime reads many more files (knowledge, examples) into the model's context.

## Differences from `opengap validate`

Lux's checks are a subset of the OpenGAP rules, plus what Lux needs to run the agent. The reference validator can also run on every import: set `LUX_OPENGAP_CMD` (for example `npx --no-install opengap`). Its result is stored with the revision. A failure is a warning unless the workspace requires OpenGAP conformance, in which case it is an error.

Known differences:
- **Built-in tools.** `opengap validate` requires `tools/<name>.yaml` for every listed tool. Lux accepts the runtime's built-in tools without a file (gitagent: `cli`, `read`, `write`, `edit`, `memory`, `capture_photo`, `task_tracker`, `skill_learner`; Ira also has `pdf`). For example, gitagent's own `agent.yaml` lists `cli`, `read`, `write` and `memory`.
- **Unknown keys.** The OpenGAP schema forbids unknown top-level keys. Lux warns instead, and accepts gitagent's `plugins` key.
- **Compliance frameworks.** FINRA, Federal Reserve, SEC and CFPB rules are checked only by `opengap validate`.

## Results against existing repositories

Run on 2026-10-09 with `LUX_TEST_AGENT_DIRS` (see `TestValidateRealAgents`):

| Repository | Valid | Errors |
|---|---|---|
| gitagent (root agent) | yes | — (no model set) |
| opengap examples: full, gitagent-helper, llm-wiki, lyzr-agent, minimal, standard, nvidia-deep-researcher | yes | — |
| opengap examples: financial-agent | no | no `SOUL.md` |
| opengap examples: jason-… | no | skill `exa-search` listed but missing |
| gitagent-rs/muse | no | no `version`, no `SOUL.md`; `pdf` tool is Ira-only (valid tool with `runtime: ira`) |
