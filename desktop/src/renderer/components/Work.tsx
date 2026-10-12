import { useEffect, useState } from "react";
import type { Agent, AgentMemory, AgentWork } from "../../shared/types";
import { lux, shortTime, useLoad, useWorkspaceChanges } from "../lib/lux";
import { ErrorNotice, explain, useToast } from "./common";
import { describeCron } from "./Routines";

/** The agent's work, like a colleague's status: its job, what it is doing, what it needs from you, and when it checks back. */
export function WorkPanel({ agent, active, onClose, onChanged, openRoutines }: {
  agent: Agent;
  active?: string;
  onClose: () => void;
  onChanged: () => void;
  openRoutines: () => void;
}) {
  const work = useLoad<AgentWork>(() => lux().agents.work(agent.id), [agent.id]);
  const toast = useToast();
  useWorkspaceChanges(() => work.reload());
  const st = work.data?.status?.status;

  async function pause() {
    const off = !agent.disabled;
    if (off && active) await lux().tasks.cancel(active);
    const r = await lux().agents.setDisabled(agent.id, off);
    if (!r.ok) return toast(explain(r.error), true);
    toast(off ? `Paused ${agent.name}: no routines or follow-ups run until you resume` : `Resumed ${agent.name}`);
    onChanged();
  }
  async function cancelFollowUp(id: string) {
    const r = await lux().routines.remove(id);
    if (!r.ok) return toast(explain(r.error), true);
    work.reload();
  }

  return (
    <aside className="files-panel work-panel" aria-label="Work">
      <div className="files-head">
        <h2>Work</h2>
        <button className="link quiet" onClick={onClose}>Close</button>
      </div>
      {work.error && <ErrorNotice error={work.error} onRetry={work.reload} />}
      <div className="files-list">
        {work.data && !st && (
          <p className="chat-hint">Give {agent.name} a job and it keeps track of it here: what it is doing, what it needs from you, decisions and deadlines.</p>
        )}
        {st && (
          <>
            {st.job && <h3 className="work-job">{st.job}</h3>}
            <section className="work-sec">
              <span className="work-label">Doing</span>
              <p>{st.doing}</p>
              {work.data?.status?.updated_at && <span className="why">Updated {shortTime(work.data.status.updated_at)}</span>}
            </section>
            {!!st.waiting_on?.length && (
              <section className="work-sec needs">
                <span className="work-label">Waiting on you</span>
                <ul>{st.waiting_on.map((w) => <li key={w}>{w}</li>)}</ul>
              </section>
            )}
            {!!st.deadlines?.length && (
              <section className="work-sec">
                <span className="work-label">Deadlines</span>
                <ul>{st.deadlines.map((d) => <li key={d.what}><span>{d.what}</span><span className="why"> · {d.when}</span></li>)}</ul>
              </section>
            )}
            {!!st.decisions?.length && (
              <section className="work-sec">
                <span className="work-label">Decided</span>
                <ul>{st.decisions.map((d) => <li key={d}>{d}</li>)}</ul>
              </section>
            )}
          </>
        )}
        {!!work.data?.follow_ups.length && (
          <section className="work-sec">
            <span className="work-label">It will check back</span>
            <ul>
              {work.data.follow_ups.map((f) => (
                <li key={f.id} className="work-follow">
                  <span>{f.name.replace(/^Follow-up: /, "").replace(/ \([^)]*UTC\)$/, "")}</span>
                  <span className="why">{f.next_run_at ? shortTime(f.next_run_at) : ""}</span>
                  <button className="link quiet small" onClick={() => cancelFollowUp(f.id)}>Cancel</button>
                </li>
              ))}
            </ul>
          </section>
        )}
        {!!work.data?.routines.length && (
          <section className="work-sec">
            <span className="work-label">Routines</span>
            <ul>{work.data.routines.map((r) => <li key={r.id}>{r.name}<span className="why"> · {describeCron(r.cron)}{r.paused ? ", paused" : ""}</span></li>)}</ul>
            <button className="link quiet small" onClick={openRoutines}>Manage routines</button>
          </section>
        )}
        <div className="row">
          <button className={`btn${agent.disabled ? " primary" : ""}`} onClick={pause}>{agent.disabled ? "Resume" : "Pause"}</button>
        </div>
      </div>
    </aside>
  );
}

/** What the agent has written down about you, which it reads at the start of every run. */
export function MemoryPanel({ agent, onClose }: { agent: Agent; onClose: () => void }) {
  const mem = useLoad<AgentMemory>(() => lux().agents.memory(agent.id), [agent.id]);
  const [text, setText] = useState("");
  const [busy, setBusy] = useState(false);
  const toast = useToast();
  useEffect(() => {
    if (mem.data) setText(mem.data.text);
  }, [mem.data]);
  async function save() {
    setBusy(true);
    const r = await lux().agents.setMemory(agent.id, text);
    setBusy(false);
    if (!r.ok) return toast(explain(r.error), true);
    toast("Saved. It reads this at the start of every run.");
    mem.reload();
  }
  return (
    <aside className="files-panel" aria-label="Memory">
      <div className="files-head">
        <h2>Memory</h2>
        <button className="link quiet" onClick={onClose}>Close</button>
      </div>
      {mem.error && <ErrorNotice error={mem.error} onRetry={mem.reload} />}
      <div className="files-list">
        <p className="chat-hint">What {agent.name} knows about you: preferences, people and decisions it saved. It reads this every time it works. Change anything that is wrong.</p>
        {mem.data && !mem.data.editable && <p className="chat-hint">This agent comes from a git repository, so its memory is kept there.</p>}
        {mem.data?.editable && (
          <>
            <textarea className="textarea memory-text" rows={14} value={text} onChange={(e) => setText(e.target.value)} aria-label="Memory"
              placeholder="Nothing yet. It writes here as it learns; you can add things too, like: I prefer aisle seats." />
            <div className="row">
              <button className="btn primary" disabled={busy || text === mem.data.text} onClick={save}>Save</button>
            </div>
          </>
        )}
      </div>
    </aside>
  );
}
