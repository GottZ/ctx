<script lang="ts">
  // Backend-pool tile — the read-only view of pool.Status() (design 04-§3.2).
  // Mutations live in the F5 pool editor; here it is health/state only.
  import type { BackendStatus } from '../../lib/api/types'

  let { backends }: { backends: BackendStatus[] } = $props()

  // K-f-Klassen-Mapping (Q3): active ok, cooldown/profile-disabled warn,
  // sonst idle (fail-closed). profile-disabled (092, U01-W2) reuses the warn
  // token — intentionally out, distinct from a broken/disabled row.
  function stateClass(s: string): string {
    if (s === 'active') return 'ok'
    if (s === 'cooldown') return 'warn'
    if (s === 'profile-disabled') return 'profile'
    return 'idle'
  }
</script>

<section class="card" aria-label="backend pool">
  <header>
    <h2>backends</h2>
    <span class="count">{backends.length}</span>
  </header>
  {#if backends.length === 0}
    <p class="empty">no backends in the pool — F3 not configured (the dashboard degrades to health only)</p>
  {:else}
    <div class="scroll">
      <table>
        <thead>
          <tr><th>name</th><th>trust</th><th>roles</th><th class="num">prio</th><th>state</th></tr>
        </thead>
        <tbody>
          {#each backends as b (b.id)}
            <tr class:off={!b.enabled}>
              <td class="name">
                {b.name}<span class="loc">{b.locality}</span>
              </td>
              <td><span class="badge">{b.trust}</span></td>
              <td class="roles">{b.roles.join(', ')}</td>
              <td class="num">{b.priority}</td>
              <td class="state">
                <span class="dot {stateClass(b.effective_state)}"></span>
                {b.effective_state}{#if b.effective_state === 'cooldown'}
                  · {b.cooldown_remaining_s}s{/if}
                {#if b.last_error_class}
                  <span class="errcls" title="last error class">{b.last_error_class}</span>
                {/if}
              </td>
            </tr>
          {/each}
        </tbody>
      </table>
    </div>
  {/if}
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
  .count {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    color: var(--text-dim);
  }
  .empty {
    margin: 0;
    padding: var(--space-3);
    color: var(--text-faint);
    font-size: var(--fs-sm);
  }
  .scroll {
    overflow-x: auto;
  }
  table {
    width: 100%;
    border-collapse: collapse;
    font-size: var(--fs-sm);
  }
  th {
    text-align: left;
    padding: var(--space-1) var(--space-3);
    color: var(--text-faint);
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    font-weight: var(--fw-medium);
    border-bottom: 1px solid var(--border);
  }
  td {
    padding: var(--space-1) var(--space-3);
    border-bottom: 1px solid var(--surface-2);
    vertical-align: top;
  }
  tr.off {
    opacity: 0.5;
  }
  .name {
    font-family: var(--font-mono);
    display: flex;
    flex-direction: column;
  }
  .loc {
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
  .badge {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    text-transform: uppercase;
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    padding: 0 var(--space-1);
    color: var(--text-dim);
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
    margin-left: var(--space-2);
    color: var(--danger);
    font-family: var(--font-mono);
    font-size: var(--label-size);
  }
</style>
