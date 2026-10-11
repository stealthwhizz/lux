import { test } from "node:test";
import assert from "node:assert/strict";
import { spawn, execFileSync } from "node:child_process";
import { mkdtempSync, writeFileSync, readFileSync, existsSync, mkdirSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { startStub, toolCallTurn, textTurn } from "./stub.mjs";
import { EventWriter, DeltaBuffer, clipArgs, codeExecutionFeatures, readSpec, writeExit, _resetExit, runtimeBlockPatch, promptWithHistory, repositoryNote, luxNote } from "../src/protocol.mjs";
import { checkToolCall, waitForApproval } from "../src/runner.mjs";

const RUNNER = fileURLToPath(new URL("../src/runner.mjs", import.meta.url));
const TOKEN = "lux_run_testtoken";

const MANIFEST = `spec_version: "0.1.0"
name: test-agent
version: 1.0.0
description: Runner test agent
model:
  preferred: anthropic:claude-sonnet-4-5
tools: [cli, read]
`;

function makeRun({ manifest = MANIFEST, prompt = "Say hello", apiUrl, maxTurns, allowCode = false } = {}) {
	const dir = mkdtempSync(join(tmpdir(), "lux-runner-"));
	const agent = join(dir, "agent");
	mkdirSync(agent);
	writeFileSync(join(agent, "agent.yaml"), manifest);
	writeFileSync(join(agent, "SOUL.md"), "# Soul\n\nYou are a test agent. Be brief.\n");
	const git = (...a) => execFileSync("git", a, { cwd: agent, env: { ...process.env, GIT_AUTHOR_NAME: "T", GIT_AUTHOR_EMAIL: "t@x.invalid", GIT_COMMITTER_NAME: "T", GIT_COMMITTER_EMAIL: "t@x.invalid" } });
	git("init", "-q", "-b", "main");
	git("add", "-A");
	git("commit", "-q", "-m", "init");
	for (const d of ["home", "tmp"]) mkdirSync(join(dir, d));
	const spec = {
		protocol: 1, run_id: "run_test", task_id: "task_test", attempt: 1, prompt, agent_dir: agent,
		events_path: join(dir, "events.jsonl"), exit_path: join(dir, "exit.json"),
		model: `lux:anthropic/claude-sonnet-4-5@${apiUrl}/gateway/v1`, api_url: apiUrl, timeout_seconds: 60,
		...(maxTurns ? { max_turns: maxTurns } : {}),
		...(allowCode ? { allow_code_execution: true } : {}),
	};
	writeFileSync(join(dir, "spec.json"), JSON.stringify(spec));
	return { dir, agent, spec };
}

// Start the runner with the clean environment luxd would give it.
function startRunner(run) {
	const child = spawn(process.execPath, [RUNNER, join(run.dir, "spec.json")], {
		cwd: run.dir,
		env: { PATH: process.env.PATH, HOME: join(run.dir, "home"), TMPDIR: join(run.dir, "tmp"), NO_COLOR: "1", LUX_RUN_TOKEN: TOKEN, OPENAI_API_KEY: TOKEN },
		stdio: ["ignore", "pipe", "pipe"],
	});
	let output = "";
	child.stdout.on("data", (d) => (output += d));
	child.stderr.on("data", (d) => (output += d));
	const done = new Promise((resolve) => child.on("exit", (code) => resolve({ code, output })));
	return { child, done };
}

function readRun(run) {
	const events = existsSync(run.spec.events_path)
		? readFileSync(run.spec.events_path, "utf8").trim().split("\n").filter(Boolean).map((l) => JSON.parse(l))
		: [];
	const exit = existsSync(run.spec.exit_path) ? JSON.parse(readFileSync(run.spec.exit_path, "utf8")) : null;
	return { events, exit, types: events.map((e) => e.type) };
}

async function runToEnd(run, timeoutMs = 30_000) {
	const { done } = startRunner(run);
	const res = await Promise.race([done, new Promise((_, rej) => setTimeout(() => rej(new Error("runner timed out")), timeoutMs))]);
	return { ...readRun(run), ...res };
}

test("a tool call that Lux allows runs, and the result is reported", async (t) => {
	const stub = await startStub({ turns: [toolCallTurn("cli", { command: "echo lux-marker-42" }), textTurn("Done: lux-marker-42")] });
	t.after(stub.close);
	const run = makeRun({ apiUrl: stub.url });
	const r = await runToEnd(run);
	assert.equal(r.exit?.reason, "completed", r.output);
	assert.equal(r.exit.summary, "Done: lux-marker-42");
	assert.equal(r.types[0], "run.started");
	assert.equal(r.events[0].data.runtime, "gitagent");
	assert.equal(r.events[0].data.model, "lux:anthropic/claude-sonnet-4-5", "the gateway URL is not reported as the model");
	const call = r.events.find((e) => e.type === "tool.call");
	assert.equal(call.data.name, "cli");
	assert.deepEqual(call.data.args, { command: "echo lux-marker-42" });
	const result = r.events.find((e) => e.type === "tool.result");
	assert.match(result.data.content, /lux-marker-42/);
	assert.equal(result.data.is_error, false);
	const final = r.events.filter((e) => e.type === "assistant.message").at(-1);
	assert.equal(final.data.content, "Done: lux-marker-42");
	assert.ok(r.types.includes("assistant.delta"), "streamed text is reported");
	// seq numbers are consecutive from 1.
	r.events.forEach((e, i) => assert.equal(e.seq, i + 1));

	// The model and the policy endpoint were called with the run token only.
	assert.ok(stub.state.modelRequests.length >= 2);
	for (const m of stub.state.modelRequests) {
		assert.equal(m.auth, `Bearer ${TOKEN}`);
		assert.equal(m.body.model, "anthropic/claude-sonnet-4-5");
	}
	assert.equal(stub.state.policyRequests.length, 1);
	assert.equal(stub.state.policyRequests[0].url, "/internal/v1/runs/run_test/tool-calls");
	assert.equal(stub.state.policyRequests[0].auth, `Bearer ${TOKEN}`);
	assert.deepEqual(stub.state.policyRequests[0].body, { tool: "cli", args: { command: "echo lux-marker-42" } });
});

test("a tool call that Lux denies does not run", async (t) => {
	const stub = await startStub({
		turns: [toolCallTurn("cli", { command: "touch ../denied-marker" }), textTurn("I could not do that.")],
		policy: () => ({ decision: "deny", reason: "needs approval" }),
	});
	t.after(stub.close);
	const run = makeRun({ apiUrl: stub.url });
	const r = await runToEnd(run);
	assert.equal(existsSync(join(run.dir, "denied-marker")), false, "denied command must not run");
	const blocked = r.events.find((e) => e.type === "tool.blocked");
	assert.deepEqual(blocked.data, { name: "cli", reason: "needs approval" });
	const result = r.events.find((e) => e.type === "tool.result");
	assert.equal(result.data.is_error, true);
	assert.equal(r.exit.reason, "completed");
});

test("when Lux cannot be reached, tool calls are blocked (fail closed)", async (t) => {
	const stub = await startStub({
		turns: [toolCallTurn("cli", { command: "touch ../unreachable-marker" }), textTurn("ok")],
		policy: () => ({ status: 500, error: "boom" }),
	});
	t.after(stub.close);
	const run = makeRun({ apiUrl: stub.url });
	const r = await runToEnd(run);
	assert.equal(existsSync(join(run.dir, "unreachable-marker")), false);
	assert.match(r.events.find((e) => e.type === "tool.blocked").data.reason, /HTTP 500/);
});

test("checkToolCall only allows an explicit allow", async () => {
	const spec = { api_url: "http://127.0.0.1:1", run_id: "run_x" };
	const answers = [
		[{ ok: true, json: async () => ({ decision: "allow" }) }, true],
		[{ ok: true, json: async () => ({ decision: "ALLOW" }) }, false],
		[{ ok: true, json: async () => ({}) }, false],
		[{ ok: true, json: async () => { throw new Error("bad json"); } }, false],
		[{ ok: false, status: 401, json: async () => ({ decision: "allow" }) }, false],
	];
	for (const [res, want] of answers) {
		const got = await checkToolCall(spec, "t", "cli", {}, async () => res);
		assert.equal(got.allow, want, JSON.stringify(res));
	}
	const unreachable = await checkToolCall(spec, "t", "cli", {});
	assert.equal(unreachable.allow, false);
	assert.match(unreachable.reason, /could not confirm/);
});

test("a model error ends the run with that error", async (t) => {
	const stub = await startStub({ modelStatus: 501 });
	t.after(stub.close);
	const r = await runToEnd(makeRun({ apiUrl: stub.url }));
	assert.equal(r.exit.reason, "error");
	assert.match(r.exit.error, /not implemented|501/i, r.exit.error);
});

test("SIGTERM cancels the run and still writes exit.json", async (t) => {
	const stub = await startStub({ modelHangs: true });
	t.after(stub.close);
	const run = makeRun({ apiUrl: stub.url });
	const { child, done } = startRunner(run);
	for (let i = 0; i < 100 && stub.state.modelRequests.length === 0; i++) await new Promise((r) => setTimeout(r, 50));
	assert.equal(stub.state.modelRequests.length, 1, "runner should be waiting on the model");
	const start = Date.now();
	child.kill("SIGTERM");
	await done;
	assert.ok(Date.now() - start < 6000, "runner should exit promptly after SIGTERM");
	assert.equal(readRun(run).exit.reason, "cancelled");
});

test("agents that run code at load are refused unless allowed", async (t) => {
	const stub = await startStub({ turns: [textTurn("hi")] });
	t.after(stub.close);
	const manifest = MANIFEST + "plugins:\n  audit:\n    enabled: true\n";
	const r = await runToEnd(makeRun({ apiUrl: stub.url, manifest }));
	assert.equal(r.exit.reason, "error");
	assert.match(r.exit.error, /runs code when it loads \(plugins\)/);
	assert.equal(stub.state.modelRequests.length, 0, "no model call before the check");
});

test("max_turns is enforced by the runner", async (t) => {
	const stub = await startStub({ turns: [toolCallTurn("cli", { command: "echo again" })] }); // always another tool call
	t.after(stub.close);
	const r = await runToEnd(makeRun({ apiUrl: stub.url, maxTurns: 2 }));
	assert.equal(r.exit.reason, "error");
	assert.match(r.exit.error, /turn limit \(2\)/);
	assert.ok(stub.state.modelRequests.length <= 3, `model called ${stub.state.modelRequests.length} times`);
});

test("missing run token is reported, not ignored", async () => {
	const run = makeRun({ apiUrl: "http://127.0.0.1:1" });
	const child = spawn(process.execPath, [RUNNER, join(run.dir, "spec.json")], { env: { PATH: process.env.PATH }, stdio: "ignore" });
	await new Promise((r) => child.on("exit", r));
	const { exit, types } = readRun(run);
	assert.equal(exit.reason, "error");
	assert.ok(types.includes("run.error"));
});

// ---- protocol helpers ----

test("DeltaBuffer coalesces and flushes", async () => {
	const dir = mkdtempSync(join(tmpdir(), "lux-delta-"));
	const w = new EventWriter(join(dir, "e.jsonl"));
	const b = new DeltaBuffer(w, "assistant.delta", { maxChars: 10, maxDelayMs: 20 });
	b.add("abc");
	b.add("def");
	assert.equal(w.seq, 0, "small deltas wait");
	b.add("ghijk"); // reaches 11 chars
	assert.equal(w.seq, 1);
	b.add("tail");
	await new Promise((r) => setTimeout(r, 50));
	assert.equal(w.seq, 2, "timer flushes the rest");
	const lines = readFileSync(join(dir, "e.jsonl"), "utf8").trim().split("\n").map((l) => JSON.parse(l));
	assert.deepEqual(lines.map((l) => l.data.text), ["abcdefghijk", "tail"]);
});

test("clipArgs bounds large arguments", () => {
	assert.deepEqual(clipArgs({ a: 1 }), { a: 1 });
	const big = clipArgs({ content: "x".repeat(50_000) });
	assert.ok(JSON.stringify(big).length < 9000);
	assert.deepEqual(clipArgs(undefined), {});
});

test("codeExecutionFeatures matches the Go validator", () => {
	const dir = mkdtempSync(join(tmpdir(), "lux-feat-"));
	assert.deepEqual(codeExecutionFeatures({}, dir), []);
	mkdirSync(join(dir, "hooks"));
	const found = codeExecutionFeatures({ extends: "https://x", plugins: { a: {} }, mcp_servers: { files: { command: "npx" }, remote: { url: "https://m" } } }, dir);
	assert.deepEqual(found, ["extends", "plugins", "mcp_servers.files", "hooks/"]);
});

test("readSpec rejects bad specs and writeExit writes once", () => {
	const dir = mkdtempSync(join(tmpdir(), "lux-spec-"));
	const p = join(dir, "spec.json");
	writeFileSync(p, JSON.stringify({ protocol: 2 }));
	assert.throws(() => readSpec(p), /protocol/);
	writeFileSync(p, JSON.stringify({ protocol: 1, run_id: "r" }));
	assert.throws(() => readSpec(p), /missing/);
	_resetExit();
	const ex = join(dir, "exit.json");
	assert.equal(writeExit(ex, { reason: "completed", summary: "first" }), true);
	assert.equal(writeExit(ex, { reason: "error" }), false);
	assert.equal(JSON.parse(readFileSync(ex, "utf8")).summary, "first");
	assert.equal(existsSync(ex + ".tmp"), false);
});

test("runtimeBlockPatch adds a runtime block only when needed", () => {
	assert.equal(runtimeBlockPatch("name: a\nruntime:\n  max_turns: 3\n", { name: "a", runtime: { max_turns: 3 } }), null);
	assert.equal(runtimeBlockPatch("name: a\n", { name: "a" }), "name: a\nruntime: {}\n");
	assert.equal(runtimeBlockPatch("name: a", { name: "a" }), "name: a\nruntime: {}\n");
	assert.equal(runtimeBlockPatch("name: a\nruntime:\ntools: [cli]\n", { name: "a", runtime: null, tools: ["cli"] }), "name: a\nruntime: {}\ntools: [cli]\n");
	assert.equal(runtimeBlockPatch("name: a\nruntime: ~ # none\n", { name: "a", runtime: null }), "name: a\nruntime: {}\n");
});

test("an agent without a runtime block runs, and the compatibility patch is reported", async (t) => {
	const stub = await startStub({ turns: [textTurn("hello")] });
	t.after(stub.close);
	const run = makeRun({ apiUrl: stub.url });
	const r = await runToEnd(run);
	assert.equal(r.exit.reason, "completed", r.output);
	const compat = r.events.find((e) => e.type === "runtime.compat");
	assert.ok(compat, "compat event emitted");
	assert.match(readFileSync(join(run.agent, "agent.yaml"), "utf8"), /runtime: \{\}/);
});

test("a tool call waits for approval and runs once approved", async (t) => {
	const stub = await startStub({
		turns: [toolCallTurn("cli", { command: "echo approved-marker > ../approved-marker" }), textTurn("done")],
		policy: () => ({ decision: "pending", approval_id: "apr_1", reason: "shell commands need approval" }),
		approval: (n) => (n < 3 ? { decision: "pending", approval_id: "apr_1" } : { decision: "allow", approval_id: "apr_1" }),
	});
	t.after(stub.close);
	const run = makeRun({ apiUrl: stub.url });
	const r = await runToEnd(run);
	assert.equal(r.exit.reason, "completed", r.output);
	assert.equal(existsSync(join(run.dir, "approved-marker")), true, "approved command should run");
	assert.equal(stub.state.approvalPolls.length, 3);
	assert.match(stub.state.approvalPolls[0].url, /^\/internal\/v1\/runs\/run_test\/approvals\/apr_1\?wait=50$/);
	assert.equal(stub.state.approvalPolls[0].auth, `Bearer ${TOKEN}`);
	const waiting = r.events.find((e) => e.type === "tool.awaiting_approval");
	assert.deepEqual(waiting.data, { name: "cli", approval_id: "apr_1", reason: "shell commands need approval" });
	const result = r.events.find((e) => e.type === "tool.approval_result");
	assert.equal(result.data.allowed, true);
	assert.equal(result.data.approval_id, "apr_1");
});

test("a rejected approval blocks the tool call", async (t) => {
	const stub = await startStub({
		turns: [toolCallTurn("cli", { command: "touch ../rejected-marker" }), textTurn("ok")],
		policy: () => ({ decision: "pending", approval_id: "apr_2" }),
		approval: () => ({ decision: "deny", reason: "the request was rejected: no" }),
	});
	t.after(stub.close);
	const run = makeRun({ apiUrl: stub.url });
	const r = await runToEnd(run);
	assert.equal(existsSync(join(run.dir, "rejected-marker")), false);
	assert.equal(r.events.find((e) => e.type === "tool.blocked").data.reason, "the request was rejected: no");
});

test("waitForApproval fails closed", async () => {
	const spec = { api_url: "http://127.0.0.1:1", run_id: "r" };
	const reply = (body, ok = true, status = 200) => async () => ({ ok, status, json: async () => body });
	assert.equal((await waitForApproval(spec, "t", "a", { fetchImpl: reply({ decision: "allow" }) })).allow, true);
	assert.equal((await waitForApproval(spec, "t", "a", { fetchImpl: reply({ decision: "ALLOW" }) })).allow, false);
	assert.equal((await waitForApproval(spec, "t", "a", { fetchImpl: reply({}) })).allow, false);
	const errs = await waitForApproval(spec, "t", "a", { fetchImpl: reply({}, false, 500), maxErrors: 2, retryDelayMs: 1 });
	assert.equal(errs.allow, false);
	assert.match(errs.reason, /could not be reached/);
	const ac = new AbortController();
	ac.abort();
	assert.match((await waitForApproval(spec, "t", "a", { fetchImpl: reply({ decision: "allow" }), signal: ac.signal })).reason, /cancelled/);
	// A malformed pending answer (no approval id) from the policy check is a deny.
	const got = await checkToolCall(spec, "t", "cli", {}, async () => ({ ok: true, json: async () => ({ decision: "pending" }) }));
	assert.equal(got.allow, false);
	assert.equal(got.pending, undefined);
});

test("promptWithHistory includes earlier turns", () => {
	assert.equal(promptWithHistory("hi", []), "hi");
	assert.equal(promptWithHistory("hi", undefined), "hi");
	const p = promptWithHistory("And the tests?", [{ role: "user", content: "Summarise the repo" }, { role: "assistant", content: "It is a Go service." }]);
	assert.match(p, /User: Summarise the repo/);
	assert.match(p, /You \(the agent\): It is a Go service\./);
	assert.ok(p.endsWith("And the tests?"));
});

test("conversation history reaches the model", async (t) => {
	const stub = await startStub({ turns: [textTurn("Tests are in test/.")] });
	t.after(stub.close);
	const run = makeRun({ apiUrl: stub.url });
	const spec = JSON.parse(readFileSync(join(run.dir, "spec.json"), "utf8"));
	spec.history = [{ role: "user", content: "remember-this-word: pelican" }, { role: "assistant", content: "Noted." }];
	writeFileSync(join(run.dir, "spec.json"), JSON.stringify(spec));
	const r = await runToEnd(run);
	assert.equal(r.exit.reason, "completed");
	const sent = JSON.stringify(stub.state.modelRequests[0].body.messages);
	assert.match(sent, /remember-this-word: pelican/);
});

test("repositoryNote tells the agent where the repository is", () => {
	assert.equal(repositoryNote(undefined), "");
	const n = repositoryNote({ dir: "repo", name: "widgets", branch: "lux/task_1", test_command: "go test ./..." });
	assert.match(n, /checked out at \.\/repo on branch lux\/task_1/);
	assert.match(n, /cd repo && go test \.\/\.\.\./);
	assert.doesNotMatch(repositoryNote({ dir: "repo", name: "w", branch: "b" }), /Run the tests/);
});

test("luxNote tells the agent its workspace persists and is what the person sees", () => {
	const n = luxNote();
	assert.match(n, /\.\/workspace\//);
	assert.match(n, /read it/);
	assert.match(n, /\.\/workspace\/uploads\//);
	assert.match(n, /is empty right now/);
	assert.match(n, /```choices\n/);
	assert.match(luxNote(["planner.md", "uploads/Offsite plan.docx.txt"]), /- uploads\/Offsite plan\.docx\.txt/);
});
