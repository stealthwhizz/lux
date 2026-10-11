import { useState } from "react";
import type { Agent, Report, Repository, Task } from "../../shared/types";
import type { Nav } from "../App";
import { lux, timeAgo, useLoad, useWorkspaceChanges } from "../lib/lux";
import { Dialog, Empty, ErrorNotice, explain, useToast } from "../components/common";
import { TaskRows } from "./TaskRows";
import { Mascot } from "../components/Mascot";

export function AgentsView({ nav }: { nav: Nav }) {
  const agents = useLoad<Agent[]>(() => lux().agents.list(), []);
  useWorkspaceChanges(() => agents.reload());
  const [importing, setImporting] = useState(nav.target === "import");
  const selected = nav.target && nav.target !== "import" ? nav.target : agents.data?.[0]?.slug;

  return (
    <div className="split">
      <div className="list-pane">
        <div className="list-head">
          <h1>Agents</h1>
          <div className="actions">
            <button className="btn primary" onClick={() => setImporting(true)}>Import</button>
          </div>
        </div>
        {agents.error && <div style={{ padding: 12 }}><ErrorNotice error={agents.error} onRetry={agents.reload} /></div>}
        <div className="rows">
          {agents.data?.map((a) => {
            const active = ["queued", "starting", "running", "waiting_approval"].reduce((n, s) => n + (a.task_counts?.[s] ?? 0), 0);
            return (
              <button key={a.id} className="rowlink with-avatar" aria-current={a.slug === selected || a.id === selected} onClick={() => nav.go("agents", a.slug)}>
                <Mascot id={a.slug} size={34} />
                <span className="rowtext">
                <span className="top">
                  <span className="title">{a.name}</span>
                  {a.disabled && <span className="chip plain">Disabled</span>}
                  {!a.revision && <span className="chip failed">No valid revision</span>}
                  {active > 0 && <span className="chip running">{active} active</span>}
                </span>
                <span className="meta">{a.description}</span>
                </span>
              </button>
            );
          })}
        </div>
      </div>
      <div className="detail-pane">
        {selected ? <AgentDetail key={selected} slug={selected} nav={nav} onChanged={agents.reload} /> : !agents.loading && (
          <Empty title="No agents yet">
            <p>An agent is a git repository in the OpenGAP format: an agent.yaml, a SOUL.md, and optional skills, tools and memory. Import one to start.</p>
            <button className="btn primary" onClick={() => setImporting(true)}>Import an agent</button>
          </Empty>
        )}
      </div>
      {importing && <ImportDialog onClose={() => setImporting(false)} onImported={(slug) => { setImporting(false); agents.reload(); nav.go("agents", slug); }} />}
    </div>
  );
}

function Issues({ report }: { report?: Report }) {
  if (!report) return null;
  const items = [...(report.errors ?? []), ...(report.warnings ?? [])];
  if (!items.length) return <p style={{ color: "var(--ink-3)", fontSize: "var(--t-sm)" }}>No validation findings.</p>;
  return (
    <ul className="issues">
      {items.map((i, n) => (
        <li key={n} className={i.severity}>{i.path ? <code>{i.path}</code> : null} {i.message}</li>
      ))}
    </ul>
  );
}

function AgentDetail({ slug, nav, onChanged }: { slug: string; nav: Nav; onChanged: () => void }) {
  const agent = useLoad<Agent>(() => lux().agents.get(slug), [slug]);
  const tasks = useLoad<Task[]>(() => lux().tasks.list({ agent: slug, limit: 10 }), [slug]);
  const [running, setRunning] = useState(false);
  const [busy, setBusy] = useState(false);
  const toast = useToast();

  if (agent.error) return <div className="detail"><ErrorNotice error={agent.error} onRetry={agent.reload} /></div>;
  const a = agent.data;
  if (!a) return null;
  const rep = a.revision?.validation;
  const models = rep?.models ?? [];

  async function sync() {
    setBusy(true);
    const r = await lux().agents.sync(a!.slug);
    setBusy(false);
    if (!r.ok) return toast(explain(r.error), true);
    toast(!r.data.report.valid ? "The latest commit is invalid; the agent keeps its current revision" : r.data.changed ? "Updated to the latest commit" : "Already up to date", !r.data.report.valid);
    agent.reload();
    onChanged();
  }
  async function toggle() {
    const r = await lux().agents.setDisabled(a!.slug, !a!.disabled);
    if (!r.ok) return toast(explain(r.error), true);
    toast(a!.disabled ? "Enabled" : "Disabled");
    agent.reload();
    onChanged();
  }

  return (
    <div className="detail">
      <div className="detail-head">
        <h2>{a.name}</h2>
        <p style={{ color: "var(--ink-2)" }}>{a.description}</p>
        <div className="row">
          <button className="btn primary" disabled={a.disabled || !a.revision} onClick={() => setRunning(true)}>Run a task</button>
          <button className="btn" disabled={a.disabled || !a.revision} onClick={() => nav.go("conversations", a.id)}>Start a conversation</button>
          <button className="btn" disabled={busy} onClick={sync}>Update from source</button>
          <button className="btn quiet" onClick={toggle}>{a.disabled ? "Enable" : "Disable"}</button>
        </div>
      </div>
      {rep?.code_execution?.length ? (
        <div className="notice warn">This agent runs code from its repository when it loads ({rep.code_execution.join(", ")}). It only runs if your workspace policy allows that.</div>
      ) : null}
      <dl className="kv">
        <dt>Source</dt><dd><code>{a.source_url}</code>{a.source_ref && <> at <code>{a.source_ref}</code></>}</dd>
        <dt>Revision</dt><dd>{a.revision ? <><code>{a.revision.commit_sha.slice(0, 12)}</code>, checked {timeAgo(a.revision.created_at)}</> : "None valid. Fix the repository, commit, and update from source."}</dd>
        <dt>Model</dt><dd>{models.length ? models.map((m) => `${m.provider}:${m.model}`).join(", then ") : "Workspace default"}</dd>
        <dt>Runs on</dt><dd>{a.executor} executor{a.executor === "local" ? " (no isolation)" : ""}</dd>
      </dl>
      <section className="stack" aria-label="Validation">
        <h3 style={{ fontSize: "var(--t-md)" }}>Validation</h3>
        <Issues report={rep} />
      </section>
      <section className="stack" aria-label="Recent tasks">
        <h3 style={{ fontSize: "var(--t-md)" }}>Recent tasks</h3>
        {tasks.data?.length ? <TaskRows tasks={tasks.data} nav={nav} /> : <p style={{ color: "var(--ink-3)", fontSize: "var(--t-sm)" }}>No tasks yet.</p>}
      </section>
      {running && <RunDialog agent={a} onClose={() => setRunning(false)} onStarted={(id) => { setRunning(false); nav.go("tasks", id); }} />}
    </div>
  );
}

