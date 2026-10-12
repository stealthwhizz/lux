// The contract between the renderer and the main process.
//
// The renderer can call only the operations listed in `operations`. The main
// process validates every argument with the matching validator before doing
// anything; a value that fails validation never reaches the network or disk.

export type Result<T> = { ok: true; data: T } | { ok: false; error: LuxError };

export interface LuxError {
  code: string;
  message: string;
  status?: number;
  details?: unknown;
}

export class ValidationError extends Error {}

// ---- small validators (no dependencies, so they run in main and tests) ----

const ID_RE = /^[a-z]+_[0-9a-z]{4,64}$/;
const SLUG_OR_ID_RE = /^[a-z][a-z0-9_-]{0,80}$/;
const STATES = ["queued", "starting", "running", "waiting_approval", "cancelling", "succeeded", "failed", "cancelled", "timed_out"];

function obj(v: unknown, what: string): Record<string, unknown> {
  if (typeof v !== "object" || v === null || Array.isArray(v)) throw new ValidationError(`${what} must be an object`);
  return v as Record<string, unknown>;
}

function onlyKeys(o: Record<string, unknown>, allowed: string[], what: string) {
  for (const k of Object.keys(o)) {
    if (!allowed.includes(k)) throw new ValidationError(`${what}: unexpected field "${k}"`);
  }
}

export function str(v: unknown, what: string, { min = 1, max = 1000, re }: { min?: number; max?: number; re?: RegExp } = {}): string {
  if (typeof v !== "string") throw new ValidationError(`${what} must be a string`);
  if (v.length < min || v.length > max) throw new ValidationError(`${what} must be ${min}–${max} characters`);
  if (re && !re.test(v)) throw new ValidationError(`${what} has an invalid format`);
  return v;
}

function optStr(v: unknown, what: string, opts?: Parameters<typeof str>[2]): string | undefined {
  return v === undefined || v === "" ? undefined : str(v, what, opts);
}

function int(v: unknown, what: string, min: number, max: number): number {
  if (typeof v !== "number" || !Number.isInteger(v) || v < min || v > max) throw new ValidationError(`${what} must be an integer ${min}–${max}`);
  return v;
}

export const id = (v: unknown, what = "id") => str(v, what, { max: 80, re: ID_RE });
export const slugOrId = (v: unknown, what = "agent") => str(v, what, { max: 81, re: SLUG_OR_ID_RE });

/** A Lux server URL: http(s), no credentials, no query or fragment. */
export function serverURL(v: unknown): string {
  const s = str(v, "url", { max: 2000 });
  let u: URL;
  try {
    u = new URL(s);
  } catch {
    throw new ValidationError("url is not a valid URL");
  }
  if (u.protocol !== "https:" && u.protocol !== "http:") throw new ValidationError("url must use http or https");
  if (u.username || u.password) throw new ValidationError("url must not contain credentials");
  if (u.search || u.hash) throw new ValidationError("url must not have a query or fragment");
  return u.origin + u.pathname.replace(/\/+$/, "");
}

/** An external link the app may open in the system browser. */
export function externalURL(v: unknown): string {
  const s = str(v, "url", { max: 2000 });
  const u = new URL(s);
  if (u.protocol !== "https:") throw new ValidationError("only https links can be opened");
  return u.toString();
}

export type ChangeTarget = "repository" | "memory";

function changeRef(a: unknown): { task: string; target: ChangeTarget } {
  const o = obj(a, "change");
  onlyKeys(o, ["task", "target"], "change");
  if (o.target !== "repository" && o.target !== "memory") throw new ValidationError("target must be repository or memory");
  return { task: id(o.task, "task"), target: o.target };
}

// ---- operations ----

export interface ProfileInput { name: string; url: string }
export interface ImportInput { sourceKind: "git" | "local"; sourceUrl: string; sourceRef?: string; slug?: string; credential?: string }
export interface AgentDefinition { name: string; description: string; instructions: string; model: string }

function definition(a: unknown): AgentDefinition {
  const o = obj(a, "definition");
  onlyKeys(o, ["name", "description", "instructions", "model"], "definition");
  return {
    name: str(o.name, "name", { max: 60, re: /^[^\r\n\t\0]+$/ }),
    description: str(o.description, "description", { max: 300, re: /^[^\r\n]+$/ }),
    instructions: str(o.instructions, "instructions", { min: 0, max: 50_000 }),
    // Empty: the server's default model.
    model: o.model === "" ? "" : model(o.model),
  };
}

const model = (a: unknown) => str(a, "model", { max: 140, re: /^(lyzr|anthropic|openai|ollama):[A-Za-z0-9._:/@-]{1,128}$/ });

