<script lang="ts">
  // One recording /gateway/recordings/:consumer/:id (issue #575):
  // model-recording-get — the metadata, the state of both sides (request:
  // present / cleared / empty; response: present / cleared / none / pending,
  // the stream's line counts), a raw view of each whole body, and the blocks
  // the server cut the recording into, in order, grouped Request / Response
  // (RecordingBlock: the original as text, more on demand, raw bytes, and a
  // translation by a model of this instance next to it).
  //
  // Every content read is audited by the server before it answers (a read it
  // cannot audit is 503 audit_unavailable and shows nothing). Leaving the page
  // aborts every running read and translation (onDestroy → dispose). Nothing
  // is kept beyond this component's state.
  //
  // Server-admin only (guard: /gateway/recordings → viewTenants; the page
  // checks the tier again before its first call).
  import { onDestroy, tick, untrack } from 'svelte'
  import { route } from '../../router'
  import { session } from '../../lib/auth.svelte'
  import type { RecordingSide } from '../../lib/api/types'
  import { RECORDING_LANGUAGES } from '../../lib/api/recordings'
  import { fmtBytes, fmtSecond } from './format'
  import RecordingBlock from './RecordingBlock.svelte'
  import RecordingBytes from './RecordingBytes.svelte'
  import { RecordingDetailModel } from './recordings.svelte'

  const consumerId = $derived(route.params.consumer ?? '')
  const recordingId = $derived(route.params.id ?? '')
  const serverAdmin = $derived(session.caps.tier === 'server-admin')
  const model = new RecordingDetailModel()

  $effect(() => {
    const id = recordingId
    if (!serverAdmin || id === '') return
    untrack(() => void model.load(id))
  })

  onDestroy(() => model.dispose())

  // Blocks come a page (≤ 500) at a time; the button sits in the card whose
  // blocks the loaded page ends with (request blocks come first).
  const cutSide = $derived(model.views.at(-1)?.block.side ?? 'request')

  // Focus stays sane: after a page arrived, focus moves to the first control
  // of its first block (the button may have moved to the other card or gone).
  async function moreBlocks(): Promise<void> {
    const before = model.views.length
    await model.loadMoreBlocks()
    const first = model.views[before]
    if (first === undefined) return
    await tick()
    const el = document.querySelector(`[data-block="${CSS.escape(first.block.address)}"]`)
    el?.querySelector<HTMLElement>('button, [tabindex="0"]')?.focus()
  }

  const LANGS = Object.entries(RECORDING_LANGUAGES)

  const rec = $derived(model.recording)
  const backTo = $derived(`/gateway/recordings/${rec?.consumer_id ?? consumerId}`)

  // which whole body shows its raw bytes
  let rawSide = $state<{ request: boolean; response: boolean }>({ request: false, response: false })

  function toggleSide(address: 'request' | 'response'): void {
    const on = !rawSide[address]
    rawSide = { ...rawSide, [address]: on }
    const r = model.sideReader(address)
    if (on && r.loaded === 0) void r.more()
  }

  function sideText(s: RecordingSide): string {
    const parts = [s.state]
    if (s.format !== undefined) parts.push(s.format)
    parts.push(fmtBytes(s.bytes))
    if (s.stream_lines !== undefined) parts.push(`${s.stream_lines} stream lines`)
    if (s.stream_lines_unparsed !== undefined) parts.push(`${s.stream_lines_unparsed} unparsed`)
    return parts.join(' · ')
  }

  function orDash(v: string | number | null | undefined): string {
    return v === null || v === undefined || v === '' ? '—' : String(v)
  }
</script>

