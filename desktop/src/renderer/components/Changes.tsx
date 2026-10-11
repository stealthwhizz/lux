import { useState } from "react";
import type { Change } from "../../shared/types";
import { lux, plural } from "../lib/lux";
import { StateChip, explain, useToast } from "./common";

export function changeStats(c: Change) {
  const added = c.files.reduce((n, f) => n + f.added, 0);
  const deleted = c.files.reduce((n, f) => n + f.deleted, 0);
  return `${plural(c.files.length, "file")}, +${added} −${deleted}`;
}

const TITLES = { repository: "Changes to the repository", memory: "Memory proposal" };
const HINTS = {
  repository: "Publishing pushes exactly this patch to a new branch. Nothing the agent did after the run is included.",
  memory: "What the agent wrote to its memory/ and skills/ folders. Publishing pushes it to a branch of the agent's repository.",
};

/** A task's captured changes, reviewable and publishable in place. */
export function ChangesPanel({ changes, canPublish, onChanged }: { changes: Change[]; canPublish: boolean; onChanged: () => void }) {
  if (changes.length === 0) return null;
  return (
    <section aria-label="Changes" className="stack">
      {changes.map((c) => <ChangeCard key={c.target} change={c} canPublish={canPublish} onChanged={onChanged} />)}
    </section>
  );
}

function ChangeCard({ change: c, canPublish, onChanged }: { change: Change; canPublish: boolean; onChanged: () => void }) {
  const [patch, setPatch] = useState<string | null>(null);
  const [pr, setPr] = useState(false);
  const [busy, setBusy] = useState(false);
  const toast = useToast();

  async function toggleDiff() {
    if (patch !== null) return setPatch(null);
    const r = await lux().changes.patch(c.task_id, c.target);
    if (!r.ok) return toast(explain(r.error), true);
    setPatch(r.data);
  }
  async function publish() {
    setBusy(true);
    const r = await lux().changes.publish(c.task_id, c.target, pr);
    setBusy(false);
    if (!r.ok) return toast(explain(r.error), true);
    toast(r.data.pull_request_error ? `Pushed, but the pull request failed: ${r.data.pull_request_error}` : `Pushed ${r.data.change.branch}`, !!r.data.pull_request_error);
    onChanged();
  }
  async function discard() {
    setBusy(true);
    const r = await lux().changes.discard(c.task_id, c.target);
    setBusy(false);
    if (!r.ok) return toast(explain(r.error), true);
    onChanged();
  }

  return (
    <article className="change" aria-label={TITLES[c.target]}>
      <div className="row">
        <strong>{TITLES[c.target]}</strong>
        <span className="why">{changeStats(c)}</span>
        <span style={{ marginLeft: "auto" }}><StateChip state={c.state} /></span>
      </div>
      <ul className="files">
        {c.files.slice(0, 50).map((f) => (
          <li key={f.path}>
            <code>{f.path}</code> <span className="why">{f.binary ? "binary" : `+${f.added} −${f.deleted}`}</span>
          </li>
        ))}
        {c.files.length > 50 && <li className="why">and {c.files.length - 50} more</li>}
      </ul>
      {c.state === "proposed" && <p className="why">{HINTS[c.target]}</p>}
      {c.last_error && <div className="notice error">Last publish failed: {c.last_error}</div>}
      {c.state === "published" && (
        <p className="why">
          Pushed to <code>{c.branch}</code> at <code>{c.commit_sha?.slice(0, 12)}</code>
          {c.pull_request_url && <> · <a href={c.pull_request_url} onClick={(e) => { e.preventDefault(); void lux().openExternal(c.pull_request_url!); }}>Open pull request</a></>}
        </p>
      )}
      {!c.patch_available && <p className="why">The patch ({c.patch_bytes} bytes) was too large to keep, so it cannot be reviewed or published here.</p>}
      <div className="row">
        {c.patch_available && <button className="btn" onClick={toggleDiff} aria-expanded={patch !== null}>{patch === null ? "Show diff" : "Hide diff"}</button>}
        {c.state === "proposed" && canPublish && c.patch_available && (
          <>
            {c.target === "repository" && (
              <label className="row why"><input type="checkbox" checked={pr} onChange={(e) => setPr(e.target.checked)} /> Open a GitHub pull request</label>
            )}
            <button className="btn approve" disabled={busy} onClick={publish} style={{ marginLeft: "auto" }}>Publish</button>
            <button className="btn quiet" disabled={busy} onClick={discard}>Discard</button>
          </>
        )}
      </div>
      {patch !== null && <Diff patch={patch} />}
    </article>
  );
}

/** A unified diff with added and removed lines marked. */
export function Diff({ patch }: { patch: string }) {
  const lines = patch.split("\n");
  return (
    <pre className="diff" aria-label="Diff">
      {lines.map((l, i) => {
        const cls = l.startsWith("+++") || l.startsWith("---") || l.startsWith("diff ") ? "file"
          : l.startsWith("@@") ? "hunk" : l.startsWith("+") ? "add" : l.startsWith("-") ? "del" : "";
        return <span key={i} className={cls}>{l}{"\n"}</span>;
      })}
    </pre>
  );
}
