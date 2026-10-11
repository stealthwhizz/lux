import { useState } from "react";
import type { Routine } from "../../shared/types";
import { lux, useLoad } from "../lib/lux";
import { ErrorNotice, explain, useToast } from "./common";

const DAYS = ["Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"];

export type Repeat = "daily" | "weekdays" | number; // a number is a weekday, 0 = Sunday

/** The cron for "every day / weekdays / every <day> at HH:MM". */
export function cronFor(repeat: Repeat, time: string): string {
  const [h, m] = time.split(":").map((x) => Number(x));
  const dow = repeat === "daily" ? "*" : repeat === "weekdays" ? "1-5" : String(repeat);
  return `${m} ${h} * * ${dow}`;
}

/** Plain words for the schedules people make here; anything else as written. */
export function describeCron(cron: string): string {
  const tz = /^CRON_TZ=(\S+) /.exec(cron)?.[1];
  const m = /^(\d+) (\d+) \* \* (\*|1-5|[0-6])$/.exec(cron.replace(/^CRON_TZ=\S+ /, ""));
  if (!m) return cron;
  const at = `${m[2].padStart(2, "0")}:${m[1].padStart(2, "0")}`;
  const when = m[3] === "*" ? "Every day" : m[3] === "1-5" ? "Weekdays" : `Every ${DAYS[Number(m[3])]}`;
  return `${when} at ${at}${tz ? ` (${tz})` : ""}`;
}

export function RoutinesPanel({ agent, agentName, onClose }: { agent: string; agentName: string; onClose: () => void }) {
  const list = useLoad<Routine[]>(() => lux().routines.list(agent), [agent]);
  const [adding, setAdding] = useState(false);
  const toast = useToast();
  async function act(p: Promise<{ ok: boolean; error?: any }>, done: string) {
    const r = await p;
    if (!r.ok) return toast(explain(r.error), true);
    toast(done);
    list.reload();
  }
  return (
    <aside className="files-panel" aria-label="Routines">
      <div className="files-head">
        <h2>Routines</h2>
        <button className="link quiet" onClick={onClose}>Close</button>
      </div>
      {list.error && <ErrorNotice error={list.error} onRetry={list.reload} />}
      <div className="files-list">
        {list.data?.length === 0 && !adding && <p className="chat-hint">{agentName} can do something for you on a schedule, like a morning briefing. Its results appear in this thread.</p>}
        {list.data?.map((r) => (
          <div key={r.id} className={`routine${r.paused ? " paused" : ""}`}>
            <strong>{r.name}</strong>
            <span className="why">{describeCron(r.cron)}{r.paused ? ", paused" : ""}</span>
            <p className="routine-prompt">{r.prompt}</p>
            {r.last_error && <span className="routine-error">Last time: {r.last_error}</span>}
            {!r.source_file && (
              <div className="row">
                <button className="btn" onClick={() => act(lux().routines.runNow(r.id), `${r.name} will start in a few seconds`)}>Run now</button>
                <button className="btn quiet" onClick={() => act(lux().routines.setPaused(r.id, !r.paused), r.paused ? "Resumed" : "Paused")}>{r.paused ? "Resume" : "Pause"}</button>
                <button className="btn quiet danger" onClick={() => act(lux().routines.remove(r.id), `Deleted ${r.name}`)}>Delete</button>
              </div>
            )}
          </div>
        ))}
        {adding ? <NewRoutine agent={agent} onDone={() => { setAdding(false); list.reload(); }} /> : (
          <button className="btn" onClick={() => setAdding(true)}>New routine</button>
        )}
      </div>
    </aside>
  );
}

function NewRoutine({ agent, onDone }: { agent: string; onDone: () => void }) {
  const [name, setName] = useState("");
  const [prompt, setPrompt] = useState("");
  const [repeat, setRepeat] = useState<Repeat>("weekdays");
  const [time, setTime] = useState("08:00");
  const [busy, setBusy] = useState(false);
  const toast = useToast();
  const timezone = Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
  async function save() {
    setBusy(true);
    const r = await lux().routines.create(agent, { name: name.trim(), prompt: prompt.trim(), cron: cronFor(repeat, time), timezone });
    setBusy(false);
    if (!r.ok) return toast(explain(r.error), true);
    toast(`Added ${r.data.name}`);
    onDone();
  }
  return (
    <div className="stack routine-form">
      <div className="field">
        <label htmlFor="rt-name">Name</label>
        <input id="rt-name" className="input" value={name} maxLength={80} onChange={(e) => setName(e.target.value)} placeholder="Morning briefing" />
      </div>
      <div className="field">
        <label htmlFor="rt-prompt">What to do</label>
        <textarea id="rt-prompt" className="textarea" rows={3} value={prompt} onChange={(e) => setPrompt(e.target.value)} placeholder="Go through my planner and tell me what today looks like." />
      </div>
      <div className="field-row">
        <div className="field grow">
          <label htmlFor="rt-repeat">When</label>
          <select id="rt-repeat" className="input" value={String(repeat)} onChange={(e) => setRepeat(/^\d$/.test(e.target.value) ? Number(e.target.value) : (e.target.value as Repeat))}>
            <option value="daily">Every day</option>
            <option value="weekdays">Weekdays</option>
            {DAYS.map((d, i) => <option key={d} value={i}>Every {d}</option>)}
          </select>
        </div>
        <div className="field">
          <label htmlFor="rt-time">At</label>
          <input id="rt-time" className="input" type="time" value={time} onChange={(e) => setTime(e.target.value)} />
        </div>
      </div>
      <span className="hint">Times are in your timezone, {timezone}.</span>
      <div className="row">
        <button className="btn primary" disabled={busy || !name.trim() || !prompt.trim() || !/^\d\d:\d\d$/.test(time)} onClick={save}>Add routine</button>
        <button className="btn quiet" onClick={onDone}>Cancel</button>
      </div>
    </div>
  );
}
