import { useEffect, useMemo, useRef, useState } from "react";

export interface Command {
  id: string;
  label: string;
  kind: string;
  run: () => void | Promise<void>;
}

export function matches(label: string, query: string): boolean {
  const q = query.trim().toLowerCase();
  if (!q) return true;
  const l = label.toLowerCase();
  // Every word of the query appears, in any order.
  return q.split(/\s+/).every((w) => l.includes(w));
}

export function CommandPalette({ commands, onClose }: { commands: Command[]; onClose: () => void }) {
  const [q, setQ] = useState("");
  const [sel, setSel] = useState(0);
  const input = useRef<HTMLInputElement>(null);
  const list = useMemo(() => commands.filter((c) => matches(c.label, q)).slice(0, 50), [commands, q]);
  useEffect(() => {
    input.current?.focus();
  }, []);
  useEffect(() => {
    setSel(0);
  }, [q]);

  const run = (c: Command | undefined) => {
    if (!c) return;
    onClose();
    void c.run();
  };

  return (
    <div className="scrim" onMouseDown={(e) => e.target === e.currentTarget && onClose()}>
      <div className="palette" role="dialog" aria-modal="true" aria-label="Search and commands">
        <input
          ref={input}
          value={q}
          placeholder="Type a command or a view"
          aria-label="Command"
          role="combobox"
          aria-expanded="true"
          aria-controls="palette-list"
          aria-activedescendant={list[sel] ? `cmd-${list[sel].id}` : undefined}
          onChange={(e) => setQ(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Escape") onClose();
            else if (e.key === "ArrowDown") {
              e.preventDefault();
              setSel((s) => Math.min(s + 1, list.length - 1));
            } else if (e.key === "ArrowUp") {
              e.preventDefault();
              setSel((s) => Math.max(s - 1, 0));
            } else if (e.key === "Enter") run(list[sel]);
          }}
        />
        <ul id="palette-list" role="listbox">
          {list.length === 0 && <li aria-disabled="true">No matching commands</li>}
          {list.map((c, i) => (
            <li key={c.id} id={`cmd-${c.id}`} role="option" aria-selected={i === sel} onMouseEnter={() => setSel(i)} onClick={() => run(c)}>
              <span>{c.label}</span>
              <span className="kind">{c.kind}</span>
            </li>
          ))}
        </ul>
      </div>
    </div>
  );
}
