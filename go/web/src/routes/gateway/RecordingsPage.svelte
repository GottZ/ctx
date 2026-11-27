<script lang="ts">
  // Recordings overview /gateway/recordings (issue #575): the consumers a
  // server-admin set to record (record_bodies) and those with recordings left
  // (a consumer switched off or deleted keeps its rows), each with its switch,
  // its last recording and this ctxd process's counts since the process began
  // — each row leads to the consumer's recordings.
  //
  // Server-admin only: the guard sends everybody else away before the page
  // mounts (guard.ts, /gateway/recordings → viewTenants) and the page checks
  // the tier again before its first call, like ConsumerDetail. The server is
  // the real gate (the model-recording-* actions are tierServerAdmin). The
  // list carries metadata only; a recording's content is read on its page.
  import { untrack } from 'svelte'
  import { session } from '../../lib/auth.svelte'
  import Table from '../../lib/ui/Table.svelte'
  import { fmtSecond } from './recording-data'
  import { RecordingConsumersModel } from './recordings.svelte'

  const model = new RecordingConsumersModel()
  const serverAdmin = $derived(session.caps.tier === 'server-admin')

  $effect(() => {
    if (!serverAdmin) return
    untrack(() => void model.load())
  })

  function statusOf(c: { active: boolean; deleted: boolean }): string {
    if (c.deleted) return 'deleted'
    return c.active ? 'active' : 'inactive'
  }
</script>

<section class="area">
  <header>
    <p class="back"><a href="/gateway">← Model API</a></p>
    <h1>Recordings</h1>
    <p class="sub">
      Requests of the consumers a server-admin set to record, held whole with the answer they got — credentials in a
      prompt included. Every view of a recording's content is written to the audit trail.
    </p>
  </header>

  {#if session.caps.tier === 'loading'}
    <p class="state" aria-busy="true">resolving session…</p>
  {:else if !serverAdmin}
    <p class="banner" role="alert">Recordings are for server-admins only — they are hidden for this key.</p>
  {:else if model.status === 'idle' || model.status === 'loading'}
    <p class="state" aria-busy="true">loading recording consumers…</p>
  {:else if model.status === 'error'}
    <div class="error" role="alert">
      <p>{model.loadError?.message ?? 'failed to load the recording consumers'}</p>
      {#if model.loadError?.requestId}<p class="request-id">request {model.loadError.requestId}</p>{/if}
      <button type="button" onclick={() => void model.load()}>Retry</button>
    </div>
  {:else if model.consumers.length === 0}
    <div class="empty" role="status">
      <p class="empty-title">No recordings</p>
      <p class="empty-hint">
        No consumer records and none has recordings left. A server-admin switches recording on per consumer with
        <code>model-consumer-update</code> (<code>record_bodies</code> and <code>confirm_record_bodies</code>).
      </p>
    </div>
  {:else}
    <section class="card" aria-label="recording consumers">
      <header class="card-head">
        <h2>consumers</h2>
        <span class="count">{model.consumers.length}</span>
      </header>
      <Table label="recording consumers">
        {#snippet head()}
          <tr>
            <th>consumer</th>
            <th>tenant</th>
            <th>recording</th>
            <th>status</th>
            <th>last recorded</th>
            <th>this process: recorded</th>
            <th>in flight</th>
            <th>unrecordable</th>
            <th>unfinished</th>
          </tr>
        {/snippet}
        {#each model.consumers as c (c.consumer_id)}
          <tr data-recording-consumer={c.consumer_id}>
            <td><a href={`/gateway/recordings/${c.consumer_id}`}>{c.label ?? c.consumer_id}</a></td>
            <td>{c.tenant_slug ?? '—'}</td>
            <td><span class="switch" class:on={c.record_bodies}>{c.record_bodies ? 'on' : 'off'}</span></td>
            <td>{statusOf(c)}</td>
            <td class="when">{fmtSecond(c.last_recorded_at)}</td>
            {#if c.process === null}
              <td class="num">—</td>
              <td class="num">—</td>
              <td class="num">—</td>
              <td class="num">—</td>
            {:else}
              <td class="num">{c.process.recorded}</td>
              <td class="num">{c.process.in_flight}</td>
              <td class="num" class:bad={c.process.unrecordable > 0}>{c.process.unrecordable}</td>
              <td class="num" class:bad={c.process.unfinished > 0}>{c.process.unfinished}</td>
            {/if}
          </tr>
        {/each}
      </Table>
      <p class="note">
        Counts of this ctxd process since {fmtSecond(model.since)} (process <code>{model.processId.slice(0, 8)}</code>);
        “—” = it admitted none of the consumer's requests. The recordings themselves are in the database.
      </p>
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
    margin: var(--space-1) 0 0;
    color: var(--text-dim);
    font-size: var(--fs-sm);
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
  .empty {
    border: 1px dashed var(--border-strong);
    border-radius: var(--radius);
    padding: var(--space-3);
  }
  .empty p {
    margin: 0;
  }
  .empty-title {
    font-weight: var(--fw-semibold);
  }
  .empty-hint {
    margin-top: var(--space-1);
    color: var(--text-dim);
    font-size: var(--fs-sm);
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
  .note {
    margin: 0;
    padding: var(--space-2) var(--space-3);
    color: var(--text-dim);
    font-size: var(--fs-xs);
  }
  code {
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
  }
  .when {
    color: var(--text-dim);
    font-size: var(--fs-xs);
    white-space: nowrap;
  }
  .num {
    font-family: var(--font-mono);
    text-align: right;
  }
  /* emphasis without a tone on the text — the contrast stays the body colour's */
  .bad {
    font-weight: var(--fw-semibold);
  }
  .switch {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    border: 1px dashed var(--border-strong);
    border-radius: var(--radius);
    padding: 0 var(--space-1);
  }
  .switch.on {
    border-style: solid;
    border-color: var(--warn);
  }
</style>
