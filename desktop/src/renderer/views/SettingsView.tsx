import { Fragment, useState } from "react";
import type { AppInfo, LocalServerStatus, Profile, ProfilesState, ServerStatus } from "../../shared/types";
import { lux, useLoad, type Loadable } from "../lib/lux";
import { ErrorNotice, explain, useToast } from "../components/common";

export function SettingsView({ profiles, status, onChange, theme, setTheme }: {
  profiles: Loadable<ProfilesState>;
  status: Loadable<ServerStatus>;
  onChange: () => void;
  theme: string;
  setTheme: (t: string) => void;
}) {
  const info = useLoad<AppInfo>(() => lux().app.info(), []);
  const local = useLoad<LocalServerStatus>(() => lux().localServer.status(), []);
  const [adding, setAdding] = useState(false);
  const state = profiles.data;
  const current = state?.profiles.find((p) => p.name === state.current);

  return (
    <div className="page">
      <div className="page-head">
        <h1>Settings</h1>
      </div>
      <div className="now">
        {current && !current.signedIn && (
          <section aria-label="Sign in" className="stack">
            <h2>Sign in to {current.name}</h2>
            <SignIn profile={current} secure={!!state?.secureStorage} onDone={onChange} />
          </section>
        )}

        <section aria-label="Servers" className="stack">
          <h2>Servers</h2>
          {profiles.error && <ErrorNotice error={profiles.error} onRetry={profiles.reload} />}
          <table className="table">
            <thead><tr><th>Name</th><th>Address</th><th>Sign-in</th><th /></tr></thead>
            <tbody>
              {state?.profiles.map((p) => <ProfileRow key={p.name} p={p} current={p.name === state.current} onChange={onChange} />)}
            </tbody>
          </table>
          {adding ? <AddServer onDone={() => { setAdding(false); onChange(); }} onCancel={() => setAdding(false)} /> : (
            <div><button className="btn" onClick={() => setAdding(true)}>Add a server</button></div>
          )}
          {state && !state.secureStorage && (
            <div className="notice warn">This system has no secure credential store, so Lux keeps sign-ins only until you quit the app. Install a keyring service (for example GNOME Keyring or KWallet) to stay signed in.</div>
          )}
        </section>

        {current?.local && (
          <section aria-label="Local server" className="stack">
            <h2>Local server</h2>
            <LocalServer status={local} onChange={() => { local.reload(); status.reload(); }} />
          </section>
        )}

        {status.data?.components && (
          <section aria-label="Connected server" className="stack">
            <h2>Connected server</h2>
            <dl className="kv">
              <dt>Version</dt><dd>luxd {status.data.version}, {status.data.mode} mode</dd>
              {Object.entries(status.data.components).map(([k, v]) => <Fragment key={k}><dt>{k.replace(/_/g, " ")}</dt><dd>{v}</dd></Fragment>)}
              <dt>Model providers</dt><dd>{(status.data.model_providers ?? []).map((p) => `${p.name}${p.has_key || p.name === "ollama" ? "" : " (no key)"}`).join(", ") || "None configured"}</dd>
              <dt>Executors</dt><dd>{(status.data.executors ?? []).filter((e) => e.available).map((e) => `${e.name} (isolation: ${e.isolation})`).join(", ") || "None available"}</dd>
            </dl>
          </section>
        )}

        <section aria-label="Appearance" className="stack">
          <h2>Appearance</h2>
          <div className="row" role="radiogroup" aria-label="Theme">
            {["system", "light", "dark"].map((t) => (
              <label key={t} className="row">
                <input type="radio" name="theme" checked={theme === t} onChange={() => { setTheme(t); try { localStorage.setItem("lux.theme", t); } catch { /* not persisted */ } }} />
                {t === "system" ? "Match the system" : t === "light" ? "Light" : "Dark"}
              </label>
            ))}
          </div>
        </section>

        {info.data && (
          <section aria-label="About" className="stack">
            <h2>About</h2>
            <dl className="kv">
              <dt>Lux desktop</dt><dd>{info.data.version}</dd>
              <dt>Credential storage</dt><dd>{info.data.secureStorage ? `Encrypted (${info.data.storageBackend})` : "Session only"}</dd>
            </dl>
          </section>
        )}
      </div>
    </div>
  );
}

function storageLabel(p: Profile) {
  switch (p.tokenStorage) {
    case "cli": return "Signed in (shared with the lux command)";
    case "encrypted": return "Signed in";
    case "session": return "Signed in until you quit";
    default: return "Not signed in";
  }
}

