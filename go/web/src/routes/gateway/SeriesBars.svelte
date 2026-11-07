<script lang="ts">
  // SVG bar chart of one usage series (RC-D C6, wave W05-10; design/05 §4.6):
  // geometry as attributes from usage-format.ts usageBars (a not-recorded
  // bucket is a GAP, never a zero-height bar), colours only via a closed
  // class set (web/lint/inline-gate.mjs), aria-hidden — the table next to it
  // is the accessible form.
  //
  // RC-D S1: contract stub — renders nothing.
  import type { UsageSeries } from '../../lib/api/types'
  import type { UsageField } from './usage-format'

  let { series, field }: { series: UsageSeries; field: UsageField } = $props()
  // S1: the props are read nowhere yet; the effect keeps them referenced.
  $effect(() => void [series, field])
</script>
