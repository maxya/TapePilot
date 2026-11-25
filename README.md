# SpoolArk — project bootstrap

**Open tape backup. Your data. Your tapes. Forever.**

Proposed repository: `spoolark` · Target: `v0.1.0-alpha.1` onward · Proposed license: Apache-2.0 · Specification baseline: 2026-11-24

SpoolArk is a self-hosted tape backup and recovery manager for Linux. GNU tar archives, the Linux SCSI tape driver, mt-st and standard utilities are the foundation; the Go service and SvelteKit web UI make them approachable. No LTFS, cloud account, proprietary archive decoder or external database is required.

This bundle is a **design and implementation specification**, not implemented or hardware-certified software. Copy its contents into the root of the new repository. Keep Markdown as the source of truth, so both humans and coding agents can edit and review the design in Git.

## Read in this order

| File | Purpose |
|---|---|
| [AGENTS.md](AGENTS.md) | Permanent instructions for Codex and other coding agents; start here. |
| [docs/MVP.md](docs/MVP.md) | Product requirements, workflows, acceptance criteria, scope boundaries. |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | Implementation design, Go modules, database, REST API, security, process supervision and deployment. |
| [docs/TAPE_FORMAT.md](docs/TAPE_FORMAT.md) | Normative raw-TAR media contract, independent recovery instructions, append and failure semantics. |
| [docs/ROADMAP.md](docs/ROADMAP.md) | Ordered implementation work packages, tests, release gates and future releases. |

The tape format and safety rules are normative. If a contradiction appears, stop the affected work, document it, and resolve it before real tape writes. The roadmap may change ordering but may not weaken safety requirements.

## Starting the repository

1. Create an empty `spoolark` repository on your chosen Git host. The name has not received trademark, domain or registry clearance.
2. Copy this bundle into the repository. Choose and add the license (Apache-2.0 recommended for broad adoption; AGPL-3.0 is an alternative if network-service copyleft matters). Do not imply that a license file already exists.
3. Commit the specifications before implementation. Give Codex the prompt below from the repository root.
4. Implement on an ordinary development Linux system using a file-backed tape simulator. Do not attach production cartridges to development builds.
5. Move to the physical LTO-5 qualification steps only after the simulation and security gates pass.

```text
Read AGENTS.md and all four docs/*.md files. Treat them as the implementation contract.
Begin with ROADMAP Phase 0, then Phase 1. Create a small, reviewable implementation plan
and a Git branch. Implement only the current phase, add tests and documentation, run the
checks, report results and update the traceability matrix. Use the simulator by default.
Do not write to a physical tape, erase media, change tape-driver settings, or advance a
hardware qualification gate without explicit operator approval and a sacrificial cartridge.
If an assumption is unsupported by the installed tools or hardware, fail closed and record
an ADR. Do not invent a custom archive format or replace GNU tar. Prefer the simplest
implementation satisfying the safety and recovery contract. Do not push or merge unless
I authorize it separately.
```

## Initial decisions at a glance

- Linux server; cross-platform browser. Native macOS/Windows tape drivers are out of scope.
- One standalone drive and one active tape job per instance for v0.1. One complete backup archive per job, fitting on one tape. Multiple jobs can be appended to qualified managed media; no automatic tape spanning or erase/reuse.
- Plain PAX/GNU TAR payloads, standard tape filemarks and small TAR metadata files. Full tape readback is mandatory before calling an archive verified.
- Stream backups without a full disk staging copy. For v0.1, web restores spool the selected entire archive to a private disk file, verify it, and extract into a new isolated destination. The disk-space cost is explicit. Streaming safe restore is later work.
- Real tape support is opt-in and hardware-profile gated. A demo/simulator is the default. Existing TAR tapes are imported read-only, never converted in place.
- Docker Compose services `backend` and `web`, with an optional native Linux deployment using the same core. Only the backend owns tape devices and the catalog.
- Initial supported filesystem policy: regular files, directories, symbolic and hard links subject to restore safety. No claims of complete filesystem imaging, ACL/xattr fidelity, snapshot consistency, encryption or sparse-file preservation in v0.1.

## Primary hardware qualification target

HP Ultrium 5-SCSI / LTO-5 on a Linux SAS host. Native capacity is approximately 1.5 TB decimal, not 1.5 TiB; do not assume advertised compressed capacity. Other LTO generations and adapters are supported only after explicit qualification. The development design is not a claim that any specific hardware combination already works.
