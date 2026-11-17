import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

// The build is embedded in pagerless-server. During development, Vite proxies the read-only API to the server's
// dashboard listener, so the same relative URLs work in both modes.
export default defineConfig({
  plugins: [react(), tailwindcss()],
  // components.json uses the shadcn/ui "@" alias; the code imports relatively, and the alias keeps the CLI honest.
  resolve: { alias: { "@": "/src" } },
  server: {
    proxy: {
      "/api": "http://127.0.0.1:8082",
    },
  },
  build: {
    outDir: "dist",
    emptyOutDir: true,
    sourcemap: false,
  },
});
