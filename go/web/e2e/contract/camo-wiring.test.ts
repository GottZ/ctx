// Camo wiring gate (GHSA 2026-10-31 §7). The unit tests in
// src/lib/markdown/camo.test.ts prove that camoClickToLoad signs nothing on
// render; this gate proves the chat surface actually USES it. LLM output can be
// steered by injected text in a retrieved block into an image URL that carries
// context, so the assistant turn must never sit behind the automatic camoProxy
// (sign on render ⇒ the server fetches the URL without anyone clicking).
//
//   - MessageBubble (the chat {@html} sink) mounts use:camoClickToLoad and does
//     not import camoProxy.
//   - No file under src/routes/chat/ reaches the automatic path indirectly by
//     importing camoProxy or the shared Markdown.svelte component.
//
// Lives under e2e/contract/ like html-sinks.test.ts: src/ is svelte-checked with
// the app tsconfig, which rejects node:fs.

import { readdirSync, readFileSync } from 'node:fs'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { describe, expect, it } from 'vitest'

const CHAT_ROOT = fileURLToPath(new URL('../../src/routes/chat', import.meta.url))

describe('camo wiring — LLM output is click-to-load (GHSA 2026-10-31 §7)', () => {
  it('MessageBubble mounts camoClickToLoad, never the automatic camoProxy', () => {
    const src = readFileSync(join(CHAT_ROOT, 'MessageBubble.svelte'), 'utf8')
    expect(src).toMatch(/use:camoClickToLoad=\{html\}/)
    expect(src).not.toMatch(/camoProxy/)
  })

  it('no chat file imports camoProxy or the auto-proxying Markdown component', () => {
    const offenders = readdirSync(CHAT_ROOT, { recursive: true, encoding: 'utf8' })
      .filter((f) => f.endsWith('.svelte') || f.endsWith('.ts'))
      .filter((f) => {
        const src = readFileSync(join(CHAT_ROOT, f), 'utf8')
        return /\bcamoProxy\b/.test(src) || /markdown\/Markdown\.svelte/.test(src)
      })
    expect(offenders).toEqual([])
  })
})
