<script lang="ts">
  // Status dashboard (design 04-§3.6, W6/G33 + W7/G34). Live updates over SSE
  // (GET /api/events): the server diffs the shared collector snapshot once per
  // tick and pushes `status`/`backends`/`llmcall` events through ONE render
  // path. A poll of GET /api/status is the fallback whenever the stream is not
  // open (connection cap → 429, network blip, server restart).
  import { fetchPublicHealth, fetchStatus, setGamingMode } from '../../lib/api/status'
  import type {
    BackendStatus,
    HealthStatus,
    LLMLogEntry,
    StatusEvent,
    StatusResponse,
  } from '../../lib/api/types'
  import { session } from '../../lib/auth.svelte'
  import { Resource } from '../../lib/resource.svelte'
  import { SseClient } from '../../lib/sse.svelte'
  import BackendsTile from './BackendsTile.svelte'
  import DreamTile from './DreamTile.svelte'
  import LlmlogTable from './LlmlogTable.svelte'

  const POLL_MS = 5000
  const status = new Resource<StatusResponse>(() => fetchStatus())
  const publicHealth = new Resource<HealthStatus>(() => fetchPublicHealth())

  let gamingBusy = $state(false)
  // Live llmcall rows pushed since connect; LlmlogTable merges them with its
  // own fetched history (client-side filter + dedup by id).
  let liveLlmcalls = $state<LLMLogEntry[]>([])
  // backends arrives right after status on connect; buffer only for the
  // theoretical out-of-order case so a backends-first frame is never dropped.
  let pendingBackends: BackendStatus[] | null = null

  function onSseEvent(name: string, data: unknown): void {
    if (name === 'status') {
      const e = data as StatusEvent
      status.data = { success: true, ...e, backends: pendingBackends ?? status.data?.backends ?? [] }
      pendingBackends = null
      status.status = 'ready'
      status.error = null
    } else if (name === 'backends') {
      const be = data as BackendStatus[]
      if (status.data) status.data = { ...status.data, backends: be }
      else pendingBackends = be
    } else if (name === 'llmcall') {
      liveLlmcalls = [data as LLMLogEntry, ...liveLlmcalls].slice(0, 200)
    } else if (name === 'error') {
      // Server ended the stream (revoked key, §3.6 re-auth). A reload returns
      // 401 → the api client's interceptor tears the session down → login.
      void status.reload()
    }
  }

  const sse = new SseClient('/api/events', onSseEvent, () => {
    const headers: Record<string, string> = {}
    if (session.key) headers.Authorization = `Bearer ${session.key}`
    return { headers }
  })

  // Admin: hold one SSE stream open for the tab's life. Non-admin keys get 403
  // and never open one (the read-only branch shows the public /health tile).
  $effect(() => {
    if (!session.admin) return
    void sse.connect()
    return () => sse.close()
  })

  // Poll fallback: while the stream is not delivering (connecting, capped,
  // errored, closed) an admin tab polls GET /api/status — same shape, one
  // render path. Stops the moment SSE is 'open'.
  $effect(() => {
    if (!session.admin || sse.status === 'open') return
    void status.reload()
    const timer = setInterval(() => {
      if (document.visibilityState === 'visible') void status.reload()
    }, POLL_MS)
    return () => clearInterval(timer)
  })

  // Non-admin degradation: the anonymous /health probe instead of a doomed
  // admin request.
  $effect(() => {
    if (session.admin) return
    void publicHealth.load()
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

  // K-f-Klassen-Mapping (Q3, design 05-§4.8.2): Daten→Farbe nur über
  // CSS-Klassen geschlossener Mengen — nie über style-Attribute.
  function healthClass(s: string): string {
    if (s === 'ok') return 'ok'
    if (s === 'degraded') return 'warn'
    return 'danger'
  }
  function svcClass(v: string): string {
    return v === 'ok' ? 'ok' : 'danger'
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
        <span class="dot big {healthClass(h.status)}"></span>
        <strong>{h.status}</strong>
        <div class="svcs">
          {#each Object.entries(h.services) as [name, value] (name)}
            <span class="svc"><span class="dot {svcClass(value)}"></span>{name}</span>
          {/each}
        </div>
      </section>
    {:else if publicHealth.status === 'error'}
      <p class="state error">health probe failed: {publicHealth.error?.message}</p>
    {:else}
      <p class="state" aria-busy="true">loading health…</p>
    {/if}
  {:else if (status.status === 'loading' && status.data === null) || status.status === 'idle'}
    <!-- Nur der ERSTE Load zeigt den Loading-Zweig. Ein Hintergrund-Reload
         (Poll-Fallback alle 5 s bzw. nach jedem SSE-Reconnect-Versuch) behält
         die gemounteten Tiles: der Zweig-Flip würde sonst pro Tick den
         kompletten Tile-DOM zerstören und neu aufbauen — sichtbares Flackern
         und Verlust des Tastatur-Fokus für den Operator (SC 2.4.3-relevant;
         gefunden über den e2e-Focus-Walk, der unter CI-Last mitten im
         Tab-Durchlauf den Fokus an body verlor). -->
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
        <span class="dot big {healthClass(s.health.status)}"></span>
        <strong>{s.health.status}</strong>
        <div class="svcs">
          {#each Object.entries(s.health.services) as [name, value] (name)}
            <span class="svc"><span class="dot {svcClass(value)}"></span>{name}</span>
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
    <LlmlogTable complete={s.llm_24h_complete} live={liveLlmcalls} />
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
    font-size: var(--fs-sm);
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
    font-size: var(--fs-xs);
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
    font-size: var(--fs-sm);
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
  .dot.ok {
    background: var(--ok);
  }
  .dot.warn {
    background: var(--warn);
  }
  .dot.danger {
    background: var(--danger);
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
    font-size: var(--fs-md);
  }
  .hint {
    margin: var(--space-1) 0 0;
    color: var(--text-faint);
    font-size: var(--fs-xs);
  }
  .muted {
    color: var(--text-faint);
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
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
