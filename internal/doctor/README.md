# General installation doctor

`doctor.Inspect(ctx, settings)` returns a sanitized simulator report with operational check IDs, utility versions, root identities and byte capacities. It performs no physical discovery, device open, tape motion or driver configuration. Explicit hardware/profile diagnostics use the separate Linux adapter qualification process.

The fixed utility allowlist checks only GNU tar, mbuffer, mt-st and optional lsscsi version arguments. The default probe resolves the fixed installed path, rejects unsafe permissions and executable types, and executes the observed ELF through its pinned descriptor. Output is capped at 4 KiB and each process has a two-second context deadline with process-group cancellation. Unknown variants remain unavailable. GNU mt cannot satisfy mt-st. Missing mbuffer reports the existing bounded Go buffering fallback.

Root checks inspect existing configured directories without creating files. They verify canonical paths, service access, owner-only state/spool permissions, root identity, disjoint configured roots and local SQLite storage. Spool and restore filesystem reports include available bytes and configured reserve. Free-space observations provide a point-in-time preflight, with the engine retaining its own checks during operations.

Absolute paths and raw subprocess output are excluded from reports. `Inspector.Probe` supports deterministic fixture tests. The web console renders the authenticated report on its System page. Reports identify simulator prerequisites separately from the outstanding physical qualification gate.
