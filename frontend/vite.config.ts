import { fileURLToPath } from "node:url";
import { defineConfig, normalizePath } from "vite";
import react from "@vitejs/plugin-react";
import { viteStaticCopy } from "vite-plugin-static-copy";

export default defineConfig({
  plugins: [react(), viteStaticCopy({
    targets: ["cmaps", "standard_fonts", "wasm"].map((folder) => ({
      src: normalizePath(fileURLToPath(new URL(`./node_modules/pdfjs-dist/${folder}/*`, import.meta.url))),
      dest: `pdfjs/${folder}`,
      rename: { stripBase: true },
    })),
  })],
  base: "/console/assets/",
  build: {
    outDir: "../build/web",
    emptyOutDir: true,
    assetsDir: "assets",
  },
  server: {
    // Vitest bundles its config through a temporary ".vitest.config.ts.*.tmpdir"
    // directory in this folder; the dev server must not watch those files.
    watch: { ignored: ["**/.vitest.config.ts.*", "**/coverage/**"] },
    proxy: Object.fromEntries(
      ["/health", "/v1", "/mcp"].map((path) => [
        path,
        {
          target: process.env.RAG_DEV_API || "http://127.0.0.1:8011",
        },
      ]),
    ),
  },
});

