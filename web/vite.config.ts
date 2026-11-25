import adapter from "@sveltejs/adapter-static";
import { sveltekit } from "@sveltejs/kit/vite";
import { defineConfig } from "vite";
export default defineConfig({
  plugins: [sveltekit({ adapter: adapter() })],
  server: { proxy: { "/api/v1/": "http://127.0.0.1:8788" } },
});
