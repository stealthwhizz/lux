# ADR 0001: Desktop architecture

**Status:** accepted, 2026-10-09. Electron over Tauri 2, after the comparison below. No desktop code existed when this was decided, so neither option carried a migration cost.

## Context
The Lux desktop app is a client of the Lux API (REST + Server-Sent Events). It shows agents, conversations, tasks and approvals, stores connection profiles and credentials, and can start or stop a local `luxd`. It must not own task state: agents run in `luxd` and its execution providers, so closing the app never stops a task. Agent execution must not depend on the desktop shell.

That makes the desktop app a thin, network-heavy client. Its native needs are modest: secure credential storage, notifications, a `lux://` deep link, starting a local process, a tray icon, and auto-update.

## Options compared

Facts marked **[doc]** come from official documentation (links at the end). Others are our assessment.

| Criterion | Electron | Tauri 2 |
|---|---|---|
| Rendering engine | Bundled Chromium, identical on every OS | System webview: WebView2 (Chromium) on Windows, WKWebView on macOS, WebKitGTK on Linux **[doc]**. Three engines to test; macOS and Linux engine versions follow the OS or distro **[doc]** |
| Linux behaviour | Same as other platforms | Documented WebKitGTK problems on NVIDIA drivers (blank windows, crashes on resize, silent slow WebGL); workarounds disable GPU paths **[doc]** |
| Security model | Main / preload / renderer split. Context isolation, no Node in renderer and sandbox are defaults since v12 / v5 / v20 **[doc]**. 20-item security checklist we must follow **[doc]** | Rust commands plus a capability system that grants permissions per window **[doc]**. By default every registered command is callable from all windows unless restricted in `build.rs` **[doc]**. Smaller attack surface overall |
| Credential storage | `safeStorage`: Keychain on macOS, DPAPI on Windows, kwallet or libsecret on Linux. Falls back to a hardcoded password (`basic_text`) when no Linux secret store exists **[doc]** | No first-party keychain plugin; Stronghold (an encrypted vault that needs a password) **[doc]**, or the community `keyring` crate |
| Auto-update | electron-updater supports macOS, Windows, and Linux AppImage, deb and rpm, from GitHub, S3 or a plain HTTPS server **[doc]**. The built-in autoUpdater covers macOS and Windows only **[doc]** | Official updater plugin; signatures are mandatory and cannot be disabled **[doc]**. The Linux path documented is AppImage only **[doc]** |
| Plugins / native APIs | Large built-in API (notifications, tray, protocol handlers, `child_process`), mature npm ecosystem | Official plugins for deep-link, notification, shell, updater, store, stronghold, single-instance, autostart, process, fs, http, global-shortcut, log **[doc]** |
| End-to-end testing | Playwright drives Electron on all three OSes | WebdriverIO `tauri-service` on all three OSes; plain `tauri-driver` is Windows and Linux only **[doc]** |
| Maintenance | A new major release about every 8 weeks; only the latest 3 are supported **[doc]**, so we must upgrade about twice a year | Rust and npm dependencies; the webview is patched by the OS. Fewer forced upgrades |
| Size and memory | Larger installer (Chromium and Node bundled) and higher memory use | Much smaller installer and lower memory (system webview) |
| Languages | TypeScript only, in main, preload and renderer | TypeScript UI plus Rust for commands |
| Existing code | None in Lux | None in Lux. gitagent's Ira desktop app (`gitagent-rs/desktop`) is a 70-line Tauri shell that loads a localhost web server with CSP disabled. That pattern is the one our security audit flagged, so there is nothing to reuse |

## Decision
Use **Electron + React + TypeScript**, built with Vite, in `desktop/`.

Why, for Lux specifically:
1. **One rendering engine.** Enterprise fleets mix macOS, Windows and Linux. A single Chromium avoids three-engine testing and the documented WebKitGTK/NVIDIA failures on Linux, the platform self-hosting teams use most.
2. **Updates on every Linux package format.** electron-updater covers deb and rpm as well as AppImage.
3. **Mature testing.** Playwright's Electron support is mature on all three operating systems.
4. **One language.** gitagent, opengap and the Lux runner are all TypeScript, so the people building them can work on the desktop shell without learning a new language.
5. **No Rust advantage here.** Tauri's main strength (Rust in-process, close to Ira) does not apply, because Lux deliberately keeps execution out of the desktop app.

What we give up: a much larger download and higher memory use; an Electron upgrade roughly every six months; and on Linux without a secret store, `safeStorage` is not secure.

### Architecture
- **Main process** (`desktop/src/main`): window lifecycle, connection profiles, credential storage, all REST and SSE calls to the Lux API, starting and stopping a local `luxd`, notifications, deep links. Tokens are encrypted with `safeStorage`. When `getSelectedStorageBackend()` returns `basic_text`, the app refuses to save a token and keeps it in memory for the session only.
- **Preload** (`desktop/src/preload`): exposes one object, `window.lux`, via `contextBridge`. Each function maps to one named IPC operation. It exposes no generic `invoke` and no Node APIs.
- **Renderer** (`desktop/src/renderer`): React UI.
  - `contextIsolation`, `sandbox` and `webSecurity` are on, and `nodeIntegration` is off.
  - Content is loaded from a custom `app://` protocol, not `file://`.
  - A strict CSP allows only bundled resources, so the renderer cannot reach the network.
  - Navigation and new windows are blocked; `https:` links open in the system browser.
  - IPC handlers check that the sender is the app's own frame.
  - Electron fuses disable `RunAsNode` and Node CLI inspect arguments in packaged builds.
- **IPC contract** (`desktop/src/shared/ipc.ts`): typed operations. The main process validates every argument before acting.
- **Keeping the shell replaceable:** the renderer only talks to a `DesktopBridge` TypeScript interface. Moving to Tauri later would mean reimplementing that interface in Rust, not rewriting the UI.

### Revisit if
- memory use or download size becomes a blocker for customers;
- a design partner standardises on Linux without a secret store and needs encrypted credentials;
- Tauri's Linux webview problems are fixed upstream.

## Sources
- Electron security checklist: https://www.electronjs.org/docs/latest/tutorial/security
- Electron safeStorage: https://www.electronjs.org/docs/latest/api/safe-storage
- Electron updates: https://www.electronjs.org/docs/latest/tutorial/updates
- Electron release timelines: https://www.electronjs.org/docs/latest/tutorial/electron-timelines
- electron-builder auto-update: https://www.electron.build/docs/features/auto-update
- Tauri webview versions: https://v2.tauri.app/reference/webview-versions/
- Tauri capabilities: https://v2.tauri.app/security/capabilities/
- Tauri updater: https://v2.tauri.app/plugin/updater/
- Tauri plugins: https://v2.tauri.app/plugin/
- Tauri Linux graphics issues: https://v2.tauri.app/develop/debug/linux-graphics/
- Tauri WebDriver testing: https://v2.tauri.app/develop/tests/webdriver/
