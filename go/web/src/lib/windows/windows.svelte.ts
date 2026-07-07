// Floating-window store (design 07-§B, Wave G5a). The renderer-AGNOSTIC core:
// holds windows as logical units (lu) + the surface, NEVER DOM pixels — so a
// later WebXR layer reuses this store verbatim and only swaps the render layer
// (WindowProjector + render-leaf). Injectable plain-runes class (like KeysModel)
// so vitest covers open/dedup/z-order/minimize/close-focus-return/closeAll/
// setSurface/move-clamp/resize-clamp without a DOM. In-memory per page-visit
// (mirrors the ephemeral graph instance) — no localStorage, no URL.

import { clampPos, clampSize, spawnRect, type LogicalRect, type SurfaceMetrics } from './window-projection'

export interface WinState {
  id: string // Block-UUID (Dedup-Schlüssel)
  rect: LogicalRect // NUR lu — niemals px (renderer-agnostisch)  // WebXR: dieselbe Geometrie als Quad-Pose
  z: number // abstrakter Stacking-Scalar                       // WebXR: -> Draw-Order/Tiefe
  minimized: boolean
  triggerEl: HTMLElement | null // Fokus-Rückgabe-Ziel beim Close (a11y); Node-Klick = null → Fallback s. close()
}

export class WindowStore {
  wins = $state<WinState[]>([])
  // Einzige Surface-Quelle: WindowManager misst SEINEN Overlay-Root (= Stage-Maß,
  // = Containing-Block) und pusht ihn via setSurface(); open()/spawnRect +
  // FloatingWindow-Projektion lesen sie hier. Sichere Defaults vor erster Messung.
  surface = $state<SurfaceMetrics>({ wLu: 1280, hLu: 720 })
  // Fallback-Fokusziel, wenn ein per Node-Klick (triggerEl:null) geöffnetes
  // Fenster schließt: zurück auf die Graph-Region statt auf <body> (a11y).
  // WindowManager/GraphPage setzt das beim Mount auf den Graph-Canvas/die Region.
  fallbackFocusEl: HTMLElement | null = null

  /** Offene Block-IDs — Quelle des Graph-Node-Highlights (GraphView Reducer-Set). */
  readonly openIds = $derived(new Set(this.wins.map((w) => w.id)))
  /** Höchstes z unter den nicht-minimierten Fenstern, sonst null (Top-Highlight). */
  readonly topId = $derived.by<string | null>(() => {
    let top: WinState | null = null
    for (const w of this.wins) {
      if (w.minimized) continue
      if (top === null || w.z > top.z) top = w
    }
    return top === null ? null : top.id
  })

  /** WindowManager pusht die gemessene Overlay-Root-Ausdehnung (single source). */
  setSurface(m: SurfaceMetrics): void {
    this.surface = m
  }

  /**
   * Öffnet ein Fenster für `id`. Dedup nach Block-UUID: existiert es schon →
   * `restore` (un-minimieren + nach vorn), kein doppeltes Fenster. Sonst wird
   * es per `spawnRect(len, surface)` platziert (surface aus this.surface — KEIN
   * surface-Param) und mit z = top+1 oben aufgesetzt. `triggerEl` ist das
   * Fokus-Rückgabe-Ziel beim Close (Node-Klick → null → fallbackFocusEl).
   */
  open(id: string, triggerEl: HTMLElement | null = null): void {
    const existing = this.wins.find((w) => w.id === id)
    if (existing !== undefined) {
      // U03-W1 (§4.2): ein NON-NULL triggerEl aktualisiert das gespeicherte
      // Fokus-Rückgabe-Ziel des existierenden Fensters VOR restore — ein zuvor
      // per Canvas-Klick (triggerEl:null) oder Deep-Link geöffnetes und danach
      // aus der Suche re-gepicktes Fenster führt beim Close so in die Suche
      // zurück statt auf .viewport. null lässt das gespeicherte unangetastet
      // (Canvas-Klick-Verhalten bricht nicht).
      if (triggerEl !== null) existing.triggerEl = triggerEl
      this.restore(id)
      return
    }
    const rect = spawnRect(this.wins.length, this.surface)
    const z = this.#maxZ() + 1
    this.wins = [...this.wins, { id, rect, z, minimized: false, triggerEl }]
  }

