<script lang="ts">
  // Lane field of a model consumer (design/05 §4.5 row "Lane"; wave W05-11 —
  // RC-D C5). Contract (S2): a radio group (role group, accessible name
  // "admission lane", like the "prompt sensitivity" group of the create dialog)
  // with one radio per lane.laneOptions(serverAdmin, current, peer) — exactly
  // the closed ExternalLane vocabulary (urgent, bulk); a disabled radio
  // carries its note ("server-admin only" / "managed by the server-admin");
  // bindable `value`.
  //
  // RC-D S1: contract stub — renders a placeholder, offers no option.
  import type { ExternalLane } from '../../lib/api/types'

  interface Props {
    value: ExternalLane
    /** the consumer's admission_lane as the server answered it */
    current: string
    serverAdmin: boolean
    peer: boolean
    disabled?: boolean
  }
  // S1: only the bound value renders; S2 builds the options.
  let { value = $bindable() }: Props = $props()
</script>

<span class="lane" data-testid="rcd-stub">lane: {value} (lane select not wired yet, RC-D W05-11)</span>
