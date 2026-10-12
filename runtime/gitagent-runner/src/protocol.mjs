// Lux runtime protocol v1 helpers (docs/runtime-protocol.md).
import { appendFileSync, readFileSync, renameSync, writeFileSync, existsSync, lstatSync } from "node:fs";
import { join } from "node:path";

const MAX_STRING = 16 * 1024; // per string field in an event
const MAX_ARGS_JSON = 8 * 1024;

/** Truncate a string, marking that it was cut. */
export function clip(s, max = MAX_STRING) {
	if (typeof s !== "string") return s;
	return s.length > max ? s.slice(0, max) + `… [${s.length - max} more characters]` : s;
}

/** Make tool arguments safe to log: bounded size, strings clipped. */
export function clipArgs(args) {
	if (args === undefined || args === null) return {};
	let json;
	try {
		json = JSON.stringify(args);
	} catch {
		return { unserializable: true };
	}
	if (json.length <= MAX_ARGS_JSON) return args;
	const out = {};
	for (const [k, v] of Object.entries(args)) out[k] = typeof v === "string" ? clip(v, 2048) : v;
	const again = JSON.stringify(out);
	return again.length <= MAX_ARGS_JSON ? out : { truncated: true, preview: clip(json, 2048) };
}

/** Appends events to events.jsonl with consecutive seq numbers. */
export class EventWriter {
	constructor(path) {
		this.path = path;
		this.seq = 0;
	}
	emit(type, data = {}) {
		this.seq += 1;
		const line = JSON.stringify({ seq: this.seq, ts: new Date().toISOString(), type, data });
		// One write per line, appended synchronously, so luxd never sees an
		// event out of order and a crash leaves at most one partial line.
		appendFileSync(this.path, line + "\n", { mode: 0o600 });
	}
}

/**
 * Collects streamed text deltas and emits them in chunks, so a long reply
 * becomes tens of events instead of thousands.
 */
export class DeltaBuffer {
	constructor(writer, type, { maxChars = 2000, maxDelayMs = 150 } = {}) {
		this.writer = writer;
		this.type = type;
		this.maxChars = maxChars;
		this.maxDelayMs = maxDelayMs;
		this.buf = "";
		this.timer = null;
	}
	add(text) {
		if (!text) return;
		this.buf += text;
		if (this.buf.length >= this.maxChars) this.flush();
		else if (!this.timer) this.timer = setTimeout(() => this.flush(), this.maxDelayMs);
	}
	flush() {
		if (this.timer) {
			clearTimeout(this.timer);
			this.timer = null;
		}
		if (this.buf) {
			this.writer.emit(this.type, { text: this.buf });
			this.buf = "";
		}
	}
}

/** Write exit.json atomically. Only the first call has effect. */
let exitWritten = false;
export function writeExit(path, { reason, summary = "", error = "" }) {
	if (exitWritten) return false;
	exitWritten = true;
	const tmp = path + ".tmp";
	writeFileSync(tmp, JSON.stringify({ reason, summary: clip(summary, 4000), error: clip(error, 4000) }), { mode: 0o600 });
	renameSync(tmp, path);
	return true;
}

/** For tests: allow writeExit again. */
export function _resetExit() {
	exitWritten = false;
}

/** Read and check spec.json. */
export function readSpec(path) {
	const spec = JSON.parse(readFileSync(path, "utf8"));
	if (spec.protocol !== 1) throw new Error(`unsupported protocol version ${spec.protocol}`);
	for (const k of ["run_id", "task_id", "prompt", "agent_dir", "events_path", "exit_path", "model", "api_url"]) {
		if (typeof spec[k] !== "string" || spec[k] === "") throw new Error(`spec.json is missing ${k}`);
	}
	return spec;
}

/**
 * Compatibility: gitagent <= 2.2.0 reads `manifest.runtime.timeout` without a
 * guard (sdk.ts, index.ts), so an agent.yaml without a `runtime:` block (valid
 * per the OpenGAP spec) crashes it. Given the agent.yaml text and its parsed
 * manifest, return patched text that adds an empty `runtime: {}` block, or
 * null when no change is needed. Only ever applied to the run's private copy.
 */
