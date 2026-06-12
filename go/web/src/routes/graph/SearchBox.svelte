<script lang="ts">
  import { toApiError, type ApiError } from '../../lib/api'
  import { searchBlocks, type SearchResult } from '../../lib/graph/api'

  let { onpick }: { onpick: (id: string) => void } = $props()

  let query = $state('')
  let results = $state<SearchResult[]>([])
  let busy = $state(false)
  let error = $state<ApiError | null>(null)
  let searched = $state(false)

  // FTS entry point (design 05-§3.1): stemmed words match, substrings/typos
  // do not — plain /api/search, no LLM touched.
  async function submit(event: SubmitEvent): Promise<void> {
    event.preventDefault()
    const q = query.trim()
    if (q === '' || busy) return
    busy = true
    error = null
    try {
      const res = await searchBlocks(q, 10)
      results = res.results
      searched = true
    } catch (err) {
      error = toApiError(err)
    } finally {
      busy = false
    }
  }

  function pick(id: string): void {
    results = []
    searched = false
    onpick(id)
  }
</script>

<form class="search" onsubmit={submit}>
  <input
    type="search"
    placeholder="search blocks (FTS) — pick a hit to focus its ego net"
    spellcheck="false"
    bind:value={query}
  />
  <button type="submit" disabled={busy || query.trim() === ''}>{busy ? '…' : 'Search'}</button>
</form>

{#if error}
  <p class="error" role="alert">{error.message}</p>
{:else if searched && results.length === 0}
  <p class="empty" role="status">no FTS match — words are stemmed, substrings don't match</p>
{:else if results.length > 0}
  <ul class="results">
    {#each results as r (r.id)}
      <li>
        <button type="button" onclick={() => pick(r.id)}>
          <span class="title">{r.title}</span>
          <span class="meta">{r.category} · {r.scope}</span>
          <span class="preview">{r.content_preview.slice(0, 140)}</span>
        </button>
      </li>
    {/each}
  </ul>
{/if}

<style>
  .search {
    display: flex;
    gap: var(--space-2);
  }
  .search input {
    flex: 1;
    min-width: 0;
    font-family: var(--font-mono);
    font-size: 0.85rem;
  }

  .error {
    margin: var(--space-2) 0 0;
    color: var(--danger);
    font-size: 0.85rem;
  }
  .empty {
    margin: var(--space-2) 0 0;
    color: var(--text-faint);
    font-size: 0.85rem;
  }

  .results {
    list-style: none;
    margin: var(--space-2) 0 0;
    padding: 0;
    border: 1px solid var(--border);
    border-radius: var(--radius);
    background: var(--surface-1);
    max-height: 22rem;
    overflow-y: auto;
  }
  .results li + li {
    border-top: 1px solid var(--border);
  }
  .results button {
    display: flex;
    flex-direction: column;
    gap: 0.15rem;
    width: 100%;
    text-align: left;
    background: transparent;
    border: none;
    border-radius: 0;
    padding: var(--space-2) var(--space-3);
  }
  .results button:hover {
    background: var(--surface-2);
  }
  .title {
    font-size: 0.9rem;
    color: var(--text);
  }
  .meta {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    color: var(--text-faint);
  }
  .preview {
    font-size: 0.78rem;
    color: var(--text-dim);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    max-width: 100%;
  }
</style>
