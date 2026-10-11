import { useEffect, useRef, useState, type CSSProperties, type ReactNode } from "react";
import type { Agent } from "../../shared/types";
import { lux } from "../lib/lux";
import { Dialog, explain, useToast } from "./common";
import { slugOf } from "./Mascot";

/** A dropdown that closes on Escape or a click outside it. */
export function Popover({ onClose, className = "", style, label, role = "menu", children }: {
  onClose: () => void;
  role?: string;
  className?: string;
  style?: CSSProperties;
  label: string;
  children: ReactNode;
}) {
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const close = (e: MouseEvent | KeyboardEvent) => {
      if (e instanceof KeyboardEvent ? e.key === "Escape" : !ref.current?.contains(e.target as Node)) onClose();
    };
    window.addEventListener("mousedown", close);
    window.addEventListener("keydown", close);
    return () => {
      window.removeEventListener("mousedown", close);
      window.removeEventListener("keydown", close);
    };
  }, [onClose]);
  return (
    <div ref={ref} className={`menu ${className}`} style={style} role={role} aria-label={label}>
      {children}
    </div>
  );
}

export interface AgentMenuItem {
  label: string;
  run: () => void;
  danger?: boolean;
}

/**
 * What can be done to an agent from anywhere: rename (the caller edits the
 * name in place), turn off or on, delete (after asking). `dialog` must be
 * rendered by the caller so the question outlives the menu.
 */
export function useAgentActions(onChanged: () => void, onDeleted?: (a: Agent) => void) {
  const [deleting, setDeleting] = useState<Agent | null>(null);
  const [busy, setBusy] = useState(false);
  const toast = useToast();

  async function setOff(a: Agent, off: boolean) {
    const r = await lux().agents.setDisabled(a.id, off);
    if (!r.ok) return toast(explain(r.error), true);
    toast(off ? `Turned off ${a.name}` : `Turned on ${a.name}`);
    onChanged();
  }
  async function remove(a: Agent) {
    setBusy(true);
    const r = await lux().agents.delete(a.id);
    setBusy(false);
    if (!r.ok) return toast(explain(r.error), true);
    setDeleting(null);
    toast(`Deleted ${a.name}`);
    onDeleted?.(a);
    onChanged();
  }
  async function rename(a: Agent, name: string) {
    const n = name.trim();
    if (!n || n === a.name) return true;
    const r = await lux().agents.rename(a.id, n);
    if (!r.ok) {
      toast(explain(r.error), true);
      return false;
    }
    onChanged();
    return true;
  }

  const items = (a: Agent, startRename: () => void): AgentMenuItem[] => [
    { label: "Rename", run: startRename },
    { label: a.disabled ? "Turn on" : "Turn off", run: () => void setOff(a, !a.disabled) },
    { label: "Delete", run: () => setDeleting(a), danger: true },
  ];

  const dialog = deleting && (
    <Dialog title={`Delete ${deleting.name}?`} onClose={() => setDeleting(null)}>
      <p>Its chat and routines go away and it stops running. What it already did stays in All tasks. This cannot be undone.</p>
      <div className="foot">
        <button className="btn" onClick={() => setDeleting(null)}>Cancel</button>
        <button className="btn danger" disabled={busy} onClick={() => remove(deleting)}>Delete</button>
      </div>
    </Dialog>
  );
  return { items, rename, setOff, dialog };
}

/** Menu items as buttons; picking one closes the menu first. */
export function MenuItems({ items, close }: { items: AgentMenuItem[]; close: () => void }) {
  return (
    <>
      {items.map((it) => (
        <button key={it.label} role="menuitem" className={it.danger ? "danger" : undefined} onClick={() => { close(); it.run(); }}>{it.label}</button>
      ))}
    </>
  );
}

/** A name edited in place: Enter saves, Escape or leaving the field cancels. */
export function RenameField({ value, onSave, onDone, className = "" }: {
  value: string;
  onSave: (name: string) => Promise<boolean>;
  onDone: () => void;
  className?: string;
}) {
  const [text, setText] = useState(value);
  const ref = useRef<HTMLInputElement>(null);
  useEffect(() => {
    ref.current?.select();
  }, []);
  return (
    <input ref={ref} className={`rename ${className}`} value={text} maxLength={60} aria-label="Agent name"
      onChange={(e) => setText(e.target.value)} onBlur={onDone} onClick={(e) => e.stopPropagation()}
      onKeyDown={async (e) => {
        e.stopPropagation();
        if (e.key === "Escape") onDone();
        if (e.key === "Enter" && (await onSave(text))) onDone();
      }} />
  );
}

export const PROVIDERS: { id: string; label: string; field: string; example: string }[] = [
  { id: "lyzr", label: "Lyzr", field: "Lyzr agent ID", example: "The agent ID from Lyzr Studio" },
  { id: "anthropic", label: "Anthropic", field: "Model", example: "claude-sonnet-4-5" },
  { id: "openai", label: "OpenAI", field: "Model", example: "gpt-5.4-mini" },
  { id: "ollama", label: "Ollama (on this computer)", field: "Model", example: "llama3.2" },
];

/**
 * One click makes an agent ("New agent", "New agent 2"…) with the server's
 * default model and opens its thread, where it greets the person and asks
 * what to do. With no default model yet, `needModel` shows where to pick one.
 */
export function useCreateAgent(agents: Agent[], onCreated: (a: Agent) => void, needModel: () => void) {
  const [busy, setBusy] = useState(false);
  const toast = useToast();

  async function create() {
    if (busy) return;
    setBusy(true);
    try {
      const taken = new Set(agents.map((a) => a.slug));
      for (let i = 1; i <= 50; i++) {
        const name = i === 1 ? "New agent" : `New agent ${i}`;
        if (taken.has(slugOf(name))) continue;
        const r = await lux().agents.create({ name, description: "Your personal agent", instructions: "", model: "" });
        if (r.ok) {
          await lux().agents.introduce(r.data.agent.id);
          return onCreated(r.data.agent);
        }
        if (r.error.code === "conflict") continue; // made elsewhere meanwhile
        if (r.error.code !== "no_default_model") return toast(explain(r.error), true);
        toast("Pick the model your agents use first.");
        return needModel();
      }
    } finally {
      setBusy(false);
    }
  }
  return { create, busy };
}
