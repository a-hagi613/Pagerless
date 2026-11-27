import { fileURLToPath } from "node:url";
import { defineConfig } from "vitest/config";

// Unit tests cover the pure seams: the ordered event fold, the unified-diff reader, status language,
// the API client and the safe Markdown renderer. The browser flows live in e2e/ and run under
// Playwright against the contract fixtures.
export default defineConfig({
  resolve: {
    alias: { "@": fileURLToPath(new URL("./src", import.meta.url)) },
  },
  test: {
    include: ["src/**/*.test.ts"],
    environment: "node",
  },
});
