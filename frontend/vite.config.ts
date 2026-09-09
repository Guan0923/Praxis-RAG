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
    outDir: "../src/rag_mcp/web",
    emptyOutDir: true,
    assetsDir: "assets",
  },
  server: {
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
