<script lang="ts" module>
  // Live-connection indicator (design 05-§4.3-c, RC-1 wave S4). Renders the six
  // SseClient states as a DOT AND A WORD, plus the `partial` word when the
  // server's heartbeat reports degraded sections.
  //
  // A11y contract: the state is legible WITHOUT colour — the word carries it,
  // the dot only repeats it (SC 1.4.1). The word therefore stays in neutral
  // text colour and only the dot takes a state class, which also keeps the
  // indicator out of the contrast ledger. The wrapper is role="status"
  // aria-live="polite", so a flip from live to stale is ANNOUNCED instead of
  // waiting to be noticed — an operator watching a dashboard is not watching
  // this corner of it.
  //
  // Vocabulary note: the transport words ('open'/'error'/'closed') are internal
  // names; the display speaks the operator's six: idle · connecting · live ·
  // stale · reconnecting · offline.
  //
  // The pure projection (connView) lives in this module script — no DOM — so it
  // unit-tests in the node-only vitest env, mirroring IdentityBadge.

  import type { SseStatus } from '../sse.svelte'

  export interface ConnView {
    /** The operator-facing word. Never empty — every status maps. */
    word: string
    /** Dot class: a CLOSED set, mapped here instead of in the markup (K-f rule,
     *  design 05-§4.8.2 — data reaches colour only through classes). */
    tone: 'ok' | 'warn' | 'danger' | 'muted'
    /** Degraded sections behind an otherwise healthy stream (hb.degraded > 0). */
    partial: boolean
    /** Long form for the title/description — the word alone is terse. */
    hint: string
  }

  const VIEWS: Record<SseStatus, { word: string; tone: ConnView['tone']; hint: string }> = {
    idle: { word: 'idle', tone: 'muted', hint: 'no live stream requested' },
    connecting: { word: 'connecting', tone: 'warn', hint: 'opening the live stream' },
    open: { word: 'live', tone: 'ok', hint: 'live stream delivering fresh data' },
    // Transport fine, data old — the one state that does NOT repair itself.
    stale: { word: 'stale', tone: 'danger', hint: 'stream connected but data is not advancing' },
    error: { word: 'reconnecting', tone: 'warn', hint: 'stream interrupted, retrying with backoff' },
    closed: { word: 'offline', tone: 'muted', hint: 'live stream closed' },
  }

  /** Project the client state into the render-ready view. Pure. */
  export function connView(status: SseStatus, partial = false): ConnView {
    // An unknown status degrades to 'offline' rather than rendering an empty
    // indicator: a blank corner reads as "fine", which is the one thing it is
    // not (forward-compat, mirroring IdentityBadge's least-privilege default).
    const v = VIEWS[status] ?? VIEWS.closed
    return { ...v, partial }
  }
</script>

<script lang="ts">
  import type { SseClient } from '../sse.svelte'

  let {
    sse,
  }: {
    /** The page's client. Read reactively — `status` and `partial` are runes. */
    sse: Pick<SseClient, 'status' | 'partial'>
  } = $props()

  const view = $derived(connView(sse.status, sse.partial))
</script>

<span class="conn" role="status" aria-live="polite">
  <span class="dot {view.tone}" aria-hidden="true"></span>
  <!-- title SUPPLEMENTS the word, it never replaces it (a11y contract §58:
       meaning is never carried by colour or title alone). -->
  <span class="word" title={view.hint}>{view.word}</span>
  {#if view.partial}
    <span class="partial" title="the server answered with degraded sections">partial</span>
  {/if}
</span>

<style>
  .conn {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
  }
  .dot {
    width: 0.55rem;
    height: 0.55rem;
    border-radius: 50%;
    flex: none;
  }
  .dot.ok {
    background: var(--ok);
  }
  .dot.warn {
    background: var(--warn);
  }
  .dot.danger {
    background: var(--danger);
  }
  .dot.muted {
    background: var(--text-faint);
  }
  .word {
    color: var(--text-dim);
  }
  .partial {
    color: var(--text-faint);
    border: 1px solid currentColor;
    border-radius: var(--radius);
    padding: 0 var(--space-1);
  }
</style>
