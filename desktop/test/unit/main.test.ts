import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, readFileSync, statSync, writeFileSync, existsSync, mkdirSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, normalize } from "node:path";
import { CONTENT_SECURITY_POLICY, allowPermission, isAppURL, resolveAppPath, webPreferences } from "../../src/main/security";
import { ProfileStore, type SecretBox } from "../../src/main/profiles";
import { Handlers } from "../../src/main/handlers";
import { SSEParser, request, ApiError } from "../../src/main/api";
import { matches } from "../../src/renderer/components/CommandPalette";
import { foldEvents } from "../../src/renderer/components/Timeline";

// ---- security settings ----

test("windows use secure defaults", () => {
  assert.equal(webPreferences.contextIsolation, true);
  assert.equal(webPreferences.nodeIntegration, false);
  assert.equal(webPreferences.sandbox, true);
  assert.equal(webPreferences.webSecurity, true);
  assert.equal(webPreferences.webviewTag, false);
  assert.equal(webPreferences.allowRunningInsecureContent, false);
});

test("CSP gives the renderer no network and no inline scripts", () => {
  assert.match(CONTENT_SECURITY_POLICY, /default-src 'none'/);
  assert.match(CONTENT_SECURITY_POLICY, /connect-src 'none'/);
  assert.match(CONTENT_SECURITY_POLICY, /script-src 'self'(;|$)/);
  assert.doesNotMatch(CONTENT_SECURITY_POLICY, /unsafe-eval/);
  assert.doesNotMatch(CONTENT_SECURITY_POLICY, /script-src[^;]*unsafe-inline/);
});

test("permissions are all denied", () => {
  for (const p of ["media", "geolocation", "notifications", "clipboard-read", "openExternal", "fullscreen"]) assert.equal(allowPermission(p), false);
});

test("only app:// pages are trusted, and paths cannot escape", () => {
  assert.equal(isAppURL("app://lux/index.html"), true);
  for (const u of ["https://lux/index.html", "app://evil/index.html", "file:///index.html", "javascript:alert(1)", "garbage"]) assert.equal(isAppURL(u), false, u);
  const root = "/opt/lux/renderer";
  assert.equal(resolveAppPath("app://lux/", root, join, normalize), "/opt/lux/renderer/index.html");
  assert.equal(resolveAppPath("app://lux/assets/a.js", root, join, normalize), "/opt/lux/renderer/assets/a.js");
  // Whatever the input, the result is null or a file inside the renderer folder.
  for (const u of ["app://lux/../../etc/passwd", "app://lux/%2e%2e/%2e%2e/etc/passwd", "app://lux/assets/..%2f..%2f..%2fetc%2fpasswd",
    "app://lux/..%5c..%5cetc%5cpasswd", "app://lux/%252e%252e/etc/passwd", "app://lux/a%00b", "app://other/index.html"]) {
    const r = resolveAppPath(u, root, join, normalize);
    assert.ok(r === null || r.startsWith(root + "/"), `${u} resolved to ${r}`);
  }
  assert.equal(resolveAppPath("app://lux/assets/..%2f..%2f..%2fetc%2fpasswd", root, join, normalize), null);
  assert.equal(resolveAppPath("app://lux/a%00b", root, join, normalize), null);
  assert.equal(resolveAppPath("app://other/index.html", root, join, normalize), null);
});

// ---- profiles and credentials ----

function box(secure: boolean): SecretBox {
  return {
    secure: () => secure,
    backend: () => (secure ? "gnome_libsecret" : "basic_text"),
    encrypt: (s) => Buffer.from("ENC:" + Buffer.from(s).toString("hex")),
    decrypt: (b) => Buffer.from(b.toString().slice(4), "hex").toString(),
  };
}

function dirs() {
  const configDir = mkdtempSync(join(tmpdir(), "lux-cfg-"));
  const userDataDir = mkdtempSync(join(tmpdir(), "lux-ud-"));
  writeFileSync(join(configDir, "config.yaml"), "current: local\nenvironments:\n  local:\n    url: http://127.0.0.1:7878\n    local: true\nlocal_server:\n  database_url: postgres://x\n");
  writeFileSync(join(configDir, "credentials.yaml"), "tokens:\n  local: lux_pat_fromcli\n", { mode: 0o600 });
  return { configDir, userDataDir };
}

