<script lang="ts">
  import { onMount } from "svelte";
  import { connected } from "../lib/status";
  let status = $state("Checking connection");
  async function check() {
    status = "Checking connection";
    status = (await connected(fetch)) ? "Connected" : "Unavailable";
  }
  onMount(() => {
    void check();
  });
</script>

<svelte:head><title>TapePilot · Simulator</title></svelte:head>
<header>TapePilot</header>
<main>
  <h1>Simulator console</h1>
  <p>
    This development build checks backend connectivity. Archive jobs and
    physical drives are unavailable.
  </p>
  <section>
    <h2>Backend connection</h2>
    <p role="status">{status}</p>
    <button onclick={check} disabled={status === "Checking connection"}
      >Check again</button
    >
  </section>
</main>

<style>
  :global(body) {
    margin: 0;
    background: #f5f7f4;
    color: #143b35;
    font:
      1.1rem/1.55 system-ui,
      sans-serif;
  }
  header {
    padding: 1rem 6vw;
    background: #143b35;
    color: white;
    font-weight: 700;
  }
  main {
    margin: 8vh auto;
    padding: 0 6vw;
    max-width: 48rem;
  }
  h1 {
    font-size: clamp(2.4rem, 6vw, 4rem);
    letter-spacing: -0.05em;
    line-height: 1.05;
  }
  section {
    margin-top: 2rem;
    padding: 1.4rem;
    background: white;
    border: 1px solid #c4d3cb;
    border-radius: 0.6rem;
  }
  h2 {
    margin: 0;
    font-size: 1.2rem;
  }
  button {
    font: inherit;
    color: white;
    background: #21594e;
    border: 0;
    border-radius: 0.3rem;
    padding: 0.5rem 1rem;
    cursor: pointer;
  }
</style>
