/// <reference types="vitest/config" />
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Everything the Go backend serves itself. In dev the SPA runs on :5173 and
// these are forwarded to the backend on :8090 so the browser sees a single
// origin — same as production, where the backend serves both the API and
// the embedded SPA, so session cookies and relative URLs behave identically.
const BACKEND = "http://localhost:8090";

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      "/api": { target: BACKEND },
      // Probes live at the root, not under /api/v1, because orchestrators
      // and load balancers hit them directly.
      "/healthz": { target: BACKEND },
      "/readyz": { target: BACKEND },
      // WebDAV for desktop/mobile sync clients.
      "/dav": { target: BACKEND },
      // WOPI endpoints the office-suite server calls back into.
      "/wopi": { target: BACKEND },
      // Public share links. Trailing slash so an SPA route that merely
      // starts with "s" (e.g. /settings) isn't swallowed by the proxy.
      "/s/": { target: BACKEND },
    },
  },
  // `dist` is copied into backend/internal/webui/dist and embedded into the
  // Go binary. Vite's default hashed `assets/` file names are kept on
  // purpose: the backend serves /assets/* with an immutable cache header,
  // which is only safe because every content change yields a new name.
  build: {
    outDir: "dist",
  },
  test: {
    environment: "jsdom",
    setupFiles: ["./src/setupTests.ts"],
    globals: false,
    css: true,
    // Node 25+ has its own localStorage, unusable without --localstorage-file,
    // and it shadows jsdom's.
    execArgv: ["--no-experimental-webstorage"],
  },
});
