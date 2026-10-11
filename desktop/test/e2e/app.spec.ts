// End-to-end tests of the built desktop app (run `npm run build` first).
//
// The "real server" tests need a fresh, empty PostgreSQL database in
// LUX_E2E_DATABASE_URL and built Go binaries in LUX_E2E_BIN (luxd and lux).
import { test, expect, _electron as electron, type ElectronApplication, type Page } from "@playwright/test";
import { spawn, execFileSync, type ChildProcess } from "node:child_process";
import { mkdtempSync, writeFileSync, mkdirSync, openSync, readFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createServer } from "node:http";
import type { AddressInfo } from "node:net";

// Pages other than conversations live in the menu on the user row.
async function openMenu(page: Page, item: string) {
  await page.getByRole("button", { name: "More", exact: true }).click();
  await page.getByRole("menuitem", { name: item }).click();
}

async function launch(configDir: string): Promise<{ app: ElectronApplication; page: Page }> {
  const app = await electron.launch({
    args: ["dist/main/main.cjs"],
    env: { ...process.env, LUX_DESKTOP_TEST: "1", LUX_CONFIG_DIR: configDir, LUX_DESKTOP_USER_DATA: mkdtempSync(join(tmpdir(), "lux-ud-")), LUX_CLI_PATH: "" },
  });
  const page = await app.firstWindow();
  await page.waitForSelector(".shell");
  return { app, page };
}

function configFor(url: string): string {
  const dir = mkdtempSync(join(tmpdir(), "lux-e2e-cfg-"));
  writeFileSync(join(dir, "config.yaml"), `current: local\nenvironments:\n  local:\n    url: ${url}\n    local: true\n`, { mode: 0o600 });
  return dir;
}

