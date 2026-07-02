<script lang="ts">
  // Dream tile — the QueueStats forecast + the mode control. The mode buttons
  // hit the existing dream-mode manage action (admin-gated server-side); on
  // success the parent re-polls so the change shows up from the live source.
  import { setDreamMode, type DreamMode } from '../../lib/api/status'
  import type { DreamStatus } from '../../lib/api/types'

  let { dream, onRefresh }: { dream: DreamStatus; onRefresh: () => void } = $props()

  let busy = $state(false)
  let err = $state<string | null>(null)

  const modes: DreamMode[] = ['on', 'throttled', 'off']

  async function pick(mode: DreamMode) {
    if (busy || mode === dream.mode) return
    busy = true
    err = null
    try {
      await setDreamMode(mode)
      onRefresh()
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
