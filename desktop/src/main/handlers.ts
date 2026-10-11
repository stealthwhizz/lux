// Implementations of the IPC operations. No Electron imports here, so the
// handlers can be tested directly.
import { execFile } from "node:child_process";
import { request, requestBytes, uploadBytes, ws, follow, ApiError, type Connection } from "./api";
import { externalURL } from "../shared/ipc";
import type { ProfileStore } from "./profiles";
import { isOperation, validators, ValidationError, type Operation, type Result, type PushedEvent } from "../shared/ipc";
import type { AppInfo, LocalServerStatus } from "../shared/types";

export interface Deps {
  profiles: ProfileStore;
  appInfo: () => AppInfo;
  openExternal: (url: string) => Promise<void>;
  /** Push a message to the renderer that owns a subscription. */
  push: (owner: number, msg: PushedEvent) => void;
  /** Path to the `lux` CLI, or null when it is not installed. */
  luxCLI: () => string | null;
  /** Ask where to save a file and write it; false if the person cancelled. */
  saveFile: (name: string, bytes: Uint8Array) => Promise<boolean>;
  /** Ask which files to attach; empty if the person cancelled. */
  pickFiles: () => Promise<{ name: string; bytes: Uint8Array }[]>;
  fetchImpl?: typeof fetch;
}

type Handler = (args: any, ctx: { sender: number }) => Promise<unknown>;

export class Handlers {
  private subs = new Map<string, { owner: number; stop: () => void }>();
  private nextSub = 1;

  constructor(private d: Deps) {}

  private conn(): Connection {
    return this.d.profiles.connection();
  }

  private api<T>(method: string, path: string, body?: unknown, conn = this.conn()): Promise<T> {
    return request<T>(conn, method, path, body, this.d.fetchImpl);
  }

  private fileBytes(agent: string, path: string) {
    const c = this.conn();
    return requestBytes(c, ws(c, `/agents/${encodeURIComponent(agent)}/files/content?path=${encodeURIComponent(path)}`), this.d.fetchImpl);
  }

  private wsApi<T>(method: string, path: string, body?: unknown): Promise<T> {
    const c = this.conn();
    return this.api<T>(method, ws(c, path), body, c);
  }

