// Bundle the main process and preload script as CommonJS (sandboxed
// preloads cannot be ES modules).
import { build } from "esbuild";

const common = { bundle: true, platform: "node", target: "node22", format: "cjs", external: ["electron"], sourcemap: true, logLevel: "warning" };
await build({ ...common, entryPoints: ["src/main/main.ts"], outfile: "dist/main/main.cjs" });
await build({ ...common, entryPoints: ["src/preload/preload.ts"], outfile: "dist/preload/preload.cjs" });
console.log("built main and preload");
