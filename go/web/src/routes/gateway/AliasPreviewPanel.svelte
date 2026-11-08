<script lang="ts">
  // Chain preview of one alias (design/05 §3.3 "Vorschau"; wave W05-13 — RC-D
  // C5; server-admin only — the page never mounts it below that tier, and the
  // server 403s model-alias-preview anyway). C7 contract: the /v1 chain is a
  // function of the CONSUMER (tenant gate, egress policy, lane, floor, tenant
  // overlay), so the request is the alias by NAME through one picked consumer
  // (AliasesModel.loadPreview) — nothing is sent before a consumer is picked
  // and "Run preview" is pressed. The consumer list (model-consumer-list,
  // every tenant for a server-admin) loads lazily when the panel opens, one
  // keyset page at a time. External rows of the chain are marked; the rows
  // the build dropped are listed with stage and reason.
  import { onMount } from 'svelte'
  import { toApiError } from '../../lib/api'
  import { listConsumers } from '../../lib/api/modelapi'
  import type { ConsumerView, ModelAliasView } from '../../lib/api/types'
  import type { AliasesModel } from './aliases.svelte'

  let { model, row, onclose }: { model: AliasesModel; row: ModelAliasView; onclose: () => void } = $props()

  let consumers = $state<ConsumerView[]>([])
  let cursor = $state<string | null>(null)
  let status = $state<'loading' | 'ready' | 'error'>('loading')
  let loadError = $state<string | null>(null)
  let loadingMore = $state(false)
  let picked = $state('')
  let running = $state(false)

  let headingEl = $state<HTMLHeadingElement>()
  const uid = `pv-${Math.random().toString(36).slice(2, 9)}`

  const answer = $derived(model.preview[row.id] ?? null)
  const failure = $derived(model.previewError[row.id] ?? null)
  const through = $derived(answer ? consumers.find((c) => c.id === answer.consumer_id) : undefined)

  async function load(more: boolean): Promise<void> {
    if (more) loadingMore = true
    try {
      const res = await listConsumers(more && cursor !== null ? { cursor } : {})
      consumers = more ? [...consumers, ...res.consumers] : res.consumers
      cursor = res.next_cursor ?? null
      loadError = null
      status = 'ready'
    } catch (err) {
      loadError = toApiError(err).message
      if (!more) status = 'error'
    } finally {
      loadingMore = false
    }
  }

  onMount(() => {
    headingEl?.focus()
    void load(false)
  })

  async function run(e: SubmitEvent): Promise<void> {
    e.preventDefault()
    if (picked === '' || running) return
    running = true
    try {
      await model.loadPreview(row, picked)
    } finally {
      running = false
    }
  }
</script>

