import { fileURLToPath } from "node:url";
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

const web = fileURLToPath(new URL("../../frontend/src", import.meta.url));

// The window reuses the web frontend's code as is (docs/desktop-client.md):
// api.ts and friends call /api/v1 by relative URL, and the app's asset
// handler proxies /api to the server, so there's no proxy here, even in
// dev (`make dev` points the app at this dev server, not the reverse).
export default defineConfig({
  plugins: [react()],
  resolve: {
    alias: { "@web": web },
    // Web files import these by bare name, which Node resolution would
    // take from frontend/node_modules: a second React (broken hooks) and
    // a second Mantine (no theme context). One copy, ours.
    dedupe: [
      "react",
      "react-dom",
      "@mantine/core",
      "@mantine/hooks",
      "@tanstack/react-query",
      "@tanstack/react-virtual",
      "@tabler/icons-react",
    ],
  },
  server: {
    // IPv4: Wails' dev proxy dials localhost as 127.0.0.1, and Vite on
    // "localhost" listens on ::1 only on macOS — the window stays blank.
    host: "127.0.0.1",
    port: 5174,
    strictPort: true,
    fs: { allow: [".", web] },
  },
  // Copied into cmd/cloud-desktop/dist and embedded by `make build`.
  build: {
    outDir: "dist",
    emptyOutDir: true,
  },
});
