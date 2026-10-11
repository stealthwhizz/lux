# Repositories, changes and memory proposals

An agent can work on a git repository. It works on a private copy. Lux shows what it changed, and nothing leaves the server until a person publishes it.

## Repositories

An administrator (`repo.manage`) registers each repository a workspace may work on:

```bash
lux repo add widgets https://github.com/acme/widgets.git --credential github --test-command "make test"
```

- luxd fetches the repository when it is added, so a wrong URL or credential fails right away.
- `--branch` sets the branch tasks start from and pull requests target. By default, Lux uses the remote's default branch.
- The test command is not run by Lux. The agent is told about it and runs it through its own tools, so the workspace policy and approvals apply as usual.

Then run a task on the repository:

```bash
lux agent run helper "Fix the flaky test in pkg/cache" --repo widgets
```

In the desktop app, choose the repository in the **Run a task** dialog.

### Where the agent sees it

For each run, luxd checks the repository out at `repo/` inside the agent's run directory, on a new branch `lux/<task>`. The checkout has no remote. The agent is told where the checkout is and how to run the tests.

gitagent's tools work relative to the agent directory, which is why the checkout lives there. If the agent repository already has a `repo/` folder, the task fails with a message saying to rename it.

## Credentials

Git credentials are configured only on the luxd host, one environment variable per credential:

```bash
LUX_GIT_CREDENTIAL_GITHUB=ghp_xxxxxxxx            # a token; GitHub accepts the username x-access-token
LUX_GIT_CREDENTIAL_GITLAB=oauth2:glpat-xxxxxxxx   # or username:token
```

Repositories and agents refer to a credential by its lowercased name (`github`, `gitlab`). The API, the CLI and the desktop app never see the secret.

luxd sends the credential only to the https remote it was given for, as an HTTP header scoped to that URL, and does not follow redirects. ssh remotes use the luxd host's ssh setup (`SSH_AUTH_SOCK`, `~/.ssh`).

Private agent repositories use the same mechanism:

```bash
lux agent import https://github.com/acme/agent.git --credential github
```

## Changes

When a run ends, luxd records two change sets:

| Target | What | Published to |
|---|---|---|
| `repository` | Everything that differs in `repo/` from the commit the run started at | `lux/<task>` in the repository |
| `memory` | Changes under the agent's `memory/` and `skills/` folders (a memory proposal) | `lux/<task>-memory` in the agent's repository |

A change set is recorded only when something changed. Patches larger than 20 MB are not kept: the file list is still shown, but the change set cannot be published.

To review changes:

- **CLI:** `lux changes` lists what is waiting for review. `lux changes show TASK [--memory]` prints the patch.
- **Desktop:** the task page shows the change sets with a diff. The **Approvals** page lists everything waiting for review.

### Publishing

```bash
lux changes publish TASK [--memory] [--pr] [--title "Fix flaky cache test"]
lux changes discard TASK [--memory]
```

Publishing needs the `approval.decide` permission. Maker-checker applies: unless the workspace policy sets `require_distinct_approver: false`, the person who started the task cannot publish its changes. Agents whose `agent.yaml` requires strict segregation of duties always need a second person.

Publishing works as follows:

- luxd applies the stored patch to the commit the run started from, commits it, and pushes it to a new branch.
- The commit is authored as `<agent> (Lux agent)`, with `Lux-Task` and `Approved-by` trailers.
- luxd never force-pushes. If the branch already exists with other history, publishing fails and the changes stay proposed.
- Anything the agent changed after its run ended is not included: what was reviewed is what is pushed.

`--pr` also opens a pull request. This works only for `https://github.com/...` remotes with a credential. Other hosts get the branch only. If the push succeeds but the pull request fails, the result says so and the branch stays pushed.

The audit log records every publish, its outcome, and every discard.

## Security notes

- luxd never runs git inside a `.git` directory the agent could have changed. Planted hooks, `core.fsmonitor` or diff drivers would otherwise run with luxd's privileges. To record changes, luxd uses a private git directory that borrows objects from its own mirror. To publish, it uses a fresh clone of that mirror.
- git processes started by luxd get a minimal environment: no provider keys and no database URL.
- While a git command runs with a credential, the credential is in that git process's environment. With the local executor (no isolation), an agent running at the same moment as the same OS user could read it from `/proc`. Isolating executors remove this; until then, use tokens that can push only to the repositories Lux works on.
