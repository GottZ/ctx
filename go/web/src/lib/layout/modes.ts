// Route -> layout-mode map as a pure module (design 01-shell-layout §3, §4 S2):
// the shell reads the mode synchronously from the pathname BEFORE the lazy page
// chunk mounts, so navigation never flashes the previous area's layout. Kept
// here (not page-declared) and free of any DOM/router import so vitest can
// assert it in a plain node environment — mirrors entryRedirect in
// routes/index.ts. routes/index.ts re-exports areaMode so the route-namespace
// test can pin "every areaRoutes key resolves to a LayoutMode".
//
// Anything not under a mapped prefix is 'reading' (settings, status,
// /settings/backends, the role-gated /admin and /tenant management areas (N4/N5
// — prose/forms, so they take the reading column), unknown paths, the '*'
// catch-all): registering a new route in areaRoutes does NOT silently change
// its layout — it stays 'reading' until listed in MODE_BY_PREFIX below. Only
// NON-reading modes are listed there, so /admin and /tenant need no entry; the
// default already pins them to 'reading'.

/** The four content-region layouts the shell drives via `data-mode`. */
export type LayoutMode = 'reading' | 'canvas' | 'split' | 'thread'

/**
 * Non-reading area modes keyed by route prefix. Prefixes are disjoint, so first
 * match wins. Matched the same way the reserved server namespace is
 * (`p === prefix || p.startsWith(`${prefix}/`)`) so deep sub-routes (e.g.
 * /graph/<id>) inherit their area's mode.
 */
const MODE_BY_PREFIX: ReadonlyArray<readonly [string, LayoutMode]> = [
  ['/graph', 'canvas'],
  ['/blocks', 'split'],
  ['/chat', 'thread'],
]

/**
 * Resolve the layout mode for a pathname. Pure: same input -> same output, no
 * side effects. Everything outside the mapped prefixes is 'reading'.
 */
export function areaMode(pathname: string): LayoutMode {
  for (const [prefix, mode] of MODE_BY_PREFIX) {
    if (pathname === prefix || pathname.startsWith(`${prefix}/`)) return mode
  }
  return 'reading'
}
