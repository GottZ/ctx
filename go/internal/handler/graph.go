// GET /api/graph/ego — scope-filtered k-hop ego subgraph (F5-W1).
//
// Read-only, no LLM/provider touch. Wire envelope per design §3.1: nodes
// without content, edges as response-local index tuples, fixed rels legend.
// 404 is identical for "does not exist" and "not visible" (no existence
// oracle); the access_log row (action='graph', block_id=NULL) is written only
// AFTER the focus visibility check succeeded.

package handler

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// GraphHandler handles GET /api/graph/ego.
type GraphHandler struct {
	pool       *pgxpool.Pool
	cfg        ConfigStore
	blocktypes *blocktype.Registry
}

// NewGraphHandler creates a new GraphHandler. The read rate limit comes from
// a config snapshot per request (F1-W7), not a boot copy. blocktypes feeds
// the per-request type-visibility allowlist (WF T6) — never the compiled-in
// builtin set (T5 DB-sourcing doctrine).
func NewGraphHandler(pool *pgxpool.Pool, cfg ConfigStore, blocktypes *blocktype.Registry) *GraphHandler {
	return &GraphHandler{pool: pool, cfg: cfg, blocktypes: blocktypes}
}

// Parameter ceilings (out-of-range → 400, never silently clamped).
const (
	egoDefaultHops      = 1
	egoMaxHops          = 3
	egoDefaultCap       = 25
	egoMaxCap           = 100
	egoDefaultLimit     = 500
	// egoMaxLimit was lowered from 5000 to 1500 by the G39/F5-W5 1M-synthetic
	// benchmark: at 5000 nodes the worst-case pipeline (hops=3/cap=100, a dense
	// region with ≥5 degree-10^4 hubs) ran p95 ~850ms — dominated by Q3 (per-node
	// visible-degree, which scales with the node count), then Q2, then the walk.
	// The node budget is the one knob that bounds all three phases. Sweep p95
	// (against the live host, with its real variance): 3000 ~510ms, 2500 ~450ms,
	// 2000 ~390–490ms (tail/max to ~610ms — crosses the 500ms bar under load),
	// 1500 ~290ms (max ~300ms — robustly clear, tail included). 1500 is the
	// largest ceiling that stays under the 500ms bar across runs, so it is the
	// one we offer ("nicht unbewiesen anbieten", design §5.3). The default (500)
	// and the client's incremental expand model are unaffected.
	// Evidence: .project/bench-graph/REPORT.md.
	egoMaxLimit         = 1500
	egoDefaultEdgeLimit = 4000
	egoMaxEdgeLimit     = 20000
)

// fullUUIDRe matches the canonical 36-char UUID shape, charset included —
// malformed ids are a 400 before any DB roundtrip.
var fullUUIDRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// egoResponse is the wire envelope — exactly the design §3.1 example shape.
// Store result and wire format must not drift apart silently (truncated lives
// in stats on the wire); the envelope golden test pins this.
type egoResponse struct {
	Success bool          `json:"success"`
	Focus   string        `json:"focus"`
	Params  egoParamsEcho `json:"params"`
	Rels    []string      `json:"rels"`
	// GA8 (design/02 §4.1): structural legends + edges are ADDITIVE and
	// unconditional arrays (never null) — old clients ignore them, the SPA
	// consumes them tolerantly since GA2. Empty until GB1–GB3 deliver.
	StructRels  []string                `json:"struct_rels"`
	Origins     []string                `json:"origins"`
	Nodes       []store.GraphNode       `json:"nodes"`
	Edges       []store.GraphEdge       `json:"edges"`
	StructEdges []store.StructGraphEdge `json:"structural_edges"`
	Stats       egoStats                `json:"stats"`
}

// egoParamsEcho echoes the validated parameters. A typed struct (not a map)
// keeps the key order deterministic and matching the §3.1 example.
type egoParamsEcho struct {
	Hops          int        `json:"hops"`
	PerNodeCap    int        `json:"per_node_cap"`
	Limit         int        `json:"limit"`
	MinConfidence float64    `json:"min_confidence"`
	LinkClass     []string   `json:"link_class"` // nil → null (= all five)
	EdgeLimit     int        `json:"edge_limit"`
	Category      []string   `json:"category,omitempty"`
	CreatedAfter  *time.Time `json:"created_after,omitempty"`
	CreatedBefore *time.Time `json:"created_before,omitempty"`
}

