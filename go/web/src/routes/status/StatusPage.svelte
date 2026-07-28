<script lang="ts">
  // Status dashboard (design 04-§3.6, W6/G33 + W7/G34). Live updates over SSE
  // (GET /api/events): the server diffs the shared collector snapshot once per
  // tick and pushes `status`/`backends`/`llmcalls` events through ONE render
  // path. A poll of GET /api/status is the fallback whenever the stream is not
  // open (connection cap → 429, network blip, server restart).
  import { fetchPublicHealth } from '../../lib/api/status'
  import { toggleDisableProfile } from '../../lib/api/profiles'
  import type { BackendStatus, HealthStatus, StatusEvent, StatusProfile } from '../../lib/api/types'
  import { session } from '../../lib/auth.svelte'
  import { Resource } from '../../lib/resource.svelte'
  import { SseClient } from '../../lib/sse.svelte'
  import BlackoutConfirm from '../../lib/components/BlackoutConfirm.svelte'
  import { LlmcallFeed } from './llmcall-feed.svelte'
  import { StatusStore } from './status-store.svelte'
  import BackendsTile from './BackendsTile.svelte'
  import DispatchTile from './DispatchTile.svelte'
  import DreamTile from './DreamTile.svelte'
  import LlmlogTable from './LlmlogTable.svelte'

  const POLL_MS = 5000
  // StatusStore holds data + the as_of-floor: every SSE frame / poll answer is
  // order-guarded, and a profile/dream mutation is spliced in + raises the floor
  // (U01-W7, §4.5). It replaces the raw Resource so a late frame can no longer
  // overwrite a just-toggled profile.
  const status = new StatusStore()
  const publicHealth = new Resource<HealthStatus>(() => fetchPublicHealth())

  // Live llmcall rows pushed since connect; LlmlogTable merges them with its
  // own fetched history (client-side filter + dedup by id). A tick above the
  // coalesce threshold arrives content-free instead (S0) and raises the feed's
  // refetch token, which the table answers with its own GET /api/llmlog.
  const llmcalls = new LlmcallFeed()

  function onSseEvent(name: string, data: unknown): void {
    if (name === 'status') {
      status.applyStatusFrame(data as StatusEvent)
    } else if (name === 'backends') {
      status.applyBackends(data as BackendStatus[])
    } else if (name === 'error') {
      // Server ended the stream (revoked key, §3.6 re-auth). A reload returns
      // 401 → the api client's interceptor tears the session down → login.
      void status.reload()
    } else {
      // `llmcalls` — and every name this client does not know, which the feed
      // drops silently rather than crash the render path.
      llmcalls.apply(name, data)
    }
  }

  // Auth fährt als httpOnly-Session-Cookie mit (OAuth R4); der GET-Stream
  // braucht weder Bearer noch CSRF — kein Extra-Init nötig.
  const sse = new SseClient('/api/events', onSseEvent)

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

  // --- Profile quick-toggles (§4.6) ------------------------------------------
  // The toggles card loops over the live s.profiles. A flip ON first dry-runs to
  // learn the role-blackout impact; an emptying role raises the shared Klartext
  // confirm (same fail-closed a11y contract as the backends page). Every write
  // is spliced into the held status by (name, scope) — no reload, and the answer
  // as_of raises the floor so a late SSE frame cannot revert it.
  let busyKey = $state<string | null>(null)
  let toggleError = $state<string | null>(null)

  interface StatusBlackout {
    name: string
    scope: string
    label: string
    roles: string[]
    embedDegraded: boolean
    trigger: HTMLElement | null
  }
  let confirm = $state<StatusBlackout | null>(null)

  function pKey(p: StatusProfile): string {
    return `${p.scope} ${p.name}`
  }
  function profileLabel(p: StatusProfile): string {
    return p.label !== '' ? p.label : p.name
  }

  async function toggleProfile(p: StatusProfile, trigger: HTMLElement): Promise<void> {
    const key = pKey(p)
    if (busyKey === key || (confirm?.name === p.name && confirm?.scope === p.scope)) return
    const next = !p.active
    busyKey = key
    toggleError = null
    try {
      if (!next) {
        const r = await toggleDisableProfile(p.name, p.scope, false)
        status.spliceProfile(r.profile, r.as_of)
        return
      }
      // Dry-run to learn the blackout set WITHOUT writing (§5.1).
      const dry = await toggleDisableProfile(p.name, p.scope, true, { dryRun: true })
      if (dry.impact.roles_blacked_out.length > 0) {
        confirm = {
          name: p.name,
          scope: p.scope,
          label: profileLabel(p),
          roles: dry.impact.roles_blacked_out.filter((r) => r !== 'embed'),
          embedDegraded: dry.impact.embed_degraded === true,
          trigger,
        }
      } else {
        const r = await toggleDisableProfile(p.name, p.scope, true)
        status.spliceProfile(r.profile, r.as_of)
      }
    } catch (e) {
      toggleError = e instanceof Error ? e.message : String(e)
    } finally {
      busyKey = null
    }
  }

  async function acceptBlackout(): Promise<void> {
    if (!confirm) return
    const c = confirm
    confirm = null // tears down the trap → focus returns to the switch
    busyKey = `${c.scope} ${c.name}`
    toggleError = null
    try {
      const r = await toggleDisableProfile(c.name, c.scope, true, { confirm: true })
      status.spliceProfile(r.profile, r.as_of)
    } catch (e) {
      toggleError = e instanceof Error ? e.message : String(e)
    } finally {
      busyKey = null
    }
  }

  function cancelBlackout(): void {
    confirm = null // trap teardown returns focus to the switch
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
        {#if toggleError}
          <p class="toggle-error" role="alert">{toggleError}</p>
        {/if}
        {#each s.profiles ?? [] as p (pKey(p))}
          {@const key = pKey(p)}
          <div class="toggle">
            <div class="toggle-info">
              <strong>{profileLabel(p)}</strong>
              <p class="hint">
                {p.member_count} {p.member_count === 1 ? 'Backend' : 'Backends'} ·
                <a href="/settings/backends">konfigurieren →</a>
              </p>
            </div>
            <!-- NOT disabled while the confirm is open: a disabled button can't
                 take focus, and the a11y contract returns focus HERE on Escape.
                 A re-click while confirming is guarded inside toggleProfile. -->
            <button
              type="button"
              class="switch"
              class:on={p.active}
              disabled={busyKey === key}
              aria-pressed={p.active}
              aria-label={`${profileLabel(p)} umschalten`}
              onclick={(e) => void toggleProfile(p, e.currentTarget)}
            >
              {p.active ? 'ON' : 'OFF'}
            </button>
          </div>
          {#if confirm?.name === p.name && confirm?.scope === p.scope}
            <BlackoutConfirm
              label={confirm.label}
              roles={confirm.roles}
              embedDegraded={confirm.embedDegraded}
              trigger={confirm.trigger}
              onconfirm={() => void acceptBlackout()}
              oncancel={cancelBlackout}
            />
          {/if}
        {/each}
        <div class="toggle">
          <div class="toggle-info">
            <strong>activity</strong>
            <p class="hint">host idle signal</p>
          </div>
          <span class="muted">{s.activity ? `${s.activity.host} · ${s.activity.idle_ms}ms` : 'no signal'}</span>
        </div>
      </section>
    </div>

    <DreamTile dream={s.dream} onApplied={(r) => status.applyDream(r)} />
    <BackendsTile backends={s.backends} />
    <DispatchTile dispatch={s.dispatch} dispatchTenant={s.dispatch_tenant} />
    <LlmlogTable complete={s.llm_24h_complete} live={llmcalls.rows} refetch={llmcalls.refetchToken} />
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
  .hint a {
    color: var(--accent);
    text-decoration: none;
  }
  .hint a:hover {
    text-decoration: underline;
  }
  .toggle-info {
    min-width: 0;
  }
  .toggle-error {
    margin: 0;
    padding: var(--space-2) var(--space-3);
    color: var(--danger);
    font-size: var(--fs-sm);
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
