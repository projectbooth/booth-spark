/// <reference types="vitest/config" />
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react()],
  // Relative asset URLs. Under iframe-proxy the browser loads this page at /iframe/spark/, and
  // booth-core forwards only the remainder of the path to the backend (ADR 0069), so absolute
  // /assets/... URLs would escape the module's prefix.
  base: "./",
  build: { assetsInlineLimit: 0 },
  test: {
    environment: "jsdom",
    globals: true,
    setupFiles: ["./src/setupTests.ts"],
  },
});
