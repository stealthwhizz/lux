# ADR 0008: Deployment modes

**Status:** accepted

| Mode | How it runs | Auth | Executor |
|---|---|---|---|
| A. Local development | `lux server start` on the developer's machine, PostgreSQL in a container or native | Owner token written to the local credentials file at first start | `local` allowed |
| B. Self-hosted single instance | `luxd` as a systemd service or container (`deploy/`), PostgreSQL managed by the operator | Tokens created with `luxd bootstrap` and `lux token create` | `local` only if explicitly allowed; `e2b`; `firecracker` when built |
| C. Enterprise self-hosted | Mode B plus multiple workspaces, roles, policies, budgets, audit export | Tokens today; SSO (OIDC) is an extension point, not implemented | Policy sets minimum isolation |
| D. Managed service | Not built. Same API; nothing in A–C depends on it | — | — |

`LUX_MODE=local|server` selects defaults. There are no license checks or calls to a Lux-operated service in any mode.