/** A file in an agent's workspace: relative, no "..", no backslashes. */
function fileRef(a: unknown): { agent: string; path: string } {
  const o = obj(a, "file");
  onlyKeys(o, ["agent", "path"], "file");
  const p = str(o.path, "path", { max: 1024, re: /^[^/\\\0][^\\\0]*$/ });
  if (p.split("/").some((s) => s === ".." || s === "." || s === "")) throw new ValidationError("path must stay inside the workspace");
  return { agent: slugOrId(o.agent), path: p };
}

/** Uploaded files sent with a message: uploads/<name>, at most 10. */
function attachments(a: unknown): string[] {
  if (a === undefined) return [];
  if (!Array.isArray(a) || a.length > 10) throw new ValidationError("attachments must be a list of at most 10 files");
  return a.map((p) => str(p, "attachment", { max: 200, re: /^uploads\/[^/\\\0]+$/ }));
}

export interface TaskFilter { agent?: string; state?: string; limit?: number }

export const validators = {
  "app.info": (_: unknown) => undefined,
  // A system notification: plain text only, short, optionally naming the agent to open on click.
  "app.notify": (a: unknown) => {
    const o = obj(a, "notification");
    onlyKeys(o, ["title", "body", "agent"], "notification");
    return {
      title: str(o.title, "title", { max: 100, re: /^[^\r\n]+$/ }),
      body: str(o.body, "body", { min: 0, max: 300 }),
      agent: o.agent === undefined ? undefined : slugOrId(o.agent),
    };
  },
  "profiles.list": (_: unknown) => undefined,
  "profiles.select": (a: unknown) => str(a, "profile", { max: 64, re: /^[A-Za-z0-9_.-]+$/ }),
  "profiles.add": (a: unknown): ProfileInput => {
    const o = obj(a, "profile");
    onlyKeys(o, ["name", "url"], "profile");
    return { name: str(o.name, "name", { max: 64, re: /^[A-Za-z0-9_.-]+$/ }), url: serverURL(o.url) };
  },
  "profiles.remove": (a: unknown) => str(a, "profile", { max: 64, re: /^[A-Za-z0-9_.-]+$/ }),
  "profiles.signIn": (a: unknown) => {
    const o = obj(a, "sign-in");
    onlyKeys(o, ["profile", "token"], "sign-in");
    return { profile: str(o.profile, "profile", { max: 64, re: /^[A-Za-z0-9_.-]+$/ }), token: str(o.token, "token", { max: 200, re: /^lux_pat_[0-9a-z]+$/ }) };
  },
  "profiles.signOut": (a: unknown) => str(a, "profile", { max: 64, re: /^[A-Za-z0-9_.-]+$/ }),
  "server.status": (_: unknown) => undefined,
  "me": (_: unknown) => undefined,
  "agents.list": (_: unknown) => undefined,
  "agents.get": (a: unknown) => slugOrId(a),
  "agents.validate": (a: unknown): ImportInput => validators["agents.import"](a),
  "agents.import": (a: unknown): ImportInput => {
    const o = obj(a, "import");
    onlyKeys(o, ["sourceKind", "sourceUrl", "sourceRef", "slug", "credential"], "import");
    if (o.sourceKind !== "git" && o.sourceKind !== "local") throw new ValidationError("sourceKind must be git or local");
    return {
      sourceKind: o.sourceKind,
      sourceUrl: str(o.sourceUrl, "sourceUrl", { max: 2000 }),
      sourceRef: optStr(o.sourceRef, "sourceRef", { max: 200, re: /^[A-Za-z0-9._/-]+$/ }),
      slug: optStr(o.slug, "slug", { max: 63, re: /^[a-z][a-z0-9-]*$/ }),
      credential: optStr(o.credential, "credential", { max: 64, re: /^[a-z0-9_]+$/ }),
    };
  },
  "agents.sync": (a: unknown) => slugOrId(a),
  "agents.delete": (a: unknown) => slugOrId(a),
  "agents.work": (a: unknown) => slugOrId(a),
  "agents.memory": (a: unknown) => slugOrId(a),
  "agents.setMemory": (a: unknown) => {
    const o = obj(a, "memory");
    onlyKeys(o, ["agent", "text"], "memory");
    return { agent: slugOrId(o.agent), text: str(o.text, "memory", { min: 0, max: 65536 }) };
  },
  "agents.introduce": (a: unknown) => slugOrId(a),
  "agents.create": (a: unknown) => definition(a),
  "files.list": (a: unknown) => slugOrId(a),
  "files.preview": (a: unknown) => fileRef(a),
  "files.save": (a: unknown) => fileRef(a),
  "agents.definition": (a: unknown) => slugOrId(a),
  "agents.updateDefinition": (a: unknown) => {
    const o = obj(a, "args");
    onlyKeys(o, ["agent", "definition"], "args");
    return { agent: slugOrId(o.agent), definition: definition(o.definition) };
  },
  "agents.rename": (a: unknown) => {
    const o = obj(a, "args");
    onlyKeys(o, ["agent", "name"], "args");
    return { agent: slugOrId(o.agent), name: str(o.name, "name", { max: 60, re: /^[^\r\n\t\0]+$/ }) };
  },
  "agents.setDisabled": (a: unknown) => {
    const o = obj(a, "args");
    onlyKeys(o, ["agent", "disabled"], "args");
    if (typeof o.disabled !== "boolean") throw new ValidationError("disabled must be a boolean");
    return { agent: slugOrId(o.agent), disabled: o.disabled };
  },
  "tasks.list": (a: unknown): TaskFilter => {
    if (a === undefined) return {};
    const o = obj(a, "filter");
    onlyKeys(o, ["agent", "state", "limit"], "filter");
    const state = optStr(o.state, "state", { max: 200 });
    if (state && !state.split(",").every((s) => STATES.includes(s))) throw new ValidationError("unknown state");
    return { agent: o.agent === undefined ? undefined : slugOrId(o.agent), state, limit: o.limit === undefined ? undefined : int(o.limit, "limit", 1, 500) };
  },
  "tasks.get": (a: unknown) => id(a, "task"),
  "tasks.create": (a: unknown) => {
    const o = obj(a, "task");
    onlyKeys(o, ["agent", "prompt", "repository"], "task");
    return { agent: slugOrId(o.agent), prompt: str(o.prompt, "prompt", { max: 100_000 }), repository: o.repository === undefined ? undefined : slugOrId(o.repository, "repository") };
  },
  "tasks.cancel": (a: unknown) => id(a, "task"),
  "tasks.events": (a: unknown) => {
    const o = obj(a, "args");
    onlyKeys(o, ["task", "after"], "args");
    return { task: id(o.task, "task"), after: o.after === undefined ? 0 : int(o.after, "after", 0, 2 ** 31 - 1) };
  },
  "approvals.list": (a: unknown) => {
    if (a === undefined || a === "") return "";
    return str(a, "state", { max: 20, re: /^(pending|approved|rejected|expired|cancelled)$/ });
  },
  "approvals.decide": (a: unknown) => {
    const o = obj(a, "decision");
    onlyKeys(o, ["approval", "decision", "note"], "decision");
    if (o.decision !== "approve" && o.decision !== "reject") throw new ValidationError("decision must be approve or reject");
    return { approval: id(o.approval, "approval"), decision: o.decision, note: o.note === undefined ? "" : str(o.note, "note", { min: 0, max: 2000 }) };
  },
  "repositories.list": (_: unknown) => undefined,
  "changes.queue": (a: unknown) => {
    if (a === undefined || a === "") return "";
    return str(a, "state", { max: 20, re: /^(proposed|publishing|published|discarded)$/ });
  },
  "changes.forTask": (a: unknown) => id(a, "task"),
  "changes.patch": (a: unknown) => changeRef(a),
  "changes.discard": (a: unknown) => changeRef(a),
  "changes.publish": (a: unknown) => {
    const o = obj(a, "publish");
    onlyKeys(o, ["task", "target", "pullRequest", "title"], "publish");
    if (o.pullRequest !== undefined && typeof o.pullRequest !== "boolean") throw new ValidationError("pullRequest must be a boolean");
    return { ...changeRef({ task: o.task, target: o.target }), pullRequest: o.pullRequest === true, title: o.title === undefined ? "" : str(o.title, "title", { min: 0, max: 200, re: /^[^\r\n]*$/ }) };
  },
  "conversations.list": (a: unknown) => (a === undefined ? undefined : slugOrId(a)),
  "conversations.get": (a: unknown) => id(a, "conversation"),
  "conversations.create": (a: unknown) => {
    const o = obj(a, "conversation");
    onlyKeys(o, ["agent", "message", "attachments"], "conversation");
    return { agent: slugOrId(o.agent), message: str(o.message, "message", { max: 100_000 }), attachments: attachments(o.attachments) };
  },
  "conversations.send": (a: unknown) => {
    const o = obj(a, "message");
    onlyKeys(o, ["conversation", "content", "attachments"], "message");
    return { conversation: id(o.conversation, "conversation"), content: str(o.content, "content", { max: 100_000 }), attachments: attachments(o.attachments) };
  },
  "files.attach": (a: unknown) => slugOrId(a),
  "models.list": (_: unknown) => undefined,
  "models.available": (a: unknown) => str(a, "provider", { max: 16, re: /^(lyzr|anthropic|openai|ollama)$/ }),
  "models.setKey": (a: unknown) => {
    const o = obj(a, "key");
    onlyKeys(o, ["provider", "apiKey"], "key");
    return { provider: str(o.provider, "provider", { max: 16, re: /^(lyzr|anthropic|openai)$/ }), apiKey: str(o.apiKey, "API key", { min: 8, max: 400, re: /^\S+$/ }) };
  },
  "models.removeKey": (a: unknown) => str(a, "provider", { max: 16, re: /^(lyzr|anthropic|openai)$/ }),
  "settings.defaultModel": (_: unknown) => undefined,
  "settings.setDefaultModel": (a: unknown) => model(a),
  "connectors.list": (_: unknown) => undefined,
  "connectors.connect": (a: unknown) => str(a, "connector", { max: 32, re: /^(google|slack|microsoft)$/ }),
  "connectors.disconnect": (a: unknown) => str(a, "connector", { max: 32, re: /^(google|slack|microsoft)$/ }),
  "connectors.setup": (a: unknown) => {
    const o = obj(a, "setup");
    onlyKeys(o, ["id", "clientId", "clientSecret", "tenant"], "setup");
    const id = str(o.id, "connector", { max: 32, re: /^(google|slack|microsoft)$/ });
    const guid = /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/;
    const clientRe = { google: /^[0-9]+-[a-z0-9]+\.apps\.googleusercontent\.com$/, slack: /^[0-9]+\.[0-9]+$/, microsoft: guid }[id as "google"];
    return {
      id,
      clientId: str(o.clientId, "client ID", { max: 200, re: clientRe }),
      clientSecret: str(o.clientSecret, "client secret", { min: 10, max: 200, re: /^\S+$/ }),
      tenant: id === "microsoft" ? str(o.tenant, "tenant", { max: 200, re: /^([0-9a-fA-F-]{36}|organizations|[a-z0-9-]+(\.[a-z0-9-]+)+)$/ }) : undefined,
    };
  },
  "connectors.setupBot": (a: unknown) => {
    const o = obj(a, "bot");
    onlyKeys(o, ["botToken", "appToken"], "bot");
    return { botToken: str(o.botToken, "bot token", { max: 300, re: /^xoxb-[A-Za-z0-9-]+$/ }), appToken: str(o.appToken, "app token", { max: 300, re: /^xapp-[A-Za-z0-9-]+$/ }) };
  },
  // A Slack user token the person copied from their Slack app.
  "connectors.pasteToken": (a: unknown) => str(a, "token", { min: 15, max: 260, re: /^xoxp-[A-Za-z0-9-]+$/ }),
  "routines.list": (a: unknown) => slugOrId(a),
  "routines.create": (a: unknown) => {
    const o = obj(a, "routine");
    onlyKeys(o, ["agent", "name", "prompt", "cron", "timezone"], "routine");
    return {
      agent: slugOrId(o.agent),
      name: str(o.name, "name", { max: 80, re: /^[^\r\n\t]+$/ }),
      prompt: str(o.prompt, "prompt", { max: 10_000 }),
      cron: str(o.cron, "cron", { max: 100, re: /^[0-9*,\/-]+( [0-9*,\/-]+){4}$/ }),
      timezone: str(o.timezone, "timezone", { max: 64, re: /^[A-Za-z_]+(\/[A-Za-z0-9_+-]+)*$/ }),
    };
  },
  "routines.setPaused": (a: unknown) => {
    const o = obj(a, "args");
    onlyKeys(o, ["routine", "paused"], "args");
    if (typeof o.paused !== "boolean") throw new ValidationError("paused must be a boolean");
    return { routine: id(o.routine, "routine"), paused: o.paused };
  },
  "routines.remove": (a: unknown) => id(a, "routine"),
  "routines.runNow": (a: unknown) => id(a, "routine"),
  "localServer.status": (_: unknown) => undefined,
  "localServer.start": (_: unknown) => undefined,
  "localServer.stop": (_: unknown) => undefined,
  "events.subscribeTask": (a: unknown) => id(a, "task"),
  "events.subscribeWorkspace": (_: unknown) => undefined,
  "events.unsubscribe": (a: unknown) => str(a, "subscription", { max: 40, re: /^sub_[0-9]+$/ }),
  "shell.openExternal": (a: unknown) => externalURL(a),
} as const;

export type Operation = keyof typeof validators;
export const operations = Object.keys(validators) as Operation[];

export function isOperation(op: unknown): op is Operation {
  return typeof op === "string" && Object.prototype.hasOwnProperty.call(validators, op);
}

/** Message pushed from main to the renderer for event subscriptions. */
export interface PushedEvent {
  subscription: string;
  kind: "event" | "end" | "error" | "status" | "open";
  data: unknown;
}

export const INVOKE_CHANNEL = "lux:invoke";
export const EVENT_CHANNEL = "lux:event";