test.describe("without a server", () => {
  let app: ElectronApplication;
  let page: Page;
  test.beforeAll(async () => {
    ({ app, page } = await launch(configFor("http://127.0.0.1:1")));
  });
  test.afterAll(async () => app?.close());

  test("explains that the server cannot be reached", async () => {
    // Not signed in and unreachable: settings and sign-in are shown, with the error banner.
    await expect(page.getByRole("alert").filter({ hasText: "Can't reach local" })).toBeVisible();
    await expect(page.getByRole("heading", { name: "Sign in to local" })).toBeVisible();
  });

  test("the renderer has no Node, no network and only the lux bridge", async () => {
    const probe = await page.evaluate(async () => {
      const w = window as any;
      let fetchBlocked = false;
      try {
        await fetch("https://example.com/");
      } catch {
        fetchBlocked = true;
      }
      return {
        require: typeof w.require,
        process: typeof w.process,
        module: typeof w.module,
        bridge: Object.keys(w.lux).sort(),
        frozen: Object.isFrozen(w.lux),
        ipc: typeof w.ipcRenderer,
        fetchBlocked,
        origin: location.origin,
      };
    });
    expect(probe.require).toBe("undefined");
    expect(probe.process).toBe("undefined");
    expect(probe.module).toBe("undefined");
    expect(probe.ipc).toBe("undefined");
    expect(probe.frozen).toBe(true);
    expect(probe.bridge).toEqual(["agents", "app", "approvals", "changes", "connectors", "conversations", "events", "files", "localServer", "models", "openExternal", "profiles", "repositories", "routines", "server", "settings", "tasks"]);
    expect(probe.fetchBlocked).toBe(true);
    expect(probe.origin).toBe("app://lux");
  });

  test("navigation away from the app is blocked", async () => {
    await page.evaluate(() => {
      location.href = "https://example.com/";
    });
    await page.waitForTimeout(500);
    expect(page.url()).toMatch(/^app:\/\/lux\//);
    const opened = await page.evaluate(() => window.open("https://example.com/docs") === null);
    expect(opened).toBe(true); // no new Electron window
    // https links go to the system browser (recorded in test mode), anything else is dropped.
    await page.evaluate(() => window.open("http://insecure.example/"));
    const external = await app.evaluate(() => (globalThis as any).__luxExternalOpened);
    expect(external).toEqual(["https://example.com/docs"]);
  });

  test("bridge input is validated in the main process", async () => {
    const r = await page.evaluate(() => (window as any).lux.tasks.get("../../v1/me"));
    expect(r).toEqual({ ok: false, error: { code: "invalid_input", message: expect.stringContaining("task") } });
  });

  test("window security settings", async () => {
    const prefs = await app.evaluate(({ BrowserWindow }) => {
      const wc = BrowserWindow.getAllWindows()[0].webContents as any;
      const p = wc.getLastWebPreferences();
      return { contextIsolation: p.contextIsolation, nodeIntegration: p.nodeIntegration, sandbox: p.sandbox, webSecurity: p.webSecurity };
    });
    expect(prefs).toEqual({ contextIsolation: true, nodeIntegration: false, sandbox: true, webSecurity: true });
  });
});

const DB = process.env.LUX_E2E_DATABASE_URL;
const BIN = process.env.LUX_E2E_BIN;

test.describe("with a real luxd", () => {
  test.skip(!DB || !BIN, "set LUX_E2E_DATABASE_URL (empty database) and LUX_E2E_BIN (luxd and lux)");
  let luxd: ChildProcess;
  let app: ElectronApplication;
  let page: Page;
  let configDir: string;
  const port = 17900 + Math.floor(Math.random() * 90);
  // A stand-in Lyzr Studio that lists one agent for the right key.
  const lyzr = createServer((req, res) => {
    const ok = req.url === "/v3/agents/" && req.headers["x-api-key"] === "e2e-lyzr-key";
    res.writeHead(ok ? 200 : 401, { "content-type": "application/json" });
    res.end(ok ? '[{"_id":"agent-e2e-1","name":"lux-manager"}]' : "{}");
  });
  const env = () => ({ ...process.env, LUX_CONFIG_DIR: configDir, LUX_DATABASE_URL: DB!, LUX_LISTEN: `127.0.0.1:${port}`, LUX_MODE: "local", XDG_DATA_HOME: join(configDir, "data"),
    LUX_LYZR_BASE_URL: `http://127.0.0.1:${(lyzr.address() as AddressInfo).port}/v4`, LUX_OLLAMA_DISABLED: "true" });

  test.beforeAll(async () => {
    await new Promise<void>((r) => lyzr.listen(0, "127.0.0.1", r));
    configDir = mkdtempSync(join(tmpdir(), "lux-e2e-real-"));
    const log = join(configDir, "luxd.log");
    const fd = openSync(log, "a");
    luxd = spawn(join(BIN!, "luxd"), ["serve"], { env: env(), stdio: ["ignore", fd, fd] });
    let ready = false;
    for (let i = 0; i < 100 && !ready; i++) {
      try {
        ready = (await fetch(`http://127.0.0.1:${port}/readyz`)).ok;
      } catch {
        /* not up yet */
      }
      if (!ready) await new Promise((r) => setTimeout(r, 200));
    }
    if (!ready) throw new Error("luxd did not become ready:\n" + readFileSync(log, "utf8"));
    // A real agent repository, imported with the real CLI.
    const repo = join(configDir, "agent");
    mkdirSync(repo);
    writeFileSync(join(repo, "agent.yaml"), 'spec_version: "0.1.0"\nname: release-notes\nversion: 1.0.0\ndescription: Drafts release notes from merged pull requests\nmodel:\n  preferred: anthropic:claude-sonnet-4-5\n');
    writeFileSync(join(repo, "SOUL.md"), "# Soul\n\nYou write clear, short release notes.\n");
    const git = (...a: string[]) => execFileSync("git", a, { cwd: repo, env: { ...process.env, GIT_AUTHOR_NAME: "T", GIT_AUTHOR_EMAIL: "t@x.invalid", GIT_COMMITTER_NAME: "T", GIT_COMMITTER_EMAIL: "t@x.invalid" } });
    git("init", "-q", "-b", "main");
    git("add", "-A");
    git("commit", "-q", "-m", "init");
    execFileSync(join(BIN!, "lux"), ["agent", "import", repo], { env: env() });
    execFileSync(join(BIN!, "lux"), ["agent", "run", "release-notes", "Draft notes for v1.2"], { env: env() });
    ({ app, page } = await launch(configDir));
  });
  test.afterAll(async () => {
    await app?.close();
    luxd?.kill("SIGTERM");
    lyzr.close();
  });

  test("shows real agents and tasks from the server", async () => {
    await openMenu(page, "Agents");
    await expect(page.getByRole("heading", { name: "Release notes" })).toBeVisible();
    await expect(page.getByText("Drafts release notes from merged pull requests").first()).toBeVisible();
    await openMenu(page, "All tasks");
    await expect(page.getByRole("heading", { name: "Draft notes for v1.2" })).toBeVisible();
  });

  test("talking to an agent continues its one thread", async () => {
    await page.getByRole("button", { name: "New message" }).click();
    await page.getByRole("textbox", { name: "Agent", exact: true }).fill("release");
    await page.getByRole("option", { name: /Release notes/ }).click();
    await expect(page.getByRole("heading", { name: "Release notes", level: 1 })).toBeVisible();
    await page.getByRole("textbox", { name: "Message", exact: true }).fill("Summarise last week's changes");
    await page.getByRole("button", { name: "Send" }).click();
    await expect(page.locator(".bubble.you", { hasText: "Summarise last week's changes" })).toBeVisible();
    const tasks = execFileSync(join(BIN!, "lux"), ["task", "list", "--json"], { env: env(), encoding: "utf8" });
    expect(tasks).toContain("Summarise last week's changes");
    // The sidebar lists agents by name, not conversation titles.
    const side = page.getByRole("navigation", { name: "Agents" });
    await expect(side.getByText("Release notes", { exact: true })).toBeVisible();
    await expect(side.getByText("Summarise last week's changes", { exact: true })).toHaveCount(0);
  });

  test("creates an agent in one click; it greets you in its chat", async () => {
    const side = page.getByRole("navigation", { name: "Agents" });
    await page.getByRole("button", { name: "New message" }).click();
    await page.getByRole("option", { name: "Create an agent" }).click();
    // No model chosen on this server yet: Models opens. Adding a key shows its models as buttons.
    const models = page.getByRole("dialog", { name: "Models" });
    await models.getByRole("button", { name: "Add key" }).first().click();
    await models.getByLabel("Lyzr API key").fill("e2e-wrong-key");
    await models.getByRole("button", { name: "Save" }).click();
    await expect(page.locator(".toast", { hasText: "Lyzr did not accept that key." })).toBeVisible();
    await models.getByLabel("Lyzr API key").fill("e2e-lyzr-key");
    await models.getByRole("button", { name: "Save" }).click();
    await models.getByRole("button", { name: "lux-manager" }).click();
    await expect(page.getByRole("heading", { name: "New agent", level: 1 })).toBeVisible();
    await expect(page.locator(".note", { hasText: "You created New agent" })).toBeVisible();
    const agents = execFileSync(join(BIN!, "lux"), ["agent", "list", "--json"], { env: env(), encoding: "utf8" });
    expect(agents).toContain('"slug": "new-agent"');
    expect(agents).toContain('"source_kind": "managed"');
    // From now on it is just the click.
    await page.getByRole("button", { name: "New message" }).click();
    await page.getByRole("option", { name: "Create an agent" }).click();
    await expect(page.getByRole("heading", { name: "New agent 2", level: 1 })).toBeVisible();
    await expect(side.getByText("New agent 2", { exact: true })).toBeVisible();
  });

  test("the sidebar menu renames, turns off and deletes an agent", async () => {
    const side = page.getByRole("navigation", { name: "Agents" });
    await side.getByText("New agent", { exact: true }).click({ button: "right" });
    await page.getByRole("menuitem", { name: "Rename" }).click();
    const field = side.getByRole("textbox", { name: "Agent name" });
    await field.fill("People team");
    await field.press("Enter");
    await expect(side.getByText("People team", { exact: true })).toBeVisible();

    await side.getByRole("button", { name: "More for People team" }).click();
    await page.getByRole("menuitem", { name: "Turn off" }).click();
    await expect(side.locator(".side-item.off", { hasText: "People team" })).toBeVisible();

    // Its introduction may still be running; stop it so it can be deleted.
    const lux = (...args: string[]) => execFileSync(join(BIN!, "lux"), args, { env: env(), encoding: "utf8" });
    const busy = () => JSON.parse(lux("task", "list", "--agent", "new-agent", "--state", "queued,starting,running,waiting_approval,cancelling", "--json")) as { id: string; state: string }[];
    for (const t of busy()) if (t.state !== "cancelling") lux("task", "cancel", t.id);
    await expect.poll(() => busy().length, { timeout: 30_000 }).toBe(0);

    await side.getByText("People team", { exact: true }).click({ button: "right" });
    await page.getByRole("menuitem", { name: "Delete" }).click();
    await page.getByRole("dialog", { name: "Delete People team?" }).getByRole("button", { name: "Delete" }).click();
    await expect(side.getByText("People team", { exact: true })).toHaveCount(0);
    expect(lux("agent", "list", "--json")).not.toContain('"slug": "new-agent"');
  });

  test("closing the app does not stop server-side work", async () => {
    await app.close();
    const st = await (await fetch(`http://127.0.0.1:${port}/v1/status`)).json();
    expect(st.components.orchestrator).toBe("running");
    ({ app, page } = await launch(configDir));
    await openMenu(page, "All tasks");
    await expect(page.getByText("Draft notes for v1.2").first()).toBeVisible();
  });

  // Design review aid, not an assertion: LUX_E2E_SHOTS=<dir> saves screenshots of each view.
  test("screenshots for design review", async () => {
    const dir = process.env.LUX_E2E_SHOTS;
    test.skip(!dir, "set LUX_E2E_SHOTS to a directory");
    await app.evaluate(({ BrowserWindow }) => {
      const w = BrowserWindow.getAllWindows()[0];
      w.setSize(1280, 800);
      w.showInactive(); // hidden windows don't paint
    });
    for (const item of ["All tasks", "Agents", "Needs you", "Settings"]) {
      await openMenu(page, item);
      await page.waitForTimeout(400);
      await page.screenshot({ path: join(dir!, `${item.toLowerCase().replace(/ /g, "-")}.png`) });
    }
    await page.getByRole("navigation", { name: "Agents" }).locator(".side-row.chat").first().click();
    await page.waitForTimeout(400);
    await page.screenshot({ path: join(dir!, "conversation.png") });
  });
});
