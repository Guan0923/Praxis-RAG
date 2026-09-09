import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react()],
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
