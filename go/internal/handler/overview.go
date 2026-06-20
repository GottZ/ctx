// GET /api/graph/overview — scope-pure Louvain cluster supergraph (F5-W6-W2).
//
// Read-only, no LLM/provider touch. Reads the precomputed scope-partitioned
// aggregate tables (migration 057) and sums only the caller's readScopes rows.
// cluster_id is NEVER emitted (existence oracle, design §6.1) — nodes carry a
// per-request dense ordinal, edges reference those ordinals. The feature is
// gated on graph_overview.enabled (off → 404, indistinguishable from absent).

package handler

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// GraphOverviewHandler handles GET /api/graph/overview.
type GraphOverviewHandler struct {
	pool *pgxpool.Pool
	cfg  ConfigStore
}

// NewGraphOverviewHandler creates a new GraphOverviewHandler.
func NewGraphOverviewHandler(pool *pgxpool.Pool, cfg ConfigStore) *GraphOverviewHandler {
	return &GraphOverviewHandler{pool: pool, cfg: cfg}
}

// Parameter ceilings (out-of-range → 400, never silently clamped).
const (
	overviewDefaultNodeLimit = 500
	overviewMaxNodeLimit     = 2000
	overviewDefaultEdgeLimit = 2000
	overviewMaxEdgeLimit     = 20000
	overviewMaxClusterSize   = 100000000 // generous; just a sane upper bound
	overviewMaxWeight        = 100000000
)

// overviewResponse is the wire envelope (design §3.1). cluster is a per-request
// ordinal, not the internal cluster_id.
type overviewResponse struct {
	Success bool               `json:"success"`
	Params  overviewParamsEcho `json:"params"`
	Nodes   []overviewNodeWire `json:"nodes"`
	Edges   []overviewEdgeWire `json:"edges"`
	Stats   overviewStats      `json:"stats"`
}

type overviewParamsEcho struct {
	MinClusterSize int     `json:"min_cluster_size"`
	MinInterWeight float64 `json:"min_inter_cluster_weight"`
	NodeLimit      int     `json:"node_limit"`
	EdgeLimit      int     `json:"edge_limit"`
}

type overviewNodeWire struct {
	Cluster       int      `json:"cluster"` // per-request ordinal (NOT cluster_id)
	Size          int      `json:"size"`
	TopCategories []string `json:"top_categories"`
	ReprID        string   `json:"repr_id"`
	ReprTitle     string   `json:"repr_title"`
	ScopeMix      []string `json:"scope_mix"`
}

// overviewEdgeWire marshals as the compact tuple [srcOrdinal, dstOrdinal,
// link_count, weight]. weight rounded to 3 decimals (REAL float noise).
type overviewEdgeWire struct {
	Src, Dst, LinkCount int
	Weight              float64
}

func (e overviewEdgeWire) MarshalJSON() ([]byte, error) {
	return fmt.Appendf(nil, "[%d,%d,%d,%.3f]", e.Src, e.Dst, e.LinkCount, e.Weight), nil
}

type overviewStats struct {
	Nodes      int        `json:"nodes"`
	Edges      int        `json:"edges"`
	Truncated  bool       `json:"truncated"`
	ComputedAt *time.Time `json:"computed_at"` // null = never built
	ElapsedMs  int64      `json:"elapsed_ms"`
}

