<script lang="ts">
  import { onMount } from 'svelte'
  import { listSettings } from '../../lib/api/settings'
  import { groupDomId } from '../../lib/settings'
  import { session } from '../../lib/auth.svelte'
  import { Resource } from '../../lib/resource.svelte'
  import { SettingsModel } from './model.svelte'
  import { SettingsUi } from './ui.svelte'
  import SettingsGroup from './SettingsGroup.svelte'

  const model = new SettingsModel()
  const ui = new SettingsUi()
  const catalog = new Resource(async () => {
    const res = await listSettings()
    model.load(res.settings)
    return res
  })

  let searchInput = $state<HTMLInputElement | null>(null)

  // Settings reads are admin-gated server-side (F4-O4: GET ⇒ 403 for
  // non-admin) — a read-only key gets the banner, not a doomed request.
  onMount(() => {
    if (session.admin) void catalog.load()
  })

  const prefixes = $derived(model.groups.map((g) => g.prefix))
  // Under a live query only groups with at least one hit render; the chip
  // row mirrors that so the nav never points at a hidden card.
  const visibleGroups = $derived(
    ui.searching ? model.groups.filter((g) => ui.visibleSettings(g.settings).length > 0) : model.groups,
  )
  const matchTotal = $derived(
    ui.searching ? visibleGroups.reduce((n, g) => n + ui.visibleSettings(g.settings).length, 0) : 0,
  )

  function jumpTo(prefix: string): void {
    ui.expand(prefix)
    document.getElementById(groupDomId(prefix))?.scrollIntoView({ behavior: 'smooth', block: 'start' })
  }

  function onPageKeydown(e: KeyboardEvent): void {
    // "/" focuses the search unless the user is already typing somewhere.
    if (e.key !== '/' || e.defaultPrevented) return
    const t = e.target as HTMLElement | null
    if (t !== null && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.tagName === 'SELECT' || t.isContentEditable))
      return
    e.preventDefault()
    searchInput?.focus()
  }
</script>

<svelte:window onkeydown={onPageKeydown} />

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
    <a class="editor-link" href="/settings/hues">
      <span class="el-title">Kategorie-Farben →</span>
      <span class="el-sub">Graph-Hue pro Kategorie überschreiben (AM-2) — Rad zeigt die Abstände perceptuell</span>
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
      <div class="toolbar">
        <div class="search" class:active={ui.searching}>
          <input
            type="search"
            placeholder="fuzzy search keys, descriptions, values…  ( / )"
            spellcheck="false"
            autocomplete="off"
            aria-label="search settings"
            bind:this={searchInput}
            bind:value={ui.query}
          />
          {#if ui.searching}
            <span class="match-count" role="status">{matchTotal} matches</span>
            <button class="ghost" type="button" title="clear search" onclick={() => (ui.query = '')}>clear</button>
          {/if}
        </div>
        {#if !ui.searching}
          <div class="bulk">
            <button class="ghost" type="button" onclick={() => ui.setAll(prefixes, false)}>expand all</button>
            <button class="ghost" type="button" onclick={() => ui.setAll(prefixes, true)}>collapse all</button>
          </div>
        {/if}
      </div>

      {#if !ui.searching}
        <nav class="chips" aria-label="setting categories">
          {#each model.groups as group (group.prefix)}
            {@const dirty = model.dirtyKeys(group.prefix).length}
            <button class="chip" type="button" class:dirty={dirty > 0} onclick={() => jumpTo(group.prefix)}>
              {group.prefix}
              <span class="chip-count">{dirty > 0 ? `${dirty}✱` : group.settings.length}</span>
            </button>
          {/each}
        </nav>
      {/if}

      <div class="groups">
        {#each visibleGroups as group (group.prefix)}
          <SettingsGroup {group} {model} {ui} />
        {/each}
        {#if ui.searching && visibleGroups.length === 0}
          <p class="state" role="status">no setting matches "{ui.query}"</p>
        {/if}
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
    font-size: var(--fs-xl);
    font-weight: var(--fw-semibold);
    letter-spacing: var(--track-1);
  }
  .sub {
    margin: var(--space-1) 0 0;
    color: var(--text-dim);
    font-size: var(--fs-sm);
  }

  .banner {
    margin: 0;
    padding: var(--space-2) var(--space-3);
    border: 1px solid var(--warn);
    border-radius: var(--radius);
    color: var(--warn);
    font-size: var(--fs-sm);
  }

  .state {
    margin: 0;
    color: var(--text-faint);
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
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
    font-size: var(--fs-sm);
  }
  .error p {
    margin: 0;
    color: var(--danger);
  }
  .request-id {
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
    color: var(--text-dim) !important;
  }

  .toolbar {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    flex-wrap: wrap;
  }
  .search {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    flex: 1;
    min-width: 16rem;
  }
  .search input {
    flex: 1;
    min-width: 0;
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
    padding: var(--space-1) var(--space-2);
  }
  .search.active input {
    border-color: var(--accent);
  }
  .match-count {
    font-family: var(--font-mono);
    font-size: var(--fs-2xs);
    color: var(--accent);
    white-space: nowrap;
  }
  .bulk {
    display: flex;
    gap: var(--space-1);
  }
  .ghost {
    font-size: var(--fs-xs);
    padding: 0 var(--space-2);
    background: transparent;
  }

  .chips {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-1);
  }
  .chip {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
    padding: 0 var(--space-2);
    background: transparent;
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    color: var(--text-dim);
    cursor: pointer;
    transition: border-color var(--dur-1) var(--ease);
  }
  .chip:hover {
    border-color: var(--accent);
    color: var(--text);
  }
  .chip.dirty {
    border-color: var(--accent);
    color: var(--accent);
  }
  .chip-count {
    font-size: var(--fs-2xs);
    color: var(--text-faint);
  }
  .chip.dirty .chip-count {
    color: var(--accent);
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
    font-size: var(--fs-md);
    color: var(--accent);
  }
  .el-sub {
    font-size: var(--fs-xs);
    color: var(--text-dim);
  }
</style>
