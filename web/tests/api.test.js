import { test } from "node:test";
import assert from "node:assert/strict";
import { API, APIError, bytes, instant } from "../src/lib/api.ts";

test("mutations require CSRF and send the caller retained idempotency key", async () => {
  /** @type {RequestInit[]} */ const requests = [];
  let failed = true;
  const api = new API(async (_url, options) => {
    requests.push(options || {});
    if (failed) {
      failed = false;
      throw Error("lost response");
    }
    return Response.json({ id: "same-job" });
  });
  await assert.rejects(
    () => api.mutate("/restores/run", { plan_id: "plan" }, "retained-key"),
    (error) => error instanceof APIError && error.code === "SESSION_REQUIRED",
  );
  assert.equal(requests.length, 0);
  api.session("csrf-token");
  await assert.rejects(() =>
    api.mutate("/restores/run", { plan_id: "plan" }, "retained-key"),
  );
  assert.deepEqual(
    await api.mutate("/restores/run", { plan_id: "plan" }, "retained-key"),
    { id: "same-job" },
  );
  for (const request of requests) {
    assert.equal(request.credentials, "same-origin");
    const headers = new Headers(request.headers);
    assert.equal(headers.get("X-CSRF-Token"), "csrf-token");
    assert.equal(headers.get("Idempotency-Key"), "retained-key");
    assert.equal(request.body, '{"plan_id":"plan"}');
  }
});

test("pagination returns the server cursor without interpreting it", async () => {
  const api = new API(async () =>
    Response.json([{ id: 1 }], {
      headers: { "X-Next-Cursor": "opaque+cursor" },
    }),
  );
  assert.deepEqual(await api.page("/archives"), {
    items: [{ id: 1 }],
    cursor: "opaque+cursor",
  });
});

test("typed errors preserve HTTP status and stable server code", async () => {
  const api = new API(async () =>
    Response.json(
      { error: { code: "STATE_CONFLICT", message: "Revision changed" } },
      { status: 409 },
    ),
  );
  await assert.rejects(
    () => api.get("/jobs"),
    (error) =>
      error instanceof APIError &&
      error.status === 409 &&
      error.code === "STATE_CONFLICT" &&
      error.message === "Revision changed",
  );
});

test("formatting keeps unknown observations distinct from zero", () => {
  assert.equal(bytes(undefined), "Unknown");
  assert.equal(bytes(null), "Unknown");
  assert.equal(bytes(0), "0 B");
  assert.equal(bytes(-1), "Unknown");
  assert.equal(instant("bad-date"), "Unknown");
});

test("operator failure descriptions explain safe corrective actions", async () => {
  const { failureMessage } = await import("../src/lib/api.ts");
  for (const [code, text] of [
    ["UNSAFE_ARCHIVE", "Restore is blocked"],
    ["INSUFFICIENT_SPACE", "Free space"],
    ["RESTORE_DESTINATION_UNAVAILABLE", "fresh destination"],
    ["SOURCE_CHANGED", "fresh backup preview"],
  ])
    assert.ok(failureMessage(code).includes(text));
  assert.equal(
    failureMessage("NEW_CODE", "Server explanation"),
    "Server explanation",
  );
});

test("legacy archives preserve a nullable commit index", async () => {
  /** @type {import('../src/lib/api.ts').Archive} */
  const legacy = {
    id: "legacy",
    volume_id: "volume",
    job_id: "import",
    sequence: 1,
    data_file_index: 0,
    commit_file_index: null,
    source_label: "Legacy",
    created_at: "2026-11-24T00:00:00Z",
    stream_bytes: 10240,
    sha256: "0".repeat(64),
    integrity: "OBSERVED",
    integrity_basis: "OBSERVED",
    restore_eligible: false,
  };
  const api = new API(async () => Response.json([legacy]));
  const rows = await api.get("/archives");
  assert.equal(rows[0].commit_file_index, null);
});

test("current integrity gates restores independently of earlier verification and graph eligibility", async () => {
  const {
    restoreIntegrityUsable,
    verifiedArchiveCount,
    historicalVerification,
  } = await import("../src/lib/api.ts");
  const historical = "2026-11-24T00:00:00Z";
  for (const integrity of ["FAILED", "UNKNOWN", "COMMITTED", "UNRECOGNIZED"]) {
    const archive = {
      integrity,
      restore_eligible: true,
      verified_at: historical,
      commit_file_index: 2,
    };
    assert.equal(restoreIntegrityUsable(archive), false);
    assert.equal(historicalVerification(archive), historical);
  }
  assert.equal(restoreIntegrityUsable(undefined), false);
  for (const integrity of ["VERIFIED", "OBSERVED"]) {
    const archive = { integrity, restore_eligible: false };
    assert.equal(restoreIntegrityUsable(archive), true);
  }
  assert.equal(
    verifiedArchiveCount([
      { integrity: "FAILED" },
      { integrity: "UNKNOWN" },
      { integrity: "COMMITTED" },
      { integrity: "OBSERVED" },
      { integrity: "VERIFIED" },
    ]),
    1,
  );
  assert.equal(
    historicalVerification({
      integrity: "OBSERVED",
      verified_at: historical,
      commit_file_index: null,
    }),
    undefined,
  );
  assert.equal(
    historicalVerification({
      integrity: "VERIFIED",
      verified_at: null,
      commit_file_index: 2,
    }),
    undefined,
  );
});

test("archive contract retains current failure beside its historical successful date", async () => {
  const { restoreIntegrityUsable, historicalVerification } =
    await import("../src/lib/api.ts");
  const date = "2026-11-24T00:00:00Z";
  const api = new API(async () =>
    Response.json({
      id: "archive",
      integrity: "FAILED",
      integrity_basis: "WRITE_READBACK",
      verified_at: date,
      commit_file_index: 2,
      restore_eligible: false,
    }),
  );
  const archive = await api.get("/archives/archive");
  assert.equal(archive.integrity, "FAILED");
  assert.equal(restoreIntegrityUsable(archive), false);
  assert.equal(historicalVerification(archive), date);
});
