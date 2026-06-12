// Graph wire types + fetchers (design 05-§3.1/§3.5). The ego endpoint is the
// §3.1 envelope verbatim; edges arrive as RESPONSE-LOCAL index tuples into
// nodes/rels — resolve them immediately on merge, never store indices.

import { apiFetch } from '../api'

// Source: go/internal/handler/graph.go (egoResponse), wire format pinned by
// the §3.1 example + handler golden test.
export interface EgoResponse {
  success: true
  focus: string
  params: Record<string, unknown>
  /** Legend for edges[i][2]. */
  rels: string[]
  nodes: ApiNode[]
  edges: [src: number, dst: number, rel: number, conf: number][]
  stats: { nodes: number; edges: number; truncated: boolean; elapsed_ms: number }
}

export interface ApiNode {
  id: string
  title: string
  category: string
  scope: string
  /** Visible degree, capped at 201 (rendered as "200+"). */
  degree: number
  hop: number
  created_at: string
}

export interface EgoQuery {
  hops?: number
  per_node_cap?: number
  limit?: number
  min_confidence?: number
  link_class?: string[]
  category?: string[]
}

export function fetchEgo(block: string, query: EgoQuery = {}): Promise<EgoResponse> {
  const params = new URLSearchParams({ block })
  if (query.hops !== undefined) params.set('hops', String(query.hops))
  if (query.per_node_cap !== undefined) params.set('per_node_cap', String(query.per_node_cap))
  if (query.limit !== undefined) params.set('limit', String(query.limit))
  if (query.min_confidence !== undefined) params.set('min_confidence', String(query.min_confidence))
  if (query.link_class?.length) params.set('link_class', query.link_class.join(','))
  if (query.category?.length) params.set('category', query.category.join(','))
  return apiFetch<EgoResponse>(`/api/graph/ego?${params.toString()}`)
}

// Source: go/internal/handler/context_search.go (compact response). No
// success field on the happy path — apiFetch only rejects success:false.
export interface SearchResponse {
  count: number
  results: SearchResult[]
}

export interface SearchResult {
  id: string
  category: string
  tags: string[]
  title: string
  content_preview: string
  content_length: number
  scope: string
  updated_at: string
  created_at: string
}

export function searchBlocks(query: string, limit = 10): Promise<SearchResponse> {
  return apiFetch<SearchResponse>('/api/search', {
    method: 'POST',
    body: JSON.stringify({ query, limit }),
  })
}