test("profiles come from the shared CLI config, including its tokens", () => {
  const d = dirs();
  const ps = new ProfileStore({ ...d, box: box(true) });
  const st = ps.state();
  assert.equal(st.current, "local");
  assert.deepEqual(st.profiles.map((p) => [p.name, p.signedIn, p.tokenStorage, p.local]), [["local", true, "cli", true]]);
  assert.equal(ps.connection().token, "lux_pat_fromcli");
});

test("tokens are encrypted at rest when a secure store exists", () => {
  const d = dirs();
  const ps = new ProfileStore({ ...d, box: box(true) });
  ps.add("work", "https://lux.example.com");
  assert.equal(ps.saveToken("work", "lux_pat_secret999"), "encrypted");
  const file = readFileSync(join(d.userDataDir, "tokens.json"), "utf8");
  assert.doesNotMatch(file, /lux_pat_secret999/, "plaintext token on disk");
  assert.equal(statSync(join(d.userDataDir, "tokens.json")).mode & 0o777, 0o600);
  assert.equal(ps.connection("work").token, "lux_pat_secret999");
  ps.signOut("work");
  assert.equal(ps.connection("work").token, null);
});

test("without a secure store, tokens stay in memory only", () => {
  const d = dirs();
  const ps = new ProfileStore({ ...d, box: box(false) });
  ps.add("work", "https://lux.example.com");
  assert.equal(ps.saveToken("work", "lux_pat_secret999"), "session");
  assert.equal(existsSync(join(d.userDataDir, "tokens.json")), false, "nothing written to disk");
  assert.equal(ps.state().profiles.find((p) => p.name === "work")!.tokenStorage, "session");
  // A new app session (new store) has forgotten it.
  assert.equal(new ProfileStore({ ...d, box: box(false) }).connection("work").token, null);
});

test("config edits keep other settings, use mode 0600, and protect the current profile", () => {
  const d = dirs();
  const ps = new ProfileStore({ ...d, box: box(true) });
  ps.add("work", "https://lux.example.com");
  ps.select("work");
  const text = readFileSync(join(d.configDir, "config.yaml"), "utf8");
  assert.match(text, /database_url: postgres:\/\/x/);
  assert.match(text, /current: work/);
  assert.equal(statSync(join(d.configDir, "config.yaml")).mode & 0o777, 0o600);
  assert.throws(() => ps.remove("work"), /switch to another profile/);
  assert.throws(() => ps.add("work", "https://x"), /already exists/);
  assert.throws(() => ps.select("nope"), /no profile/);
  // Signing out of a CLI-managed profile does not touch the CLI's file.
  ps.signOut("local");
  assert.match(readFileSync(join(d.configDir, "credentials.yaml"), "utf8"), /lux_pat_fromcli/);
});

test("a fresh install has a local profile", () => {
  const configDir = mkdtempSync(join(tmpdir(), "lux-empty-"));
  mkdirSync(configDir, { recursive: true });
  const ps = new ProfileStore({ configDir, userDataDir: mkdtempSync(join(tmpdir(), "ud-")), box: box(true) });
  assert.deepEqual(ps.state().profiles.map((p) => [p.name, p.url, p.signedIn]), [["local", "http://127.0.0.1:7878", false]]);
});

// ---- handlers ----

function fakeFetch(routes: Record<string, (init: any) => Response>) {
  const calls: { url: string; init: any }[] = [];
  const f = (async (url: string, init: any) => {
    calls.push({ url, init });
    const path = new URL(url).pathname + new URL(url).search;
    const h = routes[`${init?.method ?? "GET"} ${path}`];
    if (!h) return new Response(JSON.stringify({ error: { code: "not_found", message: "no route " + path } }), { status: 404 });
    return h(init);
  }) as unknown as typeof fetch;
  return { f, calls };
}

