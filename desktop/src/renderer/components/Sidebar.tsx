import { useCallback, useState } from "react";
import type { Agent, Approval, Conversation, Me } from "../../shared/types";
import type { View } from "../App";
import { shortTime } from "../lib/lux";
import { Mascot, mascotHue } from "./Mascot";
import { MenuItems, Popover, RenameField, useAgentActions, useCreateAgent } from "./Menu";
import { ConnectorsMenu } from "../views/ConnectorsView";
import { ModelsMenu } from "./Models";
import { MoreIcon, PlusIcon, SearchIcon } from "./Icons";

/** Each agent is one ongoing thread; this is its latest message. */
export function latestByAgent(conversations: Conversation[]) {
  const m = new Map<string, Conversation>();
  for (const c of conversations) {
    const prev = m.get(c.agent_id);
    if (!prev || Date.parse(c.updated_at) > Date.parse(prev.updated_at)) m.set(c.agent_id, c);
  }
  return m;
}

export function Sidebar({ agents, conversations, pending, selected, needsYou, me, online, view, go, openPalette, onChanged }: {
  agents: Agent[];
  pending: Approval[];
  conversations: Conversation[];
  selected?: string;
  needsYou: number;
  me?: Me;
  online: boolean;
  view: View;
  go: (v: View, target?: string) => void;
  openPalette: () => void;
  onChanged: () => void;
}) {
  const [q, setQ] = useState("");
  const [menu, setMenu] = useState<"" | "user" | "connectors" | "models" | "new">("");
  const [ctx, setCtx] = useState<{ agent: Agent; x: number; y: number } | null>(null);
  const [renaming, setRenaming] = useState("");
  const actions = useAgentActions(onChanged, (a) => a.id === selected && go("conversations"));
  const [createAfterModel, setCreateAfterModel] = useState(false);
  const creator = useCreateAgent(agents, (a) => { onChanged(); go("conversations", a.id); }, () => { setCreateAfterModel(true); setMenu("models"); });
  const latest = latestByAgent(conversations);
  const when = (a: Agent) => Date.parse(latest.get(a.id)?.updated_at ?? a.last_task_at ?? "") || 0;
  const term = q.trim().toLowerCase();
  const rows = agents
    .filter((a) => !term || [a.name, a.description, latest.get(a.id)?.last_message?.content ?? ""].some((s) => s.toLowerCase().includes(term)))
    .sort((x, y) => Number(x.disabled) - Number(y.disabled) || when(y) - when(x) || x.name.localeCompare(y.name));
  const closeMenu = useCallback(() => setMenu(""), []);
  const closeCtx = useCallback(() => setCtx(null), []);

  return (
    <nav className="side" aria-label="Agents">
      <div className="side-top">
        <span className="brand">Lux</span>
        <div className="menu-wrap">
          <button className="icon-btn" onClick={() => setMenu(menu === "new" ? "" : "new")} aria-label="New message" title="New message" aria-expanded={menu === "new"}>
            <PlusIcon />
          </button>
          {menu === "new" && <NewChatMenu agents={agents} go={go} onClose={closeMenu} onCreate={creator.create} />}
        </div>
      </div>
      <label className="search">
        <SearchIcon />
        <input value={q} onChange={(e) => setQ(e.target.value)} placeholder="Search" aria-label="Search agents" />
      </label>
      <div className="side-list">
        {needsYou > 0 && (
          <button className="side-row needs" aria-current={view === "approvals" ? "page" : undefined} onClick={() => go("approvals")}>
            <span className="lamp-dot" aria-hidden="true" />
            <span className="side-name">Needs you</span>
            <span className="side-count">{needsYou}</span>
          </button>
        )}
        {rows.map((a) => {
          const c = latest.get(a.id);
          const status = pending.some((p) => p.agent_id === a.id) ? "needs" : c?.active_task_id ? "working" : "";
          const openCtx = (x: number, y: number) => setCtx({ agent: a, x, y });
          const Row = renaming === a.id ? "div" : "button";
          return (
            <div key={a.id} className={`side-item${a.disabled ? " off" : ""}`}
              onContextMenu={(e) => { e.preventDefault(); openCtx(e.clientX, e.clientY); }}>
              {/* While renaming, the row is not a button: the name field takes the keys. */}
              <Row className="side-row chat" aria-current={view === "conversations" && a.id === selected ? "page" : undefined}
                onClick={renaming === a.id ? undefined : () => go("conversations", a.id)}>
                <AgentAvatar slug={a.slug} status={status} size={38} />
                <span className="side-text">
                  <span className="side-line">
                    {renaming === a.id
                      ? <RenameField value={a.name} onSave={(n) => actions.rename(a, n)} onDone={() => setRenaming("")} />
                      : <span className="side-name">{a.name}</span>}
                    {c && <span className="side-time">{shortTime(c.updated_at)}</span>}
                  </span>
                  {(a.disabled || status) && (
                    <span className={`side-preview ${status}`}>{a.disabled ? "Off" : status === "needs" ? "Needs you" : "Working…"}</span>
                  )}
                </span>
              </Row>
              <button className="row-more icon-btn" aria-label={`More for ${a.name}`} title="More"
                onClick={(e) => { const r = e.currentTarget.getBoundingClientRect(); openCtx(r.right - 180, r.bottom + 4); }}>
                <MoreIcon />
              </button>
            </div>
          );
        })}
        {agents.length > 0 && rows.length === 0 && <p className="side-empty">No agent matches “{q}”.</p>}
        {agents.length === 0 && <p className="side-empty">No agents yet.</p>}
      </div>
      {ctx && (
        <Popover label={ctx.agent.name} className="at" style={{ left: Math.min(ctx.x, window.innerWidth - 200), top: Math.min(ctx.y, window.innerHeight - 140) }} onClose={closeCtx}>
          <MenuItems close={closeCtx} items={actions.items(ctx.agent, () => setRenaming(ctx.agent.id))} />
        </Popover>
      )}
      {actions.dialog}
      <div className="side-foot">
        <span className="me-chip" aria-hidden="true">{initials(me?.username)}</span>
        <span className="me-name">{me?.username ?? "Not signed in"}</span>
        <span className={`dot ${online ? "up" : "down"}`} title={online ? "Connected" : "Can't reach the server"} />
        <div className="menu-wrap">
          <button className="icon-btn" aria-label="More" aria-expanded={menu === "user"} onClick={() => setMenu(menu === "user" ? "" : "user")}><MoreIcon /></button>
          {menu === "user" && (
            <Popover label="More" onClose={closeMenu}>
              <MenuItems close={closeMenu} items={[
                { label: "Models", run: () => setMenu("models") },
                { label: "Connectors", run: () => setMenu("connectors") },
                { label: "Agents", run: () => go("agents") },
                { label: "All tasks", run: () => go("tasks") },
                { label: "Needs you", run: () => go("approvals") },
                { label: "Settings", run: () => go("settings") },
                { label: "Search everything", run: openPalette },
              ]} />
            </Popover>
          )}
          {menu === "connectors" && <ConnectorsMenu onClose={closeMenu} />}
          {menu === "models" && <ModelsMenu onClose={() => { setCreateAfterModel(false); closeMenu(); }}
            onChosen={createAfterModel ? () => { setCreateAfterModel(false); closeMenu(); void creator.create(); } : undefined} />}
        </div>
      </div>
    </nav>
  );
}

