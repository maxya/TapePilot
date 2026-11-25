<script lang="ts">
  import { label } from "../api";
  let { value }: { value: string } = $props();
  const positive = $derived(
    ["SUCCEEDED", "VERIFIED", "MANAGED", "OK"].includes(value),
  );
  const attention = $derived(
    ["FAILED", "RECOVERY_REQUIRED", "QUARANTINED", "UNKNOWN", "ERROR"].includes(
      value,
    ),
  );
</script>

<span class:positive class:attention class="badge"
  >{label(value || "UNKNOWN")}</span
>

<style>
  .badge {
    display: inline-block;
    padding: 0.2rem 0.55rem;
    border: 1px solid #dce2dc;
    border-radius: 4px;
    color: #45564e;
    background: #f3f5f2;
    font-size: 0.74rem;
    font-weight: 650;
    line-height: 1.4;
    text-transform: capitalize;
    white-space: nowrap;
  }
  .positive {
    color: #226044;
    background: #eef7ee;
    border-color: #c9e1cd;
  }
  .attention {
    color: #8a332c;
    background: #fff2ee;
    border-color: #edcfc5;
  }
</style>
