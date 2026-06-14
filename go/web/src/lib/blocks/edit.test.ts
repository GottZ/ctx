// Block-editor pure-logic gates (block-workbench W4). The sensitivity rank +
// downgrade direction are VERIFIED against backends/trust.go (credentials rank
// 3, personal 2, internal 1, public 0; a DOWNGRADE is rank-falling and is the
// confirm-gated direction — applySensitivityGuard 400s newRank < curRank). The
// update diff emits only changed fields (store.UpdateBlockData omitempty);
// canEdit gates on scope === home_scope (writes are scope-gated, NOT admin).

import { describe, expect, it } from 'vitest'
import {
  blockDiff,
  canEdit,
  createStoreRequest,
  isEmptyDiff,
  isSensitivityDowngrade,
  sensitivityRank,
  type BlockDraft,
} from './edit'

function draft(p: Partial<BlockDraft> = {}): BlockDraft {
  return {
    category: 'learnings',
    title: 'a block',
    content: 'body',
    tags: ['go', 'graph'],
    scope: 'private',
    sensitivity: 'internal',
    ...p,
  }
}

describe('sensitivityRank', () => {
  it('mirrors backends/trust.go: credentials 3 … public 0, unknown -1', () => {
    expect(sensitivityRank('credentials')).toBe(3)
    expect(sensitivityRank('personal')).toBe(2)
    expect(sensitivityRank('internal')).toBe(1)
    expect(sensitivityRank('public')).toBe(0)
    expect(sensitivityRank('bogus')).toBe(-1)
  })
})

describe('isSensitivityDowngrade', () => {
  it('a falling rank IS a downgrade (the confirm-gated direction)', () => {
    // credentials(3) -> internal(1) opens the block to less-trusted backends.
    expect(isSensitivityDowngrade('credentials', 'internal')).toBe(true)
    expect(isSensitivityDowngrade('personal', 'public')).toBe(true)
  })

  it('a rising rank is NOT a downgrade (free upgrade)', () => {
    expect(isSensitivityDowngrade('internal', 'credentials')).toBe(false)
    expect(isSensitivityDowngrade('public', 'personal')).toBe(false)
  })

  it('equal sensitivity is not a downgrade', () => {
    expect(isSensitivityDowngrade('internal', 'internal')).toBe(false)
  })

  it('an unset next is never a downgrade (no sensitivity key is sent)', () => {
    expect(isSensitivityDowngrade('credentials', '')).toBe(false)
  })
})

describe('canEdit', () => {
  it('is true only when block.scope === home_scope', () => {
    expect(canEdit('private', 'private')).toBe(true)
  })

  it('is false for a foreign-scope block (read-only via read_scopes)', () => {
    expect(canEdit('shared', 'private')).toBe(false)
    expect(canEdit('hth', 'private')).toBe(false)
  })
})

describe('createStoreRequest', () => {
  it('always carries category/title/content/tags', () => {
    const req = createStoreRequest(draft({ tags: ['x'] }))
    expect(req.category).toBe('learnings')
    expect(req.title).toBe('a block')
    expect(req.content).toBe('body')
    expect(req.tags).toEqual(['x'])
  })

  it('sends scope when set and sensitivity when chosen', () => {
    const req = createStoreRequest(draft({ scope: 'shared', sensitivity: 'credentials' }))
    expect(req.scope).toBe('shared')
    expect(req.sensitivity).toBe('credentials')
  })

  it('omits an empty sensitivity so the server applies its default', () => {
    const req = createStoreRequest(draft({ sensitivity: '' }))
    expect('sensitivity' in req).toBe(false)
  })
})

describe('blockDiff', () => {
  it('emits only the changed scalar fields', () => {
    const orig = draft({ title: 'old', content: 'c', category: 'learnings' })
    const next = { ...orig, title: 'new', content: 'c2' }
    expect(blockDiff(orig, next)).toEqual({ title: 'new', content: 'c2' })
  })

  it('omits unchanged fields entirely (no key when identical)', () => {
    const orig = draft()
    const d = blockDiff(orig, { ...orig })
    expect(isEmptyDiff(d)).toBe(true)
    expect(Object.keys(d)).toEqual([])
  })

  it('treats tags as an order-insensitive set', () => {
    const orig = draft({ tags: ['go', 'graph'] })
    expect(isEmptyDiff(blockDiff(orig, { ...orig, tags: ['graph', 'go'] }))).toBe(true)
    expect(blockDiff(orig, { ...orig, tags: ['go', 'graph', 'pg'] }).tags).toEqual(['go', 'graph', 'pg'])
  })

  it('carries a changed sensitivity (downgrade direction included)', () => {
    const orig = draft({ sensitivity: 'credentials' })
    expect(blockDiff(orig, { ...orig, sensitivity: 'internal' }).sensitivity).toBe('internal')
  })

  it('never adds confirm_sensitivity_downgrade itself (the model/caller does)', () => {
    const orig = draft({ sensitivity: 'credentials' })
    const d = blockDiff(orig, { ...orig, sensitivity: 'internal' })
    expect('confirm_sensitivity_downgrade' in d).toBe(false)
  })
})
