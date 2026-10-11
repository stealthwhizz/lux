# ADR 0010: Connectors

**Status:** accepted, implemented in `internal/connectors` and `internal/api` (Google, Microsoft 365, Slack)

## Context
A personal agent is only useful with the person's own email, calendar, files and chat. Lux is open source and self-hosted, so it cannot ship OAuth clients: a client secret in a public repository is no secret, and Google's verification for Gmail and Drive scopes (with a yearly security assessment) is something a company operating a hosted service does, not a code base. Hosted aggregators (Composio, Pipedream) avoid per-server setup only because they hold everyone's tokens, which is what self-hosting customers want to avoid.

## Decision
- **Each server brings its own app.** An administrator registers Lux once with each provider (an OAuth client in Google Cloud, an Entra ID app registration, a Slack app from a prefilled manifest) and enters its credentials in the app, or Google's in luxd's settings. People then connect their own accounts.
- **Per-person, delegated access.** Google and Microsoft use OAuth 2.0 with PKCE and offline access; Slack uses a user token (`xoxp-`). An agent acts as the person who started its task, so providers enforce what that person may see. There are no service accounts and no domain-wide delegation.
- **Tokens never reach agents.** Tokens and app secrets are sealed with AES-256-GCM (`LUX_SECRET_KEY`), with the user and provider as associated data so a sealed value cannot be moved between rows. luxd serves connector tools to runs over MCP (Streamable HTTP, JSON responses) at `/internal/v1/runs/{run}/mcp`, authorized by the run token, and calls the provider itself.
- **Policy is enforced by luxd.** Tools are named `lux__<tool>` for the runtime's policy hook, and luxd re-checks the workspace rules on every call. Tools that send or create need approval by default; an approval is matched to the exact arguments (hash) and is used once.
- **Least scope.** Drive and OneDrive are read-only. A connection made before a scope was added keeps working without the tools that need it, and the app offers to connect again.
- **Slack without HTTPS.** Slack only redirects to `https://` addresses. A local server cannot use Slack's sign-in, so people can paste a user token from an app created from the same manifest. Lux verifies it with `auth.test` before saving. Entra accepts `http://localhost` but may refuse `http://127.0.0.1`, so a local server registers its `localhost` name.

## Consequences
- Setting up a provider takes an administrator a few minutes per server, documented in [connectors.md](../connectors.md).
- Every new connector is code in luxd (a client, tools, setup). A generic connector that serves an administrator-chosen MCP server under the same policy is the planned way to add many more.
- Data is read live when an agent needs it. Nothing is copied into a knowledge base, so there is no second copy to secure or keep in sync, at the cost of slower answers over large histories.
