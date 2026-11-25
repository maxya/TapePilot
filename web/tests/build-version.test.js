import test from "node:test";
import assert from "node:assert/strict";
import {
  mkdtempSync,
  mkdirSync,
  writeFileSync,
  utimesSync,
  rmSync,
} from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { buildVersion } from "../build-version.ts";
test("release frontend version depends on content and epoch, preserves development defaults", () => {
  const parent = mkdtempSync(path.join(tmpdir(), "tapepilot-version-"));
  try {
    const root = path.join(parent, "web");
    mkdirSync(path.join(root, "src/nested"), { recursive: true });
    for (const file of [
      "package.json",
      "package-lock.json",
      "tsconfig.json",
      "vite.config.ts",
      "build-version.ts",
    ])
      writeFileSync(path.join(root, file), "synthetic " + file);
    writeFileSync(path.join(parent, ".node-version"), "24.22.1");
    const source = path.join(root, "src/nested/page.svelte");
    writeFileSync(source, "synthetic component");
    assert.equal(buildVersion(root, undefined), undefined);
    const initial = buildVersion(root, "0");
    assert.equal(initial, buildVersion(root, "0"));
    utimesSync(source, 1, 2);
    assert.equal(initial, buildVersion(root, "0"));
    writeFileSync(source, "changed component");
    assert.notEqual(initial, buildVersion(root, "0"));
    assert.notEqual(buildVersion(root, "0"), buildVersion(root, "1"));
    assert.throws(() => buildVersion(root, "invalid"));
  } finally {
    rmSync(parent, { recursive: true });
  }
});
