<script lang="ts">
  // "Model API" status tile (RC-D C6, wave W05-17; design/05 §4.6, placed
  // after DispatchTile). Rides the status answer — no request of its own.
  // server-admin (`gateway`): counters, 5-min refusals, instance envelope,
  // the internal/external partition per target (dispatch.targets, A03-W3),
  // top consumers, expiring keys. tenant-admin (`gateway_tenant`): own
  // counters + the coarse depth class per target, never a number (V-03-b).
  // Neither section ⇒ the tile is not drawn (status/gateway.ts
  // gatewayTileMode).
  //
  // RC-D S1: contract stub — renders nothing (mode 'off').
  import type { StatusResponse } from '../../lib/api/types'

  let { status }: { status: StatusResponse } = $props()
  // S1: the props are read nowhere yet; the effect keeps them referenced.
  $effect(() => void [status])
</script>