<section class="area" data-recording-id={recordingId}>
  <header>
    <p class="back"><a href={backTo}>← Recordings of this consumer</a></p>
    <h1>Recording <code>{recordingId.slice(-12)}</code></h1>
    {#if rec !== null}
      <p class="sub">
        <span class="tag" class:open={rec.state === 'open'}>{rec.state}</span>
        <span>{rec.op ?? 'no op'}</span>
        <span>{rec.model ?? 'no model'}</span>
        <span>{fmtSecond(rec.created_at)}</span>
      </p>
    {/if}
  </header>

  {#if session.caps.tier === 'loading'}
    <p class="state" aria-busy="true">resolving session…</p>
  {:else if !serverAdmin}
    <p class="banner" role="alert">Recordings are for server-admins only — they are hidden for this key.</p>
  {:else if model.status === 'idle' || model.status === 'loading'}
    <p class="state" aria-busy="true">loading recording…</p>
  {:else if model.status === 'error'}
    <div class="error" role="alert" data-error-code={model.error?.code}>
      <p>{model.error?.text ?? 'failed to load the recording'}</p>
      {#if model.notFound}
        <p><a href={backTo}>Back to the list</a></p>
      {:else}
        {#if model.error?.requestId}<p class="request-id">request {model.error.requestId}</p>{/if}
        <button type="button" onclick={() => void model.load(recordingId)}>Retry</button>
      {/if}
    </div>
  {:else if rec !== null && model.request !== null && model.response !== null}
    <section class="card" aria-label="metadata">
      <header class="card-head"><h2>metadata</h2></header>
      <dl class="meta">
        <dt>path</dt>
        <dd><code>{rec.path}</code></dd>
        <dt>operation</dt>
        <dd>{rec.op ?? 'none (no route parsed the request)'}</dd>
        <dt>model</dt>
        <dd>{orDash(rec.model)}</dd>
        <dt>stream</dt>
        <dd>{rec.stream === null ? '—' : rec.stream ? 'yes' : 'no'}</dd>
        <dt>state</dt>
        <dd>{rec.state}</dd>
        <dt>status</dt>
        <dd><code>{orDash(rec.status)}{rec.code !== null ? ` ${rec.code}` : ''}</code></dd>
        <dt>request</dt>
        <dd>
          {fmtBytes(rec.request_bytes)}{rec.declared_bytes !== null ? ` (declared ${fmtBytes(rec.declared_bytes)})` : ''} ·
          {rec.request_complete ? 'read completely' : 'read incompletely'}
        </dd>
        <dt>response</dt>
        <dd>{fmtBytes(rec.response_bytes)}</dd>
        <dt>process</dt>
        <dd><code>{rec.process_id}</code></dd>
        <dt>client request id</dt>
        <dd><code>{orDash(rec.client_request_id)}</code></dd>
        <dt>created</dt>
        <dd>{fmtSecond(rec.created_at)}</dd>
        <dt>finished</dt>
        <dd>{rec.finished_at === null ? 'not yet (open)' : fmtSecond(rec.finished_at)}</dd>
      </dl>
    </section>

    {#if model.views.some((v) => v.translation !== null)}
      <div class="translate-bar">
        <label for="rec-lang">Translate into</label>
        <select id="rec-lang" bind:value={model.language}>
          {#each LANGS as [code, name] (code)}
            <option value={code}>{name} ({code})</option>
          {/each}
        </select>
        <span class="hint">
          A model of this instance (local backends only) translates a block and lays it out readably; the original stays
          the reference. Nothing is stored.
        </span>
      </div>
    {/if}

    {#each [{ side: model.request, blocks: model.requestBlocks, name: 'Request' }, { side: model.response, blocks: model.responseBlocks, name: 'Response' }] as g (g.name)}
      {@const address = g.side.address}
      <section class="card" aria-label={g.name.toLowerCase()} data-side={address}>
        <header class="card-head">
          <h2>{g.name}</h2>
          <span class="count" data-side-state>{sideText(g.side)}</span>
          {#if g.side.state === 'present' && g.side.bytes > 0}
            <button type="button" class="side-raw" aria-pressed={rawSide[address]} onclick={() => toggleSide(address)}>Raw body</button>
          {/if}
        </header>
        {#if rawSide[address]}
          <div class="side-bytes">
            <RecordingBytes reader={model.sideReader(address)} label={`${address} body`} size={g.side.bytes} />
          </div>
        {/if}
        {#if g.side.state === 'cleared'}
          <p class="note">The retention cleared this body; its size stays ({fmtBytes(g.side.bytes)}).</p>
        {:else if g.side.state === 'pending'}
          <p class="note">The request is still running — no answer is recorded yet.</p>
        {:else if g.side.state === 'none'}
          <p class="note">No answer was recorded.</p>
        {:else if g.side.state === 'empty'}
          <p class="note">Nothing was read.</p>
        {/if}
        {#each g.blocks as view (view.block.address)}
          <RecordingBlock model={view} language={model.language} />
        {/each}
        <!-- the next page continues where the loaded blocks end: in this card -->
        {#if model.hasMoreBlocks && cutSide === address}
          <div class="more-blocks" data-more-blocks>
            <span role="status">showing {model.views.length} of {model.blocksTotal} blocks</span>
            <button type="button" disabled={model.loadingMoreBlocks} onclick={() => void moreBlocks()}>
              {model.loadingMoreBlocks ? 'loading…' : 'Load more blocks'}
            </button>
            {#if model.moreBlocksError !== null}
              <p class="err" role="alert" data-error-code={model.moreBlocksError.code}>{model.moreBlocksError.text}</p>
            {/if}
          </div>
        {/if}
      </section>
    {/each}
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
  }
  h1 code {
    font-size: var(--fs-lg);
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
    overflow-wrap: anywhere;
  }
  .tag {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    padding: 0 var(--space-1);
    color: var(--text);
  }
  .tag.open {
    border-style: dashed;
    border-color: var(--warn);
  }
  .state {
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
    flex-wrap: wrap;
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
  .side-raw {
    margin-left: auto;
  }
  .side-bytes {
    padding: var(--space-2) var(--space-3);
  }
  .meta {
    display: grid;
    grid-template-columns: max-content minmax(0, 1fr);
    gap: var(--space-1) var(--space-3);
    margin: 0;
    padding: var(--space-2) var(--space-3);
    font-size: var(--fs-sm);
  }
  .meta dt {
    color: var(--text-dim);
  }
  .meta dd {
    margin: 0;
    overflow-wrap: anywhere;
  }
  .note {
    margin: 0;
    padding: var(--space-2) var(--space-3);
    color: var(--text-dim);
    font-size: var(--fs-sm);
  }
  .more-blocks {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-2);
    align-items: center;
    padding: var(--space-2) var(--space-3);
    border-top: 1px solid var(--border);
    color: var(--text-dim);
    font-size: var(--fs-sm);
  }
  .err {
    flex-basis: 100%;
    margin: 0;
    color: var(--text);
    border-left: 2px solid var(--danger);
    padding-left: var(--space-2);
  }
  .translate-bar {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-2);
    align-items: center;
    font-size: var(--fs-sm);
  }
  .translate-bar select {
    max-width: 100%;
  }
  .hint {
    flex-basis: 100%;
    color: var(--text-dim);
    font-size: var(--fs-xs);
  }
</style>