function handlers(routes: Record<string, (init: any) => Response>, secure = true) {
  const d = dirs();
  const profiles = new ProfileStore({ ...d, box: box(secure) });
  const { f, calls } = fakeFetch(routes);
  const pushed: any[] = [];
  const saved: [string, Uint8Array][] = [];
  const notified: [number, unknown][] = [];
  const h = new Handlers({
    profiles,
    appInfo: () => ({ version: "t", platform: "linux", secureStorage: secure, storageBackend: "x" }),
    openExternal: async () => {},
    push: (owner, msg) => pushed.push([owner, msg]),
    luxCLI: () => null,
    saveFile: async (name, bytes) => { saved.push([name, bytes]); return true; },
    pickFiles: async () => [{ name: "notes.txt", bytes: new TextEncoder().encode("hello") }],
    notify: (owner, n) => notified.push([owner, n]),
    fetchImpl: f,
  });
  return { h, calls, profiles, pushed, saved, notified };
}

const json = (v: unknown, status = 200) => new Response(JSON.stringify(v), { status, headers: { "content-type": "application/json" } });

test("invalid input never reaches the server", async () => {
  const { h, calls } = handlers({});
  for (const [op, args] of [["tasks.get", "../../v1/me"], ["agents.get", "a/b"], ["nonsense", {}], ["approvals.decide", { approval: "apr_1234", decision: "approve", x: 1 }]] as const) {
    const r = await h.invoke(op, args, 1);
    assert.equal(r.ok, false, op);
  }
  assert.equal(calls.length, 0);
});

test("requests carry the token to the server and use the workspace path", async () => {
  const { h, calls } = handlers({ "GET /v1/w/default/agents": () => json({ agents: [{ id: "agt_1" }] }) });
  const r = await h.invoke("agents.list", undefined, 1);
  assert.deepEqual(r, { ok: true, data: [{ id: "agt_1" }] });
  assert.equal(calls[0].init.headers.authorization, "Bearer lux_pat_fromcli");
  assert.equal(calls[0].init.redirect, "error", "never follow redirects with a token");
});

test("API errors come back as results, not exceptions", async () => {
  const { h } = handlers({ "POST /v1/w/default/approvals/apr_1234/decision": () => json({ error: { code: "maker_checker", message: "you started this task" } }, 403) });
  const r = await h.invoke("approvals.decide", { approval: "apr_1234", decision: "approve" }, 1);
  assert.deepEqual(r, { ok: false, error: { code: "maker_checker", message: "you started this task", status: 403, details: undefined } });
});

test("sign-in verifies the token before storing it", async () => {
  const { h, profiles } = handlers({ "GET /v1/me": (init) => (init.headers.authorization === "Bearer lux_pat_good" ? json({ username: "amogh" }) : json({ error: { code: "unauthenticated", message: "no" } }, 401)) });
  profiles.add("work", "https://lux.example.com");
  let r = await h.invoke("profiles.signIn", { profile: "work", token: "lux_pat_bad" }, 1);
  assert.equal(r.ok, false);
  assert.equal(profiles.connection("work").token, null, "rejected token must not be stored");
  r = await h.invoke("profiles.signIn", { profile: "work", token: "lux_pat_good" }, 1);
  assert.equal(r.ok, true);
  assert.equal(profiles.connection("work").token, "lux_pat_good");
});

test("an unreachable server is reported clearly", async () => {
  const d = dirs();
  const h = new Handlers({
    profiles: new ProfileStore({ ...d, box: box(true) }),
    appInfo: () => ({ version: "t", platform: "linux", secureStorage: true, storageBackend: "x" }),
    openExternal: async () => {}, push: () => {}, luxCLI: () => null, saveFile: async () => false, pickFiles: async () => [], notify: () => {},
    fetchImpl: (async () => { throw Object.assign(new TypeError("fetch failed"), { cause: { code: "ECONNREFUSED" } }); }) as any,
  });
  const r = await h.invoke("server.status", undefined, 1);
  assert.equal(r.ok, false);
  if (!r.ok) {
    assert.equal(r.error.code, "unreachable");
    assert.match(r.error.message, /ECONNREFUSED/);
  }
});

