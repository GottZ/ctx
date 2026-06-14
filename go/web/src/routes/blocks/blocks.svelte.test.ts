// BlocksModel gates (block-workbench W1, read-only): load() reaches 'ready'
// and fills results from the empty-query default list (newest-first); the api
// is called WITHOUT a query text (or an empty one). search(q) forwards q to
// the search api. An ApiError lands in status 'error' + loadError. fakeApi
// tracks calls (pool.svelte.test.ts pattern). No W2-filter / W4-editor cases.

import { describe, expect, it } from 'vitest'
import { ApiError } from '../../lib/api'
import type {
  BlocksApi,
  ListCategoriesResponse,
  ListMetaResponse,
  SearchCursor,
  SearchRequest,
} from '../../lib/api/blocks'
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

describe('BlocksModel loadMore (W7)', () => {
  // A fake that serves a SCRIPTED sequence of search responses (page 1, page 2,
  // …) and records the `after` cursor of every search call, so the test can
  // prove the cursor round-trips and the pages append without duplication.
  function paginatingApi(pages: SearchResponse[]): BlocksApi & { afters: (SearchCursor | undefined)[] } {
    const afters: (SearchCursor | undefined)[] = []
    let i = 0
    return {
      afters,
      search: (req: SearchRequest): Promise<SearchResponse> => {
        afters.push(req.after)
        const page = pages[Math.min(i, pages.length - 1)]
        i += 1
        return Promise.resolve(page)
      },
      listMeta: (): Promise<ListMetaResponse> => Promise.resolve({ success: true, blocks: [] }),
      listCategories: (): Promise<ListCategoriesResponse> =>
        Promise.resolve({ success: true, categories: [] }),
      get: (id: string): Promise<{ success: true; block: BlockDetail }> =>
        Promise.resolve({
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
        }),
    }
  }

  const cursor1: SearchCursor = { after_updated: '2026-06-14T10:00:00Z', after_id: 'b2' }

  it('appends the next page, adopts the next cursor, then stops at null', async () => {
    const api = paginatingApi([
      // page 1: full window, more to come → next_after set
      { count: 2, results: [result({ id: 'b1' }), result({ id: 'b2' })], next_after: cursor1 },
      // page 2: last page → next_after null
      { count: 1, results: [result({ id: 'b3' })], next_after: null },
    ])
    const m = new BlocksModel(api)

    await m.load()
    expect(m.results.map((r) => r.id)).toEqual(['b1', 'b2'])
    // page 1 produced a cursor → loadMore is available.
    expect(m.nextCursor).toEqual(cursor1)

    await m.loadMore()
    // APPEND, not replace — page 2 is concatenated onto page 1.
    expect(m.results.map((r) => r.id)).toEqual(['b1', 'b2', 'b3'])
    // The cursor from page 1 was sent up on the loadMore call.
    expect(api.afters[1]).toEqual(cursor1)
    // page 2 was the last page → cursor cleared, loadMore now inert.
    expect(m.nextCursor).toBeNull()

    const callsBefore = api.afters.length
    await m.loadMore()
    expect(api.afters.length).toBe(callsBefore) // no further search call
    expect(m.results).toHaveLength(3) // unchanged
  })

  it('newest-first load with no further pages leaves nextCursor null', async () => {
    const api = paginatingApi([{ count: 1, results: [result({ id: 'b1' })], next_after: null }])
    const m = new BlocksModel(api)
    await m.load()
    expect(m.nextCursor).toBeNull()
    const before = api.afters.length
    await m.loadMore() // inert
    expect(api.afters.length).toBe(before)
  })
})
