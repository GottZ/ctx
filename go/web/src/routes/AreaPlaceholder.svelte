<script lang="ts">
  import type { Snippet } from 'svelte'

  // Shared skeleton for the four area placeholders (design 04-W3: structure
  // and navigation are real, target UIs ship in later waves).
  let {
    title,
    description,
    ships,
    children,
  }: {
    title: string
    description: string
    /** Which wave/feature delivers the real UI. */
    ships: string
    children?: Snippet
  } = $props()
</script>

<section class="area">
  <header>
    <h1>{title}</h1>
    <span class="pending">pending</span>
  </header>
  {#if children}
    {@render children()}
  {/if}
  <p class="description">{description}</p>
  <p class="ships">{ships}</p>
</section>

<style>
  .area {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    max-width: 44rem;
  }

  header {
    display: flex;
    align-items: baseline;
    gap: var(--space-3);
    border-bottom: 1px solid var(--border);
    padding-bottom: var(--space-2);
  }
  h1 {
    margin: 0;
    font-size: 1.35rem;
    font-weight: 600;
    letter-spacing: 0.01em;
  }
  .pending {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    color: var(--text-faint);
    border: 1px dashed var(--border-strong);
    border-radius: var(--radius);
    padding: 0.05rem var(--space-2);
  }

  .description {
    margin: 0;
    color: var(--text);
  }
  .ships {
    margin: 0;
    color: var(--text-dim);
    font-size: 0.875rem;
  }
</style>
