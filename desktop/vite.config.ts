import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The renderer is loaded from app://lux/ with a strict CSP: no inline
// scripts, no remote resources.
export default defineConfig({
  root: "src/renderer",
  base: "./",
  plugins: [react()],
  build: { outDir: "../../dist/renderer", emptyOutDir: true, sourcemap: false, target: "chrome130", modulePreload: { polyfill: false } },
});
