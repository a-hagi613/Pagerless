import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

// The build is embedded in pagerless-server. During development and in the Playwright preview, the
// same relative /api URLs are proxied to a server: the real one on 8082 by default, or the offline
// mock the suite starts on 8092.
const apiTarget = process.env.PAGERLESS_API_TARGET ?? "http://127.0.0.1:8082";
const proxy = { "/api": { target: apiTarget, changeOrigin: false } };

export default defineConfig({
  plugins: [react(), tailwindcss()],
  // components.json uses the shadcn/ui "@" alias for the primitives this tree keeps.
  resolve: { alias: { "@": "/src" } },
  server: { proxy },
  preview: { proxy },
  build: {
    outDir: "dist",
    emptyOutDir: true,
    sourcemap: false,
  },
});
