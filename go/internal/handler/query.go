package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/embed"
	"github.com/GottZ/ctx/internal/embedcache"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/rrf"
	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ConfigStore is the snapshot source for request-scoped configuration.
// *config.Store implements it; tests substitute a counting fake to pin the
// one-snapshot-per-request invariant.
type ConfigStore interface {
	Snapshot() *config.Config
}

var _ ConfigStore = (*config.Store)(nil)

// QueryHandler handles POST /api/query. It holds no configuration values —
// every request takes ONE snapshot from the store (F1-W4), so the heartbeat
// gate, the rerank dispatch and every backend tuple read the same generation
// by construction, and a config replace is live from the next request on.
type QueryHandler struct {
	pool *pgxpool.Pool
	cfg  ConfigStore
}

// NewQueryHandler creates a new QueryHandler.
func NewQueryHandler(pool *pgxpool.Pool, cfg ConfigStore) *QueryHandler {
	return &QueryHandler{pool: pool, cfg: cfg}
}

// queryRequest is the JSON body for the query endpoint.
type queryRequest struct {
	Query    string   `json:"query"`
	Category *string  `json:"category,omitempty"`
	Tags     []string `json:"tags,omitempty"`
	Limit    *int     `json:"limit,omitempty"`
	// v2.0.0 C2 (M048): optional exclude-lists. Trigger: CRAG-Bench
	// topic-map-private slot-stealing in 4/10 Variant-A movie queries
	// (Session 38c). Empty slice / omitted = no exclude.
	CategoriesExclude []string `json:"categories_exclude,omitempty"`
	BlockRolesExclude []string `json:"block_roles_exclude,omitempty"`
	// Synthesize gates the LLM answer step. nil/true = full pipeline (default);
	// false = retrieval-only: return the post-rerank sources without the LLM
	// synthesis call. Deterministic + fast, for the A/B sweep / eval harness.
	Synthesize *bool `json:"synthesize,omitempty"`
}

// queryResponse is the JSON response from the query endpoint.
type queryResponse struct {
	Success    bool             `json:"success"`
	Answer     string           `json:"answer"`
	Sources    []sourceResponse `json:"sources"`
	Confidence string           `json:"confidence"`
	Model      string           `json:"model,omitempty"`
	EvalCount  int              `json:"eval_count,omitempty"`
	Translated bool             `json:"translated"`
}

// sourceResponse is a single source in the query response.
type sourceResponse struct {
	ID               string   `json:"id"`
	Title            string   `json:"title"`
	Category         string   `json:"category"`
	Score            float64  `json:"score"`
	AgeDays          int      `json:"age_days"`
	RerankScore      *float64 `json:"rerank_score,omitempty"`
	RRFScoreOriginal *float64 `json:"rrf_score_original,omitempty"`
	SupersededBy     *string  `json:"superseded_by,omitempty"`
}

// buildSourceResponses maps retrieval sources to the API response shape,
// attaching superseded_by where the supersedes map has an entry. Shared by the
// full-synthesis path (Step 10) and the retrieval-only path (Step 7b).
func buildSourceResponses(sources []llm.Source, supersedesMap map[string][]string) []sourceResponse {
	out := make([]sourceResponse, len(sources))
	for i, s := range sources {
		out[i] = sourceResponse{
			ID:               s.ID,
			Title:            s.Title,
			Category:         s.Category,
			Score:            s.Score,
			AgeDays:          s.AgeDays,
			RerankScore:      s.RerankScore,
			RRFScoreOriginal: s.RRFScoreOriginal,
		}
		if supersedesMap != nil {
			if sl, ok := supersedesMap[s.ID]; ok && len(sl) > 0 {
				first := sl[0]
				out[i].SupersededBy = &first
			}
		}
	}
	return out
}

// queryHeartbeat keeps a long-running query alive through a buffering reverse
// proxy. With the cross-encoder reranker engaged a query runs ~80s, past a
// typical 60s proxy_read_timeout -> 504. 1xx informational responses do NOT
// survive nginx (proxy_buffering swallows them, the body is lost — measured); a
// chunked body heartbeat does: each flushed byte is a real upstream read that
// resets the timeout. We commit the 200 header up front and emit one space every
// ~25s (leading whitespace is valid JSON per RFC 8259, so the final body still
// decodes) until finish() writes the real payload. Gated to the slow rerank path
// only; the fast path keeps plain writeJSON and its non-200 error statuses.
// heartbeatInterval is the keepalive cadence — well under a typical 60s proxy
// read timeout. A package var so tests can shrink it.
var heartbeatInterval = 25 * time.Second

