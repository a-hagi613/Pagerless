import { defineConfig, devices } from "@playwright/test";

// The suite drives the real Portal served by a real pagerless-server. The Go harness in
// internal/server/portal_e2e_test.go starts envtest, the server, and its bootstrap-token sign-in on this
// address; `make portal-e2e` builds the dashboard and runs both. A standalone `npm run test:e2e` uses the
// same address, so the harness must already be running.
const baseURL = process.env.PAGERLESS_PORTAL_URL ?? "http://127.0.0.1:18082";

export default defineConfig({
  testDir: "./e2e",
  timeout: 90_000,
  expect: { timeout: 20_000 },
  fullyParallel: false,
  // The projects share one server and one credentials Secret, so they run one at a time.
  workers: 1,
  retries: 0,
  reporter: process.env.CI === "1" ? [["list"], ["github"]] : [["list"]],
  use: {
    baseURL,
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
    video: "off",
  },
  projects: [
    {
      name: "desktop-1440x900",
      use: { ...devices["Desktop Chrome"], viewport: { width: 1440, height: 900 } },
    },
    {
      name: "mobile-390x844",
      use: {
        ...devices["Desktop Chrome"],
        viewport: { width: 390, height: 844 },
        isMobile: true,
        hasTouch: true,
        deviceScaleFactor: 2,
      },
    },
  ],
});
