<script lang="ts">
  // Hue-Wheel (design 02a §A2/§A4-W6/§A5). Ein Rad, das die belegten Kategorie-
  // Hues als Marker zeigt — »so kann man wunderbar die abstände sehen«.
  //
  // ANZEIGE-RAUM = OKLCH (perceptuell), SPEICHER-RAUM = HSL-Hue-Grad. Die Marker
  // stehen am OKLCH-Hue-WINKEL der aus dem HSL-Hue gebackenen Farbe, damit
  // Farben, die für das Auge nah sind, auch am Rad nah sitzen. Persistiert +
  // an onpick zurückgegeben wird IMMER der HSL-Hue-Grad (0–359). PIN (§A5):
  // KEIN OKLCH-Persist einführen — nur der HSL-Hue ist vom G1a-Sweep (alle 360
  // Integer-Hues) gedeckt; ein OKLCH-Persist bräche die 1:1-Deckung »gesweepte
  // == wählbare Hues«. Die Perceptual-Geometrie ist rein kosmetisch (kein
  // Kontrast-Risiko, G1a deckt jeden HSL-Hue).
  import { hslToHex } from '../../../lib/graph/graph-client'
  import type { GraphPalette } from '../../../lib/graph/graph-theme'
  import { hexToOklchHue } from '../../../lib/graph/color-space'

  interface Marker {
    category: string
    /** Effektiver HSL-Hue-Grad (Override oder Seed). */
    hue: number
    overridden: boolean
  }

  let {
    markers,
    selected,
    palette,
    onpick,
  }: {
    markers: Marker[]
    selected: string | null
    palette: GraphPalette
    onpick: (hue: number) => void
  } = $props()

  const CX = 120
  const CY = 120
  const R = 92 // Ring-Radius (Ticks)
  const MARK_R = 92 // Marker sitzen auf dem Ring
  const STEP = 2 // HSL-Hue-Schrittweite der Ring-Ticks → 180 Ticks

  // Vorberechnete HSL-Hue → OKLCH-Winkel-Tabelle für die aktuelle Palette. Wird
  // sowohl fürs Malen der Ticks als auch für die INVERSE (Klick-Winkel → HSL-Hue)
  // gebraucht. Neu berechnet, wenn sich sat/lum (Theme) ändern.
  const table = $derived.by(() => {
    const t = new Array<number>(360)
    for (let h = 0; h < 360; h++) t[h] = hexToOklchHue(hslToHex(h, palette.nodeSat, palette.nodeLum))
    return t
  })

  /** SVG-Position eines OKLCH-Winkels (Grad) auf einem Radius. y ist SVG-abwärts,
   *  daher −sin für mathematisch-CCW-Anordnung. */
  function pos(oklchDeg: number, radius: number): { x: number; y: number } {
    const rad = (oklchDeg * Math.PI) / 180
    return { x: CX + radius * Math.cos(rad), y: CY - radius * Math.sin(rad) }
  }

  const ticks = $derived.by(() => {
    const out: { x: number; y: number; color: string }[] = []
    for (let h = 0; h < 360; h += STEP) {
      const color = hslToHex(h, palette.nodeSat, palette.nodeLum)
      const p = pos(table[h], R)
      out.push({ x: p.x, y: p.y, color })
    }
    return out
  })

  const markerViews = $derived(
    markers.map((m) => {
      const color = hslToHex(m.hue, palette.nodeSat, palette.nodeLum)
      const p = pos(table[m.hue], MARK_R)
      return { ...m, color, x: p.x, y: p.y }
    }),
  )

  let svgEl: SVGSVGElement

  /** Klick am Rad → OKLCH-Winkel → nächster HSL-Hue (inverse Tabelle) → onpick.
   *  Nur ECHTE Zeiger-Klicks (e.detail>0): Tastatur-Aktivierung des Buttons
   *  (detail=0) ignorieren — der Keyboard-Pfad läuft über den Regler, das Rad
   *  ist die Zeiger-Bequemlichkeit. */
  function onWheelClick(e: MouseEvent): void {
    if (selected === null || e.detail === 0) return
    const rect = svgEl.getBoundingClientRect()
    // Klientkoordinaten in das 240×240-viewBox skalieren.
    const mx = ((e.clientX - rect.left) / rect.width) * 240
    const my = ((e.clientY - rect.top) / rect.height) * 240
    const dx = mx - CX
    const dy = CY - my // SVG-y umdrehen
    let theta = (Math.atan2(dy, dx) * 180) / Math.PI
    if (theta < 0) theta += 360
    // Inverse: HSL-Hue, dessen OKLCH-Winkel dem Klick am nächsten liegt.
    let bestHue = 0
    let bestDist = Infinity
    for (let h = 0; h < 360; h++) {
      let d = Math.abs(table[h] - theta)
      if (d > 180) d = 360 - d
      if (d < bestDist) {
        bestDist = d
        bestHue = h
      }
    }
    onpick(bestHue)
  }
</script>

<!-- Interaktives Element = der Button (tastatur-fokussierbar); der Zeiger-Klick
     mappt Koordinaten → HSL-Hue. Der Keyboard-Pfad läuft über den Regler in der
     Page (detail=0-Guard), das svg ist dekorativ (aria-hidden). -->
<button
  type="button"
  class="wheel-btn"
  aria-label="Farb-Rad — Kategorie-Hue per Zeiger wählen (Tastatur: Regler unten)"
  onclick={onWheelClick}
>
  <svg bind:this={svgEl} viewBox="0 0 240 240" class="wheel" aria-hidden="true">
    <!-- Ring: die tatsächlichen Node-Farben, am OKLCH-Winkel platziert -->
    {#each ticks as t (t.x + ',' + t.y)}
      <circle cx={t.x} cy={t.y} r="3.4" fill={t.color} />
    {/each}

    <!-- innerer Ruhekreis -->
    <circle cx={CX} cy={CY} r="60" class="hub" />

    <!-- Marker der belegten/effektiven Hues -->
    {#each markerViews as m (m.category)}
      <g class="marker" class:sel={m.category === selected} class:ov={m.overridden}>
        <line x1={CX} y1={CY} x2={m.x} y2={m.y} class="spoke" />
        <circle cx={m.x} cy={m.y} r={m.category === selected ? 8 : 6} fill={m.color} class="dot" />
      </g>
    {/each}
  </svg>
</button>

<style>
  .wheel-btn {
    display: block;
    width: 100%;
    max-width: 20rem;
    padding: 0;
    border: none;
    background: transparent;
    cursor: crosshair;
  }
  .wheel-btn:focus-visible {
    outline: var(--focus-ring);
    outline-offset: var(--focus-offset);
    border-radius: 50%;
  }
  .wheel {
    width: 100%;
    height: auto;
    aspect-ratio: 1;
    touch-action: none;
  }
  .hub {
    fill: var(--surface-1);
    stroke: var(--border);
  }
  .spoke {
    stroke: var(--border);
    stroke-width: 1;
    opacity: 0;
  }
  .marker.sel .spoke {
    opacity: 1;
    stroke: var(--text-dim);
  }
  .marker.ov .dot {
    stroke: var(--text);
    stroke-width: 2;
  }
  .marker.sel .dot {
    stroke: var(--accent);
    stroke-width: 2.5;
  }
  .dot {
    stroke: var(--surface-0);
    stroke-width: 1;
  }
</style>
