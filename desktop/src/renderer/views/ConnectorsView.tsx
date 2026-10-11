import { useEffect, useState } from "react";
import type { ConnectorInfo } from "../../shared/types";
import { lux, shortTime, useLoad } from "../lib/lux";
import { Dialog, ErrorNotice, explain, useToast } from "../components/common";
import { Popover } from "../components/Menu";

// What each tool lets an agent do, in plain words.
const TOOL_WORDS: Record<string, string> = {
  gmail_search: "Search your email",
  gmail_read: "Read an email",
  gmail_draft: "Save a draft (asks you first)",
  gmail_send: "Send an email (asks you first)",
  calendar_events: "See your calendar",
  calendar_create: "Add an event (asks you first)",
  drive_search: "Search your Drive",
  drive_read: "Read a Drive file",
  outlook_search: "Search your Outlook",
  outlook_read: "Read an email",
  outlook_send: "Send an email (asks you first)",
  outlook_events: "See your calendar",
  outlook_create_event: "Add an event (asks you first)",
  onedrive_search: "Search your OneDrive",
  onedrive_read: "Read a OneDrive file",
  slack_search: "Search your Slack",
  slack_channels: "See your channels",
  slack_read: "Read a channel",
  slack_send: "Post as you (asks you first)",
};

/** Accounts your agents can use for you, as a dropdown. Tokens stay on the Lux server. */
export function ConnectorsMenu({ onClose }: { onClose: () => void }) {
  const list = useLoad<ConnectorInfo[]>(() => lux().connectors.list(), []);
  const [waiting, setWaiting] = useState<string | null>(null);
  const [setup, setSetup] = useState<ConnectorInfo | null>(null);
  const [pasting, setPasting] = useState<ConnectorInfo | null>(null);
  const toast = useToast();

  // While a sign-in is open in the browser, check back until it completes.
  useEffect(() => {
    if (!waiting) return;
    let n = 0;
    const timer = setInterval(async () => {
      n++;
      const r = await lux().connectors.list();
      if (r.ok && r.data.find((c) => c.id === waiting)?.connected) {
        setWaiting(null);
        list.reload();
        toast("Connected");
      } else if (n > 90) {
        setWaiting(null);
      }
    }, 2000);
    return () => clearInterval(timer);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [waiting]);

  async function connect(c: ConnectorInfo) {
    if (c.paste_token && !c.sign_in) {
      // An administrator on an https:// server sets up sign-in for everyone; otherwise paste a token.
      if (c.can_setup && !c.note) return setSetup(c);
      return setPasting(pasting?.id === c.id ? null : c);
    }
    if (!c.available) {
      if (c.can_setup) return setSetup(c);
      return toast(`${c.name} is not set up on this server yet. Ask an administrator to set it up.`, true);
    }
    const r = await lux().connectors.connect(c.id);
    if (!r.ok) return toast(explain(r.error), true);
    setWaiting(c.id);
  }
  async function disconnect(c: ConnectorInfo) {
    const r = await lux().connectors.disconnect(c.id);
    if (!r.ok) return toast(explain(r.error), true);
    toast(`Disconnected ${c.name}`);
    list.reload();
  }

  // The setup dialog is rendered inside the dropdown so clicks in it don't close it.
  return (
    <Popover label="Connectors" role="dialog" className="wide connectors-menu" onClose={setup ? () => {} : onClose}>
      <div className="menu-head">
        <strong>Connectors</strong>
        <span className="why">Accounts your agents can use for you. Agents never see the keys.</span>
      </div>
      {list.error && <ErrorNotice error={list.error} onRetry={list.reload} />}
      {list.data?.map((c) => (
        <div key={c.id} className={`connector-row${c.connected ? " on" : ""}`} title={c.tools.map((t) => TOOL_WORDS[t] ?? t).join(" · ")}>
          <span className="connector-mark" aria-hidden="true">{c.name[0]}</span>
          <span className="connector-title">
            <span className="connector-name">{c.name}</span>
            <span className="why">
              {waiting === c.id ? "Finish signing in in your browser…" : c.connected ? `Connected as ${c.account}${c.connected_at ? `, since ${shortTime(c.connected_at)}` : ""}` : c.description}
            </span>
            {c.last_error && <span className="connector-error">{c.last_error}</span>}
          </span>
          {c.connected ? (
            c.note && c.sign_in
              ? <button className="btn primary small" title={c.note} disabled={waiting === c.id} onClick={() => connect(c)}>{waiting === c.id ? "Waiting…" : "Connect again"}</button>
              : <button className="btn quiet small" onClick={() => disconnect(c)}>Disconnect</button>
          ) : (
            <button className="btn primary small" disabled={waiting === c.id} onClick={() => connect(c)}>{waiting === c.id ? "Waiting…" : "Connect"}</button>
          )}
        </div>
      ))}
      {pasting && <PasteToken c={pasting} onDone={() => { setPasting(null); list.reload(); toast(`Connected ${pasting.name}`); }} />}
      {setup?.id === "slack" && (
        <SetupSlack c={setup} onClose={() => setSetup(null)} onSaved={async () => {
          setSetup(null);
          list.reload();
          const r = await lux().connectors.connect("slack");
          if (!r.ok) return toast(explain(r.error), true);
          setWaiting("slack");
        }} />
      )}
      {setup?.id === "microsoft" && (
        <SetupMicrosoft c={setup} onClose={() => setSetup(null)} onSaved={async () => {
          setSetup(null);
          list.reload();
          const r = await lux().connectors.connect("microsoft");
          if (!r.ok) return toast(explain(r.error), true);
          setWaiting("microsoft");
        }} />
      )}
      {setup?.id === "google" && (
        <SetupGoogle onClose={() => setSetup(null)} onSaved={async () => {
          setSetup(null);
          list.reload();
          const r = await lux().connectors.connect("google");
          if (!r.ok) return toast(explain(r.error), true);
          setWaiting("google");
        }} />
      )}
    </Popover>
  );
}

/** One-time setup by an administrator: the Google OAuth client everyone connects through. */
function SetupGoogle({ onClose, onSaved }: { onClose: () => void; onSaved: () => void }) {
  const [id, setId] = useState("");
  const [secret, setSecret] = useState("");
  const [busy, setBusy] = useState(false);
  const toast = useToast();
  const idOk = /^[0-9]+-[a-z0-9]+\.apps\.googleusercontent\.com$/.test(id.trim());
  async function save() {
    setBusy(true);
    const r = await lux().connectors.setup("google", id.trim(), secret.trim());
    setBusy(false);
    if (!r.ok) return toast(explain(r.error), true);
    onSaved();
  }
  return (
    <Dialog title="Set up Google for everyone on this server" onClose={onClose}>
      <ol className="setup-steps">
        <li>
          In <a href="https://console.cloud.google.com/apis/credentials" onClick={(e) => { e.preventDefault(); void lux().openExternal("https://console.cloud.google.com/apis/credentials"); }}>Google Cloud Console</a>,
          enable the Gmail API and the Google Calendar API.
        </li>
        <li>Create credentials: OAuth client ID, application type <strong>Desktop app</strong>.</li>
        <li>Paste its client ID and secret here. You do this once; after that everyone just presses Connect.</li>
      </ol>
      <div className="field">
        <label htmlFor="g-id">Client ID</label>
        <input id="g-id" className="input mono" value={id} onChange={(e) => setId(e.target.value)} placeholder="1234…apps.googleusercontent.com" />
      </div>
      <div className="field">
        <label htmlFor="g-secret">Client secret</label>
        <input id="g-secret" className="input mono" type="password" autoComplete="off" value={secret} onChange={(e) => setSecret(e.target.value)} />
        <span className="hint">Stored encrypted on the Lux server. Agents never see it.</span>
      </div>
      <div className="foot">
        <button className="btn" onClick={onClose}>Cancel</button>
        <button className="btn primary" disabled={busy || !idOk || secret.trim().length < 10} onClick={save}>Save and connect</button>
      </div>
    </Dialog>
  );
}

const open = (url: string) => (e: { preventDefault: () => void }) => { e.preventDefault(); void lux().openExternal(url); };

/** Connect Slack with your own user token: works on any Lux server. */
function PasteToken({ c, onDone }: { c: ConnectorInfo; onDone: () => void }) {
  const [token, setToken] = useState("");
  const [busy, setBusy] = useState(false);
  const toast = useToast();
  const ok = /^xoxp-[A-Za-z0-9-]{10,}$/.test(token.trim());
  async function save() {
    setBusy(true);
    const r = await lux().connectors.pasteToken(token.trim());
    setBusy(false);
    if (!r.ok) return toast(r.error.code === "token_rejected" ? `Slack did not accept that token: ${r.error.message}` : explain(r.error), true);
    onDone();
  }
  return (
    <div className="paste-token">
      {c.note && <p className="why">{c.note}</p>}
      <ol className="setup-steps">
        <li><a href={c.setup_url} onClick={open(c.setup_url ?? "")}>Create the Lux app in Slack</a>, pick your workspace and press Create.</li>
        <li>In the app, open <strong>Install App</strong> and press <strong>Install to Workspace</strong>, then Allow.</li>
        <li>Copy the <strong>User OAuth Token</strong> (it starts with xoxp-) and paste it here.</li>
      </ol>
      <div className="key-row flat">
        <input className="input mono" type="password" autoComplete="off" autoFocus value={token} onChange={(e) => setToken(e.target.value)}
          placeholder="xoxp-…" aria-label="Slack user token" onKeyDown={(e) => { if (e.key === "Enter" && ok && !busy) void save(); }} />
        <button className="btn primary small" disabled={busy || !ok} onClick={save}>{busy ? "Checking…" : "Connect"}</button>
      </div>
    </div>
  );
}

/** One-time setup by an administrator on an https:// server: everyone then just presses Connect. */
function SetupSlack({ c, onClose, onSaved }: { c: ConnectorInfo; onClose: () => void; onSaved: () => void }) {
  const [id, setId] = useState("");
  const [secret, setSecret] = useState("");
  const [busy, setBusy] = useState(false);
  const toast = useToast();
  async function save() {
    setBusy(true);
    const r = await lux().connectors.setup("slack", id.trim(), secret.trim());
    setBusy(false);
    if (!r.ok) return toast(explain(r.error), true);
    onSaved();
  }
  return (
    <Dialog title="Set up Slack for everyone on this server" onClose={onClose}>
      <ol className="setup-steps">
        <li><a href={c.setup_url} onClick={open(c.setup_url ?? "")}>Create the Lux app in Slack</a>: everything is filled in. Pick your workspace and press Create.</li>
        <li>On the app's <strong>Basic Information</strong> page, copy the Client ID and Client Secret here.</li>
        <li>You do this once; after that everyone just presses Connect.</li>
      </ol>
      <div className="field">
        <label htmlFor="s-id">Client ID</label>
        <input id="s-id" className="input mono" value={id} onChange={(e) => setId(e.target.value)} placeholder="1234567890.1234567890" />
      </div>
      <div className="field">
        <label htmlFor="s-secret">Client secret</label>
        <input id="s-secret" className="input mono" type="password" autoComplete="off" value={secret} onChange={(e) => setSecret(e.target.value)} />
        <span className="hint">Stored encrypted on the Lux server. Agents never see it.</span>
      </div>
      <div className="foot">
        <button className="btn" onClick={onClose}>Cancel</button>
        <button className="btn primary" disabled={busy || !/^[0-9]+\.[0-9]+$/.test(id.trim()) || secret.trim().length < 16} onClick={save}>Save and connect</button>
      </div>
    </Dialog>
  );
}

/** One-time setup by an administrator: an Entra ID app registration in the organization's tenant. */
function SetupMicrosoft({ c, onClose, onSaved }: { c: ConnectorInfo; onClose: () => void; onSaved: () => void }) {
  const [id, setId] = useState("");
  const [tenant, setTenant] = useState("");
  const [secret, setSecret] = useState("");
  const [busy, setBusy] = useState(false);
  const toast = useToast();
  const guid = /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/;
  async function save() {
    setBusy(true);
    const r = await lux().connectors.setup("microsoft", id.trim(), secret.trim(), tenant.trim().toLowerCase());
    setBusy(false);
    if (!r.ok) return toast(explain(r.error), true);
    onSaved();
  }
  const portal = "https://entra.microsoft.com/#view/Microsoft_AAD_RegisteredApps/ApplicationsListBlade";
  return (
    <Dialog title="Set up Microsoft 365 for everyone on this server" onClose={onClose}>
      <ol className="setup-steps">
        <li>In the <a href={portal} onClick={open(portal)}>Microsoft Entra admin center</a>, open App registrations and press <strong>New registration</strong>. Name it Lux, accounts in this organization only.</li>
        <li>Under Redirect URI choose <strong>Web</strong> and enter <code className="copyable">{c.redirect_uri}</code></li>
        <li>In <strong>API permissions</strong>, add Microsoft Graph delegated permissions Mail.Read, Mail.Send, Calendars.ReadWrite, Files.Read and offline_access, then <strong>Grant admin consent</strong>.</li>
        <li>In <strong>Certificates &amp; secrets</strong>, add a client secret and copy its <strong>Value</strong>.</li>
      </ol>
      <div className="field">
        <label htmlFor="m-id">Application (client) ID</label>
        <input id="m-id" className="input mono" value={id} onChange={(e) => setId(e.target.value)} placeholder="1b2c3d4e-0000-1111-2222-333344445555" />
      </div>
      <div className="field">
        <label htmlFor="m-tenant">Directory (tenant) ID</label>
        <input id="m-tenant" className="input mono" value={tenant} onChange={(e) => setTenant(e.target.value)} placeholder="from the app's Overview page, or your domain" />
      </div>
      <div className="field">
        <label htmlFor="m-secret">Client secret value</label>
        <input id="m-secret" className="input mono" type="password" autoComplete="off" value={secret} onChange={(e) => setSecret(e.target.value)} />
        <span className="hint">Stored encrypted on the Lux server. Agents never see it.</span>
      </div>
      <div className="foot">
        <button className="btn" onClick={onClose}>Cancel</button>
        <button className="btn primary" disabled={busy || !guid.test(id.trim()) || !tenant.trim() || secret.trim().length < 16} onClick={save}>Save and connect</button>
      </div>
    </Dialog>
  );
}
