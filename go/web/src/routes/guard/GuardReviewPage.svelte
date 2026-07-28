<script lang="ts">
  // Guard review queue (needs_review pipeline W4): the first SPA surface for
  // the write guard's flagged blocks. List (similarity-sorted, server-side),
  // pair view (block vs. matched partner, loaded on expand), single keep/
  // archive per row and a batch bar over the checkbox selection (the W1 ids[]
  // contract). Authorization stays server-side (writableBlockScopes) — this
  // page is UX only; the nav gates visibility on viewOpsSurfaces.
  //
  // LIVE (RC-1 wave S6, design/05 §4.5): the page used to load exactly twice —
  // the statusFilter effect and the post-resolve reload — so a decision made by
  // anyone else stayed invisible until someone pressed Reload. GuardLive polls
  // GET /api/status and reloads when the READ-SCOPE vector moves (not the
  // home-scope counter: list and counter run on two different scope predicates).
  // Stufe 2 (S13) swaps the transport for SSE and keeps the poll as fallback.
  import { toApiError } from '../../lib/api'
  import { guardList, guardResolve, type GuardListItem, type GuardSkip } from '../../lib/api/guard'
  import { getBlock, type BlockDetail } from '../../lib/api/blocks'
  import ConfirmDialog from '../../lib/components/ConfirmDialog.svelte'
  import ConnState from '../../lib/ui/ConnState.svelte'
  import Table from '../../lib/ui/Table.svelte'
  import { GuardLive } from './guard-live.svelte'

  let items = $state<GuardListItem[]>([])
  let loading = $state(false)
  let error = $state('')
  let statusFilter = $state('')
  let typeFilter = $state('')
  let picked = $state<Set<string>>(new Set())
  let expanded = $state<string | null>(null)
  let pair = $state<{ left: BlockDetail | null; right: BlockDetail | null }>({ left: null, right: null })
  let pairLoading = $state(false)
  let busy = $state(false)
  let lastSkipped = $state<GuardSkip[]>([])
  let confirmArchive = $state<string[] | null>(null)

  const types = $derived([...new Set(items.map((i) => i.type_name))].sort())
  const visible = $derived(typeFilter ? items.filter((i) => i.type_name === typeFilter) : items)
  const allPicked = $derived(visible.length > 0 && visible.every((i) => picked.has(i.id)))

  async function reload() {
    loading = true
    error = ''
    try {
      const resp = await guardList({ status: statusFilter || undefined, limit: 200 })
      items = resp.blocks
      picked = new Set()
      expanded = null
    } catch (e) {
      error = toApiError(e).message
    } finally {
      loading = false
    }
  }

  $effect(() => {
    void statusFilter
    void reload()
  })

  // The live channel. Reads nothing reactive here, so it is armed ONCE for the
  // page's life and torn down on unmount — the statusFilter effect above must
  // not restart the poll.
  const live = new GuardLive({ onChanged: () => void reload() })
  $effect(() => {
    live.start()
    return () => live.stop()
  })

  // Queue counters from the server's guard generation. Three states, never
  // conflated (B10): fresh renders digits (0 is a real answer — "queue clear"),
  // missing/stale render '—' plus how old the data is, so a degraded read can
  // never be misread as an empty queue.
  const queueAge = $derived(
    live.section.ageMs === null ? 'no data' : `${Math.round(live.section.ageMs / 1000)}s old`,
  )

  function togglePick(id: string) {
    const next = new Set(picked)
    if (next.has(id)) next.delete(id)
    else next.add(id)
    picked = next
  }

  function togglePickAll() {
    picked = allPicked ? new Set() : new Set(visible.map((i) => i.id))
  }

  async function toggleExpand(item: GuardListItem) {
    if (expanded === item.id) {
      expanded = null
      return
    }
    expanded = item.id
    pair = { left: null, right: null }
    pairLoading = true
    try {
      const left = await getBlock(item.id)
      const right = item.matched_id ? await getBlock(item.matched_id) : null
      // Only publish when this row is still the expanded one (stale-response guard).
      if (expanded === item.id) pair = { left: left.block, right: right?.block ?? null }
    } catch (e) {
      if (expanded === item.id) error = toApiError(e).message
    } finally {
      pairLoading = false
    }
  }

  async function resolve(ids: string[], resolution: 'archive' | 'keep') {
    busy = true
    error = ''
    // Order guard (N14): a poll answer that predates this mutation must not
    // become the compare baseline, and the reload below is the one this
    // resolution owes the operator — the next poll adopts its result silently.
    live.markMutation()
    try {
      const resp = await guardResolve(ids, resolution)
      lastSkipped = resp.skipped
      await reload()
    } catch (e) {
      error = toApiError(e).message
    } finally {
      busy = false
    }
  }

  function requestArchive(ids: string[]) {
    confirmArchive = ids
  }

  function fmtSim(sim: string | null): string {
    if (!sim) return '—'
    const n = Number(sim)
    return Number.isNaN(n) ? sim : `${(n * 100).toFixed(1)}%`
  }
