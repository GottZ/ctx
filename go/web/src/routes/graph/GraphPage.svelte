<script lang="ts">
  import { onMount } from 'svelte'
  import { toApiError, type ApiError } from '../../lib/api'
  import { fetchEgo } from '../../lib/graph/api'
  import { createGraph, mergeEgo, recomputeHops } from '../../lib/graph/graph-client'
  import GraphView from './GraphView.svelte'
  import SearchBox from './SearchBox.svelte'

  // ONE graphology instance per page visit = single source of truth
  // (design 05-§3.5). Deliberately NOT $state — sigma observes the graph
  // through graphology events, Svelte only tracks the scalars below.
  const graph = createGraph()

  let view = $state<ReturnType<typeof GraphView> | null>(null)
  let focus = $state<string | null>(null)
  let busy = $state(false)
  let error = $state<ApiError | null>(null)
  let stats = $state<{ nodes: number; edges: number; truncated: boolean } | null>(null)

  // Deep-link sync: /graph?focus=<uuid> survives reload and is shareable.
  onMount(() => {
    const fromUrl = new URLSearchParams(location.search).get('focus')
    if (fromUrl) void setFocus(fromUrl, { pushUrl: false })
  })

  async function setFocus(id: string, opts: { pushUrl?: boolean } = {}): Promise<void> {
    if (busy) return
    busy = true
    error = null
    try {
      const resp = await fetchEgo(id, { hops: 2 })
      focus = id
      mergeEgo(graph, resp)
      recomputeHops(graph, id)
      stats = resp.stats
      if (opts.pushUrl !== false) {
        const url = new URL(location.href)
        url.searchParams.set('focus', id)
        history.replaceState(null, '', url)
      }
      view?.resetCamera()
    } catch (err) {
      error = toApiError(err)
    } finally {
      busy = false
    }
  }
</script>

<section class="area">
  <header>
    <h1>Graph</h1>
    <p class="sub">
      dream-link ego networks — search a block, click a hit to focus; click a node to re-focus
      (read-only, no LLM touched)
    </p>
  </header>

  <SearchBox onpick={(id) => void setFocus(id)} />

  {#if error}
    <div class="error" role="alert">
      <p>{error.message}</p>
      {#if error.requestId}
        <p class="request-id">request {error.requestId}</p>
      {/if}
    </div>
  {/if}

  {#if focus !== null}
    <div class="meta-row">
      <code class="focus" title="focused block">{focus}</code>
      {#if busy}
        <span class="loading" aria-busy="true">loading…</span>
      {/if}
      {#if stats}
        <span class="stats">
          {graph.order} nodes · {graph.size} edges{stats.truncated ? ' · server truncated' : ''}
        </span>
      {/if}
    </div>
    <div class="viewport">
      <GraphView bind:this={view} {graph} onnodeclick={(id) => void setFocus(id)} />
    </div>
  {:else}
    <p class="hint">no focus yet — search above or open a deep link like <code>/graph?focus=&lt;uuid&gt;</code></p>
  {/if}
</section>

<style>
  .area {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    height: 100%;
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

  .error {
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

  .meta-row {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    min-height: 1.4rem;
  }
  .focus {
    font-size: 0.75rem;
  }
  .loading {
    color: var(--text-faint);
    font-family: var(--font-mono);
    font-size: 0.8rem;
  }
  .stats {
    margin-left: auto;
    color: var(--text-dim);
    font-family: var(--font-mono);
    font-size: 0.8rem;
  }

  .viewport {
    flex: 1;
    min-height: 30rem;
    border: 1px solid var(--border);
    border-radius: var(--radius);
    overflow: hidden;
  }

  .hint {
    margin: 0;
    color: var(--text-faint);
    font-size: 0.875rem;
  }
</style>
