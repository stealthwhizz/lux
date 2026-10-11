import { test } from "node:test";
import assert from "node:assert/strict";
import { operations, validators, serverURL, externalURL, isOperation, ValidationError } from "../../src/shared/ipc";
import { buildBridge } from "../../src/preload/bridge";

const bad = (fn: () => unknown) => assert.throws(fn, ValidationError);

test("only listed operations exist", () => {
  assert.equal(isOperation("agents.list"), true);
  for (const op of ["eval", "__proto__", "constructor", "toString", "fs.readFile", "", 42, null]) {
    assert.equal(isOperation(op), false, String(op));
  }
});

test("server URLs: http(s) only, no credentials, query or fragment", () => {
  assert.equal(serverURL("https://lux.example.com/"), "https://lux.example.com");
  assert.equal(serverURL("http://127.0.0.1:7878"), "http://127.0.0.1:7878");
  assert.equal(serverURL("https://example.com/lux/"), "https://example.com/lux");
  for (const u of ["javascript:alert(1)", "file:///etc/passwd", "ftp://x", "https://user:pw@x.com", "https://x.com/?a=1", "https://x.com/#f", "not a url", ""]) {
    bad(() => serverURL(u));
  }
});

test("external links: https only", () => {
  assert.equal(externalURL("https://docs.example.com/a"), "https://docs.example.com/a");
  for (const u of ["http://x.com", "javascript:alert(1)", "file:///etc/passwd", "lux://x", "mailto:a@b.c"]) {
    assert.throws(() => externalURL(u));
  }
});

test("tokens must look like Lux personal tokens", () => {
  const v = validators["profiles.signIn"];
  assert.deepEqual(v({ profile: "local", token: "lux_pat_abc123" }), { profile: "local", token: "lux_pat_abc123" });
  bad(() => v({ profile: "local", token: "lux_run_abc" })); // run tokens are not user credentials
  bad(() => v({ profile: "local", token: "lux_pat_ABC; rm -rf /" }));
  bad(() => v({ profile: "../etc", token: "lux_pat_abc" }));
  bad(() => v({ profile: "local", token: "lux_pat_abc", extra: true }));
});

test("ids, slugs and states are checked", () => {
  assert.equal(validators["tasks.get"]("task_0123abcd"), "task_0123abcd");
  for (const v of ["../../etc", "task_..", "TASK_x", "task_abc/def", "", 5]) bad(() => validators["tasks.get"](v));
  assert.equal(validators["agents.get"]("repo-helper"), "repo-helper");
  bad(() => validators["agents.get"]("Repo Helper"));
  bad(() => validators["agents.get"]("a/../b"));
  assert.deepEqual(validators["tasks.list"]({ state: "running,waiting_approval", limit: 20 }), { agent: undefined, state: "running,waiting_approval", limit: 20 });
  bad(() => validators["tasks.list"]({ state: "running,deleted" }));
  bad(() => validators["tasks.list"]({ limit: 10_000 }));
  bad(() => validators["tasks.list"]({ limit: 1.5 }));
});

test("structured inputs reject unknown fields and wrong types", () => {
  assert.deepEqual(validators["approvals.decide"]({ approval: "apr_1234", decision: "approve" }), { approval: "apr_1234", decision: "approve", note: "" });
  bad(() => validators["approvals.decide"]({ approval: "apr_1234", decision: "yes" }));
  bad(() => validators["approvals.decide"]({ approval: "apr_1234", decision: "approve", as_user: "admin" }));
  bad(() => validators["agents.setDisabled"]({ agent: "x", disabled: "true" }));
  bad(() => validators["agents.import"]({ sourceKind: "svn", sourceUrl: "x" }));
  bad(() => validators["agents.import"]({ sourceKind: "git", sourceUrl: "https://x", sourceRef: "--upload-pack=x" }));
  bad(() => validators["tasks.create"]({ agent: "a", prompt: "" }));
  bad(() => validators["tasks.create"]({ agent: "a", prompt: "x".repeat(100_001) }));
  bad(() => validators["tasks.create"](["a", "b"]));
  bad(() => validators["tasks.create"](null));
  bad(() => validators["events.unsubscribe"]("sub_1; drop"));
});

test("the bridge is frozen and calls only fixed operations", async () => {
  const calls: [string, unknown][] = [];
  const bridge = buildBridge(async (op, args) => {
    calls.push([op, args]);
    return { ok: true, data: null };
  }, () => () => {});
  assert.equal(Object.isFrozen(bridge), true);
  assert.equal(Object.isFrozen(bridge.tasks), true);
  assert.deepEqual(Object.keys(bridge).sort(), ["agents", "app", "approvals", "changes", "connectors", "conversations", "events", "files", "localServer", "models", "openExternal", "profiles", "repositories", "routines", "server", "settings", "tasks"]);
  assert.equal((bridge as any).invoke, undefined, "no generic invoke");
  await bridge.tasks.create("helper", "do it");
  await bridge.approvals.decide("apr_1234", "reject", "no");
  assert.deepEqual(calls, [
    ["tasks.create", { agent: "helper", prompt: "do it" }],
    ["approvals.decide", { approval: "apr_1234", decision: "reject", note: "no" }],
  ]);
  // Every bridge function maps to an operation the main process knows.
  const seen = new Set<string>();
  const probe = buildBridge(async (op) => {
    seen.add(op);
    return { ok: true, data: null };
  }, () => () => {});
  const walk = async (o: any) => {
    for (const v of Object.values(o)) {
      if (typeof v === "function" && v !== probe.events.onEvent) await (v as any)("x", "y");
      else if (typeof v === "object") await walk(v);
    }
  };
  await walk(probe);
  for (const op of seen) assert.ok(operations.includes(op as any), op);
});

test("change operations accept only known targets and plain titles", () => {
  assert.deepEqual(validators["changes.publish"]({ task: "task_abcd1234", target: "memory", pullRequest: true, title: "Tidy" }),
    { task: "task_abcd1234", target: "memory", pullRequest: true, title: "Tidy" });
  bad(() => validators["changes.publish"]({ task: "task_abcd1234", target: "elsewhere" }));
  bad(() => validators["changes.publish"]({ task: "task_abcd1234", target: "repository", title: "a\nb" }));
  bad(() => validators["changes.publish"]({ task: "task_abcd1234", target: "repository", pullRequest: "yes" }));
  bad(() => validators["changes.patch"]({ task: "../x", target: "repository" }));
  bad(() => validators["changes.queue"]("everything"));
  assert.equal(validators["tasks.create"]({ agent: "a", prompt: "p", repository: "widgets" }).repository, "widgets");
  bad(() => validators["tasks.create"]({ agent: "a", prompt: "p", repository: "../x" }));
});
