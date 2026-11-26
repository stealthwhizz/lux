import { useEffect, useRef, useState } from "react";
import type { Agent, BrowserInput, BrowserView } from "../../shared/types";
import { lux } from "../lib/lux";
import { explain, useToast } from "./common";

/**
 * The agent's own browser, live. You can watch it work, or take it over
 * (click, type, go somewhere) to sign in for it; the agent waits until you
 * give it back.
 */
export function BrowserPanel({ agent, onClose }: { agent: Agent; onClose: () => void }) {
  const [view, setView] = useState<BrowserView | null>(null);
  const [error, setError] = useState("");
  const [address, setAddress] = useState("");
  const [text, setText] = useState("");
  const busy = useRef(false);
  const img = useRef<HTMLImageElement>(null);
  const toast = useToast();

  // Refresh about once a second while the panel is open.
  useEffect(() => {
    let live = true;
    const tick = async () => {
      if (busy.current) return;
      const r = await lux().agents.browser(agent.id);
      if (!live) return;
      if (r.ok) {
        setView(r.data);
        setError("");
      } else setError(explain(r.error));
    };
    void tick();
    const t = setInterval(tick, 1000);
    return () => {
      live = false;
      clearInterval(t);
    };
  }, [agent.id]);
  useEffect(() => {
    if (view?.url && document.activeElement?.id !== "bp-address") setAddress(view.url);
  }, [view?.url]);

  async function act(input: BrowserInput) {
    busy.current = true;
    const r = await lux().agents.browserInput(agent.id, input);
    busy.current = false;
    if (!r.ok) return toast(explain(r.error), true);
    setView(r.data);
  }
  async function giveBack() {
    const r = await lux().agents.browserRelease(agent.id);
    if (!r.ok) return toast(explain(r.error), true);
    toast(`${agent.name} has its browser back`);
    setView((v) => (v ? { ...v, held: false } : v));
  }
  // A point on the picture, in the page's own pixels.
  const point = (e: { clientX: number; clientY: number }) => {
    const box = img.current!.getBoundingClientRect();
    const scale = (view?.width ?? 1280) / box.width;
    return { x: Math.round((e.clientX - box.left) * scale), y: Math.round((e.clientY - box.top) * scale) };
  };
  const open = () => {
    const u = address.trim();
    if (u) void act({ type: "open", url: /^https?:\/\//.test(u) ? u : "https://" + u });
  };

  return (
    <aside className="files-panel browser-panel" aria-label="Browser">
      <div className="files-head">
        <h2>Browser</h2>
        <button className="link quiet" onClick={onClose}>Close</button>
      </div>
      <div className="browser-bar">
        <button className="btn small quiet" onClick={() => act({ type: "back" })} disabled={!view?.open} aria-label="Back">←</button>
        <input id="bp-address" className="input mono" value={address} onChange={(e) => setAddress(e.target.value)} placeholder="Go to an address, e.g. booking.com"
          aria-label="Address" onKeyDown={(e) => { if (e.key === "Enter") open(); }} />
        <button className="btn small" onClick={open} disabled={!address.trim()}>Go</button>
      </div>
      <div className="files-list browser-body">
        {error && <p className="chat-hint">{error}</p>}
        {view && !view.open && !error && (
          <p className="chat-hint">{agent.name}'s browser isn't open. It opens when the agent uses it, or go to an address above to sign in to a site for it ahead of time; its browser keeps the sign-in.</p>
        )}
        {view?.open && (
          <>
            {view.held ? (
              <div className="browser-held">
                <span>You're in control. {agent.name} waits until you give it back.</span>
                <button className="btn small primary" onClick={giveBack}>Give back to agent</button>
              </div>
            ) : (
              <p className="why">Watching {agent.name}. Click, type or go somewhere to take over.</p>
            )}
            <img ref={img} className="browser-screen" src={`data:image/jpeg;base64,${view.image}`} alt={view.title || "The agent's browser"}
              onClick={(e) => act({ type: "click", ...point(e) })}
              onWheel={(e) => act({ type: "scroll", ...point(e), dy: Math.max(-1500, Math.min(1500, e.deltaY)) })} />
            <div className="browser-type">
              <input className="input" type="password" autoComplete="off" value={text} onChange={(e) => setText(e.target.value)}
                placeholder="Type into the page (click a field first)" aria-label="Type into the page"
                onKeyDown={(e) => { if (e.key === "Enter" && text) { void act({ type: "type", text }); setText(""); } }} />
              <button className="btn small" disabled={!text} onClick={() => { void act({ type: "type", text }); setText(""); }}>Type</button>
              <button className="btn small quiet" onClick={() => act({ type: "key", key: "Enter" })}>Enter</button>
              <button className="btn small quiet" onClick={() => act({ type: "key", key: "Tab" })}>Tab</button>
              <button className="btn small quiet" onClick={() => act({ type: "key", key: "Backspace" })} aria-label="Backspace">⌫</button>
            </div>
            <p className="why">What you type goes straight to the page and is not kept by Lux.</p>
          </>
        )}
      </div>
    </aside>
  );
}
