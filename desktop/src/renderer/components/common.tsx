import { createContext, useCallback, useContext, useEffect, useRef, useState, type ReactNode } from "react";
import type { LuxError } from "../../shared/ipc";

const STATE_LABELS: Record<string, string> = {
  queued: "Queued",
  starting: "Starting",
  running: "Running",
  waiting_approval: "Needs approval",
  cancelling: "Cancelling",
  succeeded: "Done",
  failed: "Failed",
  cancelled: "Cancelled",
  timed_out: "Timed out",
  pending: "Waiting for you",
  approved: "Approved",
  rejected: "Rejected",
  expired: "Expired",
  proposed: "Waiting for review",
  publishing: "Publishing",
  published: "Published",
  discarded: "Discarded",
};

export function StateChip({ state }: { state: string }) {
  return <span className={`chip ${state}`}>{STATE_LABELS[state] ?? state}</span>;
}

export function ErrorNotice({ error, onRetry }: { error: LuxError; onRetry?: () => void }) {
  return (
    <div className="notice error" role="alert">
      <div className="row">
        <span>{explain(error)}</span>
        {onRetry && (
          <button className="btn quiet" onClick={onRetry} style={{ marginLeft: "auto" }}>
            Try again
          </button>
        )}
      </div>
    </div>
  );
}

/** Turn an API error into a sentence that says what to do. */
export function explain(e: LuxError): string {
  switch (e.code) {
    case "unreachable":
      return `${e.message}. Check that the server is running.`;
    case "unauthenticated":
      return "This server did not accept your sign-in. Sign in again in Settings.";
    case "forbidden":
      return e.message || "Your role in this workspace does not allow this.";
    case "maker_checker":
      return e.message ? e.message[0].toUpperCase() + e.message.slice(1) + "." : "You started this task, so someone else has to decide.";
    case "policy_denied":
      return e.message;
    case "turn_in_progress":
      return "The agent is still working on your last message. Wait for it to finish or cancel it.";
    default:
      return e.message || "Something went wrong.";
  }
}

export function Empty({ title, children }: { title: string; children?: ReactNode }) {
  return (
    <div className="empty">
      <h2>{title}</h2>
      {children}
    </div>
  );
}

// ---- toasts ----
type Toast = { text: string; error?: boolean } | null;
const ToastCtx = createContext<(text: string, error?: boolean) => void>(() => {});

export function ToastProvider({ children }: { children: ReactNode }) {
  const [toast, setToast] = useState<Toast>(null);
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined);
  const show = useCallback((text: string, error = false) => {
    setToast({ text, error });
    clearTimeout(timer.current);
    timer.current = setTimeout(() => setToast(null), error ? 6000 : 3000);
  }, []);
  return (
    <ToastCtx.Provider value={show}>
      {children}
      <div aria-live="polite" className="visually-hidden">
        {toast?.text}
      </div>
      {toast && <div className={`toast${toast.error ? " error" : ""}`}>{toast.text}</div>}
    </ToastCtx.Provider>
  );
}

export const useToast = () => useContext(ToastCtx);

// ---- dialog ----
export function Dialog({ title, onClose, children }: { title: string; onClose: () => void; children: ReactNode }) {
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const prev = document.activeElement as HTMLElement | null;
    ref.current?.querySelector<HTMLElement>("input, textarea, select, button")?.focus();
    const onKey = (e: KeyboardEvent) => e.key === "Escape" && onClose();
    window.addEventListener("keydown", onKey);
    return () => {
      window.removeEventListener("keydown", onKey);
      prev?.focus();
    };
  }, [onClose]);
  return (
    <div className="scrim" onMouseDown={(e) => e.target === e.currentTarget && onClose()}>
      <div className="dialog" role="dialog" aria-modal="true" aria-label={title} ref={ref}>
        <h2>{title}</h2>
        {children}
      </div>
    </div>
  );
}