function RunDialog({ agent, onClose, onStarted }: { agent: Agent; onClose: () => void; onStarted: (id: string) => void }) {
  const [prompt, setPrompt] = useState("");
  const [repo, setRepo] = useState("");
  const [busy, setBusy] = useState(false);
  const repos = useLoad<Repository[]>(() => lux().repositories.list(), []);
  const toast = useToast();
  async function run() {
    setBusy(true);
    const r = await lux().tasks.create(agent.slug, prompt.trim(), repo || undefined);
    setBusy(false);
    if (!r.ok) return toast(explain(r.error), true);
    onStarted(r.data.id);
  }
  return (
    <Dialog title={`Run ${agent.name}`} onClose={onClose}>
      <div className="field">
        <label htmlFor="run-prompt">Task</label>
        <textarea id="run-prompt" className="textarea" value={prompt} onChange={(e) => setPrompt(e.target.value)} placeholder="What should the agent do?" />
      </div>
      {!!repos.data?.length && (
        <div className="field">
          <label htmlFor="run-repo">Repository</label>
          <select id="run-repo" className="input" value={repo} onChange={(e) => setRepo(e.target.value)}>
            <option value="">None</option>
            {repos.data.map((r) => <option key={r.id} value={r.name}>{r.name}</option>)}
          </select>
          <span className="hint">The agent gets its own checkout. What it changes is shown for review; nothing is pushed until someone publishes it.</span>
        </div>
      )}
      <div className="foot">
        <button className="btn" onClick={onClose}>Cancel</button>
        <button className="btn primary" disabled={busy || !prompt.trim()} onClick={run}>Start task</button>
      </div>
    </Dialog>
  );
}

function ImportDialog({ onClose, onImported }: { onClose: () => void; onImported: (slug: string) => void }) {
  const [kind, setKind] = useState<"git" | "local">("git");
  const [url, setUrl] = useState("");
  const [ref, setRef] = useState("");
  const [busy, setBusy] = useState(false);
  const [report, setReport] = useState<Report | null>(null);
  const toast = useToast();
  async function submit() {
    setBusy(true);
    setReport(null);
    const r = await lux().agents.import({ sourceKind: kind, sourceUrl: url.trim(), sourceRef: ref.trim() || undefined });
    setBusy(false);
    if (!r.ok) {
      const details = r.error.details as { report?: Report } | undefined;
      if (r.error.code === "invalid_agent" && details?.report) {
        setReport(details.report);
        return;
      }
      return toast(explain(r.error), true);
    }
    toast(`Imported ${r.data.agent.name}`);
    onImported(r.data.agent.slug);
  }
  return (
    <Dialog title="Import an agent" onClose={onClose}>
      <div className="row" role="radiogroup" aria-label="Source">
        <label className="row"><input type="radio" checked={kind === "git"} onChange={() => setKind("git")} /> Git repository</label>
        <label className="row"><input type="radio" checked={kind === "local"} onChange={() => setKind("local")} /> Folder on the server</label>
      </div>
      <div className="field">
        <label htmlFor="imp-url">{kind === "git" ? "Repository URL" : "Absolute path on the server"}</label>
        <input id="imp-url" className="input" value={url} onChange={(e) => setUrl(e.target.value)} placeholder={kind === "git" ? "https://github.com/org/agent.git" : "/home/you/agents/helper"} />
        {kind === "local" && <span className="hint">Only available when the server runs in local mode. Lux uses the last commit, not uncommitted changes.</span>}
      </div>
      <div className="field">
        <label htmlFor="imp-ref">Branch or tag (optional)</label>
        <input id="imp-ref" className="input" value={ref} onChange={(e) => setRef(e.target.value)} placeholder="main" />
      </div>
      {report && (
        <div className="stack">
          <div className="notice error">The repository did not pass validation.</div>
          <Issues report={report} />
        </div>
      )}
      <div className="foot">
        <button className="btn" onClick={onClose}>Cancel</button>
        <button className="btn primary" disabled={busy || !url.trim()} onClick={submit}>{busy ? "Fetching…" : "Import"}</button>
      </div>
    </Dialog>
  );
}
