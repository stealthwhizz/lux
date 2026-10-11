// Access to the preload bridge, plus small React helpers built on it.
import { useCallback, useEffect, useRef, useState } from "react";
import type { DesktopBridge } from "../../preload/bridge";
import type { LuxError, PushedEvent, Result } from "../../shared/ipc";

declare global {
  interface Window {
    lux: DesktopBridge;
  }
}

export const lux = (): DesktopBridge => window.lux;

export interface Loadable<T> {
  data: T | undefined;
  error: LuxError | undefined;
  loading: boolean;
  reload: () => void;
}

/** Load data through the bridge and reload on demand or when deps change. */
export function useLoad<T>(fn: () => Promise<Result<T>>, deps: unknown[]): Loadable<T> {
  const [state, setState] = useState<{ data?: T; error?: LuxError; loading: boolean }>({ loading: true });
  const [n, setN] = useState(0);
  const fnRef = useRef(fn);
  fnRef.current = fn;
  useEffect(() => {
    let live = true;
    setState((s) => ({ ...s, loading: true }));
    fnRef.current().then((r) => {
      if (!live) return;
      setState(r.ok ? { data: r.data, loading: false } : { data: undefined, error: r.error, loading: false });
    });
    return () => {
      live = false;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...deps, n]);
  const reload = useCallback(() => setN((x) => x + 1), []);
  return { data: state.data, error: state.error, loading: state.loading, reload };
}

// ---- workspace change feed ----
// One subscription for the whole app; views register to be told when
// anything in the workspace changed, then refetch what they show.

type ChangeListener = (taskId?: string) => void;
const listeners = new Set<ChangeListener>();
let workspaceSub: string | null = null;
let unlisten: (() => void) | null = null;

export function startWorkspaceFeed(onStatus: (connected: boolean) => void) {
  stopWorkspaceFeed();
  const early: PushedEvent[] = [];
  const handle = (msg: PushedEvent) => {
    if (msg.kind === "status") {
      onStatus(!!(msg.data as any)?.connected);
      return;
    }
    if (msg.kind === "event") {
      const d = msg.data as { event: string; data: any };
      const taskId = d.event === "task_updated" ? d.data?.task_id : undefined;
      for (const l of listeners) l(taskId);
    }
  };
  // Hold messages that arrive before we know our subscription id.
  unlisten = lux().events.onEvent((msg: PushedEvent) => {
    if (workspaceSub === null) early.push(msg);
    else if (msg.subscription === workspaceSub) handle(msg);
  });
  lux().events.subscribeWorkspace().then((r) => {
    if (!r.ok) {
      onStatus(false);
      return;
    }
    workspaceSub = r.data;
    for (const m of early.splice(0)) if (m.subscription === workspaceSub) handle(m);
  });
}

export function stopWorkspaceFeed() {
  if (workspaceSub) void lux().events.unsubscribe(workspaceSub);
  workspaceSub = null;
  unlisten?.();
  unlisten = null;
}

/** Call `fn` whenever the workspace changes (debounced). */
export function useWorkspaceChanges(fn: (taskId?: string) => void) {
  const ref = useRef(fn);
  ref.current = fn;
  useEffect(() => {
    let timer: ReturnType<typeof setTimeout> | undefined;
    const l: ChangeListener = (id) => {
      clearTimeout(timer);
      timer = setTimeout(() => ref.current(id), 250);
    };
    listeners.add(l);
    return () => {
      clearTimeout(timer);
      listeners.delete(l);
    };
  }, []);
}

export function timeAgo(iso?: string | null): string {
  if (!iso) return "";
  const d = (Date.now() - Date.parse(iso)) / 1000;
  if (d < 45) return "just now";
  if (d < 3600) return `${Math.round(d / 60)} min ago`;
  if (d < 86400) return `${Math.round(d / 3600)} h ago`;
  return new Date(iso).toLocaleDateString();
}

/** Clock time today, "Yesterday", a weekday this week, else a short date. */
export function shortTime(iso: string, now = new Date()): string {
  const d = new Date(iso);
  const day = (x: Date) => new Date(x.getFullYear(), x.getMonth(), x.getDate()).getTime();
  const diff = Math.round((day(now) - day(d)) / 86_400_000);
  if (diff <= 0) return d.toLocaleTimeString([], { hour: "numeric", minute: "2-digit" });
  if (diff === 1) return "Yesterday";
  if (diff < 7) return d.toLocaleDateString([], { weekday: "long" });
  return d.toLocaleDateString([], { month: "short", day: "numeric" });
}

export function plural(n: number, one: string, many = one + "s") {
  return `${n} ${n === 1 ? one : many}`;
}
