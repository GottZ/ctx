// Graph wire types + fetchers (design 05-§3.1/§3.5). The ego endpoint is the
// §3.1 envelope verbatim; edges arrive as RESPONSE-LOCAL index tuples into
// nodes/rels — resolve them immediately on merge, never store indices.

import { ApiError, apiFetch } from '../api'

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
  /** Legend for structural_edges[i][2] — delivered link classes (dynamic,
   *  registry-driven; NOT the fixed dream five). Absent on old servers. */
  struct_rels?: string[]
  /** Legend for structural_edges[i][3] (system | forge-sync | manual | …). */
  origins?: string[]
  /** Structural facts as index tuples into the SAME nodes array; no conf slot —
   *  structural links are 1.0 by definition (M076). Absent on old servers →
   *  the merge loop runs empty (tolerance invariant, design 03-§4.1). */
  structural_edges?: [src: number, dst: number, cls: number, origin: number][]
  /** Clusters the delivered nodes sit in (Cluster-Topic-Map C2). `cluster` is a
   *  RESPONSE-LOCAL ordinal, never a stable id — cluster_of[] indexes into this
   *  array. Empty whenever cluster.ego_annotate is off (the default), the
   *  annotation ceiling tripped, or the probe failed: one shape for "no cluster
   *  information", so the client never branches on the reason. Since C5 each
   *  entry also carries the stable `topic` handle + `label`. Absent on old
   *  servers. */
  clusters?: EgoCluster[]
  /** Positionally parallel to nodes: cluster_of[i] indexes clusters[], or -1 for
   *  "no visible membership" (unclustered, grant-only, or newer than the last
   *  rebuild). Never resolve an index against a different response. */
  cluster_of?: number[]
  stats: {
    nodes: number
    edges: number
    /** Delivered structural-edge count; stats.edges stays dream-only (E4). */
    structural_edges?: number
    /** Delivered clusters[] length (C2). */
    clusters?: number
    truncated: boolean
    elapsed_ms: number
  }
}

/** One entry of EgoResponse.clusters (C2). size is the SCOPE-PURE size summed
 *  over the partitions of this cluster the caller may see — never a global
 *  count. in_response is how many of THIS response's nodes sit in it. */
export interface EgoCluster {
  cluster: number
  /** Stable handle of this cluster's LARGEST visible partition (C5) — a v4
   *  uuid from the identity table, never the internal cluster_id. Absent while
   *  the identity layer has not reached this cluster (a normal mid-rollout
   *  state) and on old servers. Unlike `cluster` it survives rebuilds, so it is
   *  the only value worth persisting on the client. */
  topic?: string
  /** The primary partition's name; absent when unlabelled. */
  label?: string
  /** ALL visible partition handles, primary first — present ONLY when the
   *  cluster spans more than one visible scope (a handle is scope-bound, so
   *  such a cluster has several). Single-partition clusters carry `topic`
   *  alone. */
  topics?: string[]
  size: number
  top_categories: string[]
  scope_mix: string[]
  in_response: number
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
  /** ONE unified class channel (GB5, User-Direktive „kein neuer Parameter"):
   *  dream legend ∪ structural registry vocabulary in one CSV. Server
   *  semantics: absent = everything; set = both sides partitioned, an empty
   *  side matches NOTHING — toEgoQuery therefore always derives BOTH sides
   *  together (design 03-§4.3, GC2-amended). */
  link_class?: string[]
  category?: string[]
  created_after?: string
  created_before?: string
}

/**
 * GET /api/graph/all — the flat "load all" seed: every visible block up to
 * limit (server default = ceiling 1500), induced edges of both classes,
 * degrees. Same envelope as ego with focus="" (mergeEgo tolerates it: the
 * unknown focus seeds at the origin). hops/per_node_cap do not exist here —
 * nothing is traversed.
 */
