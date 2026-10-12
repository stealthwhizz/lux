// Electron main process: window lifecycle, security policy, IPC.
import { app, BrowserWindow, dialog, ipcMain, Notification, protocol, safeStorage, session, shell, type WebContents } from "electron";
import { extname, join, normalize } from "node:path";
import { readFile, writeFile } from "node:fs/promises";
import { homedir } from "node:os";
import { execFileSync } from "node:child_process";
import { existsSync } from "node:fs";
import { ProfileStore, type SecretBox } from "./profiles";
import { Handlers } from "./handlers";
import { APP_ORIGIN, APP_SCHEME, CONTENT_SECURITY_POLICY, allowPermission, isAppURL, resolveAppPath, webPreferences } from "./security";
import { EVENT_CHANNEL, INVOKE_CHANNEL } from "../shared/ipc";

const TEST_MODE = process.env.LUX_DESKTOP_TEST === "1";

const MIME: Record<string, string> = {
  ".html": "text/html; charset=utf-8",
  ".js": "text/javascript; charset=utf-8",
  ".css": "text/css; charset=utf-8",
  ".svg": "image/svg+xml",
  ".png": "image/png",
  ".woff2": "font/woff2",
  ".json": "application/json",
};

// app:// must be registered before the app is ready.
protocol.registerSchemesAsPrivileged([{ scheme: APP_SCHEME, privileges: { standard: true, secure: true, supportFetchAPI: false, corsEnabled: false } }]);

// Tests run with an isolated profile directory.
if (TEST_MODE && process.env.LUX_DESKTOP_USER_DATA) app.setPath("userData", process.env.LUX_DESKTOP_USER_DATA);

// One instance at a time.
if (!TEST_MODE && !app.requestSingleInstanceLock()) app.quit();

function configDir(): string {
  if (process.env.LUX_CONFIG_DIR) return process.env.LUX_CONFIG_DIR;
  if (process.env.XDG_CONFIG_HOME) return join(process.env.XDG_CONFIG_HOME, "lux");
  return join(homedir(), ".config", "lux");
}

function secretBox(): SecretBox {
  return {
    secure: () => {
      if (!safeStorage.isEncryptionAvailable()) return false;
      // On Linux without a keyring, Electron falls back to a hard-coded key.
      return process.platform !== "linux" || safeStorage.getSelectedStorageBackend() !== "basic_text";
    },
    backend: () => (process.platform === "linux" ? safeStorage.getSelectedStorageBackend() : process.platform === "darwin" ? "keychain" : "dpapi"),
    encrypt: (s) => safeStorage.encryptString(s),
    decrypt: (b) => safeStorage.decryptString(b),
  };
}

function findLuxCLI(): string | null {
  const candidates = [process.env.LUX_CLI_PATH, join(process.resourcesPath ?? "", "bin", "lux")].filter(Boolean) as string[];
  for (const c of candidates) if (existsSync(c)) return c;
  try {
    const which = process.platform === "win32" ? "where" : "which";
    const p = execFileSync(which, ["lux"], { encoding: "utf8", stdio: ["ignore", "pipe", "ignore"] }).split("\n")[0].trim();
    return p || null;
  } catch {
    return null;
  }
}

// Saving asks the person where, with the system dialog; tests record instead.
const savedFiles: string[] = [];
(globalThis as any).__luxSavedFiles = savedFiles;
async function saveFile(name: string, bytes: Uint8Array): Promise<boolean> {
  if (TEST_MODE) {
    savedFiles.push(name);
    return true;
  }
  const r = await dialog.showSaveDialog(mainWindow!, { defaultPath: join(app.getPath("downloads"), name.replace(/[\\/]/g, "_")) });
  if (r.canceled || !r.filePath) return false;
  await writeFile(r.filePath, bytes);
  return true;
}

// Choosing files uses the system dialog; tests name files in LUX_DESKTOP_TEST_ATTACH.
async function pickFiles(): Promise<{ name: string; bytes: Uint8Array }[]> {
  const paths = TEST_MODE
    ? (process.env.LUX_DESKTOP_TEST_ATTACH ?? "").split(":").filter(Boolean)
    : (await dialog.showOpenDialog(mainWindow!, { properties: ["openFile", "multiSelections"] })).filePaths;
  const out = [];
  for (const p of paths.slice(0, 10)) out.push({ name: p.split(/[\\/]/).pop()!, bytes: new Uint8Array(await readFile(p)) });
  return out;
}

// In tests, external links are recorded instead of opening a real browser.
const externalOpened: string[] = [];
(globalThis as any).__luxExternalOpened = externalOpened;
function openExternal(url: string): Promise<void> {
  if (TEST_MODE) {
    externalOpened.push(url);
    return Promise.resolve();
  }
  return shell.openExternal(url);
}

