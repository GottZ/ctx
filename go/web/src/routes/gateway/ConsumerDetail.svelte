<script lang="ts">
  // Consumer detail page /gateway/consumers/:id (RC-D C6, wave W05-09;
  // design/05 §4.6): head (label, kind badge, server status), policy, the key
  // chain (chain.ts keyChain — rotated_from links), UsagePanel (W05-10),
  // LimitsCard (W05-12), the last calls (calls.ts callRows — body-free, a
  // detail control for server-admin ONLY: below it /api/llmlog/{id} answers a
  // uniform 404 for consumer rows) and the audit trail as a chain (chain.ts
  // auditEntries). State: ConsumerDetailModel (consumer.svelte.ts). A foreign
  // or unknown id renders the server's No-Oracle text — the same for both.
  // The route inherits the /gateway guard (viewModelApi); the load effect is
  // an extra self-gate, like GatewayPage.
  import { untrack } from 'svelte'
  import { route } from '../../router'
  import { session } from '../../lib/auth.svelte'
  import Table from '../../lib/ui/Table.svelte'
  import { LlmlogDetailModel } from '../status/llmlog-detail.svelte'
  import { ConsumerDetailModel } from './consumer.svelte'
  import { auditEntries, keyChain } from './chain'
  import { callRows } from './calls'
  import { STATUS_LABEL, consumerTone, fmtDay, fmtMinute, keyTone } from './format'
  import LimitsCard from './LimitsCard.svelte'
  import UsagePanel from './UsagePanel.svelte'

  const consumerId = $derived(route.params.id ?? '')
  const model = new ConsumerDetailModel()
  // Server-admin only: the body detail of one call (never instantiated into a
  // fetch for anyone else — the rows carry no detail id below server-admin).
  const detail = new LlmlogDetailModel()

  $effect(() => {
    const id = consumerId
    if (!session.caps.viewModelApi || id === '') return
    untrack(() => void model.load(id))
  })

  const c = $derived(model.consumer)
  const chain = $derived(c === null ? [] : keyChain(c.keys))
  const hintOf = $derived(new Map((c?.keys ?? []).map((k) => [k.id, k.hint])))
  const trail = $derived(c === null ? [] : auditEntries(model.audit, c.keys))
  const rows = $derived(callRows(model.calls, session.caps.tier))
  const serverAdmin = $derived(session.caps.tier === 'server-admin')

  function modelsText(): string {
    if (c === null) return ''
    if (c.all_models) return 'all offered models, incl. future ones'
    return c.models.length === 0 ? 'none' : `${c.models.length} granted`
  }
</script>

