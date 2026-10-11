import { useEffect, useMemo, useRef, useState } from "react";
import type { PushedEvent } from "../../shared/ipc";
import type { Approval, TaskEvent } from "../../shared/types";
import { lux, plural } from "../lib/lux";
import { ApprovalCard } from "./ApprovalCard";
import { RichText } from "./RichText";

/**
 * Follow a task's events live (falls back to the stored list when the task
 * has ended). Events come from the server; nothing is synthesized here.
 */
export function useTaskEvents(taskId: string | undefined) {
  const [events, setEvents] = useState<TaskEvent[]>([]);
  const [connected, setConnected] = useState(true);
  const [ended, setEnded] = useState(false);
  useEffect(() => {
    setEvents([]);
    setEnded(false);
    if (!taskId) return;
    let sub: string | null = null;
    let live = true;
    const seen = new Set<number>();
    const add = (evs: TaskEvent[]) => {
      const fresh = evs.filter((e) => !seen.has(e.seq));
      fresh.forEach((e) => seen.add(e.seq));
      if (fresh.length) setEvents((prev) => [...prev, ...fresh].sort((a, b) => a.seq - b.seq));
    };
    const handle = (msg: PushedEvent) => {
      if (msg.kind === "event") {
        const d = msg.data as { event: string; data: TaskEvent };
        if (d.event === "task_event") add([d.data]);
      } else if (msg.kind === "end") setEnded(true);
      else if (msg.kind === "status") setConnected(!!(msg.data as any)?.connected);
    };
    // The server replays history as soon as the stream opens, which can be
    // before we learn our subscription id: hold messages until then.
    const early: PushedEvent[] = [];
    const stop = lux().events.onEvent((msg: PushedEvent) => {
      if (!live) return;
      if (sub === null) early.push(msg);
      else if (msg.subscription === sub) handle(msg);
    });
    lux().events.subscribeTask(taskId).then((r) => {
      if (!live) {
        if (r.ok) void lux().events.unsubscribe(r.data);
        return;
      }
      if (!r.ok) {
        setConnected(false);
        return;
      }
      sub = r.data;
      for (const m of early.splice(0)) if (m.subscription === sub) handle(m);
    });
    return () => {
      live = false;
      stop();
      if (sub) void lux().events.unsubscribe(sub);
    };
  }, [taskId]);
  return { events, connected, ended };
}

interface Item {
  key: string;
  kind: "text" | "tool" | "state" | "alert" | "approval";
  text?: string;
  tool?: { name: string; arg: string; result?: string; error?: boolean; blocked?: string };
  approvalId?: string;
}

/** Fold raw events into readable items: streamed text joins, tool calls pair with results. */
export function foldEvents(events: TaskEvent[]): Item[] {
  const items: Item[] = [];
  const tools = new Map<string, Item>();
  let text: Item | null = null;
  const flushText = () => {
    text = null;
  };
  for (const e of events) {
    const p = e.payload ?? {};
    switch (e.type) {
      case "assistant.delta":
        if (!text) {
          text = { key: `t${e.seq}`, kind: "text", text: "" };
          items.push(text);
        }
        text.text += p.text ?? "";
        break;
      case "assistant.message":
        if (p.error) items.push({ key: `a${e.seq}`, kind: "alert", text: `Model error: ${p.error}` });
        flushText();
        break;
      case "tool.call": {
        flushText();
        const it: Item = { key: `c${e.seq}`, kind: "tool", tool: { name: p.name, arg: p.args?.command ?? p.args?.path ?? JSON.stringify(p.args ?? {}) } };
        tools.set(p.id, it);
        items.push(it);
        break;
      }
      case "tool.result": {
        const it = tools.get(p.id);
        if (it?.tool) Object.assign(it.tool, { result: p.content, error: !!p.is_error });
        break;
      }
      case "tool.blocked": {
        flushText();
        items.push({ key: `b${e.seq}`, kind: "alert", text: `${p.name} was blocked: ${p.reason}` });
        break;
      }
      case "lux.approval_requested":
        flushText();
        items.push({ key: `p${e.seq}`, kind: "approval", approvalId: p.approval_id, text: p.summary });
        break;
      case "lux.tool_denied":
        items.push({ key: `d${e.seq}`, kind: "alert", text: `${p.tool} is not allowed by the workspace policy: ${p.reason}` });
        break;
      case "lux.changes": {
        const where = p.target === "memory" ? "its memory" : "the repository";
        items.push({ key: `g${e.seq}`, kind: "state", text: `Changed ${where}: ${plural(p.files, "file")}, +${p.added} −${p.deleted}${p.reviewable ? "" : " (too large to review here)"}` });
        break;
      }
      case "lux.changes_published":
        items.push({ key: `g${e.seq}`, kind: "state", text: `${p.by} published the ${p.target} changes to ${p.branch}` });
        break;
      case "lux.changes_error":
      case "lux.budget_exceeded":
      case "run.error":
        items.push({ key: `e${e.seq}`, kind: "alert", text: p.message });
        break;
      case "task.state":
        if (["queued", "starting"].includes(p.state)) break;
        flushText();
        items.push({ key: `s${e.seq}`, kind: "state", text: stateLine(p.state, p.reason) });
        break;
    }
  }
  return items;
}

function stateLine(state: string, reason?: string) {
  const base: Record<string, string> = {
    running: "Started",
    waiting_approval: "Paused for approval",
    succeeded: "Finished",
    failed: "Failed",
    cancelled: "Cancelled",
    timed_out: "Stopped: time limit reached",
    cancelling: "Cancelling",
  };
  const s = base[state] ?? state;
  return reason && !["waiting_approval", "running"].includes(state) ? `${s}: ${reason}` : s;
}

export function Timeline({ events, approvals, onApprovalDecided }: { events: TaskEvent[]; approvals: Approval[]; onApprovalDecided: () => void }) {
  const items = useMemo(() => foldEvents(events), [events]);
  const end = useRef<HTMLDivElement>(null);
  // Block body: in current Chromium scrollIntoView() returns a Promise, which
  // React would mistake for an effect cleanup function.
  useEffect(() => {
    void end.current?.scrollIntoView({ block: "nearest" });
  }, [items.length]);
  const byId = new Map(approvals.map((a) => [a.id, a]));
  return (
    <div className="timeline">
      {items.map((it) => {
        switch (it.kind) {
          case "text":
            return <RichText key={it.key} text={it.text ?? ""} />;
          case "state":
            return <div key={it.key} className="t-state">{it.text}</div>;
          case "alert":
            return <div key={it.key} className="t-alert" role="status">{it.text}</div>;
          case "approval": {
            const a = byId.get(it.approvalId!);
            return a ? <ApprovalCard key={it.key} approval={a} onDecided={onApprovalDecided} /> : <div key={it.key} className="t-state">Approval requested: {it.text}</div>;
          }
          case "tool":
            return (
              <details key={it.key} className="t-tool">
                <summary>
                  <span className="name">{it.tool!.name}</span>
                  <span className="arg">{it.tool!.arg}</span>
                  <span className={`status${it.tool!.error ? " error" : ""}`}>{it.tool!.result === undefined ? "running…" : it.tool!.error ? "error" : "done"}</span>
                </summary>
                {it.tool!.result !== undefined && <pre>{it.tool!.result || "(no output)"}</pre>}
              </details>
            );
        }
      })}
      <div ref={end} />
    </div>
  );
}
