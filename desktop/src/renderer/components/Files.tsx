import { useState } from "react";
import type { Attachment, FilePreview, TaskEvent, WorkspaceFile } from "../../shared/types";
import { lux, shortTime, useLoad } from "../lib/lux";
import { Dialog, ErrorNotice, explain, useToast } from "./common";
import { RichText } from "./RichText";

interface Changed { path: string; status: "added" | "modified" | "deleted"; size?: number }

function sizeLabel(n?: number) {
  if (n === undefined) return "";
  if (n < 1024) return `${n} B`;
  if (n < 1 << 20) return `${Math.round(n / 1024)} KB`;
  return `${(n / (1 << 20)).toFixed(1)} MB`;
}

/** Files a run made or changed, under the reply that made them. */
export function FileChips({ agent, taskId }: { agent: string; taskId: string }) {
  const evs = useLoad<{ events: TaskEvent[] }>(() => lux().tasks.events(taskId), [taskId]);
  const [open, setOpen] = useState<string | null>(null);
  const files = (evs.data?.events ?? [])
    .filter((e) => e.type === "lux.files")
    .flatMap((e) => (e.payload.changed as Changed[]) ?? [])
    .filter((f) => f.status !== "deleted");
  if (files.length === 0) return null;
  return (
    <div className="chips">
      {files.map((f) => (
        <button key={f.path} className="file-chip" onClick={() => setOpen(f.path)} title={`${f.status === "added" ? "New" : "Changed"}: ${f.path}`}>
          <span className="file-ext" aria-hidden="true">{ext(f.path)}</span>
          <span className="file-name">{f.path.split("/").pop()}</span>
          <span className="file-size">{sizeLabel(f.size)}</span>
        </button>
      ))}
      {open && <FilePreviewDialog agent={agent} path={open} onClose={() => setOpen(null)} />}
    </div>
  );
}

/** Files a person attached to a message. */
export function AttachmentChips({ agent, files }: { agent: string; files: Attachment[] }) {
  const [open, setOpen] = useState<string | null>(null);
  return (
    <div className="chips">
      {files.map((f) => (
        <button key={f.path} className="file-chip" onClick={() => setOpen(f.path)} title={f.name}>
          <span className="file-ext" aria-hidden="true">{ext(f.name)}</span>
          <span className="file-name">{f.name}</span>
          <span className="file-size">{sizeLabel(f.size)}</span>
        </button>
      ))}
      {open && <FilePreviewDialog agent={agent} path={open} onClose={() => setOpen(null)} />}
    </div>
  );
}

/** Everything an agent keeps in its workspace, newest first. */
export function FilesPanel({ agent, onClose }: { agent: string; onClose: () => void }) {
  const files = useLoad<WorkspaceFile[]>(() => lux().files.list(agent), [agent]);
  const [open, setOpen] = useState<string | null>(null);
  return (
    <aside className="files-panel" aria-label="Files">
      <div className="files-head">
        <h2>Files</h2>
        <button className="link quiet" onClick={onClose}>Close</button>
      </div>
      {files.error && <ErrorNotice error={files.error} onRetry={files.reload} />}
      {files.data?.length === 0 && <p className="chat-hint">Nothing yet. Files the agent makes for you appear here.</p>}
      <div className="files-list">
        {files.data?.map((f) => (
          <button key={f.path} className="file-row" onClick={() => setOpen(f.path)}>
            <span className="file-ext" aria-hidden="true">{ext(f.path)}</span>
            <span className="file-name">{f.path}</span>
            <span className="file-size">{shortTime(f.modified)}</span>
          </button>
        ))}
      </div>
      {open && <FilePreviewDialog agent={agent} path={open} onClose={() => setOpen(null)} />}
    </aside>
  );
}

function FilePreviewDialog({ agent, path, onClose }: { agent: string; path: string; onClose: () => void }) {
  const preview = useLoad<FilePreview>(() => lux().files.preview(agent, path), [agent, path]);
  const toast = useToast();
  async function save() {
    const r = await lux().files.save(agent, path);
    if (!r.ok) return toast(explain(r.error), true);
    if (r.data.saved) toast(`Saved ${path.split("/").pop()}`);
  }
  const p = preview.data;
  return (
    <Dialog title={path.split("/").pop() ?? path} onClose={onClose}>
      {preview.error && <ErrorNotice error={preview.error} onRetry={preview.reload} />}
      <div className="file-preview">
        {p?.kind === "text" && (/\.(md|markdown)$/i.test(path) ? <RichText text={p.text} /> : <pre>{p.text}</pre>)}
        {p?.kind === "image" && <img src={p.dataUrl} alt={path} />}
        {p?.kind === "none" && <p className="chat-hint">No preview for this kind of file ({sizeLabel(p.size)}). Save a copy to open it.</p>}
      </div>
      <div className="foot">
        <button className="btn" onClick={onClose}>Close</button>
        <button className="btn primary" onClick={save}>Save a copy</button>
      </div>
    </Dialog>
  );
}

function ext(p: string) {
  const m = /\.([a-z0-9]{1,4})$/i.exec(p);
  return (m ? m[1] : "file").toUpperCase().slice(0, 4);
}
