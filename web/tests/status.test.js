import { test } from "node:test";
import assert from "node:assert/strict";
import { connected } from "../src/lib/status.ts";
test("only a successful simulator health response confirms connectivity", async () => {
  const cases = /** @type {Array<[object, number, boolean]>} */ ([
    [{ status: "ok", mode: "simulator" }, 200, true],
    [{}, 200, false],
    [{ status: "ok", mode: "physical" }, 200, false],
    [{}, 503, false],
  ]);
  for (const [body, code, expected] of cases) {
    assert.equal(
      await connected(async () => Response.json(body, { status: code })),
      expected,
    );
  }
  assert.equal(
    await connected(async () => {
      throw Error("offline");
    }),
    false,
  );
});