// Notifications come from the main process (the page has no notification
// permission); tests record them instead of showing them.
const notified: { title: string; body: string; agent?: string }[] = [];
(globalThis as any).__luxNotified = notified;
function notify(owner: number, n: { title: string; body: string; agent?: string }) {
  if (TEST_MODE) {
    notified.push(n);
    return;
  }
  if (!Notification.isSupported()) return;
  const note = new Notification({ title: n.title, body: n.body, silent: false });
  note.on("click", () => {
    const w = BrowserWindow.getAllWindows().find((x) => x.webContents.id === owner);
    if (!w || w.isDestroyed()) return;
    if (w.isMinimized()) w.restore();
    w.show();
    w.focus();
    if (n.agent) w.webContents.send(EVENT_CHANNEL, { subscription: "notify", kind: "open", data: { agent: n.agent } });
  });
  note.show();
}

const rendererDir = join(__dirname, "..", "renderer");
let mainWindow: BrowserWindow | null = null;

function createWindow() {
  mainWindow = new BrowserWindow({
    width: 1280,
    height: 820,
    minWidth: 880,
    minHeight: 560,
    show: false,
    title: "Lux",
    backgroundColor: "#111214",
    webPreferences: { ...webPreferences, preload: join(__dirname, "..", "preload", "preload.cjs") },
  });
  const wc = mainWindow.webContents;
  // Navigation and new windows are blocked; https links go to the system browser.
  wc.on("will-navigate", (e, url) => {
    if (!isAppURL(url)) e.preventDefault();
  });
  wc.on("will-redirect", (e, url) => {
    if (!isAppURL(url)) e.preventDefault();
  });
  wc.setWindowOpenHandler(({ url }) => {
    if (url.startsWith("https://")) void openExternal(url);
    return { action: "deny" };
  });
  wc.on("will-attach-webview", (e) => e.preventDefault());
  if (!TEST_MODE) mainWindow.once("ready-to-show", () => mainWindow?.show());
  void mainWindow.loadURL(`${APP_ORIGIN}/index.html`);
  mainWindow.on("closed", () => {
    handlers.stopAll(wc.id);
    mainWindow = null;
  });
}

const profiles = new ProfileStore({ configDir: configDir(), userDataDir: app.getPath("userData"), box: secretBox() });
const handlers = new Handlers({
  profiles,
  appInfo: () => ({ version: app.getVersion(), platform: process.platform, secureStorage: secretBox().secure(), storageBackend: secretBox().backend() }),
  openExternal,
  push: (owner, msg) => {
    const wc = BrowserWindow.getAllWindows().map((w) => w.webContents).find((w) => w.id === owner);
    if (wc && !wc.isDestroyed()) wc.send(EVENT_CHANNEL, msg);
  },
  luxCLI: findLuxCLI,
  saveFile,
  pickFiles,
  notify,
});

// Only the app's own top-level frame may call into the main process.
function trustedSender(sender: WebContents, frameURL: string | undefined): boolean {
  return !!mainWindow && sender.id === mainWindow.webContents.id && !!frameURL && isAppURL(frameURL);
}

app.whenReady().then(() => {
  const ses = session.defaultSession;
  ses.setPermissionRequestHandler((_wc, permission, cb) => cb(allowPermission(permission)));
  ses.setPermissionCheckHandler((_wc, permission) => allowPermission(permission));
  // The renderer must not reach the network itself; this backs up the CSP.
  ses.webRequest.onBeforeRequest((details, cb) => {
    const ok = details.url.startsWith(`${APP_SCHEME}:`) || details.url.startsWith("devtools:");
    cb({ cancel: !ok });
  });

  protocol.handle(APP_SCHEME, async (req) => {
    const file = resolveAppPath(req.url, rendererDir, join, normalize);
    if (!file) return new Response("not found", { status: 404 });
    let body: Buffer;
    try {
      body = await readFile(file);
    } catch {
      return new Response("not found", { status: 404 });
    }
    return new Response(new Uint8Array(body), {
      status: 200,
      headers: {
        "Content-Type": MIME[extname(file)] ?? "application/octet-stream",
        "Content-Security-Policy": CONTENT_SECURITY_POLICY,
        "X-Content-Type-Options": "nosniff",
      },
    });
  });

  ipcMain.handle(INVOKE_CHANNEL, (event, op: unknown, args: unknown) => {
    if (!trustedSender(event.sender, event.senderFrame?.url)) {
      return { ok: false, error: { code: "forbidden", message: "untrusted sender" } };
    }
    return handlers.invoke(op, args, event.sender.id);
  });

  createWindow();
  app.on("activate", () => {
    if (BrowserWindow.getAllWindows().length === 0) createWindow();
  });
});

app.on("web-contents-created", (_e, contents) => {
  contents.on("will-attach-webview", (e) => e.preventDefault());
});

app.on("second-instance", () => {
  if (mainWindow) {
    if (mainWindow.isMinimized()) mainWindow.restore();
    mainWindow.focus();
  }
});

app.on("window-all-closed", () => {
  // Closing the window never stops agent runs: they live in luxd.
  handlers.stopAll();
  if (process.platform !== "darwin") app.quit();
});