// heartbeatWriteWindow is the rolling write-deadline budget per tick. The
// global http.Server WriteTimeout (main.go) is an ABSOLUTE response deadline —
// it killed the >120s CPU-fallback path mid-flight (context canceled, client
// got only the tick bytes). While the heartbeat ticks, liveness is proven, so
// every tick pushes the connection write deadline one window ahead instead.
// 90s = ~3 missed ticks before the server gives up on a dead connection.
var heartbeatWriteWindow = 90 * time.Second

type queryHeartbeat struct {
	w      http.ResponseWriter
	rc     *http.ResponseController
	mu     sync.Mutex
	done   bool
	active bool
}

// startHeartbeat commits the 200 header and (when active) starts the keepalive
// goroutine. Call exactly one finish() afterwards.
func startHeartbeat(w http.ResponseWriter, active bool) *queryHeartbeat {
	hb := &queryHeartbeat{w: w, active: active}
	if !active {
		return hb
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	hb.rc = http.NewResponseController(w)
	if err := hb.rc.Flush(); err != nil {
		// Streaming is broken — likely a middleware ResponseWriter wrapper
		// without Unwrap(), so the controller cannot reach the real Flusher.
		// The heartbeat then buffers to the end of the response and a reverse
		// proxy will still 504. Loud here so the regression shows up in logs
		// instead of as silent buffering.
		slog.Warn("query heartbeat: response writer not flushable, keepalive ineffective",
			"error", err)
	}
	if err := hb.extendWriteDeadline(); err != nil {
		// Without deadline extension the server's absolute WriteTimeout caps
		// the response — long fallback syntheses (>120s) will be cut off.
		slog.Warn("query heartbeat: cannot extend write deadline, server WriteTimeout caps long responses",
			"error", err)
	}
	go hb.run()
	return hb
}

// extendWriteDeadline rolls the connection write deadline one window ahead.
// While the heartbeat ticks, liveness is proven — the absolute server
// WriteTimeout would otherwise kill the deliberately long rerank/fallback
// path mid-flight.
func (hb *queryHeartbeat) extendWriteDeadline() error {
	return hb.rc.SetWriteDeadline(time.Now().Add(heartbeatWriteWindow))
}

func (hb *queryHeartbeat) run() {
	t := time.NewTicker(heartbeatInterval)
	defer t.Stop()
	for range t.C {
		hb.mu.Lock()
		if hb.done {
			hb.mu.Unlock()
			return
		}
		_ = hb.extendWriteDeadline()
		_, err := io.WriteString(hb.w, " ")
		if err == nil {
			_ = hb.rc.Flush()
		}
		hb.mu.Unlock()
		if err != nil {
			return
		}
	}
}

// finish writes the final response exactly once. In heartbeat mode the 200 header
// is already committed, so status is ignored and v is encoded into the
// (whitespace-prefixed) body — a late failure can only carry success:false there.
// Without heartbeat it is a plain writeJSON(status, v). Mutex-serialized against
// the keepalive writes.
func (hb *queryHeartbeat) finish(status int, v any) {
	if !hb.active {
		writeJSON(hb.w, status, v)
		return
	}
	hb.mu.Lock()
	defer hb.mu.Unlock()
	hb.done = true
	_ = hb.extendWriteDeadline()
	_ = json.NewEncoder(hb.w).Encode(v)
	_ = hb.rc.Flush()
}

// HandleQuery orchestrates the full query pipeline:
// parse -> auth -> detect language -> translate -> embed -> RRF search ->
// filter -> confidence -> reorder -> synthesize -> access log -> respond.
//
//nolint:cyclop // complex HTTP handler with sequential pipeline stages
func (h *QueryHandler) HandleQuery(w http.ResponseWriter, r *http.Request) {
	// THE single snapshot of this request (F1-W4). Every stage below derives
	// from this one frozen generation — heartbeat gate and rerank dispatch can
	// never see two different config stands within one request.
	cfg := h.cfg.Snapshot()
	chat := cfg.ChatBackend()
	rerankCfg := cfg.RerankRRF()
	graphCfg := cfg.GraphRRF()

	ctx := r.Context()
	requestID := RequestIDFromContext(ctx)

	// Step 1: Parse JSON body.
	var req queryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		slog.Warn("invalid request body",
			"error", err,
			"request_id", requestID,
		)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}

	query := strings.TrimSpace(req.Query)
	if query == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "query is required"})
		return
	}

	// Query size limit (10 KB).
	if len(req.Query) > 10*1024 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "Query exceeds 10KB limit"})
		return
	}

	// Step 2: Auth (from middleware context).
	ar := AuthResultFromContext(ctx)
	if ar == nil || !ar.IsValid {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	// Read rate limit check (0 = disabled).
	if rateLimitRead := cfg.Query.RateLimitRead; rateLimitRead > 0 {
		readCount, err := store.CheckRateLimitByAction(ctx, h.pool, ar.ApiKeyID, "query")
		if err != nil {
			slog.Error("query: read rate limit check error", "error", err, "request_id", requestID)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "Internal server error"})
			return
		}
		if readCount >= rateLimitRead {
			writeJSON(w, http.StatusTooManyRequests, map[string]any{
				"success": false,
				"error":   fmt.Sprintf("Rate limit exceeded: max %d reads per 60 seconds", rateLimitRead),
			})
			return
		}
	}

	// Clamp limit: 1-20, default 5.
	limit := 5
	if req.Limit != nil {
		limit = *req.Limit
		if limit < 1 {
			limit = 1
		}
		if limit > 20 {
			limit = 20
		}
	}

	slog.Info("query pipeline start",
		"query", query,
		"request_id", requestID,
	)

	// Step 3: Detect German and translate if needed.
	originalQuery := query
	searchQuery := query
	translated := false

	if llm.DetectGerman(query) {
		slog.Info("german detected, translating",
			"request_id", requestID,
		)
		translatedQuery, err := llm.TranslateQuery(ctx, chat, query)
		if err != nil {
			slog.Warn("translation failed, using original",
				"error", err,
				"request_id", requestID,
			)
		} else if translatedQuery != query {
			searchQuery = translatedQuery
			translated = true
			slog.Info("query translated",
				"original", query,
				"translated", searchQuery,
				"request_id", requestID,
			)
		}
	}

	// Step 3b: Temporal normalization.
	// PRIMARY: deterministic rule-based parser (0ms, no LLM call).
	// FALLBACK: LLM normalization (only when rules return nil but query has temporal intent).
	var temporal string
	var temporalResult *llm.TemporalResult

	now := time.Now().In(cfg.Query.Timezone)
	temporalResult = llm.NormalizeTemporalRules(originalQuery, now)

	if temporalResult != nil {
		temporal = llm.TemporalToFTSExpansion(temporalResult.Dates)
		slog.Info("temporal normalization (rule-based)",
			"dates", temporalResult.Dates,
			"fts_terms", temporal,
			"request_id", requestID,
		)
	} else if llm.HasTemporalIntent(originalQuery) {
		// LLM fallback: query seems temporal but rules couldn't parse it.
		var err error
		temporalResult, err = llm.NormalizeTemporal(ctx, chat, originalQuery, now)
		if err != nil {
			slog.Warn("temporal LLM fallback failed, no temporal expansion available",
				"error", err,
				"request_id", requestID,
			)
		} else if temporalResult != nil {
			temporal = llm.TemporalToFTSExpansion(temporalResult.Dates)
			slog.Info("temporal normalization (LLM fallback)",
				"dates", temporalResult.Dates,
				"fts_terms", temporal,
				"request_id", requestID,
			)
		}
	}

	// Step 3c: Temporal prefix for embedding augmentation — DISABLED.
	// T06: Embed prefix empirically worsens 4/5 queries (33% centroid shift).
	// Temporal relevance handled by FTS expansion, Dimension Table, and Gravity Boost.
	embedQuery := searchQuery

	// Step 3d: Backfill any blocks with missing embeddings before searching.
	// Ensures freshly stored blocks are immediately searchable.
	embedB := cfg.EmbedBackend()
	if backfilled := h.backfillPending(ctx, embedB); backfilled > 0 {
		slog.Info("query: backfilled embeddings before search", "count", backfilled, "request_id", requestID)
	}

	// Step 4: Embed the search query with query prefix. Cached by (hash(prefix||text), model) —
	// repeated queries (debug sessions, recurring lookups) serve from cache in a single UPDATE.
	embedding, err := embedcache.Embed(ctx, h.pool, embedB, embedQuery, embed.PrefixQuery)
	if err != nil {
		slog.Error("embedding failed",
			"error", err,
			"request_id", requestID,
		)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "embedding failed"})
		return
	}

	// Step 5: Hyphen preprocessing for FTS.
	querySpaced := strings.ReplaceAll(searchQuery, "-", " ")
	// OR query for broader FTS recall — infrastructure ready (Migration 018, BuildORQuery),
	// but disabled at 440 blocks: empirical dead weight is 20% (not 68%), OR matching
	// changes RRF score distribution and causes LLM synthesis regressions.
	// Activate when FTS dead weight exceeds 40% or scale reaches 10K+ blocks.
	queryOR := ""

	// Step 6: RRF Search via PG function.
	// Internal limit: fetch 200 candidates so the Go-side gravity reranker
	// and LLM reranker can re-score a broad set before truncation.
	internalLimit := 200
	if limit > internalLimit {
		internalLimit = limit // respect explicit large limits
	}

	// Welle 41 M039: query-aware audit-trail damping. Pattern-Detection in
	// rrf.AuditTrailFactor returns 1.0 for explicit-target queries (session/
	// welle/audit/recurrent/handover/self-audit), 0.3 for generic queries.
	auditTrailFactor := rrf.AuditTrailFactor(req.Query)

	results, err := rrf.Search(ctx, h.pool, embedding, searchQuery, querySpaced, ar.ReadScopes, req.Category, req.Tags, internalLimit, temporal, queryOR, auditTrailFactor, req.CategoriesExclude, req.BlockRolesExclude)
	if err != nil {
		slog.Error("rrf search failed",
			"error", err,
			"request_id", requestID,
		)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "search failed"})
		return
	}

	slog.Info("rrf search complete",
		"result_count", len(results),
		"internal_limit", internalLimit,
		"user_limit", limit,
		"request_id", requestID,
	)

	// RRF (the last 500-capable stage) has succeeded. From here, when the
	// cross-encoder reranker is engaged the query runs ~80s — start the keepalive
	// heartbeat so a buffering reverse proxy doesn't time out. It commits the 200
	// header now; every later return goes through hb.finish (no more status codes).
	// Gate and dispatch (Step 6b) read the same rerankCfg local — one snapshot
	// stand by construction.
	useHeartbeat := rerankCfg.Enabled && rerankCfg.Host != ""
	hb := startHeartbeat(w, useHeartbeat)

	// Step 6a: Post-RRF temporal gravity boost (GottZ Cyclic Phase Model).
	//
	// Two paths based on TemporalResult.DimensionWeights:
	//  - Linear: Distance-only gravity on content_times (existing path).
	//  - Cyclic: Multi-dimensional Gaussian decay on EAV context_temporal rows.
	// Mixed weights (e.g. "am Dienstag" = {linear:0.6, weekday:0.4}) run both,
	// each scaled by its weight so total boost stays ≤0.30.
	if temporalResult != nil && len(temporalResult.Dates) > 0 {
		gravStart := time.Now()
		d := temporalResult.Dates[0]
		target, _ := time.Parse("2006-01-02", d.Date)
		// Time-of-day matchers (morgens/abends/etc.) set Hour for daily dimension phase.
		if d.Hour != nil {
			target = time.Date(target.Year(), target.Month(), target.Day(), *d.Hour, 0, 0, 0, target.Location())
		}
		cutoff := 14
		if d.End != nil {
			cutoff = 60
		}

		// Collect dimension weights (linear vs cyclic).
		dimWeights := temporalResult.DimensionWeights
		linearWeight := 1.0 // backward-compat: if no DimensionWeights, assume pure linear
		cyclicDims := []string{}
		cyclicWeightSum := 0.0
		if dimWeights != nil {
			linearWeight = dimWeights["linear"]
			for dim, w := range dimWeights {
				if dim != "linear" && w > 0 {
					cyclicDims = append(cyclicDims, dim)
					cyclicWeightSum += w
				}
			}
		}

		ids := make([]string, len(results))
		for i, r := range results {
			ids[i] = r.ID
		}

		const maxBoost = 0.30

		// Cyclic path: fetch EAV dimensions and apply multi-dim Gaussian decay.
		if len(cyclicDims) > 0 {
			blockDims, err := store.FetchBlockDimensions(ctx, h.pool, ids, cyclicDims)
			if err != nil {
				slog.Warn("cyclic gravity: fetch dimensions failed, skipping",
					"error", err,
					"request_id", requestID,
				)
			} else {
				// Convert store.BlockDimension → rrf.TemporalDim
				rrfDims := make(map[string][]rrf.TemporalDim, len(blockDims))
				for blockID, dims := range blockDims {
					td := make([]rrf.TemporalDim, len(dims))
					for i, bd := range dims {
						td[i] = rrf.TemporalDim{Dimension: bd.Dimension, Value: bd.Value}
					}
					rrfDims[blockID] = td
				}
				results = rrf.ApplyCyclicGravityBoost(results, rrfDims, dimWeights, target, maxBoost*cyclicWeightSum)
				slog.Info("cyclic gravity scoring",
					"cyclic_dims", cyclicDims,
					"cyclic_weight_sum", cyclicWeightSum,
					"blocks_with_dims", len(blockDims),
					"gravity_latency_ms", float64(time.Since(gravStart).Milliseconds()),
					"request_id", requestID,
				)
			}
		}

		// Linear path: distance-only gravity on content_times.
		if linearWeight > 0 {
			blockDates, err := store.FetchContentTimes(ctx, h.pool, ids)
			if err != nil {
				slog.Warn("linear gravity: fetch dates failed, skipping",
					"error", err,
					"request_id", requestID,
				)
			} else {
				results = rrf.ApplyGravityBoost(results, blockDates, rrf.GravityParams{
					TargetDate:  target,
					Direction:   d.Dir,
					Cutoff:      cutoff,
					Power:       1.5,
					BoostWeight: maxBoost * linearWeight,
				})
				slog.Info("linear gravity scoring",
					"linear_weight", linearWeight,
					"blocks_with_dates", len(blockDates),
					"request_id", requestID,
				)
			}
		}
	}

	// Step 6a-graph: Dream-graph expansion (GottZ Graph Expansion, Wave 1).
	// Post-RRF, post-gravity (so seeds enter sorted by boosted RRFScore) and
	// PRE-rerank (so the reranker is the final arbiter of any graph-introduced
	// neighbor). 1-hop-expands the top RRF seeds along the positive Dream link
	// types and fuses neighbors via a Go boost. Gated default-OFF; fail-open
	// (any error keeps the pre-expansion results).
	//
	// Independence: graph runs whether or not the reranker is on, so an A/B
	// sweep can isolate the graph effect from the rerank effect. When graph is
	// on but the reranker is off there is no LLM noise-filter behind the
	// synthetic neighbors — warn once, then proceed.
	if graphCfg.Enabled {
		if !rerankCfg.Enabled {
			slog.Warn("graph expansion active without reranker noise-filter",
				"request_id", requestID,
			)
		}
		if expanded, gerr := rrf.GraphExpand(ctx, h.pool, results, ar.ReadScopes, graphCfg); gerr != nil {
			slog.Warn("graph expand failed; using pre-expansion results",
				"error", gerr,
				"request_id", requestID,
			)
		} else {
			results = expanded
		}
	}

	// Step 6b: Rerank (skipped if disabled or fewer than 3 results). Dispatch on
	// RerankCfg.Host: set => the local cross-encoder sidecar (Wave 2, up to
	// MaxDocs=50 candidates, the final arbiter of graph-injected neighbors);
	// empty => the LLM-as-judge on the chat model (up to 15). Both fail open —
	// on error the pre-rerank order is kept.
	if rerankCfg.Enabled {
		if rerankCfg.Host != "" {
			results, err = rrf.RerankCrossEncoder(ctx, rerankCfg.Host, rerankCfg.APIKey, rerankCfg.Model, rerankCfg.MaxDocs, rerankCfg.BlendWeight, originalQuery, results)
		} else {
			results, err = rrf.Rerank(ctx, chat, originalQuery, results)
		}
		if err != nil {
			slog.Warn("rerank failed, using original order",
				"error", err,
				"request_id", requestID,
			)
		}
	}

	// Step 6c: Supersedes enrichment — look up which result blocks have been superseded.
	// Done before truncation so filterSuperseded can check the full result set.
	resultIDs := make([]string, len(results))
	for i, r := range results {
		resultIDs[i] = r.ID
	}
	supersedesMap, err := dream.SupersedesMap(ctx, h.pool, resultIDs)
	if err != nil {
		slog.Warn("supersedes lookup failed, skipping", "error", err, "request_id", requestID)
		supersedesMap = nil
	}

	// Step 6d: Filter superseded blocks from results.
	// Gate: only for non-temporal queries (temporal queries need historical blocks).
	// Safety: a superseded block is only removed if its superseder is also in results.
	if supersedesMap != nil && (temporalResult == nil || len(temporalResult.Dates) == 0) {
		results = filterSuperseded(results, supersedesMap)
	}

	// Step 6e: Truncate to user-facing limit after reranking and filtering.
	if len(results) > limit {
		results = results[:limit]
	}

	// Step 7: Convert RRF results to LLM source format.
	now = time.Now()
	sources := make([]llm.Source, len(results))
	for i, r := range results {
		ageDays := int(math.Floor(now.Sub(r.UpdatedAt).Hours() / 24))
		if ageDays < 0 {
			ageDays = 0
		}
		sources[i] = llm.Source{
			ID:               r.ID,
			Title:            r.Title,
			Category:         r.Category,
			Content:          r.Content,
			Score:            r.RRFScore,
			RerankScore:      r.RerankScore,
			RRFScoreOriginal: r.RRFScoreOriginal,
			AgeDays:          ageDays,
		}
	}

	// Step 7b: Retrieval-only mode (eval / sweep) — skip the LLM synthesis
	// (Steps 8-10) and return the post-rerank sources directly. Deterministic +
	// fast so an A/B sweep can score retrieval in isolation from synthesis
	// variance. Confidence is classified from the max RAW RRF score (the
	// threshold's scale), preferring RRFScoreOriginal where the reranker set it.
	if req.Synthesize != nil && !*req.Synthesize {
		maxScore := 0.0
		for _, s := range sources {
			raw := s.Score
			if s.RRFScoreOriginal != nil {
				raw = *s.RRFScoreOriginal
			}
			if raw > maxScore {
				maxScore = raw
			}
		}
		go h.logAccess(ar, results, query)
		slog.Info("query pipeline complete (retrieval-only)",
			"source_count", len(sources),
			"translated", translated,
			"request_id", requestID,
		)
		hb.finish(http.StatusOK, queryResponse{
			Success:    true,
			Sources:    buildSourceResponses(sources, supersedesMap),
			Confidence: llm.ClassifyConfidence(maxScore, cfg.SynthesisSettings()),
			Translated: translated,
		})
		return
	}

	// Step 8: Synthesize (filter, confidence, reorder, LLM call).
	// Uses originalQuery so the LLM answers in the user's language.
	// Temporal dates are passed for conditional date context in the synthesis prompt.
	var temporalDates []llm.TemporalDate
	if temporalResult != nil {
		temporalDates = temporalResult.Dates
	}
	synthResult, err := llm.Synthesize(ctx, h.pool, chat, cfg.ChatFallbackBackend(), cfg.SynthesisSettings(), originalQuery, sources, temporalDates)
	if err != nil {
		slog.Error("synthesis failed",
			"error", err,
			"request_id", requestID,
		)
		// In heartbeat mode the 200 is already committed -> success:false in body.
		hb.finish(http.StatusInternalServerError, map[string]any{"success": false, "error": "synthesis failed"})
		return
	}

	// Step 9: Access log (async, non-blocking).
	go h.logAccess(ar, results, query)

	// Step 10: Build response.
	respSources := buildSourceResponses(synthResult.Sources, supersedesMap)

	resp := queryResponse{
		Success:    true,
		Answer:     synthResult.Answer,
		Sources:    respSources,
		Confidence: synthResult.Confidence,
		Model:      synthResult.Model,
		EvalCount:  synthResult.EvalCount,
		Translated: translated,
	}

	// Set model even when LLM was skipped (for consistency).
	if resp.Model == "" {
		resp.Model = chat.Model
	}

	slog.Info("query pipeline complete",
		"confidence", resp.Confidence,
		"source_count", len(respSources),
		"eval_count", resp.EvalCount,
		"translated", translated,
		"request_id", requestID,
	)

	hb.finish(http.StatusOK, resp)
}

