import { defineConfig } from "vitest/config";

// Unit tests cover the pure seams: the filter rules, the unified-diff reader, and the Portal/CLI
// parity manifest. The browser flows live in e2e/ and run under Playwright.
export default defineConfig({
  test: {
    include: ["src/**/*.test.ts"],
    environment: "node",
  },
});