</script>

<section class="card" aria-label="guard review">
  <header class="card-head">
    <h2>guard review</h2>
    <span class="count">{visible.length} flagged</span>
    {#if live.section.counts}
      <span class="queue" title="needs_review / near_duplicate / possible_duplicate in your home scope">
        {live.section.counts.needs_review} / {live.section.counts.near_duplicate} / {live.section.counts
          .possible_duplicate}
      </span>
    {:else}
      <span class="queue" title="the server's flagged-block counts are unavailable — this is NOT an empty queue">
        — <span class="age">{queueAge}</span>
      </span>
    {/if}
    <span class="live"><ConnState sse={live} /></span>
  </header>

  <p class="note">
    Blocks the write guard flagged as possible duplicates. <strong>keep</strong> clears the flag,
    <strong>archive</strong> archives the block as a duplicate (the matched partner stays canonical).
    Flagged blocks keep ranking in retrieval until resolved.
  </p>

  <div class="controls">
    <label>
      status
      <select bind:value={statusFilter} disabled={loading || busy}>
        <option value="">all flagged</option>
        <option value="needs_review">needs_review</option>
        <option value="near_duplicate">near_duplicate</option>
        <option value="possible_duplicate">possible_duplicate</option>
      </select>
    </label>
    <label>
      type
      <select bind:value={typeFilter} disabled={loading || busy}>
        <option value="">all types</option>
        {#each types as t (t)}
          <option value={t}>{t}</option>
        {/each}
      </select>
    </label>
    <button type="button" onclick={() => void reload()} disabled={loading || busy}>Reload</button>
  </div>

  {#if picked.size > 0}
    <div class="batch-bar" role="toolbar" aria-label="batch actions">
      <span>{picked.size} selected</span>
      <button type="button" disabled={busy} onclick={() => void resolve([...picked], 'keep')}>Keep all</button>
      <button type="button" class="danger" disabled={busy} onclick={() => requestArchive([...picked])}>
        Archive all
      </button>
    </div>
  {/if}

  {#if error}
    <p class="err" role="alert">{error}</p>
  {/if}
  {#if lastSkipped.length > 0}
    <p class="skipped" role="status">
      {lastSkipped.length} skipped: {lastSkipped.map((s) => `${s.id.slice(0, 8)}… (${s.reason})`).join(', ')}
    </p>
  {/if}

  <Table>
    {#snippet head()}
      <tr>
        <th class="pick">
          <input
            type="checkbox"
            checked={allPicked}
            onchange={togglePickAll}
            aria-label="select all"
            disabled={visible.length === 0}
          />
        </th>
        <th>block</th>
        <th>type</th>
        <th>status</th>
        <th class="num">similarity</th>
        <th>matched</th>
        <th class="actions">actions</th>
      </tr>
    {/snippet}
    {#snippet emptyState()}
      <p class="empty">{loading ? 'loading…' : 'No flagged blocks — the queue is clear.'}</p>
    {/snippet}
    {#each visible as item (item.id)}
      <tr class:expanded={expanded === item.id}>
        <td class="pick">
          <input
            type="checkbox"
            checked={picked.has(item.id)}
            onchange={() => togglePick(item.id)}
            aria-label={`select ${item.title}`}
          />
        </td>
        <td>
          <button type="button" class="linkish" onclick={() => void toggleExpand(item)}>
            {item.title}
          </button>
          <span class="dim">[{item.category}]</span>
        </td>
        <td class="mono">{item.type_name}</td>
        <td class="mono">{item.guard_status}</td>
        <td class="num mono">{fmtSim(item.similarity)}</td>
        <td class="matched">{item.matched_title ?? '—'}</td>
        <td class="actions">
          <button type="button" disabled={busy} onclick={() => void resolve([item.id], 'keep')}>keep</button>
          <button type="button" class="danger" disabled={busy} onclick={() => requestArchive([item.id])}>
            archive
          </button>
        </td>
      </tr>
      {#if expanded === item.id}
        <tr class="pair-row">
          <td colspan="7">
            {#if pairLoading}
              <p class="dim">loading pair…</p>
            {:else}
              <div class="pair">
                <div class="pane">
                  <h4>flagged: {pair.left?.title ?? item.title}</h4>
                  <pre>{pair.left?.content ?? '—'}</pre>
                </div>
                <div class="pane">
                  <h4>matched: {pair.right?.title ?? item.matched_title ?? 'not visible in your scopes'}</h4>
                  <pre>{pair.right?.content ?? '—'}</pre>
                </div>
              </div>
            {/if}
          </td>
        </tr>
      {/if}
    {/each}
  </Table>
</section>

{#if confirmArchive}
  <ConfirmDialog
    title={`Archive ${confirmArchive.length} block(s)?`}
    message="Archived blocks leave retrieval; the matched partner stays canonical."
    confirmLabel="Archive"
    danger
    onconfirm={() => {
      const ids = confirmArchive ?? []
      confirmArchive = null
      void resolve(ids, 'archive')
    }}
    oncancel={() => (confirmArchive = null)}
  />
{/if}

<style>
  .card {
    background: var(--surface-1);
    border: 1px solid var(--border);
    border-radius: var(--radius);
  }
  .card-head {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-2) var(--space-3);
    border-bottom: 1px solid var(--border);
  }
  h2 {
    margin: 0;
    font-family: var(--font-mono);
    font-size: var(--fs-md);
    font-weight: var(--fw-semibold);
  }
  .count {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    color: var(--text-dim);
  }
  .queue {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    color: var(--text-faint);
  }
  .age {
    font-style: italic;
  }
  .live {
    margin-left: auto;
  }
  .note {
    margin: 0;
    padding: var(--space-2) var(--space-3);
    color: var(--text-faint);
    font-size: var(--fs-sm);
    border-bottom: 1px solid var(--surface-2);
  }
  .controls {
    display: flex;
    align-items: end;
    gap: var(--space-2);
    padding: var(--space-2) var(--space-3);
  }
  .controls label {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    font-size: var(--fs-xs);
    color: var(--text-dim);
  }
  .batch-bar {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-2) var(--space-3);
    border-top: 1px solid var(--surface-2);
    background: var(--surface-2);
    font-size: var(--fs-sm);
  }
  .err {
    margin: 0;
    padding: var(--space-2) var(--space-3);
    color: var(--danger);
    font-size: var(--fs-sm);
  }
  .skipped {
    margin: 0;
    padding: var(--space-2) var(--space-3);
    color: var(--text-dim);
    font-size: var(--fs-xs);
    font-family: var(--font-mono);
  }
  .empty {
    padding: var(--space-3);
    color: var(--text-faint);
  }
  th,
  td {
    text-align: left;
    padding: var(--space-1) var(--space-2);
  }
  .pick {
    width: 2rem;
  }
  .num {
    text-align: right;
  }
  .mono {
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
  }
  .dim {
    color: var(--text-faint);
    font-size: var(--fs-xs);
  }
  .matched {
    color: var(--text-dim);
    font-size: var(--fs-sm);
  }
  .actions {
    white-space: nowrap;
    text-align: right;
  }
  .linkish {
    background: none;
    border: none;
    padding: 0;
    color: var(--accent);
    cursor: pointer;
    font-size: var(--fs-sm);
    text-align: left;
  }
  button.danger {
    color: var(--danger);
    border-color: var(--danger-dim);
  }
  .pair-row td {
    background: var(--surface-2);
  }
  .pair {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: var(--space-2);
  }
  .pane h4 {
    margin: 0 0 var(--space-1);
    font-size: var(--fs-xs);
    font-family: var(--font-mono);
    color: var(--text-dim);
  }
  .pane pre {
    margin: 0;
    padding: var(--space-2);
    background: var(--surface-1);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    white-space: pre-wrap;
    overflow-wrap: anywhere;
    max-height: 22rem;
    overflow-y: auto;
    font-size: var(--fs-xs);
  }
</style>
