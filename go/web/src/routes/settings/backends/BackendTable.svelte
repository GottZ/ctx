<script lang="ts">
  // Backend-pool table (design §3.5): the editable view of backend-list. The
  // chain order (priority DESC, name ASC) is steered three ways, all through
  // ONE atomic backend-reorder write: a per-row drag handle (raw pointer
  // events, no lib — rows FLIP into place while dragging), the ▲▼ buttons as
  // the keyboard fallback, and a click-to-edit prio cell for direct numeric
  // entry (single-field PATCH). Each backend renders as its own <tbody> group
  // (Table grouped mode) so the row + its expandable test row move as one
  // animated unit. While a mutation is in flight the model's `mutating` guard
  // no-ops every other one; the INITIATING control stays enabled and carries
  // aria-busy — disabling it would drop keyboard focus. enabled toggles
  // inline; Test runs the reachability/chat probe per row (read-only, own
  // running state); Edit opens the tuple dialog; Delete confirms inline. The
  // secret column joins api_key_ref against the known secret names (a missing
  // one is the derived "fehlt" status). Health/state is read-only from the
  // merged live status — mutation and status are separate server paths.
  import { flip } from 'svelte/animate'
  import { toApiError } from '../../../lib/api'
  import type { BackendListItem, BackendTestResult } from '../../../lib/api/types'
  import { backendDiff, draftFromBackend, isEmptySpec } from '../../../lib/backends'
  import Table from '../../../lib/ui/Table.svelte'
  import type { PoolModel } from './pool.svelte'
  import BackendDialog from './BackendDialog.svelte'

  let {
    pool,
    knownSecrets,
    profileOptions = [],
  }: { pool: PoolModel; knownSecrets: Set<string>; profileOptions?: { name: string; label: string }[] } = $props()

  let editing = $state<{ mode: 'create' | 'edit'; backend: BackendListItem | null } | null>(null)
  let confirmDeleteId = $state<string | null>(null)
  let notice = $state<string[]>([])
  type TestState = BackendTestResult | { error: string } | { running: true }
  let testResults = $state<Record<string, TestState>>({})

  // Drag state: dragIds is the working order while a handle is held — the
  // rows render from it so every hover-crossing FLIPs immediately; commit
  // happens once on drop. null = not dragging (render the model's sort).
  let dragId = $state<string | null>(null)
  let dragIds = $state<string[] | null>(null)

  // Click-to-edit priority cell. prioCancel guards the blur that may follow
  // an Escape (Escape must discard, not commit).
  let prioEdit = $state<{ id: string; text: string } | null>(null)
  let prioCancel = false

  const rows = $derived.by(() => {
    if (dragIds === null) return pool.sorted
    const byId = new Map(pool.sorted.map((b) => [b.id, b]))
    return dragIds.flatMap((id) => byId.get(id) ?? [])
  })

  // K-f-Klassen-Mapping (Q3): active ok, cooldown/profile-disabled warn,
  // sonst idle (fail-closed). profile-disabled (092, U01-W2) reuses the warn
  // token — intentionally out, distinct from a broken/disabled row.
  function stateClass(s: string): string {
    if (s === 'active') return 'ok'
    if (s === 'cooldown') return 'warn'
    if (s === 'profile-disabled') return 'profile'
    return 'idle'
  }

  function dragStart(e: PointerEvent, id: string): void {
    if (pool.mutating) return
    e.preventDefault() // no text selection / focus scroll while dragging
    dragId = id
    dragIds = pool.sorted.map((b) => b.id)
    ;(e.currentTarget as HTMLElement).setPointerCapture(e.pointerId)
  }

  function dragMove(e: PointerEvent): void {
    if (dragId === null || dragIds === null) return
    // Pointer capture keeps events on the handle; hit-testing finds the
    // hovered row group by its data-id.
    const over = document.elementFromPoint(e.clientX, e.clientY)?.closest<HTMLElement>('tbody[data-id]')
    const overId = over?.dataset.id
    if (overId === undefined || overId === dragId) return
    const from = dragIds.indexOf(dragId)
    const to = dragIds.indexOf(overId)
    if (from < 0 || to < 0 || from === to) return
    const next = [...dragIds]
    next.splice(from, 1)
    next.splice(to, 0, dragId)
    dragIds = next
  }

  function dragEnd(): void {
    if (dragId === null || dragIds === null) return
    const before = pool.sorted.map((b) => b.id)
    const after = dragIds
    dragId = null
    dragIds = null
    if (after.some((id, i) => id !== before[i])) void pool.reorder(after)
  }

  function dragCancel(): void {
    dragId = null
    dragIds = null
  }

  function openPrioEdit(b: BackendListItem): void {
    if (pool.mutating) return
    prioCancel = false
    prioEdit = { id: b.id, text: String(b.priority) }
  }

  async function commitPrio(b: BackendListItem, raw: string): Promise<void> {
    const cancelled = prioCancel
    prioCancel = false
    prioEdit = null
    if (cancelled) return
    const n = parseInt(raw, 10)
    if (!Number.isFinite(n) || n === b.priority) return
    await pool.setPriority(b.id, n)
  }

  function focusSelect(el: HTMLInputElement): void {
    el.focus()
    el.select()
  }

  async function save(args: {
    mode: 'create' | 'edit'
    name: string
    original: BackendListItem | null
    draft: ReturnType<typeof draftFromBackend>
    confirmTrust: boolean
  }): Promise<string[]> {
    if (args.mode === 'create') {
      notice = await pool.create(args.name, args.draft, { confirmTrustElevation: args.confirmTrust })
      return notice
    }
    const spec = backendDiff(args.original as BackendListItem, args.draft)
    if (isEmptySpec(spec)) {
      notice = []
      return [] // nothing changed — close without a round-trip
    }
    notice = await pool.update((args.original as BackendListItem).id, spec, {
      confirmTrustElevation: args.confirmTrust,
    })
    return notice
  }

  function isTesting(id: string): boolean {
    const t = testResults[id]
    return t !== undefined && 'running' in t
  }

  async function runTest(id: string, probeChat: boolean): Promise<void> {
    if (isTesting(id)) return
    testResults[id] = { running: true }
    try {
      testResults[id] = await pool.test(id, probeChat)
    } catch (err) {
      testResults[id] = { error: toApiError(err).message }
    }
  }

  async function doDelete(id: string): Promise<void> {
    confirmDeleteId = null
    try {
      await pool.remove(id)
    } catch {
      /* pool.actionError carries it */
    }
  }

  // The checkbox flips in the DOM before the server answers — snap it to the
  // model's truth afterwards (guarded no-op, error reload and success all
  // land here; Svelte skips the write when the bound value did not change).
  async function toggleEnabled(b: BackendListItem, el: HTMLInputElement): Promise<void> {
    if (pool.mutating) {
      el.checked = b.enabled
      return
    }
    await pool.setEnabled(b.id, el.checked)
    const now = pool.byId(b.id)
    if (now !== undefined) el.checked = now.enabled
  }

  function openDialog(mode: 'create' | 'edit', backend: BackendListItem | null): void {
    notice = [] // a stale warning from the last save must not outlive it
    editing = { mode, backend }
  }
