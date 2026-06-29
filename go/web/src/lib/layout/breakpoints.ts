// Layout-Breakpoints (Achse 01/S1). Die px-Konstanten sind die JS-Seite des
// responsiven Systems; CSS-@media nutzt die rem-Literale direkt, weil
// Media-Queries keine CSS-Custom-Properties lesen können. rem-Pendant in
// Klammern bei 16px-Root-Annahme der UA-Media-Query-Auswertung.
//
// Hinweis: html{font-size:15px} (app.css) skaliert rem-BASIERTE Maße in den
// Tokens; @media-Breakpoints werten jedoch gegen die UA-Default-16px aus —
// daher hier px, nicht rem.

export const SM = 640 // < SM: Nav-Rail → Drawer, Panes stacken
export const MD = 1024 // < MD: Nav-Rail icon-only
export const LG = 1440 // >= LG: Nav-Rail labeled, volle Canvas-Affordances

// matchMedia über globalThis (im Browser == window.matchMedia), damit der
// Pfad SSR-sicher ist (kein window-Throw) UND im Test via stubGlobal mockbar.
function mq(query: string): MediaQueryList | null {
  return typeof globalThis.matchMedia === 'function' ? globalThis.matchMedia(query) : null
}

/** True, wenn der Viewport schmaler als `px` ist (false ohne matchMedia/SSR). */
export function below(px: number): boolean {
  return mq(`(max-width: ${px - 1}px)`)?.matches ?? false
}

/** True, wenn der Viewport mindestens `px` breit ist (false ohne matchMedia/SSR). */
export function atLeast(px: number): boolean {
  return mq(`(min-width: ${px}px)`)?.matches ?? false
}

/**
 * Abonniert Viewport-Über-/Unterschreitungen von `px` und ruft `cb` mit dem
 * aktuellen below-Zustand sofort + bei jeder Änderung. Gibt eine Cleanup-
 * Funktion zurück; no-op ohne matchMedia (SSR).
 */
export function onBelow(px: number, cb: (isBelow: boolean) => void): () => void {
  const m = mq(`(max-width: ${px - 1}px)`)
  if (!m) return () => {}
  const handler = () => cb(m.matches)
  handler()
  m.addEventListener('change', handler)
  return () => m.removeEventListener('change', handler)
}
