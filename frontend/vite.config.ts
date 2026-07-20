import { resolve } from "node:path";
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

// Build output goes into the Go restapi package, which embeds it into the
// router binary (single-binary deploy). Two HTML entries: end-user app and
// operator admin (Kumo). In dev, /api is proxied to a locally running
// `router serve`.
export default defineConfig({
  plugins: [react(), tailwindcss()],
  build: {
    outDir: "../restapi/dist",
    emptyOutDir: true,
    rollupOptions: {
      input: {
        main: resolve(__dirname, "index.html"),
        admin: resolve(__dirname, "admin.html"),
      },
    },
  },
  server: {
    proxy: {
      // ws: the terminal endpoint upgrades to a WebSocket.
      "/api": { target: "http://127.0.0.1:8400", ws: true },
    },
  },
});
