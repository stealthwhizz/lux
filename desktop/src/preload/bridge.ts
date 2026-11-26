// The DesktopBridge: what the renderer can do. Built from two primitives so
// it can be tested without Electron, and so a different shell (for example
// Tauri) could provide the same object.
import type { AgentDefinition, ChangeTarget, ImportInput, Operation, PushedEvent, Result, TaskFilter } from "../shared/ipc";
import type {
  Agent, AgentMemory, AgentWork, AppInfo, BrowserInput, BrowserView, ModelChoice, ModelProviders, Approval, Attachment, Change, ConnectorInfo, Conversation, FilePreview, Repository, Routine, WorkspaceFile, LocalServerStatus, Me, Message, ProfilesState, Report, ServerStatus, Task, TaskEvent,
} from "../shared/types";

export type Invoke = (op: Operation, args?: unknown) => Promise<Result<unknown>>;
export type Listen = (listener: (msg: PushedEvent) => void) => () => void;

export function buildBridge(invoke: Invoke, listen: Listen) {
  const call = <T>(op: Operation, args?: unknown) => invoke(op, args) as Promise<Result<T>>;
  return Object.freeze({
    app: Object.freeze({
      info: () => call<AppInfo>("app.info"),
      notify: (title: string, body: string, agent?: string) => call<boolean>("app.notify", agent ? { title, body, agent } : { title, body }),
    }),
    profiles: Object.freeze({
      list: () => call<ProfilesState>("profiles.list"),
      select: (name: string) => call<ProfilesState>("profiles.select", name),
      add: (name: string, url: string) => call<ProfilesState>("profiles.add", { name, url }),
      remove: (name: string) => call<ProfilesState>("profiles.remove", name),
      signIn: (profile: string, token: string) => call<{ me: Me; storage: "encrypted" | "session"; state: ProfilesState }>("profiles.signIn", { profile, token }),
      signOut: (name: string) => call<ProfilesState>("profiles.signOut", name),
    }),
    server: Object.freeze({ status: () => call<ServerStatus>("server.status"), me: () => call<Me>("me") }),
    agents: Object.freeze({
      list: () => call<Agent[]>("agents.list"),
      get: (agent: string) => call<Agent>("agents.get", agent),
      validate: (i: ImportInput) => call<{ commit_sha: string; report: Report }>("agents.validate", i),
      import: (i: ImportInput) => call<{ agent: Agent; report: Report }>("agents.import", i),
      create: (d: AgentDefinition) => call<{ agent: Agent; report: Report }>("agents.create", d),
      definition: (agent: string) => call<AgentDefinition>("agents.definition", agent),
      updateDefinition: (agent: string, definition: AgentDefinition) =>
        call<{ agent: Agent; report: Report; changed: boolean }>("agents.updateDefinition", { agent, definition }),
      sync: (agent: string) => call<{ agent: Agent; report: Report; changed: boolean }>("agents.sync", agent),
      rename: (agent: string, name: string) => call<Agent>("agents.rename", { agent, name }),
      setDisabled: (agent: string, disabled: boolean) => call<Agent>("agents.setDisabled", { agent, disabled }),
      delete: (agent: string) => call<void>("agents.delete", agent),
      work: (agent: string) => call<AgentWork>("agents.work", agent),
      browser: (agent: string) => call<BrowserView>("agents.browser", agent),
      browserInput: (agent: string, input: BrowserInput) => call<BrowserView>("agents.browserInput", { agent, ...input }),
      browserRelease: (agent: string) => call<null>("agents.browserRelease", agent),
      memory: (agent: string) => call<AgentMemory>("agents.memory", agent),
      setMemory: (agent: string, text: string) => call<AgentMemory>("agents.setMemory", { agent, text }),
      introduce: (agent: string) => call<{ conversation: Conversation }>("agents.introduce", agent),
    }),
    tasks: Object.freeze({
      list: (f?: TaskFilter) => call<Task[]>("tasks.list", f),
      get: (id: string) => call<Task>("tasks.get", id),
      create: (agent: string, prompt: string, repository?: string) => call<Task>("tasks.create", { agent, prompt, ...(repository ? { repository } : {}) }),
      cancel: (id: string) => call<Task>("tasks.cancel", id),
      events: (task: string, after = 0) => call<{ events: TaskEvent[]; task_state: string }>("tasks.events", { task, after }),
    }),
    approvals: Object.freeze({
      list: (state?: string) => call<Approval[]>("approvals.list", state ?? ""),
      decide: (approval: string, decision: "approve" | "reject", note?: string) => call<Approval>("approvals.decide", { approval, decision, note }),
    }),
    files: Object.freeze({
      list: (agent: string) => call<WorkspaceFile[]>("files.list", agent),
      preview: (agent: string, path: string) => call<FilePreview>("files.preview", { agent, path }),
      save: (agent: string, path: string) => call<{ saved: boolean }>("files.save", { agent, path }),
      /** Choose files with the system dialog and upload them for the agent. */
      attach: (agent: string) => call<Attachment[]>("files.attach", agent),
    }),
    models: Object.freeze({
      list: () => call<ModelProviders>("models.list"),
      available: (provider: string) => call<ModelChoice[]>("models.available", provider),
      setKey: (provider: string, apiKey: string) => call<ModelChoice[]>("models.setKey", { provider, apiKey }),
      removeKey: (provider: string) => call<null>("models.removeKey", provider),
    }),
    settings: Object.freeze({
      defaultModel: () => call<{ model: string; can_set: boolean }>("settings.defaultModel"),
      setDefaultModel: (model: string) => call<{ model: string; can_set: boolean }>("settings.setDefaultModel", model),
    }),
    connectors: Object.freeze({
      list: () => call<ConnectorInfo[]>("connectors.list"),
      /** Opens the provider's sign-in in the system browser. */
      connect: (id: string) => call<{ opened: boolean }>("connectors.connect", id),
      disconnect: (id: string) => call<null>("connectors.disconnect", id),
      pasteToken: (token: string) => call<null>("connectors.pasteToken", token),
      setupBot: (botToken: string, appToken: string) => call<null>("connectors.setupBot", { botToken, appToken }),
      /** Administrators: save the OAuth client people connect through. */
      setup: (id: string, clientId: string, clientSecret: string, tenant?: string) =>
        call<{ redirect_uri: string }>("connectors.setup", { id, clientId, clientSecret, ...(tenant ? { tenant } : {}) }),
    }),
    routines: Object.freeze({
      list: (agent: string) => call<Routine[]>("routines.list", agent),
      create: (agent: string, r: { name: string; prompt: string; cron: string; timezone: string }) => call<Routine>("routines.create", { agent, ...r }),
      setPaused: (routine: string, paused: boolean) => call<Routine>("routines.setPaused", { routine, paused }),
      remove: (routine: string) => call<null>("routines.remove", routine),
      runNow: (routine: string) => call<{ due: boolean }>("routines.runNow", routine),
    }),
    repositories: Object.freeze({ list: () => call<Repository[]>("repositories.list") }),
    changes: Object.freeze({
      queue: (state?: string) => call<Change[]>("changes.queue", state ?? ""),
      forTask: (task: string) => call<Change[]>("changes.forTask", task),
      patch: (task: string, target: ChangeTarget) => call<string>("changes.patch", { task, target }),
      discard: (task: string, target: ChangeTarget) => call<Change>("changes.discard", { task, target }),
      publish: (task: string, target: ChangeTarget, pullRequest: boolean, title?: string) =>
        call<{ change: Change; pull_request_error?: string }>("changes.publish", { task, target, pullRequest, ...(title ? { title } : {}) }),
    }),
    conversations: Object.freeze({
      list: (agent?: string) => call<Conversation[]>("conversations.list", agent),
      get: (id: string) => call<{ conversation: Conversation; messages: Message[] }>("conversations.get", id),
      create: (agent: string, message: string, attachments: string[] = []) =>
        call<{ conversation: Conversation; message: Message; task: Task }>("conversations.create", { agent, message, attachments }),
      send: (conversation: string, content: string, attachments: string[] = []) =>
        call<{ message: Message; task: Task }>("conversations.send", { conversation, content, attachments }),
    }),
    localServer: Object.freeze({
      status: () => call<LocalServerStatus>("localServer.status"),
      start: () => call<LocalServerStatus>("localServer.start"),
      stop: () => call<LocalServerStatus>("localServer.stop"),
    }),
    events: Object.freeze({
      subscribeTask: (task: string) => call<string>("events.subscribeTask", task),
      subscribeWorkspace: () => call<string>("events.subscribeWorkspace"),
      unsubscribe: (sub: string) => call<boolean>("events.unsubscribe", sub),
      /** Receive pushed events; returns a function that stops listening. */
      onEvent: (listener: (msg: PushedEvent) => void) => listen(listener),
    }),
    openExternal: (url: string) => call<boolean>("shell.openExternal", url),
  });
}

export type DesktopBridge = ReturnType<typeof buildBridge>;
