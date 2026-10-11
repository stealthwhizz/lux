# Using Lux

This guide covers the desktop app. Everything here also works from the [`lux` command](cli.md).

## Agents

The sidebar lists your agents. Each agent is one ongoing conversation. Under a name you only see a status when there is one: **Working…**, **Needs you** (waiting for your approval) or **Off**.

**Create an agent.** Press **+** and **Create an agent**. You get an agent called "New agent"; its chat opens and it greets you, asks what it should do, and suggests first jobs as buttons. After your first message it renames itself after that job in one or two words, like "Calendar Planner", using its own model. If the model gives nothing usable, it stays "New agent".

The first time anyone creates an agent on a server, an administrator chooses the model new agents use under **… → Models** (see [models.md](models.md)).

**Change an agent.** Right-click it in the sidebar (or hover and press **…**), or click its name at the top of its chat:

| Action | What it does |
|---|---|
| **Rename** | Edit the name in place; Enter saves, Escape cancels. The agent's id (its slug) does not change. |
| **Turn off / Turn on** | An agent that is off cannot start tasks, and its routines don't run. It stays in the list, dimmed. |
| **Delete** | Asks first. Its chat and routines go away and its name can be used again. What it already did stays in **All tasks**. An agent that is in the middle of a task can't be deleted; stop the task first. |
| **Files, Routines, Edit** | In the menu at the top of the chat. **Edit** changes instructions and model. |

**Import an agent** from a git repository: **+ → Import from a git repository**. Imported agents keep their instructions and model in the repository; in Lux you can rename them and turn them on or off. See [git-workflows.md](git-workflows.md) and [agent-validation.md](agent-validation.md).

## Talking to an agent

Type and press Enter (Shift+Enter for a new line). While the agent works, a card under the conversation shows what it is doing in plain words ("Reading planner.md", "Searching your email"). Press ■ to stop it.

**Answer buttons.** When an agent asks a question with a few likely answers, they appear as buttons under its reply. Click one to answer, or type your own. Agents produce these with a `choices` block at the end of a reply; models that ignore the instruction just ask in text.

**Attachments.** Press **+** in the message box to attach up to 10 files (25 MB each). Word documents, and PDFs when the server has `pdftotext`, get a plain-text copy the agent reads. The agent sees an image's name and size, not its contents.

**Files.** Each agent has a workspace folder that is kept between conversations. Files it makes there appear as chips under its reply and in **Files** (in the agent menu), where you can preview and save them.

## Approvals

What an agent may do alone is set by the workspace rules ([policy.md](policy.md)). By default an agent may read its own files and your connected accounts and write in its own workspace folder; a person must approve every shell command, writing or reading outside its folder, and anything that changes the outside world: sending email, posting in Slack, adding calendar events.

When an agent needs you, its avatar turns orange and an **Allow / Don't allow** card appears in the chat. **Needs you** at the top of the sidebar collects everything waiting across agents. An approval covers exactly the call you saw: the same email to a different person needs a new approval, and one approval sends once.

## Routines

**Routines** (in the agent menu) run the agent on a schedule: every day, on weekdays, or on one weekday, at a time you choose. The result is posted into the agent's chat, as if you had asked. Routines run on the server, so they run whether or not the app is open. If the server was down when one was due, it runs once when the server is back.

## Connectors and models

- **… → Connectors**: connect your Google, Microsoft 365 or Slack account so your agents can use it ([connectors.md](connectors.md)).
- **… → Models**: administrators add model keys and choose the model new agents use ([models.md](models.md)).

## When the app is closed

The app is a window onto your Lux server. Closing it, or closing the laptop, does not stop agents running on a server elsewhere. If the server runs on the same laptop (`lux server start`), it sleeps with the laptop: a task in progress pauses, and a routine that came due runs once when the laptop wakes.

## Keyboard

| Keys | Action |
|---|---|
| Ctrl/⌘ K | Search everything |
| Ctrl/⌘ 1–4 | Conversations, Needs you, All tasks, Agents |
| Ctrl/⌘ , | Settings |
| Enter / Shift+Enter | Send / new line |
| Escape | Close a menu or dialog |
