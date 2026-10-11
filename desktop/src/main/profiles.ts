// Connection profiles and their credentials.
//
// Profiles are the environments in the shared Lux client config
// (~/.config/lux/config.yaml), so the app and the `lux` CLI see the same
// servers. Tokens are looked up in this order:
//   1. session memory (when the OS has no secure storage),
//   2. the app's own store, encrypted with Electron safeStorage,
//   3. the CLI's credentials file (credentials.yaml, mode 0600).
// The renderer never receives a token.
import { existsSync, mkdirSync, readFileSync, renameSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import YAML from "yaml";
import type { Connection } from "./api";
import type { Profile, ProfilesState } from "../shared/types";

export interface SecretBox {
  /** True when encryption is backed by an OS secret store. */
  secure(): boolean;
  backend(): string;
  encrypt(plain: string): Buffer;
  decrypt(data: Buffer): string;
}

interface EnvConfig {
  url: string;
  workspace?: string;
  local?: boolean;
}

function writeAtomic(path: string, data: string, mode = 0o600) {
  mkdirSync(dirname(path), { recursive: true, mode: 0o700 });
  const tmp = `${path}.tmp-${process.pid}`;
  writeFileSync(tmp, data, { mode });
  renameSync(tmp, path);
}

export class ProfileStore {
  private session = new Map<string, string>();

  constructor(private opts: { configDir: string; userDataDir: string; box: SecretBox }) {}

  private get configPath() { return join(this.opts.configDir, "config.yaml"); }
  private get cliCredentialsPath() { return join(this.opts.configDir, "credentials.yaml"); }
  private get tokenStorePath() { return join(this.opts.userDataDir, "tokens.json"); }

  private readConfig(): YAML.Document {
    if (!existsSync(this.configPath)) {
      return new YAML.Document({ current: "local", environments: { local: { url: "http://127.0.0.1:7878", local: true } } });
    }
    return YAML.parseDocument(readFileSync(this.configPath, "utf8"));
  }

  private environments(doc: YAML.Document): Record<string, EnvConfig> {
    const envs = (doc.toJS()?.environments ?? {}) as Record<string, EnvConfig>;
    return envs;
  }

  private cliTokens(): Record<string, string> {
    try {
      const doc = YAML.parse(readFileSync(this.cliCredentialsPath, "utf8"));
      return (doc?.tokens ?? {}) as Record<string, string>;
    } catch {
      return {};
    }
  }

  private storedTokens(): Record<string, string> {
    try {
      return JSON.parse(readFileSync(this.tokenStorePath, "utf8"));
    } catch {
      return {};
    }
  }

  private tokenFor(name: string): { token: string | null; storage: Profile["tokenStorage"] } {
    const s = this.session.get(name);
    if (s) return { token: s, storage: "session" };
    const stored = this.storedTokens()[name];
    if (stored && this.opts.box.secure()) {
      try {
        return { token: this.opts.box.decrypt(Buffer.from(stored, "base64")), storage: "encrypted" };
      } catch {
        // Unreadable (e.g. the keyring changed): treat as signed out.
      }
    }
    const cli = this.cliTokens()[name];
    if (cli) return { token: cli, storage: "cli" };
    return { token: null, storage: "none" };
  }

  state(): ProfilesState {
    const doc = this.readConfig();
    const envs = this.environments(doc);
    const current = (doc.get("current") as string) || "local";
    const profiles: Profile[] = Object.entries(envs)
      .sort(([a], [b]) => a.localeCompare(b))
      .map(([name, e]) => {
        const t = this.tokenFor(name);
        return { name, url: e.url, local: !!e.local, workspace: e.workspace || "default", signedIn: !!t.token, tokenStorage: t.storage };
      });
    return { current: envs[current] ? current : profiles[0]?.name ?? "", profiles, secureStorage: this.opts.box.secure() };
  }

  connection(name?: string): Connection & { name: string } {
    const st = this.state();
    const n = name ?? st.current;
    const p = st.profiles.find((x) => x.name === n);
    if (!p) throw new Error(`no profile named ${n}`);
    return { name: n, url: p.url, workspace: p.workspace, token: this.tokenFor(n).token };
  }

  select(name: string) {
    const doc = this.readConfig();
    if (!this.environments(doc)[name]) throw new Error(`no profile named ${name}`);
    doc.set("current", name);
    writeAtomic(this.configPath, doc.toString());
  }

  add(name: string, url: string) {
    const doc = this.readConfig();
    if (this.environments(doc)[name]) throw new Error(`a profile named ${name} already exists`);
    doc.setIn(["environments", name], { url });
    writeAtomic(this.configPath, doc.toString());
  }

  remove(name: string) {
    const doc = this.readConfig();
    if (((doc.get("current") as string) || "local") === name) throw new Error("switch to another profile before removing this one");
    doc.deleteIn(["environments", name]);
    writeAtomic(this.configPath, doc.toString());
    this.signOut(name);
  }

  /** Store a verified token. Returns where it was kept. */
  saveToken(name: string, token: string): "encrypted" | "session" {
    if (!this.opts.box.secure()) {
      // No OS secret store: never write the token in a recoverable form.
      this.session.set(name, token);
      return "session";
    }
    const all = this.storedTokens();
    all[name] = this.opts.box.encrypt(token).toString("base64");
    writeAtomic(this.tokenStorePath, JSON.stringify(all));
    this.session.delete(name);
    return "encrypted";
  }

  /** Forget the app's token. A token managed by the CLI is left alone. */
  signOut(name: string) {
    this.session.delete(name);
    const all = this.storedTokens();
    if (name in all) {
      delete all[name];
      writeAtomic(this.tokenStorePath, JSON.stringify(all));
    }
  }
}
