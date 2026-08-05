// GET /api/graph/cluster — cluster ego: the members and neighbours of ONE topic
// (Cluster-Topic-Map, design/03 §4.3, wave C7).
//
// Read-only, no LLM/provider touch. It is the drill-down the landkarte lacks:
// today a click on a cluster opens the ego net of its representative, which in
// 41 of 59 live clusters is just the oldest block with quality_score 1 — the
// caller lands in an arbitrary neighbourhood instead of in the topic.
//
// DOUBLE FEATURE GATE, and both halves answer the SAME 404 as an absent route:
// graph_overview.enabled (verbatim the handler/overview.go reasoning — the flag
// also gates the rebuild job, so "off" means the tables are stale or empty
// anyway) and cluster.route_enabled (the wave's own switch; without it C7 would
// be the only wave of this axis with no way back, breaking the pausability
// invariant). Neither gate writes an access-log row.

package handler

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"time"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// GraphClusterHandler handles GET /api/graph/cluster.
type GraphClusterHandler struct {
	pool       *pgxpool.Pool
	cfg        ConfigStore
	blocktypes *blocktype.Registry
}

// NewGraphClusterHandler creates a new GraphClusterHandler. blocktypes feeds the
// per-request type-visibility allowlist (WF T6) — never the compiled-in builtin
// set, so a live registry edit changes what a topic contains without a restart.
func NewGraphClusterHandler(pool *pgxpool.Pool, cfg ConfigStore, blocktypes *blocktype.Registry) *GraphClusterHandler {
	return &GraphClusterHandler{pool: pool, cfg: cfg, blocktypes: blocktypes}
}

// READ CAPS — this route's own ceilings, deliberately not inherited.
//
// The member cap shares the ego route's 1500 because it is the same payload
// question (nodes without content over one HTTP response) and the same measured
// envelope; the DEFAULT stays 500, so the expensive shape is something a caller
// asks for explicitly. The neighbour cap is its own axis: neighbours are TOPICS,
// bounded by cluster-pair count rather than corpus size, and 200 is far above
// the live maximum while still bounding the aggregate.
//
// Out of range is a 400, never a silent clamp — the ceiling discipline of
// handler/graph.go, reusing its egoIntParam helper rather than a second copy.
const (
	clusterDefaultLimit         = 500
	clusterMaxLimit             = 1500
	clusterDefaultNeighborLimit = 50
	clusterMaxNeighborLimit     = 200
)

// clusterEgoResponse is the wire envelope (§4.3).
type clusterEgoResponse struct {
	Success   bool                    `json:"success"`
	Params    clusterParamsEcho       `json:"params"`
	Cluster   clusterMetaWire         `json:"cluster"`
	Nodes     []store.ClusterMember   `json:"nodes"`
	Neighbors []store.ClusterNeighbor `json:"neighbors"`
	Stats     clusterStats            `json:"stats"`
}

type clusterParamsEcho struct {
	Limit         int `json:"limit"`
	NeighborLimit int `json:"neighbor_limit"`
}

// clusterMetaWire describes the topic itself.
//
// LabelSource/LabelModel are the label PROVENANCE (decision E4-02 / amendment
// A01-3): they live in the database and on THIS detail route, and deliberately
// not on the overview wire — the map is a dense list where per-node provenance
// would be noise, while a caller who opened one topic is exactly the caller who
// may want to know whether the name came from a human, a model, or the
// deterministic fallback.
//
// computed_at rides along unconditionally, because this route hands the
// freshness judgement to its viewer instead of gating on it (§4.3). null = never
// built.
type clusterMetaWire struct {
	Handle        string     `json:"handle"`
	Label         string     `json:"label,omitempty"`
	LabelSource   string     `json:"label_source"`
	LabelModel    string     `json:"label_model,omitempty"`
	Size          int        `json:"size"`
	TopCategories []string   `json:"top_categories"`
	ScopeMix      []string   `json:"scope_mix"`
	ReprID        string     `json:"repr_id"`
	ReprTitle     string     `json:"repr_title"`
	ComputedAt    *time.Time `json:"computed_at"`
}

type clusterStats struct {
	Nodes     int   `json:"nodes"`
	Neighbors int   `json:"neighbors"`
	Truncated bool  `json:"truncated"`
	ElapsedMs int64 `json:"elapsed_ms"`
}

