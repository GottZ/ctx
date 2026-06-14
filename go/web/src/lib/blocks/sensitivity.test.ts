// Sensitivity badge helper gates (block-workbench W6). Pins the level→tone/
// label mapping the /blocks list + detail badge render: the four trust-gate
// levels map onto the semantic token tones (danger/warn/ok) + accent, and an
// unknown/empty level FAIL-CLOSES to the credentials (danger) badge — an
// unclassified block defaults to credentials server-side, so the badge must
// never under-state it. Mirrors lib/blocks/edit.test.ts (pure helper, no DOM).

import { describe, expect, it } from 'vitest'
import { sensitivityBadge } from './sensitivity'

describe('sensitivityBadge', () => {
  it('maps each level to its token tone (credentials→danger … public→ok)', () => {
    expect(sensitivityBadge('credentials').tone).toBe('danger')
    expect(sensitivityBadge('personal').tone).toBe('warn')
    expect(sensitivityBadge('internal').tone).toBe('accent')
    expect(sensitivityBadge('public').tone).toBe('ok')
  })

  it('labels the badge with the level name', () => {
    expect(sensitivityBadge('credentials').label).toBe('credentials')
    expect(sensitivityBadge('personal').label).toBe('personal')
    expect(sensitivityBadge('internal').label).toBe('internal')
    expect(sensitivityBadge('public').label).toBe('public')
  })

  it('fail-closes an unknown level to the credentials (danger) badge', () => {
    // A value the <select> can never emit, but the server might omit entirely.
    const b = sensitivityBadge('bogus')
    expect(b.tone).toBe('danger')
    expect(b.label).toBe('credentials')
  })

  it('fail-closes an empty/absent level to the credentials (danger) badge', () => {
    // The wire field is optional (json omitempty) — an old/unclassified row
    // arrives with no sensitivity, which the badge must treat as credentials.
    const b = sensitivityBadge('')
    expect(b.tone).toBe('danger')
    expect(b.label).toBe('credentials')
  })

  it('only ever emits a known token tone (CSS-class suffix is real)', () => {
    const tones = new Set(['danger', 'warn', 'accent', 'ok'])
    for (const lvl of ['credentials', 'personal', 'internal', 'public', 'bogus', '']) {
      expect(tones.has(sensitivityBadge(lvl).tone)).toBe(true)
    }
  })
})
