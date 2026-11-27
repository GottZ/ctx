<script lang="ts">
  // One consumer's recordings /gateway/recordings/:consumer (issue #575):
  // model-recording-list, newest first, metadata and sizes only — never a
  // body. Filters: time range (typed in the viewer's zone, sent as
  // RFC 3339), operation, state, status class; Load more appends the next
  // keyset page. A filter that matches rarely stops the server's walk at its
  // scan bound: the page then says how many rows were searched and Continue
  // goes on from where the walk stopped. Each row leads to the recording.
  //
  // Server-admin only (guard: /gateway/recordings → viewTenants; the page
  // checks the tier again before its first call).
  import { tick, untrack } from 'svelte'
  import { route } from '../../router'
  import { session } from '../../lib/auth.svelte'
  import type { RecordingOpFilter, RecordingStateFilter, RecordingStatusFilter } from '../../lib/api/types'
  import Table from '../../lib/ui/Table.svelte'
  import { fmtBytes, fmtSecond } from './format'
  import { EMPTY_RECORDING_FILTER, RecordingConsumersModel, RecordingListModel, type RecordingFilter } from './recordings.svelte'

  const consumerId = $derived(route.params.consumer ?? '')
  const serverAdmin = $derived(session.caps.tier === 'server-admin')
  const model = new RecordingListModel()
  // the consumer's label for the heading (the list carries ids only)
  const consumers = new RecordingConsumersModel()

  const OPS: RecordingOpFilter[] = ['chat', 'embed', 'rerank', 'none']
  const STATES: RecordingStateFilter[] = ['open', 'done']
  const STATUSES: RecordingStatusFilter[] = ['2xx', '3xx', '4xx', '5xx', 'none']

  // The form's draft; applied on submit.
  let draft = $state<RecordingFilter>({ ...EMPTY_RECORDING_FILTER, ops: [] })

  $effect(() => {
    const id = consumerId
    if (!serverAdmin || id === '') return
    untrack(() => {
      draft = { ...EMPTY_RECORDING_FILTER, ops: [] }
      void model.load(id, draft)
      void consumers.load()
    })
  })

  const consumer = $derived(consumers.consumers.find((c) => c.consumer_id === consumerId) ?? null)

  function toggleOp(op: RecordingOpFilter, on: boolean): void {
    draft = { ...draft, ops: on ? [...draft.ops, op] : draft.ops.filter((o) => o !== op) }
  }

  function apply(e: SubmitEvent): void {
    e.preventDefault()
    void model.setFilter({ ...draft, ops: [...draft.ops] })
  }

  function reset(): void {
    draft = { ...EMPTY_RECORDING_FILTER, ops: [] }
    void model.setFilter({ ...draft, ops: [] })
  }

  // Focus stays sane: the button stays while more remain; when the last page
  // arrived it disappears, so focus moves to the first appended row's link.
  let cardEl: HTMLElement | undefined = $state()
  async function more(): Promise<void> {
    const before = model.recordings.length
    await model.loadMore()
    if (model.nextCursor !== null) return
    await tick()
    const links = cardEl?.querySelectorAll<HTMLAnchorElement>('tr[data-recording-row] a')
    links?.[Math.min(before, links.length - 1)]?.focus()
  }

  function statusText(status: number | null, code: string | null): string {
    const s = status === null ? 'none' : String(status)
    return code === null ? s : `${s} ${code}`
  }

  function streamText(stream: boolean | null): string {
    return stream === null ? '—' : stream ? 'stream' : 'whole'
  }
</script>

