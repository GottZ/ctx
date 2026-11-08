<script lang="ts">
  // Lane field of a model consumer (design/05 §4.5 row "Lane"; wave W05-11 —
  // RC-D C5): a radio group (role group, accessible name "admission lane",
  // like the "prompt sensitivity" group of the create dialog) with one radio
  // per lane.laneOptions(serverAdmin, current, peer) — exactly the closed
  // ExternalLane vocabulary (urgent, bulk), never the dispatcher's queue names;
  // a disabled radio carries its note ("server-admin only" / "managed by the
  // server-admin"). The server stays the authority (bulk → urgent is a 403
  // below server-admin).
  import type { ExternalLane } from '../../lib/api/types'
  import { laneOptions } from './lane'

  interface Props {
    value: ExternalLane
    /** the consumer's admission_lane as the server answered it */
    current: string
    serverAdmin: boolean
    peer: boolean
    disabled?: boolean
    /** FOLGE-Z Z4: a visible reason under the options (the create dialog: why a tenant-admin starts on bulk). */
    note?: string | null
  }
  let { value = $bindable(), current, serverAdmin, peer, disabled = false, note = null }: Props = $props()

  const options = $derived(laneOptions(serverAdmin, current, peer))
</script>

<fieldset class="opts">
  <legend>admission lane</legend>
  {#each options as o (o.value)}
    <label class="check" class:off={o.disabled}>
      <input type="radio" name="admission-lane" value={o.value} disabled={disabled || o.disabled} bind:group={value} />
      {o.value}
      {#if o.note}<span class="note">{o.note}</span>{/if}
    </label>
  {/each}
  <p class="hint">urgent is admitted ahead of ctx's own background work; bulk queues behind it.</p>
  {#if note}<p class="hint" data-testid="lane-hint">{note}</p>{/if}
</fieldset>

<style>
  fieldset {
    display: flex;
    flex-direction: row;
    flex-wrap: wrap;
    gap: var(--space-1) var(--space-3);
    margin: 0;
    padding: var(--space-2);
    border: 1px solid var(--border);
    border-radius: var(--radius);
  }
  legend {
    padding: 0 var(--space-1);
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    color: var(--text-dim);
  }
  .check {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
    min-height: 24px;
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
    color: var(--text);
    cursor: pointer;
  }
  .check.off {
    color: var(--text-dim);
    cursor: not-allowed;
  }
  .note {
    font-family: var(--font-ui);
    font-size: var(--fs-xs);
    color: var(--text-dim);
  }
  .hint {
    flex-basis: 100%;
    margin: 0;
    font-size: var(--fs-xs);
    color: var(--text-dim);
  }
</style>
