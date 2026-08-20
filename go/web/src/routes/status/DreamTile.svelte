<script lang="ts">
  // Dream tile — the QueueStats forecast + the mode control. The mode buttons
  // hit the existing dream-mode manage action (admin-gated server-side); on
  // success the TYPED answer (mode + interval + as_of) is merged into the held
  // status via onApplied — no stale reload (U01-W7, §4.5-4). The as_of raises the
  // client's floor so a late SSE frame cannot revert the mode.
  import { setDreamMode, type DreamMode, type DreamModeResponse } from '../../lib/api/status'
  import type { DreamStatus } from '../../lib/api/types'
  import { DreamBackoffModel, fmtHours } from './dream-backoff.svelte'

  let { dream, onApplied }: { dream: DreamStatus; onApplied: (r: DreamModeResponse) => void } = $props()

  // Back-off histogram (the `ctx dream stats` view): fed by every status
  // merge; the model throttles the O(n) dream-stats fetch to moved
  // last_cycle_at stamps (see dream-backoff.svelte.ts).
  const backoff = new DreamBackoffModel()
  $effect(() => {
    backoff.sync(dream.last_cycle_at)
  })

  const bo = $derived(backoff.data?.backoff ?? null)
  const maxLevelBlocks = $derived(bo ? Math.max(1, ...bo.levels.map((l) => l.blocks)) : 1)

  let busy = $state(false)
  let err = $state<string | null>(null)

  const modes: DreamMode[] = ['on', 'throttled', 'off']

  async function pick(mode: DreamMode) {
    if (busy || mode === dream.mode) return
    busy = true
    err = null
    try {
      onApplied(await setDreamMode(mode))
    } catch (e) {
      err = e instanceof Error ? e.message : String(e)
    } finally {
      busy = false
    }
  }

  function fmt(ts: string | null): string {
    return ts ? new Date(ts).toLocaleString() : '—'
  }

  const stats = $derived([
    { label: 'pickable now', value: dream.pickable_now },
    { label: 'in cooldown', value: dream.in_cooldown },
    { label: 'never dreamed', value: dream.never_dreamed },
    { label: 'awaiting embed', value: dream.awaiting_embed },
    { label: 'incoming 1h', value: dream.incoming_1h },
    { label: 'incoming 6h', value: dream.incoming_6h },
  ])
</script>

