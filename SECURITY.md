# Security

Lux runs AI agents with access to people's email, files, calendars and chats. We take reports seriously and fix confirmed problems before anything else.

## Reporting a vulnerability

Please report privately through GitHub: on this repository's **Security** tab, choose **Report a vulnerability**. Don't open a public issue, discussion or pull request for a vulnerability.

Include what an attacker can do, the steps to reproduce it, and the version or commit. We will acknowledge your report, keep you updated while we fix it, and credit you when the fix is released, unless you'd rather not be named.

## Supported versions

Lux has not had a release yet. Fixes go to the `main` branch. Once releases exist, the latest release will receive security fixes.

## What Lux protects, and what it doesn't yet

How Lux is meant to protect people, so you can tell a bug from a known limit:

- **Secrets stay in luxd.** Model keys, account tokens and app secrets are kept by luxd and encrypted at rest (AES-256-GCM). An agent's run gets a short-lived run token that only lets it reach luxd's gateway and its own task's endpoints. luxd hides its own memory from other processes on Linux (`PR_SET_DUMPABLE=0`) and gives tools it starts (git, pdftotext) a minimal environment.
- **Agents ask before acting.** luxd itself checks the workspace rules for every tool call it serves, and every connector call. An approval covers one exact call and works once. Maker-checker can require a second person.
- **Agents are sandboxed on Linux.** With bubblewrap, a run sees its own folder, the system's programs read-only and the runner read-only. It can't see home directories, other runs, luxd's data, or other processes.
- **Connector access is per person.** An agent uses the accounts of the person who started its task, and sees what that person can see.

Known limits (not vulnerabilities, but tell us if you find a way to make them worse):

- Sandboxed runs share the host's network. An agent can reach services the server can reach, though it needs their credentials to use them.
- The **local** executor does not isolate agents. Use it only on a computer where every agent is trusted; server mode turns it off by default.
- luxd speaks plain HTTP. Put it behind a proxy that terminates TLS before other machines connect ([docs/self-hosting.md](docs/self-hosting.md)).
- People sign in with API tokens. Single sign-on is not built yet.
- Agents in a workspace are visible to everyone in that workspace.
