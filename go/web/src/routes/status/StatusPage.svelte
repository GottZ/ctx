<script lang="ts">
  // Status dashboard (design 04-§3.6, W6/G33). Polls GET /api/status every 5s
  // while the tab is visible (document.visibilityState) — the server serves it
  // from the process-wide collector cache, so N tabs cost one refresh. SSE
  // (W7/G34) will later push the same shapes through one render path.
  import { onMount } from 'svelte'
  import { fetchPublicHealth, fetchStatus, setGamingMode } from '../../lib/api/status'
  import type { HealthStatus, StatusResponse } from '../../lib/api/types'
  import { session } from '../../lib/auth.svelte'
  import { Resource } from '../../lib/resource.svelte'
  import BackendsTile from './BackendsTile.svelte'
  import DreamTile from './DreamTile.svelte'
  import LlmlogTable from './LlmlogTable.svelte'

  const POLL_MS = 5000
  const status = new Resource<StatusResponse>(() => fetchStatus())
  const publicHealth = new Resource<HealthStatus>(() => fetchPublicHealth())

  let gamingBusy = $state(false)
  let timer: ReturnType<typeof setInterval> | null = null

  function onVisible() {
    if (document.visibilityState === 'visible') void status.reload()
  }

  onMount(() => {
    // /api/status is admin-only (403 for read-only keys) — degrade to the
    // anonymous /health tile instead of firing a doomed request.
    if (!session.admin) {
      void publicHealth.load()
      return
    }
    void status.load()
    timer = setInterval(() => {
      if (document.visibilityState === 'visible') void status.reload()
    }, POLL_MS)
    document.addEventListener('visibilitychange', onVisible)
    return () => {
      if (timer) clearInterval(timer)
      document.removeEventListener('visibilitychange', onVisible)
    }
  })

  async function toggleGaming(active: boolean) {
    if (gamingBusy) return
    gamingBusy = true
    try {
      await setGamingMode(active)
      await status.reload()
    } finally {
      gamingBusy = false
    }
  }

  function healthColor(s: string): string {
    if (s === 'ok') return 'var(--ok)'
    if (s === 'degraded') return 'var(--warn)'
    return 'var(--danger)'
  }
  function svcColor(v: string): string {
    return v === 'ok' ? 'var(--ok)' : 'var(--danger)'
  }
  function fmtAge(asOf: string): string {
    const s = Math.max(0, Math.round((Date.now() - new Date(asOf).getTime()) / 1000))
    return `${s}s ago`
  }
</script>

