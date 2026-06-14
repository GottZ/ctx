// BlocksModel gates (block-workbench W1, read-only): load() reaches 'ready'
// and fills results from the empty-query default list (newest-first); the api
// is called WITHOUT a query text (or an empty one). search(q) forwards q to
// the search api. An ApiError lands in status 'error' + loadError. fakeApi
// tracks calls (pool.svelte.test.ts pattern). No W2-filter / W4-editor cases.

import { describe, expect, it } from 'vitest'
import { ApiError } from '../../lib/api'
import type { BlocksApi, ListCategoriesResponse, ListMetaResponse } from '../../lib/api/blocks'
import type { BlockDetail, SearchResponse, SearchResult } from '../../lib/graph/api'
import { defaultFilters } from '../../lib/blocks/filters'
import { BlocksModel } from './blocks.svelte'

function result(p: Partial<SearchResult> & Pick<SearchResult, 'id'>): SearchResult {
  return {
    category: 'learnings',
    tags: [],
    title: `title-${p.id}`,
    content_preview: 'preview',
    content_length: 7,
    scope: 'private',
    updated_at: '2026-06-14T00:00:00Z',
    created_at: '2026-06-01T00:00:00Z',
    ...p,
  }
}

interface Call {
  m: 'search' | 'listMeta' | 'listCategories' | 'get'
  query?: string
  category?: string
  tags?: string[]
  id?: string
}

function fakeApi(initial: SearchResult[], fail?: Partial<Record<Call['m'], ApiError>>): BlocksApi & {
  calls: Call[]
  setResults: (r: SearchResult[]) => void
} {
  const calls: Call[] = []
  let current = initial
  return {
    calls,
    setResults: (r: SearchResult[]) => (current = r),
    search: (req): Promise<SearchResponse> => {
      calls.push({ m: 'search', query: req.query, category: req.category, tags: req.tags })
      if (fail?.search) return Promise.reject(fail.search)
      return Promise.resolve({ count: current.length, results: current })
    },
    listMeta: (): Promise<ListMetaResponse> => {
      calls.push({ m: 'listMeta' })
      if (fail?.listMeta) return Promise.reject(fail.listMeta)
      return Promise.resolve({ success: true, blocks: [] })
    },
    listCategories: (): Promise<ListCategoriesResponse> => {
      calls.push({ m: 'listCategories' })
      if (fail?.listCategories) return Promise.reject(fail.listCategories)
      return Promise.resolve({
        success: true,
        categories: [
          { category: 'learnings', count: 3 },
          { category: 'decisions', count: 2 },
        ],
      })
    },
    get: (id: string): Promise<{ success: true; block: BlockDetail }> => {
      calls.push({ m: 'get', id })
      if (fail?.get) return Promise.reject(fail.get)
      return Promise.resolve({
        success: true,
        block: {
          id,
          category: 'learnings',
          tags: [],
          title: 't',
          content: 'c',
          metadata: null,
          scope: 'private',
          created_at: '2026-06-01T00:00:00Z',
          updated_at: '2026-06-14T00:00:00Z',
        },
      })
    },
  }
}

describe('BlocksModel load', () => {
  it('populates and reaches ready', async () => {
    const api = fakeApi([result({ id: 'b1' }), result({ id: 'b2' })])
    const m = new BlocksModel(api)
    await m.load()
    expect(m.status).toBe('ready')
    expect(m.results).toHaveLength(2)
    expect(m.results[0]?.id).toBe('b1')
  })

  it('loads the default list with no query text', async () => {
    const api = fakeApi([result({ id: 'b1' })])
    const m = new BlocksModel(api)
    await m.load()
    const search = api.calls.find((c) => c.m === 'search')
    expect(search).toBeDefined()
    // Empty-query default list: the model must not invent a query string
    // (no `?? ''` coalesce — an undefined query must fail this, not pass).
    expect(search?.query).toBe('')
  })

  it('surfaces a load error', async () => {
    const api = fakeApi([], { search: new ApiError(403, 'forbidden', 'no access') })
    const m = new BlocksModel(api)
    await m.load()
    expect(m.status).toBe('error')
    expect(m.loadError?.status).toBe(403)
  })
})

describe('BlocksModel search', () => {
  it('forwards the query to the api and reaches ready', async () => {
    const api = fakeApi([result({ id: 'b1' })])
    const m = new BlocksModel(api)
    await m.search('graph traversal')
    const search = api.calls.find((c) => c.m === 'search')
    expect(search?.query).toBe('graph traversal')
    expect(m.status).toBe('ready')
    expect(m.results).toHaveLength(1)
  })

  it('surfaces a search error', async () => {
    const api = fakeApi([], { search: new ApiError(500, 'server', 'boom') })
    const m = new BlocksModel(api)
    await m.search('q')
    expect(m.status).toBe('error')
    expect(m.loadError?.status).toBe(500)
  })
})

describe('BlocksModel filters (W2)', () => {
  it('forwards active category + tags into the search body', async () => {
    const api = fakeApi([result({ id: 'b1' })])
    const m = new BlocksModel(api)
    await m.setFilters({ ...defaultFilters(), query: 'go', category: 'learnings', tags: ['a', 'b'] })
    const search = api.calls.find((c) => c.m === 'search')
    expect(search?.query).toBe('go')
    expect(search?.category).toBe('learnings')
    expect(search?.tags).toEqual(['a', 'b'])
    expect(m.status).toBe('ready')
    expect(m.filters.category).toBe('learnings')
  })

  it('empty filters behave like W1 (empty query, no category/tags)', async () => {
    const api = fakeApi([result({ id: 'b1' })])
    const m = new BlocksModel(api)
    await m.setFilters(defaultFilters())
    const search = api.calls.find((c) => c.m === 'search')
    expect(search?.query).toBe('')
    // Defaults are omitted from the body — undefined, not '' / [].
    expect(search?.category).toBeUndefined()
    expect(search?.tags).toBeUndefined()
  })

  it('loadCategories fills the facet options', async () => {
    const api = fakeApi([])
    const m = new BlocksModel(api)
    await m.loadCategories()
    expect(api.calls.some((c) => c.m === 'listCategories')).toBe(true)
    expect(m.categories.map((c) => c.category)).toEqual(['learnings', 'decisions'])
  })
})