<section class="panel" data-testid="alias-preview" aria-labelledby={`${uid}-title`}>
  <header>
    <h2 id={`${uid}-title`} tabindex="-1" bind:this={headingEl}>
      Chain preview of <code>{row.name}</code> <span class="scope">({row.scope})</span>
    </h2>
    <button type="button" class="x" aria-label="close the chain preview" onclick={onclose}>×</button>
  </header>
  <p class="hint">
    The /v1 chain this alias name resolves to for one consumer — its tenant, egress policy, lane and floor decide.
    Built without contacting a backend.
  </p>

  <form class="pick" onsubmit={run}>
    <div class="field">
      <label for={`${uid}-consumer`}>consumer</label>
      <select id={`${uid}-consumer`} bind:value={picked} disabled={status !== 'ready'}>
        <option value="">{status === 'loading' ? 'loading consumers…' : 'pick a consumer'}</option>
        {#each consumers as c (c.id)}
          <option value={c.id}>{c.label} ({c.tenant_slug})</option>
        {/each}
      </select>
    </div>
    <button type="submit" disabled={picked === '' || running}>{running ? 'running…' : 'Run preview'}</button>
    {#if cursor !== null}
      <button type="button" class="ghost" disabled={loadingMore} onclick={() => void load(true)}>
        {loadingMore ? 'loading…' : 'More consumers'}
      </button>
    {/if}
  </form>
  {#if loadError}
    <p class="problem" role="alert">consumer list unavailable: {loadError}</p>
  {/if}
  {#if failure}
    <p class="problem" role="alert">preview failed: {failure}</p>
  {/if}

  {#if answer}
    <dl class="facts" aria-live="polite">
      <div><dt>consumer</dt><dd>{through ? `${through.label} (${through.tenant_slug})` : answer.consumer_id}</dd></div>
      <div>
        <dt>resolved alias</dt>
        <dd>{answer.alias ? `${answer.alias.name} (${answer.alias.scope})` : 'not resolved'}</dd>
      </div>
      <div><dt>required sensitivity</dt><dd>{answer.required_sensitivity || '—'}</dd></div>
      <div><dt>lane</dt><dd>{answer.lane || '—'}</dd></div>
    </dl>

    <h3>chain</h3>
    {#if answer.chain.length === 0}
      <p class="hint">no backend in the chain.</p>
    {:else}
      <ol class="chain" data-testid="preview-chain">
        {#each answer.chain as r, i (`${i}-${r.backend}`)}
          <li data-locality={r.locality} class:external={r.locality === 'external'}>
            <span class="backend">{r.backend}</span>
            <span class="chip" class:ext={r.locality === 'external'}>{r.locality}</span>
            <span class="chip">{r.provider_class}</span>
            <span class="chip">{r.billing}</span>
            <span class="chip">trust {r.trust}</span>
          </li>
        {/each}
      </ol>
    {/if}

    <h3>excluded</h3>
    {#if answer.excluded.length === 0}
      <p class="hint">no backend was dropped.</p>
    {:else}
      <ul class="excluded" data-testid="preview-excluded">
        {#each answer.excluded as x, i (`${i}-${x.backend}`)}
          <li>
            <span class="backend">{x.backend}</span>
            <span class="chip">{x.stage}</span>
            <span class="reason">{x.reason}</span>
          </li>
        {/each}
      </ul>
    {/if}

    {#if answer.notes.length > 0}
      <h3>notes</h3>
      <ul class="notes">
        {#each answer.notes as n (n)}
          <li><code>{n}</code></li>
        {/each}
      </ul>
    {/if}
  {/if}
</section>

<style>
  .panel {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    padding: var(--space-2) var(--space-3) var(--space-3);
    background: var(--surface-1);
    border: 1px solid var(--border);
    border-radius: var(--radius);
  }
  header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-2);
  }
  h2 {
    margin: 0;
    font-family: var(--font-mono);
    font-size: var(--fs-md);
    font-weight: var(--fw-semibold);
  }
  h3 {
    margin: var(--space-1) 0 0;
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    color: var(--text-dim);
  }
  .scope {
    color: var(--text-dim);
  }
  code {
    font-family: var(--font-mono);
  }
  .x {
    min-height: 24px;
    min-width: 24px;
    background: transparent;
    border: none;
    color: var(--text-dim);
    font-size: var(--fs-xl);
    line-height: var(--lh-solid);
    cursor: pointer;
    font-family: inherit;
  }
  .hint {
    margin: 0;
    font-size: var(--fs-xs);
    color: var(--text-dim);
  }
  .pick {
    display: flex;
    align-items: flex-end;
    flex-wrap: wrap;
    gap: var(--space-2);
  }
  .field {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    min-width: 0;
  }
  label {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    color: var(--text-dim);
  }
  select,
  .pick button {
    min-height: 24px;
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
    padding: var(--space-1) var(--space-2);
    background: var(--surface-2);
    color: var(--text);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
  }
  select {
    max-width: 100%;
  }
  .pick button {
    font-family: var(--font-ui);
    cursor: pointer;
  }
  .pick button.ghost {
    background: transparent;
  }
  .pick button:hover:not(:disabled) {
    border-color: var(--accent);
    color: var(--accent);
  }
  .pick button:disabled {
    opacity: 0.55;
    cursor: not-allowed;
  }
  .problem {
    margin: 0;
    font-size: var(--fs-xs);
    padding: var(--space-1) var(--space-2);
    border: 1px solid var(--danger);
    border-radius: var(--radius);
    color: var(--text);
    background: var(--danger-dim);
  }
  .facts {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-1) var(--space-4);
    margin: 0;
  }
  .facts div {
    display: flex;
    gap: var(--space-1);
    align-items: baseline;
  }
  dt {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    color: var(--text-dim);
  }
  dd {
    margin: 0;
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
    color: var(--text);
  }
  ol,
  ul {
    margin: 0;
    padding-left: var(--space-4);
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    font-size: var(--fs-sm);
  }
  li {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-1) var(--space-2);
  }
  ol li {
    display: list-item;
  }
  ol li > span {
    margin-right: var(--space-2);
  }
  li.external .backend {
    color: var(--warn);
  }
  .backend {
    font-family: var(--font-mono);
    font-weight: var(--fw-semibold);
  }
  .chip {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    color: var(--text-dim);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    padding: 0 var(--space-1);
    white-space: nowrap;
  }
  .chip.ext {
    color: var(--warn);
    border-color: var(--warn);
  }
  .reason {
    color: var(--text-dim);
  }
</style>
