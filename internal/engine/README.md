# Simulator engine

`Engine.Execute(ctx, job)` accepts a scheduler-claimed PREFLIGHT job. The scheduler owns the active slot, cancellation and failed/cancelled/recovery outcomes. The engine owns RUNNING and VERIFYING transitions and publishes success atomically with exact readback evidence.

Construct the engine with its `Store`, `Drive`, administrator-configured `Sources` and private `Spool`. `BackupOptions` selects a source ID, relative selection, exclusions and descriptive source label. An empty selection includes the configured root's children and is recorded as `.` in the commit.

Initialization additionally requires `InitializeArmed`, an exact `InitializeJobID`, and `InitOptions` with a confirmed cartridge ID and current simulator generation. This capability belongs to a local CLI invocation. HTTP-serving engines use zero values for the arming fields. Initialization leaves the volume unenrolled. Explicit durable enrollment enables later managed appends.

Every backup scans the entire loaded media chain, checks UUID and header digest, validates the source plan, applies a conservative native-capacity budget and checks enrollment. The data and commit use checked closes. Commit authorization uses the same intent and retained lease. A separate full readback scan stages actual archive headers and compares raw padded data and commit digests before publication. Restore eligibility remains false until the restore planner validates the entire archive graph.

Progress persists accepted write bytes and independently observed readback bytes. Waiting-media rechecks preserve telemetry with `progress.FromSnapshot` and create fresh tape authority. Cancellation and uncertain writes never replay automatically. Failure hooks are unexported test instrumentation. Simulator fault controls belong to local tests and are absent from configuration and HTTP contracts.

Tests include ordinary GNU tar extraction after closing the catalog, actual SQLite trigger-induced publication failure, initialization and backup fault boundaries, stale local arming, cancellation at publication, revocation, byte corruption and actual scheduler media rechecks. All inputs are synthetic.
