# Connectors

Connectors let agents use a person's own accounts: Google, Microsoft 365 and Slack. Each person connects their own account under **… → Connectors** in the desktop app. An agent then acts as the person who gave it the task, and sees only what that person can see.

## How it works

- **Who sets up what.** Lux is open source, so it ships no client IDs or secrets. Each Lux server brings its own app registration with Google, Microsoft or Slack, made once by an administrator. After that, everyone on the server just presses **Connect**. This is the usual model for self-hosted software, and it means account data only travels between your server and the provider.
- **Where tokens live.** Account tokens and app secrets are encrypted in luxd's database with AES-256-GCM. The key comes from `LUX_SECRET_KEY` (32 bytes, base64: `openssl rand -base64 32`), or luxd creates one in its data folder. Keep that key, or the data folder, in your backups: without it, people must connect again.
- **How agents reach them.** A run never receives a token. luxd serves connector tools to the run over MCP (`/internal/v1/runs/{run}/mcp`), authorized by the run's short-lived token, and makes the provider call itself.
- **Rules.** luxd checks the workspace rules ([policy.md](policy.md)) for every connector call, whatever the runtime did. By default reading is allowed and sending or creating needs approval. An approval covers exactly the call that was approved, and works once.
- **Which tasks get them.** A task gets the tools of the person who started it. A routine runs as the person who made it. Agent schedules defined in a repository have no person and get no connector tools.
- **Audit.** Connecting, disconnecting and setup are recorded in the audit log. Each tool call is recorded in the task's events (`lux.connector_call`, with provider, tool and success, but not the content).

## Google (Gmail, Calendar, Drive)

| Tool | What it does | Default rule |
|---|---|---|
| `gmail_search`, `gmail_read` | Search and read email | Allowed |
| `gmail_draft` | Save a draft | Needs approval |
| `gmail_send` | Send email | Needs approval |
| `calendar_events` | List events on the primary calendar | Allowed |
| `calendar_create` | Add an event | Needs approval |
| `drive_search`, `drive_read` | Search Drive; read Docs and Slides as text, Sheets as CSV, text files as they are | Allowed |

Scopes requested: `gmail.readonly`, `gmail.compose`, `calendar.events`, `drive.readonly`, plus the account's address.

**Set up (administrator, once):**
1. In [Google Cloud Console](https://console.cloud.google.com/apis/credentials), enable the Gmail API, the Google Calendar API and the Google Drive API.
2. Configure the OAuth consent screen. For a company, choose user type **Internal**: only your Google Workspace's people can connect, and Google's app verification is not needed. An **External** app in testing mode works for up to 100 test users you list.
3. Create credentials: **OAuth client ID**. For a server on your own computer, use type **Desktop app**. For a shared server, use **Web application** and add the redirect URI `<LUX_PUBLIC_URL>/v1/connectors/google/callback`.
4. In Lux, press **Connect** on Google. As an administrator you are asked for the client ID and secret, once. Or set `LUX_GOOGLE_CLIENT_ID` and `LUX_GOOGLE_CLIENT_SECRET` on the server; values there take precedence and can't be changed in the app.

Gmail and Drive scopes are "restricted" in Google's terms. Offering a Lux server to the public (an External app in production) needs Google's verification and a yearly security assessment. Internal apps don't.

People who connected Google before Drive was added see **Connect again**. Until they reconnect, their agents just don't get the Drive tools.

## Microsoft 365 (Outlook, calendar, OneDrive)

| Tool | What it does | Default rule |
|---|---|---|
| `outlook_search`, `outlook_read` | Search and read email | Allowed |
| `outlook_send` | Send email | Needs approval |
| `outlook_events` | List calendar events | Allowed |
| `outlook_create_event` | Add an event | Needs approval |
| `onedrive_search`, `onedrive_read` | Search OneDrive; read Word documents and text files (up to 20 MB) | Allowed |

Delegated Microsoft Graph permissions: `User.Read`, `Mail.Read`, `Mail.Send`, `Calendars.ReadWrite`, `Files.Read`, `offline_access`.

**Set up (administrator, once):**
1. In the [Microsoft Entra admin center](https://entra.microsoft.com/), open **App registrations**, then **New registration**. Name it Lux, with accounts in this organizational directory only.
2. Under **Redirect URI**, choose **Web** and enter the address Lux shows in its setup dialog: `<LUX_PUBLIC_URL>/v1/connectors/microsoft/callback`. Entra accepts `http://` only for `localhost`, so for a server on `http://127.0.0.1:7878`, Lux uses `http://localhost:7878/...`.
3. In **API permissions**, add the delegated Microsoft Graph permissions above, then **Grant admin consent**.
4. In **Certificates & secrets**, add a client secret and copy its **Value** (not its ID).
5. In Lux, press **Connect** on Microsoft 365 and paste the Application (client) ID, the Directory (tenant) ID or your domain, and the secret value.

Client secrets expire (at most two years). When one does, Connect stops working; add a new secret and enter it again in Lux.

## Slack

| Tool | What it does | Default rule |
|---|---|---|
| `slack_search` | Search messages the person can see | Allowed |
| `slack_channels` | List the person's channels and conversations | Allowed |
| `slack_read` | Read recent messages in a channel or conversation | Allowed |
| `slack_send` | Post a message as the person | Needs approval |

Lux uses a Slack **user token** (`xoxp-`) per person, so agents act as that person and see only their conversations. User scopes: `search:read`, `channels:read`, `groups:read`, `im:read`, `mpim:read`, `channels:history`, `groups:history`, `im:history`, `mpim:history`, `chat:write`, `users:read`.

Slack's sign-in only returns to `https://` addresses. So there are two ways in:

**Connect (servers on an `https://` address).** An administrator presses Connect once and follows the dialog:
1. **Create the Lux app in Slack** opens Slack with the app already filled in: name, scopes and redirect URL, from a manifest.
2. Pick the workspace and create it. If your workspace requires it, a Slack admin approves the app.
3. Paste the Client ID and Client Secret from the app's **Basic Information** page into Lux.

After that, people press Connect and approve in Slack.

**Paste a token (any server, including your own computer).** Press Connect, then:
1. **Create the Lux app in Slack**, which opens the same prefilled manifest, without a redirect URL.
2. In the app, **Install App → Install to Workspace**, then Allow.
3. Copy the **User OAuth Token** (it starts with `xoxp-`) and paste it into Lux.

Lux checks the token with Slack (`auth.test`) before saving it.

## Disconnecting

**Disconnect** deletes the person's stored token. Their agents lose those tools from the next tool call. It doesn't revoke the grant at the provider; to do that, remove Lux in your Google account's security settings, in My Apps (Microsoft), or in Slack's app settings.

## Limits

- Tested against stand-in Google, Microsoft and Slack services, not yet against real accounts.
- Drive and OneDrive are read-only. PDFs, images and other binary files in Drive and OneDrive are listed but not read.
- An agent reads Slack live. Lux does not copy Slack history into a knowledge base.
- Each provider's app settings are set in the app (or, for Google, in luxd's settings). With several luxd replicas they are read from the shared database, so all replicas see them.
