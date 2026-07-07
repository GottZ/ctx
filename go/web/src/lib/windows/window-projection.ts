// Renderer-agnostic floating-window geometry (design 07-§A, Wave G5a). The D4
// core: window position/size live as LOGICAL UNITS ("lu"), NEVER as hard DOM
// pixels. The store (windows.svelte.ts) knows ONLY lu; a WindowProjector is the
// single place that translates lu into something renderer-specific. Today the
// only renderer is 2D-DOM (DomProjector, identity scale 1 — 1 lu == 1 CSS-px),
// but the interface is generic over the output type `TRender` so a later WebXR
// layer can slot a `SpatialProjector implements WindowProjector<QuadPose>` into
// the SAME seam, mapping the same LogicalRect onto a 3D-quad pose — without
// touching the store (it stays pose-free). All such seams carry a `// WebXR:`
// marker. Pure functions / no Svelte / no DOM → vitest.

/** Logische Fenster-Geometrie. Einheit = "lu" (logical unit). KEINE px.
 *  Konvention: top-left-Origin, y-DOWN (2D-DOM-Erbe). Der SpatialProjector
 *  invertiert das extern (y-Flip auf y-up) — NICHT im Store (s. // WebXR). */
export interface LogicalRect {
  x: number
  y: number
  w: number
  h: number
} // top-left + Größe, in lu

/** Logische Ausdehnung der Fenster-Fläche (= der WindowManager-Overlay-Root).
 *  2D: 1 lu = 1 CSS-px. REINE Ausdehnung — KEINE Welt-Pose (Position/
 *  Orientierung in Metern liegt projector-lokal im SpatialProjector, s. WebXR). */
export interface SurfaceMetrics {
  wLu: number
  hLu: number
}

/** Konkrete 2D-Platzierung für den DOM-Renderer (das `TRender` des DomProjector). */
export interface RenderRect {
  left: number
  top: number
  width: number
  height: number
} // px

/**
 * Renderer-Brücke, GENERISCH über das Output-Format `TRender`. Der Store kennt
 * NUR LogicalRect (lu); der Projector ist die EINZIGE Stelle, die lu in etwas
 * Renderer-Spezifisches übersetzt. Generisch, damit der VR-Swap TYP-EHRLICH ist:
 * ein SpatialProjector ist `WindowProjector<QuadPose>` und slottet in DIESELBE
 * Schnittstelle — ein 2D-only `: RenderRect` würde das verhindern. Default
 * `TRender = RenderRect` hält den 2D-Pfad unverändert.
 * // WebXR: SpatialProjector<QuadPose> mappt denselben LogicalRect auf eine
 * //        3D-Quad-Pose (Position/Orientierung in Metern auf der Panel-Ebene,
 * //        Quad-Maße aus w/h). WinState/Store bleiben unverändert.
 */
export interface WindowProjector<TRender = RenderRect> {
  toRender(rect: LogicalRect, surface: SurfaceMetrics): TRender
  /** Konkretes Eingabe-Delta (px / Ray) -> logisches Delta (lu, panel-lokal).
   *  toLogicalDelta MUSS die lineare Skala von toRender invertieren (Round-Trip,
   *  G5a-Gate) — sonst läuft Drag bei scale≠1 aus dem Ruder. */
  toLogicalDelta(dxConcrete: number, dyConcrete: number, surface: SurfaceMetrics): { dx: number; dy: number }
}

/**
 * Der einzig gebaute Renderer: 2D-DOM, scale = 1.0 (1 lu == 1 px). Bewusst
 * Identity — die Indirektion existiert für die VR-Naht, OHNE im 2D-Pfad einen
 * Koordinaten-Bug zu riskieren (es gibt nur diesen einen Renderer heute). Dass
 * die Identity NICHT bloß „zufällig korrekt weil 1:1" ist, gatet ein NICHT-
 * Identity-Mock-Projector im G5a-Test (scale≠1, anisotrop sx≠sy).
 * // WebXR: hier slottet stattdessen ein SpatialProjector<QuadPose>.
 */
export const DomProjector: WindowProjector<RenderRect> = {
  // // WebXR: SpatialProjector liefert hier eine Quad-Pose statt RenderRect.
  toRender: (r) => ({ left: r.x, top: r.y, width: r.w, height: r.h }),
  // // WebXR: Controller/Hand-Ray-Delta -> lu (Inverse der Render-Skala).
  toLogicalDelta: (dx, dy) => ({ dx, dy }),
}

// 1 lu = 1 CSS-px (DomProjector identity). rem→px rechnet gegen den App-Root
// 15px (breakpoints.ts:6 / app.css `html{font-size:15px}`), NICHT 16 — die
// UA-16px gelten nur für @media-Breakpoints, nicht für rem-basierte Token-Maße.
const REM = 15
export const MIN_W_LU = 17 * REM // 255 lu Mindestbreite (Titelleiste + Actions lesbar)
// 12rem = 180 lu Mindesthöhe: Titlebar (25) + Padding (~26) + h2 (19) +
// Actions (~34) + ~4 Prose-Zeilen (~70) — die kleinste Form, in der die Karte
// noch „Karte mit Inhalt" ist statt leerer Rahmen (W4, 04-node-card §4.4).
export const MIN_H_LU = 12 * REM // 180 lu Mindesthöhe
export const TITLEBAR_H_LU = 2 * REM // 30 lu Titelleisten-Höhe; muss beim Move on-surface bleiben
export const MIN_VISIBLE_LU = 4 * REM // 60 lu min. greifbarer Fenster-Anteil (Grip + min/close)

