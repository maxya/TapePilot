import { test, expect } from "@playwright/test";
import {
  mkdtemp,
  readFile,
  writeFile,
  mkdir,
  rm,
  realpath,
  stat,
} from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { spawn, execFile } from "node:child_process";
import type { ChildProcess } from "node:child_process";
import type { Archive } from "../src/lib/api.ts";
import { promisify } from "node:util";
import net from "node:net";
const execute = promisify(execFile);
const repo = path.resolve(
  path.dirname(fileURLToPath(import.meta.url)),
  "../..",
);
const origin = "http://127.0.0.1:18789";
let fixture = "";
let password = "";
let volume = "";
const children: ChildProcess[] = [];
async function ready(url: string, child: ChildProcess) {
  for (let i = 0; i < 150; i++) {
    if (child.exitCode !== null)
      throw Error("Acceptance server exited before readiness");
    try {
      const response = await fetch(url);
      if (response.ok) return;
    } catch {}
    await new Promise((resolve) => setTimeout(resolve, 200));
  }
  throw Error("Acceptance server readiness timed out");
}
async function stop(child: ChildProcess) {
  if (child.exitCode !== null) return;
  const ended = new Promise<void>((resolve) =>
    child.once("exit", () => resolve()),
  );
  child.kill("SIGTERM");
  await Promise.race([
    ended,
    new Promise<void>((resolve) => setTimeout(resolve, 3000)),
  ]);
  if (child.exitCode === null) {
    child.kill("SIGKILL");
    await ended;
  }
}

async function availablePort(port: number) {
  await new Promise<void>((resolve, reject) => {
    const server = net.createServer();
    server.once("error", () =>
      reject(new Error("Acceptance port " + port + " is already occupied")),
    );
    server.listen(port, "127.0.0.1", () =>
      server.close((error) => (error ? reject(error) : resolve())),
    );
  });
}
test.beforeAll(async () => {
  await availablePort(8788);
  await availablePort(18789);
  fixture = await mkdtemp(path.join(tmpdir(), "tapepilot-browser-"));
  const env = {
    ...process.env,
    GOTOOLCHAIN: "go1.26.11",
    GOCACHE: process.env.GOCACHE || "/tmp/tapepilot-go-cache",
    TAPEPILOT_BROWSER_FIXTURE_DIR: fixture,
    TAPEPILOT_BROWSER_ORIGIN: origin,
  };
  await execute(
    "go",
    [
      "test",
      "./cmd/tapepilot",
      "-run",
      "^TestCreateBrowserFixture$",
      "-count=1",
    ],
    { cwd: repo, env, timeout: 120000 },
  );
  await execute(
    "go",
    ["build", "-o", path.join(fixture, "tapepilot"), "./cmd/tapepilot"],
    { cwd: repo, env, timeout: 120000 },
  );
  password = (await readFile(path.join(fixture, "password"), "utf8")).trim();
  volume = (await readFile(path.join(fixture, "volume"), "utf8")).trim();
  const backend = spawn(
    path.join(fixture, "tapepilot"),
    ["-config", path.join(fixture, "config.json"), "serve"],
    { cwd: repo, env, stdio: "ignore" },
  );
  children.push(backend);
  await ready("http://127.0.0.1:8788/api/v1/health", backend);
  await execute(
    process.execPath,
    [path.join(repo, "web/node_modules/vite/bin/vite.js"), "build"],
    { cwd: path.join(repo, "web"), env, timeout: 120000 },
  );
  const web = spawn(
    process.execPath,
    [
      path.join(repo, "web/node_modules/vite/bin/vite.js"),
      "preview",
      "--host",
      "127.0.0.1",
      "--port",
      "18789",
      "--strictPort",
    ],
    { cwd: path.join(repo, "web"), env, stdio: "ignore" },
  );
  children.push(web);
  await ready(origin, web);
});
test.afterAll(async () => {
  for (const child of children.reverse()) await stop(child);
  if (
    fixture &&
    (await realpath(fixture)) === fixture &&
    path.basename(fixture).startsWith("tapepilot-browser-") &&
    (await stat(fixture)).isDirectory()
  )
    await rm(fixture, { recursive: true });
});