export function fetchGraphAll(query: Omit<EgoQuery, 'hops' | 'per_node_cap'> = {}): Promise<EgoResponse> {
  const params = new URLSearchParams()
  if (query.limit !== undefined) params.set('limit', String(query.limit))
  if (query.min_confidence !== undefined) params.set('min_confidence', String(query.min_confidence))
  if (query.link_class?.length) params.set('link_class', query.link_class.join(','))
  if (query.category?.length) params.set('category', query.category.join(','))
  if (query.created_after) params.set('created_after', query.created_after)
  if (query.created_before) params.set('created_before', query.created_before)
  const qs = params.toString()
  return apiFetch<EgoResponse>(`/api/graph/all${qs ? `?${qs}` : ''}`)
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

/**
 * Keyset-pagination cursor for the empty-query browse path (block-workbench
 * W7). It captures the {updated_at, id} of the LAST row of a page; the next
 * request resumes strictly after it. updated_at is NOT unique, so id is the
 * mandatory tiebreak — the wire shape mirrors store.SearchCursor 1:1
 * (json:"after_updated"/"after_id"). The FTS (ranked) path never paginates.
 */
export interface SearchCursor {
  after_updated: string
  after_id: string
}

// Source: go/internal/handler/context_search.go (compact response). No
// success field on the happy path — apiFetch only rejects success:false.
export interface SearchResponse {
  count: number
  results: SearchResult[]
  /**
   * Cursor for the FOLLOWING page (W7 "Load more"), or null when there is no
   * next page (last page, or the FTS "top matches" mode that never paginates).
   * Absent on old servers ⇒ treated as null (no more pages).
   */
  next_after?: SearchCursor | null
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

export function getBlock(id: string): Promise<{ success: true; block: BlockDetail; graph_visible?: boolean }> {
  return apiFetch<{ success: true; block: BlockDetail; graph_visible?: boolean }>('/api/manage', {
    method: 'POST',
    body: JSON.stringify({ action: 'get', id }),
  })
}

/**
 * True when an ego fetch failed with the graph route's 404 — the focus block
 * does not exist or is not graph-visible (archived, retrieval-excluded type,
 * foreign scope). The handler deliberately answers one identical 404 for all
 * three (no existence oracle), so the client cannot distinguish them and must
 * treat them alike: fall back to the topic map instead of a hard error banner.
 */
export function isEgoFocusNotFound(err: unknown): boolean {
  return err instanceof ApiError && err.status === 404
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
  /**
   * Stable topic identity across rebuilds (Cluster-Topic-Map W7). Optional:
   * absent until the server has run a rebuild with the identity layer, and
   * absent for the WHOLE response while any partition is still without one —
   * the server errs towards a complete map, not a complete identity.
   *
   * Unlike cluster_id this is emittable: a v4 uuid, so no block reference and
   * no timestamp component, and scope-partitioned, so two tenants can never
   * observe a common handle.
   */
  topic?: string
  /**
   * The topic's name. ACCOMPANIES repr_title, it does not replace it: the
   * drill-down hangs off repr_id and repr_title is its caption. Render
   * `label ?? repr_title`.
   */
  label?: string
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

// ── Category-hue overrides (AM-2, U02-W5, design 02a §A3/§A4-W5) ──────────────
// Source: go/internal/handler/context_graph_category_hues.go. The GET is
// member-tier and returns the RESOLVED sparse map (tenant > _global per
// category); PUT/DELETE are tenant-admin, the server derives the target scope
// from auth (never the client). Only the HUE (HSL degree 0–359) is overridden —
// the consumer (W6) merges this atop the hash seed, this wave ships the client
// only (no renderer yet).

/** Resolved sparse override map: category → HSL hue degree (0–359). */
export interface CategoryHuesResponse {
  success: true
  hues: Record<string, number>
}

/** GET the resolved override map for the caller's effective {_global, tenant} view. */
export function fetchCategoryHues(): Promise<CategoryHuesResponse> {
  return apiFetch<CategoryHuesResponse>('/api/graph/category-hues')
}

export interface CategoryHuePutResponse {
  success: true
  category: string
  hue: number
  scope: string
}

/** PUT an override for one category (tenant-admin). hue is an integer 0–359. */
export function putCategoryHue(category: string, hue: number): Promise<CategoryHuePutResponse> {
  return apiFetch<CategoryHuePutResponse>(`/api/graph/category-hues/${encodeURIComponent(category)}`, {
    method: 'PUT',
    body: JSON.stringify({ hue }),
  })
}

export interface CategoryHueDeleteResponse {
  success: true
  category: string
  deleted: true
  scope: string
}

/** DELETE the override for one category (tenant-admin) — reverts to the seed. */
export function deleteCategoryHue(category: string): Promise<CategoryHueDeleteResponse> {
  return apiFetch<CategoryHueDeleteResponse>(`/api/graph/category-hues/${encodeURIComponent(category)}`, {
    method: 'DELETE',
  })
}