  private table: Record<Operation, Handler> = {
    "app.info": async () => this.d.appInfo(),
    "profiles.list": async () => this.d.profiles.state(),
    "profiles.select": async (name: string) => {
      this.d.profiles.select(name);
      this.stopAll();
      return this.d.profiles.state();
    },
    "profiles.add": async ({ name, url }) => {
      this.d.profiles.add(name, url);
      return this.d.profiles.state();
    },
    "profiles.remove": async (name: string) => {
      this.d.profiles.remove(name);
      return this.d.profiles.state();
    },
    "profiles.signIn": async ({ profile, token }) => {
      // Verify before storing: a wrong token is never kept.
      const base = this.d.profiles.connection(profile);
      const me = await this.api("GET", "/v1/me", undefined, { ...base, token });
      const storage = this.d.profiles.saveToken(profile, token);
      return { me, storage, state: this.d.profiles.state() };
    },
    "profiles.signOut": async (name: string) => {
      this.d.profiles.signOut(name);
      return this.d.profiles.state();
    },
    "server.status": async () => this.api("GET", "/v1/status"),
    "me": async () => this.api("GET", "/v1/me"),
    "agents.list": async () => (await this.wsApi<{ agents: unknown[] }>("GET", "/agents")).agents,
    "agents.get": async (a: string) => this.wsApi("GET", `/agents/${encodeURIComponent(a)}`),
    "agents.validate": async (i) => this.wsApi("POST", "/agents/validate", { source_kind: i.sourceKind, source_url: i.sourceUrl, source_ref: i.sourceRef }),
    "agents.import": async (i) => this.wsApi("POST", "/agents", { source_kind: i.sourceKind, source_url: i.sourceUrl, source_ref: i.sourceRef, slug: i.slug, credential: i.credential }),
    "agents.create": async (d) => this.wsApi("POST", "/agents/new", d),
    "agents.definition": async (a: string) => this.wsApi("GET", `/agents/${encodeURIComponent(a)}/definition`),
    "agents.updateDefinition": async ({ agent, definition }) => this.wsApi("PUT", `/agents/${encodeURIComponent(agent)}/definition`, definition),
    "files.list": async (agent: string) => (await this.wsApi<{ files: unknown[] }>("GET", `/agents/${encodeURIComponent(agent)}/files`)).files,
    "files.preview": async ({ agent, path }) => {
      const { bytes, contentType } = await this.fileBytes(agent, path);
      if (contentType.startsWith("text/plain") && bytes.length <= 1 << 20) return { kind: "text", text: new TextDecoder().decode(bytes) };
      if (/^image\/(png|jpeg|gif|webp)$/.test(contentType) && bytes.length <= 10 << 20) {
        return { kind: "image", dataUrl: `data:${contentType};base64,${Buffer.from(bytes).toString("base64")}` };
      }
      return { kind: "none", size: bytes.length };
    },
    "files.save": async ({ agent, path }) => {
      const { bytes } = await this.fileBytes(agent, path);
      return { saved: await this.d.saveFile(path.split("/").pop() ?? "file", bytes) };
    },
    "agents.sync": async (a: string) => this.wsApi("POST", `/agents/${encodeURIComponent(a)}/sync`),
    "agents.delete": async (a: string) => this.wsApi("DELETE", `/agents/${encodeURIComponent(a)}`),
    "agents.introduce": async (a: string) => this.wsApi("POST", `/agents/${encodeURIComponent(a)}/introduce`),
    "agents.rename": async ({ agent, name }) => this.wsApi("PATCH", `/agents/${encodeURIComponent(agent)}`, { name }),
    "agents.setDisabled": async ({ agent, disabled }) => this.wsApi("PATCH", `/agents/${encodeURIComponent(agent)}`, { disabled }),
    "tasks.list": async (f) => {
      const q = new URLSearchParams();
      if (f.agent) q.set("agent", f.agent);
      if (f.state) q.set("state", f.state);
      if (f.limit) q.set("limit", String(f.limit));
      return (await this.wsApi<{ tasks: unknown[] }>("GET", `/tasks?${q}`)).tasks;
    },
    "tasks.get": async (id: string) => this.wsApi("GET", `/tasks/${id}`),
    "tasks.create": async ({ agent, prompt, repository }) => this.wsApi("POST", "/tasks", { agent, prompt, repository }),
    "tasks.cancel": async (id: string) => this.wsApi("POST", `/tasks/${id}/cancel`),
    "tasks.events": async ({ task, after }) => this.wsApi("GET", `/tasks/${task}/events?after=${after}`),
    "approvals.list": async (state: string) => (await this.wsApi<{ approvals: unknown[] }>("GET", `/approvals${state ? `?state=${state}` : ""}`)).approvals,
    "approvals.decide": async ({ approval, decision, note }) => this.wsApi("POST", `/approvals/${approval}/decision`, { decision, note }),
    "repositories.list": async () => (await this.wsApi<{ repositories: unknown[] }>("GET", "/repositories")).repositories,
    "changes.queue": async (state: string) => (await this.wsApi<{ changes: unknown[] }>("GET", `/changes${state ? `?state=${state}` : ""}`)).changes,
    "changes.forTask": async (task: string) => (await this.wsApi<{ changes: unknown[] }>("GET", `/tasks/${task}/changes`)).changes,
    "changes.patch": async ({ task, target }) => {
      const c = this.conn();
      return request<string>(c, "GET", ws(c, `/tasks/${task}/changes/${target}/patch`), undefined, this.d.fetchImpl, true);
    },
    "changes.discard": async ({ task, target }) => this.wsApi("POST", `/tasks/${task}/changes/${target}/discard`),
    "changes.publish": async ({ task, target, pullRequest, title }) =>
      this.wsApi("POST", `/tasks/${task}/changes/${target}/publish`, { pull_request: pullRequest, ...(title ? { title } : {}) }),
    "conversations.list": async (agent?: string) =>
      (await this.wsApi<{ conversations: unknown[] }>("GET", `/conversations${agent ? `?agent=${encodeURIComponent(agent)}` : ""}`)).conversations,
    "conversations.get": async (id: string) => this.wsApi("GET", `/conversations/${id}`),
    "conversations.create": async ({ agent, message, attachments }) => this.wsApi("POST", "/conversations", { agent, message, attachments }),
    "conversations.send": async ({ conversation, content, attachments }) => this.wsApi("POST", `/conversations/${conversation}/messages`, { content, attachments }),
    "models.list": async () => this.api("GET", "/v1/models"),
    "models.available": async (p: string) => (await this.api<{ models: unknown[] }>("GET", `/v1/models/${p}/available`)).models,
    "models.setKey": async ({ provider, apiKey }) => (await this.api<{ models: unknown[] }>("PUT", `/v1/models/${provider}/key`, { api_key: apiKey })).models,
    "models.removeKey": async (p: string) => this.api("DELETE", `/v1/models/${p}/key`),
    "settings.defaultModel": async () => this.api("GET", "/v1/settings/default-model"),
    "settings.setDefaultModel": async (model: string) => this.api("PUT", "/v1/settings/default-model", { model }),
    "connectors.list": async () => (await this.api<{ connectors: unknown[] }>("GET", "/v1/connectors")).connectors,
    "connectors.connect": async (id: string) => {
      const { url } = await this.api<{ url: string }>("POST", `/v1/connectors/${id}/connect`);
      await this.d.openExternal(externalURL(url)); // sign in happens in the person's own browser
      return { opened: true };
    },
    "connectors.disconnect": async (id: string) => this.api("DELETE", `/v1/connectors/${id}`),
    "connectors.pasteToken": async (token: string) => this.api("PUT", "/v1/connectors/slack/token", { token }),
    "connectors.setup": async ({ id, clientId, clientSecret, tenant }) => this.api("PUT", `/v1/connectors/${id}/setup`, { client_id: clientId, client_secret: clientSecret, tenant }),
    "routines.list": async (agent: string) => (await this.wsApi<{ routines: unknown[] }>("GET", `/agents/${encodeURIComponent(agent)}/routines`)).routines,
    "routines.create": async ({ agent, ...r }) => this.wsApi("POST", `/agents/${encodeURIComponent(agent)}/routines`, r),
    "routines.setPaused": async ({ routine, paused }) => this.wsApi("PATCH", `/routines/${routine}`, { paused }),
    "routines.remove": async (routine: string) => this.wsApi("DELETE", `/routines/${routine}`),
    "routines.runNow": async (routine: string) => this.wsApi("POST", `/routines/${routine}/run`),
    "files.attach": async (agent: string) => {
      const c = this.conn();
      const out = [];
      for (const f of await this.d.pickFiles()) {
        if (f.bytes.length > 25 << 20) throw new ApiError(413, "too_large", `${f.name} is larger than 25 MB`);
        out.push(await uploadBytes(c, ws(c, `/agents/${encodeURIComponent(agent)}/uploads?name=${encodeURIComponent(f.name)}`), f.bytes, this.d.fetchImpl));
      }
      return out;
    },
    "localServer.status": async () => this.localServer("status"),
    "localServer.start": async () => this.localServer("start"),
    "localServer.stop": async () => this.localServer("stop"),
    "events.subscribeTask": async (task: string, ctx) => {
      const c = this.conn();
      return this.subscribe(ctx.sender, c, (last) => ws(c, `/tasks/${task}/events/stream${last ? `?after=${encodeURIComponent(last)}` : ""}`));
    },
    "events.subscribeWorkspace": async (_: unknown, ctx) => {
      const c = this.conn();
      return this.subscribe(ctx.sender, c, () => ws(c, "/events/stream"));
    },
    "events.unsubscribe": async (sub: string, ctx) => {
      const s = this.subs.get(sub);
      if (s && s.owner === ctx.sender) {
        s.stop();
        this.subs.delete(sub);
      }
      return true;
    },
    "shell.openExternal": async (url: string) => {
      await this.d.openExternal(url);
      return true;
    },
  };