async function login(page: import("@playwright/test").Page) {
  await page.goto("/");
  await page.getByLabel("Administrator password").fill(password);
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "Backup status" }),
  ).toBeVisible();
}
async function nav(page: import("@playwright/test").Page, name: string) {
  await page
    .getByRole("navigation", { name: "Main navigation" })
    .getByRole("button", { name, exact: true })
    .click();
}
async function selectedJob(
  page: import("@playwright/test").Page,
  state: string,
) {
  await expect(
    page
      .getByRole("region", { name: "Selected job" })
      .getByRole("status", { name: "Job state" }),
  ).toHaveText(state, { timeout: 30000 });
}
async function planBackup(page: import("@playwright/test").Page) {
  await nav(page, "Backup profiles");
  await page
    .getByRole("button", { name: "Preview backup", exact: true })
    .click();
  await expect(
    page.getByRole("region", { name: "Backup preview" }),
  ).toBeVisible();
}

test.describe.serial("real simulator console", () => {
  test("backup retry, offline search, selective restore and mobile navigation", async ({
    page,
    browserName,
  }) => {
    const errors: string[] = [];
    page.on("pageerror", (error) => errors.push(error.message));
    await login(page);
    await nav(page, "Backup profiles");
    await page
      .getByLabel("Profile name", { exact: true })
      .fill("Browser acceptance");
    await page.getByLabel("Source", { exact: true }).selectOption("sample");
    await page.getByLabel("Target tape", { exact: true }).selectOption(volume);
    await page
      .getByRole("button", { name: "Save profile", exact: true })
      .click();
    await expect(page.getByText("Backup profile saved.")).toBeVisible();
    await page
      .getByRole("button", { name: "Preview backup", exact: true })
      .click();
    let firstKey = "";
    let firstJob = "";
    const runPattern = "**/api/v1/backup-profiles/*/run";
    await page.route(runPattern, async (route) => {
      firstKey = route.request().headers()["idempotency-key"];
      const response = await route.fetch();
      const job = await response.json();
      firstJob = job.id;
      await route.abort("failed");
    });
    await page.getByRole("button", { name: "Run backup", exact: true }).click();
    await expect(page.getByRole("alert")).toContainText(
      "server could not be reached",
    );
    await page.unroute(runPattern);
    const retry = page.waitForRequest(
      (request) =>
        request.url().includes("/backup-profiles/") &&
        request.url().endsWith("/run"),
    );
    await page.getByRole("button", { name: "Run backup", exact: true }).click();
    expect((await retry).headers()["idempotency-key"]).toBe(firstKey);
    await selectedJob(page, "succeeded");
    await expect(
      page.getByRole("region", { name: "Selected job" }),
    ).toContainText(firstJob);
    const snapshot = await page.request.get("/api/v1/jobs");
    const rows = await snapshot.json();
    expect(
      rows.filter((job: { kind: string }) => job.kind === "BACKUP"),
    ).toHaveLength(1);
    await nav(page, "Catalog");
    await page.getByLabel("Archived path", { exact: true }).fill("example.txt");
    await page
      .getByRole("button", { name: "Search catalog", exact: true })
      .click();
    await expect(
      page.getByRole("cell", { name: "example.txt", exact: true }),
    ).toBeVisible();
    await page
      .getByRole("button", { name: "Open archive", exact: true })
      .click();
    await page.getByLabel("Restore the full archive").uncheck();
    await page.getByLabel("Select example.txt", { exact: true }).check();
    await page.getByLabel("Restore root", { exact: true }).selectOption("out");
    await page.getByLabel("New destination name").fill("browser-selected");
    await page
      .getByRole("button", { name: "Preview restore", exact: true })
      .click();
    await expect(
      page.getByRole("region", { name: "Restore preview" }),
    ).toContainText("out/browser-selected");
    await page
      .getByRole("button", { name: "Run restore", exact: true })
      .click();
    await selectedJob(page, "succeeded");
    const restored = page.getByRole("region", { name: "Selected job" });
    await expect(restored).toContainText("out/browser-selected");
    await expect(restored).toContainText("added by selection expansion");
    await expect(restored).toContainText("Integrity basis");
    expect(
      await readFile(
        path.join(fixture, "restored/browser-selected/example.txt"),
        "utf8",
      ),
    ).toBe(await readFile(path.join(fixture, "source/example.txt"), "utf8"));

    await expect(
      readFile(path.join(fixture, "restored/browser-selected/another.txt")),
    ).rejects.toThrow();
    await nav(page, "Catalog");
    await page.getByLabel("New destination name").fill("browser-selected");
    await page
      .getByRole("button", { name: "Preview restore", exact: true })
      .click();
    await expect(page.getByRole("alert")).toContainText(
      "fresh destination name",
    );
    await nav(page, "Overview");
    await mkdir(path.join(repo, "docs/images"), { recursive: true });
    await page.getByRole("link", { name: "Skip to content" }).focus();
    await page.keyboard.press("Enter");
    await expect(page.locator("#main")).toBeFocused();
    await page.evaluate(() => {
      if (document.activeElement instanceof HTMLElement)
        document.activeElement.blur();
    });
    await expect(page.locator(".skip")).not.toBeInViewport();
    if (browserName === "chromium") {
      await page.screenshot({
        path: path.join(repo, "docs/images/simulator-dashboard.png"),
        fullPage: true,
      });
    }
    await page.setViewportSize({ width: 390, height: 844 });
    await nav(page, "Jobs");
    await expect(
      page.getByRole("heading", { name: "Jobs", exact: true }),
    ).toBeVisible();
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= window.innerWidth,
      ),
    ).toBe(true);
    await page.setViewportSize({ width: 1280, height: 900 });
    expect(errors).toEqual([]);
  });
  test("wrong media waits, resumes explicitly, and waiting work can be cancelled", async ({
    page,
  }) => {
    await login(page);
    await nav(page, "Tapes");
    await page.getByLabel("Inventory entry").selectOption(volume);
    await page
      .getByLabel("Barcode (optional)", { exact: false })
      .fill("LAB-BROWSER-001");
    await page.getByRole("button", { name: "Save catalog metadata" }).click();
    await expect(
      page.getByRole("cell", { name: "LAB-BROWSER-001", exact: true }),
    ).toBeVisible();
    await page.getByLabel("Cartridge ID", { exact: true }).fill("other");
    await page
      .getByRole("button", { name: "Insert cartridge", exact: true })
      .click();
    await expect(page.getByText("Simulator cartridge inserted.")).toBeVisible();
    await planBackup(page);
    await page.getByRole("button", { name: "Run backup", exact: true }).click();
    await selectedJob(page, "waiting media");
    await nav(page, "Tapes");
    await page.getByLabel("Cartridge ID", { exact: true }).fill("lab");
    await page
      .getByRole("button", { name: "Insert cartridge", exact: true })
      .click();
    await nav(page, "Jobs");
    await page
      .getByRole("button", { name: "Recheck selected tape", exact: true })
      .click();
    await selectedJob(page, "succeeded");
    await nav(page, "Tapes");
    await page.getByLabel("Cartridge ID", { exact: true }).fill("other");
    await page
      .getByRole("button", { name: "Insert cartridge", exact: true })
      .click();
    await planBackup(page);
    await page.getByRole("button", { name: "Run backup", exact: true }).click();
    await selectedJob(page, "waiting media");
    await page
      .getByRole("button", { name: "Request cancellation", exact: true })
      .click();
    await selectedJob(page, "cancelled");
    await nav(page, "Schedules");
    await page
      .getByLabel("Backup profile", { exact: true })
      .selectOption({ label: "Browser acceptance" });
    await page.getByLabel("Operation", { exact: true }).selectOption("VERIFY");
    await page.getByLabel("IANA timezone").fill("UTC");
    await page
      .getByRole("button", { name: "Save schedule", exact: true })
      .click();
    await expect(page.getByText("Schedule saved.")).toBeVisible();
    await page
      .getByRole("button", { name: "Pause schedule", exact: true })
      .click();
    await expect(
      page.getByRole("button", { name: "Enable schedule", exact: true }),
    ).toBeVisible();
    await nav(page, "Alerts");
    const mark = page.getByRole("button", { name: "Mark read", exact: true });
    if (await mark.count()) {
      await mark.first().click();
    }
    await page.getByRole("button", { name: "Sign out", exact: true }).click();
    await expect(
      page.getByRole("heading", { name: "Sign in", exact: true }),
    ).toBeVisible();
  });
  test("changed source failures explain how to recover in the job result", async ({
    page,
  }) => {
    await login(page);
    await nav(page, "Tapes");
    await page.getByLabel("Cartridge ID", { exact: true }).fill("lab");
    await page
      .getByRole("button", { name: "Insert cartridge", exact: true })
      .click();
    await planBackup(page);
    await writeFile(
      path.join(fixture, "source/example.txt"),
      "Changed after reviewed preview\n",
    );
    await page.getByRole("button", { name: "Run backup", exact: true }).click();
    await selectedJob(page, "failed");
    await expect(
      page.getByRole("region", { name: "Selected job" }),
    ).toContainText("fresh backup preview");
    await nav(page, "System");
    await page.getByRole("button", { name: "Run doctor", exact: true }).click();
    await expect(
      page.getByRole("region", { name: "Doctor report" }),
    ).toContainText("utility:tar");
    await expect(
      page.getByRole("region", { name: "Doctor report" }),
    ).toContainText("root:source:sample");
    await expect(
      page.getByRole("region", { name: "Doctor report" }),
    ).not.toContainText(fixture);
  });
  test("new failed readback overrides historical verification and blocks restoration", async ({
    page,
  }) => {
    await login(page);
    const response = await page.request.get(
      "/api/v1/archives?volume_id=" + encodeURIComponent(volume) + "&limit=100",
    );
    const beforeRows = (await response.json()) as Archive[];
    const before = beforeRows.find((row) => row.data_file_index === 1);
    expect(before?.integrity).toBe("VERIFIED");
    if (!before) throw Error("Verified synthetic baseline archive is missing");
    const historical = before.verified_at || "";
    expect(historical).not.toBe("");
    await nav(page, "Catalog");
    await page.getByLabel("Archive", { exact: true }).selectOption(before.id);
    await expect(
      page.getByText("Last successful verification (historical)", {
        exact: false,
      }),
    ).toBeVisible();
    await page.getByLabel("New destination name").fill("integrity-blocked");
    await page
      .getByRole("button", { name: "Preview restore", exact: true })
      .click();
    await expect(
      page.getByRole("region", { name: "Restore preview" }),
    ).toBeVisible();
    const tapeFile = path.join(fixture, "state/simulator/lab/000001.tar");
    const raw = await readFile(tapeFile);
    let changed = false;
    for (let offset = 0; offset + 512 <= raw.length;) {
      const header = raw.subarray(offset, offset + 512);
      if (header.every((value) => value === 0)) break;
      const size = Number.parseInt(
        header.subarray(124, 136).toString("ascii").replace(/\0/g, "").trim() ||
          "0",
        8,
      );
      if (!Number.isSafeInteger(size) || size < 0)
        throw Error("Synthetic fixture has unsupported raw TAR size");
      if ((header[156] === 0 || header[156] === 48) && size > 0) {
        raw[offset + 512] ^= 1;
        changed = true;
        break;
      }
      offset += 512 + Math.ceil(size / 512) * 512;
    }
    expect(changed).toBe(true);
    await writeFile(tapeFile, raw);
    await page
      .getByRole("button", { name: "Verify tape read-only", exact: true })
      .click();
    await selectedJob(page, "recovery required");
    await nav(page, "Catalog");
    await page
      .getByRole("button", { name: "Refresh data", exact: true })
      .click();
    await expect(page.getByRole("alert")).toContainText(
      "Current integrity is failed",
    );
    await expect(
      page.getByRole("button", { name: "Preview restore", exact: true }),
    ).toBeDisabled();
    await expect(
      page.getByRole("button", { name: "Run restore", exact: true }),
    ).toHaveCount(0);
    await expect(
      page.getByRole("region", { name: "Restore preview" }),
    ).toHaveCount(0);
    await expect(page.locator("time[datetime]:visible")).toHaveAttribute(
      "datetime",
      historical,
    );
    const afterResponse = await page.request.get(
      "/api/v1/archives/" + encodeURIComponent(before.id),
    );
    const after = (await afterResponse.json()) as Archive;
    expect(after.integrity).toBe("FAILED");
    expect(after.verified_at).toBe(historical);
    const allResponse = await page.request.get("/api/v1/archives?limit=100");
    const allRows = (await allResponse.json()) as Archive[];
    await nav(page, "Overview");
    await expect(
      page
        .getByText("Verified archives in view", { exact: true })
        .locator("..")
        .locator("strong"),
    ).toHaveText(
      String(allRows.filter((row) => row.integrity === "VERIFIED").length),
    );
  });
});
