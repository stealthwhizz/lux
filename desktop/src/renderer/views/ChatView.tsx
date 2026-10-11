import { useCallback, useEffect, useRef, useState } from "react";
import type { AgentDefinition } from "../../shared/ipc";
import type { Agent, Approval, Attachment, Conversation, Message, ServerStatus } from "../../shared/types";
import type { Nav } from "../App";
import { lux, useLoad, useWorkspaceChanges } from "../lib/lux";
import { ErrorNotice, explain, useToast } from "../components/common";
import { useTaskEvents } from "../components/Timeline";
import { Mascot, mascotHue, slugOf } from "../components/Mascot";
import { Activity } from "../components/Activity";
import { AgentAvatar } from "../components/Sidebar";
import { RichText } from "../components/RichText";
import { ChevronIcon, PlusIcon, SendIcon } from "../components/Icons";
import { MenuItems, PROVIDERS, Popover, RenameField, useAgentActions, useCreateAgent } from "../components/Menu";
import { AttachmentChips, FileChips, FilesPanel } from "../components/Files";
import { ModelsList } from "../components/Models";
import { RoutinesPanel } from "../components/Routines";

/**
 * The main pane. Each agent is one ongoing thread (target = agent id);
 * "edit:<agent>" changes one.
 */
export function ChatView({ nav, agents, latest, status, pending, onChanged }: {
  nav: Nav;
  agents: Agent[];
  latest: Map<string, Conversation>;
  status?: ServerStatus;
  pending: Approval[];
  onChanged: () => void;
}) {
  const t = nav.target ?? "";
  if (t.startsWith("edit:")) {
    const agent = agents.find((a) => a.id === t.slice(5));
    return agent ? <AgentForm key={agent.id} agent={agent} status={status} onSaved={(a) => { onChanged(); nav.go("conversations", a.id); }} onCancel={() => nav.go("conversations", agent.id)} /> : null;
  }
  const agent = agents.find((a) => a.id === t);
  if (agent) return <Thread key={agent.id} agent={agent} conversationId={latest.get(agent.id)?.id} needsYou={pending.some((p) => p.agent_id === agent.id)} nav={nav} onChanged={onChanged} />;
  return agents.length ? null : <NoAgents agents={agents} nav={nav} onChanged={onChanged} />;
}

/** First run: one button; the new agent asks what to do in its chat. */
function NoAgents({ agents, nav, onChanged }: { agents: Agent[]; nav: Nav; onChanged: () => void }) {
  const [pickModel, setPickModel] = useState(false);
  const creator = useCreateAgent(agents, (a) => { onChanged(); nav.go("conversations", a.id); }, () => setPickModel(true));
  return (
    <section className="chat" aria-label="No agents yet">
      <div className="chat-body">
        <div className="hello">
          <Mascot id="new-agent" size={112} />
          <h2>Your first agent</h2>
          <p>It works for you around the clock and asks before it acts.</p>
          <button className="btn primary" disabled={creator.busy} onClick={creator.create}>{creator.busy ? "Creating…" : "Create an agent"}</button>
          {pickModel && <div className="models-inline"><ModelsList onChosen={() => { setPickModel(false); void creator.create(); }} /></div>}
        </div>
      </div>
    </section>
  );
}

// ---- edit an agent ----