  /**
   * Schließt ein Fenster und gibt den Fokus zurück auf `win.triggerEl ??
   * this.fallbackFocusEl` — bei Node-Klick-Fenstern (triggerEl:null) also auf
   * die Graph-Region statt stiller <body>-Fokusverlust (a11y).
   */
  close(id: string): void {
    const win = this.wins.find((w) => w.id === id)
    if (win === undefined) return
    const returnEl = win.triggerEl ?? this.fallbackFocusEl
    this.wins = this.wins.filter((w) => w.id !== id)
    returnEl?.focus()
  }

  /**
   * Bulk-Reset (backToOverview / Map-Wechsel): leert wins komplett. KEIN per-win
   * triggerEl-Fokus — beim Wechsel zurück zur Cluster-Map gibt es kein Fokus-Ziel
   * (und ein Re-Focus brächte die stale Fenster des vorigen Ego-Graphen zurück).
   */
  closeAll(): void {
    this.wins = []
  }

  /**
   * Hebt `id` nach vorn. IN-PLACE-Mutation (KEINE .map()-Objekt-Ersetzung):
   * Svelte-5-`$state`-Arrays proxien tief, Feld-Mutation ist fein-granular
   * reaktiv (U04-W2, §4.2). Damit bleibt das WinState-OBJEKT identitätsstabil —
   * das Snippet-Argument (FloatingWindow.svelte:132) invalidiert nicht mehr, der
   * Content re-mountet nicht. Der z-Raise-No-op (bereits-top ⇒ kein Schreiben)
   * eliminiert zusätzlich das unbegrenzte z-Wachstum pro pointerdown.
   */
  focus(id: string): void {
    const w = this.wins.find((x) => x.id === id)
    if (w === undefined) return
    const top = this.#maxZ() // #maxZ() schließt w selbst ein
    if (w.z < top) w.z = top + 1 // nur anheben, wenn nicht bereits oben
  }

  /**
   * Logisches Move-Delta (lu) anwenden + Ergebnis via clampPos clampen
   * (greifbar-Invariante: Titelleiste + MIN_VISIBLE bleiben on-surface — ein per
   * Drag „verlorenes" Fenster ist ausgeschlossen). Der Caller projiziert px→lu
   * über den WindowProjector (toLogicalDelta), bevor er hier reingibt.
   * In-Place-Mutation des rect (§4.2) — identitätsstabil.
   */
  move(id: string, dxLu: number, dyLu: number): void {
    const w = this.wins.find((x) => x.id === id)
    if (w === undefined) return
    w.rect = clampPos({ ...w.rect, x: w.rect.x + dxLu, y: w.rect.y + dyLu }, this.surface)
  }

  /** Logisches Resize-Delta (lu) + clampSize (MIN_W_LU/MIN_H_LU erzwungen). In-Place (§4.2). */
  resize(id: string, dwLu: number, dhLu: number): void {
    const w = this.wins.find((x) => x.id === id)
    if (w === undefined) return
    const { w: nw, h: nh } = clampSize(w.rect.w + dwLu, w.rect.h + dhLu)
    w.rect = { ...w.rect, w: nw, h: nh }
  }

  minimize(id: string): void {
    const w = this.wins.find((x) => x.id === id)
    if (w !== undefined) w.minimized = true
  }

  /** Un-minimieren + nach vorn (focus). In-Place (§4.2). */
  restore(id: string): void {
    const w = this.wins.find((x) => x.id === id)
    if (w === undefined) return
    w.minimized = false
    this.focus(id)
  }

  #maxZ(): number {
    return this.wins.reduce((m, w) => Math.max(m, w.z), 0)
  }
}
