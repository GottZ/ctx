<script lang="ts">
  // Interactive dream back-off curve editor (settings dream card): renders
  // the would-be cooldown curve over the DRAFT values of the six
  // dream.backoff_* keys as a smoothed bezier, live on every keystroke, with
  // the saved policy ghosted behind it and the corpus maturity histogram
  // (dream-stats levels) underneath. Mouse interaction writes DRAFTS only —
  // dragging the curve solves the factor, dragging the min/cap guides sets
  // the floor/ceiling — so nothing is active until the card's Save button
  // runs; the BackoffSaveWatcher then re-evaluates the live pipeline
  // (dream-backoff-restamp) and refetches the histogram.

  import { onMount } from 'svelte'
  import { fmtHours } from '../status/dream-backoff.svelte'
  import { formatValue } from '../../lib/settings'
  import type { SettingsModel } from './model.svelte'
  import {
    BACKOFF_KEYS,
    BackoffSaveWatcher,
    bezierPath,
    cooldownHours,
    fmtHoursDraft,
    policyFromDrafts,
    solveFactor,
    xAxisMax,
    type BackoffPolicy,
  } from './backoff-curve.svelte'

  let { model }: { model: SettingsModel } = $props()

  const watcher = new BackoffSaveWatcher()
  onMount(() => void watcher.loadStats())

  // Saved-value fingerprint → one restamp per completed Save (not per draft).
  const savedFingerprint = $derived(BACKOFF_KEYS.map((k) => formatValue(model.byKey(k)?.value)).join('|'))
  $effect(() => watcher.sync(savedFingerprint))

  const draftPolicy = $derived(policyFromDrafts((key) => model.drafts[key] ?? ''))
  const savedPolicy = $derived(policyFromDrafts((key) => formatValue(model.byKey(key)?.value)))
  const policy = $derived(draftPolicy.policy)
  const dirty = $derived(BACKOFF_KEYS.some((k) => model.isDirty(k)))
  const policiesDiffer = $derived(
    savedPolicy.policy !== null && policy !== null && JSON.stringify(savedPolicy.policy) !== JSON.stringify(policy),
  )

  // ── geometry ────────────────────────────────────────────────────────────
  const W = 680
  const H = 330
  const M = { left: 56, right: 16, top: 14, bottom: 70 }
  const plotW = W - M.left - M.right
  const plotH = H - M.top - M.bottom
  const histTop = H - M.bottom + 22
  const histH = 30

  const nMax = $derived(xAxisMax(policy, watcher.stats?.backoff.max_eval_count ?? 0))
  const yMaxHours = $derived.by(() => {
    const caps = [336] // off-mode inert 14d keeps the axis honest across modes
    if (policy !== null) caps.push(policy.capHours)
    if (savedPolicy.policy !== null) caps.push(savedPolicy.policy.capHours)
    return Math.max(...caps) * 1.15
  })

  const x = (n: number): number => M.left + (n / nMax) * plotW
  const y = (hours: number): number => {
    const clamped = Math.min(Math.max(hours, 1), yMaxHours)
    return M.top + (1 - Math.log(clamped) / Math.log(yMaxHours)) * plotH
  }
  const hoursFromY = (py: number): number => {
    const frac = 1 - (py - M.top) / plotH
    return Math.exp(Math.min(Math.max(frac, 0), 1) * Math.log(yMaxHours))
  }
  const nFromX = (px: number): number => Math.round(Math.min(Math.max((px - M.left) / plotW, 0), 1) * nMax)

  const curvePoints = (p: BackoffPolicy, inert: boolean): Array<{ x: number; y: number }> => {
    const pts: Array<{ x: number; y: number }> = []
    for (let n = 0; n <= nMax; n++) pts.push({ x: x(n), y: y(cooldownHours(p, n, inert)) })
    return pts
  }
  const activePath = $derived(policy === null ? '' : bezierPath(curvePoints(policy, false)))
  const inertPath = $derived(policy === null ? '' : bezierPath(curvePoints(policy, true)))
  const savedPath = $derived(
    policiesDiffer && savedPolicy.policy !== null ? bezierPath(curvePoints(savedPolicy.policy, false)) : '',
  )

  // Log-decade ticks that exist inside the axis range, labeled via fmtHours.
  const yTicks = $derived([1, 6, 24, 72, 24 * 7, 24 * 30, 24 * 90].filter((h) => h <= yMaxHours))
  const xTickStep = $derived(nMax > 40 ? 10 : nMax > 16 ? 5 : 2)
  const xTicks = $derived(Array.from({ length: Math.floor(nMax / xTickStep) + 1 }, (_, i) => i * xTickStep))

  const levels = $derived(watcher.stats?.backoff.levels ?? [])
  const maxLevelBlocks = $derived(levels.reduce((m, l) => Math.max(m, l.blocks), 0))

  // ── pointer interaction ─────────────────────────────────────────────────
  let svgEl = $state<SVGSVGElement | null>(null)
  let drag = $state<'min' | 'cap' | 'curve' | null>(null)
  let hover = $state<{ n: number; active: number; inert: number } | null>(null)

  function svgPoint(e: PointerEvent): { px: number; py: number } {
    const rect = svgEl?.getBoundingClientRect()
    if (rect === undefined || svgEl === null) return { px: 0, py: 0 }
    return {
      px: ((e.clientX - rect.left) / rect.width) * W,
      py: ((e.clientY - rect.top) / rect.height) * H,
    }
  }

  function startDrag(target: 'min' | 'cap' | 'curve', e: PointerEvent): void {
    if (policy === null || model.saving) return
    if (target === 'curve' && policy.mode === 'off') return
    drag = target
    svgEl?.setPointerCapture(e.pointerId)
    applyDrag(e)
  }

  function applyDrag(e: PointerEvent): void {
    if (drag === null || policy === null) return
    const { px, py } = svgPoint(e)
    const hours = hoursFromY(py)
    if (drag === 'min') {
      const clamped = Math.min(Math.max(hours, 1), policy.capHours)
      model.drafts['dream.backoff_min'] = fmtHoursDraft(clamped)
    } else if (drag === 'cap') {
      const clamped = Math.max(hours, Math.max(policy.minHours, 1))
      model.drafts['dream.backoff_cap'] = fmtHoursDraft(clamped)
    } else {
      const f = solveFactor(policy, nFromX(px), hours)
      if (f !== null) model.drafts['dream.backoff_factor'] = String(f)
    }
  }

  function onPointerMove(e: PointerEvent): void {
    if (drag !== null) {
      applyDrag(e)
      return
    }
    if (policy === null) return
    const { px } = svgPoint(e)
    const n = nFromX(px)
    hover = { n, active: cooldownHours(policy, n, false), inert: cooldownHours(policy, n, true) }
  }

  function endDrag(e: PointerEvent): void {
    if (drag !== null) svgEl?.releasePointerCapture(e.pointerId)
    drag = null
  }

  function stepInt(key: 'dream.backoff_grace' | 'dream.backoff_inert_offset', delta: number): void {
    const current = Number((model.drafts[key] ?? '').trim())
    const base = Number.isInteger(current) ? current : 0
    model.drafts[key] = String(Math.max(0, base + delta))
  }
