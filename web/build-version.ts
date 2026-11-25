import { createHash } from "node:crypto";
import { readFileSync, readdirSync } from "node:fs";
import path from "node:path";

// Release builds derive SvelteKit's version from shipped inputs. Development
// retains the framework's normal default when SOURCE_DATE_EPOCH is absent.
export function buildVersion(
  root: string,
  epoch: string | undefined,
): string | undefined {
  if (epoch === undefined) return undefined;
  if (!/^[0-9]+$/.test(epoch))
    throw new Error("SOURCE_DATE_EPOCH must be a nonnegative integer");
  const files = [
    "../.node-version",
    "package.json",
    "package-lock.json",
    "tsconfig.json",
    "vite.config.ts",
    "build-version.ts",
  ];
  function walk(relative: string): void {
    const entries = readdirSync(path.join(root, relative), {
      withFileTypes: true,
    });
    for (const entry of entries) {
      const name = path.posix.join(relative, entry.name);
      if (entry.isSymbolicLink())
        throw new Error(
          "Release frontend inputs must use regular files and directories",
        );
      if (entry.isDirectory()) walk(name);
      else if (entry.isFile()) files.push(name);
    }
  }
  walk("src");
  files.sort();
  const hash = createHash("sha256");
  hash.update("tapepilot-frontend\0" + epoch + "\0");
  for (const name of files) {
    const data = readFileSync(path.join(root, name));
    hash.update(name + "\0" + data.length + "\0");
    hash.update(data);
  }
  return "source-" + hash.digest("hex");
}
