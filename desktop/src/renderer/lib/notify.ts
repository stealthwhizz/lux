import { useEffect, useRef } from "react";
import type { Approval, Conversation } from "../../shared/types";
import { lux } from "./lux";

/** What an approval asks for, in plain words. */
export function approvalText(summary: string): string {
  if (summary.startsWith("lux__browser_handoff: ")) return "Needs you in its browser: " + summary.slice(22, 162);
  const s = summary.replace(/^cli: /, "run ").replace(/^lux__/, "").replace(/\s+/g, " ").trim();
  return "Wants to " + (s.length > 140 ? s.slice(0, 139) + "…" : s);
}

/** A reply's first words, without formatting. */
export function replyText(content: string): string {
  const s = content.replace(/```[\s\S]*?```/g, " ").replace(/[*_`#>]+/g, "").replace(/\s+/g, " ").trim();
  return s.length > 160 ? s.slice(0, 159) + "…" : s;
}

/**
 * Tell the person, with a system notification, when an agent needs them or
 * replies while Lux is not the window they are looking at. What was already
 * there when the app opened is not announced.
 */
export function useNotifications(pending?: Approval[], convs?: Conversation[]) {
  const approvals = useRef<Set<string> | null>(null);
  const replies = useRef<Map<string, string> | null>(null);

  useEffect(() => {
    if (!pending) return;
    const known = approvals.current;
    approvals.current = new Set(pending.map((a) => a.id));
    if (!known || document.hasFocus()) return;
    for (const a of pending) {
      if (!known.has(a.id)) void lux().app.notify(`${a.agent_name} needs you`, approvalText(a.summary), a.agent_id);
    }
  }, [pending]);

  useEffect(() => {
    if (!convs) return;
    const known = replies.current;
    const next = new Map<string, string>();
    for (const c of convs) if (c.last_message?.role === "assistant") next.set(c.id, c.last_message.id);
    replies.current = next;
    if (!known || document.hasFocus()) return;
    for (const c of convs) {
      const m = c.last_message;
      if (m?.role === "assistant" && known.get(c.id) !== m.id) void lux().app.notify(`${c.agent_name} replied`, replyText(m.content), c.agent_id);
    }
  }, [convs]);
}