// HandleOverview processes GET /api/graph/overview requests.
func (h *GraphOverviewHandler) HandleOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)

	authResult := AuthResultFromContext(ctx)
	if authResult == nil || !authResult.IsValid {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "unauthorized"})
		return
	}

	cfg := h.cfg.Snapshot() //nolint:forbidigo // MT 06 BLIND: graph-overview feature gate + resolution are server-global (the rebuilt overview is one shared artifact), not tenant-scoped.

	// Feature gate: disabled → 404, indistinguishable from a route that does not
	// exist (no oracle for "the map exists but is off"). The Enabled flag also
	// gates the rebuild job (scheduler), so disabled means the tables are stale
	// or empty anyway.
	if !cfg.GraphOverview.Enabled {
		writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "Not found"})
		return
	}

	// Read rate limit (0 = disabled) — own action bucket "graph-overview".
	if limit := cfg.Query.RateLimitRead; limit > 0 {
		readCount, err := store.CheckRateLimitByAction(ctx, h.pool, authResult.ApiKeyID, "graph-overview")
		if err != nil {
			slog.Error("overview: rate limit check error", "error", err, "request_id", reqID)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "Internal server error"})
			return
		}
		if readCount >= limit {
			writeJSON(w, http.StatusTooManyRequests, map[string]any{
				"success": false,
				"error":   fmt.Sprintf("Rate limit exceeded: max %d graph reads per 60 seconds", limit),
			})
			return
		}
	}

	params, err := parseOverviewParams(r.URL.Query())
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": err.Error()})
		return
	}

	start := time.Now()
	result, err := store.GraphOverview(ctx, h.pool, params, authResult.ReadScopes)
	if err != nil {
		slog.Error("overview: query error", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "Internal server error"})
		return
	}
	elapsedMs := time.Since(start).Milliseconds()

	// Telemetry after success only (block_id=NULL, like LogGraphAccess). No 404
	// path here: an empty map is a legitimate 200 (overview has no focus block).
	if err := store.LogGraphOverviewAccess(ctx, h.pool, authResult.ApiKeyID, len(result.Nodes), len(result.Edges)); err != nil {
		slog.Error("overview: access log error", "error", err, "request_id", reqID)
	}

	writeJSON(w, http.StatusOK, buildOverviewResponse(result, params, elapsedMs))
}

// buildOverviewResponse assembles the wire envelope, assigning each cluster a
// per-request ordinal (the internal cluster_id never leaves the server) and
// remapping edges onto those ordinals. Pure — pinned by the envelope golden test.
func buildOverviewResponse(res *store.OverviewResult, p store.OverviewParams, elapsedMs int64) overviewResponse {
	ordinal := make(map[string]int, len(res.Nodes))
	nodes := make([]overviewNodeWire, len(res.Nodes))
	for i, n := range res.Nodes {
		ordinal[n.ClusterID] = i
		cats := n.TopCategories
		if cats == nil {
			cats = []string{}
		}
		mix := n.ScopeMix
		if mix == nil {
			mix = []string{}
		}
		nodes[i] = overviewNodeWire{
			Cluster:       i,
			Size:          n.Size,
			TopCategories: cats,
			ReprID:        n.ReprID,
			ReprTitle:     n.ReprTitle,
			ScopeMix:      mix,
		}
	}

	edges := make([]overviewEdgeWire, 0, len(res.Edges))
	for _, e := range res.Edges {
		si, ok1 := ordinal[e.A]
		di, ok2 := ordinal[e.B]
		if !ok1 || !ok2 {
			continue // defensive: edges are pre-restricted to returned clusters
		}
		edges = append(edges, overviewEdgeWire{Src: si, Dst: di, LinkCount: e.LinkCount, Weight: e.Weight})
	}

	var computedAt *time.Time
	if !res.ComputedAt.IsZero() {
		computedAt = &res.ComputedAt
	}

	return overviewResponse{
		Success: true,
		Params: overviewParamsEcho{
			MinClusterSize: p.MinClusterSize,
			MinInterWeight: p.MinInterWeight,
			NodeLimit:      p.NodeLimit,
			EdgeLimit:      p.EdgeLimit,
		},
		Nodes: nodes,
		Edges: edges,
		Stats: overviewStats{
			Nodes:      len(nodes),
			Edges:      len(edges),
			Truncated:  res.Truncated,
			ComputedAt: computedAt,
			ElapsedMs:  elapsedMs,
		},
	}
}

// parseOverviewParams validates the query string into store.OverviewParams.
// Reuses the ego int/float helpers; out-of-range is a 400, never clamped.
func parseOverviewParams(q url.Values) (store.OverviewParams, error) {
	p := store.OverviewParams{}
	var err error
	if p.MinClusterSize, err = egoIntParam(q, "min_cluster_size", 1, 1, overviewMaxClusterSize); err != nil {
		return p, err
	}
	if p.NodeLimit, err = egoIntParam(q, "node_limit", overviewDefaultNodeLimit, 1, overviewMaxNodeLimit); err != nil {
		return p, err
	}
	if p.EdgeLimit, err = egoIntParam(q, "edge_limit", overviewDefaultEdgeLimit, 1, overviewMaxEdgeLimit); err != nil {
		return p, err
	}
	if p.MinInterWeight, err = egoFloatParam(q, "min_inter_cluster_weight", 0, 0, overviewMaxWeight); err != nil {
		return p, err
	}
	return p, nil
}