/** The + dropdown: start talking to an agent, or make a new one. */
function NewChatMenu({ agents, go, onClose, onCreate }: { agents: Agent[]; go: (v: View, target?: string) => void; onClose: () => void; onCreate: () => void }) {
  const [query, setQuery] = useState("");
  const term = query.trim().toLowerCase();
  const usable = agents.filter((a) => !a.disabled && a.revision);
  const matches = usable.filter((a) => !term || a.name.toLowerCase().includes(term) || a.description.toLowerCase().includes(term));
  const pick = (t: string, v: View = "conversations") => { onClose(); go(v, t); };
  return (
    <Popover label="New message" role="dialog" className="down wide" onClose={onClose}>
      <input autoFocus className="menu-search" value={query} onChange={(e) => setQuery(e.target.value)} placeholder="Find an agent" aria-label="Agent"
        onKeyDown={(e) => { if (e.key === "Enter" && matches[0]) pick(matches[0].id); }} />
      <div role="listbox" aria-label="Agents" className="menu-list">
        <button role="option" aria-selected={false} className="menu-pick" onClick={() => { onClose(); onCreate(); }}>
          <span className="pick-plus" aria-hidden="true">+</span>Create an agent
        </button>
        {matches.map((a) => (
          <button key={a.id} role="option" aria-selected={false} className="menu-pick" onClick={() => pick(a.id)}>
            <Mascot id={a.slug} size={22} /><span className="pick-name">{a.name}</span>
          </button>
        ))}
        {usable.length > 0 && matches.length === 0 && <p className="menu-empty">No agent matches “{query}”.</p>}
      </div>
      <button className="menu-foot" onClick={() => pick("import", "agents")}>Import from a git repository</button>
    </Popover>
  );
}

/** A mascot in a ring of its own colour: turning while the agent works, amber when it needs you. */
export function AgentAvatar({ slug, status, size }: { slug: string; status: string; size: number }) {
  return (
    <span className={`avatar ${status}`} style={{ ["--hue" as string]: mascotHue(slug), width: size + 8, height: size + 8 }}
      title={status === "needs" ? "Needs you" : status === "working" ? "Working" : undefined}>
      <Mascot id={slug} size={size} />
    </span>
  );
}

function initials(name?: string) {
  if (!name) return "?";
  const parts = name.split(/[\s._-]+/).filter(Boolean);
  return ((parts[0]?.[0] ?? "") + (parts[1]?.[0] ?? parts[0]?.[1] ?? "")).toUpperCase();
}