type egoStats struct {
	Nodes int `json:"nodes"`
	// Edges stays dream-only (E4) — existing consumers keep their semantics;
	// StructuralEdges is the separate fact-edge counter.
	Edges           int   `json:"edges"`
	StructuralEdges int   `json:"structural_edges"`
	Truncated       bool  `json:"truncated"`
	ElapsedMs       int64 `json:"elapsed_ms"`
}

// HandleEgo processes GET /api/graph/ego requests.
func (h *GraphHandler) HandleEgo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)

	// Auth from middleware context.
	authResult := AuthResultFromContext(ctx)
	if authResult == nil || !authResult.IsValid {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "unauthorized"})
		return
	}

	// Read rate limit check (0 = disabled) — own action bucket "graph". MT
	// 06-C5: per-tenant via the request context (tenant's own RateLimitRead
	// override, else _global).
	if limit := h.cfg.SnapshotForRequest(ctx).Query.RateLimitRead; limit > 0 {
		readCount, err := store.CheckRateLimitByAction(ctx, h.pool, authResult.ApiKeyID, "graph")
		if err != nil {
			slog.Error("graph: read rate limit check error", "error", err, "request_id", reqID)
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

	params, err := parseEgoParams(r.URL.Query())
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": err.Error()})
		return
	}

	start := time.Now()
	// grantedBlockIDs nil (T40a): the graph-ego handler is NOT live-wired for
	// block grants in T40a (EgoGraph inherits the OR centrally, but its grant
	// resolution is a later wave) — nil ⇒ no-op OR-arm.
	// T6: the type allowlist comes from the REGISTRY snapshot per request —
	// a live registry edit changes graph visibility without a restart.
	visibleTypes := h.blocktypes.SnapshotForRequest(ctx).VisibleTypes()
	result, err := store.EgoGraph(ctx, h.pool, params, authResult.ReadScopes, nil, visibleTypes)
	if err != nil {
		if errors.Is(err, store.ErrNotVisible) {
			// One identical 404 for "does not exist" and "not visible" — no
			// existence oracle. NO access_log entry on this path: invisible
			// blocks must not be telemetrically bumpable via UUID probing.
			writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "Block not found"})
			return
		}
		slog.Error("graph: ego query error", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "Internal server error"})
		return
	}
	elapsedMs := time.Since(start).Milliseconds()

	// Telemetry strictly AFTER the successful visibility check (design §2.7).
	// Best effort: a failed audit insert must not fail a completed read.
	if err := store.LogGraphAccess(ctx, h.pool, authResult.ApiKeyID, result.Focus, params.Hops, params.Limit, len(result.Nodes)); err != nil {
		slog.Error("graph: access log error", "error", err, "request_id", reqID)
	}

	writeJSON(w, http.StatusOK, buildEgoResponse(result, params, elapsedMs))
}

// buildEgoResponse assembles the wire envelope from the store result and the
// validated params. Pure — pinned by the envelope golden unit test.
func buildEgoResponse(res *store.EgoResult, p store.EgoParams, elapsedMs int64) egoResponse {
	nodes := res.Nodes
	if nodes == nil {
		nodes = []store.GraphNode{}
	}
	edges := res.Edges
	if edges == nil {
		edges = []store.GraphEdge{}
	}
	structRels := res.StructRels
	if structRels == nil {
		structRels = []string{}
	}
	origins := res.Origins
	if origins == nil {
		origins = []string{}
	}
	structEdges := res.StructEdges
	if structEdges == nil {
		structEdges = []store.StructGraphEdge{}
	}
	return egoResponse{
		Success: true,
		Focus:   res.Focus,
		Params: egoParamsEcho{
			Hops:          p.Hops,
			PerNodeCap:    p.PerNodeCap,
			Limit:         p.Limit,
			MinConfidence: p.MinConfidence,
			LinkClass:     p.LinkClasses,
			EdgeLimit:     p.EdgeLimit,
			Category:      p.Categories,
			CreatedAfter:  p.CreatedAfter,
			CreatedBefore: p.CreatedBefore,
		},
		Rels:        res.Rels,
		StructRels:  structRels,
		Origins:     origins,
		Nodes:       nodes,
		Edges:       edges,
		StructEdges: structEdges,
		Stats: egoStats{
			Nodes:           len(res.Nodes),
			Edges:           len(res.Edges),
			StructuralEdges: len(res.StructEdges),
			Truncated:       res.Truncated,
			ElapsedMs:       elapsedMs,
		},
	}
}

