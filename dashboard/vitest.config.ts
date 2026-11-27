import { defineConfig } from "vitest/config";

// Unit tests cover the pure seams: the ordered event fold, the unified-diff reader, status language and
// the API client. The browser flows live in e2e/ and run under Playwright against the contract fixtures.
export default defineConfig({
  test: {
    include: ["src/**/*.test.ts"],
    environment: "node",
  },
});
