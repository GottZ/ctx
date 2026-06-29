<script lang="ts">
  // Corpus-maintenance panel (design 04 §3, Wave A7) — the two tenant-LESS
  // background jobs on the server home scope: the G41 sensitivity audit and the
  // G40 credentials classify. Each has a dry-run-DEFAULT trigger (no write) and a
  // live progress readout that the CorpusModel polls while the run is running and
  // then stops (§6.7a). The model carries its own runes (const instance); we only
  // hold the dry-run checkbox state locally and dispose the model on unmount so no
  // poll leaks. Per §6.6 the run covers the scope echoed in the status payload,
  // not "the whole corpus" — that caveat is rendered, not implied.
  import { CorpusModel } from './corpus.svelte'

  const model = new CorpusModel()
  let auditDryRun = $state(true)
  let classifyDryRun = $state(true)

  // Stop the poll + clear the timer when this panel unmounts (no leak).
  $effect(() => () => model.dispose())

  /** ISO timestamp (from the status payload) → date+time, degrades to raw. */
  function fmt(iso?: string): string {
    if (!iso) return '—'
    const t = Date.parse(iso)
    return Number.isNaN(t) ? iso : new Date(t).toISOString().slice(0, 16).replace('T', ' ')
  }
</script>

<section class="card" aria-label="corpus maintenance">
  <header class="card-head">
    <h2>corpus maintenance</h2>
    <span class="count">audit · classify</span>
  </header>

  <p class="note">
    Sensitivity audit (G41) and credentials classify (G40) run over the server home scope only — per-tenant
    coverage is a later backend cut. Dry-run is the default and writes nothing.
  </p>

  <!-- ── Sensitivity audit (G41) ───────────────────────────────────────── -->
  <div class="op">
    <div class="op-head">
      <h3>sensitivity audit</h3>
      {#if model.auditPolling}
        <span class="live" role="status" aria-busy="true">running…</span>
      {:else if model.auditRun?.running === false}
        <span class="done" role="status">finished</span>
      {/if}
    </div>

    <div class="controls">
      <label class="dry">
        <input type="checkbox" bind:checked={auditDryRun} disabled={model.auditBusy} />
        dry-run (sample only, no write)
      </label>
      <button type="button" disabled={model.auditBusy} onclick={() => void model.startAudit(auditDryRun)}>
        {model.auditBusy ? 'audit running…' : 'Run audit'}
      </button>
    </div>

    {#if model.auditError}
      <p class="err" role="alert">{model.auditError}</p>
    {/if}

    {#if model.audit}
      {@const audit = model.audit}
      {@const run = audit.run}
      <p class="scope">
        scope <code>{audit.scope}</code> · {audit.pending} pending ·
        <span class="mode">{run.dry_run ? 'dry-run' : 'LIVE'}</span>
      </p>
      <dl class="stats">
        <div><dt>processed</dt><dd>{run.processed}</dd></div>
        <div><dt>kept credentials</dt><dd>{run.kept_credentials}</dd></div>
        <div><dt>→ personal</dt><dd>{run.to_personal}</dd></div>
        <div><dt>→ internal</dt><dd>{run.to_internal}</dd></div>
        <div><dt>no verdict</dt><dd>{run.no_verdict}</dd></div>
        <div><dt>discarded</dt><dd>{run.discarded}</dd></div>
      </dl>
      <p class="times">
        started {fmt(run.started_at)} · finished {fmt(run.finished_at)}{#if run.aborted} · <span class="abort">aborted</span>{/if}
      </p>
      {#if run.last_error}
        <p class="err" role="alert">{run.last_error}</p>
      {/if}
    {/if}
  </div>

  <!-- ── Credentials classify (G40) ────────────────────────────────────── -->
  <div class="op">
    <div class="op-head">
      <h3>credentials classify</h3>
      {#if model.classifyPolling}
        <span class="live" role="status" aria-busy="true">running…</span>
      {:else if model.classifyRun?.running === false}
        <span class="done" role="status">finished</span>
      {/if}
    </div>

    <div class="controls">
      <label class="dry">
        <input type="checkbox" bind:checked={classifyDryRun} disabled={model.classifyBusy} />
        dry-run (sample only, no write)
      </label>
      <button type="button" disabled={model.classifyBusy} onclick={() => void model.startClassify(classifyDryRun)}>
        {model.classifyBusy ? 'classify running…' : 'Run classify'}
      </button>
    </div>

    {#if model.classifyError}
      <p class="err" role="alert">{model.classifyError}</p>
    {/if}

    {#if model.classify}
      {@const classify = model.classify}
      {@const run = classify.run}
      <p class="scope">
        scope <code>{classify.scope}</code> ·
        <span class="mode">{run.dry_run ? 'dry-run' : 'LIVE'}</span>
      </p>
      <dl class="stats">
        <div><dt>scanned</dt><dd>{run.scanned}</dd></div>
        <div><dt>upgraded</dt><dd>{run.upgraded}</dd></div>
        <div><dt>discarded</dt><dd>{run.discarded}</dd></div>
      </dl>
      <p class="times">
        started {fmt(run.started_at)} · finished {fmt(run.finished_at)}{#if run.aborted} · <span class="abort">aborted</span>{/if}
      </p>
      {#if run.last_error}
        <p class="err" role="alert">{run.last_error}</p>
      {/if}
    {/if}
  </div>
</section>

<style>
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
    font-size: 0.95rem;
    font-weight: 600;
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
    color: var(--text-faint);
    font-size: 0.8rem;
    border-bottom: 1px solid var(--surface-2);
  }
  .op {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    padding: var(--space-3);
  }
  .op + .op {
    border-top: 1px solid var(--surface-2);
  }
  .op-head {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }
  h3 {
    margin: 0;
    font-size: 0.95rem;
    font-weight: 600;
    letter-spacing: 0.01em;
  }
  .live,
  .done {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    padding: 0 var(--space-1);
  }
  .live {
    color: var(--accent);
    border-color: var(--accent);
  }
  .done {
    color: var(--ok);
    border-color: var(--ok);
  }
  .controls {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    flex-wrap: wrap;
  }
  .dry {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
    font-size: 0.82rem;
    color: var(--text-dim);
  }
  button {
    font-family: inherit;
    font-size: 0.82rem;
    padding: var(--space-1) var(--space-3);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    background: var(--surface-2);
    color: var(--text);
    cursor: pointer;
  }
  button:disabled {
    opacity: 0.55;
    cursor: not-allowed;
  }
  .scope {
    margin: 0;
    font-size: 0.8rem;
    color: var(--text-dim);
  }
  .scope code {
    font-family: var(--font-mono);
    color: var(--text);
  }
  .mode {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    color: var(--text-dim);
  }
  .stats {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(8rem, 1fr));
    gap: var(--space-2);
    margin: 0;
  }
  .stats div {
    display: flex;
    flex-direction: column;
    gap: 2px;
    padding: var(--space-1) var(--space-2);
    border: 1px solid var(--surface-2);
    border-radius: var(--radius);
  }
  dt {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    color: var(--text-faint);
  }
  dd {
    margin: 0;
    font-family: var(--font-mono);
    font-size: 1.1rem;
    font-variant-numeric: tabular-nums;
    color: var(--text);
  }
  .times {
    margin: 0;
    font-family: var(--font-mono);
    font-size: 0.75rem;
    color: var(--text-faint);
  }
  .abort {
    color: var(--danger);
  }
  .err {
    margin: 0;
    padding: var(--space-2) var(--space-3);
    border: 1px solid var(--danger);
    border-radius: var(--radius);
    background: var(--danger-dim);
    color: var(--danger);
    font-size: 0.82rem;
  }
</style>
