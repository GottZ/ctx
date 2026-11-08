<script lang="ts">
  // SVG bar chart of one usage series (RC-D C6, wave W05-10; design/05 §4.6):
  // geometry as attributes from usage-format.ts usageBars (a not-recorded
  // bucket is a GAP marker, never a zero-height bar), colours only via the
  // closed class set below (web/lint/inline-gate.mjs), aria-hidden — the
  // table next to it is the accessible form of the same rows.
  import type { UsageSeries } from '../../lib/api/types'
  import { usageBars, type UsageField } from './usage-format'

  let { series, field }: { series: UsageSeries; field: UsageField } = $props()

  /** viewBox geometry: one 10-unit slot per bucket, 100 units high. */
  const SLOT = 10
  const HEIGHT = 100
  const bars = $derived(usageBars(series, field))
</script>

<svg
  class="bars"
  viewBox="0 0 {Math.max(bars.length, 1) * SLOT} {HEIGHT}"
  preserveAspectRatio="none"
  aria-hidden="true"
  data-testid="usage-bars"
>
  {#each bars as b, i (b.bucketStart)}
    {#if b.height === null}
      <rect class="gap" x={i * SLOT + 1} y={HEIGHT - 4} width={SLOT - 2} height="4" data-bar-gap></rect>
    {:else}
      <rect class="bar" x={i * SLOT + 1} y={HEIGHT - b.height * HEIGHT} width={SLOT - 2} height={b.height * HEIGHT} data-bar
      ></rect>
    {/if}
  {/each}
</svg>

<style>
  .bars {
    display: block;
    width: 100%;
    height: 5rem;
  }
  .bar {
    fill: var(--accent);
  }
  .gap {
    fill: var(--text-faint);
    opacity: 0.4;
  }
</style>
