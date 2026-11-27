import { defineConfig, devices } from "@playwright/test";

// Standalone, the suite drives the built dashboard against the test-only contract fixture server
// (e2e/fixture-server.mjs). When PAGERLESS_PORTAL_URL is set it points at a real pagerless-server instead,
// which is how the integrated Go harness runs the same browser flows.
const baseURL = process.env.PAGERLESS_PORTAL_URL ?? "http://127.0.0.1:18082";
const externalServer = process.env.PAGERLESS_PORTAL_URL !== undefined;

export default defineConfig({
  testDir: "./e2e",
  timeout: 90_000,
  expect: { timeout: 20_000 },
  fullyParallel: false,
  // The scenarios share one fixture process, so they run one at a time.
  workers: 1,
  retries: 0,
  reporter: process.env.CI === "1" ? [["list"], ["github"]] : [["list"]],
  use: {
    baseURL,
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
    video: "off",
  },
  webServer: externalServer
    ? undefined
    : {
        command: "npm run build && node e2e/fixture-server.mjs",
        url: "http://127.0.0.1:18082/readyz",
        reuseExistingServer: process.env.CI !== "1",
        timeout: 180_000,
      },
  projects: [
    {
      name: "desktop-1280x900",
      use: { ...devices["Desktop Chrome"], viewport: { width: 1280, height: 900 } },
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