</script>

<div class="curve" class:dirty>
  <div class="legend">
    <span class="title">re-dream back-off curve</span>
    <span class="mode">mode <code>{model.drafts['dream.backoff_mode'] ?? ''}</code></span>
    <span class="mode">factor <code>{model.drafts['dream.backoff_factor'] ?? ''}</code></span>
    <span class="stepper">
      grace <code>{model.drafts['dream.backoff_grace'] ?? ''}</code>
      <button type="button" aria-label="decrease grace" onclick={() => stepInt('dream.backoff_grace', -1)}>−</button>
      <button type="button" aria-label="increase grace" onclick={() => stepInt('dream.backoff_grace', 1)}>+</button>
    </span>
    <span class="stepper">
      inert +<code>{model.drafts['dream.backoff_inert_offset'] ?? ''}</code>
      <button type="button" aria-label="decrease inert offset" onclick={() => stepInt('dream.backoff_inert_offset', -1)}
        >−</button
      >
      <button type="button" aria-label="increase inert offset" onclick={() => stepInt('dream.backoff_inert_offset', 1)}
        >+</button
      >
    </span>
  </div>

  {#if policy === null}
    <p class="notice warn" role="status">
      curve paused — invalid draft in {draftPolicy.invalid.join(', ')}; the last valid values stay active
    </p>
  {:else}
    <svg
      bind:this={svgEl}
      viewBox="0 0 {W} {H}"
      role="img"
      aria-label="dream back-off cooldown curve over eval count, draggable"
      class:dragging={drag !== null}
      onpointermove={onPointerMove}
      onpointerup={endDrag}
      onpointercancel={endDrag}
      onpointerleave={() => (hover = null)}
    >
      <!-- grid + axes -->
      {#each yTicks as h (h)}
        <line class="grid" x1={M.left} y1={y(h)} x2={W - M.right} y2={y(h)} />
        <text class="tick" x={M.left - 6} y={y(h) + 3} text-anchor="end">{fmtHours(h)}</text>
      {/each}
      {#each xTicks as n (n)}
        <text class="tick" x={x(n)} y={H - M.bottom + 14} text-anchor="middle">{n}</text>
      {/each}
      <text class="axis-label" x={M.left + plotW / 2} y={H - 2} text-anchor="middle">completed dream cycles (dream_eval_count)</text>

      <!-- corpus maturity histogram (saved pipeline state) -->
      {#if maxLevelBlocks > 0}
        {#each levels as level (level.eval_count)}
          <rect
            class="hist"
            x={x(level.eval_count) - 3}
            y={histTop + histH - (level.blocks / maxLevelBlocks) * histH}
            width="6"
            height={(level.blocks / maxLevelBlocks) * histH}
          >
            <title>{level.blocks} blocks at n={level.eval_count} → {fmtHours(level.cooldown_hours)}</title>
          </rect>
        {/each}
        <text class="tick" x={M.left - 6} y={histTop + histH} text-anchor="end">blocks</text>
      {/if}

      <!-- min/cap guides with drag handles -->
      <g class="guide" role="presentation" onpointerdown={(e) => startDrag('cap', e)}>
        <line x1={M.left} y1={y(policy.capHours)} x2={W - M.right} y2={y(policy.capHours)} />
        <rect class="handle" x={W - M.right - 46} y={y(policy.capHours) - 8} width="46" height="16" rx="3" />
        <text class="handle-label" x={W - M.right - 23} y={y(policy.capHours) + 3} text-anchor="middle">
          cap {fmtHours(policy.capHours)}
        </text>
      </g>
      <g class="guide" role="presentation" onpointerdown={(e) => startDrag('min', e)}>
        <line x1={M.left} y1={y(policy.minHours)} x2={W - M.right} y2={y(policy.minHours)} />
        <rect class="handle" x={M.left} y={y(policy.minHours) - 8} width="46" height="16" rx="3" />
        <text class="handle-label" x={M.left + 23} y={y(policy.minHours) + 3} text-anchor="middle">
          min {fmtHours(policy.minHours)}
        </text>
      </g>

      <!-- curves: saved ghost, draft inert, draft active (drag surface) -->
      {#if savedPath !== ''}
        <path class="saved" d={savedPath} />
      {/if}
      <path class="inert" d={inertPath} />
      <path class="active" d={activePath} role="presentation" onpointerdown={(e) => startDrag('curve', e)} />

      <!-- hover crosshair -->
      {#if hover !== null && drag === null}
        <line class="crosshair" x1={x(hover.n)} y1={M.top} x2={x(hover.n)} y2={H - M.bottom} />
        <circle class="dot active-dot" cx={x(hover.n)} cy={y(hover.active)} r="3.5" />
        <circle class="dot inert-dot" cx={x(hover.n)} cy={y(hover.inert)} r="3" />
        <text class="readout" x={Math.min(x(hover.n) + 8, W - 150)} y={M.top + 12}>
          n={hover.n} · active {fmtHours(hover.active)} · inert {fmtHours(hover.inert)}
        </text>
      {/if}
    </svg>

    <p class="hint">
      drag the curve to set the factor, drag the guides for min/cap — drafts only;
      <strong>Save</strong> activates them and re-evaluates every block's cooldown
    </p>
  {/if}

  {#if watcher.phase === 'running'}
    <p class="notice" role="status" aria-busy="true">re-evaluating back-off pipeline…</p>
  {:else if watcher.phase === 'done' && watcher.restamped !== null}
    <p class="notice ok" role="status">
      pipeline re-evaluated — {watcher.restamped.restamped} cooldown stamps recomputed
      {#if watcher.restamped.skipped_transient > 0}({watcher.restamped.skipped_transient} transient claims untouched){/if}
    </p>
  {:else if watcher.phase === 'error'}
    <p class="notice warn" role="alert">pipeline re-evaluation failed: {watcher.errorMessage}</p>
  {/if}
</div>

<style>
  .curve {
    margin: var(--space-2) var(--space-3);
    padding: var(--space-2);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    background: var(--surface-0);
  }
  .curve.dirty {
    border-color: var(--accent);
  }

  .legend {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    flex-wrap: wrap;
    margin-bottom: var(--space-1);
    font-size: var(--fs-xs);
    color: var(--text-dim);
  }
  .title {
    font-family: var(--font-mono);
    color: var(--text);
  }
  .mode code,
  .stepper code {
    color: var(--accent);
    background: transparent;
    padding: 0;
  }
  .stepper {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
  }
  .stepper button {
    font-size: var(--fs-xs);
    padding: 0 var(--space-1);
    background: transparent;
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    cursor: pointer;
    color: var(--text-dim);
  }
  .stepper button:hover {
    border-color: var(--accent);
    color: var(--text);
  }

  svg {
    display: block;
    width: 100%;
    height: auto;
    touch-action: none;
    user-select: none;
  }
  svg.dragging {
    cursor: grabbing;
  }

  .grid {
    stroke: var(--border);
    stroke-width: 0.5;
  }
  .tick {
    fill: var(--text-faint);
    font-family: var(--font-mono);
    /* stylelint-disable-next-line scale-unlimited/declaration-strict-value -- SVG-viewBox-Einheit: font-size in px ist hier eine USER-UNIT im 680x330-viewBox (skaliert mit der Grafik), keine Typo-Skalen-Stufe; rem-Tokens skalieren NICHT mit dem viewBox mit und machen die Achsen-Labels bei schmalen Karten unlesbar. */
    font-size: 9px;
  }
  .axis-label {
    fill: var(--text-faint);
    /* stylelint-disable-next-line scale-unlimited/declaration-strict-value -- SVG-viewBox-Einheit: font-size in px ist hier eine USER-UNIT im 680x330-viewBox (skaliert mit der Grafik), keine Typo-Skalen-Stufe; rem-Tokens skalieren NICHT mit dem viewBox mit und machen die Achsen-Labels bei schmalen Karten unlesbar. */
    font-size: 9px;
  }

  .hist {
    fill: var(--text-faint);
    opacity: 0.55;
  }

  .guide line {
    stroke: var(--warn);
    stroke-width: 1;
    stroke-dasharray: 5 4;
    opacity: 0.7;
  }
  .guide .handle {
    fill: var(--surface-1);
    stroke: var(--warn);
    cursor: ns-resize;
  }
  .guide .handle-label {
    fill: var(--warn);
    font-family: var(--font-mono);
    /* stylelint-disable-next-line scale-unlimited/declaration-strict-value -- SVG-viewBox-Einheit: font-size in px ist hier eine USER-UNIT im 680x330-viewBox (skaliert mit der Grafik), keine Typo-Skalen-Stufe; rem-Tokens skalieren NICHT mit dem viewBox mit und machen die Achsen-Labels bei schmalen Karten unlesbar. */
    font-size: 8px;
    pointer-events: none;
  }

  .saved {
    fill: none;
    stroke: var(--text-faint);
    stroke-width: 1.5;
    opacity: 0.5;
  }
  .inert {
    fill: none;
    stroke: var(--accent);
    stroke-width: 1;
    stroke-dasharray: 3 4;
    opacity: 0.7;
  }
  .active {
    fill: none;
    stroke: var(--accent);
    stroke-width: 2.5;
    cursor: grab;
  }
  .active:hover {
    stroke-width: 3.5;
  }

  .crosshair {
    stroke: var(--text-faint);
    stroke-width: 0.5;
  }
  .dot.active-dot {
    fill: var(--accent);
  }
  .dot.inert-dot {
    fill: none;
    stroke: var(--accent);
  }
  .readout {
    fill: var(--text);
    font-family: var(--font-mono);
    /* stylelint-disable-next-line scale-unlimited/declaration-strict-value -- SVG-viewBox-Einheit: font-size in px ist hier eine USER-UNIT im 680x330-viewBox (skaliert mit der Grafik), keine Typo-Skalen-Stufe; rem-Tokens skalieren NICHT mit dem viewBox mit und machen die Achsen-Labels bei schmalen Karten unlesbar. */
    font-size: 10px;
  }

  .hint {
    margin: var(--space-1) 0 0;
    font-size: var(--fs-xs);
    color: var(--text-faint);
  }
  .notice {
    margin: var(--space-1) 0 0;
    font-size: var(--fs-xs);
    color: var(--text-dim);
  }
  .notice.ok {
    color: var(--accent);
  }
  .notice.warn {
    color: var(--warn);
  }
</style>
