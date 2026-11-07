<script lang="ts">
  // The live keys of one consumer with their actions (design/05 §4.6 key row,
  // §5 B-20; wave W05-08 — RC-D C5; reused by the consumer detail of C6
  // W05-09). Contract (S2): one entry per key — hint, server status badge
  // (data-key-status), expiry; "Revoke" per key (typed 4-hint gate via the
  // page's ConfirmDialog); "Rotate key" (data-testid="key-rotate") only where
  // rotate.canRotate allows; in status `rotating` ONLY the predecessor
  // (rotate.endGraceTarget, status superseded) carries "End grace period now"
  // (data-testid="key-end-grace", aria-label "End grace period of key <hint>"),
  // whose click hands THAT key to onendgrace — the page revokes its key_id.
  //
  // RC-D S1: contract stub — renders the hints only, no action.
  import type { ConsumerView, KeyView } from '../../lib/api/types'

  export interface KeyChainProps {
    consumer: ConsumerView
    busy: boolean
    /** rotate.canRotate(consumer, caps) */
    rotatable: boolean
    onrevoke: (k: KeyView) => void
    /** the predecessor whose grace window ends now (model-key-revoke with ITS key_id) */
    onendgrace: (k: KeyView) => void
    onrotate: () => void
  }
  // S1: only the hints render; S2 wires the actions.
  let { consumer }: KeyChainProps = $props()
</script>

<ul data-testid="key-chain">
  {#each consumer.keys as k (k.id)}
    <li><code>{k.hint}</code></li>
  {/each}
</ul>
