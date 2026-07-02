<script lang="ts">
  import { onMount } from 'svelte'
  import { listSettings } from '../../lib/api/settings'
  import { session } from '../../lib/auth.svelte'
  import { Resource } from '../../lib/resource.svelte'
  import { SettingsModel } from './model.svelte'
  import SettingsGroup from './SettingsGroup.svelte'

  const model = new SettingsModel()
  const catalog = new Resource(async () => {
    const res = await listSettings()
    model.load(res.settings)
    return res
  })

  // Settings reads are admin-gated server-side (F4-O4: GET ⇒ 403 for
  // non-admin) — a read-only key gets the banner, not a doomed request.
  onMount(() => {
    if (session.admin) void catalog.load()
  })
</script>

<section class="area">
  <header>
    <h1>Settings</h1>
    <p class="sub">
      runtime configuration — precedence <code>db</code> &gt; <code>env</code> &gt; <code>default</code>;
      overrides apply on the fly
    </p>
  </header>

  {#if !session.admin}
    <p class="banner" role="status">
      read-only key — settings are admin-gated (the server answers 403); sign in with an admin key to
      view and edit.
    </p>
  {:else}
    <a class="editor-link" href="/settings/backends">
      <span class="el-title">Backend pool &amp; vault →</span>
      <span class="el-sub">manage provider backends, trust tiers and API-key secrets</span>
    </a>
    {#if catalog.status === 'loading' || catalog.status === 'idle'}
      <p class="state" aria-busy="true">loading settings catalog…</p>
    {:else if catalog.status === 'error'}
      <div class="error" role="alert">
        <p>{catalog.error?.message}</p>
        {#if catalog.error?.requestId}
          <p class="request-id">request {catalog.error.requestId}</p>
        {/if}
        <button type="button" onclick={() => void catalog.reload()}>Retry</button>
      </div>
    {:else}
      <div class="groups">
        {#each model.groups as group (group.prefix)}
          <SettingsGroup {group} {model} />
        {/each}
      </div>
    {/if}
  {/if}
</section>

<style>
  .area {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }

  header {
    border-bottom: 1px solid var(--border);
    padding-bottom: var(--space-2);
  }
  h1 {
    margin: 0;
    font-size: 1.35rem;
    font-weight: 600;
    letter-spacing: 0.01em;
  }
  .sub {
    margin: var(--space-1) 0 0;
    color: var(--text-dim);
    font-size: 0.85rem;
  }

  .banner {
    margin: 0;
    padding: var(--space-2) var(--space-3);
    border: 1px solid var(--warn);
    border-radius: var(--radius);
    color: var(--warn);
    font-size: 0.875rem;
  }

  .state {
    margin: 0;
    color: var(--text-faint);
    font-family: var(--font-mono);
    font-size: 0.85rem;
  }

  .error {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    align-items: flex-start;
    border: 1px solid var(--danger);
    border-radius: var(--radius);
    background: var(--danger-dim);
    padding: var(--space-2) var(--space-3);
    font-size: 0.85rem;
  }
  .error p {
    margin: 0;
    color: var(--danger);
  }
  .request-id {
    font-family: var(--font-mono);
    font-size: 0.75rem;
    color: var(--text-dim) !important;
  }

  .groups {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }

  .editor-link {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    padding: var(--space-2) var(--space-3);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    background: var(--surface-1);
    text-decoration: none;
    transition: border-color var(--dur-1) var(--ease);
  }
  .editor-link:hover {
    border-color: var(--accent);
  }
  .el-title {
    font-family: var(--font-mono);
    font-size: 0.9rem;
    color: var(--accent);
  }
  .el-sub {
    font-size: 0.78rem;
    color: var(--text-dim);
  }
</style>
