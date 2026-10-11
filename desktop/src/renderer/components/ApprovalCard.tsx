import { useState } from "react";
import type { Approval } from "../../shared/types";
import { lux, timeAgo } from "../lib/lux";
import { StateChip, explain, useToast } from "./common";

/** One approval, decidable in place. The lamp border marks "a person is needed". */
export function ApprovalCard({ approval, onDecided, compact = false }: { approval: Approval; onDecided?: () => void; compact?: boolean }) {
  const [note, setNote] = useState("");
  const [busy, setBusy] = useState(false);
  const toast = useToast();
  const pending = approval.state === "pending";
  const args = approval.details?.args ?? {};
  const what = args.command ?? args.path ?? approval.summary;

  async function decide(decision: "approve" | "reject") {
    setBusy(true);
    const r = await lux().approvals.decide(approval.id, decision, note.trim() || undefined);
    setBusy(false);
    if (!r.ok) {
      toast(explain(r.error), true);
      return;
    }
    toast(decision === "approve" ? "Approved" : "Rejected");
    onDecided?.();
  }

  return (
    <article className={`approval${pending ? "" : " decided"}`} aria-label={`Approval request: ${approval.summary}`}>
      <div className="row">
        <strong>{approval.agent_name}</strong>
        <span className="why">wants to use {approval.details?.tool ?? approval.kind}</span>
        <span style={{ marginLeft: "auto" }} className="row">
          <StateChip state={approval.state} />
          <span className="why">{timeAgo(approval.created_at)}</span>
        </span>
      </div>
      <div className="what">{what}</div>
      {approval.details?.reason && <p className="why">{approval.details.reason}</p>}
      {!pending && approval.decision_note && <p className="why">Note: {approval.decision_note}</p>}
      {pending && (
        <div className={compact ? "row" : "stack"} style={{ gap: 8 }}>
          {!compact && (
            <input
              className="input note"
              placeholder="Note (optional, recorded with your decision)"
              value={note}
              onChange={(e) => setNote(e.target.value)}
              aria-label="Decision note"
            />
          )}
          <div className="row">
            <button className="btn approve" disabled={busy} onClick={() => decide("approve")}>
              Approve
            </button>
            <button className="btn" disabled={busy} onClick={() => decide("reject")}>
              Reject
            </button>
          </div>
        </div>
      )}
    </article>
  );
}