function AgentForm({ agent, status, onSaved, onCancel }: { agent?: Agent; status?: ServerStatus; onSaved: (a: Agent) => void; onCancel: () => void }) {
  const existing = useLoad<AgentDefinition | null>(() => (agent && agent.source_kind === "managed" ? lux().agents.definition(agent.id) : Promise.resolve({ ok: true as const, data: null })), [agent?.id]);
  const configured = new Set((status?.model_providers ?? []).map((p) => p.name));
  const [name, setName] = useState(agent?.name ?? "");
  const [description, setDescription] = useState(agent?.description ?? "");
  const [instructions, setInstructions] = useState("");
  const [provider, setProvider] = useState(PROVIDERS.find((p) => configured.has(p.id))?.id ?? "lyzr");
  const [model, setModel] = useState("");
  const [busy, setBusy] = useState(false);
  const toast = useToast();
  useEffect(() => {
    const d = existing.data;
    if (!d) return;
    setName(agent?.name ?? d.name);
    setDescription(d.description);
    setInstructions(d.instructions);
    const [p, ...rest] = d.model.split(":");
    setProvider(p);
    setModel(rest.join(":"));
  }, [existing.data, agent?.name]);

  const prov = PROVIDERS.find((p) => p.id === provider) ?? PROVIDERS[0];
  const ready = name.trim() && description.trim() && model.trim();
  async function save() {
    setBusy(true);
    const d = { name: name.trim(), description: description.trim(), instructions, model: `${provider}:${model.trim()}` };
    const r = agent ? await lux().agents.updateDefinition(agent.id, d) : await lux().agents.create(d);
    setBusy(false);
    if (!r.ok) return toast(r.error.code === "invalid_agent" ? "The agent did not pass validation. Check the model and try again." : explain(r.error), true);
    toast(agent ? `Saved ${d.name}` : `Created ${d.name}`);
    onSaved(r.data.agent);
  }

  const imported = !!agent && agent.source_kind !== "managed";
  async function rename() {
    setBusy(true);
    const r = await lux().agents.rename(agent!.id, name.trim());
    setBusy(false);
    if (!r.ok) return toast(explain(r.error), true);
    toast(`Renamed to ${r.data.name}`);
    onSaved(r.data);
  }
  const previewSlug = agent?.slug ?? (slugOf(name) || "new-agent");
  return (
    <section className="chat" aria-label={agent ? `Edit ${agent.name}` : "Create an agent"}>
      <header className="chat-head">
        <h1>{agent ? `Edit ${agent.name}` : "Create an agent"}</h1>
      </header>
      <div className="chat-body">
        {existing.error && !imported && <ErrorNotice error={existing.error} onRetry={existing.reload} />}
        <div className="form">
          <div className="form-mascot" aria-hidden="true">
            <Mascot id={previewSlug} size={84} />
            <span>{name.trim() || "Your agent"}</span>
          </div>
          {imported && (
            <>
              <div className="field">
                <label htmlFor="ag-name">Name</label>
                <input id="ag-name" className="input" value={name} maxLength={60} onChange={(e) => setName(e.target.value)} />
                <span className="hint">{agent!.name} comes from a git repository, so its instructions and model change there. Here you can change the name people see.</span>
              </div>
              <div className="row">
                <button className="btn primary" disabled={busy || !name.trim() || name.trim() === agent!.name} onClick={rename}>Save name</button>
                <button className="btn quiet" onClick={onCancel}>Cancel</button>
              </div>
            </>
          )}
          {!imported && <>
          <div className="field">
            <label htmlFor="ag-name">Name</label>
            <input id="ag-name" className="input" value={name} maxLength={60} onChange={(e) => setName(e.target.value)} placeholder="HR crew" />
          </div>
          <div className="field">
            <label htmlFor="ag-desc">What it does</label>
            <input id="ag-desc" className="input" value={description} maxLength={300} onChange={(e) => setDescription(e.target.value)} placeholder="Answers questions about leave, payroll and onboarding" />
          </div>
          <div className="field">
            <label htmlFor="ag-instr">Instructions</label>
            <textarea id="ag-instr" className="textarea" rows={8} value={instructions} onChange={(e) => setInstructions(e.target.value)}
              placeholder="How it should work, in plain words. For example: Be brief. Ask before sending anything to anyone." />
          </div>
          <div className="field-row">
            <div className="field">
              <label htmlFor="ag-prov">Model provider</label>
              <select id="ag-prov" className="input" value={provider} onChange={(e) => setProvider(e.target.value)}>
                {PROVIDERS.map((p) => <option key={p.id} value={p.id}>{p.label}{configured.has(p.id) ? "" : " (no key on this server)"}</option>)}
              </select>
            </div>
            <div className="field grow">
              <label htmlFor="ag-model">{prov.field}</label>
              <input id="ag-model" className="input mono" value={model} onChange={(e) => setModel(e.target.value)} placeholder={prov.example} />
            </div>
          </div>
          {!configured.has(provider) && <p className="hint">This server has no {prov.label} key yet. Add it to the server's settings file (see .env.example), then restart the server.</p>}
          <div className="row">
            <button className="btn primary" disabled={busy || !ready} onClick={save}>{agent ? "Save" : "Create agent"}</button>
            <button className="btn quiet" onClick={onCancel}>Cancel</button>
          </div>
          </>}
        </div>
      </div>
    </section>
  );
}

// ---- an agent's thread ----

function Thread({ agent, conversationId, needsYou, nav, onChanged }: { agent: Agent; conversationId?: string; needsYou: boolean; nav: Nav; onChanged: () => void }) {
  const conv = useLoad<{ conversation: Conversation; messages: Message[] } | null>(
    () => (conversationId ? lux().conversations.get(conversationId) : Promise.resolve({ ok: true as const, data: null })), [conversationId]);
  const active = conv.data?.conversation.active_task_id;
  const approvals = useLoad<Approval[]>(() => lux().approvals.list("pending"), [active]);
  const { events } = useTaskEvents(active);
  const toast = useToast();
  const body = useRef<HTMLDivElement>(null);
  const [panel, setPanel] = useState<"" | "files" | "routines">("");
  const [menu, setMenu] = useState(false);
  const [renaming, setRenaming] = useState(false);
  const closeMenu = useCallback(() => setMenu(false), []);
  const actions = useAgentActions(onChanged, () => nav.go("conversations"));

  useWorkspaceChanges((taskId) => {
    if (!taskId || taskId === active || !active) {
      conv.reload();
      approvals.reload();
    }
  });
  useEffect(() => {
    body.current?.scrollTo({ top: body.current.scrollHeight });
  }, [conv.data?.messages.length, events.length]);

  async function send(content: string, attachments: string[]) {
    const r = conversationId ? await lux().conversations.send(conversationId, content, attachments) : await lux().conversations.create(agent.slug, content, attachments);
    if (!r.ok) {
      toast(explain(r.error), true);
      return false;
    }
    conv.reload();
    onChanged();
    return true;
  }
  async function stop() {
    if (!active) return;
    const r = await lux().tasks.cancel(active);
    if (!r.ok) toast(explain(r.error), true);
  }

  const msgs = conv.data?.messages ?? [];
  const state = needsYou ? "needs" : active ? "working" : "";
  return (
    <section className="chat" aria-label={agent.name} style={{ ["--hue" as string]: mascotHue(agent.slug) }}>
      <header className="chat-head">
        <div className="menu-wrap head-agent">
          {renaming ? (
            <span className="head-btn">
              <AgentAvatar slug={agent.slug} status={state} size={28} />
              <RenameField className="head-rename" value={agent.name} onSave={(n) => actions.rename(agent, n)} onDone={() => setRenaming(false)} />
            </span>
          ) : (
            <button className="head-btn" aria-expanded={menu} aria-label={`${agent.name} menu`} onClick={() => setMenu(!menu)}>
              <AgentAvatar slug={agent.slug} status={state} size={28} />
              <span className="head-text">
                <h1>{agent.name}<ChevronIcon /></h1>
                <span className={`head-status ${state}`}>{agent.disabled ? "Off" : state === "needs" ? "Needs you" : state === "working" ? "Working…" : agent.description}</span>
              </span>
            </button>
          )}
          {menu && (
            <Popover label={agent.name} className="down" onClose={closeMenu}>
              <MenuItems close={closeMenu} items={[
                { label: "Files", run: () => setPanel("files") },
                { label: "Routines", run: () => setPanel("routines") },
                { label: "Edit", run: () => nav.go("conversations", "edit:" + agent.id) },
                ...actions.items(agent, () => setRenaming(true)),
              ]} />
            </Popover>
          )}
        </div>
        {actions.dialog}
      </header>
      <div className="chat-body" ref={body} aria-live="polite">
        {conv.error && <ErrorNotice error={conv.error} onRetry={conv.reload} />}
        {(conv.data === null || msgs.length > 0) && (
          <div className={`hello${msgs.length ? " top" : ""}`}>
            <Mascot id={agent.slug} size={112} />
            <h2>{agent.name}</h2>
            <p>{agent.description}</p>
          </div>
        )}
        {msgs.map((m, i) => (
          <div key={m.id} className="turn">
            {startsGroup(msgs, i) && <div className="when">{whenLabel(m.created_at)}</div>}
            {m.role === "system" ? (
              <div className="note">{m.content}</div>
            ) : m.role === "user" ? (
              <div className="bubble you">
                {m.content}
                {!!m.attachments?.length && <AttachmentChips agent={agent.id} files={m.attachments} />}
              </div>
            ) : (
              <div className="bubble agent">
                <RichText text={splitChoices(m.content).text} />
                {m.task_id && <FileChips agent={agent.id} taskId={m.task_id} />}
                {m.task_id && <button className="link quiet small" onClick={() => nav.go("tasks", m.task_id)}>See what it did</button>}
              </div>
            )}
          </div>
        ))}
        {!active && !agent.disabled && <Choices message={msgs[msgs.length - 1]} onPick={(c) => void send(c, [])} />}
        {agent.disabled && <div className="note">{agent.name} is off. <button className="link" onClick={() => void actions.setOff(agent, false)}>Turn it on</button> to talk to it.</div>}
        {active && <Activity events={events} approvals={approvals.data ?? []} onDecided={() => { approvals.reload(); onChanged(); }} />}
      </div>
      {panel === "files" && <FilesPanel agent={agent.id} onClose={() => setPanel("")} />}
      {panel === "routines" && <RoutinesPanel agent={agent.id} agentName={agent.name} onClose={() => setPanel("")} />}
      <Composer placeholder={active ? `${agent.name} is working` : `Message ${agent.name}`} disabled={!!active || agent.disabled} onSend={send} onStop={active ? stop : undefined}
        onAttach={async () => {
          const r = await lux().files.attach(agent.id);
          if (!r.ok) {
            toast(explain(r.error), true);
            return [];
          }
          return r.data;
        }} />
    </section>
  );
}

/** The answers an agent offered as a choices block (see the runner's note), and the text without it. */
export function splitChoices(content: string): { text: string; choices: string[] } {
  const m = /\n?```choices[^\n]*\n([\s\S]*?)```\s*$/.exec(content);
  if (!m) return { text: content, choices: [] };
  const choices = m[1].split("\n").map((l) => l.replace(/^\s*(?:[-*•]|\d+[.)])\s*/, "").trim()).filter(Boolean)
    .filter((c, i, all) => c.length <= 80 && all.indexOf(c) === i).slice(0, 5);
  return { text: content.slice(0, m.index).trimEnd(), choices };
}

