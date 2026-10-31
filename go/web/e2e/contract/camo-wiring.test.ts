// Camo wiring gate (GHSA 2026-10-31 §7, security review 2026-10-31 §E). The
// unit tests in src/lib/markdown/camo.test.ts prove that camoClickToLoad signs
// nothing on render; this gate proves every markdown surface actually USES it.
// Signing on render makes the server fetch a foreign URL without anyone
// clicking, and no surface renders only trusted text: LLM output can be steered
// by injected text in a retrieved block, and issue / comment bodies are written
// by agents through MCP, `ctx manage` and REST with the same api-key auth as the
// web UI (§E) — so there is no surface left that may sign on render.
//
//   - Both renderMarkdown {@html} sinks — Markdown.svelte (issue bodies,
//     comments) and MessageBubble (chat assistant turns) — mount
//     use:camoClickToLoad.
//   - No file under src/ mentions camoProxy, the removed sign-on-render action.
//   - signImages (POST /api/img/sign) is called only from lib/markdown/camo.ts,
//     where the click handler is its one caller; a new render-time signer
//     elsewhere turns this red.
//   - The SPA never declares the retired sign origin "issue" (the server
//     refuses it, §E): no file under src/ — tests included — writes
//     origin: 'issue', and the two camo wire modules carry no "issue" literal.
//
// Lives under e2e/contract/ like html-sinks.test.ts: src/ is svelte-checked with
// the app tsconfig, which rejects node:fs.

import { readdirSync, readFileSync } from 'node:fs'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { describe, expect, it } from 'vitest'

const SRC_ROOT = fileURLToPath(new URL('../../src', import.meta.url))

/** Every .svelte / .ts file under src/ (tests only on request), src-relative. */
function sourceFiles(withTests = false): string[] {
  return readdirSync(SRC_ROOT, { recursive: true, encoding: 'utf8' })
    .filter((f) => f.endsWith('.svelte') || f.endsWith('.ts'))
    .filter((f) => withTests || !f.endsWith('.test.ts'))
    .map((f) => f.split('\\').join('/'))
    .sort()
}

const read = (f: string) => readFileSync(join(SRC_ROOT, f), 'utf8')

describe('camo wiring — every remote image is click-to-load (GHSA 2026-10-31 §7, §E)', () => {
  it.each(['lib/markdown/Markdown.svelte', 'routes/chat/MessageBubble.svelte'])(
    '%s mounts camoClickToLoad on its {@html} sink',
    (f) => {
      expect(read(f)).toMatch(/use:camoClickToLoad=\{html\}/)
    },
  )

  it('no file under src/ references the sign-on-render camoProxy action', () => {
    const offenders = sourceFiles().filter((f) => /\bcamoProxy\b/.test(read(f)))
    expect(offenders).toEqual([])
  })

  it('signImages is called only from lib/markdown/camo.ts', () => {
    const callers = sourceFiles().filter((f) => f !== 'lib/api/images.ts' && /\bsignImages\s*\(/.test(read(f)))
    expect(callers).toEqual(['lib/markdown/camo.ts'])
  })

  it('no file under src/ declares the retired sign origin "issue"', () => {
    const offenders = sourceFiles(true).filter((f) => /\borigin\s*:\s*['"`]issue['"`]/.test(read(f)))
    expect(offenders).toEqual([])
  })

  it.each(['lib/api/images.ts', 'lib/markdown/camo.ts'])('%s carries no "issue" literal', (f) => {
    expect(read(f)).not.toMatch(/['"`]issue['"`]/)
  })
})