<section class="area" data-consumer-id={consumerId}>
  <header>
    <p class="back"><a href="/gateway">← Model API</a></p>
    <h1>{c?.label ?? 'Model consumer'}</h1>
    {#if c !== null}
      <p class="sub">
        <span class="badge" class:peer={c.kind === 'ctx-peer'}>{c.kind}</span>
        <span class="status {consumerTone(c.status)}" data-testid="consumer-status"
          >{STATUS_LABEL[c.status] ?? c.status}</span
        >
        {#if serverAdmin}<span class="chip">{c.tenant_slug}</span>{/if}
      </p>
    {/if}
  </header>

  {#if model.status === 'idle' || model.status === 'loading'}
    <p class="state" aria-busy="true">loading consumer…</p>
  {:else if model.status === 'error'}
    <div class="error" role="alert">
      <p>{model.loadError?.message ?? 'failed to load the consumer'}</p>
      {#if model.notFound}
        <p><a href="/gateway">Back to the consumer list</a></p>
      {:else}
        {#if model.loadError?.requestId}<p class="request-id">request {model.loadError.requestId}</p>{/if}
        <button type="button" onclick={() => void model.reload()}>Retry</button>
      {/if}
    </div>
  {:else if c !== null}
    <section class="card" aria-label="policy">
      <header class="card-head"><h2>policy</h2></header>
      <dl class="policy">
        <dt>description</dt>
        <dd>{c.description === '' ? '—' : c.description}</dd>
        <dt>scope</dt>
        <dd>{c.tenant_scope ?? 'global models only'}</dd>
        <dt>models</dt>
        <dd>{modelsText()}</dd>
        <dt>prompt sensitivity</dt>
        <dd>{c.prompt_sensitivity}</dd>
        <dt>lane</dt>
        <dd>{c.admission_lane}</dd>
        <dt>network</dt>
        <dd>{c.allowed_cidrs === null ? 'any source' : c.allowed_cidrs.join(', ')}</dd>
        <dt>last used</dt>
        <dd>{c.last_used_at === null ? 'never' : fmtMinute(c.last_used_at)}</dd>
      </dl>
    </section>

    <section class="card" aria-label="credentials">
      <header class="card-head">
        <h2>key chain</h2>
        <span class="count">{c.keys_total}</span>
      </header>
      {#if chain.length === 0}
        <p class="empty">no key</p>
      {:else}
        <ol class="chain" aria-label="key chain">
          {#each chain as link (link.key.id)}
            <li data-key-link>
              <code data-key-hint>{link.key.hint}</code>
              <span class="status {keyTone(link.key.status)}">{link.key.status}</span>
              <span class="when">created {fmtDay(link.key.created_at)}</span>
              {#if link.key.expires_at !== null}<span class="when">expires {fmtDay(link.key.expires_at)}</span>{/if}
              <span class="from">
                {#if link.predecessorId !== null}rotated from{/if}
                <code data-rotated-from
                  >{link.predecessorId === null || link.truncated ? '' : (hintOf.get(link.predecessorId) ?? '')}</code
                >
                {#if link.truncated}<span class="cut" data-chain-cut>earlier key not loaded</span>{/if}
              </span>
            </li>
          {/each}
        </ol>
        {#if model.keysTruncated}
          <p class="note" role="status">The chain is cut at the server's cap — older keys are not shown.</p>
        {/if}
      {/if}
    </section>

    <UsagePanel consumerId={c.id} />
    <LimitsCard consumerId={c.id} />

    <section class="card" aria-label="calls">
      <header class="card-head">
        <h2>last calls</h2>
        <span class="count">{rows.length}</span>
      </header>
      {#if model.callsStatus === 'error'}
        <p class="note warn" role="status">calls unavailable: {model.callsError?.message ?? 'error'}</p>
      {:else if rows.length === 0}
        <p class="empty">no call recorded in the last 30 days</p>
      {:else}
        <Table label="last calls" focusableScroll={!serverAdmin}>
          {#snippet head()}
            <tr>
              <th>time</th>
              <th>model</th>
              <th>pipeline</th>
              <th>outcome</th>
              <th>duration</th>
              <th>tokens</th>
              {#if serverAdmin}<th>detail</th>{/if}
            </tr>
          {/snippet}
          {#each rows as r (r.id)}
            <tr data-call-row>
              <td class="when" data-call-id={r.id}>{fmtMinute(r.at)}</td>
              <td>{r.model}</td>
              <td><code>{r.pipeline}</code></td>
              <td class:bad={r.outcome !== 'ok'}>{r.outcome}</td>
              <td>{r.durationText}</td>
              <td>{r.tokensText}</td>
              {#if r.detailId !== null}
                {@const id = r.detailId}
                <td>
                  <button type="button" aria-expanded={detail.openId === id} onclick={() => void detail.open(id)}
                    >Open call {id.slice(-6)}</button
                  >
                </td>
              {/if}
            </tr>
            {#if serverAdmin && detail.openId === r.id}
              <tr class="detail-row">
                <td colspan="7">
                  {#if detail.status === 'loading'}
                    <span class="state">loading…</span>
                  {:else if detail.status === 'error'}
                    <span class="bad">{detail.error?.message ?? 'detail unavailable'}</span>
                  {:else if detail.detail !== null}
                    <p class="state">body: {detail.detail.body_state}</p>
                    {#if detail.detail.request_user !== null}<pre>{detail.detail.request_user}</pre>{/if}
                    {#if detail.detail.response_content !== null}<pre>{detail.detail.response_content}</pre>{/if}
                  {/if}
                </td>
              </tr>
            {/if}
          {/each}
        </Table>
      {/if}
    </section>

    <section class="card" aria-label="audit">
      <header class="card-head">
        <h2>audit trail</h2>
        <span class="count">{trail.length}</span>
      </header>
      {#if trail.length === 0}
        <p class="empty">no audit entry visible</p>
      {:else}
        <ol class="trail" aria-label="audit trail">
          {#each trail as e (e.id)}
            <li data-audit-entry>
              <span class="when">{fmtMinute(e.at)}</span>
              <span class="action">{e.entity} {e.action}</span>
              {#if e.keyHint !== null}
                <code
                  >{e.rotatedFromHint !== null ? `${e.keyHint} ← ${e.rotatedFromHint}` : e.keyHint}</code
                >
              {/if}
              {#if e.fields.length > 0}<span class="fields">{e.fields.join(', ')}</span>{/if}
              <span class="actor">{e.actor ?? 'masked'}{e.via !== null ? ` · via ${e.via}` : ''}</span>
            </li>
          {/each}
        </ol>
      {/if}
    </section>
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
  .state,
  .empty {
    margin: 0;
    padding: var(--space-2) var(--space-3);
    color: var(--text-faint);
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
  }
  .note {
    margin: 0;
    padding: var(--space-2) var(--space-3);
    color: var(--text-dim);
    font-size: var(--fs-sm);
  }
  .note.warn,
  .bad {
    color: var(--warn);
  }
  .error {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    align-items: flex-start;
    border: 1px solid var(--danger);
    border-radius: var(--radius);
    background: var(--danger-dim);
    padding: var(--space-2) var(--space-3);
    font-size: var(--fs-sm);
  }
  .error p {
    margin: 0;
    /* --text, not --danger (as ModelsPage / GatewayPage): light-theme --danger
       on --danger-dim misses AA (RC-D S3 axe probe on /gateway: 3.96:1) */
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
  .policy {
    display: grid;
    grid-template-columns: max-content 1fr;
    gap: var(--space-1) var(--space-3);
    margin: 0;
    padding: var(--space-2) var(--space-3);
    font-size: var(--fs-sm);
  }
  .policy dt {
    color: var(--text-dim);
  }
  .policy dd {
    margin: 0;
    overflow-wrap: anywhere;
  }
  .chain,
  .trail {
    list-style: none;
    margin: 0;
    padding: var(--space-2) var(--space-3);
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    font-size: var(--fs-sm);
  }
  .chain li,
  .trail li {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-2);
    align-items: baseline;
  }
  code {
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
  }
  .when,
  .actor,
  .from,
  .cut {
    color: var(--text-dim);
    font-size: var(--fs-xs);
  }
  .fields {
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
  }
  .badge,
  .chip,
  .status {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    padding: 0 var(--space-1);
  }
  .badge.peer {
    border-color: var(--accent);
  }
  /* tone on the border only — the text keeps the body colour so the contrast
     holds on the page background in both themes (axe color-contrast) */
  .status {
    color: var(--text);
  }
  .status.ok {
    border-color: var(--ok);
  }
  .status.warn {
    border-color: var(--warn);
  }
  .status.off {
    border-style: dashed;
  }
  pre {
    margin: var(--space-1) 0 0;
    white-space: pre-wrap;
    overflow-wrap: anywhere;
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
  }
</style>