// parseEgoParams validates the query string into store.EgoParams. Ceilings
// are enforced, not clamped: out-of-range values are an explicit 400 with a
// plain-text reason (design §3.1).
func parseEgoParams(q url.Values) (store.EgoParams, error) {
	p := store.EgoParams{}

	p.Focus = q.Get("block")
	if p.Focus == "" {
		return p, errors.New("block parameter is required")
	}
	if !fullUUIDRe.MatchString(p.Focus) {
		return p, errors.New("block must be a full UUID")
	}

	var err error
	if p.Hops, err = egoIntParam(q, "hops", egoDefaultHops, 1, egoMaxHops); err != nil {
		return p, err
	}
	if p.PerNodeCap, err = egoIntParam(q, "per_node_cap", egoDefaultCap, 1, egoMaxCap); err != nil {
		return p, err
	}
	if p.Limit, err = egoIntParam(q, "limit", egoDefaultLimit, 1, egoMaxLimit); err != nil {
		return p, err
	}
	if p.EdgeLimit, err = egoIntParam(q, "edge_limit", egoDefaultEdgeLimit, 1, egoMaxEdgeLimit); err != nil {
		return p, err
	}
	if p.MinConfidence, err = egoFloatParam(q, "min_confidence", 0, 0, 1); err != nil {
		return p, err
	}
	if p.LinkClasses, err = egoLinkClasses(q.Get("link_class")); err != nil {
		return p, err
	}
	p.Categories = egoCSV(q.Get("category"))
	if p.CreatedAfter, err = egoTimeParam(q, "created_after"); err != nil {
		return p, err
	}
	if p.CreatedBefore, err = egoTimeParam(q, "created_before"); err != nil {
		return p, err
	}

	return p, nil
}

// egoIntParam parses an optional integer query parameter with inclusive
// bounds. Absent or empty → def.
func egoIntParam(q url.Values, name string, def, minVal, maxVal int) (int, error) {
	raw := q.Get(name)
	if raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	if v < minVal || v > maxVal {
		return 0, fmt.Errorf("%s must be between %d and %d (got %d)", name, minVal, maxVal, v)
	}
	return v, nil
}

// egoFloatParam parses an optional float query parameter with inclusive
// bounds. Absent or empty → def.
func egoFloatParam(q url.Values, name string, def, minVal, maxVal float64) (float64, error) {
	raw := q.Get(name)
	if raw == "" {
		return def, nil
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be a number", name)
	}
	if v < minVal || v > maxVal {
		return 0, fmt.Errorf("%s must be between %g and %g (got %g)", name, minVal, maxVal, v)
	}
	return v, nil
}

// egoTimeParam parses an optional RFC3339 timestamp query parameter.
func egoTimeParam(q url.Values, name string) (*time.Time, error) {
	raw := q.Get(name)
	if raw == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, fmt.Errorf("%s must be an RFC3339 timestamp", name)
	}
	return &t, nil
}

// egoLinkClasses parses the link_class CSV against the fixed legend; an
// unknown class is a 400. Empty → nil (= all five).
func egoLinkClasses(raw string) ([]string, error) {
	classes := egoCSV(raw)
	if classes == nil {
		return nil, nil
	}
	known := make(map[string]bool, len(store.GraphRels))
	for _, r := range store.GraphRels {
		known[r] = true
	}
	for _, c := range classes {
		if !known[c] {
			return nil, fmt.Errorf("unknown link_class %q (valid: %s)", c, strings.Join(store.GraphRels, ","))
		}
	}
	return classes, nil
}

// egoCSV splits a comma-separated value, trimming whitespace and dropping
// empty items. Empty input → nil.
func egoCSV(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