/** Buttons for the latest reply's choices; picking one sends it as the answer. */
function Choices({ message, onPick }: { message?: Message; onPick: (choice: string) => void }) {
  if (message?.role !== "assistant") return null;
  const { choices } = splitChoices(message.content);
  if (!choices.length) return null;
  return (
    <div className="choices" role="group" aria-label="Suggested answers">
      {choices.map((c) => <button key={c} className="choice" onClick={() => onPick(c)}>{c}</button>)}
    </div>
  );
}

/** A new time label when the conversation resumes after a pause. */
function startsGroup(msgs: Message[], i: number) {
  if (i === 0) return true;
  return Date.parse(msgs[i].created_at) - Date.parse(msgs[i - 1].created_at) > 30 * 60 * 1000;
}

export function whenLabel(iso: string, now = new Date()) {
  const d = new Date(iso);
  const time = d.toLocaleTimeString([], { hour: "numeric", minute: "2-digit" });
  const day = (x: Date) => new Date(x.getFullYear(), x.getMonth(), x.getDate()).getTime();
  const diff = Math.round((day(now) - day(d)) / 86_400_000);
  if (diff === 0) return time;
  if (diff === 1) return `Yesterday ${time}`;
  return `${d.toLocaleDateString([], { month: "short", day: "numeric" })} ${time}`;
}

