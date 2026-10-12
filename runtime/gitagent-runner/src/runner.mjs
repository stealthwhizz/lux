#!/usr/bin/env node
// Lux gitagent runner: runs one task with the gitagent SDK and reports
// progress using the Lux runtime protocol v1 (docs/runtime-protocol.md).
//
// Usage: node runner.mjs <spec.json>
import { readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { pathToFileURL } from "node:url";
import yaml from "js-yaml";
import { query } from "@open-gitagent/gitagent";
import { EventWriter, DeltaBuffer, clip, clipArgs, writeExit, readSpec, codeExecutionFeatures, runtimeBlockPatch, promptWithHistory, repositoryNote, luxNote } from "./protocol.mjs";

const RUNNER_VERSION = "0.1.0";
const POLICY_TIMEOUT_MS = 30_000;
const CANCEL_GRACE_MS = 3_000;

function gitagentVersion() {
	try {
		const url = new URL("../node_modules/@open-gitagent/gitagent/package.json", import.meta.url);
		return JSON.parse(readFileSync(url, "utf8")).version;
	} catch {
		return "unknown";
	}
}

/**
 * Ask luxd whether a tool call may run. Anything other than an explicit
 * "allow" (errors, timeouts, odd answers) is a block: fail closed.
 * Returns { allow, reason } or, when luxd opened an approval,
 * { pending: true, approvalId, reason }.
 */
export async function checkToolCall(spec, token, tool, args, fetchImpl = fetch) {
	const url = `${spec.api_url}/internal/v1/runs/${encodeURIComponent(spec.run_id)}/tool-calls`;
	try {
		const res = await fetchImpl(url, {
			method: "POST",
			headers: { "content-type": "application/json", authorization: `Bearer ${token}` },
			body: JSON.stringify({ tool, args: args ?? {} }),
			signal: AbortSignal.timeout(POLICY_TIMEOUT_MS),
		});
		if (!res.ok) return { allow: false, reason: `Lux refused the policy check (HTTP ${res.status})` };
		const body = await res.json();
		if (body?.decision === "allow") return { allow: true };
		if (body?.decision === "pending" && typeof body.approval_id === "string" && body.approval_id) {
			return { allow: false, pending: true, approvalId: body.approval_id, reason: body.reason || "needs approval" };
		}
		return { allow: false, reason: typeof body?.reason === "string" && body.reason ? body.reason : "denied by Lux policy" };
	} catch (err) {
		return { allow: false, reason: `Lux could not confirm this tool call is allowed (${err?.name === "TimeoutError" ? "timed out" : "unreachable"})` };
	}
}

/**
 * Wait for a person to decide an approval. Long-polls luxd; returns
 * { allow, reason }. Errors are retried a few times (luxd may be
 * restarting), then deny. An aborted run denies at once.
 */
export async function waitForApproval(spec, token, approvalId, { fetchImpl = fetch, signal, maxErrors = 5, retryDelayMs = 2000, waitSeconds = 50 } = {}) {
	const url = `${spec.api_url}/internal/v1/runs/${encodeURIComponent(spec.run_id)}/approvals/${encodeURIComponent(approvalId)}?wait=${waitSeconds}`;
	let errors = 0;
	for (;;) {
		if (signal?.aborted) return { allow: false, reason: "the run was cancelled while waiting for approval" };
		try {
			const res = await fetchImpl(url, {
				headers: { authorization: `Bearer ${token}` },
				signal: signal ? AbortSignal.any([signal, AbortSignal.timeout((waitSeconds + 15) * 1000)]) : AbortSignal.timeout((waitSeconds + 15) * 1000),
			});
			if (!res.ok) throw new Error(`HTTP ${res.status}`);
			const body = await res.json();
			errors = 0;
			if (body?.decision === "allow") return { allow: true };
			if (body?.decision === "pending") continue;
			return { allow: false, reason: typeof body?.reason === "string" && body.reason ? body.reason : "the approval was not granted" };
		} catch {
			if (signal?.aborted) return { allow: false, reason: "the run was cancelled while waiting for approval" };
			errors += 1;
			if (errors >= maxErrors) return { allow: false, reason: "Lux could not be reached while waiting for approval" };
			await new Promise((r) => setTimeout(r, retryDelayMs));
		}
	}
}

async function main() {
	// gitagent commits memory and task notes in the run's own checkout; those
	// commits are thrown away with the run (luxd keeps the files), but git
	// refuses to commit without an identity, and the agent would report that.
	for (const k of ["GIT_AUTHOR", "GIT_COMMITTER"]) {
		process.env[`${k}_NAME`] ??= "Lux agent";
		process.env[`${k}_EMAIL`] ??= "agent@lux.invalid";
	}
	const specPath = process.argv[2];
	if (!specPath) {
		console.error("usage: runner.mjs <spec.json>");
		process.exit(2);
	}
	const spec = readSpec(specPath);
	const token = process.env.LUX_RUN_TOKEN;
	const events = new EventWriter(spec.events_path);
	const finish = (reason, extra = {}) => {
		writeExit(spec.exit_path, { reason, ...extra });
	};
	if (!token) {
		events.emit("run.error", { message: "LUX_RUN_TOKEN is not set" });
		finish("error", { error: "the runner was started without a run token" });
		process.exit(1);
	}

	// Defence in depth: luxd already refuses these unless the workspace allows them.
	let manifest = {};
	let compatPatched = false;
	const manifestPath = join(spec.agent_dir, "agent.yaml");
	try {
		const text = readFileSync(manifestPath, "utf8");
		manifest = yaml.load(text) ?? {};
		const patched = runtimeBlockPatch(text, manifest);
		if (patched !== null) {
			// Work around a gitagent 2.2.0 crash on manifests without `runtime:`.
			// This changes only the run's private checkout, never the source.
			writeFileSync(manifestPath, patched);
			compatPatched = true;
		}
	} catch (err) {
		events.emit("run.error", { message: `cannot read agent.yaml: ${err.message}` });
		finish("error", { error: "cannot read agent.yaml" });
		return;
	}
	const features = codeExecutionFeatures(manifest, spec.agent_dir);
	if (features.length && !spec.allow_code_execution) {
		const msg = `this agent runs code when it loads (${features.join(", ")}) and the run does not allow that`;
		events.emit("run.error", { message: msg });
		finish("error", { error: msg });
		return;
	}

	const abort = new AbortController();
	let cancelled = false;
	process.on("SIGTERM", () => {
		if (cancelled) return;
		cancelled = true;
		abort.abort();
		// Do not rely on the SDK to unwind: report and leave after a grace period.
		setTimeout(() => {
			finish("cancelled");
			process.exit(0);
		}, CANCEL_GRACE_MS).unref();
	});

	events.emit("run.started", {
		runtime: "gitagent",
		runtime_version: gitagentVersion(),
		runner_version: RUNNER_VERSION,
		agent: manifest.name ?? "",
		model: spec.model.replace(/@https?:\/\/.*$/, ""),
	});
	if (compatPatched) {
		events.emit("runtime.compat", { file: "agent.yaml", change: "added an empty runtime: block because gitagent 2.2.0 requires one" });
	}

	const text = new DeltaBuffer(events, "assistant.delta");
	const thinking = new DeltaBuffer(events, "thinking.delta");
	const maxTurns = Number.isInteger(spec.max_turns) && spec.max_turns > 0 ? spec.max_turns : 50;
	let turns = 0;
	let lastContent = "";
	let lastError = "";
	let turnLimitHit = false;

	const q = query({
		prompt: promptWithHistory(spec.prompt, spec.history),
		dir: spec.agent_dir,
		systemPromptSuffix: [luxNote(spec.workspace_files), repositoryNote(spec.repository)].filter(Boolean).join("\n\n"),
		// The person's connected accounts, served by luxd: tokens never come here.
		// Every run gets luxd's tools (status, follow-ups, and the person's connected accounts).
		mcpServers: { lux: { type: "http", url: `${spec.api_url}/internal/v1/runs/${spec.run_id}/mcp`, headers: { Authorization: `Bearer ${token}` } } },
		model: spec.model,
		sessionId: spec.run_id,
		maxTurns,
		abortController: abort,
		hooks: {
			preToolUse: async (ctx) => {
				let decision = await checkToolCall(spec, token, ctx.toolName, ctx.args);
				if (decision.pending) {
					text.flush();
					const approvalId = decision.approvalId;
					events.emit("tool.awaiting_approval", { name: ctx.toolName, approval_id: approvalId, reason: decision.reason });
					decision = await waitForApproval(spec, token, approvalId, { signal: abort.signal });
					events.emit("tool.approval_result", { name: ctx.toolName, approval_id: approvalId, allowed: decision.allow, reason: decision.reason ?? "" });
				}
				if (decision.allow) return { action: "allow" };
				text.flush();
				events.emit("tool.blocked", { name: ctx.toolName, reason: decision.reason });
				return { action: "block", reason: decision.reason };
			},
		},
	});

	try {
		for await (const msg of q) {
			switch (msg.type) {
				case "delta":
					(msg.deltaType === "thinking" ? thinking : text).add(msg.content);
					break;
				case "assistant": {
					text.flush();
					thinking.flush();
					turns += 1;
					if (msg.content) lastContent = msg.content;
					if (msg.stopReason === "error") lastError = msg.errorMessage || "the model returned an error";
					events.emit("assistant.message", {
						content: clip(msg.content ?? ""),
						model: msg.model,
						provider: msg.provider,
						stop_reason: msg.stopReason,
						usage: msg.usage ?? null,
						error: msg.errorMessage ? clip(msg.errorMessage, 4000) : undefined,
					});
					// gitagent 2.2.0 does not enforce max_turns; enforce it here.
					if (turns >= maxTurns && msg.stopReason === "toolUse") {
						turnLimitHit = true;
						abort.abort();
					}
					break;
				}
				case "tool_use":
					text.flush();
					events.emit("tool.call", { id: msg.toolCallId, name: msg.toolName, args: clipArgs(msg.args) });
					break;
				case "tool_result":
					events.emit("tool.result", { id: msg.toolCallId, name: msg.toolName, content: clip(msg.content ?? ""), is_error: !!msg.isError });
					break;
				case "system":
					events.emit("system", { subtype: msg.subtype, content: clip(msg.content ?? "", 4000) });
					if (msg.subtype === "error") lastError = msg.content || "runtime error";
					break;
			}
		}
	} catch (err) {
		if (!cancelled && !turnLimitHit) {
			lastError = err?.message || String(err);
			events.emit("run.error", { message: clip(lastError, 4000) });
		}
	}
	text.flush();
	thinking.flush();

	if (cancelled) {
		finish("cancelled");
	} else if (turnLimitHit) {
		finish("error", { summary: lastContent, error: `stopped after reaching the turn limit (${maxTurns})` });
	} else if (lastError) {
		finish("error", { summary: lastContent, error: lastError });
	} else {
		finish("completed", { summary: lastContent });
	}
	process.exit(0);
}

const isMain = process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href;
if (isMain) {
	main().catch((err) => {
		console.error(err);
		try {
			const spec = readSpec(process.argv[2]);
			writeExit(spec.exit_path, { reason: "error", error: `runner crashed: ${err?.message ?? err}` });
		} catch {
			/* nothing more we can do; luxd will report runtime_lost */
		}
		process.exit(1);
	});
}
