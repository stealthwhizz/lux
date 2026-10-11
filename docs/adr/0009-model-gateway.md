# ADR 0009: Model gateway

**Status:** accepted, implemented in `internal/gateway`

## Context
gitagent sends custom-endpoint models through the OpenAI Chat Completions API (`createCustomModel` uses `openai-completions`). Lyzr, OpenAI and Ollama expose that API. Anthropic offers an OpenAI-compatible endpoint, which Anthropic describes as intended for testing rather than long-term production use.

## Decision
- `luxd` serves `POST /gateway/v1/chat/completions`. The runner points gitagent at it with the model string `lux:<provider>/<model>@<api-url>/gateway/v1` and the run token as the API key. Only run tokens are accepted, and only while the run is active and not being cancelled.
- **Declared models only.** The requested model must be one the agent declares in `agent.yaml` (preferred or fallback). A runtime cannot switch itself to another model.
- **Keys stay in luxd.** The gateway replaces the run token with the operator's provider key. Runtime headers such as `X-Session-Id` are not forwarded.
- **Before each request:**
  - the per-agent rate limit (`model_requests_per_minute`);
  - the workspace's and the agent's monthly token budgets (`monthly_token_budget`, `agent_monthly_token_budget`, calendar month in UTC).
  - Budgets are checked before a request, so the request that crosses a budget completes.
- **Fallback.** The gateway moves to the agent's next declared model only when the workspace policy sets `allow_model_fallback`, and only on a connection error or 5xx before any response bytes were sent, or when the requested provider has no credentials. 4xx responses are passed through and never retried elsewhere. Every fallback is a `lux.model_fallback` task event.
- **Usage.** Read from the response's `usage` object (the final chunk of a stream). For providers that accept it, the gateway adds `stream_options.include_usage`. Providers that report no usage are recorded with `usage_known = false`, and budgets cannot count them.
- **Recording.** Every call writes a `model_usage` row and a `lux.model_call` task event. Logs record metadata only (provider, model, status, tokens, latency), never request or response bodies, prompts or keys.

## Providers

| Name | Endpoint (default) | Credentials | Usage in streams | Notes |
|---|---|---|---|---|
| `anthropic` | `https://api.anthropic.com/v1` | `LUX_ANTHROPIC_API_KEY` (or `ANTHROPIC_API_KEY`); `LUX_ANTHROPIC_WORKSPACE_ID` for multi-workspace keys | yes | OpenAI-compatible layer: no prompt caching; `strict`, `response_format` and some fields are ignored; thinking is not returned |
| `openai` | `https://api.openai.com/v1` | `LUX_OPENAI_API_KEY` (or `OPENAI_API_KEY`) | yes | |
| `lyzr` | `https://agent-prod.studio.lyzr.ai/v4` | `LUX_LYZR_API_KEY` (or `LYZR_API_KEY`) | not requested | The model id is a Lyzr Studio agent id (`lyzr:<agent-id>`), as in gitagent's Lyzr example |
| `ollama` | `http://127.0.0.1:11434/v1` | none | not requested | Enabled unless `LUX_OLLAMA_DISABLED=true`; reachability is only known when a request is made |

Endpoints can be changed with `LUX_<PROVIDER>_BASE_URL`, for example to use a corporate proxy. They come only from the operator's environment; an agent repository cannot set them.

## Consequences
- Features outside Chat Completions (native Anthropic Messages features such as prompt caching) are not available through Lux yet. A native Anthropic adapter is the follow-up.
- Budgets and rate limits are per luxd process for rate limits, and per database for budgets.
- **Lyzr verified live (2026-10-09)** against a Lyzr manager agent with sub-agents. Through `/v4/chat/completions` with Bearer auth (`x-api-key` is rejected there), plain replies, streaming and tool calls all work. A real Lux task ran through the gitagent runner: Lyzr returned a `cli` tool call, Lux paused it for approval, then the run read a file and succeeded. Lyzr reports token usage as 0, so usage figures and token budgets do not work for Lyzr models yet. Lux cannot see whether a manager delegated to its sub-agents: that happens inside Lyzr.
- **Live verification is outstanding for Anthropic, OpenAI and Ollama.** The gateway is tested against scripted OpenAI-compatible upstreams, including a full run through the real gitagent runner. It has not yet been run against the real Anthropic, OpenAI, Lyzr or Ollama services, because no credentials were available while building it.
