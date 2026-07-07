// Floating-window store (design 07-§B, Wave G5a). The renderer-AGNOSTIC core:
// holds windows as logical units (lu) + the surface, NEVER DOM pixels — so a
// later WebXR layer reuses this store verbatim and only swaps the render layer
// (WindowProjector + render-leaf). Injectable plain-runes class (like KeysModel)
// so vitest covers open/dedup/z-order/minimize/close-focus-return/closeAll/
// setSurface/move-clamp/resize-clamp without a DOM. Die Fenster-LISTE ist
// in-memory pro Seiten-Besuch (mirrors the ephemeral graph instance) — no
// localStorage, no URL. Die POSITIONEN sind dagegen über die SPA-Session
// gemerkt (Modul-Heap `lastRects`, U04-W6/§8-D1): ein Re-Open desselben Blocks
// — auch nach closeAll/Route-Wechsel oder in einem zweiten WindowStore —
// landet an der zuletzt user-verstellten Stelle; ein Reload startet frisch.

import { clampPos, clampSize, spawnRect, type LogicalRect, type SurfaceMetrics } from './window-projection'

// Positions-Gedächtnis der SPA-Session (U04-W6, 04-node-card §4.6). Modul-Ebene,
// AUSSERHALB der Klasse: geteilt über ALLE WindowStore-Instanzen der Session
// (Graph-Besuch #1 → Board → Graph-Besuch #2) — genau die Wechsel, bei denen der
// Store per closeAll()/Route-Wechsel OHNE close()-Aufrufe zerstört wird. Bewusst
// KEIN sessionStorage/localStorage (§8-D1): die Fenster-Liste überlebt Reload
// ohnehin nicht, also kein Serialisierungs-/Versionierungs-Code, kein Cross-Tab-
// Bleed. Keys sind Fenster-IDs (Block-/Issue-UUIDs, disjunkte Namensräume →
// kollisionsfrei). LRU-gedeckelt: auch eine 1M-Korpus-Browsing-Session wächst
// nicht über ~200 Rects (≈ 20 KB Heap).
const lastRects = new Map<string, LogicalRect>()
const LAST_RECTS_CAP = 200

/** Merkt sich das (bereits geclampte) Rect für `id` mit LRU-Touch. Map iteriert
 *  in Insert-Reihenfolge → re-insert schiebt ans Ende, Überlauf löscht den
 *  ältesten Key. Nur move()/resize() rufen das auf (nur USER-verstellte Fenster
 *  merken — die Spawn-Kaskade bleibt für nie angefasste Fenster deterministisch). */
function rememberRect(id: string, rect: LogicalRect): void {
  lastRects.delete(id) // re-insert ⇒ LRU-Touch
  lastRects.set(id, rect)
  if (lastRects.size > LAST_RECTS_CAP) {
    const oldest = lastRects.keys().next().value // ältester Key (Insert-Reihenfolge)
    if (oldest !== undefined) lastRects.delete(oldest)
  }
}

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
    // U04-W6 (§4.6): kennt die Session eine user-verstellte Position für `id`,
    // an ihr wieder öffnen — sonst deterministischer Spawn wie Ist. Das gemerkte
    // Rect wird gegen die AKTUELLE Surface re-geclampt (greifbar-Invariante,
    // §5-Nr.4): clampPos hält Titelleiste + MIN_VISIBLE on-surface; clampSize
    // erzwingt nur das MIN, KEIN Surface-Max — eine auf großem Monitor gemerkte
    // Übergröße bleibt (bewusst partiell off-canvas, wie beim User-Resize, §8-D5),
    // wird aber nie ungreifbar.
    const remembered = lastRects.get(id)
    let rect: LogicalRect
    if (remembered !== undefined) {
      const { w, h } = clampSize(remembered.w, remembered.h)
      rect = clampPos({ x: remembered.x, y: remembered.y, w, h }, this.surface)
    } else {
      rect = spawnRect(this.wins.length, this.surface)
    }
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
    rememberRect(id, { ...w.rect }) // U04-W6: NACH dem Clamp merken (Snapshot, nicht der $state-Proxy)
  }

  /** Logisches Resize-Delta (lu) + clampSize (MIN_W_LU/MIN_H_LU erzwungen). In-Place (§4.2). */
  resize(id: string, dwLu: number, dhLu: number): void {
    const w = this.wins.find((x) => x.id === id)
    if (w === undefined) return
    const { w: nw, h: nh } = clampSize(w.rect.w + dwLu, w.rect.h + dhLu)
    w.rect = { ...w.rect, w: nw, h: nh }
    rememberRect(id, { ...w.rect }) // U04-W6: NACH dem Clamp merken (Snapshot, nicht der $state-Proxy)
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