test("local server controls explain when the CLI is missing", async () => {
  const { h } = handlers({});
  const r = await h.invoke("localServer.start", undefined, 1);
  assert.equal(r.ok, true);
  if (r.ok) assert.equal((r.data as any).available, false);
});

test("a renderer cannot cancel another renderer's subscription", async () => {
  let release!: () => void;
  const hang = new Promise<void>((r) => (release = r));
  const { h } = handlers({
    "GET /v1/w/default/events/stream": () => new Response(new ReadableStream({ async start(c) { c.enqueue(new TextEncoder().encode("event: ready\ndata: {}\n\n")); await hang; c.close(); } }), { headers: { "content-type": "text/event-stream" } }),
  });
  const sub = await h.invoke("events.subscribeWorkspace", undefined, 7);
  assert.equal(sub.ok, true);
  const id = (sub as any).data as string;
  await h.invoke("events.unsubscribe", id, 99); // someone else
  assert.equal((h as any).subs.has(id), true);
  await h.invoke("events.unsubscribe", id, 7); // the owner
  assert.equal((h as any).subs.has(id), false);
  release();
});

// ---- SSE ----

test("SSE parser handles split chunks, comments, CRLF and multi-line data", () => {
  const p = new SSEParser();
  assert.deepEqual(p.push("id: 3\nevent: task_ev"), []);
  assert.deepEqual(p.push('ent\ndata: {"a":\ndata: 1}\n\n: keep-alive\n\n'), [{ event: "task_event", id: "3", data: '{"a":\n1}' }]);
  assert.deepEqual(p.push("event: end\r\ndata: {}\r\n\r\n"), [{ event: "end", id: "3", data: "{}" }]);
});

test("request() rejects non-JSON success bodies", async () => {
  await assert.rejects(request({ url: "http://x", token: null, workspace: "default" }, "GET", "/v1/status", undefined, (async () => new Response("<html>")) as any), ApiError);
});

// ---- renderer logic ----

test("command palette matching", () => {
  assert.equal(matches("Go to Approvals", "appr"), true);
  assert.equal(matches("Switch to work (https://lux.example.com)", "work switch"), true);
  assert.equal(matches("Go to Tasks", "agents"), false);
  assert.equal(matches("anything", "  "), true);
});

test("timeline folds streamed text and pairs tool calls with results", () => {
  const ev = (seq: number, type: string, payload: any) => ({ seq, type, payload, created_at: "" });
  const items = foldEvents([
    ev(1, "task.state", { state: "queued" }),
    ev(2, "task.state", { state: "running" }),
    ev(3, "assistant.delta", { text: "Look" }),
    ev(4, "assistant.delta", { text: "ing." }),
    ev(5, "tool.call", { id: "c1", name: "cli", args: { command: "ls" } }),
    ev(6, "lux.approval_requested", { approval_id: "apr_1", summary: "cli: ls" }),
    ev(7, "tool.result", { id: "c1", name: "cli", content: "a.txt", is_error: false }),
    ev(8, "assistant.delta", { text: "Done" }),
    ev(9, "task.state", { state: "succeeded" }),
  ]);
  assert.deepEqual(items.map((i) => i.kind), ["state", "text", "tool", "approval", "text", "state"]);
  assert.equal(items[1].text, "Looking.");
  assert.deepEqual(items[2].tool, { name: "cli", arg: "ls", result: "a.txt", error: false });
});