export function runtimeBlockPatch(text, manifest) {
	if (manifest && typeof manifest.runtime === "object" && manifest.runtime !== null) return null;
	if (manifest && Object.prototype.hasOwnProperty.call(manifest, "runtime")) {
		// `runtime:` present but empty/null: replace that line.
		const replaced = text.replace(/^runtime:[ \t]*(null|~)?[ \t]*(#.*)?$/m, "runtime: {}");
		return replaced === text ? null : replaced;
	}
	return text.replace(/\s*$/, "\n") + "runtime: {}\n";
}

/**
 * Features of an agent repository that make gitagent run code from the
 * repository (or fetch more) while loading. Mirrors Lux's validator.
 */
export function codeExecutionFeatures(manifest, agentDir) {
	const found = [];
	if (manifest?.extends) found.push("extends");
	if (Array.isArray(manifest?.dependencies) && manifest.dependencies.length) found.push("dependencies");
	if (manifest?.plugins && Object.keys(manifest.plugins).length) found.push("plugins");
	for (const [name, cfg] of Object.entries(manifest?.mcp_servers ?? {})) {
		if (cfg && typeof cfg === "object" && "command" in cfg) found.push(`mcp_servers.${name}`);
	}
	const hooks = join(agentDir, "hooks");
	if (existsSync(hooks) && lstatSync(hooks).isDirectory()) found.push("hooks/");
	return found;
}

/**
 * Build the prompt for a conversation turn. gitagent's in-process
 * multi-turn API is not used (it is unreliable in 2.2.0); earlier turns are
 * given as context instead.
 */
export function promptWithHistory(prompt, history) {
	if (!Array.isArray(history) || history.length === 0) return prompt;
	const lines = ["Earlier in this conversation:", ""];
	for (const turn of history) {
		if (!turn || typeof turn.content !== "string") continue;
		const who = turn.role === "assistant" ? "You (the agent)" : "User";
		lines.push(`${who}: ${turn.content}`, "");
	}
	lines.push("Now respond to the user's new message:", "", prompt);
	return lines.join("\n");
}

/**
 * Tells the agent where the task's repository is. Lux checks it out inside
 * the agent directory, where the agent's tools are rooted.
 */
export function repositoryNote(repo) {
	if (!repo || typeof repo.dir !== "string") return "";
	const lines = [
		`This task works on the git repository "${repo.name}", checked out at ./${repo.dir} on branch ${repo.branch}.`,
		`Make your changes there (for example: cd ${repo.dir} && ...). Lux records what you change when the run ends;`,
		"a person reviews it and Lux pushes it. You cannot push, and you do not need to commit.",
	];
	if (repo.test_command) lines.push(`Run the tests with: cd ${repo.dir} && ${repo.test_command}`);
	return lines.join("\n");
}

/**
 * What every agent run under Lux needs to know about where it is: the
 * person cannot see its folders, only its replies and its workspace files.
 */
export function luxNote(files = []) {
	const shown = Array.isArray(files) ? files.filter((f) => typeof f === "string").slice(0, 200) : [];
	const listing = shown.length
		? ["", "Files in ./workspace/ right now:", ...shown.map((f) => `- ${f}`)]
		: ["", "./workspace/ is empty right now."];
	return [
		"You are running in Lux for one person, who talks to you in a chat thread.",
		"They see only your replies and the files in ./workspace/. That folder is kept between conversations",
		"and its files appear in the Lux app, where the person can open and save them. Everything else in",
		"this folder is discarded when you finish.",
		"- Save anything you make for the person (documents, plans, code, data) in ./workspace/.",
		"- Before changing a file in ./workspace/, read it: it may hold earlier work. Never recreate it from memory.",
		"- Files the person attached are in ./workspace/uploads/ (with a .txt copy of documents). When they mention an uploaded file, look there.",
		"- If a file is not where you expected, list ./workspace/ before saying it is missing.",
		"- Refer to files by name (planner.md), not by path.",
		"- Some actions need the person's approval. If one is rejected, say so and carry on without it.",
		"You are not a chat bot: you own ongoing work for this person and keep it moving between their messages.",
		"- Whenever your work changes, call lux__update_status (job, doing, waiting_on, decisions, deadlines). The person sees it in your Work panel.",
		"- When something needs checking later (a reply, a deadline, a price), schedule it with lux__follow_up instead of asking the person to remind you.",
		"- Save what you learn about the person (preferences, people, recurring facts, decisions) with your memory tool; it is there in every future run.",
		"- If you have lux__browser_open, you have your own web browser: use it to look things up and to use websites that have no connector.",
		"- When you ask the person a question that has a few likely answers, ask one question and end your reply",
		"  with the answers as a choices block; Lux shows them as buttons (they can still type their own). Exactly:",
		"```choices",
		"Weekly",
		"Daily",
		"```",
		"  At most 5 answers, each a few words, written as the person would say it. Leave it out for open questions.",
		...listing,
	].join("\n");
}