// logAccess inserts access log entries asynchronously.
// One row per block returned in the search results.
// Errors are logged but never surface to the caller.
func (h *QueryHandler) logAccess(ar *auth.AuthResult, results []rrf.SearchResult, queryText string) {
	if ar == nil || ar.ApiKeyID == "" || len(results) == 0 {
		return
	}

	// Use a background context with generous timeout — the request context may be cancelled.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for _, r := range results {
		metadata := map[string]interface{}{
			"score":  math.Round(r.RRFScore*10000) / 10000,
			"scope":  r.Scope,
			"source": "agent",
		}
		metaJSON, err := json.Marshal(metadata)
		if err != nil {
			slog.Warn("access log: marshal metadata failed",
				"error", err,
				"block_id", r.ID,
			)
			continue
		}

		_, err = h.pool.Exec(ctx,
			`INSERT INTO context_access_log (api_key_id, block_id, action, query_text, metadata)
			 VALUES ($1, $2, 'query', $3, $4)`,
			ar.ApiKeyID, r.ID, queryText, string(metaJSON),
		)
		if err != nil {
			slog.Warn("access log: insert failed",
				"error", err,
				"block_id", r.ID,
			)
		}
	}
}

// backfillPending generates embeddings for any blocks that don't have one yet.
// Called before search to ensure freshly stored blocks are immediately findable.
// Returns the number of blocks backfilled (0 in the common case).
// The embed tuple comes from the caller's request snapshot (F1-W4).
// Uses a TX with FOR UPDATE SKIP LOCKED per block to avoid races with the scheduler.
func (h *QueryHandler) backfillPending(ctx context.Context, embedB backends.Backend) int {
	count := 0
	for {
		var blockID, title, content string
		err := h.pool.QueryRow(ctx,
			`SELECT id, title, content FROM context_blocks
			WHERE embedding IS NULL AND NOT is_archived
			LIMIT 1`).Scan(&blockID, &title, &content)
		if err != nil {
			break // No more pending blocks (or error).
		}

		embedText := title + "\n\n" + content
		vec, err := embed.Embed(ctx, embedB, embedText, embed.PrefixDocument)
		if err != nil {
			slog.Warn("query backfill: embed failed", "block_id", blockID, "error", err)
			break // Embed backend likely unavailable, don't retry.
		}
		if err := store.StoreEmbedding(ctx, h.pool, blockID, vec); err != nil {
			slog.Warn("query backfill: store failed", "block_id", blockID, "error", err)
			break
		}
		count++
	}
	return count
}

