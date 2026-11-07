<script lang="ts">
  // Consumer detail page /gateway/consumers/:id (RC-D C6, wave W05-09;
  // design/05 §4.6): head (label, kind badge, server status), policy, the key
  // chain (chain.ts keyChain — rotated_from links), UsagePanel (W05-10),
  // LimitsCard (W05-12), the last calls (calls.ts callRows — body-free, a
  // detail link for server-admin ONLY) and the audit trail as a chain
  // (chain.ts auditEntries). State: ConsumerDetailModel (consumer.svelte.ts).
  // A foreign or unknown id renders the No-Oracle "consumer not found".
  //
  // RC-D S1: contract stub — the page renders its heading and a not-wired
  // note; S2 builds the sections above.
  import { route } from '../../router'

  const consumerId = $derived(route.params.id ?? '')
</script>

<section class="area" data-consumer-id={consumerId}>
  <header>
    <h1>Model consumer</h1>
  </header>
  <p class="muted">Not wired yet (RC-D S1 contract stub).</p>
</section>

<style>
  .area {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }
  header {
    border-bottom: 1px solid var(--border);
    padding-bottom: var(--space-2);
  }
  h1 {
    margin: 0;
    font-size: var(--fs-xl);
    font-weight: var(--fw-semibold);
    letter-spacing: var(--track-1);
  }
  .muted {
    margin: 0;
    color: var(--text-dim);
    font-size: var(--fs-sm);
  }
</style>