<section class="area">
  <header>
    <h1>Status</h1>
    <p class="sub">live system health, backend pool, dream queue and LLM telemetry</p>
  </header>

  {#if !session.admin}
    <p class="banner" role="status">
      read-only key — the full dashboard is admin-gated (the server answers 403). Showing the public
      health probe only.
    </p>
    {#if publicHealth.status === 'ready' && publicHealth.data}
      {@const h = publicHealth.data}
      <section class="card health" aria-label="health">
        <span class="dot big" style="background:{healthColor(h.status)}"></span>
        <strong>{h.status}</strong>
        <div class="svcs">
          {#each Object.entries(h.services) as [name, value] (name)}
            <span class="svc"><span class="dot" style="background:{svcColor(value)}"></span>{name}</span>
          {/each}
        </div>
      </section>
    {:else if publicHealth.status === 'error'}
      <p class="state error">health probe failed: {publicHealth.error?.message}</p>
    {:else}
      <p class="state" aria-busy="true">loading health…</p>
    {/if}
  {:else if status.status === 'loading' || status.status === 'idle'}
    <p class="state" aria-busy="true">loading status…</p>
  {:else if status.status === 'error'}
    <div class="state error" role="alert">
      <p>{status.error?.message}</p>
      {#if status.error?.requestId}<p class="request-id">request {status.error.requestId}</p>{/if}
      <button type="button" onclick={() => void status.reload()}>Retry</button>
    </div>
  {:else if status.data}
    {@const s = status.data}
    <p class="asof">updated {fmtAge(s.as_of)}</p>

    <div class="tiles">
      <section class="card health" aria-label="health">
        <span class="dot big" style="background:{healthColor(s.health.status)}"></span>
        <strong>{s.health.status}</strong>
        <div class="svcs">
          {#each Object.entries(s.health.services) as [name, value] (name)}
            <span class="svc"><span class="dot" style="background:{svcColor(value)}"></span>{name}</span>
          {/each}
        </div>
      </section>

      <section class="card toggles" aria-label="toggles">
        <div class="toggle">
          <div>
            <strong>gaming lock</strong>
            <p class="hint">excludes the GPU backends from every chain</p>
          </div>
          <button
            type="button"
            class="switch"
            class:on={s.gaming.active}
            disabled={gamingBusy}
            aria-pressed={s.gaming.active}
            onclick={() => toggleGaming(!s.gaming.active)}
          >
            {s.gaming.active ? 'ON' : 'OFF'}
          </button>
        </div>
        <div class="toggle">
          <div>
            <strong>activity</strong>
            <p class="hint">host idle signal</p>
          </div>
          <span class="muted">{s.activity ? `${s.activity.host} · ${s.activity.idle_ms}ms` : 'no signal'}</span>
        </div>
      </section>
    </div>

    <DreamTile dream={s.dream} onRefresh={() => void status.reload()} />
    <BackendsTile backends={s.backends} />
    <LlmlogTable complete={s.llm_24h_complete} />
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
  .asof {
    margin: 0;
    color: var(--text-faint);
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
  }
  .state {
    margin: 0;
    color: var(--text-faint);
    font-family: var(--font-mono);
    font-size: 0.85rem;
  }
  .state.error {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    align-items: flex-start;
    border: 1px solid var(--danger);
    border-radius: var(--radius);
    background: var(--danger-dim);
    padding: var(--space-2) var(--space-3);
    color: var(--danger);
  }
  .request-id {
    font-family: var(--font-mono);
    font-size: 0.75rem;
    color: var(--text-dim);
  }
  .tiles {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(18rem, 1fr));
    gap: var(--space-3);
  }
  .card {
    background: var(--surface-1);
    border: 1px solid var(--border);
    border-radius: var(--radius);
  }
  .health {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    flex-wrap: wrap;
    padding: var(--space-3);
  }
  .health strong {
    font-family: var(--font-mono);
    text-transform: uppercase;
  }
  .svcs {
    display: flex;
    gap: var(--space-3);
    flex-wrap: wrap;
    margin-left: var(--space-2);
  }
  .svc {
    color: var(--text-dim);
    font-size: 0.82rem;
    display: inline-flex;
    align-items: center;
  }
  .dot {
    display: inline-block;
    width: 0.55rem;
    height: 0.55rem;
    border-radius: 50%;
    margin-right: var(--space-1);
    vertical-align: middle;
  }
  .dot.big {
    width: 0.8rem;
    height: 0.8rem;
  }
  .toggles {
    display: flex;
    flex-direction: column;
  }
  .toggle {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-3);
    padding: var(--space-2) var(--space-3);
  }
  .toggle + .toggle {
    border-top: 1px solid var(--border);
  }
  .toggle strong {
    font-size: 0.9rem;
  }
  .hint {
    margin: var(--space-1) 0 0;
    color: var(--text-faint);
    font-size: 0.75rem;
  }
  .muted {
    color: var(--text-faint);
    font-family: var(--font-mono);
    font-size: 0.8rem;
  }
  .switch {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    padding: var(--space-1) var(--space-3);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    background: var(--surface-2);
    color: var(--text-dim);
    cursor: pointer;
  }
  .switch.on {
    background: var(--accent-dim);
    color: var(--accent);
    border-color: var(--accent);
  }
  .switch:disabled {
    opacity: 0.6;
    cursor: default;
  }
</style>
