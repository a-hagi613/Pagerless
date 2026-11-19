import { defineConfig } from "@playwright/test";

// The suite runs against the built Portal served by `vite preview`, with an offline mock of
// pagerless-server beside it. Nothing here reaches a cluster or a model.
export default defineConfig({
  testDir: "./e2e",
  timeout: 90_000,
  expect: { timeout: 15_000 },
  fullyParallel: false,
  workers: 1,
  reporter: [["list"]],
  use: {
    baseURL: "http://127.0.0.1:8091",
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
  },
  webServer: [
    {
      command: "node e2e/mock-api.mjs",
      port: 8092,
      reuseExistingServer: false,
      env: { MOCK_API_PORT: "8092" },
    },
    {
      command: "npm run preview -- --port 8091 --strictPort",
      port: 8091,
      reuseExistingServer: false,
      env: { PAGERLESS_API_TARGET: "http://127.0.0.1:8092" },
    },
  ],
});
