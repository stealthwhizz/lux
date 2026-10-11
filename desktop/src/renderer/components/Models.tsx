import { useState } from "react";
import type { ModelChoice, ModelProviders } from "../../shared/types";
import { lux, useLoad } from "../lib/lux";
import { ErrorNotice, explain, useToast } from "./common";
import { PROVIDERS, Popover } from "./Menu";

const label = (name: string) => PROVIDERS.find((p) => p.id === name)?.label.replace(/ \(.*\)$/, "") ?? name;

/** The Models dropdown, opened from the profile menu. */
export function ModelsMenu({ onClose, onChosen }: { onClose: () => void; onChosen?: () => void }) {
  return (
    <Popover label="Models" role="dialog" className="wide models-menu" onClose={onClose}>
      <ModelsList onChosen={onChosen} />
    </Popover>
  );
}

/**
 * Model providers: an administrator adds a key, sees the models it can use
 * as buttons, and picks the one new agents use. Keys stay on the Lux server.
 */
export function ModelsList({ onChosen }: { onChosen?: () => void }) {
  const list = useLoad<ModelProviders>(() => lux().models.list(), []);
  const [open, setOpen] = useState("");
  const d = list.data;
  return (
    <>
      <div className="menu-head">
        <strong>Models</strong>
        <span className="why">What your agents think with. Keys stay on the Lux server; agents never see them.</span>
      </div>
      {list.error && <ErrorNotice error={list.error} onRetry={list.reload} />}
      {d?.providers.map((p) => (
        <Provider key={p.name} p={p} canSet={d.can_set} current={d.default} open={open === p.name}
          toggle={() => setOpen(open === p.name ? "" : p.name)} onChange={list.reload} onChosen={onChosen} />
      ))}
      {d && <p className="menu-note">New agents use: <strong>{d.default ? d.default.replace(":", " · ") : "nothing yet, pick a model"}</strong></p>}
    </>
  );
}

function Provider({ p, canSet, current, open, toggle, onChange, onChosen }: {
  p: ModelProviders["providers"][number];
  canSet: boolean;
  current: string;
  open: boolean;
  toggle: () => void;
  onChange: () => void;
  onChosen?: () => void;
}) {
  const [editing, setEditing] = useState(false);
  const [key, setKey] = useState("");
  const [busy, setBusy] = useState(false);
  const [models, setModels] = useState<ModelChoice[] | null>(null);
  const [error, setError] = useState("");
  const toast = useToast();
  const status = p.keyless ? "On this computer" : p.source === "server" ? "Key set in the server file" : p.ready ? "Key saved" : "No key";

  async function show() {
    toggle();
    if (open || models) return;
    setError("");
    const r = await lux().models.available(p.name);
    if (r.ok) setModels(r.data);
    else setError(explain(r.error));
  }
  async function save() {
    setBusy(true);
    const r = await lux().models.setKey(p.name, key.trim());
    setBusy(false);
    if (!r.ok) return toast(r.error.code === "key_rejected" ? `${label(p.name)} did not accept that key.` : explain(r.error), true);
    setKey("");
    setEditing(false);
    setModels(r.data);
    if (!open) toggle();
    onChange();
  }
  async function remove() {
    const r = await lux().models.removeKey(p.name);
    if (!r.ok) return toast(explain(r.error), true);
    setModels(null);
    if (open) toggle();
    onChange();
  }
  async function choose(m: ModelChoice) {
    const r = await lux().settings.setDefaultModel(`${p.name}:${m.id}`);
    if (!r.ok) return toast(explain(r.error), true);
    toast(`New agents use ${m.name}`);
    onChange();
    onChosen?.();
  }

  return (
    <div className={`model-provider${open ? " open" : ""}`}>
      <div className="connector-row">
        <span className="connector-mark" aria-hidden="true">{label(p.name)[0]}</span>
        <span className="connector-title">
          <span className="connector-name">{label(p.name)}</span>
          <span className="why">{status}</span>
        </span>
        <span className="row">
          {p.ready && <button className="btn quiet small" aria-expanded={open} onClick={show}>{open ? "Hide" : "Models"}</button>}
          {canSet && !p.keyless && p.source !== "server" && (
            p.ready
              ? <button className="btn quiet small" onClick={remove}>Remove</button>
              : <button className="btn primary small" onClick={() => setEditing(!editing)}>Add key</button>
          )}
        </span>
      </div>
      {editing && (
        <div className="key-row">
          <input className="input mono" type="password" autoComplete="off" autoFocus value={key} onChange={(e) => setKey(e.target.value)}
            placeholder={`${label(p.name)} API key`} aria-label={`${label(p.name)} API key`}
            onKeyDown={(e) => { if (e.key === "Enter" && key.trim().length >= 8 && !busy) void save(); }} />
          <button className="btn primary small" disabled={busy || key.trim().length < 8} onClick={save}>{busy ? "Checking…" : "Save"}</button>
        </div>
      )}
      {open && (
        <div className="choices model-choices" role="group" aria-label={`${label(p.name)} models`}>
          {error && <span className="connector-error">{error}</span>}
          {!models && !error && <span className="why">Asking {label(p.name)}…</span>}
          {models?.length === 0 && <span className="why">This key has no models.</span>}
          {models?.map((m) => {
            const on = current === `${p.name}:${m.id}`;
            return (
              <button key={m.id} className={`choice${on ? " on" : ""}`} aria-pressed={on} disabled={!canSet} title={m.id} onClick={() => choose(m)}>{m.name}</button>
            );
          })}
        </div>
      )}
    </div>
  );
}