<section class="card" aria-label="dream queue">
  <header>
    <h2>dream</h2>
    <div class="modes" role="group" aria-label="dream mode">
      {#each modes as m (m)}
        <button
          type="button"
          class:active={m === dream.mode}
          disabled={busy}
          onclick={() => pick(m)}
        >
          {m}
        </button>
      {/each}
    </div>
  </header>

  {#if err}
    <p class="err" role="alert">{err}</p>
  {/if}

  <div class="grid">
    {#each stats as s (s.label)}
      <div class="stat">
        <span class="v">{s.value}</span>
        <span class="l">{s.label}</span>
      </div>
    {/each}
  </div>

  {#if bo && backoff.data}
    <div class="backoff" aria-label="re-dream back-off distribution">
      <div class="bo-head">
        <span class="bo-title">re-dream back-off</span>
        <span class="bo-policy">
          mode={bo.mode} factor={bo.factor} min={fmtHours(bo.min_hours)} grace={bo.grace} cap={fmtHours(
            bo.cap_hours,
          )} · inert +{bo.inert_offset}
        </span>
      </div>
      <div class="bo-coverage">
        checked {backoff.data.dream_checked}/{backoff.data.total_blocks}
        ({Math.round(backoff.data.coverage_pct)}%) · links {backoff.data.dream_links} · unchecked {backoff
          .data.unchecked}
      </div>
      <div class="bo-levels">
        {#each bo.levels as l (l.eval_count)}
          <span class="bo-n">n={l.eval_count}</span>
          <span class="bo-count">{l.blocks}</span>
          <span class="bo-bar">
            <span class="bo-fill" style:width={`${(l.blocks / maxLevelBlocks) * 100}%`}></span>
          </span>
          <span class="bo-cd">→ {fmtHours(l.cooldown_hours)}</span>
        {/each}
      </div>
      {#if bo.truncated}
        <div class="bo-note">… list truncated (max eval_count {bo.max_eval_count})</div>
      {/if}
    </div>
  {:else if backoff.status === 'error'}
    <div class="backoff bo-note" aria-label="re-dream back-off distribution">
      back-off distribution unavailable ({backoff.error?.message ?? 'fetch failed'})
    </div>
  {/if}

  <dl class="meta">
    <div>
      <dt>throttle</dt>
      <dd>{dream.throttle_interval_s > 0 ? `${dream.throttle_interval_s}s` : 'off'}</dd>
    </div>
    <div>
      <dt>last cycle</dt>
      <dd>{fmt(dream.last_cycle_at)}</dd>
    </div>
    <div>
      <dt>next pending</dt>
      <dd>{fmt(dream.next_pending_at)}</dd>
    </div>
  </dl>
</section>

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
  .modes {
    margin-left: auto;
    display: flex;
    gap: 1px;
    background: var(--border);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    overflow: hidden;
  }
  .modes button {
    border: none;
    border-radius: 0;
    background: var(--surface-2);
    color: var(--text-dim);
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    padding: var(--space-1) var(--space-2);
    cursor: pointer;
  }
  .modes button.active {
    background: var(--accent-dim);
    color: var(--accent);
  }
  .modes button:disabled {
    cursor: default;
    opacity: 0.6;
  }
  .err {
    margin: 0;
    padding: var(--space-2) var(--space-3);
    color: var(--danger);
    font-size: var(--fs-sm);
  }
  .grid {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(8rem, 1fr));
    gap: 1px;
    background: var(--border);
    padding: 1px;
  }
  .stat {
    background: var(--surface-1);
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    padding: var(--space-2) var(--space-3);
  }
  .stat .v {
    font-family: var(--font-mono);
    font-size: var(--fs-xl);
    font-variant-numeric: tabular-nums;
  }
  .stat .l {
    color: var(--text-faint);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
  }
  .backoff {
    padding: var(--space-2) var(--space-3);
    border-top: 1px solid var(--border);
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
  }
  .bo-head {
    display: flex;
    flex-wrap: wrap;
    align-items: baseline;
    gap: var(--space-2);
  }
  .bo-title {
    color: var(--text-faint);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
  }
  .bo-policy,
  .bo-coverage {
    color: var(--text-dim);
    font-size: var(--fs-sm);
  }
  .bo-coverage {
    margin-top: var(--space-1);
  }
  .bo-levels {
    margin-top: var(--space-2);
    display: grid;
    grid-template-columns: auto auto 1fr auto;
    align-items: center;
    column-gap: var(--space-2);
    row-gap: 2px;
  }
  .bo-n {
    color: var(--text-faint);
  }
  .bo-count {
    text-align: right;
    font-variant-numeric: tabular-nums;
  }
  .bo-bar {
    display: block;
    height: 0.75rem;
    background: var(--surface-2);
    border-radius: var(--radius);
    overflow: hidden;
  }
  .bo-fill {
    display: block;
    height: 100%;
    min-width: 2px;
    background: var(--accent);
    opacity: 0.75;
    border-radius: var(--radius);
  }
  .bo-cd {
    color: var(--text-faint);
    font-variant-numeric: tabular-nums;
    white-space: nowrap;
  }
  .bo-note {
    margin-top: var(--space-1);
    color: var(--text-faint);
  }

  .meta {
    margin: 0;
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-3);
    padding: var(--space-2) var(--space-3);
    border-top: 1px solid var(--border);
  }
  .meta div {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  dt {
    color: var(--text-faint);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
  }
  dd {
    margin: 0;
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
    color: var(--text-dim);
  }
</style>
