import { defineConfig } from "@playwright/test";

// The smoke test drives a running dashboard and a real Keycloak, both started
// by dashboard/e2e/run.sh. It is not part of `npm run build`.
export default defineConfig({
  testDir: "./e2e",
  timeout: 180_000,
  expect: { timeout: 60_000 },
  fullyParallel: false,
  workers: 1,
  reporter: [["list"]],
  use: {
    baseURL: process.env.PAGERLESS_SMOKE_BASE_URL ?? "http://127.0.0.1:18082",
    trace: "retain-on-failure",
  },
});
