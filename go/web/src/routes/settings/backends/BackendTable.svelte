<script lang="ts">
  // Backend-pool table (design §3.5): the editable view of backend-list. priority
  // is reordered with up/down (no drag lib — 2 buttons, 0 deps); enabled toggles
  // inline; Test runs the reachability/chat probe; Edit opens the tuple dialog;
  // Delete confirms inline. The secret column joins api_key_ref against the known
  // secret names (a missing one is the derived "fehlt" status). Health/state is
  // read-only from the merged live status — mutation and status are separate
  // server paths. The scroll+table shell and cell chrome come from the shared
  // lib/ui/Table primitive (Q10); only the columns/rows/empty are ours.
  import { toApiError } from '../../../lib/api'
  import type { BackendListItem, BackendTestResult } from '../../../lib/api/types'
  import { backendDiff, draftFromBackend, isEmptySpec } from '../../../lib/backends'
  import Table from '../../../lib/ui/Table.svelte'
  import type { PoolModel } from './pool.svelte'
  import BackendDialog from './BackendDialog.svelte'

  let { pool, knownSecrets }: { pool: PoolModel; knownSecrets: Set<string> } = $props()

  let editing = $state<{ mode: 'create' | 'edit'; backend: BackendListItem | null } | null>(null)
  let confirmDeleteId = $state<string | null>(null)
  let notice = $state<string[]>([])
  type TestState = BackendTestResult | { error: string }
  let testResults = $state<Record<string, TestState>>({})

  // K-f-Klassen-Mapping (Q3): active ok, cooldown warn, sonst idle (fail-closed).
  function stateClass(s: string): string {
    if (s === 'active') return 'ok'
    if (s === 'cooldown') return 'warn'
    return 'idle'
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

  async function runTest(id: string, probeChat: boolean): Promise<void> {
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

</script>

<section class="card" aria-label="backend pool editor">
  <header>
    <h2>backend pool</h2>
    <span class="count">{pool.backends.length}</span>
    <button type="button" class="new" onclick={() => (editing = { mode: 'create', backend: null })}>+ new backend</button>
  </header>

  {#if pool.actionError}
    <p class="problem error" role="alert">{pool.actionError}</p>
  {/if}
  {#if notice.length > 0}
    <div class="problem warn" role="status">
      {#each notice as w (w)}<p>{w}</p>{/each}
      <button type="button" class="dismiss" onclick={() => (notice = [])}>dismiss</button>
    </div>
  {/if}

  <Table empty={pool.backends.length === 0}>
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
    {#each pool.sorted as b (b.id)}
      <tr class:off={!b.enabled}>
        <td class="name">
          {b.name}
          <span class="meta">{b.locality} · {b.protocol} · {b.base_url}</span>
        </td>
        <td><span class="badge">{b.trust}</span></td>
        <td class="roles">{b.roles.join(', ') || '—'}</td>
        <td class="num">
          <div class="prio">
            <span>{b.priority}</span>
            <span class="updown">
              <button type="button" title="raise priority" disabled={pool.busyId === b.id} onclick={() => void pool.reprioritize(b.id, 'up')}>▲</button>
              <button type="button" title="lower priority" disabled={pool.busyId === b.id} onclick={() => void pool.reprioritize(b.id, 'down')}>▼</button>
            </span>
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
              disabled={pool.busyId === b.id}
              onchange={(e) => void pool.setEnabled(b.id, e.currentTarget.checked)}
            />
          </label>
          <button type="button" class="ghost" disabled={pool.busyId === b.id} onclick={() => void runTest(b.id, false)}>test</button>
          <button type="button" class="ghost" disabled={pool.busyId === b.id} onclick={() => void runTest(b.id, true)}>test+chat</button>
          <button type="button" class="ghost" onclick={() => (editing = { mode: 'edit', backend: b })}>edit</button>
          {#if confirmDeleteId === b.id}
            <span class="confirm-del">
              delete?
              <button type="button" class="danger" onclick={() => void doDelete(b.id)}>yes</button>
              <button type="button" class="ghost" onclick={() => (confirmDeleteId = null)}>no</button>
            </span>
          {:else}
            <button type="button" class="ghost danger-text" onclick={() => (confirmDeleteId = b.id)}>delete</button>
          {/if}
        </td>
      </tr>
      {#if testResults[b.id]}
        {@const t = testResults[b.id]}
        <tr class="test-row">
          <td colspan="7">
            {#if 'error' in t}
              <span class="problem error">test failed: {t.error}</span>
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
              <button type="button" class="dismiss" onclick={() => delete testResults[b.id]}>×</button>
            {/if}
          </td>
        </tr>
      {/if}
    {/each}
  </Table>
</section>

{#if editing}
  <BackendDialog mode={editing.mode} backend={editing.backend} {save} onclose={() => (editing = null)} />
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
  .name {
    font-family: var(--font-mono);
    display: flex;
    flex-direction: column;
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
    padding: 0;
  }
  .updown button:disabled {
    opacity: 0.4;
    cursor: not-allowed;
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
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-2);
    align-items: center;
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
</style>
