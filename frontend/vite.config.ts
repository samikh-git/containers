import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

// Build output goes into the Go restapi package, which embeds it into the
// router binary (single-binary deploy). In dev, /api is proxied to a locally
// running `router serve`.
export default defineConfig({
  plugins: [react(), tailwindcss()],
  build: {
    outDir: "../restapi/dist",
    emptyOutDir: true,
  },
  server: {
    proxy: {
      // ws: the terminal endpoint upgrades to a WebSocket.
      "/api": { target: "http://127.0.0.1:8400", ws: true },
    },
  },
});
