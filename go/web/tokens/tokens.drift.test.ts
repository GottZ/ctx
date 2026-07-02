// Drift-Gate (Q1, design 05-§4.1): die committete src/styles/tokens.css MUSS
// byte-genau der Generator-Ausgabe aus tokens/tokens.json entsprechen.
// Die CSS bleibt committed (Vite dev + //go:embed brauchen sie ohne
// Build-Hook) — dieser Test macht Hand-Edits an der generierten Datei
// unmöglich: jede Abweichung = rot. Fix: tokens.json editieren und
// `bun run tokens` laufen lassen. (Liegt in tokens/, nicht src/: Node-Welt
// mit node:-Imports — typgeprüft über tsconfig.node.json, nicht die App-tsconfig.)
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { afterAll, expect, test } from 'vitest'
import { generate } from './generate.mjs'

const tokensDir = dirname(fileURLToPath(import.meta.url))
const webRoot = join(tokensDir, '..')

// Temp-Verzeichnis IM Worktree (nie /tmp: dort noexec + welt-lesbar).
const tmpBase = join(webRoot, 'node_modules', '.tmp')
mkdirSync(tmpBase, { recursive: true })
const tmpDir = mkdtempSync(join(tmpBase, 'tokens-drift-'))
afterAll(() => rmSync(tmpDir, { recursive: true, force: true }))

test('tokens.css ist byte-identisch zur Generator-Ausgabe aus tokens.json', () => {
  const generatedPath = join(tmpDir, 'tokens.css')
  writeFileSync(generatedPath, generate())

  const generated = readFileSync(generatedPath, 'utf8')
  const committed = readFileSync(join(webRoot, 'src', 'styles', 'tokens.css'), 'utf8')

  // Byte-Vergleich; bei Drift zeigt vitest den Zeilen-Diff.
  expect(committed, 'src/styles/tokens.css driftet von tokens/tokens.json — tokens.json editieren und `bun run tokens` ausführen (Hand-Edits an der CSS sind verboten)').toBe(generated)
})
