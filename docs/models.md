# Models

Agents think with a model from one of four providers. luxd holds the keys: an agent's run reaches models only through luxd's model gateway with a short-lived run token, and never sees a provider key. See [ADR 0009](adr/0009-model-gateway.md) for how the gateway routes, limits and records calls.

| Provider | Model names | Key |
|---|---|---|
| **Lyzr** | `lyzr:<agent id>`: a Lyzr Studio agent, which can manage its own sub-agents | Lyzr API key |
| **Anthropic** | `anthropic:<model>`, e.g. `anthropic:claude-sonnet-4-5` | Anthropic API key |
| **OpenAI** | `openai:<model>`, e.g. `openai:gpt-5.4-mini` | OpenAI API key |
| **Ollama** | `ollama:<model>`, e.g. `ollama:llama3.2` | None; runs on the server's computer |

## Adding keys

**In the app.** Administrators open **… → Models**, press **Add key** on a provider and paste the key. Lux checks it by asking the provider which models it can use. A key the provider refuses is not saved. A good key works at once, without a restart. Keys added this way are encrypted in luxd's database with the same key as connector tokens ([connectors.md](connectors.md#how-it-works)), and are never sent back to the app.

**In luxd's settings.** `LUX_LYZR_API_KEY`, `LUX_ANTHROPIC_API_KEY` and `LUX_OPENAI_API_KEY` (see [self-hosting.md](self-hosting.md#configuration), or `~/.config/lux/luxd.env` for `lux server start`). A key set there always wins, shows as "Key set in the server file", and can only be changed there. Use it when keys are managed by your secrets tooling.

With several luxd replicas behind one address, a key added in the app is used by the replica that saved it at once, and by the others after they restart.

## The model new agents use

Under **… → Models**, press **Models** on a ready provider to see what its key can use, as buttons:

- **Lyzr**: your Lyzr Studio agents, by name.
- **Anthropic and OpenAI**: their chat models (OpenAI's embedding, audio and image models are left out).
- **Ollama**: the models pulled on the server.

Click one to make it the model every new agent gets. It is highlighted, and the bottom of the dropdown shows "New agents use: …". Only administrators can change it. Existing agents keep their model; change one with **Edit** in its menu.

When no model has been chosen yet, **Create an agent** opens Models first, then creates the agent once you pick one.

## Lyzr

A Lyzr model is a Lyzr Studio agent, with its own instructions in Lyzr. Lux adds its own instructions about the workspace, files and answer buttons, but the agent's Lyzr instructions shape its replies. If an agent's replies come back as plans ("THINK… ACT…") instead of conversation, change that agent's instructions in Lyzr Studio, or pick a different Lyzr agent.

Lyzr does not report token usage, so model budgets don't limit Lyzr agents.

## Ollama

Ollama runs models on the server's own computer, with no key and no data leaving it. Lux expects it at `http://127.0.0.1:11434` (`LUX_OLLAMA_BASE_URL` changes that; `LUX_OLLAMA_DISABLED=true` turns it off).

```bash
ollama pull qwen3:8b
```

Start Ollama with a context long enough for an agent's instructions and tools. The default of 4,096 tokens cuts them off:

```bash
OLLAMA_CONTEXT_LENGTH=16384 ollama serve
```

The model must be good at tool calling. In our tests, llama3.2 (3B) wrote tool calls as text instead of making them, so the agent's replies came out as JSON. Pick a larger model with tool-calling support, such as `qwen3:8b`; we have not tested larger local models with Lux yet. Without a GPU, expect a minute or more per reply.
