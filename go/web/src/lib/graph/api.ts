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
  created_after?: string
  created_before?: string
}

export function fetchEgo(block: string, query: EgoQuery = {}): Promise<EgoResponse> {
  const params = new URLSearchParams({ block })
  if (query.hops !== undefined) params.set('hops', String(query.hops))
  if (query.per_node_cap !== undefined) params.set('per_node_cap', String(query.per_node_cap))
  if (query.limit !== undefined) params.set('limit', String(query.limit))
  if (query.min_confidence !== undefined) params.set('min_confidence', String(query.min_confidence))
  if (query.link_class?.length) params.set('link_class', query.link_class.join(','))
  if (query.category?.length) params.set('category', query.category.join(','))
  if (query.created_after) params.set('created_after', query.created_after)
  if (query.created_before) params.set('created_before', query.created_before)
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
  /**
   * Trust-gate level (credentials|personal|internal|public) for the W6 list
   * badge. Optional + tolerant: the server omits it on old/unclassified rows
   * (json:"sensitivity,omitempty"), so the UI fail-closes an absent value.
   */
  sensitivity?: string
}

export function searchBlocks(query: string, limit = 10): Promise<SearchResponse> {
  return apiFetch<SearchResponse>('/api/search', {
    method: 'POST',
    body: JSON.stringify({ query, limit }),
  })
}

// Source: go/internal/handler/context_manage.go (handleGet) — the existing
// scope-checked block fetch; the sidebar lazy-loads full content through it.
export interface BlockDetail {
  id: string
  category: string
  tags: string[]
  title: string
  content: string
  metadata: Record<string, unknown> | null
  scope: string
  created_at: string
  updated_at: string
  /**
   * Trust-gate level (credentials|personal|internal|public) for the W6 detail
   * badge — GetBlock already RETURNS the column (blocks.go:367), only the type
   * ignored it. Optional + tolerant: the server omits it on old/unclassified
   * rows. The W4 editor seeds editInitial.sensitivity from THIS so the
   * downgrade-confirm is reachable from the page.
   */
  sensitivity?: string
}

export function getBlock(id: string): Promise<{ success: true; block: BlockDetail }> {
  return apiFetch<{ success: true; block: BlockDetail }>('/api/manage', {
    method: 'POST',
    body: JSON.stringify({ action: 'get', id }),
  })
}

// Source: go/internal/handler/overview.go (overviewResponse), wire format
// pinned by the §3.1 example + handler golden test. The cluster supergraph
// "landkarte": `cluster` is a per-request ordinal (the internal cluster_id is
// NEVER on the wire — existence oracle, design 07-§6.1); edges are local
// ordinal tuples [src, dst, link_count, weight].
export interface OverviewResponse {
  success: true
  params: Record<string, unknown>
  nodes: OverviewNode[]
  edges: [src: number, dst: number, link_count: number, weight: number][]
  stats: {
    nodes: number
    edges: number
    truncated: boolean
    /** Last rebuild timestamp; null = never built. */
    computed_at: string | null
    elapsed_ms: number
  }
}

export interface OverviewNode {
  /** Per-request ordinal (NOT the internal cluster_id). */
  cluster: number
  /** Visible member count (scope-pure, design 07-§2). */
  size: number
  top_categories: string[]
  repr_id: string
  repr_title: string
  scope_mix: string[]
}

export interface OverviewQuery {
  min_cluster_size?: number
  min_inter_cluster_weight?: number
  node_limit?: number
  edge_limit?: number
}

export function fetchOverview(query: OverviewQuery = {}): Promise<OverviewResponse> {
  const params = new URLSearchParams()
  if (query.min_cluster_size !== undefined) params.set('min_cluster_size', String(query.min_cluster_size))
  if (query.min_inter_cluster_weight !== undefined)
    params.set('min_inter_cluster_weight', String(query.min_inter_cluster_weight))
  if (query.node_limit !== undefined) params.set('node_limit', String(query.node_limit))
  if (query.edge_limit !== undefined) params.set('edge_limit', String(query.edge_limit))
  const qs = params.toString()
  return apiFetch<OverviewResponse>(`/api/graph/overview${qs ? `?${qs}` : ''}`)
}
