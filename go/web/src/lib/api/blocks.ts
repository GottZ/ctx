// block-workbench read client (W1). The /blocks route reads the corpus through
// the scope-gated POST /api/search + POST /api/manage {list-meta} actions (NOT
// admin). searchBlocks builds the search body (query/category/tags/compact/limit);
// listMeta drives the A–Z navigation. Block detail reuses getBlock/BlockDetail
// from lib/graph/api so W3 shares one type.

import { apiFetch } from '../api'
import { getBlock, type BlockDetail, type SearchResponse, type SearchResult } from '../graph/api'

export { getBlock, type BlockDetail, type SearchResponse, type SearchResult }

/**
 * Server hard-clamps POST /api/search limit to 50 (context_search.go:81). The
 * wrapper mirrors it client-side so the UI never asks for more than the server
 * will return — the page paginates against a known ceiling (real keyset
 * pagination is W7).
 */
export const MAX_SEARCH_LIMIT = 50

/** POST /api/search body (context_search.go searchRequest). No offset field. */
export interface SearchRequest {
  query: string
  category?: string
  tags?: string[]
  /** compact=true → preview + content_length (server default true). */
  compact?: boolean
  limit?: number
}

/**
 * POST /api/manage {action:"list-meta"} row (blocks.go ListMeta, ORDER BY
 * category,title LIMIT 10000). Lightweight nav index — no content/preview.
 */
export interface BlockMeta {
  id: string
  category: string
  title: string
  tags: string[]
  scope: string
  updated_at: string
}

export interface ListMetaResponse {
  success: true
  blocks: BlockMeta[]
}

/**
 * POST /api/manage {action:"list-categories"} row (blocks.go CategoryCount).
 * Scope-gated (NOT admin) — drives the W2 category facet options.
 */
export interface CategoryCount {
  category: string
  count: number
}

export interface ListCategoriesResponse {
  success: true
  categories: CategoryCount[]
}

/** Injected into BlocksModel for testing (constructor-DI, pool.svelte pattern). */
export interface BlocksApi {
  search: typeof searchBlocks
  listMeta: typeof listMeta
  listCategories: typeof listCategories
  get: typeof getBlock
}

// ---- write path (W4 editor) -------------------------------------------------

/**
 * POST /api/store body (context_store.go storeRequest). Upsert on
 * (category, title, scope). scope/sensitivity omitted ⇒ server defaults
 * (home_scope, pool.default_block_sensitivity). An explicit sensitivity on an
 * upsert-conflict is upgrade-only — a downgrade goes through the update path.
 */
export interface StoreBlockRequest {
  category: string
  title: string
  content: string
  tags?: string[]
  metadata?: Record<string, unknown>
  scope?: string
  sensitivity?: string
}

export interface StoreBlockResponse {
  success: true
  /** The stored/updated block (handler returns the block envelope). */
  block: BlockDetail
}

/**
 * POST /api/manage {action:"update"} data payload (store.UpdateBlockData) —
 * ONLY the changed fields, plus confirm_sensitivity_downgrade when lowering the
 * sensitivity (else the server answers 400, context_manage.go:547). The
 * server-only sensitivity_audit field is never sent from the client.
 */
export interface UpdateBlockData {
  category?: string
  title?: string
  content?: string
  tags?: string[]
  metadata?: Record<string, unknown>
  scope?: string
  sensitivity?: string
  confirm_sensitivity_downgrade?: boolean
}

export interface UpdateBlockResponse {
  success: true
  block: BlockDetail
}

/** Injected into the W4 edit model (constructor-DI, pool.svelte pattern). */
export interface BlockWriteApi {
  store: typeof storeBlock
  update: typeof updateBlock
}

export function searchBlocks(req: SearchRequest): Promise<SearchResponse> {
  // Build the body verbatim: query is always sent; the optional fields only
  // when set (the server applies its own defaults — compact=true, limit=10 —
  // for anything omitted). limit is clamped to the server's MAX-50 ceiling so
  // the page never asks for more than /api/search will return.
  const body: SearchRequest = { query: req.query }
  if (req.category !== undefined) body.category = req.category
  if (req.tags !== undefined) body.tags = req.tags
  if (req.compact !== undefined) body.compact = req.compact
  if (req.limit !== undefined) body.limit = Math.min(req.limit, MAX_SEARCH_LIMIT)
  return apiFetch<SearchResponse>('/api/search', {
    method: 'POST',
    body: JSON.stringify(body),
  })
}

export function listMeta(): Promise<ListMetaResponse> {
  return apiFetch<ListMetaResponse>('/api/manage', {
    method: 'POST',
    body: JSON.stringify({ action: 'list-meta' }),
  })
}

/** POST /api/manage {action:"list-categories"} — category facet options (W2). */
export function listCategories(): Promise<ListCategoriesResponse> {
  // Scope-gated (NOT admin) — drives the W2 category single-select.
  return apiFetch<ListCategoriesResponse>('/api/manage', {
    method: 'POST',
    body: JSON.stringify({ action: 'list-categories' }),
  })
}

/** POST /api/store — create or upsert a block (body sent verbatim). */
export function storeBlock(req: StoreBlockRequest): Promise<StoreBlockResponse> {
  return apiFetch<StoreBlockResponse>('/api/store', {
    method: 'POST',
    body: JSON.stringify(req),
  })
}

/**
 * POST /api/manage {action:"update", id, data} — patch a block with only the
 * changed fields. `id` is the FULL block UUID (the server resolves ids in
 * HomeScope only). A sensitivity downgrade carries confirm_sensitivity_-
 * downgrade in `data`; apiFetch rejects the 400/403/409/422 as ApiError.
 */
export function updateBlock(id: string, data: UpdateBlockData): Promise<UpdateBlockResponse> {
  return apiFetch<UpdateBlockResponse>('/api/manage', {
    method: 'POST',
    body: JSON.stringify({ action: 'update', id, data }),
  })
}
