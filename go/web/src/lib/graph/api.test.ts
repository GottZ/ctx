// Pins the fetchEgo query serialization against the GB5 contract (GC2 review
// carry): link_class is the ONE unified class channel — there is NO
// struct_class parameter on the server (grep go/internal → 0), so nothing in
// the client may ever emit one. Empty arrays vanish (an empty CSV would read
// as "no classes" server-side).

import { beforeEach, describe, expect, it, vi } from 'vitest'

const apiFetch = vi.fn().mockResolvedValue({ success: true })
vi.mock('../api', () => ({ apiFetch: (...args: unknown[]) => apiFetch(...args) }))

import { fetchEgo, fetchGraphAll } from './api'

describe('fetchEgo class-channel serialization', () => {
  beforeEach(() => apiFetch.mockClear())

  it('serializes the unified link_class as one CSV (both vocabularies)', async () => {
    await fetchEgo('b1', { link_class: ['topical', 'references', 'duplicate-of'] })
    const url = String(apiFetch.mock.calls[0][0])
    expect(url).toContain(`link_class=${encodeURIComponent('topical,references,duplicate-of')}`)
  })

  it('omits link_class when absent OR empty, and never emits struct_class', async () => {
    await fetchEgo('b1', {})
    await fetchEgo('b1', { link_class: [] })
    for (const call of apiFetch.mock.calls) {
      expect(String(call[0])).not.toContain('link_class')
      expect(String(call[0])).not.toContain('struct_class')
    }
  })
})

describe('fetchGraphAll serialization', () => {
  beforeEach(() => apiFetch.mockClear())

  it('hits the bare /api/graph/all without params (server defaults = ceilings)', async () => {
    await fetchGraphAll()
    expect(String(apiFetch.mock.calls[0][0])).toBe('/api/graph/all')
  })

  it('serializes the shared filter params, unified link_class included', async () => {
    await fetchGraphAll({
      limit: 100,
      min_confidence: 0.5,
      link_class: ['topical', 'references'],
      category: ['infrastructure'],
      created_after: '2026-01-01T00:00:00Z',
    })
    const url = String(apiFetch.mock.calls[0][0])
    expect(url).toContain('/api/graph/all?')
    expect(url).toContain('limit=100')
    expect(url).toContain('min_confidence=0.5')
    expect(url).toContain(`link_class=${encodeURIComponent('topical,references')}`)
    expect(url).toContain('category=infrastructure')
    // No traversal params exist on this endpoint — nothing may emit them.
    expect(url).not.toContain('hops')
    expect(url).not.toContain('per_node_cap')
  })
})
