// Bundle the TypeScript unit tests with esbuild, then run them with node:test.
import { build } from "esbuild";
import { readdirSync, rmSync } from "node:fs";
import { spawnSync } from "node:child_process";

const dir = "test/unit";
const out = ".test-dist";
rmSync(out, { recursive: true, force: true });
const entries = readdirSync(dir).filter((f) => f.endsWith(".test.ts")).map((f) => `${dir}/${f}`);
await build({ entryPoints: entries, outdir: out, bundle: true, platform: "node", format: "esm", target: "node22", packages: "external", outExtension: { ".js": ".mjs" }, loader: { ".png": "file" }, logLevel: "warning" });
const files = readdirSync(out).filter((f) => f.endsWith(".mjs")).map((f) => `${out}/${f}`);
const r = spawnSync(process.execPath, ["--test", ...files], { stdio: "inherit" });
process.exit(r.status ?? 1);
