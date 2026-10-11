import { useEffect, useState } from "react";
import type { Approval, Change, Task } from "../../shared/types";
import { isTerminal } from "../../shared/types";
import type { Nav } from "../App";
import { lux, timeAgo, useLoad, useWorkspaceChanges } from "../lib/lux";
import { Empty, ErrorNotice, StateChip, explain, useToast } from "../components/common";
import { Timeline, useTaskEvents } from "../components/Timeline";
import { ChangesPanel } from "../components/Changes";
import { RichText } from "../components/RichText";

const FILTERS: { label: string; state?: string }[] = [
  { label: "All" },
  { label: "Active", state: "queued,starting,running,cancelling" },
  { label: "Needs approval", state: "waiting_approval" },
  { label: "Finished", state: "succeeded,failed,cancelled,timed_out" },
];

export function TasksView({ nav }: { nav: Nav }) {
  const [filter, setFilter] = useState(0);
  const tasks = useLoad<Task[]>(() => lux().tasks.list({ state: FILTERS[filter].state, limit: 100 }), [filter]);
  useWorkspaceChanges(() => tasks.reload());
  const selected = nav.target && nav.target.startsWith("task_") ? nav.target : tasks.data?.[0]?.id;

  return (
    <div className="split">
      <div className="list-pane">
        <div className="list-head">
          <h1>Tasks</h1>
        </div>
        <div className="row" role="tablist" aria-label="Filter tasks" style={{ padding: "8px 10px", gap: 2 }}>
          {FILTERS.map((f, i) => (
            <button key={f.label} role="tab" aria-selected={filter === i} className={`btn tab ${filter === i ? "" : "quiet"}`} onClick={() => setFilter(i)}>
              {f.label}
            </button>
          ))}
        </div>
        {tasks.error && <div style={{ padding: 12 }}><ErrorNotice error={tasks.error} onRetry={tasks.reload} /></div>}
        {tasks.data?.length === 0 && <p style={{ padding: 16, color: "var(--ink-3)" }}>No tasks here.</p>}
        <div className="rows">
          {tasks.data?.map((t) => (
            <button key={t.id} className="rowlink" aria-current={t.id === selected} onClick={() => nav.go("tasks", t.id)}>
              <span className="top">
                <StateChip state={t.state} />
                <span className="when">{timeAgo(t.created_at)}</span>
              </span>
              <span className="title">{t.prompt}</span>
              <span className="meta">{t.agent_name}</span>
            </button>
          ))}
        </div>
      </div>
      <div className="detail-pane">
        {selected ? <TaskDetail key={selected} id={selected} onChanged={tasks.reload} /> : !tasks.loading && <Empty title="No task selected"><p>Start a task from an agent, or from a conversation.</p></Empty>}
      </div>
    </div>
  );
}

export function TaskDetail({ id, onChanged }: { id: string; onChanged?: () => void }) {
  const task = useLoad<Task>(() => lux().tasks.get(id), [id]);
  const approvals = useLoad<Approval[]>(() => lux().approvals.list(""), [id]);
  const changes = useLoad<Change[]>(() => lux().changes.forTask(id), [id]);
  const { events, connected } = useTaskEvents(id);
  const toast = useToast();
  // Refresh the header when the event stream reports a state change.
  const lastState = [...events].reverse().find((e) => e.type === "task.state")?.payload.state as string | undefined;
  const lastApproval = events.filter((e) => e.type.startsWith("lux.approval_")).length;
  const changeEvents = events.filter((e) => e.type.startsWith("lux.changes")).length;
  useEffect(() => {
    if (changeEvents) changes.reload();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [changeEvents]);
  useEffect(() => {
    if (!lastState) return;
    task.reload();
    onChanged?.();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [lastState]);
  useEffect(() => {
    approvals.reload();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [lastApproval]);

  if (task.error) return <div className="detail"><ErrorNotice error={task.error} onRetry={task.reload} /></div>;
  const t = task.data;
  if (!t) return null;
  const mine = (approvals.data ?? []).filter((a) => a.task_id === id);

  async function cancel() {
    const r = await lux().tasks.cancel(id);
    if (!r.ok) toast(explain(r.error), true);
    else toast("Cancellation requested");
    task.reload();
  }

  return (
    <div className="detail">
      <div className="detail-head">
        <div className="row">
          <StateChip state={t.state} />
          {!isTerminal(t.state) && t.state !== "cancelling" && (
            <button className="btn danger" style={{ marginLeft: "auto" }} onClick={cancel}>Cancel task</button>
          )}
        </div>
        <h2>{t.prompt}</h2>
        <div className="facts">
          <span><b>{t.agent_name}</b> at <code>{t.commit_sha.slice(0, 12)}</code></span>
          <span>Started {timeAgo(t.started_at ?? t.created_at)}</span>
          {t.attempt > 1 && <span>Attempt {t.attempt}</span>}
          <code>{t.id}</code>
        </div>
        {t.isolation === "none" && (
          <div className="notice warn">This task runs without isolation, as the server's operating-system user. Only run agents you trust.</div>
        )}
        {t.state_reason && isTerminal(t.state) && t.state !== "succeeded" && <div className="notice error">{t.state_reason}</div>}
        {!connected && <div className="notice warn">Live updates paused; reconnecting.</div>}
      </div>
      <Timeline events={events} approvals={mine} onApprovalDecided={() => { approvals.reload(); task.reload(); }} />
      <ChangesPanel changes={changes.data ?? []} canPublish={t.state === "succeeded"} onChanged={changes.reload} />
      {t.state === "succeeded" && t.result_summary && !events.some((e) => e.type.startsWith("assistant.")) && (
        <section aria-label="Result" className="stack">
          <h3 style={{ fontSize: "var(--t-md)" }}>Result</h3>
          <RichText text={t.result_summary} />
        </section>
      )}
    </div>
  );
}