  private subscribe(owner: number, conn: Connection, pathFor: (last?: string) => string): string {
    const id = `sub_${this.nextSub++}`;
    const f = follow(conn, pathFor, {
      onMessage: (m) => {
        let data: unknown = m.data;
        try {
          data = JSON.parse(m.data);
        } catch {
          /* keep raw */
        }
        if (m.event === "end") this.d.push(owner, { subscription: id, kind: "end", data });
        else if (m.event === "error") this.d.push(owner, { subscription: id, kind: "error", data });
        else this.d.push(owner, { subscription: id, kind: "event", data: { event: m.event, data } });
      },
      onStatus: (connected, error) => this.d.push(owner, { subscription: id, kind: "status", data: { connected, error } }),
    }, this.d.fetchImpl);
    this.subs.set(id, { owner, stop: f.stop });
    f.done.then(() => this.subs.delete(id));
    return id;
  }

  /** Stop every subscription (profile switch) or those of one renderer. */
  stopAll(owner?: number) {
    for (const [id, s] of this.subs) {
      if (owner === undefined || s.owner === owner) {
        s.stop();
        this.subs.delete(id);
      }
    }
  }

  private localServer(action: "status" | "start" | "stop"): Promise<LocalServerStatus> {
    const cli = this.d.luxCLI();
    if (!cli) return Promise.resolve({ available: false, running: false, message: "The lux command-line tool was not found. Install it to start a local server from the app." });
    return new Promise((resolve) => {
      // argv form, fixed arguments: nothing from the renderer reaches the command line.
      execFile(cli, ["server", action, "--json"], { timeout: 60_000 }, (err, stdout, stderr) => {
        let parsed: any = {};
        try {
          parsed = JSON.parse(stdout || "{}");
        } catch {
          /* human output */
        }
        if (action === "status") {
          resolve({ available: true, running: !!parsed.ready, url: parsed.url, pid: parsed.pid || undefined, message: err ? (stderr || "").trim() : undefined });
          return;
        }
        resolve({ available: true, running: action === "start" && !err, message: (err ? stderr : stdout).toString().trim() });
      });
    });
  }

  /** Validate and run one operation. Errors become a Result, never an exception. */
  async invoke(op: unknown, args: unknown, sender: number): Promise<Result<unknown>> {
    if (!isOperation(op)) return { ok: false, error: { code: "unknown_operation", message: "unknown operation" } };
    let input: unknown;
    try {
      input = (validators[op] as (a: unknown) => unknown)(args);
    } catch (err) {
      if (err instanceof ValidationError) return { ok: false, error: { code: "invalid_input", message: err.message } };
      return { ok: false, error: { code: "invalid_input", message: "invalid input" } };
    }
    try {
      return { ok: true, data: await this.table[op](input, { sender }) };
    } catch (err: any) {
      if (err instanceof ApiError) return { ok: false, error: { code: err.code, message: err.message, status: err.status, details: err.details } };
      return { ok: false, error: { code: "error", message: err?.message ?? String(err) } };
    }
  }
}
