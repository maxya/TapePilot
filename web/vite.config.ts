import adapter from "@sveltejs/adapter-static";
import { sveltekit } from "@sveltejs/kit/vite";
import { defineConfig } from "vite";
import { fileURLToPath } from "node:url";
import { buildVersion } from "./build-version.ts";
const version = buildVersion(
  fileURLToPath(new URL(".", import.meta.url)),
  process.env.SOURCE_DATE_EPOCH,
);
export default defineConfig({
  plugins: [
    sveltekit({
      adapter: adapter(),
      ...(version ? { version: { name: version } } : {}),
    }),
  ],
  preview: {
    proxy: {
      "/api/v1/": { target: "http://127.0.0.1:8788", changeOrigin: false },
    },
  },
  server: {
    proxy: {
      "/api/v1/": { target: "http://127.0.0.1:8788", changeOrigin: false },
    },
  },
});
