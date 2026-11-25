<script lang="ts">
  import { onMount } from "svelte";
  import {
    API,
    APIError,
    bytes,
    instant,
    label,
    newRunKey,
    failureMessage,
    restoreIntegrityUsable,
    verifiedArchiveCount,
    historicalVerification,
    terminal,
  } from "../lib/api";
  import type {
    Session,
    SystemInfo,
    Job,
    Volume,
    Archive,
    Member,
    Profile,
    Schedule,
    Notification,
    Drive,
    BackupPlan,
    RestoreOptions,
    RestorePlan,
    DoctorReport,
  } from "../lib/api";
  import Badge from "../lib/components/Badge.svelte";
  import JobDetail from "../lib/components/JobDetail.svelte";
  import "../lib/app.css";

  const api = new API();
  const tabs = [
    "Overview",
    "Tapes",
    "Backup profiles",
    "Catalog",
    "Jobs",
    "Schedules",
    "Alerts",
    "System",
  ] as const;
  type Tab = (typeof tabs)[number];
  let tab = $state<Tab>("Overview");
  let initializing = $state(true);
  let authenticated = $state(false);
  let password = $state("");
  let busy = $state(false);
  let refreshing = $state(false);
  let error = $state("");
  let notice = $state("");
  let info = $state<SystemInfo | null>(null);
  let doctorReport = $state<DoctorReport | null>(null);
  let checkingDoctor = $state(false);
  let sources = $state<{ id: string }[]>([]);
  let drives = $state<Drive[]>([]);
  let volumes = $state<Volume[]>([]);
  let archives = $state<Archive[]>([]);
  let profiles = $state<Profile[]>([]);
  let jobs = $state<Job[]>([]);
  let schedules = $state<Schedule[]>([]);
  let alerts = $state<Notification[]>([]);
  let selectedJob = $state<Job | null>(null);
  let query = $state("");
  let results = $state<Member[]>([]);
  let searched = $state(false);
  let moreResults = $state(false);
  let searchCursor = $state("");
  let searchedQuery = $state("");
  let archiveCursor = $state("");
  let archiveID = $state("");
  let members = $state<Member[]>([]);
  let memberCursor = $state("");
  let moreMembers = $state(false);
  let selectedMembers = $state<number[]>([]);
  let allMembers = $state(true);
  let restoreRoot = $state("");
  let destination = $state("");
  let legacySource = $state("");
  let legacyConfirmed = $state(false);
  let backupPreview = $state<BackupPlan | null>(null);
  let backupProfileID = $state("");
  let restorePreview = $state<RestorePlan | null>(null);
  let restoreRunKey = $state("");
  let backupRunKey = $state("");
  let profileName = $state("");
  let profileSource = $state("");
  let profileSelection = $state("");
  let profileExclusions = $state("");
  let profileVolume = $state("");
  let scheduleProfile = $state("");
  let scheduleOperation = $state<"BACKUP" | "VERIFY">("BACKUP");
  let inventoryID = $state("");
  let inventoryLabel = $state("");
  let inventoryLocation = $state("");
  let inventoryBarcode = $state("");
  let inventoryPrevious = $state<Volume | null>(null);
  let cadence = $state<"daily" | "weekly">("daily");
  let timezone = $state("UTC");
  let localTime = $state("02:00");
  let weekday = $state(1);
  let cartridge = $state("");
  const driveKeys = new Map<string, string>();
  let jobFilter = $state("");
  const unread = $derived(alerts.filter((item) => !item.read_at).length);
  const verified = $derived(verifiedArchiveCount(archives));
  const attention = $derived(
    jobs.filter((item) =>
      ["FAILED", "RECOVERY_REQUIRED", "WAITING_MEDIA"].includes(item.state),
    ).length,
  );
  const activeJobs = $derived(jobs.filter((item) => !terminal(item.state)));
  const selectedArchive = $derived(
    archives.find((item) => item.id === archiveID),
  );
  const selectedVolume = $derived(
    volumes.find((item) => item.id === selectedArchive?.volume_id),
  );
  const currentRestoreAllowed = $derived(
    restoreIntegrityUsable(selectedArchive),
  );
  const historicalVerifiedAt = $derived(
    selectedArchive ? historicalVerification(selectedArchive) : undefined,
  );
  const verificationKeys = new Map<string, string>();
  $effect(() => {
    if (!currentRestoreAllowed) {
      restorePreview = null;
      restoreRunKey = "";
    }
  });

  const filteredJobs = $derived(
    jobs.filter((item) => !jobFilter || item.state === jobFilter),
  );

  function showError(value: unknown) {
    error =
      value instanceof APIError
        ? failureMessage(value.code, value.message)
        : value instanceof Error
          ? value.message
          : "The request could not be completed.";
    if (value instanceof APIError && value.status === 401)
      authenticated = false;
  }
  function navigate(next: Tab) {
    tab = next;
    error = "";
    notice = "";
  }
  async function refresh() {
    refreshing = true;
    try {
      const settled = await Promise.allSettled([
        api.get<SystemInfo>("/system/info").then((value) => {
          info = value;
        }),
        api.get<{ id: string }[]>("/sources").then((value) => {
          sources = value;
        }),
        api.get<Drive[]>("/drives").then((value) => {
          drives = value;
        }),
        api.get<Volume[]>("/volumes").then((value) => {
          volumes = value;
        }),
        api.page<Archive>("/archives?limit=100").then((value) => {
          archives = value.items;
          archiveCursor = value.cursor;
        }),
        api.get<Profile[]>("/backup-profiles").then((value) => {
          profiles = value;
        }),
        api.get<Job[]>("/jobs").then((value) => {
          jobs = value;
          if (selectedJob)
            selectedJob =
              value.find((job) => job.id === selectedJob?.id) || selectedJob;
        }),
        api.get<Schedule[]>("/schedules").then((value) => {
          schedules = value;
        }),
        api.get<Notification[]>("/notifications").then((value) => {
          alerts = value;
        }),
      ]);
      if (!profileSource && sources.length) profileSource = sources[0].id;
      if (!restoreRoot && info?.restore_roots.length)
        restoreRoot = info.restore_roots[0];
      const failed = settled.find((result) => result.status === "rejected");
      if (failed?.status === "rejected") throw failed.reason;
    } finally {
      refreshing = false;
    }
  }
  async function session() {
    const value = await api.get<Session>("/auth/session");
    api.session(value.csrf_token);
    authenticated = value.authenticated;
    if (authenticated) await refresh();
  }
  onMount(() => {
    void session()
      .catch(showError)
      .finally(() => {
        initializing = false;
      });
  });
  async function login(event: SubmitEvent) {
    event.preventDefault();
    busy = true;
    error = "";
    try {
      const anonymous = await api.get<Session>("/auth/session");
      api.session(anonymous.csrf_token);
      await api.mutate("/auth/login", { password });
      await session();
    } catch (value) {
      showError(value);
    } finally {
      password = "";
      busy = false;
    }
  }
  async function logout() {
    busy = true;
    error = "";
    try {
      await api.mutate("/auth/logout", {});
      authenticated = false;
      api.session("");
      selectedJob = null;
    } catch (value) {
      showError(value);
    } finally {
      busy = false;
    }
  }
  async function saveProfile(event: SubmitEvent) {
    event.preventDefault();
    busy = true;
    error = "";
    notice = "";
    try {
      await api.mutate("/backup-profiles", {
        name: profileName,
        source_id: profileSource,
        relative_selection: profileSelection,
        exclusions: profileExclusions.split("\n").filter(Boolean),
        target_volume_id: profileVolume,
        enabled: true,
      });
      profileName = "";
      profileSelection = "";
      profileExclusions = "";
      notice = "Backup profile saved.";
      await refresh();
    } catch (value) {
      showError(value);
    } finally {
      busy = false;
    }
  }
  async function previewBackup(profile: Profile) {
    busy = true;
    error = "";
    notice = "";
    backupPreview = null;
    try {
      backupPreview = await api.mutate<BackupPlan>(
        "/backup-profiles/" + encodeURIComponent(profile.id) + "/plan",
        {},
      );
      backupProfileID = profile.id;
      backupRunKey = newRunKey();
    } catch (value) {
      showError(value);
    } finally {
      busy = false;
    }
  }
  async function runBackup() {
    if (!backupPreview || !backupRunKey) return;
    busy = true;
    error = "";
    try {
      const job = await api.mutate<Job>(
        "/backup-profiles/" + encodeURIComponent(backupProfileID) + "/run",
        { plan_id: backupPreview.id, revision: backupPreview.revision },
        backupRunKey,
      );
      backupPreview = null;
      backupRunKey = "";
      selectedJob = job;
      tab = "Jobs";
      notice = "Backup job queued.";
      await refresh();
    } catch (value) {
      showError(value);
    } finally {
      busy = false;
    }
  }
  async function toggleProfile(profile: Profile) {
    busy = true;
    error = "";
    try {
      await api.mutate(
        "/backup-profiles/" + encodeURIComponent(profile.id),
        { revision: profile.revision, enabled: !profile.enabled },
        undefined,
        "PATCH",
      );
      await refresh();
    } catch (value) {
      showError(value);
    } finally {
      busy = false;
    }
  }
  async function search(event?: SubmitEvent, append = false) {
    event?.preventDefault();
    busy = true;
    error = "";
    try {
      const text = append ? searchedQuery : query;
      const params = new URLSearchParams({ q: text, limit: "100" });
      if (append && searchCursor) params.set("cursor", searchCursor);
      const value = await api.page<Member>("/catalog/search?" + params);
      results = append ? [...results, ...value.items] : value.items;
      searchCursor = value.cursor;
      moreResults = !!value.cursor;
      searchedQuery = text;
      searched = true;
    } catch (value) {
      showError(value);
    } finally {
      busy = false;
    }
  }
  async function openArchive(id: string) {
    archiveID = id;
    members = [];
    selectedMembers = [];
    allMembers = true;
    restorePreview = null;
    restoreRunKey = "";
    memberCursor = "";
    moreMembers = false;
    tab = "Catalog";
    await loadMembers();
  }
  async function moreArchives() {
    busy = true;
    error = "";
    try {
      const value = await api.page<Archive>(
        "/archives?limit=100&cursor=" + encodeURIComponent(archiveCursor),
      );
      archives = [...archives, ...value.items];
      archiveCursor = value.cursor;
    } catch (value) {
      showError(value);
    } finally {
      busy = false;
    }
  }
  async function loadMembers() {
    busy = true;
    error = "";
    try {
      const params = new URLSearchParams({ limit: "100" });
      if (memberCursor) params.set("cursor", memberCursor);
      const value = await api.page<Member>(
        "/archives/" + encodeURIComponent(archiveID) + "/members?" + params,
      );
      members = [...members, ...value.items];
      memberCursor = value.cursor;
      moreMembers = !!value.cursor;
    } catch (value) {
      showError(value);
    } finally {
      busy = false;
    }
  }
  function selectMember(id: number, checked: boolean) {
    selectedMembers = checked
      ? [...selectedMembers, id]
      : selectedMembers.filter((item) => item !== id);
    restorePreview = null;
    restoreRunKey = "";
  }
  function options(): RestoreOptions {
    return {
      archive_id: archiveID,
      member_ids: allMembers ? [] : selectedMembers,
      all: allMembers,
      restore_root_id: restoreRoot,
      destination_slug: destination,
      legacy_source_id: legacySource,
      legacy_profile:
        selectedVolume?.kind === "LEGACY" ? "simulator-plain-tar-v1" : "",
      legacy_confirmation: legacyConfirmed,
    };
  }
  async function refreshRestoreEvidence(): Promise<boolean> {
    if (!selectedArchive) return false;
    const fresh = await api.get<Archive>(
      "/archives/" + encodeURIComponent(selectedArchive.id),
    );
    archives = archives.map((archive) =>
      archive.id === fresh.id ? fresh : archive,
    );
    if (!restoreIntegrityUsable(fresh)) {
      error =
        "Restore requires current verified or observed integrity. Run read-only verification and refresh this archive.";
      return false;
    }
    return true;
  }
  async function previewRestore(event: SubmitEvent) {
    event.preventDefault();
    if (!currentRestoreAllowed) {
      error =
        "Restore requires current verified or observed integrity. Run read-only verification and refresh this archive.";
      return;
    }
    busy = true;
    error = "";
    restorePreview = null;
    try {
      if (!(await refreshRestoreEvidence())) return;
      restorePreview = await api.mutate<RestorePlan>(
        "/restores/plan",
        options(),
      );
      restoreRunKey = newRunKey();
    } catch (value) {
      showError(value);
    } finally {
      busy = false;
    }
  }
  async function runRestore() {
    if (!restorePreview || !restoreRunKey || !currentRestoreAllowed) return;
    busy = true;
    error = "";
    try {
      if (!(await refreshRestoreEvidence())) return;
      const job = await api.mutate<Job>(
        "/restores/run",
        { plan_id: restorePreview.id },
        restoreRunKey,
      );
      restorePreview = null;
      restoreRunKey = "";
      selectedJob = job;
      tab = "Jobs";
      notice = "Restore job queued.";
      await refresh();
    } catch (value) {
      showError(value);
    } finally {
      busy = false;
    }
  }
  function editInventory(volume?: Volume) {
    const selected = volume || volumes.find((item) => item.id === inventoryID);
    inventoryID = selected?.id || "";
    inventoryPrevious = selected ? { ...selected } : null;
    inventoryLabel = selected?.label || "";
    inventoryLocation = selected?.location || "";
    inventoryBarcode = selected?.barcode || "";
  }
  async function saveInventory(event: SubmitEvent) {
    event.preventDefault();
    busy = true;
    error = "";
    notice = "";
    try {
      const metadata = {
        label: inventoryLabel,
        location: inventoryLocation,
        barcode: inventoryBarcode,
      };
      let result: Volume;
      if (inventoryPrevious) {
        result = await api.mutate<Volume>(
          "/volumes/" + encodeURIComponent(inventoryPrevious.id),
          {
            ...metadata,
            previous_label: inventoryPrevious.label,
            previous_location: inventoryPrevious.location,
            previous_barcode: inventoryPrevious.barcode || "",
          },
          undefined,
          "PATCH",
        );
      } else {
        result = await api.mutate<Volume>("/volumes", metadata);
      }
      editInventory(result);
      notice = "Catalog metadata saved.";
      await refresh();
    } catch (value) {
      showError(value);
    } finally {
      busy = false;
    }
  }
  async function saveSchedule(event: SubmitEvent) {
    event.preventDefault();
    busy = true;
    error = "";
    notice = "";
    try {
      await api.mutate("/schedules", {
        profile_id: scheduleProfile,
        operation: scheduleOperation,
        cadence,
        timezone,
        local_time: localTime,
        weekday: Number(weekday),
        enabled: true,
      });
      notice = "Schedule saved.";
      await refresh();
    } catch (value) {
      showError(value);
    } finally {
      busy = false;
    }
  }
  async function toggleSchedule(schedule: Schedule) {
    busy = true;
    error = "";
    try {
      await api.mutate(
        "/schedules/" + encodeURIComponent(schedule.id),
        { revision: schedule.revision, enabled: !schedule.enabled },
        undefined,
        "PATCH",
      );
      await refresh();
    } catch (value) {
      showError(value);
    } finally {
      busy = false;
    }
  }
  async function markRead(item: Notification) {
    busy = true;
    error = "";
    try {
      await api.mutate("/notifications/" + item.id + "/read", {});
      await refresh();
    } catch (value) {
      showError(value);
    } finally {
      busy = false;
    }
  }
  async function insertCartridge(event: SubmitEvent) {
    event.preventDefault();
    busy = true;
    error = "";
    try {
      await api.mutate("/drives/simulator/insert", { cartridge_id: cartridge });
      notice = "Simulator cartridge inserted.";
      await refresh();
    } catch (value) {
      showError(value);
    } finally {
      busy = false;
    }
  }
  async function driveAction(action: "inspect" | "rewind" | "unload") {
    busy = true;
    error = "";
    const key = driveKeys.get(action) || newRunKey();
    driveKeys.set(action, key);
    try {
      const job = await api.mutate<Job>("/drives/simulator/" + action, {}, key);
      driveKeys.delete(action);
      selectedJob = job;
      tab = "Jobs";
      await refresh();
    } catch (value) {
      showError(value);
    } finally {
      busy = false;
    }
  }
  function invalidateRestore() {
    restorePreview = null;
    restoreRunKey = "";
  }
  async function verifyArchive() {
    if (!selectedArchive) return;
    const volume = selectedArchive.volume_id;
    const key = verificationKeys.get(volume) || newRunKey();
    verificationKeys.set(volume, key);
    busy = true;
    error = "";
    try {
      const job = await api.mutate<Job>(
        "/volumes/" + encodeURIComponent(volume) + "/verify",
        {},
        key,
      );
      verificationKeys.delete(volume);
      selectedJob = job;
      tab = "Jobs";
      notice = "Read-only verification job submitted.";
      await refresh();
    } catch (value) {
      showError(value);
    } finally {
      busy = false;
    }
  }
  async function runDoctor() {
    checkingDoctor = true;
    error = "";
    try {
      doctorReport = await api.get<DoctorReport>("/system/doctor");
    } catch (value) {
      showError(value);
    } finally {
      checkingDoctor = false;
    }
  }
  async function seal(volume: Volume) {
    if (
      !confirm(
        "Seal " +
          volume.label +
          "? Sealing blocks further backups to this tape.",
      )
    )
      return;
    busy = true;
    error = "";
    try {
      await api.mutate(
        "/volumes/" + encodeURIComponent(volume.id) + "/seal",
        {},
      );
      notice = "Tape sealed.";
      await refresh();
    } catch (value) {
      showError(value);
    } finally {
      busy = false;
    }
  }
  function inspectJob(job: Job) {
    selectedJob = job;
    tab = "Jobs";
  }