<section class="area" data-recordings-consumer={consumerId}>
  <header>
    <p class="back"><a href="/gateway/recordings">← Recordings</a></p>
    <h1>{consumer?.label ?? 'Recordings of a consumer'}</h1>
    <p class="sub">
      {#if consumer?.tenant_slug}<span class="chip">{consumer.tenant_slug}</span>{/if}
      <code>{consumerId}</code> · newest first · metadata and sizes only
    </p>
  </header>

  {#if session.caps.tier === 'loading'}
    <p class="state" aria-busy="true">resolving session…</p>
  {:else if !serverAdmin}
    <p class="banner" role="alert">Recordings are for server-admins only — they are hidden for this key.</p>
  {:else}
    <form class="toolbar" aria-label="filter recordings" onsubmit={apply}>
      <!-- plain text in a fixed form, not datetime-local: that control is a
           row of tab stops of its own in Chromium, which the contract focus
           walk (one stop per element) cannot pass -->
      <div class="field">
        <label for="rec-since">from (local time)</label>
        <input
          id="rec-since"
          type="text"
          placeholder="YYYY-MM-DD HH:MM"
          autocomplete="off"
          spellcheck="false"
          aria-describedby="rec-time-hint"
          bind:value={draft.since}
        />
      </div>
      <div class="field">
        <label for="rec-until">until (local time)</label>
        <input
          id="rec-until"
          type="text"
          placeholder="YYYY-MM-DD HH:MM"
          autocomplete="off"
          spellcheck="false"
          aria-describedby="rec-time-hint"
          bind:value={draft.until}
        />
      </div>
      <p id="rec-time-hint" class="hint">YYYY-MM-DD, optionally with HH:MM or HH:MM:SS, in this browser's time zone; until is exclusive.</p>
      <fieldset class="ops">
        <legend>operation</legend>
        {#each OPS as op (op)}
          <label class="chk">
            <input type="checkbox" checked={draft.ops.includes(op)} onchange={(e) => toggleOp(op, e.currentTarget.checked)} />
            {op}
          </label>
        {/each}
      </fieldset>
      <div class="field">
        <label for="rec-state">state</label>
        <select id="rec-state" bind:value={draft.state}>
          <option value="">any</option>
          {#each STATES as s (s)}<option value={s}>{s}</option>{/each}
        </select>
      </div>
      <div class="field">
        <label for="rec-status">status</label>
        <select id="rec-status" bind:value={draft.status}>
          <option value="">any</option>
          {#each STATUSES as s (s)}<option value={s}>{s}</option>{/each}
        </select>
      </div>
      <div class="buttons">
        <button type="submit">Apply</button>
        <button type="button" onclick={reset}>Reset</button>
      </div>
      {#if model.filterError !== null}
        <p class="field-error" role="alert">{model.filterError}</p>
      {/if}
    </form>

    {#if model.status === 'idle' || model.status === 'loading'}
      <p class="state" aria-busy="true">loading recordings…</p>
    {:else if model.status === 'error'}
      <div class="error" role="alert">
        <p>{model.loadError?.message ?? 'failed to load the recordings'}</p>
        {#if model.loadError?.requestId}<p class="request-id">request {model.loadError.requestId}</p>{/if}
        <button type="button" onclick={() => void model.setFilter(model.filter)}>Retry</button>
      </div>
    {:else}
      <section class="card" aria-label="recordings" bind:this={cardEl}>
        <header class="card-head">
          <h2>recordings</h2>
          <span class="count">{model.recordings.length}{model.nextCursor !== null ? '+' : ''}</span>
        </header>
        {#if model.recordings.length === 0 && !model.scanLimitReached}
          <p class="empty" role="status">No recording matches — widen the filter or reset it.</p>
        {:else if model.recordings.length > 0}
          <Table label="recordings">
            {#snippet head()}
              <tr>
                <th>time</th>
                <th>op</th>
                <th>model</th>
                <th>stream</th>
                <th>state</th>
                <th>status</th>
                <th>request</th>
                <th>response</th>
              </tr>
            {/snippet}
            {#each model.recordings as rec (rec.id)}
              <tr data-recording-row={rec.id}>
                <td class="when"><a href={`/gateway/recordings/${consumerId}/${rec.id}`}>{fmtSecond(rec.created_at)}</a></td>
                <td>{rec.op ?? 'none'}</td>
                <td>{rec.model ?? '—'}</td>
                <td>{streamText(rec.stream)}</td>
                <td><span class="tag" class:open={rec.state === 'open'}>{rec.state}</span></td>
                <td><code>{statusText(rec.status, rec.code)}</code></td>
                <td class="num">
                  {fmtBytes(rec.request_bytes)}{#if rec.request_cleared}{' '}<span class="mark" data-cleared="request">cleared</span>{/if}{#if !rec.request_complete}{' '}<span class="mark">incomplete</span>{/if}
                </td>
                <td class="num">
                  {rec.response_null && rec.response_bytes === 0 ? '—' : fmtBytes(rec.response_bytes)}{#if rec.response_null && rec.response_bytes > 0}{' '}<span class="mark" data-cleared="response">cleared</span>{/if}
                </td>
              </tr>
            {/each}
          </Table>
        {/if}
        {#if model.scanLimitReached && model.nextCursor !== null}
          <p class="scan" role="status" data-scan-limit>
            Searched {model.scanned} rows so far, {model.recordings.length} matched — the server stops each walk at its
            bound. Continue searching older rows?
          </p>
        {/if}
        {#if model.nextCursor !== null}
          <div class="more">
            <button type="button" disabled={model.loadingMore} onclick={() => void more()}>
              {model.loadingMore ? 'loading…' : model.scanLimitReached ? 'Continue' : 'Load more'}
            </button>
          </div>
        {/if}
      </section>
      {#if model.moreError}
        <p class="action-error" role="alert">{model.moreError.message}</p>
      {/if}
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
  .card header.card-head {
    padding-bottom: var(--space-2);
  }
  h1 {
    margin: 0;
    font-size: var(--fs-xl);
    font-weight: var(--fw-semibold);
    letter-spacing: var(--track-1);
    overflow-wrap: anywhere;
  }
  .back {
    margin: 0 0 var(--space-1);
    font-size: var(--fs-sm);
  }
  .sub {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-2);
    align-items: center;
    margin: var(--space-1) 0 0;
    color: var(--text-dim);
    font-size: var(--fs-sm);
  }
  code {
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
  }
  .sub code {
    overflow-wrap: anywhere;
  }
  /* a status code stays on one line; the table scrolls sideways instead */
  td code {
    white-space: nowrap;
  }
  .chip,
  .tag {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    padding: 0 var(--space-1);
  }
  .tag.open {
    border-style: dashed;
    border-color: var(--warn);
  }
  .toolbar {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-2) var(--space-3);
    align-items: flex-end;
  }
  .field {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    font-size: var(--fs-sm);
    min-width: 0;
  }
  .field label,
  legend {
    color: var(--text-dim);
    font-size: var(--fs-xs);
  }
  .field input,
  .field select {
    max-width: 100%;
  }
  .hint {
    flex-basis: 100%;
    order: 1;
    margin: 0;
    color: var(--text-dim);
    font-size: var(--fs-xs);
  }
  .ops {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-2);
    margin: 0;
    border: 0;
    padding: 0;
    font-size: var(--fs-sm);
  }
  .ops legend {
    padding: 0;
    margin-bottom: var(--space-1);
  }
  .chk {
    display: inline-flex;
    gap: var(--space-1);
    align-items: center;
  }
  .buttons {
    display: flex;
    gap: var(--space-2);
  }
  .field-error,
  .action-error {
    flex-basis: 100%;
    margin: 0;
    color: var(--text);
    font-size: var(--fs-sm);
    border-left: 2px solid var(--danger);
    padding-left: var(--space-2);
  }
  .state,
  .empty {
    margin: 0;
    padding: var(--space-2) var(--space-3);
    color: var(--text-faint);
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
  }
  .banner,
  .error {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    align-items: flex-start;
    margin: 0;
    border: 1px solid var(--danger);
    border-radius: var(--radius);
    background: var(--danger-dim);
    padding: var(--space-2) var(--space-3);
    font-size: var(--fs-sm);
    color: var(--text);
  }
  .error p {
    margin: 0;
    color: var(--text);
  }
  .request-id {
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
  }
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
  .when {
    white-space: nowrap;
    font-size: var(--fs-xs);
  }
  .num {
    font-family: var(--font-mono);
    white-space: nowrap;
  }
  .mark {
    color: var(--text-dim);
    font-size: var(--fs-xs);
  }
  .scan {
    margin: 0;
    padding: var(--space-2) var(--space-3);
    color: var(--text-dim);
    font-size: var(--fs-sm);
  }
  .more {
    padding: var(--space-2) var(--space-3);
  }
</style>