function ProfileRow({ p, current, onChange }: { p: Profile; current: boolean; onChange: () => void }) {
  const toast = useToast();
  const act = async (fn: () => Promise<{ ok: boolean; error?: any }>, done: string) => {
    const r = await fn();
    if (!r.ok) return toast(explain(r.error), true);
    toast(done);
    onChange();
  };
  return (
    <tr>
      <td><strong>{p.name}</strong>{current && <span className="chip plain" style={{ marginLeft: 8 }}>In use</span>}</td>
      <td><code>{p.url}</code></td>
      <td>{storageLabel(p)}</td>
      <td style={{ textAlign: "right" }}>
        <div className="row" style={{ justifyContent: "flex-end" }}>
          {!current && <button className="btn" onClick={() => act(() => lux().profiles.select(p.name), `Switched to ${p.name}`)}>Use</button>}
          {(p.tokenStorage === "encrypted" || p.tokenStorage === "session") && <button className="btn quiet" onClick={() => act(() => lux().profiles.signOut(p.name), "Signed out")}>Sign out</button>}
          {!current && <button className="btn quiet danger" onClick={() => act(() => lux().profiles.remove(p.name), `Removed ${p.name}`)}>Remove</button>}
        </div>
      </td>
    </tr>
  );
}

function SignIn({ profile, secure, onDone }: { profile: Profile; secure: boolean; onDone: () => void }) {
  const [token, setToken] = useState("");
  const [busy, setBusy] = useState(false);
  const toast = useToast();
  async function submit() {
    setBusy(true);
    const r = await lux().profiles.signIn(profile.name, token.trim());
    setBusy(false);
    setToken("");
    if (!r.ok) return toast(r.error.code === "invalid_input" ? "That doesn't look like a Lux API token (lux_pat_…)." : explain(r.error), true);
    toast(`Signed in as ${r.data.me.username}`);
    onDone();
  }
  return (
    <div className="stack" style={{ maxWidth: 560 }}>
      <p>Paste an API token for <code>{profile.url}</code>. An administrator can create one, or run <code>lux login</code> on this computer to share the command line's sign-in.</p>
      <div className="field">
        <label htmlFor="token">API token</label>
        <input id="token" className="input mono" type="password" autoComplete="off" value={token} onChange={(e) => setToken(e.target.value)} onKeyDown={(e) => e.key === "Enter" && token && void submit()} />
        <span className="hint">{secure ? "Stored encrypted by your operating system's credential store." : "Kept only until you quit: this system has no secure credential store."}</span>
      </div>
      <div><button className="btn primary" disabled={busy || !token.trim()} onClick={submit}>Sign in</button></div>
    </div>
  );
}

function AddServer({ onDone, onCancel }: { onDone: () => void; onCancel: () => void }) {
  const [name, setName] = useState("");
  const [url, setUrl] = useState("https://");
  const toast = useToast();
  async function add() {
    const r = await lux().profiles.add(name.trim(), url.trim());
    if (!r.ok) return toast(explain(r.error), true);
    toast(`Added ${name}. Switch to it and sign in.`);
    onDone();
  }
  return (
    <div className="stack" style={{ maxWidth: 560 }}>
      <div className="field">
        <label htmlFor="srv-name">Name</label>
        <input id="srv-name" className="input" value={name} onChange={(e) => setName(e.target.value)} placeholder="work" />
      </div>
      <div className="field">
        <label htmlFor="srv-url">Address</label>
        <input id="srv-url" className="input" value={url} onChange={(e) => setUrl(e.target.value)} />
        {url.startsWith("http://") && !/^http:\/\/(127\.0\.0\.1|localhost|\[::1\])/.test(url) && <span className="hint" style={{ color: "var(--bad)" }}>Plain HTTP sends your token unencrypted. Use https for remote servers.</span>}
      </div>
      <div className="row">
        <button className="btn primary" disabled={!name.trim() || url.length < 10} onClick={add}>Add server</button>
        <button className="btn quiet" onClick={onCancel}>Cancel</button>
      </div>
    </div>
  );
}

function LocalServer({ status, onChange }: { status: Loadable<LocalServerStatus>; onChange: () => void }) {
  const [busy, setBusy] = useState(false);
  const toast = useToast();
  const s = status.data;
  async function act(which: "start" | "stop") {
    setBusy(true);
    const r = await lux().localServer[which]();
    setBusy(false);
    if (!r.ok) return toast(explain(r.error), true);
    if (r.data.message) toast(r.data.message, which === "start" && !r.data.running);
    onChange();
  }
  if (!s) return null;
  if (!s.available) return <p className="notice">{s.message}</p>;
  return (
    <div className="stack">
      <p>{s.running ? `Running at ${s.url}${s.pid ? ` (process ${s.pid})` : ""}.` : "Not running."} Stopping it does not stop agents that are already working; they continue when it starts again.</p>
      <div className="row">
        {s.running ? <button className="btn" disabled={busy} onClick={() => act("stop")}>Stop local server</button> : <button className="btn primary" disabled={busy} onClick={() => act("start")}>Start local server</button>}
      </div>
    </div>
  );
}