export function clampSize(w: number, h: number): { w: number; h: number } {
  return { w: Math.max(MIN_W_LU, w), h: Math.max(MIN_H_LU, h) }
}

/**
 * MOVE-Clamp (greifbar-Invariante, NICHT volle Containment — n8n/Maps-Feel):
 * move DARF das Fenster teilweise aus der Surface schieben, hält aber
 * Titelleiste + MIN_VISIBLE greifbar:
 *   x ∈ [MIN_VISIBLE - w, wLu - MIN_VISIBLE]  (mind. MIN_VISIBLE an jeder Kante)
 *   y ∈ [0, hLu - TITLEBAR_H]                 (Titelleiste nie über die Oberkante / komplett unter die Unterkante)
 * Damit bleiben Grip/min/close IMMER erreichbar (sonst „verlorenes" Fenster).
 * BEWUSSTE ASYMMETRIE zu spawnRect (das voll-containt) — Open = sauber sichtbar,
 * Move = User-gesteuertes Teil-off-Canvas erlaubt. Pure → vitest (G5a).
 */
export function clampPos(r: LogicalRect, s: SurfaceMetrics): LogicalRect {
  const x = Math.max(MIN_VISIBLE_LU - r.w, Math.min(r.x, s.wLu - MIN_VISIBLE_LU))
  const y = Math.max(0, Math.min(r.y, s.hLu - TITLEBAR_H_LU))
  return { ...r, x, y }
}

export const SPAWN_STEP = 24 // lu Kaskaden-Offset je Fenster
export const SPAWN_WRAP = 6 // K: die Kaskade läuft nach K Fenstern wieder von vorn (modulo)
// Kaskaden-Ursprung NICHT (0,0): dort liegt die chrome-left-Overlay-Spalte
// (Search + Filter, GraphPage.svelte:232-242, width min(30rem,…), top/left:space-3).
// Fenster (--z-window:350 > --z-overlay:300) lägen sonst beim Open SOFORT über der
// einzigen Such-Affordanz. Ursprung jenseits der 30rem-Spalte. Auf schmaler Surface
// zieht der Containment-Clamp ihn zur rechten Kante zurück (unter SM ohnehin Sheet).
export const SPAWN_ORIGIN_X = 31 * REM // 465 lu — > chrome-left (30rem) + Gap
export const SPAWN_ORIGIN_Y = 1 * REM // 15 lu

/**
 * Open-Default + deterministische Kaskade (lu), HART und VOLL in die Surface
 * geclampt (Open-Fenster sind immer komplett sichtbar — Gegensatz zu clampPos/move).
 * Größe: 40rem-Äquiv (≤ 90% Surface-Breite) × 38rem-Äquiv (≤ 80% Höhe), durch
 * `clampSize` auf das MIN gezogen. Position: (ORIGIN + off, ORIGIN + off) mit
 * off = SPAWN_STEP*(index % SPAWN_WRAP), dann geclampt in [0, wLu-w] × [0, hLu-h].
 * Sub-Min-Sonderfall: auf einer Surface < MIN_W_LU/MIN_H_LU erzwingt `clampSize`
 * das MIN → das Fenster überläuft die Mini-Surface BEWUSST (ein sub-min, also
 * unlesbares Fenster ist schlechter als Overflow). In der Praxis unerreichbar:
 * unter SM=640 rendert der WindowManager Sheets (§D), keine spawnRect-Geometrie.
 * Pure → vitest (G5a deckt den normalen UND den Sub-Min-Zweig ab).
 */
export function spawnRect(index: number, surface: SurfaceMetrics): LogicalRect {
  // Breite 40rem (600 lu): 40rem-Fenster ≈ 38rem Prose + Body-Padding — der
  // Default zeigt die Prose-Zeile ungebrochen (U04-E2, 04-node-card §4.4).
  // Höhe 38rem (570 lu): korrespondiert mit --measure-prose; ein Normblock
  // (~1–1.5k chars ≈ 14–20 Zeilen) passt vollständig ohne Scroll (W4, §4.4).
  const { w, h } = clampSize(Math.min(40 * REM, 0.9 * surface.wLu), Math.min(38 * REM, 0.8 * surface.hLu))
  const off = SPAWN_STEP * (index % SPAWN_WRAP)
  const x = Math.max(0, Math.min(SPAWN_ORIGIN_X + off, surface.wLu - w)) // voll-clamp in [0, wLu-w]
  const y = Math.max(0, Math.min(SPAWN_ORIGIN_Y + off, surface.hLu - h)) // voll-clamp in [0, hLu-h]
  return { x, y, w, h }
}