// ---- composer ----

function Composer({ placeholder, disabled, onSend, onStop, onAttach }: {
  placeholder: string;
  disabled: boolean;
  onSend: (text: string, attachments: string[]) => Promise<boolean>;
  onStop?: () => void;
  onAttach?: () => Promise<Attachment[]>;
}) {
  const [text, setText] = useState("");
  const [files, setFiles] = useState<Attachment[]>([]);
  const [busy, setBusy] = useState(false);
  const area = useRef<HTMLTextAreaElement>(null);
  useEffect(() => {
    const el = area.current;
    if (!el) return;
    el.style.height = "auto";
    el.style.height = Math.min(el.scrollHeight, 180) + "px";
  }, [text]);
  async function send() {
    const t = text.trim();
    if (!t || disabled || busy) return;
    setBusy(true);
    const ok = await onSend(t, files.map((f) => f.path));
    setBusy(false);
    if (ok) {
      setText("");
      setFiles([]);
    }
  }
  async function attach() {
    if (!onAttach) return;
    setBusy(true);
    const more = await onAttach();
    setBusy(false);
    setFiles((f) => [...f, ...more].slice(0, 10));
    area.current?.focus();
  }
  return (
    <div className="composer">
      <div className="pill">
        {files.length > 0 && (
          <div className="pending">
            {files.map((f) => (
              <span key={f.path} className="file-chip static">
                <span className="file-name">{f.name}</span>
                <button className="chip-x" onClick={() => setFiles(files.filter((x) => x !== f))} aria-label={`Remove ${f.name}`}>×</button>
              </span>
            ))}
          </div>
        )}
        <div className="pill-row">
          {onAttach && <button className="attach" onClick={attach} disabled={disabled || busy} aria-label="Attach files" title="Attach files"><PlusIcon /></button>}
          <textarea ref={area} rows={1} value={text} disabled={disabled} placeholder={placeholder} aria-label="Message"
            onChange={(e) => setText(e.target.value)}
            onKeyDown={(e) => { if (e.key === "Enter" && !e.shiftKey && !e.nativeEvent.isComposing) { e.preventDefault(); void send(); } }} />
          {onStop ? (
            <button className="stop" onClick={onStop} aria-label="Stop the agent" title="Stop">■</button>
          ) : (
            <button className="send" onClick={send} disabled={disabled || busy || !text.trim()} aria-label="Send" title="Send (Enter)"><SendIcon /></button>
          )}
        </div>
      </div>
    </div>
  );
}
