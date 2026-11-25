<script lang="ts">
  import { untrack } from "svelte";
  import { API, bytes, instant, failureMessage, label, terminal } from "../api";
  import type { Job, Progress, JobEvent } from "../api";
  import Badge from "./Badge.svelte";
  let {
    job,
    api,
    onrefresh,
    onerror,
  }: {
    job: Job;
    api: API;
    onrefresh: () => Promise<void>;
    onerror: (error: unknown) => void;
  } = $props();
  let current = $state<Job>(untrack(() => job));
  let progress = $state<Progress | null>(null);
  type Publication = {
    restore_root_id: string;
    destination_slug: string;
    completed_at: string;
    selected_members: number | null;
    expanded_members: number | null;
    integrity_basis: string;
  };
  let publication = $state<Publication | null>(null);
  let events = $state<JobEvent[]>([]);
  let live = $state("Connecting to live updates");
  let busy = $state(false);
  let lastState = "";
  $effect(() => {
    const id = job.id;
    current = job;
    events = [];
    progress = null;
    publication = null;
    lastState = job.state;
    const source = new EventSource(
      "/api/v1/jobs/" + encodeURIComponent(id) + "/stream",
    );
    let active = true;
    function update(next: Job) {
      if (!active) return;
      current = next;
      if (next.state !== lastState) {
        lastState = next.state;
        void onrefresh().catch(onerror);
      }
    }
    source.addEventListener("snapshot", (event) => {
      try {
        const value = JSON.parse((event as MessageEvent).data) as {
          job: Job;
          progress: Progress | null;
          publication?: Publication;
        };
        if (value.job?.id === id) {
          update(value.job);
          progress = value.progress;
          publication = value.publication || null;
          if (terminal(value.job.state)) {
            source.close();
            live = "Final job state received";
          }
        }
      } catch {
        live = "Live update could not be read";
      }
    });
    source.addEventListener("log", (event) => {
      try {
        const value = JSON.parse((event as MessageEvent).data) as JobEvent;
        if (
          value.job_id === id &&
          !events.some((item) => item.id === value.id)
        ) {
          events = [...events, value].slice(-200);
        }
      } catch {
        live = "Log update could not be read";
      }
    });
    source.onopen = () => {
      live = "Live updates connected";
    };
    source.onerror = () => {
      live = "Reconnecting live updates · polling every 5 seconds";
    };
    const poll = setInterval(() => {
      if (!terminal(current.state)) {
        void api
          .get<Job>("/jobs/" + encodeURIComponent(id))
          .then(update)
          .catch(onerror);
      }
    }, 5000);
    return () => {
      active = false;
      clearInterval(poll);
      source.close();
    };
  });
  async function action(kind: "cancel" | "recheck-media") {
    busy = true;
    try {
      await api.mutate(
        "/jobs/" + encodeURIComponent(current.id) + "/" + kind,
        {},
      );
      current = await api.get<Job>("/jobs/" + encodeURIComponent(current.id));
      await onrefresh();
    } catch (error) {
      onerror(error);
    } finally {
      busy = false;
    }
  }
</script>

<section class="panel job-detail" aria-label="Selected job">
  <div class="panel-heading">
    <div>
      <p class="eyebrow">Selected job</p>
      <h2>{label(current.kind)}</h2>
    </div>
    <div role="status" aria-label="Job state">
      <Badge value={current.state} />
    </div>
  </div>
  <p class="identifier">{current.id}</p>
  <dl class="detail-grid">
    <div>
      <dt>Started</dt>
      <dd>{instant(current.created_at)}</dd>
    </div>
    <div>
      <dt>Phase</dt>
      <dd>{label(progress?.phase || current.phase || "queued")}</dd>
    </div>
  </dl>
  {#if publication?.completed_at}<p class="restore-result">
      Restored to <strong
        >{publication.restore_root_id}/{publication.destination_slug}</strong
      >
    </p>
    <p>
      {#if publication.selected_members !== null}{publication.selected_members} members
        restored, including {publication.expanded_members} added by selection expansion.
      {/if}Integrity basis: {label(publication.integrity_basis)}. Ownership,
      ACLs, extended attributes and privileged bits were omitted.
    </p>
  {/if}
  {#if current.error_code}<p class="inline-error">
      {failureMessage(current.error_code)}
      <span class="identifier">{current.error_code}</span>
    </p>{/if}
  {#if progress}
    <div class="transfer">
      <div>
        <span>Accepted write bytes</span><strong
          >{bytes(progress.accepted_bytes)}</strong
        >
      </div>
      <div>
        <span>Readback bytes</span><strong
          >{bytes(progress.readback_bytes)}</strong
        >
      </div>
      <div>
        <span>Elapsed</span><strong
          >{Math.floor(progress.elapsed_seconds)} s</strong
        >
      </div>
      {#if !terminal(current.state) && progress.eta_seconds !== null}
        <div>
          <span>Estimated time remaining</span><strong
            >{Math.ceil(progress.eta_seconds)} s</strong
          >
        </div>
      {/if}
      <div>
        <span>Speed in this phase</span><strong
          >{progress.speed_mib_per_second.toFixed(1)} MiB/s</strong
        >
      </div>
    </div>
    {#if progress.estimated_percent !== null && Number.isFinite(progress.estimated_percent)}<label
        class="progress-label"
        for="job-progress"
        >Estimated {label(progress.phase)} progress · {Math.round(
          progress.estimated_percent,
        )}%</label
      ><progress id="job-progress" max="100" value={progress.estimated_percent}
      ></progress>{/if}
  {/if}
  <div class="actions">
    {#if !terminal(current.state) && current.state !== "CANCEL_REQUESTED"}<button
        class="secondary"
        onclick={() => action("cancel")}
        disabled={busy}>Request cancellation</button
      >{/if}{#if current.state === "WAITING_MEDIA"}<button
        onclick={() => action("recheck-media")}
        disabled={busy}>Recheck selected tape</button
      >{/if}
  </div>
  <h3>Recorded events</h3>
  <p class="muted compact" role="status">{live}</p>
  {#if events.length}<ol class="event-list">
      {#each events as event (event.id)}<li>
          <time datetime={event.timestamp}>{instant(event.timestamp)}</time
          ><span><strong>{label(event.code)}</strong> {event.message}</span>
        </li>{/each}
    </ol>{:else}<p class="muted">No recorded events received yet.</p>{/if}
</section>
