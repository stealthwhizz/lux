import type { Approval, Change } from "../../shared/types";
import type { Nav } from "../App";
import { lux, timeAgo, useLoad, useWorkspaceChanges } from "../lib/lux";
import { ApprovalCard } from "../components/ApprovalCard";
import { changeStats } from "../components/Changes";
import { Empty, ErrorNotice, StateChip } from "../components/common";

export function ApprovalsView({ onChange, nav }: { onChange: () => void; nav: Nav }) {
  const pending = useLoad<Approval[]>(() => lux().approvals.list("pending"), []);
  const all = useLoad<Approval[]>(() => lux().approvals.list(""), []);
  const review = useLoad<Change[]>(() => lux().changes.queue("proposed"), []);
  useWorkspaceChanges(() => {
    pending.reload();
    all.reload();
    review.reload();
  });
  const decided = (all.data ?? []).filter((a) => a.state !== "pending").slice(0, 20);
  const changed = () => {
    pending.reload();
    all.reload();
    onChange();
  };
  const nothing = pending.data?.length === 0 && review.data?.length === 0;
  return (
    <div className="page">
      <div className="page-head">
        <h1>Needs you</h1>
        <span className="sub">Actions agents want to take, and changes they made, waiting for your decision.</span>
      </div>
      {pending.error && <ErrorNotice error={pending.error} onRetry={changed} />}
      <div className="now">
        {nothing && <Empty title="Nothing is waiting for you"><p>When an agent asks to run a command or change files, or finishes changes for review, it appears here.</p></Empty>}
        {!!pending.data?.length && (
          <section aria-label="Waiting for you">
            {pending.data.map((a) => <ApprovalCard key={a.id} approval={a} onDecided={changed} />)}
          </section>
        )}
        {!!review.data?.length && (
          <section aria-label="Changes to review">
            <h2>Changes to review</h2>
            <table className="table">
              <tbody>
                {review.data.map((c) => (
                  <tr key={c.task_id + c.target} className="clickable" tabIndex={0} onClick={() => nav.go("tasks", c.task_id)} onKeyDown={(e) => e.key === "Enter" && nav.go("tasks", c.task_id)}>
                    <td style={{ width: "1%", whiteSpace: "nowrap" }}><StateChip state={c.state} /></td>
                    <td>
                      <div style={{ fontWeight: 560 }}>{c.target === "memory" ? "Memory proposal" : "Repository changes"}: {changeStats(c)}</div>
                      <div style={{ color: "var(--ink-3)", fontSize: "var(--t-sm)" }}>{c.agent_name}, {timeAgo(c.created_at)}: {c.task_prompt}</div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </section>
        )}
        {decided.length > 0 && (
          <section aria-label="Recently decided">
            <h2>Recently decided</h2>
            {decided.map((a) => <ApprovalCard key={a.id} approval={a} compact />)}
          </section>
        )}
      </div>
    </div>
  );
}
