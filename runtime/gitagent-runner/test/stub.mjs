// A scripted stand-in for luxd in tests: an OpenAI-compatible
// /gateway/v1/chat/completions that replays canned model turns, and the
// /internal/v1/runs/:id/tool-calls policy endpoint. Test-only.
import http from "node:http";

export function toolCallTurn(name, args, id = "call_1") {
	return { kind: "tool", name, args, id };
}
export function textTurn(text) {
	return { kind: "text", text };
}

function sse(res, obj) {
	res.write(`data: ${JSON.stringify(obj)}\n\n`);
}

function chunk(delta, finish = null) {
	return { id: "chatcmpl-test", object: "chat.completion.chunk", created: 0, model: "stub", choices: [{ index: 0, delta, finish_reason: finish }] };
}

export async function startStub({ turns = [], policy = () => ({ decision: "allow" }), approval = () => ({ decision: "allow" }), modelStatus = 200, modelHangs = false } = {}) {
	const state = { modelRequests: [], policyRequests: [], approvalPolls: [], turn: 0 };
	const server = http.createServer(async (req, res) => {
		let body = "";
		for await (const c of req) body += c;
		const json = body ? JSON.parse(body) : {};
		if (req.url.startsWith("/internal/v1/runs/") && req.url.endsWith("/tool-calls")) {
			state.policyRequests.push({ url: req.url, auth: req.headers.authorization, body: json });
			const answer = policy(json);
			res.writeHead(answer.status ?? 200, { "content-type": "application/json" });
			res.end(JSON.stringify(answer));
			return;
		}
		if (req.method === "GET" && /^\/internal\/v1\/runs\/[^/]+\/approvals\//.test(req.url)) {
			state.approvalPolls.push({ url: req.url, auth: req.headers.authorization });
			const answer = approval(state.approvalPolls.length);
			res.writeHead(answer.status ?? 200, { "content-type": "application/json" });
			res.end(JSON.stringify(answer));
			return;
		}
		if (req.url === "/gateway/v1/chat/completions") {
			state.modelRequests.push({ auth: req.headers.authorization, body: json });
			if (modelHangs) return; // never answer
			if (modelStatus !== 200) {
				res.writeHead(modelStatus, { "content-type": "application/json" });
				res.end(JSON.stringify({ error: { message: "The Lux model gateway is not implemented yet, so this agent cannot call a model.", type: "lux_error", code: "gateway_not_implemented" } }));
				return;
			}
			const turn = turns[Math.min(state.turn, turns.length - 1)];
			state.turn += 1;
			res.writeHead(200, { "content-type": "text/event-stream" });
			sse(res, chunk({ role: "assistant", content: "" }));
			if (turn.kind === "tool") {
				sse(res, chunk({ tool_calls: [{ index: 0, id: turn.id, type: "function", function: { name: turn.name, arguments: "" } }] }));
				sse(res, chunk({ tool_calls: [{ index: 0, function: { arguments: JSON.stringify(turn.args) } }] }));
				sse(res, chunk({}, "tool_calls"));
			} else {
				for (const part of turn.text.match(/.{1,5}/gs) ?? []) sse(res, chunk({ content: part }));
				sse(res, chunk({}, "stop"));
			}
			sse(res, { ...chunk({}), choices: [], usage: { prompt_tokens: 11, completion_tokens: 7, total_tokens: 18 } });
			res.write("data: [DONE]\n\n");
			res.end();
			return;
		}
		res.writeHead(404);
		res.end();
	});
	await new Promise((r) => server.listen(0, "127.0.0.1", r));
	const { port } = server.address();
	return { url: `http://127.0.0.1:${port}`, state, close: () => new Promise((r) => { server.closeAllConnections(); server.close(r); }) };
}
