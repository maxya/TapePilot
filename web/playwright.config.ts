import { existsSync } from "node:fs";
import { defineConfig } from "@playwright/test";
const browser = process.env.TAPEPILOT_TEST_BROWSER || "chromium";
if (browser !== "chromium" && browser !== "firefox" && browser !== "webkit") {
  throw new Error("Unsupported acceptance browser");
}
export default defineConfig({
  testDir: "./e2e",
  fullyParallel: false,
  workers: 1,
  timeout: 120000,
  retries: 0,
  reporter: "list",
  use: {
    browserName: browser,
    baseURL: "http://127.0.0.1:18789",
    actionTimeout: 15000,
    viewport: { width: 1280, height: 900 },
    trace: "off",
    video: "off",
    screenshot: "only-on-failure",
    launchOptions: {
      executablePath:
        browser === "chromium"
          ? process.env.TAPEPILOT_BROWSER_EXECUTABLE ||
            (existsSync("/sbin/chromium") ? "/sbin/chromium" : undefined)
          : undefined,
    },
  },
  outputDir: "/tmp/tapepilot-playwright-results",
});
