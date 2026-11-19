<script lang="ts">
  // Foreign-text marks of a block or hit (#295, web part): `untrusted` and
  // `untrusted_origin` as small badges, the full statement in the title. Text
  // nodes only. Renders NOTHING (no wrapper either) for a block without either
  // field, so a host's flex gap or line box stays as it was.
  import { trustMarks, type TrustFields } from './trust'

  let { block }: { block: TrustFields } = $props()

  const marks = $derived(trustMarks(block))
</script>

{#if marks.length > 0}
  <span class="marks">
    {#each marks as m (m.key)}
      <span class="trust" data-trust={m.key} title={m.title}>{m.label}</span>
    {/each}
  </span>
{/if}

<style>
  /* The wrapper spaces two marks (each-block edges carry no whitespace). */
  .marks {
    display: inline-flex;
    flex-wrap: wrap;
    gap: var(--space-1);
  }
  .trust {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    /* a type name is shown as written, also inside an uppercase host line */
    text-transform: none;
    border: 1px solid var(--warn);
    border-radius: var(--radius);
    padding: 0 var(--space-1);
    color: var(--warn);
    white-space: nowrap;
  }
</style>
