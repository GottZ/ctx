// ConnState gates (design 05-§4.3-c, RC-1 wave S4). Two layers:
//
//   1. The PURE projection (connView) — the six-word vocabulary and the closed
//      tone set, in the node-only vitest env like IdentityBadge.
//   2. The RENDERED a11y contract — the wave's probe (d): the state must be
//      legible WITHOUT colour and must be announced as a live region.
//
// Layer 2 needs real markup. `mount()` is unavailable in this env (svelte
// resolves to its server build under the node condition, and flipping the
// resolve conditions would change rune semantics for all 88 test files), so the
// component is SERVER-rendered and asserted on its markup — no Playwright
// runtime, no new package, no config change. That covers role / aria-live /
// text content exactly; the pw-mock axe run named in §7-S4-(d) stays the Lead's
// e2e-tier gate on top (see the wave report).

import { describe, expect, it } from 'vitest'
import { render } from 'svelte/server'
import type { SseStatus } from '../sse.svelte'
import ConnState, { connView } from './ConnState.svelte'

// In the design's display order (§4.3-c): idle · connecting · live · stale ·
// reconnecting · offline.
const ALL: SseStatus[] = ['idle', 'connecting', 'open', 'stale', 'error', 'closed']

/** Server-render the component to markup. */
function html(status: SseStatus, partial = false): string {
  return render(ConnState, { props: { sse: { status, partial } } }).body
}

/** The opening tag that carries `needle`, with all its attributes — so the
 *  assertions below are attribute-ORDER agnostic. */
function tagWith(markup: string, needle: string): string {
  const m = markup.match(new RegExp(`<[a-z]+[^>]*${needle}[^>]*>`))
  return m ? m[0] : ''
}

/** Everything a screen reader would read out: tags and comments stripped.
 *  Deliberately NOT attribute-aware — a state that only lives in a class or a
 *  colour leaves this string empty. */
function textOf(markup: string): string {
  return markup
    .replace(/<[^>]*>/g, ' ')
    .replace(/\s+/g, ' ')
    .trim()
}

describe('connView — the six-word vocabulary', () => {
  it('speaks the operator words, not the transport names', () => {
    expect(ALL.map((s) => connView(s).word)).toEqual([
      'idle',
      'connecting',
      'live',
      'stale',
      'reconnecting',
      'offline',
    ])
  })

  it('gives every state its own word (no two states read alike)', () => {
    const words = ALL.map((s) => connView(s).word)
    expect(new Set(words).size).toBe(ALL.length)
  })

  it('separates stale from reconnecting — the distinction the operator acts on', () => {
    // 'error' repairs itself (backoff runs); 'stale' does not. Different word,
    // different consequence — §4.3-c "Warum stale und nicht error".
    expect(connView('error').word).not.toBe(connView('stale').word)
    expect(connView('stale').tone).toBe('danger')
    expect(connView('open').tone).toBe('ok')
  })

  it('maps every state to the closed tone set', () => {
    for (const s of ALL) expect(['ok', 'warn', 'danger', 'muted']).toContain(connView(s).tone)
  })

  it('degrades an unknown status to offline instead of rendering nothing', () => {
    // A blank indicator reads as "fine" — the one thing an unknown state is not.
    expect(connView('bogus' as SseStatus).word).toBe('offline')
  })

  it('gives every state a long form that adds to the word instead of repeating it', () => {
    for (const s of ALL) {
      const v = connView(s)
      expect(v.hint.length, `status '${s}'`).toBeGreaterThan(v.word.length)
    }
  })

  it('carries the partial flag through untouched', () => {
    expect(connView('open', true).partial).toBe(true)
    expect(connView('open').partial).toBe(false)
  })
})

// --- probe (d): the a11y contract, on the rendered markup -------------------
describe('ConnState — rendered a11y contract', () => {
  it('announces itself as a polite live region', () => {
    // RED against a plain <span> without the live region: a flip from live to
    // stale would then wait to be NOTICED instead of being announced.
    const region = tagWith(html('open'), 'role="status"')
    expect(region).not.toBe('')
    expect(region).toContain('aria-live="polite"')
  })

  it('states every status in TEXT, so colour is never the only channel', () => {
    // RED against a dot-only variant (§4.3-c "Punkt UND Wort"): with the word
    // removed the readable text is empty for all six states.
    for (const s of ALL) {
      expect(textOf(html(s)), `status '${s}' renders no word`).toContain(connView(s).word)
    }
  })

  it('hides the dot from the accessibility tree (it only repeats the word)', () => {
    expect(tagWith(html('open'), 'class="dot')).toContain('aria-hidden="true"')
  })

  it('renders the degradation as a WORD, not a colour', () => {
    expect(textOf(html('open', true))).toContain('partial')
    expect(textOf(html('open', false))).not.toContain('partial')
  })

  it('keeps state out of inline styles (K-f rule: data reaches colour via classes)', () => {
    for (const s of ALL) expect(html(s)).not.toContain('style=')
  })
})