</script>

<svelte:head
  ><title>TapePilot · {authenticated ? tab : "Sign in"}</title><meta
    name="robots"
    content="noindex,nofollow"
  /><meta
    name="description"
    content="TapePilot private tape backup and recovery console."
  /></svelte:head
>
<a class="skip" href="#main">Skip to content</a>
{#if initializing}
  <main tabindex="-1" id="main" class="login-page">
    <div class="loading" role="status">Connecting to TapePilot…</div>
  </main>
{:else if !authenticated}
  <main tabindex="-1" id="main" class="login-page">
    <div class="login-brand">
      TapePilot<span>Take control of your tape backups.</span>
    </div>
    <section class="login-card">
      <p class="eyebrow">Operator access</p>
      <h1>Sign in</h1>
      <p class="muted">Manage your backup catalog and recovery jobs.</p>
      {#if error}<p class="inline-error" role="alert">{error}</p>{/if}
      <form onsubmit={login}>
        <label for="password">Administrator password</label><input
          id="password"
          type="password"
          bind:value={password}
          required
          autocomplete="current-password"
        /><button disabled={busy} class="login-submit" type="submit"
          >{busy ? "Signing in…" : "Sign in"}</button
        >
      </form>
      <p class="field-help login-help">
        Create the administrator locally with the TapePilot CLI.
      </p>
    </section>
  </main>
{:else}
  <div class="app-shell">
    <aside class="sidebar">
      <a class="brand" href="#main" onclick={() => navigate("Overview")}
        >TapePilot<span>Backup & recovery</span></a
      >
      <nav aria-label="Main navigation">
        {#each tabs as name}<button
            class:active={tab === name}
            aria-label={name}
            aria-current={tab === name ? "page" : undefined}
            onclick={() => navigate(name)}
            >{name}{#if name === "Alerts" && unread}<span
                class="nav-count"
                aria-label={unread + " unread"}>{unread}</span
              >{/if}</button
          >{/each}
      </nav>
      <div class="sidebar-footer">
        <span class="mode"
          ><span aria-hidden="true"></span>{info
            ? label(info.mode) + " mode"
            : "Mode unavailable"}</span
        ><button class="logout" onclick={logout} disabled={busy}
          >Sign out</button
        >
      </div>
    </aside>
    <main tabindex="-1" id="main" class="workspace">
      <div class="topbar">
        <span>Private backup console</span><button
          class="text-button"
          onclick={() => {
            void refresh().catch(showError);
          }}
          disabled={refreshing}
          >{refreshing ? "Refreshing…" : "Refresh data"}</button
        >
      </div>
      {#if error}<div class="message" role="alert">
          <span>{error}</span><button
            class="text-button"
            aria-label="Dismiss error"
            onclick={() => {
              error = "";
            }}>Dismiss</button
          >
        </div>{/if}
      {#if notice}<div class="message success" role="status">
          <span>{notice}</span><button
            class="text-button"
            aria-label="Dismiss notification"
            onclick={() => {
              notice = "";
            }}>Dismiss</button
          >
        </div>{/if}
      {#if tab === "Overview"}
        <div class="page-heading">
          <div>
            <p class="eyebrow">Overview</p>
            <h1>Backup status</h1>
            <p>Archive evidence, loaded media and work in progress.</p>
          </div>
          <button onclick={() => navigate("Backup profiles")}
            >Plan a backup</button
          >
        </div>
        <div class="metrics">
          <div>
            <span>Verified archives in view</span><strong>{verified}</strong
            ><small>{archives.length} archives loaded</small>
          </div>
          <div>
            <span>Tapes in catalog</span><strong>{volumes.length}</strong><small
              >Available offline</small
            >
          </div>
          <div>
            <span>Recent jobs needing attention</span><strong
              >{attention}</strong
            ><small>Waiting, failed or uncertain</small>
          </div>
        </div>
        <div class="split">
          <section class="panel">
            <div class="panel-heading">
              <h2>Current work</h2>
              <button class="text-button" onclick={() => navigate("Jobs")}
                >View all jobs</button
              >
            </div>
            {#if activeJobs.length}<ul class="row-list">
                {#each activeJobs.slice(0, 5) as job}<li>
                    <div class="row-head">
                      <div>
                        <h3>{label(job.kind)}</h3>
                        <p>{instant(job.created_at)}</p>
                      </div>
                      <Badge value={job.state} />
                    </div>
                    <button class="text-button" onclick={() => inspectJob(job)}
                      >Open job</button
                    >
                  </li>{/each}
              </ul>{:else}<div class="empty">
                <strong>No active jobs</strong>Plan a backup or choose an
                archive to restore.
              </div>{/if}
          </section>
          <section class="panel">
            <div class="panel-heading">
              <h2>Loaded drive</h2>
              <button class="text-button" onclick={() => navigate("Tapes")}
                >Manage tapes</button
              >
            </div>
            {#if drives.length}{#each drives.slice(0, 1) as drive}<p
                  class="drive-title"
                >
                  {drive.mode === "simulator" ? "Virtual tape drive" : drive.id}
                </p>
                <dl class="detail-grid">
                  <div>
                    <dt>Cartridge</dt>
                    <dd>{drive.cartridge_id || "No cartridge reported"}</dd>
                  </div>
                  <div>
                    <dt>Status</dt>
                    <dd>{drive.status ? label(drive.status) : "Unknown"}</dd>
                  </div>
                </dl>
                <p class="compact muted">
                  {drive.mode === "simulator"
                    ? "Disk-backed simulator. Physical hardware is unqualified."
                    : "Physical operation requires an approved hardware profile."}
                </p>{/each}{:else}<div class="empty">
                <strong>No drive information</strong>Refresh to check backend
                status.
              </div>{/if}
          </section>
        </div>
        <section class="panel recent">
          <div class="panel-heading">
            <h2>Archives in view</h2>
            <button class="text-button" onclick={() => navigate("Catalog")}
              >Browse catalog</button
            >
          </div>
          {#if archives.length}<div class="table-wrap">
              <table>
                <thead
                  ><tr
                    ><th scope="col">Source</th><th scope="col">Created</th><th
                      scope="col">Stream size</th
                    ><th scope="col">Integrity</th><th scope="col"
                      ><span class="sr-only">Action</span></th
                    ></tr
                  ></thead
                ><tbody
                  >{#each archives.slice(0, 5) as archive}<tr
                      ><td>{archive.source_label}</td><td
                        >{instant(archive.created_at)}</td
                      ><td>{bytes(archive.stream_bytes)}</td><td
                        ><Badge value={archive.integrity} /></td
                      ><td
                        ><button
                          class="text-button"
                          onclick={() => {
                            void openArchive(archive.id);
                          }}>Browse</button
                        ></td
                      ></tr
                    >{/each}</tbody
                >
              </table>
            </div>{:else}<div class="empty">
              <strong>Your catalog is empty</strong>Completed backups appear
              here after exact readback verification.
            </div>{/if}
        </section>
      {:else if tab === "Tapes"}
        <div class="page-heading">
          <div>
            <p class="eyebrow">Media inventory</p>
            <h1>Tapes & drives</h1>
            <p>
              On-media identity and catalog protection are checked before
              writes.
            </p>
          </div>
        </div>
        <div class="split">
          <section class="panel">
            <h2>Drive status</h2>
            {#each drives as drive}<dl class="detail-grid">
                <div>
                  <dt>Drive</dt>
                  <dd>{drive.id}</dd>
                </div>
                <div>
                  <dt>Mode</dt>
                  <dd>{label(drive.mode)}</dd>
                </div>
                <div>
                  <dt>Cartridge</dt>
                  <dd>{drive.cartridge_id || "None reported"}</dd>
                </div>
                <div>
                  <dt>Capacity</dt>
                  <dd>{bytes(drive.capacity_bytes)}</dd>
                </div>
                <div>
                  <dt>Recorded bytes</dt>
                  <dd>{bytes(drive.used_bytes)}</dd>
                </div>
                <div>
                  <dt>Write protection</dt>
                  <dd>
                    {drive.write_protected === undefined
                      ? "Unknown"
                      : drive.write_protected
                        ? "Protected"
                        : "Unprotected"}
                  </dd>
                </div>
              </dl>
              {#if drive.uncertain}<p class="inline-error">
                  Media requires read-only investigation before another write.
                </p>{/if}
              {#if drive.mode === "simulator"}<div class="actions">
                  <button
                    class="secondary"
                    onclick={() => {
                      void driveAction("inspect");
                    }}
                    disabled={busy}>Inspect drive</button
                  ><button
                    class="secondary"
                    onclick={() => {
                      void driveAction("rewind");
                    }}
                    disabled={busy}>Rewind</button
                  ><button
                    class="secondary"
                    onclick={() => {
                      void driveAction("unload");
                    }}
                    disabled={busy}>Unload</button
                  >
                </div>{/if}
            {/each}{#if !drives.length}<p class="empty">
                No drive information available.
              </p>{/if}
          </section>
          <section class="panel">
            <h2>Insert a simulator cartridge</h2>
            <p class="compact muted">
              Use the exact ID of an existing synthetic cartridge.
              Initialization and write enrollment use the local CLI.
            </p>
            <form onsubmit={insertCartridge}>
              <label for="cartridge"
                >Cartridge ID<input
                  id="cartridge"
                  bind:value={cartridge}
                  required
                  pattern={"[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}"}
                  maxlength="64"
                /></label
              >
              <div class="actions">
                <button
                  type="submit"
                  disabled={busy || info?.mode !== "simulator"}
                  >Insert cartridge</button
                >
              </div>
            </form>
          </section>
        </div>
        <section class="panel recent">
          <h2>Catalog metadata</h2>
          <form onsubmit={saveInventory} class="form-grid">
            <label class="full" for="inventory-id"
              >Inventory entry<select
                id="inventory-id"
                bind:value={inventoryID}
                onchange={() => editInventory()}
              >
                <option value="">New offline inventory entry</option>
                {#each volumes as volume}<option value={volume.id}
                    >{volume.label} · {volume.id}</option
                  >{/each}
              </select></label
            >
            <label for="inventory-label"
              >Label<input
                id="inventory-label"
                bind:value={inventoryLabel}
                maxlength="256"
                required
              /></label
            >
            <label for="inventory-location"
              >Location<input
                id="inventory-location"
                bind:value={inventoryLocation}
                maxlength="256"
              /></label
            >
            <label class="full" for="inventory-barcode"
              >Barcode (optional)<input
                id="inventory-barcode"
                bind:value={inventoryBarcode}
                maxlength="128"
                autocomplete="off"
                spellcheck="false"
              /><span class="field-help"
                >Recorded as catalog metadata. Tape identity is checked from its
                on-media UUID.</span
              ></label
            >
            <div class="actions full">
              <button type="submit" disabled={busy}
                >Save catalog metadata</button
              >
            </div>
          </form>
        </section>
        <section class="panel recent">
          <h2>Tape catalog</h2>
          {#if volumes.length}<div class="table-wrap">
              <table>
                <thead
                  ><tr
                    ><th scope="col">Tape</th><th scope="col">Location</th><th
                      scope="col">Barcode</th
                    ><th scope="col">Format</th><th scope="col">State</th><th
                      scope="col"><span class="sr-only">Actions</span></th
                    ></tr
                  ></thead
                ><tbody
                  >{#each volumes as volume}<tr
                      ><td
                        ><strong>{volume.label}</strong>
                        <p class="identifier">{volume.id}</p></td
                      ><td>{volume.location || "Unspecified"}</td><td
                        >{volume.barcode || "Unspecified"}</td
                      ><td
                        >{label(volume.kind)}
                        <p class="compact muted">
                          {volume.generation || "Unknown generation"}
                        </p></td
                      ><td><Badge value={volume.state} /></td><td
                        ><button
                          class="text-button"
                          onclick={() => editInventory(volume)}
                          disabled={busy}>Edit metadata</button
                        >{#if volume.state === "MANAGED"}<button
                            class="text-button"
                            onclick={() => {
                              void seal(volume);
                            }}
                            disabled={busy}>Seal tape</button
                          >{/if}<a
                          href={"/api/v1/volumes/" +
                            encodeURIComponent(volume.id) +
                            "/recovery-descriptor"}
                          download>Recovery descriptor</a
                        ></td
                      ></tr
                    >{/each}</tbody
                >
              </table>
            </div>{:else}<div class="empty">
              <strong>No tapes registered</strong>Initialize a simulator
              cartridge with the local CLI or import a supported archive.
            </div>{/if}
        </section>
      {:else if tab === "Backup profiles"}
        <div class="page-heading">
          <div>
            <p class="eyebrow">Backup setup</p>
            <h1>Backup profiles</h1>
            <p>Preview an allowlisted source before submitting a backup job.</p>
          </div>
        </div>
        <div class="split">
          <section class="panel">
            <h2>Saved profiles</h2>
            {#if profiles.length}<ul class="row-list">
                {#each profiles as profile}<li>
                    <div class="row-head">
                      <div>
                        <h3>{profile.name}</h3>
                        <p>
                          {profile.source_id}{profile.relative_selection
                            ? "/" + profile.relative_selection
                            : " · entire source"}
                        </p>
                        <p>
                          Target {volumes.find(
                            (item) => item.id === profile.target_volume_id,
                          )?.label || profile.target_volume_id}
                        </p>
                      </div>
                      <span class="muted compact"
                        >{profile.enabled ? "Enabled" : "Paused"}</span
                      >
                    </div>
                    <div class="actions">
                      <button
                        onclick={() => {
                          void previewBackup(profile);
                        }}
                        disabled={busy || !profile.enabled}
                        >Preview backup</button
                      ><button
                        class="secondary"
                        onclick={() => {
                          void toggleProfile(profile);
                        }}
                        disabled={busy}
                        >{profile.enabled
                          ? "Pause profile"
                          : "Enable profile"}</button
                      >
                    </div>
                  </li>{/each}
              </ul>{:else}<div class="empty">
                <strong>No backup profiles</strong>Choose a configured source
                and an initialized target tape.
              </div>{/if}
          </section>
          <section class="panel">
            <h2>Create profile</h2>
            <form onsubmit={saveProfile} class="form-grid">
              <label class="full" for="profile-name"
                >Profile name<input
                  id="profile-name"
                  bind:value={profileName}
                  maxlength="256"
                  required
                /></label
              ><label for="profile-source"
                >Source<select
                  id="profile-source"
                  aria-label="Source"
                  bind:value={profileSource}
                  required
                  ><option value="" disabled>Select a source</option
                  >{#each sources as source}<option value={source.id}
                      >{source.id}</option
                    >{/each}</select
                ></label
              ><label for="profile-volume"
                >Target tape<select
                  id="profile-volume"
                  aria-label="Target tape"
                  bind:value={profileVolume}
                  required
                  ><option value="" disabled>Select a tape</option
                  >{#each volumes.filter((item) => item.state === "MANAGED") as volume}<option
                      value={volume.id}>{volume.label}</option
                    >{/each}</select
                ></label
              ><label class="full" for="profile-selection"
                >Relative subtree<input
                  id="profile-selection"
                  bind:value={profileSelection}
                  placeholder="Leave empty for the entire source"
                  maxlength="4096"
                /><span class="field-help"
                  >Relative to the configured source root.</span
                ></label
              ><label class="full" for="profile-exclusions"
                >Excluded relative paths<textarea
                  id="profile-exclusions"
                  bind:value={profileExclusions}
                  placeholder="One path per line"
                ></textarea></label
              >
              <div class="actions full">
                <button
                  type="submit"
                  disabled={busy ||
                    !sources.length ||
                    !volumes.some((item) => item.state === "MANAGED")}
                  >Save profile</button
                >
              </div>
            </form>
          </section>
        </div>
        {#if backupPreview}<section
            class="panel preview recent"
            aria-label="Backup preview"
          >
            <div class="panel-heading">
              <h2>Review backup</h2>
              <span class="compact muted"
                >Expires {instant(backupPreview.expires_at)}</span
              >
            </div>
            <dl class="detail-grid">
              <div>
                <dt>Entries</dt>
                <dd>{backupPreview.entries.toLocaleString()}</dd>
              </div>
              <div>
                <dt>Logical source bytes</dt>
                <dd>{bytes(backupPreview.logical_bytes)}</dd>
              </div>
              <div>
                <dt>Estimated stream</dt>
                <dd>{bytes(backupPreview.estimated_bytes)}</dd>
              </div>
            </dl>
            <p class="compact muted">
              Live sources are best-effort. Success requires a complete archive,
              checked closes and exact readback.
            </p>
            <div class="actions">
              <button
                class="secondary"
                onclick={() => {
                  backupPreview = null;
                  backupRunKey = "";
                }}
                disabled={busy}>Discard preview</button
              ><button
                onclick={() => {
                  void runBackup();
                }}
                disabled={busy}>{busy ? "Submitting…" : "Run backup"}</button
              >
            </div>
          </section>{/if}
      {:else if tab === "Catalog"}
        <div class="page-heading">
          <div>
            <p class="eyebrow">Offline catalog</p>
            <h1>Find & restore</h1>
            <p>Search archived paths without loading a tape.</p>
          </div>
        </div>
        <section class="panel">
          <form
            class="search-form"
            onsubmit={(event) => {
              void search(event);
            }}
          >
            <label for="search"
              >Archived path<input
                id="search"
                type="search"
                bind:value={query}
                maxlength="1024"
                placeholder="Search for a file or directory"
              /></label
            ><button type="submit" disabled={busy}>Search catalog</button>
          </form>
          {#if results.length}<div class="table-wrap">
              <table>
                <thead
                  ><tr
                    ><th scope="col">Path</th><th scope="col">Tape</th><th
                      scope="col">Integrity</th
                    ><th scope="col"><span class="sr-only">Action</span></th
                    ></tr
                  ></thead
                ><tbody
                  >{#each results as member (member.id)}<tr
                      ><td class="filename">{member.path}</td><td
                        >{member.volume_label || member.volume_id}</td
                      ><td><Badge value={member.integrity || "UNKNOWN"} /></td
                      ><td
                        ><button
                          class="text-button"
                          onclick={() => {
                            if (member.archive_id)
                              void openArchive(member.archive_id);
                          }}>Open archive</button
                        ></td
                      ></tr
                    >{/each}</tbody
                >
              </table>
            </div>
            {#if moreResults}<div class="actions">
                <button
                  class="secondary"
                  onclick={() => {
                    void search(undefined, true);
                  }}
                  disabled={busy}>Load more matches</button
                >
              </div>{/if}{:else if searched}<div class="empty">
              <strong>No matching paths</strong>Try another part of the file or
              directory name.
            </div>{/if}
        </section>
        <section class="panel recent">
          <div class="panel-heading"><h2>Browse an archive</h2></div>
          <label for="archive"
            >Archive<select
              id="archive"
              aria-label="Archive"
              value={archiveID}
              onchange={(event) => {
                if (event.currentTarget.value)
                  void openArchive(event.currentTarget.value);
              }}
              ><option value="">Choose an archive</option
              >{#each archives as archive}<option value={archive.id}
                  >{archive.source_label} · {instant(archive.created_at)} · {archive.id.slice(
                    0,
                    8,
                  )}</option
                >{/each}</select
            ></label
          >{#if archiveCursor}<div class="actions">
              <button
                class="secondary"
                onclick={() => {
                  void moreArchives();
                }}
                disabled={busy}>Load more archives</button
              >
            </div>{/if}{#if selectedArchive}<dl class="detail-grid">
              <div>
                <dt>Tape required</dt>
                <dd>{selectedVolume?.label || selectedArchive.volume_id}</dd>
              </div>
              <div>
                <dt>Current integrity</dt>
                <dd><Badge value={selectedArchive.integrity} /></dd>
              </div>
              <div>
                <dt>Integrity basis</dt>
                <dd>{label(selectedArchive.integrity_basis)}</dd>
              </div>
              <div>
                <dt>Full spool size</dt>
                <dd>{bytes(selectedArchive.stream_bytes)}</dd>
              </div>
            </dl>
            {#if historicalVerifiedAt}<p class="compact muted">
                Last successful verification (historical) <time
                  datetime={historicalVerifiedAt}
                  >{instant(historicalVerifiedAt)}</time
                >
              </p>{/if}
            <p class="identifier">SHA-256 {selectedArchive.sha256}</p>
            {#if !currentRestoreAllowed}<p class="inline-error" role="alert">
                Current integrity is {label(selectedArchive.integrity)}. Restore
                is unavailable until fresh read-only verification establishes
                usable evidence.
              </p>{/if}
            {#if selectedVolume?.kind === "MANAGED"}<div class="actions">
                <button
                  class="secondary"
                  onclick={() => {
                    void verifyArchive();
                  }}
                  disabled={busy}>Verify tape read-only</button
                >
                <p class="field-help">
                  Load the required tape, run verification, then refresh the
                  archive evidence.
                </p>
              </div>{:else if selectedVolume?.kind === "LEGACY"}<p
                class="compact muted"
              >
                Legacy restoration compares the complete spool with its recorded
                observed digest. Refresh legacy evidence using a locally
                confirmed read-only import of the configured source.
              </p>{/if}
            <label class="checkbox"
              ><input
                type="checkbox"
                bind:checked={allMembers}
                onchange={() => {
                  restorePreview = null;
                  restoreRunKey = "";
                }}
              />Restore the full archive</label
            >
            {#if members.length}<div class="table-wrap">
                <table>
                  <thead
                    ><tr
                      >{#if !allMembers}<th scope="col">Select</th>{/if}<th
                        scope="col">Archived path</th
                      ><th scope="col">Size</th><th scope="col">Type</th></tr
                    ></thead
                  ><tbody
                    >{#each members as member (member.id)}<tr
                        >{#if !allMembers}<td
                            ><input
                              type="checkbox"
                              aria-label={"Select " + member.path}
                              checked={selectedMembers.includes(member.id)}
                              onchange={(event) =>
                                selectMember(
                                  member.id,
                                  event.currentTarget.checked,
                                )}
                            /></td
                          >{/if}<td class="filename"
                          >{member.path}{#if member.link_target}<span
                              class="muted"
                            >
                              → {member.link_target}</span
                            >{/if}</td
                        ><td>{bytes(member.size)}</td><td
                          >{(
                            {
                              "0": "File",
                              "5": "Directory",
                              "1": "Hardlink",
                              "2": "Symlink",
                            } as Record<string, string>
                          )[member.type] || "Unsupported"}</td
                        ></tr
                      >{/each}</tbody
                  >
                </table>
              </div>
              {#if moreMembers}<div class="actions">
                  <button
                    class="secondary"
                    onclick={() => {
                      void loadMembers();
                    }}
                    disabled={busy}>Load more members</button
                  >
                </div>{/if}{:else}<p class="empty">No members loaded.</p>{/if}
            <form class="form-grid restore-form" onsubmit={previewRestore}>
              <label for="restore-root"
                >Restore root<select
                  id="restore-root"
                  aria-label="Restore root"
                  bind:value={restoreRoot}
                  onchange={invalidateRestore}
                  required
                  ><option value="">Choose a configured root</option
                  >{#each info?.restore_roots || [] as root}<option value={root}
                      >{root}</option
                    >{/each}</select
                ></label
              ><label for="destination"
                >New destination name<input
                  id="destination"
                  bind:value={destination}
                  oninput={invalidateRestore}
                  required
                  pattern={"[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}"}
                  maxlength="64"
                  placeholder="recovered-documents"
                /></label
              >{#if selectedVolume?.kind === "LEGACY"}<label for="legacy-source"
                  >Legacy source<select
                    id="legacy-source"
                    aria-label="Legacy source"
                    bind:value={legacySource}
                    onchange={invalidateRestore}
                    required
                    ><option value="">Confirm the source</option
                    >{#each info?.legacy_roots || [] as root}<option
                        value={root}>{root}</option
                      >{/each}</select
                  ></label
                ><label class="checkbox"
                  ><input
                    type="checkbox"
                    bind:checked={legacyConfirmed}
                    onchange={invalidateRestore}
                    required
                  />I confirm this is the selected legacy source.</label
                >{/if}
              <div class="actions full">
                <button
                  type="submit"
                  disabled={busy ||
                    !currentRestoreAllowed ||
                    (!allMembers && !selectedMembers.length)}
                  >Preview restore</button
                >
              </div>
            </form>
          {/if}
        </section>
        {#if restorePreview}<section
            class="panel preview recent"
            aria-label="Restore preview"
          >
            <h2>Review restore</h2>
            <dl class="detail-grid">
              <div>
                <dt>Selected members</dt>
                <dd>{restorePreview.selected_members}</dd>
              </div>
              <div>
                <dt>Added dependencies</dt>
                <dd>{restorePreview.expanded_members}</dd>
              </div>
              <div>
                <dt>Logical output bytes</dt>
                <dd>{bytes(restorePreview.output_bytes)}</dd>
              </div>
              <div>
                <dt>Verified spool required</dt>
                <dd>{bytes(restorePreview.spool_bytes)}</dd>
              </div>
              <div>
                <dt>Destination</dt>
                <dd>
                  {restorePreview.restore_root_id}/{restorePreview.destination_slug}
                </dd>
              </div>
            </dl>
            <ul class="preview-list">
              {#each restorePreview.members as member}<li class="filename">
                  {member.path}
                </li>{/each}
            </ul>
            {#if restorePreview.truncated}<p class="compact muted">
                The preview shows the first 200 members. All selected members
                are included in the count.
              </p>{/if}
            <p class="compact muted">
              Restoration verifies the complete archive before extraction and
              publishes a fresh directory. Ownership, ACLs and extended
              attributes are excluded.
            </p>
            <div class="actions">
              <button
                class="secondary"
                onclick={() => {
                  restorePreview = null;
                  restoreRunKey = "";
                }}
                disabled={busy}>Discard preview</button
              ><button
                onclick={() => {
                  void runRestore();
                }}
                disabled={busy || !currentRestoreAllowed}
                >{busy ? "Submitting…" : "Run restore"}</button
              >
            </div>
          </section>{/if}
      {:else if tab === "Jobs"}
        <div class="page-heading">
          <div>
            <p class="eyebrow">Execution history</p>
            <h1>Jobs</h1>
            <p>Readback verification and uncertain outcomes stay visible.</p>
          </div>
        </div>
        <div class="split">
          <section class="panel">
            <label for="job-filter"
              >State<select
                id="job-filter"
                aria-label="State"
                bind:value={jobFilter}
                ><option value="">All states</option
                >{#each ["QUEUED", "PREFLIGHT", "WAITING_MEDIA", "RUNNING", "VERIFYING", "SUCCEEDED", "FAILED", "CANCEL_REQUESTED", "CANCELLED", "RECOVERY_REQUIRED"] as state}<option
                    value={state}>{label(state)}</option
                  >{/each}</select
              ></label
            >{#if filteredJobs.length}<ul class="row-list">
                {#each filteredJobs as job (job.id)}<li>
                    <div class="row-head">
                      <div>
                        <h3>{label(job.kind)}</h3>
                        <p>{instant(job.created_at)}</p>
                      </div>
                      <Badge value={job.state} />
                    </div>
                    <button
                      class="text-button"
                      onclick={() => {
                        selectedJob = job;
                      }}>View progress & events</button
                    >
                  </li>{/each}
              </ul>{:else}<div class="empty">
                <strong>No jobs in this view</strong>Submitted backup and
                restore work appears here.
              </div>{/if}
          </section>
          {#if selectedJob}<JobDetail
              job={selectedJob}
              {api}
              onrefresh={refresh}
              onerror={showError}
            />{:else}<section class="panel empty">
              <strong>Select a job</strong>View its phase, accepted bytes,
              independent readback and event history.
            </section>{/if}
        </div>
      {:else if tab === "Schedules"}
        <div class="page-heading">
          <div>
            <p class="eyebrow">Automation</p>
            <h1>Schedules</h1>
            <p>
              Schedule backups or read-only verification for a profile’s exact
              tape.
            </p>
          </div>
        </div>
        <div class="split">
          <section class="panel">
            <h2>Saved schedules</h2>
            {#if schedules.length}<ul class="row-list">
                {#each schedules as schedule}<li>
                    <div class="row-head">
                      <div>
                        <h3>
                          {profiles.find(
                            (profile) => profile.id === schedule.profile_id,
                          )?.name || schedule.profile_id}
                        </h3>
                        <p>
                          {schedule.operation === "VERIFY"
                            ? "Verify"
                            : "Backup"} · {schedule.cadence === "weekly"
                            ? [
                                "Sunday",
                                "Monday",
                                "Tuesday",
                                "Wednesday",
                                "Thursday",
                                "Friday",
                                "Saturday",
                              ][schedule.weekday]
                            : "Daily"} at {schedule.local_time} · {schedule.timezone}
                        </p>
                      </div>
                      <span class="muted compact"
                        >{schedule.enabled ? "Enabled" : "Paused"}</span
                      >
                    </div>
                    <div class="actions">
                      <button
                        class="secondary"
                        onclick={() => {
                          void toggleSchedule(schedule);
                        }}
                        disabled={busy}
                        >{schedule.enabled
                          ? "Pause schedule"
                          : "Enable schedule"}</button
                      >
                    </div>
                  </li>{/each}
              </ul>{:else}<div class="empty">
                <strong>No scheduled work</strong>Create a schedule for an
                existing backup profile.
              </div>{/if}
          </section>
          <section class="panel">
            <h2>Create schedule</h2>
            <form onsubmit={saveSchedule} class="form-grid">
              <label class="full" for="schedule-operation"
                >Operation<select
                  id="schedule-operation"
                  aria-label="Operation"
                  bind:value={scheduleOperation}
                  ><option value="BACKUP">Backup</option><option value="VERIFY"
                    >Read-only verification</option
                  ></select
                ></label
              >
              <label class="full" for="schedule-profile"
                >Backup profile<select
                  id="schedule-profile"
                  aria-label="Backup profile"
                  bind:value={scheduleProfile}
                  required
                  ><option value="">Choose a profile</option
                  >{#each profiles.filter((item) => item.enabled) as profile}<option
                      value={profile.id}>{profile.name}</option
                    >{/each}</select
                ></label
              ><label for="cadence"
                >Cadence<select
                  id="cadence"
                  aria-label="Cadence"
                  bind:value={cadence}
                  ><option value="daily">Daily</option><option value="weekly"
                    >Weekly</option
                  ></select
                ></label
              ><label for="time"
                >Local time<input
                  id="time"
                  type="time"
                  bind:value={localTime}
                  required
                /></label
              >{#if cadence === "weekly"}<label for="weekday"
                  >Day<select id="weekday" aria-label="Day" bind:value={weekday}
                    >{#each ["Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"] as day, index}<option
                        value={index}>{day}</option
                      >{/each}</select
                  ></label
                >{/if}<label for="timezone"
                >IANA timezone<input
                  id="timezone"
                  bind:value={timezone}
                  required
                  maxlength="128"
                  placeholder="America/Chicago"
                /></label
              >
              <p class="field-help full">
                Verification rereads the target tape and works without the
                source tree or write enrollment. A repeated daylight-saving time
                runs once. A skipped local time runs at the first later valid
                minute that day. Missed runs coalesce to the latest eligible
                slot.
              </p>
              <div class="actions full">
                <button
                  type="submit"
                  disabled={busy || !profiles.some((item) => item.enabled)}
                  >Save schedule</button
                >
              </div>
            </form>
          </section>
        </div>
      {:else if tab === "System"}
        <div class="page-heading">
          <div>
            <p class="eyebrow">Local diagnostics</p>
            <h1>System checks</h1>
            <p>
              Review utility versions, configured root access and free space.
            </p>
          </div>
          <button
            onclick={() => {
              void runDoctor();
            }}
            disabled={checkingDoctor}
            >{checkingDoctor ? "Checking…" : "Run doctor"}</button
          >
        </div>
        <section class="panel">
          <dl class="detail-grid">
            <div>
              <dt>Engine mode</dt>
              <dd>{info ? label(info.mode) : "Unknown"}</dd>
            </div>
            <div>
              <dt>Restore roots</dt>
              <dd>{info?.restore_roots.join(", ") || "None configured"}</dd>
            </div>
            <div>
              <dt>Legacy sources</dt>
              <dd>{info?.legacy_roots.join(", ") || "None configured"}</dd>
            </div>
          </dl>
          <p class="compact muted">
            Physical qualification and local write authorization remain
            operator-controlled.
          </p>
        </section>
        <section class="panel recent" aria-label="Doctor report">
          {#if checkingDoctor}<p class="loading" role="status">
              Checking fixed utility versions and configured filesystems…
            </p>{:else if doctorReport}<div class="panel-heading">
              <h2>
                {doctorReport.ready
                  ? "Simulator prerequisites satisfied"
                  : "Configuration needs attention"}
              </h2>
              <span class="compact muted"
                >{instant(doctorReport.checked_at)}</span
              >
            </div>
            <ul class="row-list">
              {#each doctorReport.checks as check (check.id)}<li>
                  <div class="row-head">
                    <h3 class="identifier">{check.id}</h3>
                    <Badge value={check.status.toUpperCase()} />
                  </div>
                  <p class="compact">{check.message}</p>
                  {#if check.version}<p class="compact muted">
                      Version {check.version}
                    </p>{/if}{#if check.available_bytes !== undefined}<p
                      class="compact muted"
                    >
                      Available {bytes(check.available_bytes)} · configured reserve
                      {bytes(check.required_bytes)}
                    </p>{/if}{#if check.identity}<p class="identifier">
                      Root identity {check.identity}
                    </p>{/if}
                </li>{/each}
            </ul>{:else}<div class="empty">
              <strong>Diagnostics have not run</strong>Run doctor to inspect
              this installation without tape discovery or motion.
            </div>{/if}
        </section>
      {:else if tab === "Alerts"}
        <div class="page-heading">
          <div>
            <p class="eyebrow">Operator inbox</p>
            <h1>Alerts</h1>
            <p>Media requests, verification failures and job outcomes.</p>
          </div>
        </div>
        <section class="panel">
          {#if alerts.length}<ul class="row-list">
              {#each alerts as item (item.id)}<li class:unread={!item.read_at}>
                  <div class="row-head">
                    <div>
                      <h3>{label(item.code)}</h3>
                      <p>{instant(item.created_at)}</p>
                    </div>
                    {#if !item.read_at}<button
                        class="secondary"
                        onclick={() => {
                          void markRead(item);
                        }}
                        disabled={busy}>Mark read</button
                      >{:else}<span class="compact muted">Read</span>{/if}
                  </div>
                  <p class="alert-message">{item.message}</p>
                  {#if item.job_id && jobs.some((job) => job.id === item.job_id)}<button
                      class="text-button"
                      onclick={() => {
                        const job = jobs.find((job) => job.id === item.job_id);
                        if (job) inspectJob(job);
                      }}>Open related job</button
                    >{/if}
                </li>{/each}
            </ul>{:else}<div class="empty">
              <strong>Your inbox is clear</strong>New operational events appear
              here.
            </div>{/if}
        </section>
      {/if}
    </main>
  </div>
{/if}

<style>
  .app-shell {
    display: grid;
    grid-template-columns: 224px minmax(0, 1fr);
    min-height: 100vh;
  }
  .sidebar {
    min-width: 0;
    background: #193b31;
    color: #e5eee5;
    padding: 2rem 1.2rem;
    display: flex;
    flex-direction: column;
    position: sticky;
    top: 0;
    height: 100vh;
  }
  .brand {
    color: #fff;
    text-decoration: none;
    letter-spacing: -0.045em;
    font-size: 1.65rem;
    font-weight: 700;
    padding: 0.1rem 0.7rem 2.3rem;
  }
  .brand span {
    display: block;
    font-size: 0.7rem;
    letter-spacing: 0.045em;
    font-weight: 450;
    color: #a9c0ae;
    margin-top: 0.4rem;
  }
  .sidebar nav {
    display: grid;
    gap: 0.35rem;
  }
  .sidebar nav button {
    display: flex;
    align-items: center;
    justify-content: space-between;
    background: transparent;
    border: 0;
    border-radius: 5px;
    color: #c7d7ca;
    text-align: left;
    font-size: 0.85rem;
    font-weight: 500;
    padding: 0.8rem 0.85rem;
    min-height: 44px;
  }
  .sidebar nav button:hover,
  .sidebar nav button.active {
    background: #2c5141;
    color: white;
  }
  .sidebar nav button.active {
    box-shadow: inset 3px 0 #c1d681;
  }
  .nav-count {
    border-radius: 4px;
    background: #d3dfaa;
    color: #244733;
    font-size: 0.68rem;
    font-weight: 750;
    min-width: 20px;
    text-align: center;
    padding: 0.1rem 0.2rem;
  }
  .sidebar-footer {
    margin-top: auto;
    padding: 1.2rem 0.7rem 0;
    border-top: 1px solid #3b5a48;
  }
  .mode {
    display: flex;
    align-items: center;
    gap: 0.45rem;
    font-size: 0.72rem;
    color: #c7d9c7;
    text-transform: capitalize;
  }
  .mode > span {
    height: 6px;
    width: 6px;
    border-radius: 50%;
    background: #ccdc99;
  }
  .logout {
    background: transparent;
    border: 0;
    padding: 0.7rem 0;
    color: #d3dfd0;
    font-size: 0.75rem;
    font-weight: 450;
  }
  .logout:hover {
    background: transparent;
    color: white;
    text-decoration: underline;
  }
  .workspace {
    min-width: 0;
    padding: 0 clamp(1rem, 3vw, 3rem) 3rem;
    max-width: 1600px;
    width: 100%;
    margin: auto;
  }
  .topbar {
    height: 76px;
    border-bottom: 1px solid #dce2d9;
    display: flex;
    align-items: center;
    justify-content: space-between;
    margin-bottom: 2.5rem;
    font-size: 0.75rem;
    color: #768173;
  }
  .metrics {
    display: grid;
    grid-template-columns: repeat(3, 1fr);
    border: 1px solid #dce2d9;
    border-radius: 8px;
    background: #fff;
    margin-bottom: 1.5rem;
  }
  .metrics > div {
    padding: 1.5rem 1.7rem;
    border-right: 1px solid #e5e9de;
  }
  .metrics > div:last-child {
    border-right: 0;
  }
  .metrics span,
  .metrics small {
    display: block;
    color: #63736b;
    font-size: 0.77rem;
  }
  .metrics strong {
    display: block;
    font-size: 2.3rem;
    letter-spacing: -0.055em;
    font-weight: 550;
    margin: 0.65rem 0 0.45rem;
  }
  .metrics small {
    font-size: 0.71rem;
  }
  .recent {
    margin-top: 1.5rem;
  }
  .drive-title {
    font-size: 1.15rem;
    font-weight: 600;
    margin-top: 1.8rem;
  }
  .search-form {
    display: flex;
    align-items: flex-end;
    gap: 1rem;
    margin-bottom: 1rem;
  }
  .search-form label {
    flex: 1;
  }
  .restore-form {
    margin-top: 1.75rem;
    padding-top: 1.5rem;
    border-top: 1px solid #e5e9de;
  }
  .alert-message {
    font-size: 0.9rem;
    margin: 1rem 0 0.5rem;
    white-space: pre-wrap;
  }
  .unread {
    border-left: 3px solid #aac47c;
    padding-left: 1rem !important;
  }
  .login-page {
    min-height: 100vh;
    background: linear-gradient(
      115deg,
      #193b31 0%,
      #193b31 43%,
      #eef1e7 43%,
      #f5f6f2 100%
    );
    display: flex;
    align-items: center;
    justify-content: center;
    gap: 9vw;
    padding: 2rem;
  }
  .login-brand {
    color: #fff;
    font-size: clamp(2.5rem, 5vw, 4rem);
    letter-spacing: -0.055em;
    font-weight: 700;
  }
  .login-brand span {
    display: block;
    font-size: 0.85rem;
    font-weight: 400;
    letter-spacing: 0;
    color: #d2e0c4;
    margin-top: 1rem;
  }
  .login-card {
    width: 390px;
    max-width: 100%;
    padding: 2.5rem;
    background: white;
    border: 1px solid #d4dece;
    border-radius: 9px;
    box-shadow: 0 10px 60px #153b3110;
  }
  .login-card h1 {
    font-size: 1.8rem;
  }
  .login-card > p {
    font-size: 0.86rem;
  }
  .login-submit {
    width: 100%;
    margin-top: 1.3rem;
  }
  .login-help {
    margin-top: 1.5rem;
    font-size: 0.73rem;
  }
  .sr-only {
    position: absolute;
    width: 1px;
    height: 1px;
    padding: 0;
    margin: -1px;
    overflow: hidden;
    clip: rect(0, 0, 0, 0);
    white-space: nowrap;
    border: 0;
  }
  @media (max-width: 900px) {
    .app-shell {
      grid-template-columns: minmax(0, 1fr);
    }
    .sidebar {
      position: static;
      height: auto;
      padding: 1rem;
    }
    .brand {
      font-size: 1.4rem;
      padding: 0 0.6rem 1rem;
    }
    .brand span {
      display: none;
    }
    .sidebar nav {
      display: flex;
      overflow-x: auto;
      gap: 0.3rem;
    }
    .sidebar nav button {
      white-space: nowrap;
      padding: 0.65rem 0.8rem;
      font-size: 0.8rem;
      flex-shrink: 0;
    }
    .sidebar-footer {
      display: flex;
      align-items: center;
      justify-content: space-between;
      padding: 0.75rem 0.6rem 0;
      margin-top: 0.7rem;
    }
    .logout {
      padding: 0;
      min-height: 32px;
    }
    .topbar {
      height: 58px;
      margin-bottom: 1.5rem;
    }
    .workspace {
      padding-bottom: 2rem;
    }
    .login-page {
      gap: 3rem;
      flex-direction: column;
      background: #193b31;
    }
    .login-brand {
      text-align: center;
    }
    .login-brand span {
      font-size: 0.8rem;
    }
    .login-card {
      padding: 1.8rem;
    }
  }
  @media (max-width: 600px) {
    .metrics {
      grid-template-columns: 1fr;
    }
    .metrics > div {
      border-right: 0;
      border-bottom: 1px solid #e5e9de;
      padding: 1rem 1.25rem;
      display: grid;
      grid-template-columns: 1fr auto;
      align-items: center;
      gap: 0.3rem;
    }
    .metrics > div:last-child {
      border-bottom: 0;
    }
    .metrics strong {
      font-size: 1.8rem;
      margin: 0;
      grid-column: 2;
      grid-row: 1/3;
    }
    .metrics small {
      grid-column: 1;
    }
    .search-form {
      display: block;
    }
    .search-form button {
      margin-top: 0.75rem;
      width: 100%;
    }
    .login-page {
      padding: 1.2rem;
    }
    .sidebar nav button {
      min-height: 42px;
    }
  }
</style>
