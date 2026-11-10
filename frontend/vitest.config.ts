import { defineConfig } from "vitest/config";

// Console unit and contract tests. The DOM environment is enabled by default so
// component tests can rely on the same setup; the current suite only needs it
// for `location`-based helpers. Tests run in worker threads instead of forked
// processes: the suite has no native dependencies and threads keep the setup
// portable (and some CI/agent sandboxes refuse piped child processes).
export default defineConfig({
  test: {
    environment: "jsdom",
    include: ["src/**/*.test.{ts,tsx}"],
    pool: "threads",
    restoreMocks: true,
    clearMocks: true,
  },
});
