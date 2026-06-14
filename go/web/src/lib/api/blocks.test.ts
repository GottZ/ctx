// blocks.ts wrapper gates (block-workbench W1): searchBlocks builds the
// POST /api/search body verbatim (query/category/tags/compact/limit) and
// mirrors the server's MAX-50 limit clamp client-side; listMeta calls the
// scope-gated manage list-meta action. Pure node, fetch stubbed — mirrors
// lib/api.test.ts. No secret-shaped literals.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { configureApi } from '../api'
import { MAX_SEARCH_LIMIT, listCategories, listMeta, searchBlocks } from './blocks'

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

function stubFetch(...responses: Response[]): ReturnType<typeof vi.fn> {
  const mock = vi.fn()
  for (const res of responses) mock.mockResolvedValueOnce(res)
  vi.stubGlobal('fetch', mock)
  return mock
}

/** Parse the JSON body of the recorded fetch call. */
function sentBody(mock: ReturnType<typeof vi.fn>): Record<string, unknown> {
  const init = mock.mock.calls[0]?.[1] as RequestInit
  return JSON.parse(String(init.body)) as Record<string, unknown>
}

beforeEach(() => {
  vi.unstubAllGlobals()
  configureApi({ getKey: () => null, onUnauthorized: () => {} })
})
afterEach(() => vi.unstubAllGlobals())

describe('searchBlocks', () => {
  it('posts to /api/search with the empty-query default body', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, count: 0, results: [] }))
    await searchBlocks({ query: '' })
    expect(mock.mock.calls[0]?.[0]).toBe('/api/search')
    const init = mock.mock.calls[0]?.[1] as RequestInit
    expect(init.method).toBe('POST')
    expect(sentBody(mock)).toMatchObject({ query: '' })
  })

  it('forwards category, tags, compact and limit', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, count: 0, results: [] }))
    await searchBlocks({ query: 'go', category: 'learnings', tags: ['a', 'b'], compact: true, limit: 20 })
    expect(sentBody(mock)).toMatchObject({
      query: 'go',
      category: 'learnings',
      tags: ['a', 'b'],
      compact: true,
      limit: 20,
    })
  })

  it('clamps limit to the server MAX of 50', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, count: 0, results: [] }))
    await searchBlocks({ query: 'x', limit: 999 })
    expect(sentBody(mock).limit).toBe(MAX_SEARCH_LIMIT)
    expect(MAX_SEARCH_LIMIT).toBe(50)
  })

  it('returns the parsed results envelope', async () => {
    stubFetch(
      jsonResponse(200, {
        success: true,
        count: 1,
        results: [
          {
            id: 'b1',
            category: 'learnings',
            tags: [],
            title: 'T',
            content_preview: 'p',
            content_length: 1,
            scope: 'private',
            updated_at: '2026-06-14T00:00:00Z',
            created_at: '2026-06-01T00:00:00Z',
          },
        ],
      }),
    )
    const res = await searchBlocks({ query: 'go' })
    expect(res.count).toBe(1)
    expect(res.results[0]?.id).toBe('b1')
  })
})

describe('listMeta', () => {
  it('calls the manage list-meta action', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, blocks: [] }))
    await listMeta()
    expect(mock.mock.calls[0]?.[0]).toBe('/api/manage')
    expect(sentBody(mock)).toEqual({ action: 'list-meta' })
  })

  it('returns the blocks array', async () => {
    stubFetch(
      jsonResponse(200, {
        success: true,
        blocks: [
          {
            id: 'b1',
            category: 'learnings',
            title: 'T',
            tags: [],
            scope: 'private',
            updated_at: '2026-06-14T00:00:00Z',
          },
        ],
      }),
    )
    const res = await listMeta()
    expect(res.blocks).toHaveLength(1)
    expect(res.blocks[0]?.title).toBe('T')
  })
})

describe('listCategories', () => {
  it('calls the manage list-categories action', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, categories: [] }))
    await listCategories()
    expect(mock.mock.calls[0]?.[0]).toBe('/api/manage')
    expect(sentBody(mock)).toEqual({ action: 'list-categories' })
  })

  it('returns the category counts', async () => {
    stubFetch(
      jsonResponse(200, {
        success: true,
        categories: [
          { category: 'learnings', count: 3 },
          { category: 'decisions', count: 2 },
        ],
      }),
    )
    const res = await listCategories()
    expect(res.categories).toHaveLength(2)
    expect(res.categories[0]).toEqual({ category: 'learnings', count: 3 })
  })
})
