<script lang="ts">
  import type { BlockDetailModel } from './detail.svelte'

  // Read-only detail panel (block-workbench W3). The model owns the lazy
  // getBlock load + status; this component is a thin sidebar mirroring the
  // graph DetailSidebar: a meta-<dl>, the full content as a TEXT NODE (never
  // {@html} — repo XSS convention), an "open in graph" deep-link and a close
  // button. Editing/delete/sensitivity arrive in later waves (W4/W5/W6).
  let { model }: { model: BlockDetailModel } = $props()
</script>

<aside class="sidebar" aria-label="block details">
  <header>
    <h2>{model.block?.title ?? 'Block'}</h2>
    <button class="close" type="button" title="close" onclick={() => model.close()}>×</button>
  </header>

  {#if model.status === 'loading'}
    <p class="state" aria-busy="true">loading…</p>
  {:else if model.status === 'error'}
    <p class="problem" role="alert">{model.loadError?.message ?? 'failed to load block'}</p>
    {#if model.loadError?.requestId}
      <p class="request-id">request {model.loadError.requestId}</p>
    {/if}
  {:else if model.block}
    {@const b = model.block}
    <dl class="meta">
      <dt>id</dt>
      <dd><code>{b.id}</code></dd>
      <dt>category</dt>
      <dd>{b.category}</dd>
      <dt>scope</dt>
      <dd>{b.scope}</dd>
      {#if b.tags.length > 0}
        <dt>tags</dt>
        <dd>{b.tags.join(', ')}</dd>
      {/if}
      <dt>created</dt>
      <dd>{b.created_at.slice(0, 10)}</dd>
      <dt>updated</dt>
      <dd>{b.updated_at.slice(0, 10)}</dd>
    </dl>

    {#if model.openId}
      <div class="actions">
        <a href={`/graph?focus=${model.openId}`}>Open in graph</a>
      </div>
    {/if}

    <!-- Always a text node, never {@html} — repo XSS convention (DetailSidebar). -->
    <pre class="content">{b.content}</pre>
  {/if}
</aside>

<style>
  .sidebar {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    width: 22rem;
    max-width: 40vw;
    border: 1px solid var(--border);
    border-radius: var(--radius);
    background: var(--surface-1);
    padding: var(--space-3);
    overflow-y: auto;
  }

  header {
    display: flex;
    align-items: flex-start;
    gap: var(--space-2);
  }
  h2 {
    margin: 0;
    font-size: 0.95rem;
    font-weight: 600;
    line-height: 1.35;
    flex: 1;
  }
  .close {
    padding: 0 var(--space-2);
    font-size: 1rem;
    line-height: 1.4;
  }

  .meta {
    display: grid;
    grid-template-columns: auto 1fr;
    gap: 0.15rem var(--space-3);
    margin: 0;
    font-size: 0.8rem;
  }
  dt {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    color: var(--text-faint);
    align-self: baseline;
  }
  dd {
    margin: 0;
    color: var(--text-dim);
    overflow-wrap: anywhere;
  }
  dd code {
    font-size: 0.7rem;
  }

  .actions {
    display: flex;
    gap: var(--space-2);
  }
  .actions a {
    font-size: 0.8rem;
    color: var(--accent);
  }

  .state {
    margin: 0;
    color: var(--text-faint);
    font-family: var(--font-mono);
    font-size: 0.8rem;
  }
  .problem {
    margin: 0;
    color: var(--danger);
    font-size: 0.8rem;
  }
  .request-id {
    margin: 0;
    font-family: var(--font-mono);
    font-size: 0.75rem;
    color: var(--text-dim);
  }

  .content {
    margin: 0;
    padding: var(--space-2);
    background: var(--surface-0);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    font-size: 0.78rem;
    line-height: 1.5;
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }
</style>
