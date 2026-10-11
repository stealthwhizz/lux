# ADR 0005: Execution isolation

**Status:** accepted

## Decision
`luxd` starts runs through an `executor.Provider` interface (`internal/executor`). Each provider reports its isolation level, and the API and UI show it on every task.

| Provider | Isolation level | Status | Boundary |
|---|---|---|---|
| `local` | `none` | implemented | A child process of `luxd` with a clean environment (no provider keys, no Lux credentials in env), its own process group and a private HOME. It runs as the same OS user, so it can read any file that user can. **Not a security boundary.** For trusted agents on a developer machine only. |
| `e2b` | `remote-sandbox` | implemented, not live-verified | Agent loop runs on the host runner; gitagent sends shell/file tools into an e2b VM through gitmachine. Needs `E2B_API_KEY` on the `luxd` host. |
| `firecracker` | `microvm` | not implemented | Planned for Linux servers. The interface reserves it; `lux doctor` reports it as unavailable. |

- Workspace policy sets the minimum isolation level an agent may run with. The `local` provider is refused in server mode unless an administrator explicitly allows it.
- Provider credentials and Git credentials stay in `luxd`. Model calls go through the gateway with a run token; pushes and pull requests are made by `luxd`, not by the agent.

## Consequences
We never describe `local` as sandboxed. Strong isolation for untrusted agent repositories requires the microVM provider, which is not built yet.
