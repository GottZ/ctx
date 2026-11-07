<script lang="ts">
  // Usage panel of one consumer (RC-D C6, wave W05-10; design/05 §4.6):
  // window switch 24h · 7d · 30d · 90d, SeriesBars chart and — rendered from
  // the SAME rows (usage-format.ts usageTableRows) — a table alternative for
  // screen readers. null / complete:false render "not recorded" (never 0);
  // hist:false shows the marked means (Ø); auth refusals are never a number
  // per consumer. State: UsageModel (usage.svelte.ts), 60 s visible-only poll.
  //
  // RC-D S1: contract stub — renders nothing.
  let { consumerId }: { consumerId: string } = $props()
  // S1: the props are read nowhere yet; the effect keeps them referenced.
  $effect(() => void [consumerId])
</script>
