// Test-only Shim (U02-W3, G4b). Sigmas `sigma/rendering`-Barrel liest die WebGL-
// Enum-Konstanten (WebGL2RenderingContext.BOOL/BYTE/FLOAT …) auf MODUL-TOP-LEVEL,
// um eine Größen-Lookup-Tabelle zu bauen (node_modules/sigma/dist/index-*.js:145).
// Die vitest-'node'-Umgebung hat keine WebGL-Globals → ReferenceError beim Import.
// Dieser Shim setzt einen minimalen Ersatz, BEVOR sigma importiert wird (in der
// Test-Datei ZUERST importieren). Zur Laufzeit nie aktiv (der Browser hat die
// echten Globals), rein test-seitig — kein Produktions-Pfad berührt diese Datei.
const GL_ENUM: Record<string, number> = {
  BOOL: 0x8b56,
  BYTE: 0x1400,
  UNSIGNED_BYTE: 0x1401,
  SHORT: 0x1402,
  UNSIGNED_SHORT: 0x1403,
  INT: 0x1404,
  UNSIGNED_INT: 0x1405,
  FLOAT: 0x1406,
}
let seq = 0x9000
// Proxy: bekannte Enums → echte Werte, unbekannte Zugriffe → eindeutige Zahl
// (verhindert Key-Kollisionen in sigmas _defineProperty-Lookup).
const stub = new Proxy(GL_ENUM, {
  get: (t, k) => (typeof k === 'string' ? (k in t ? t[k] : (t[k] = seq++)) : undefined),
})
const g = globalThis as Record<string, unknown>
g.WebGL2RenderingContext ??= stub
g.WebGLRenderingContext ??= stub

export {}
