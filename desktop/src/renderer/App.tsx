import { useCallback, useEffect, useMemo, useState } from "react";
import type { Agent, Approval, Change, Conversation, Me, ProfilesState, ServerStatus } from "../shared/types";
import { lux, startWorkspaceFeed, stopWorkspaceFeed, useLoad, useWorkspaceChanges } from "./lib/lux";
import { ToastProvider } from "./components/common";
import { useNotifications } from "./lib/notify";
import { CommandPalette, type Command } from "./components/CommandPalette";
import { ChatView } from "./views/ChatView";
import { Sidebar, latestByAgent } from "./components/Sidebar";
import { TasksView } from "./views/TasksView";
import { ApprovalsView } from "./views/ApprovalsView";
import { AgentsView } from "./views/AgentsView";
import { SettingsView } from "./views/SettingsView";

export type View = "now" | "conversations" | "tasks" | "approvals" | "agents" | "settings";

const NAV: { id: View; label: string; key: string }[] = [
  { id: "conversations", label: "Conversations", key: "1" },
  { id: "approvals", label: "Needs you", key: "2" },
  { id: "tasks", label: "All tasks", key: "3" },
  { id: "agents", label: "Agents", key: "4" },
];

export interface Nav {
  go: (v: View, target?: string) => void;
  target?: string;
}

function applyTheme(t: string) {
  if (t === "system") delete document.documentElement.dataset.theme;
  else document.documentElement.dataset.theme = t;
}