// HandleCluster processes GET /api/graph/cluster requests.
func (h *GraphClusterHandler) HandleCluster(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)

	authResult := AuthResultFromContext(ctx)
	if authResult == nil || !authResult.IsValid {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "unauthorized"})
		return
	}

	cfg := h.cfg.Snapshot() //nolint:forbidigo // MT 06 BLIND: like GET /api/graph/overview, both gates and the cluster map itself are server-global (ONE rebuilt artefact), not tenant-scoped.

	// Double gate. One indistinguishable 404 for both, and for "the route does
	// not exist" — there is no oracle for "the feature exists but is off".
	if !cfg.GraphOverview.Enabled || !cfg.ClusterOps.RouteEnabled {
		writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "Not found"})
		return
	}

	// Read rate limit (0 = disabled) — own action bucket, like every other graph
	// surface. The bucket name matches the access-log action.
	if limit := cfg.Query.RateLimitRead; limit > 0 {
		readCount, err := store.CheckRateLimitByAction(ctx, h.pool, authResult.ApiKeyID, "graph-cluster")
		if err != nil {
			slog.Error("cluster: rate limit check error", "error", err, "request_id", reqID)
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

	params, err := parseClusterParams(r.URL.Query())
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": err.Error()})
		return
	}

	start := time.Now()
	result, err := store.ClusterEgo(ctx, h.pool, params, authResult.ReadScopes, h.blocktypes.SnapshotForRequest(ctx).VisibleTypes())
	if err != nil {
		if errors.Is(err, store.ErrNotVisible) {
			// ONE identical 404 for "does not exist", "not visible" and "retired",
			// and NO access_log entry: otherwise the telemetry itself would be
			// bumpable by UUID probing, which is the oracle in slow motion.
			writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "Cluster not found"})
			return
		}
		slog.Error("cluster: query error", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "Internal server error"})
		return
	}
	elapsedMs := time.Since(start).Milliseconds()

	// Telemetry strictly AFTER a successful, visible read. Best effort: a failed
	// audit insert must not fail a completed read.
	if err := store.LogGraphClusterAccess(ctx, h.pool, authResult.ApiKeyID, len(result.Members), len(result.Neighbors)); err != nil {
		slog.Error("cluster: access log error", "error", err, "request_id", reqID)
	}

	writeJSON(w, http.StatusOK, buildClusterResponse(result, params, elapsedMs))
}

// buildClusterResponse assembles the wire envelope. Pure — pinned by the
// envelope test.
func buildClusterResponse(res *store.ClusterEgoResult, p store.ClusterEgoParams, elapsedMs int64) clusterEgoResponse {
	nodes := res.Members
	if nodes == nil {
		nodes = []store.ClusterMember{}
	}
	neighbors := res.Neighbors
	if neighbors == nil {
		neighbors = []store.ClusterNeighbor{}
	}
	// Weight is a REAL sum; marshalled raw it would emit float noise like
	// 4.811999797821045. Three decimals, the same rounding the edge tuples of
	// the other graph surfaces carry.
	for i := range neighbors {
		neighbors[i].Weight = math.Round(neighbors[i].Weight*1000) / 1000
	}
	cats := res.TopCategories
	if cats == nil {
		cats = []string{}
	}
	var computedAt *time.Time
	if !res.ComputedAt.IsZero() {
		computedAt = &res.ComputedAt
	}
	return clusterEgoResponse{
		Success: true,
		Params:  clusterParamsEcho{Limit: p.Limit, NeighborLimit: p.NeighborLimit},
		Cluster: clusterMetaWire{
			Handle:        res.Handle,
			Label:         res.Label,
			LabelSource:   res.LabelSource,
			LabelModel:    res.LabelModel,
			Size:          res.Size,
			TopCategories: cats,
			// A handle is scope-bound, so the mix is exactly one scope. It stays
			// an ARRAY to keep the shape of the other cluster surfaces — a client
			// reading `scope_mix` should not have to switch types per route.
			ScopeMix:   []string{res.Scope},
			ReprID:     res.ReprID,
			ReprTitle:  res.ReprTitle,
			ComputedAt: computedAt,
		},
		Nodes:     nodes,
		Neighbors: neighbors,
		Stats: clusterStats{
			Nodes:     len(nodes),
			Neighbors: len(neighbors),
			Truncated: res.Truncated,
			ElapsedMs: elapsedMs,
		},
	}
}

// parseClusterParams validates the query string. The handle form check happens
// BEFORE any DB roundtrip (fullUUIDRe, pattern handler/graph.go): a malformed id
// must never reach the uuid cast, where it would return SQLSTATE 22P02 → a 500
// that is itself a distinguishable answer.
func parseClusterParams(q url.Values) (store.ClusterEgoParams, error) {
	p := store.ClusterEgoParams{}

	p.Handle = q.Get("cluster")
	if p.Handle == "" {
		return p, errors.New("cluster parameter is required")
	}
	if !fullUUIDRe.MatchString(p.Handle) {
		return p, errors.New("cluster must be a full UUID")
	}

	var err error
	if p.Limit, err = egoIntParam(q, "limit", clusterDefaultLimit, 1, clusterMaxLimit); err != nil {
		return p, err
	}
	if p.NeighborLimit, err = egoIntParam(q, "neighbor_limit", clusterDefaultNeighborLimit, 1, clusterMaxNeighborLimit); err != nil {
		return p, err
	}
	return p, nil
}