// filterSuperseded removes blocks from results that are superseded by another block
// also present in the results. This ensures the LLM sees current information.
// Safety rule: a superseded block is only removed if its superseder is in the result set.
// Preserves RRF score ordering.
//
// Welle 46 Convention-Switch (2026-05-22): supersedesMap is now keyed by the
// OUTDATED block id with values = newer source ids that supersede it (English
// "A supersedes B" → key=B, value=[A]). The body of this function does not
// change because it already operated on the abstract "id → []supersederIDs"
// shape; the inversion lives in dream.SupersedesMap.
func filterSuperseded(results []rrf.SearchResult, supersedesMap map[string][]string) []rrf.SearchResult {
	if len(supersedesMap) == 0 {
		return results
	}

	// Build set of all result IDs for O(1) lookup.
	resultSet := make(map[string]bool, len(results))
	for _, r := range results {
		resultSet[r.ID] = true
	}

	filtered := make([]rrf.SearchResult, 0, len(results))
	for _, r := range results {
		supersederIDs, isSuperseded := supersedesMap[r.ID]
		if !isSuperseded {
			filtered = append(filtered, r)
			continue
		}

		// Only remove if at least one superseder is also in the results.
		supersederPresent := false
		for _, sid := range supersederIDs {
			if resultSet[sid] {
				supersederPresent = true
				break
			}
		}
		if supersederPresent {
			slog.Info("filterSuperseded: removed",
				"block_id", r.ID,
				"superseded_by", supersederIDs,
			)
		} else {
			filtered = append(filtered, r)
		}
	}
	return filtered
}