export function App() {
  const [view, setView] = useState<View>("conversations");
  const [target, setTarget] = useState<string | undefined>();
  const [palette, setPalette] = useState(false);
  const [theme, setTheme] = useState<string>(() => {
    try {
      return localStorage.getItem("lux.theme") ?? "dark";
    } catch {
      return "dark";
    }
  });
  const [feedUp, setFeedUp] = useState(true);
  const profiles = useLoad<ProfilesState>(() => lux().profiles.list(), []);
  const current = profiles.data?.profiles.find((p) => p.name === profiles.data?.current);
  const status = useLoad<ServerStatus>(() => lux().server.status(), [profiles.data?.current]);
  const signedIn = !!current?.signedIn;
  const none = <T,>(data: T) => Promise.resolve({ ok: true as const, data });
  const pending = useLoad<Approval[]>(() => (signedIn ? lux().approvals.list("pending") : none([])), [profiles.data?.current, signedIn]);
  const review = useLoad<Change[]>(() => (signedIn ? lux().changes.queue("proposed") : none([])), [profiles.data?.current, signedIn]);
  const convs = useLoad<Conversation[]>(() => (signedIn ? lux().conversations.list() : none([])), [profiles.data?.current, signedIn]);
  const agents = useLoad<Agent[]>(() => (signedIn ? lux().agents.list() : none([])), [profiles.data?.current, signedIn]);
  const me = useLoad<Me | undefined>(() => (signedIn ? lux().server.me() : none(undefined)), [profiles.data?.current, signedIn]);

  useEffect(() => {
    applyTheme(theme);
  }, [theme]);

  // One live feed per connection; views refetch when it reports changes.
  useEffect(() => {
    if (!current?.signedIn || status.error) return;
    startWorkspaceFeed(setFeedUp);
    return () => stopWorkspaceFeed();
  }, [current?.name, current?.signedIn, status.error]);
  useWorkspaceChanges(() => {
    pending.reload();
    review.reload();
    convs.reload();
    agents.reload();
  });

  const go = useCallback((v: View, t?: string) => {
    setView(v);
    setTarget(t);
  }, []);

  // System notifications for approvals and replies; clicking one opens the agent.
  useNotifications(pending.data, convs.data);
  useEffect(() => {
    const stop = lux().events.onEvent((m) => {
      if (m.subscription === "notify" && m.kind === "open") go("conversations", (m.data as { agent: string }).agent);
    });
    return stop;
  }, [go]);

  // Keyboard: Ctrl/Cmd+K palette, Ctrl/Cmd+1..5 views, Ctrl/Cmd+, settings.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const mod = e.metaKey || e.ctrlKey;
      if (!mod) return;
      if (e.key === "k") {
        e.preventDefault();
        setPalette((p) => !p);
      } else if (e.key === ",") {
        e.preventDefault();
        go("settings");
      } else {
        const n = NAV.find((x) => x.key === e.key);
        if (n) {
          e.preventDefault();
          go(n.id);
        }
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [go]);

  const reloadAll = () => {
    profiles.reload();
    status.reload();
    pending.reload();
    review.reload();
    convs.reload();
    agents.reload();
    me.reload();
  };

  const commands: Command[] = useMemo(() => {
    const cmds: Command[] = NAV.map((n) => ({ id: "go-" + n.id, label: `Go to ${n.label}`, kind: "View", run: () => go(n.id) }));
    cmds.push({ id: "go-settings", label: "Open settings", kind: "View", run: () => go("settings") });
    cmds.push({ id: "import", label: "Import an agent", kind: "Action", run: () => go("agents", "import") });
    for (const p of profiles.data?.profiles ?? []) {
      if (p.name !== profiles.data?.current) {
        cmds.push({ id: "profile-" + p.name, label: `Switch to ${p.name} (${p.url})`, kind: "Server", run: async () => {
          await lux().profiles.select(p.name);
          reloadAll();
        } });
      }
    }
    for (const t of ["system", "light", "dark"]) {
      cmds.push({ id: "theme-" + t, label: `Use ${t} theme`, kind: "Appearance", run: () => {
        setTheme(t);
        try {
          localStorage.setItem("lux.theme", t);
        } catch {
          /* not persisted */
        }
      } });
    }
    return cmds;
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [profiles.data, go]);

  const needsYou = (pending.data?.length ?? 0) + (review.data?.length ?? 0);
  const unreachable = status.error?.code === "unreachable";
  const nav: Nav = { go, target };
  const needsSignIn = profiles.data && current && !current.signedIn;

  // Each agent is one thread. With none chosen, open the most recently active one.
  const latest = latestByAgent(convs.data ?? []);
  const mostRecent = [...latest.values()].sort((a, b) => Date.parse(b.updated_at) - Date.parse(a.updated_at))[0]?.agent_id;
  const chatTarget = target ?? mostRecent ?? (convs.data && agents.data ? (agents.data.length ? agents.data[0].id : "") : undefined);
  let body;
  if (view === "settings" || needsSignIn) body = <SettingsView profiles={profiles} status={status} onChange={reloadAll} theme={theme} setTheme={setTheme} />;
  else if (view === "tasks") body = <TasksView nav={nav} />;
  else if (view === "approvals") body = <ApprovalsView onChange={() => { pending.reload(); review.reload(); }} nav={nav} />;
  else if (view === "agents") body = <AgentsView nav={nav} />;
  else if (chatTarget !== undefined) body = <ChatView nav={{ go, target: chatTarget }} agents={agents.data ?? []} latest={latest} status={status.data} pending={pending.data ?? []}
    onChanged={() => { convs.reload(); agents.reload(); }} />;

  return (
    <ToastProvider>
      <div className="shell">
        <Sidebar agents={agents.data ?? []} conversations={convs.data ?? []} pending={pending.data ?? []} selected={chatTarget} needsYou={needsYou} me={me.data} online={!!status.data && !status.error}
          view={view} go={go} openPalette={() => setPalette(true)} onChanged={() => { convs.reload(); agents.reload(); }} />
        <main className="main">
          {unreachable && (
            <div className="banner error" role="alert">
              <span>Can't reach {current?.name} at {current?.url}. Agents running there keep going; this app just can't see them right now.</span>
              <span className="spacer" />
              <button className="btn" onClick={reloadAll}>Try again</button>
              <button className="btn" onClick={() => go("settings")}>Server settings</button>
            </div>
          )}
          {!unreachable && !feedUp && current?.signedIn && (
            <div className="banner warn" role="status">Live updates paused; reconnecting.</div>
          )}
          {status.data?.components && status.data.components.orchestrator !== "running" && !unreachable && (
            <div className="banner warn" role="status">
              This server is not running agents right now (orchestrator: {status.data.components.orchestrator}). Messages will wait in the queue.
            </div>
          )}
          {body}
        </main>
      </div>
      {palette && <CommandPalette commands={commands} onClose={() => setPalette(false)} />}
    </ToastProvider>
  );
}