</script>

<section class="card" aria-label="backend pool editor">
  <header>
    <h2>backend pool</h2>
    <span class="count">{pool.backends.length}</span>
    <button type="button" class="new" onclick={() => openDialog('create', null)}>+ new backend</button>
  </header>

  {#if pool.actionError}
    <div class="problem error alert" role="alert">
      <p>{pool.actionError}</p>
      <button type="button" class="dismiss" aria-label="dismiss error" onclick={() => (pool.actionError = null)}>×</button>
    </div>
  {/if}
  {#if notice.length > 0}
    <div class="problem warn" role="status">
      {#each notice as w (w)}<p>{w}</p>{/each}
      <button type="button" class="dismiss" onclick={() => (notice = [])}>dismiss</button>
    </div>
  {/if}

  <Table grouped empty={pool.backends.length === 0}>
    {#snippet emptyState()}
      <p class="empty">no backends — create one to populate the pool</p>
    {/snippet}
    {#snippet head()}
      <tr>
        <th>name</th>
        <th>trust</th>
        <th>roles</th>
        <th class="num">prio</th>
        <th>secret</th>
        <th>state</th>
        <th class="act">actions</th>
      </tr>
    {/snippet}
    {#each rows as b (b.id)}
      <tbody data-id={b.id} animate:flip={{ duration: 150 }}>
        <tr
          class:off={!b.enabled}
          class:profile-off={b.effective_state === 'profile-disabled'}
          class:dragging={dragId === b.id}
        >
          <td class="name">
            {b.name}
            <span class="meta">{b.locality} · {b.protocol} · {b.base_url}</span>
            {#if b.disable_profiles && b.disable_profiles.length > 0}
              <span class="memberships">
                {#each b.disable_profiles as pm (pm)}
                  <span class="pchip" class:pchip-active={b.disabled_by_profiles?.includes(pm)} title={b.disabled_by_profiles?.includes(pm) ? 'aktives Abschaltprofil' : 'Abschaltprofil-Mitglied'}>{pm}</span>
                {/each}
              </span>
            {/if}
          </td>
          <td><span class="badge">{b.trust}</span></td>
          <td class="roles">{b.roles.join(', ') || '—'}</td>
          <td class="num">
            <div class="prio">
              {#if pool.isWritable(b)}
                <button
                  type="button"
                  class="drag"
                  title={`drag to reorder: ${b.name}`}
                  aria-label={`drag to reorder: ${b.name}`}
                  onpointerdown={(e) => dragStart(e, b.id)}
                  onpointermove={dragMove}
                  onpointerup={dragEnd}
                  onpointercancel={dragCancel}
                >⠿</button>
              {:else}
                <!-- T37: _global row in a tenant view — visible, not mutable;
                     no handle, the ladder skips it (reorder sends writable only). -->
                <span class="drag placeholder" title="managed at server scope" aria-hidden="true">·</span>
              {/if}
              {#if prioEdit?.id === b.id}
                <input
                  class="prio-input"
                  type="number"
                  step="1"
                  value={prioEdit.text}
                  aria-label={`priority: ${b.name}`}
                  use:focusSelect
                  onkeydown={(e) => {
                    if (e.key === 'Escape') {
                      prioCancel = true
                      prioEdit = null
                    } else if (e.key === 'Enter') {
                      e.preventDefault()
                      e.currentTarget.blur() // blur commits — one path for Enter and click-away
                    }
                  }}
                  onblur={(e) => void commitPrio(b, e.currentTarget.value)}
                />
              {:else if pool.isWritable(b)}
                <button type="button" class="prio-val" title={`edit priority: ${b.name}`} onclick={() => openPrioEdit(b)}>{b.priority}</button>
              {:else}
                <span class="prio-val readonly">{b.priority}</span>
              {/if}
              {#if pool.isWritable(b)}
                <span class="updown">
                  <button
                    type="button"
                    title={`raise priority: ${b.name}`}
                    aria-label={`raise priority: ${b.name}`}
                    aria-busy={pool.busyId === b.id ? 'true' : undefined}
                    disabled={pool.mutating && pool.busyId !== b.id}
                    onclick={() => {
                      if (!pool.mutating) void pool.reprioritize(b.id, 'up')
                    }}
                  >▲</button>
                  <button
                    type="button"
                    title={`lower priority: ${b.name}`}
                    aria-label={`lower priority: ${b.name}`}
                    aria-busy={pool.busyId === b.id ? 'true' : undefined}
                    disabled={pool.mutating && pool.busyId !== b.id}
                    onclick={() => {
                      if (!pool.mutating) void pool.reprioritize(b.id, 'down')
                    }}
                  >▼</button>
                </span>
              {/if}
            </div>
          </td>
          <td class="secret">
            {#if b.api_key_ref === ''}
              <span class="dim">—</span>
            {:else if knownSecrets.has(b.api_key_ref)}
              <span class="ok-ref" title="resolves to a vault secret">✓ {b.api_key_ref}</span>
            {:else}
              <span class="missing" title="no vault secret of this name — set it below">⚠ fehlt: {b.api_key_ref}</span>
            {/if}
          </td>
          <td class="state">
            <span class="dot {stateClass(b.effective_state)}"></span>
            {b.effective_state}{#if b.effective_state === 'cooldown'} · {b.cooldown_remaining_s}s{/if}
            {#if b.last_error}<span class="errcls" title="last error class">{b.last_error}</span>{/if}
          </td>
          <td class="act">
            <label class="switch" title="enabled">
              <input
                type="checkbox"
                checked={b.enabled}
                aria-label={`${b.name} enabled`}
                aria-busy={pool.busyId === b.id ? 'true' : undefined}
                disabled={pool.mutating && pool.busyId !== b.id}
                onchange={(e) => void toggleEnabled(b, e.currentTarget)}
              />
            </label>
            <button
              type="button"
              class="ghost"
              disabled={(pool.mutating && pool.busyId !== b.id) || isTesting(b.id)}
              onclick={() => void runTest(b.id, false)}
            >test</button>
            <button
              type="button"
              class="ghost"
              disabled={(pool.mutating && pool.busyId !== b.id) || isTesting(b.id)}
              onclick={() => void runTest(b.id, true)}
            >test+chat</button>
            <button type="button" class="ghost" onclick={() => openDialog('edit', b)}>edit</button>
            {#if confirmDeleteId === b.id}
              <span class="confirm-del">
                delete?
                <button type="button" class="danger" aria-busy={pool.busyId === b.id ? 'true' : undefined} onclick={() => void doDelete(b.id)}>yes</button>
                <button type="button" class="ghost" onclick={() => (confirmDeleteId = null)}>no</button>
              </span>
            {:else}
              <button
                type="button"
                class="ghost danger-text"
                disabled={pool.mutating && pool.busyId !== b.id}
                onclick={() => (confirmDeleteId = b.id)}
              >delete</button>
            {/if}
          </td>
        </tr>
        {#if testResults[b.id]}
          {@const t = testResults[b.id]}
          <tr class="test-row">
            <td colspan="7">
              <!-- inner flex wrapper: the td itself must stay display:table-cell
                   or the colspan collapses (a flex td leaves the table layout) -->
              <div class="t-wrap" role="status">
                {#if 'running' in t}
                  <span class="t-running">testing…</span>
                {:else if 'error' in t}
                  <span class="problem error">test failed: {t.error}</span>
                  <button type="button" class="dismiss" aria-label="dismiss test result" onclick={() => delete testResults[b.id]}>×</button>
                {:else}
                  <span class="t-verdict" class:bad={!t.reachable}>{t.reachable ? 'reachable' : 'unreachable'}</span>
                  <span class="t-lat">{t.latency_ms}ms</span>
                  {#each Object.entries(t.checks) as [k, v] (k)}
                    <span class="t-check" class:bad={v.startsWith('error')}>{k}: {v}</span>
                  {/each}
                  {#if t.openrouter}
                    {#if t.openrouter.credits_remaining !== undefined}<span class="t-check">credits: {t.openrouter.credits_remaining}</span>{/if}
                    {#if t.openrouter.zdr_endpoints !== undefined}<span class="t-check" class:bad={t.openrouter.zdr_endpoints === 0}>zdr endpoints: {t.openrouter.zdr_endpoints}</span>{/if}
                  {/if}
                  <button type="button" class="dismiss" aria-label="dismiss test result" onclick={() => delete testResults[b.id]}>×</button>
                {/if}
              </div>
            </td>
          </tr>
        {/if}
      </tbody>
    {/each}
  </Table>
</section>

{#if editing}
  <BackendDialog mode={editing.mode} backend={editing.backend} {profileOptions} {save} onclose={() => (editing = null)} />
{/if}

<style>
  .card {
    background: var(--surface-1);
    border: 1px solid var(--border);
    border-radius: var(--radius);
  }
  header {
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
  .new {
    margin-left: auto;
    font-size: var(--fs-sm);
    padding: var(--space-1) var(--space-2);
  }
  .empty {
    margin: 0;
    padding: var(--space-3);
    color: var(--text-faint);
    font-size: var(--fs-sm);
  }
  tr.off .name,
  tr.off .roles {
    opacity: 0.5;
  }
  /* An active disable-profile holds the backend out of every chain (§4.2) —
     dim its config row like a disabled one, but the profile chip carries the
     distinct reason. */
  tr.profile-off .name,
  tr.profile-off .roles {
    opacity: 0.5;
  }
  tr.dragging td {
    background: var(--surface-2);
  }
  .name {
    font-family: var(--font-mono);
    display: flex;
    flex-direction: column;
  }
  .memberships {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-1);
    margin-top: var(--space-1);
  }
  .pchip {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    padding: 0 var(--space-1);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    color: var(--text-faint);
  }
  .pchip-active {
    border-color: var(--warn);
    color: var(--warn);
  }
  .meta {
    color: var(--text-faint);
    font-size: var(--label-size);
  }
  .roles {
    color: var(--text-dim);
    font-size: var(--fs-xs);
  }
  .num {
    text-align: right;
    font-variant-numeric: tabular-nums;
  }
  .prio {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
  }
  .drag {
    background: transparent;
    border: none;
    color: var(--text-faint);
    cursor: grab;
    touch-action: none; /* the handle owns the pointer — no scroll-vs-drag race on touch */
    user-select: none;
    padding: var(--space-1);
    font-size: var(--fs-xs);
  }
  .drag:active {
    cursor: grabbing;
    color: var(--text-dim);
  }
  .drag.placeholder {
    cursor: default;
    color: var(--text-faint);
  }
  .prio-val.readonly {
    cursor: default;
    color: var(--text-dim);
  }
  .prio-val {
    background: transparent;
    border: none;
    color: var(--text);
    font-variant-numeric: tabular-nums;
    font-size: var(--fs-sm);
    cursor: pointer;
    padding: 0 var(--space-1);
  }
  .prio-val:hover {
    text-decoration: underline dotted;
  }
  .prio-input {
    width: 4.5rem;
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
    padding: 0 var(--space-1);
    background: var(--surface-0);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    color: var(--text);
  }
  .updown {
    display: inline-flex;
    flex-direction: column;
    /* stylelint-disable-next-line scale-unlimited/declaration-strict-value -- Micro-Spinner-Geometrie: die 0.7 zieht die gestapelten ▲▼-Glyphen dicht zusammen; keine Text-Zeilenhöhe, außerhalb der lh-Skala (design 05-§2.2 nennt 0.7 ×1 als Einzelfall). */
    line-height: 0.7;
  }
  .updown button {
    background: transparent;
    border: none;
    color: var(--text-dim);
    cursor: pointer;
    /* stylelint-disable-next-line scale-unlimited/declaration-strict-value -- Micro-Spinner-Glyph: 0.55rem ist der Skalen-Ausreißer (design 05-§4.3: 307 Cluster-rem + genau diese 0.55rem = 308); ein Snap auf --fs-2xs (0.72) würde die ▲▼-Pfeile sichtbar aufblähen. */
    font-size: 0.55rem;
    /* hit-area: the glyph stays micro, the padding earns a tappable target */
    padding: var(--space-1) var(--space-1);
  }
  .updown button:disabled {
    opacity: 0.4;
    cursor: not-allowed;
  }
  .updown button[aria-busy='true'] {
    opacity: 0.4;
    cursor: progress;
  }
  .badge {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    text-transform: uppercase;
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    padding: 0 var(--space-1);
    color: var(--text-dim);
    white-space: nowrap;
  }
  .secret {
    font-size: var(--fs-xs);
    font-family: var(--font-mono);
  }
  .ok-ref {
    color: var(--text-dim);
  }
  .missing {
    color: var(--danger);
  }
  .dim {
    color: var(--text-faint);
  }
  .state {
    white-space: nowrap;
  }
  .dot {
    display: inline-block;
    width: 0.55rem;
    height: 0.55rem;
    border-radius: 50%;
    margin-right: var(--space-1);
    vertical-align: middle;
  }
  .dot.ok {
    background: var(--ok);
  }
  .dot.warn {
    background: var(--warn);
  }
  .dot.idle {
    background: var(--text-faint);
  }
  .dot.profile {
    background: var(--warn);
  }
  .errcls {
    margin-left: var(--space-1);
    color: var(--danger);
    font-family: var(--font-mono);
    font-size: var(--label-size);
  }
  .act {
    white-space: nowrap;
    display: flex;
    align-items: center;
    gap: var(--space-1);
    flex-wrap: wrap;
  }
  .switch input {
    accent-color: var(--accent);
  }
  .ghost {
    font-size: var(--fs-2xs);
    padding: 0 var(--space-1);
    background: transparent;
  }
  .danger-text {
    color: var(--danger);
  }
  button.danger {
    border-color: var(--danger);
    color: var(--danger);
    font-size: var(--fs-2xs);
    padding: 0 var(--space-1);
  }
  .confirm-del {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
    font-size: var(--fs-xs);
    color: var(--danger);
  }
  .test-row td {
    background: var(--surface-0);
    font-size: var(--fs-xs);
  }
  .t-wrap {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-2);
    align-items: center;
  }
  .t-running {
    color: var(--text-dim);
    font-family: var(--font-mono);
  }
  .t-verdict {
    color: var(--ok);
    font-family: var(--font-mono);
  }
  .t-verdict.bad {
    color: var(--danger);
  }
  .t-lat {
    color: var(--text-dim);
    font-variant-numeric: tabular-nums;
  }
  .t-check {
    font-family: var(--font-mono);
    color: var(--text-dim);
  }
  .t-check.bad {
    color: var(--danger);
  }
  .dismiss {
    background: transparent;
    border: none;
    color: var(--text-dim);
    cursor: pointer;
    font-size: var(--fs-sm);
    margin-left: auto;
  }
  .problem {
    margin: var(--space-2) var(--space-3) 0;
    font-size: var(--fs-xs);
    padding: var(--space-1) var(--space-2);
    border-radius: var(--radius);
    border: 1px solid;
  }
  .problem.error {
    color: var(--danger);
    border-color: var(--danger);
    background: var(--danger-dim);
  }
  .alert {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }
  .problem.warn {
    color: var(--warn);
    border-color: var(--warn);
    background: transparent;
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  .problem p {
    margin: 0;
  }
  /* test-row's inline error chip: reset the block-level problem margin, it
     sits inside the flex wrapper, not above the table */
  .t-wrap .problem {
    margin: 0;
  }
</style>