// Regression guard: an effect written as `useEffect(() => expr)` returns
// expr to React as its cleanup. Browser APIs such as scrollIntoView() now
// return Promises, which crashed the app on unmount. Effects use block bodies.
test("no React effect returns an expression", async () => {
  const { readdirSync, readFileSync, statSync } = await import("node:fs");
  const walk = (d: string): string[] => readdirSync(d).flatMap((f) => (statSync(join(d, f)).isDirectory() ? walk(join(d, f)) : [join(d, f)]));
  const offenders = walk("src/renderer").filter((f) => /\.tsx?$/.test(f)).filter((f) => /useEffect\(\(\)\s*=>\s*[^{\s]/.test(readFileSync(f, "utf8")));
  assert.deepEqual(offenders, []);
});

test("timeline shows recorded and published changes", () => {
  const items = foldEvents([
    { seq: 1, type: "lux.changes", payload: { target: "repository", files: 1, added: 2, deleted: 0, reviewable: true } },
    { seq: 2, type: "lux.changes", payload: { target: "memory", files: 3, added: 1, deleted: 1, reviewable: false } },
    { seq: 3, type: "lux.changes_published", payload: { target: "repository", branch: "lux/task_1", by: "sam" } },
    { seq: 4, type: "lux.changes_error", payload: { target: "memory", message: "Lux could not record the changes: boom" } },
  ] as any);
  assert.deepEqual(items.map((i) => [i.kind, i.text]), [
    ["state", "Changed the repository: 1 file, +2 −0"],
    ["state", "Changed its memory: 3 files, +1 −1 (too large to review here)"],
    ["state", "sam published the repository changes to lux/task_1"],
    ["alert", "Lux could not record the changes: boom"],
  ]);
});

test("replies render as text: no raw HTML, no script links", async () => {
  const { createElement } = await import("react");
  const { renderToStaticMarkup } = await import("react-dom/server");
  const { RichText } = await import("../../src/renderer/components/RichText");
  const html = renderToStaticMarkup(createElement(RichText, { text: "**Done**\n\n- one\n- two\n\n<script>alert(1)</script><img src=x onerror=alert(1)>\n\n[click](javascript:alert(1)) ![pic](https://example.com/a.png)" }));
  assert.match(html, /<strong>Done<\/strong>/);
  assert.match(html, /<li>one<\/li>/);
  assert.doesNotMatch(html, /<script|onerror|<img/i);
  assert.doesNotMatch(html, /javascript:/i);
  assert.match(html, />pic<\/a>/); // web images become links, nothing remote loads
});

test("each conversation keeps the same mascot colour", async () => {
  const { mascotIndex } = await import("../../src/renderer/components/Mascot");
  assert.equal(mascotIndex("conv_abc123"), mascotIndex("conv_abc123"));
  const seen = new Set(["conv_1", "conv_2", "conv_3", "conv_4", "conv_5", "conv_6", "conv_7", "conv_8"].map(mascotIndex));
  assert.ok(seen.size >= 4, "ids should spread across colours");
  for (const i of seen) assert.ok(i >= 0 && i < 9);
});

test("sidebar times read like a messaging app", async () => {
  const { shortTime } = await import("../../src/renderer/lib/lux");
  const now = new Date(2026, 9, 9, 21, 0);
  assert.match(shortTime(new Date(2026, 9, 9, 8, 41).toISOString(), now), /8:41/);
  assert.equal(shortTime(new Date(2026, 9, 8, 23, 0).toISOString(), now), "Yesterday");
  assert.equal(shortTime(new Date(2026, 9, 6, 10, 0).toISOString(), now), new Date(2026, 9, 6).toLocaleDateString([], { weekday: "long" }));
  assert.doesNotMatch(shortTime(new Date(2026, 7, 1).toISOString(), now), /:/);
});

test("workspace files: text and images preview, anything else only saves", async () => {
  const base = "GET /v1/w/default/agents/chief/files/content?path=";
  const { h, saved, profiles } = handlers({
    [base + "plan.md"]: () => new Response("# Plan", { headers: { "content-type": "text/plain; charset=utf-8" } }),
    [base + "chart.png"]: () => new Response(new Uint8Array([137, 80, 78, 71]), { headers: { "content-type": "image/png" } }),
    [base + "data.bin"]: () => new Response(new Uint8Array([0, 1, 2]), { headers: { "content-type": "application/octet-stream" } }),
    [base + "gone.md"]: () => new Response(JSON.stringify({ error: { code: "not_found", message: "file not found" } }), { status: 404 }),
  });
  profiles.add("t", "http://127.0.0.1:9");
  profiles.select("t");
  profiles.saveToken("t", "lux_pat_x");
  const call = (op: string, args: unknown) => h.invoke(op, args, 1);
  assert.deepEqual(await call("files.preview", { agent: "chief", path: "plan.md" }), { ok: true, data: { kind: "text", text: "# Plan" } });
  const img = (await call("files.preview", { agent: "chief", path: "chart.png" })) as any;
  assert.equal(img.data.kind, "image");
  assert.match(img.data.dataUrl, /^data:image\/png;base64,/);
  assert.deepEqual(await call("files.preview", { agent: "chief", path: "data.bin" }), { ok: true, data: { kind: "none", size: 3 } });
  assert.deepEqual(await call("files.save", { agent: "chief", path: "data.bin" }), { ok: true, data: { saved: true } });
  assert.equal(saved[0][0], "data.bin");
  assert.deepEqual([...saved[0][1]], [0, 1, 2]);
  const missing = (await call("files.preview", { agent: "chief", path: "gone.md" })) as any;
  assert.equal(missing.error.code, "not_found");
  for (const p of ["../secret", "/etc/passwd", "a/../../b", "a\\b", "./x", ""]) {
    const r = (await call("files.preview", { agent: "chief", path: p })) as any;
    assert.equal(r.error?.code, "invalid_input", p);
  }
});

test("agent activity reads as plain steps and hides bookkeeping", async () => {
  const { stepsFrom } = await import("../../src/renderer/components/Activity");
  const steps = stepsFrom([
    { seq: 1, type: "tool.call", payload: { id: "1", name: "task_tracker", args: {} } },
    { seq: 2, type: "tool.call", payload: { id: "2", name: "read", args: { path: "/runs/x/agent/workspace/planner.md" } } },
    { seq: 3, type: "tool.result", payload: { id: "2" } },
    { seq: 4, type: "lux.approval_requested", payload: { approval_id: "apr_1", summary: "cli: ls -la" } },
    { seq: 5, type: "lux.approval_decided", payload: { approval_id: "apr_1", decision: "deny" } },
    { seq: 6, type: "tool.call", payload: { id: "3", name: "edit", args: { path: "workspace/planner.md" } } },
  ] as any);
  assert.deepEqual(steps.map((s) => [s.text, s.state]), [
    ["Read planner.md", "done"],
    ["Not allowed to run ls -la", "failed"],
    ["Editing planner.md", "doing"],
  ]);
});

test("attaching uploads the chosen files and returns where they went", async () => {
  const { h, profiles, calls } = handlers({
    "POST /v1/w/default/agents/chief/uploads?name=notes.txt": () => json({ path: "uploads/notes.txt", name: "notes.txt", size: 5 }, 201),
  });
  profiles.add("t", "http://127.0.0.1:9");
  profiles.select("t");
  profiles.saveToken("t", "lux_pat_x");
  assert.deepEqual(await h.invoke("files.attach", "chief", 1), { ok: true, data: [{ path: "uploads/notes.txt", name: "notes.txt", size: 5 }] });
  assert.equal(new TextDecoder().decode(calls.at(-1)!.init.body), "hello");
  for (const bad of [["uploads/../SOUL.md"], ["SOUL.md"], Array(11).fill("uploads/a")]) {
    const r = (await h.invoke("conversations.send", { conversation: "conv_abcd1234", content: "x", attachments: bad }, 1)) as any;
    assert.equal(r.error?.code, "invalid_input", JSON.stringify(bad).slice(0, 40));
  }
});

test("routine schedules read in plain words", async () => {
  const { cronFor, describeCron } = await import("../../src/renderer/components/Routines");
  assert.equal(cronFor("weekdays", "08:05"), "5 8 * * 1-5");
  assert.equal(cronFor("daily", "21:30"), "30 21 * * *");
  assert.equal(cronFor(0, "09:00"), "0 9 * * 0");
  assert.equal(describeCron("CRON_TZ=Asia/Kolkata 5 8 * * 1-5"), "Weekdays at 08:05 (Asia/Kolkata)");
  assert.equal(describeCron("0 9 * * 0"), "Every Sunday at 09:00");
  assert.equal(describeCron("*/15 * * * *"), "*/15 * * * *");
});

test("an agent's choices block becomes answer buttons and leaves the text", async () => {
  const { splitChoices } = await import("../../src/renderer/views/ChatView");
  const reply = "Which format do you want?\n\n```choices\n- Weekly\n2. Daily\nMonthly\nWeekly\n```\n";
  assert.deepEqual(splitChoices(reply), { text: "Which format do you want?", choices: ["Weekly", "Daily", "Monthly"] });
  // Only a block at the very end counts; other code blocks are left alone.
  const mid = "```choices\nA\n```\nThen more text";
  assert.deepEqual(splitChoices(mid), { text: mid, choices: [] });
  assert.equal(splitChoices("```choices\n1\n2\n3\n4\n5\n6\n```").choices.length, 5);
});

test("notifications go through the main process, plain text and short", async () => {
  const { h, notified } = handlers({});
  assert.equal((await h.invoke("app.notify", { title: "Trip Planner needs you", body: "Wants to run ls", agent: "agt_1" }, 7)).ok, true);
  assert.deepEqual(notified, [[7, { title: "Trip Planner needs you", body: "Wants to run ls", agent: "agt_1" }]]);
  assert.equal((await h.invoke("app.notify", { title: "two\nlines", body: "" }, 7)).ok, false);
  assert.equal((await h.invoke("app.notify", { title: "x", body: "y".repeat(301) }, 7)).ok, false);
  assert.equal((await h.invoke("app.notify", { title: "x", body: "", url: "https://evil.example" }, 7)).ok, false);
});

test("notification text is plain and short", async () => {
  const { approvalText, replyText } = await import("../../src/renderer/lib/notify");
  assert.equal(approvalText("cli: ls workspace"), "Wants to run ls workspace");
  assert.equal(approvalText("lux__gmail_send: to=a@b.c"), "Wants to gmail_send: to=a@b.c");
  assert.equal(replyText("**Done** — saved `plan.md`\n\n```choices\nA\n```"), "Done — saved plan.md");
  assert.ok(replyText("x ".repeat(200)).length <= 160);
});

test("the agent's own tools read as plain words, and a handoff asks for the person", async () => {
  const { stepsFrom } = await import("../../src/renderer/components/Activity");
  const ev = (seq: number, type: string, payload: any) => ({ seq, type, payload, task_id: "t", created_at: "" }) as any;
  const steps = stepsFrom([
    ev(1, "tool.call", { id: "1", name: "lux__web_search", args: { query: "goa weather" } }),
    ev(2, "tool.result", { id: "1" }),
    ev(3, "tool.call", { id: "2", name: "lux__browser_open", args: { url: "https://www.booking.com/x" } }),
    ev(4, "lux.approval_requested", { approval_id: "apr_1", summary: "lux__browser_handoff: Sign in to booking.com" }),
  ]);
  assert.equal(steps[0].text, "Searched the web for “goa weather”");
  assert.equal(steps[1].text, "Opening www.booking.com");
  assert.equal(steps[2].text, "Needs you in its browser: Sign in to booking.com");
  assert.equal(steps[2].handoff, "Sign in to booking.com");
  const done = stepsFrom([ev(4, "lux.approval_requested", { approval_id: "apr_1", summary: "lux__browser_handoff: Sign in" }), ev(5, "lux.approval_decided", { approval_id: "apr_1", decision: "allow" })]);
  assert.equal(done[0].text, "You did: Sign in");
});

test("browser input is checked before it leaves the app", async () => {
  const { validators } = await import("../../src/shared/ipc");
  const v = validators["agents.browserInput"];
  assert.deepEqual(JSON.parse(JSON.stringify(v({ agent: "agt_1", type: "click", x: 10, y: 20 }))), { agent: "agt_1", type: "click", x: 10, y: 20 });
  assert.throws(() => v({ agent: "agt_1", type: "eval", x: 1 }));
  assert.throws(() => v({ agent: "agt_1", type: "open", url: "file:///etc/passwd" }));
  assert.throws(() => v({ agent: "agt_1", type: "key", key: "F12" }));
  assert.throws(() => v({ agent: "agt_1", type: "click", x: -5, y: 1 }));
});
