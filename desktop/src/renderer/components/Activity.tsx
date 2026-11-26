import { useState } from "react";
import type { Approval, TaskEvent } from "../../shared/types";
import { lux } from "../lib/lux";
import { explain, useToast } from "./common";

/** One thing the agent did, in plain words. */
export interface Step {
  key: string;
  text: string;
  state: "doing" | "done" | "failed" | "blocked";
  approvalId?: string;
  /** The agent asked the person to do something in its browser. */
  handoff?: string;
}

const host = (u: unknown) => {
  try {
    return new URL(String(u)).host;
  } catch {
    return "a page";
  }
};
const base = (p: unknown) => String(p ?? "").split("/").pop() || String(p ?? "");
const clip = (s: string, n = 70) => (s.length > n ? s.slice(0, n - 1) + "…" : s);

// gitagent's own bookkeeping tools mean nothing to the person.
const QUIET = new Set(["task_tracker", "skill_learner"]);

function describe(name: string, args: Record<string, any> = {}, done: boolean): string {
  switch (name) {
    case "read": return `${done ? "Read" : "Reading"} ${base(args.path)}`;
    case "write": return `${done ? "Saved" : "Saving"} ${base(args.path)}`;
    case "edit": return `${done ? "Edited" : "Editing"} ${base(args.path)}`;
    case "cli": return `${done ? "Ran" : "Running"} ${clip(String(args.command ?? ""))}`;
    case "memory": return done ? "Updated its memory" : "Updating its memory";
    case "lux__update_status": return done ? "Updated its status" : "Updating its status";
    case "lux__follow_up": return done ? "Scheduled a check-in" : "Scheduling a check-in";
    case "lux__web_search": return `${done ? "Searched" : "Searching"} the web for “${clip(String(args.query ?? ""), 50)}”`;
    case "lux__browser_open": return `${done ? "Opened" : "Opening"} ${host(args.url)}`;
    case "lux__browser_read": return done ? "Read the page" : "Reading the page";
    case "lux__browser_back": return "Went back a page";
    case "lux__browser_click": return done ? "Clicked on the page" : "Clicking on the page";
    case "lux__browser_type": return done ? "Typed into the page" : "Typing into the page";
    default: return `${done ? "Used" : "Using"} ${name}`;
  }
}

/** Fold a run's events into the steps a person cares about. */
export function stepsFrom(events: TaskEvent[]): Step[] {
  const steps: Step[] = [];
  const byCall = new Map<string, Step>();
  const approvalFor = new Map<string, Step>();
  for (const e of events) {
    const p = e.payload ?? {};
    switch (e.type) {
      case "tool.call": {
        if (QUIET.has(p.name)) break;
        const s: Step = { key: `c${e.seq}`, text: describe(p.name, p.args, false), state: "doing" };
        (s as any).tool = { name: p.name, args: p.args };
        byCall.set(p.id, s);
        steps.push(s);
        break;
      }
      case "tool.result": {
        const s = byCall.get(p.id);
        if (!s) break;
        const t = (s as any).tool;
        s.text = describe(t.name, t.args, true);
        s.state = p.is_error ? "failed" : "done";
        break;
      }
      case "lux.approval_requested": {
        const summary = String(p.summary ?? "");
        const handoff = summary.startsWith("lux__browser_handoff: ") ? summary.slice(22) : undefined;
        const s: Step = handoff
          ? { key: `a${e.seq}`, text: `Needs you in its browser: ${clip(handoff, 90)}`, state: "blocked", approvalId: p.approval_id, handoff }
          : { key: `a${e.seq}`, text: `Wants to ${clip(summary.replace(/^cli: /, "run "), 90)}`, state: "blocked", approvalId: p.approval_id };
        approvalFor.set(p.approval_id, s);
        steps.push(s);
        break;
      }
      case "lux.approval_decided": {
        const s = approvalFor.get(p.approval_id);
        if (s) {
          s.state = p.decision === "allow" ? "done" : "failed";
          s.text = s.handoff
            ? `${p.decision === "allow" ? "You did" : "You couldn't"}: ${clip(s.handoff, 90)}`
            : s.text.replace(/^Wants to/, p.decision === "allow" ? "Allowed to" : "Not allowed to");
          s.approvalId = undefined;
        }
        break;
      }
      case "tool.blocked":
        steps.push({ key: `b${e.seq}`, text: `Stopped: ${clip(String(p.reason ?? ""), 90)}`, state: "failed" });
        break;
      case "lux.tool_denied":
        steps.push({ key: `d${e.seq}`, text: `Not allowed by your workspace rules: ${p.tool}`, state: "failed" });
        break;
    }
  }
  return steps;
}

/** The agent at work: what it is doing now, with earlier steps folded. */
export function Activity({ events, approvals, onDecided, onOpenBrowser }: { events: TaskEvent[]; approvals: Approval[]; onDecided: () => void; onOpenBrowser?: () => void }) {
  const [open, setOpen] = useState(false);
  const steps = stepsFrom(events);
  const pending = steps.filter((s) => s.approvalId && approvals.some((a) => a.id === s.approvalId && a.state === "pending"));
  const current = [...steps].reverse().find((s) => s.state === "doing");
  const earlier = steps.filter((s) => s !== current && !pending.includes(s));
  return (
    <div className={`activity${pending.length ? " needs" : ""}`}>
      {pending.map((s) => <Decision key={s.key} step={s} onDecided={onDecided} onOpenBrowser={onOpenBrowser} />)}
      {!pending.length && (
        <div className="activity-now">
          <span className="pulse" aria-hidden="true" />
          {current?.text ?? "Thinking…"}
        </div>
      )}
      {earlier.length > 0 && (
        <>
          <button className="link quiet small" onClick={() => setOpen(!open)} aria-expanded={open}>
            {open ? "Hide steps" : `${earlier.length} ${earlier.length === 1 ? "step" : "steps"} so far`}
          </button>
          {open && (
            <ol className="steps">
              {earlier.map((s) => <li key={s.key} className={s.state}>{s.text}</li>)}
            </ol>
          )}
        </>
      )}
    </div>
  );
}

function Decision({ step, onDecided, onOpenBrowser }: { step: Step; onDecided: () => void; onOpenBrowser?: () => void }) {
  const [busy, setBusy] = useState(false);
  const toast = useToast();
  async function decide(d: "approve" | "reject") {
    setBusy(true);
    const r = await lux().approvals.decide(step.approvalId!, d);
    setBusy(false);
    if (!r.ok) return toast(explain(r.error), true);
    onDecided();
  }
  return (
    <div className="decision" role="group" aria-label="Needs your approval">
      <span className="decision-text">{step.text}</span>
      <div className="row">
        {step.handoff ? (
          <>
            {onOpenBrowser && <button className="btn" onClick={onOpenBrowser}>Open browser</button>}
            <button className="btn approve" disabled={busy} onClick={() => decide("approve")}>Done</button>
            <button className="btn quiet" disabled={busy} onClick={() => decide("reject")}>I can't</button>
          </>
        ) : (
          <>
            <button className="btn approve" disabled={busy} onClick={() => decide("approve")}>Allow</button>
            <button className="btn quiet" disabled={busy} onClick={() => decide("reject")}>Don't allow</button>
          </>
        )}
      </div>
    </div>
  );
}
