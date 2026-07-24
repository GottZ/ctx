package handler

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/embed"
	"github.com/GottZ/ctx/internal/embedcache"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/rrf"
	"github.com/GottZ/ctx/internal/sensitivity"
	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ConfigStore is the snapshot source for request-scoped configuration.
// *config.Store implements it; tests substitute a counting fake to pin the
// one-snapshot-per-request invariant.
//
// MT 06-C5: SnapshotForRequest is the request-path entry — it derives the
// tenant scope INTERNALLY from the context (the request-scope hook over
// AuthResultFromContext), so a caller cannot point a request at a foreign
// tenant; there is no scope parameter to spoof (fail-closed by construction,
// §5.1). SnapshotForTenant takes an explicit scope and is for BACKGROUND
// iteration over the authoritative tenant list ONLY (it has no AuthResult);
// request handlers must use SnapshotForRequest. Both fall back to the base
// generation while the overlay/hook are unset, so single-tenant behavior is
// byte-identical.
type ConfigStore interface {
	Snapshot() *config.Config
	SnapshotForRequest(ctx context.Context) *config.Config
	SnapshotForTenant(ctx context.Context, tenantScope string) *config.Config
}

var _ ConfigStore = (*config.Store)(nil)

// QueryHandler handles POST /api/query. It holds no configuration values —
// every request takes ONE snapshot from the store (F1-W4), so the heartbeat
// gate, the rerank dispatch and every backend tuple read the same generation
// by construction, and a config replace is live from the next request on.
type QueryHandler struct {
	pool        *pgxpool.Pool
	cfg         ConfigStore
	backendPool *backends.Pool
	quota       *backends.QuotaAccountant
	blocktypes  *blocktype.Registry
	// admitter is the ONE process-wide dispatch admission layer (MW3, I-D1);
	// every non-stream LLM call of the query pipeline acquires through it.
	admitter dispatch.Admitter
}

// NewQueryHandler creates a new QueryHandler. backendPool feeds the
// synthesis chain (F3-P2) — Chain() is the only way to a backend tuple. quota
// enforces per-tenant cost/call budgets on the synthesis path (T36, 04-W4); it
// may be nil (the gate is then skipped — behavior-identical to pre-T36).
// blocktypes is the block-type registry (WF T4/T5): the retrieval type policy
// (visibility allowlist + damping + intent lift) resolves from a
// SnapshotForRequest per query — NEVER from the compiled-in builtin set (the
// live DB-sourcing probe would catch that). Must be non-nil for the retrieval
// path; tests that reach rrf inject their own registry instance.
// admitter is the dispatch admission layer (MW3); nil is tolerated for tests
// that never reach a resolved chain — a resolving LLM call then fails loudly
// in llm (I-D1: no unadmitted wire call), never silently passes through.
func NewQueryHandler(pool *pgxpool.Pool, cfg ConfigStore, backendPool *backends.Pool, quota *backends.QuotaAccountant, blocktypes *blocktype.Registry, admitter dispatch.Admitter) *QueryHandler {
	return &QueryHandler{pool: pool, cfg: cfg, backendPool: backendPool, quota: quota, blocktypes: blocktypes, admitter: admitter}
}

// admission binds the query pipeline's dispatch class (MW3/MW5, design/01
// §4.6 N1): EVERYTHING on this handler is interactive — a human waits
// synchronously on translate/temporal/rerank/synthesize AND on the embed
// path including the pre-search backfill (E-U5(a): backfill runs inside the
// request latency; the overtake relief is structural, per-attempt acquire in
// EmbedChain). The principal is NOT bound here (MW4, design/03 §4.1.1): the
// dispatcher derives it from the request ctx that flows into every acquire,
// via the boot-installed RequestPrincipal hook.
func (h *QueryHandler) admission() llm.Admission {
	return llm.Admission{
		Admitter: h.admitter,
		Class:    dispatch.ClassInteractive,
	}
}

// embedAdmission is admission in embedcache's mirror type (the embed chain
// does not import llm — same decoupling as embedcache.ReportFunc). The
// principal is ctx-derived since MW4, so the mirror carries class only.
func (h *QueryHandler) embedAdmission() embedcache.Admission {
	adm := h.admission()
	return embedcache.Admission{Admitter: adm.Admitter, Class: adm.Class}
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
	// TypesExclude (WF T10, seam 17) is the CANONICAL wire name for the
	// request-level type exclude (p_types_exclude since M073);
	// block_roles_exclude above stays as the documented legacy alias. Both
	// present ⇒ the UNION applies (monotone-restrictive, no silent
	// precedence).
	TypesExclude []string `json:"types_exclude,omitempty"`
	// Synthesize gates the LLM answer step. nil/true = full pipeline (default);
	// false = retrieval-only: return the post-rerank sources without the LLM
	// synthesis call. Deterministic + fast, for the A/B sweep / eval harness.
	Synthesize *bool `json:"synthesize,omitempty"`
	// Sensitivity classifies the QUERY TEXT for trust gating (F3 §2.3b).
	// Precedence: request > settings key pool.default_query_sensitivity
	// (initial personal — queries are user-typed and rarely carry secrets;
	// credentials would lock translate/synthesis failover for harmless
	// questions, decision F3-E2). MCP query inherits via delegation.
	Sensitivity string `json:"sensitivity,omitempty"`
	// IncludeContent attaches a content snippet (<= maxRetrievalSnippet chars)
	// to each source on the retrieval-only path (F6 ctx_query tool delegation).
	// Default false ⇒ eval.sh / A-B sweep responses are byte-for-byte unchanged.
	IncludeContent bool `json:"include_content,omitempty"`
}

// maxRetrievalSnippet caps the per-source content attached when include_content
// is set — the same window the synthesis prompt feeds the model.
const maxRetrievalSnippet = 1500

// queryResponse is the JSON response from the query endpoint.
type queryResponse struct {
	Success    bool             `json:"success"`
	Answer     string           `json:"answer"`
	Sources    []sourceResponse `json:"sources"`
	Confidence string           `json:"confidence"`
	Model      string           `json:"model,omitempty"`
	EvalCount  int              `json:"eval_count,omitempty"`
	Translated bool             `json:"translated"`
	// ActivatedDimWeights exposes the temporal dimension weights the gravity
	// boost ran with (GottZ Cyclic Phase Model) — the eval-cyclic dim_weight_pass
	// assert reads this field. Omitted when the query had no temporal treatment.
	ActivatedDimWeights map[string]float64 `json:"activated_dim_weights,omitempty"`
}

// collectDimWeights splits DimensionWeights into the linear share and the
// cyclic dimension list feeding the gravity boost (Step 6a). nil map =
// backward-compat pure linear. Keys outside rrf.DimensionSigma (e.g. "year",
// which the stale vocabulary once listed) are SKIPPED: ComputeCyclicGravity
// would ignore them anyway (contribution 0), but their weight would still
// inflate cyclicWeightSum and with it the boost budget
// maxBoost*cyclicWeightSum — breaking the ≤0.30 invariant (design 01 R5).
// Allowlist truth is rrf.DimensionSigma; the llm package cannot import it
// (rrf imports llm), so this consumer-side check is the vocabulary gate —
// defense in depth behind the deterministic derivation (D-B, A-W2).
func collectDimWeights(dimWeights map[string]float64) (linearWeight float64, cyclicDims []string, cyclicWeightSum float64) {
	linearWeight = 1.0 // backward-compat: if no DimensionWeights, assume pure linear
	if dimWeights == nil {
		return linearWeight, nil, 0
	}
	linearWeight = dimWeights["linear"]
	for dim, w := range dimWeights {
		if dim == "linear" || w <= 0 {
			continue
		}
		if _, ok := rrf.DimensionSigma[dim]; !ok {
			slog.Warn("cyclic gravity: unknown dimension key skipped (vocabulary is rrf.DimensionSigma)",
				"dimension", dim, "weight", w)
			continue
		}
		cyclicDims = append(cyclicDims, dim)
		cyclicWeightSum += w
	}
	return linearWeight, cyclicDims, cyclicWeightSum
}

// activatedDimWeights mirrors the gravity-boost weight resolution (Step 6a):
// nil result or no dates → no temporal treatment (nil); dates without
// DimensionWeights → the backward-compat pure-linear default; otherwise the
// weights as parsed. Kept as a function so the eval contract is unit-testable.
func activatedDimWeights(tr *llm.TemporalResult) map[string]float64 {
	if tr == nil || len(tr.Dates) == 0 {
		return nil
	}
	if tr.DimensionWeights == nil {
		return map[string]float64{"linear": 1.0}
	}
	return tr.DimensionWeights
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
	Content          string   `json:"content,omitempty"` // only when include_content (F6 ctx_query)
}

// buildSourceResponses maps retrieval sources to the API response shape,
// attaching superseded_by where the supersedes map has an entry. Shared by the
// full-synthesis path (Step 10) and the retrieval-only path (Step 7b).
func buildSourceResponses(sources []llm.Source, supersedesMap map[string][]string, includeContent bool) []sourceResponse {
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
		if includeContent && s.Content != "" {
			if r := []rune(s.Content); len(r) > maxRetrievalSnippet {
				out[i].Content = string(r[:maxRetrievalSnippet])
			} else {
				out[i].Content = s.Content
			}
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
	// never see two different config stands within one request. MT 06-C5: the
	// request-path entry resolves the caller's per-tenant generation from the
	// context (tenant scope from the auth result, never from the body); it is
	// byte-identical to the base while no tenant override exists.
	cfg := h.cfg.SnapshotForRequest(r.Context())
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

	// Query sensitivity: request > setting (F3 §2.3b). Feeds every chain
	// resolution on this path — translate/temporal/embed are Q-only, rerank
	// and synthesis max it with their block sets.
	querySens := cfg.Pool.DefaultQuerySensitivity
	if req.Sensitivity != "" {
		s := backends.Sensitivity(req.Sensitivity)
		if !backends.ValidSensitivity(s) {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"success": false, "error": "Invalid sensitivity: must be credentials|personal|internal|public",
			})
			return
		}
		querySens = s
	}

	// G40 credentials detector: a query carrying a secret must never reach a
	// lower-trust backend — raise to credentials regardless of the requested or
	// default level (upgrade-only). The reason goes to the log, never the match.
	if m, hit := sensitivity.Scan(query); hit {
		querySens = backends.MaxSensitivity(querySens, backends.SensCredentials)
		slog.Warn("query: credentials pattern in query text — sensitivity raised to credentials",
			"kind", m.Kind, "request_id", requestID)
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
		translatedQuery, err := llm.TranslateQuery(ctx, h.pool, h.backendPool, querySens, query, ar.ApiKeyID, h.admission())
		if err != nil {
			// Fail-open (design 03 §2.4 translate row) — covers the empty
			// chain (trust/profile/disabled) AND exhausted attempts alike.
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
		temporalResult, err = llm.NormalizeTemporal(ctx, h.pool, h.backendPool, querySens, originalQuery, now, ar.ApiKeyID, h.admission())
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
	// Ensures freshly stored blocks are immediately searchable. Per block the
	// chain resolves with THAT block's floor-adjusted sensitivity (F3 §2.3
	// gate table, embed-backfill row).
	floor := cfg.Pool.ScopeSensitivityFloor
	if backfilled := h.backfillPending(ctx, floor, ar.HomeScope, h.embedAdmission()); backfilled > 0 {
		slog.Info("query: backfilled embeddings before search", "count", backfilled, "request_id", requestID)
	}

	// Step 4: Embed the search query with query prefix. Cached by (hash(prefix||text), model) —
	// repeated queries (debug sessions, recurring lookups) serve from cache in a single UPDATE.
	// The chain resolves with the query sensitivity; empty/exhausted chain
	// keeps today's semantics: query 500, embed is health-mandatory.
	embChain, err := h.backendPool.Chain(backends.RoleEmbed, querySens, ar.HomeScope)
	if err != nil {
		slog.Error("embedding failed: no eligible backend", "error", err, "request_id", requestID)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "embedding failed"})
		return
	}
	embedding, embServed, embAttempts, embWired, err := embedcache.EmbedChain(
		ctx, h.pool, embChain, backends.RoleEmbed, embedQuery, embed.PrefixQuery,
		embedcache.ReportFunc(llm.PoolReporter(h.backendPool)), h.embedAdmission())
	if embWired {
		// Slim row per actual wire call — cache hits are no egress (§2.7.3).
		// MW11: duration/queue_wait derive from the attempts inside
		// LogEmbedWire (§4.4a) — no caller-side clock spanning the acquires.
		h.logEmbedWire(ctx, "query-embed", querySens, embServed, embAttempts, nil, err, ar.ApiKeyID)
	}
	if err != nil {
		if dispatch.IsRejection(err) {
			// Capacity rejection at the embed step (design/03 §4.5.2 embed row):
			// map to a generic 429 + B1 Retry-After instead of the 500 that
			// poisons client retry/alerting. This site runs BEFORE the heartbeat
			// commits the 200, so the header travels on the wire. Body stays
			// B6-generic (§3.3): no target, no depth.
			setRejectRetryAfter(w, h.admitter, err)
			slog.Warn("query embed rejected by dispatcher", "error", err, "request_id", requestID)
			writeJSON(w, http.StatusTooManyRequests, rejectionBody())
			return
		}
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

	// WF T5 (M073, design/01 §3.5 + §4.4 #3): the retrieval type policy comes
	// from the REGISTRY snapshot per request — visibility allowlist +
	// query-aware damping arrays (intent lift generalises Welle 41 M039's
	// audit-trail scalar). NEVER the compiled-in builtin set: a live registry
	// edit (damping/patterns) must change this ranking without a restart
	// (DB-sourcing probe; the wiring is structurally pinned by
	// TestQueryRetrievalWiring).
	typeSet := h.blocktypes.SnapshotForRequest(ctx)
	visibleTypes := typeSet.VisibleTypes()
	dampedTypes, dampedFactors := typeSet.DampedTypesFor(req.Query)

	// T40b (design/07 §4.2): resolve the caller's block-grant set ONCE and feed
	// it into both the RRF retrieval OR-arm and the downstream GraphExpand. Same
	// fail-closed helper as the MCP paths (resolveGrants): a resolver error logs
	// and yields an empty set → scope-only retrieval, never a crash and never a
	// widen to full access.
	grantedBlockIDs := resolveGrants(ctx, h.pool, ar)

	// Aggregate-to-parent over-fetch (Achse-02 I-E, design/02 §4.4): the fold
	// COLLAPSES rows (N comments of one issue ⇒ one issue row), so a fixed fetch
	// truncated to the user limit can under-fill and under-diversify when a scope
	// carries many comments per issue (10k+ comments/repo). When the scope
	// actually holds an aggregating-type block, fetch a wider candidate window so
	// enough DISTINCT parents survive the collapse to fill the limit; the
	// per-parent cap in the fold keeps one hot thread from monopolising that
	// window. Gated on real presence (a single EXISTS probe) — a corpus without
	// comments takes the base 200 unchanged, keeping eval.sh byte-identical.
	internalLimit = h.aggregateOverFetchLimit(ctx, internalLimit, typeSet.AggregateTypes(), ar.ReadScopes)

	// Wire compat (seam 17, closed in WF T10): types_exclude is the canonical
	// wire name for the request-level p_types_exclude; block_roles_exclude
	// stays as the documented legacy alias — both present ⇒ union.
	results, err := rrf.Search(ctx, h.pool, embedding, searchQuery, querySpaced, ar.ReadScopes, req.Category, req.Tags, internalLimit, temporal, queryOR, visibleTypes, dampedTypes, dampedFactors, req.CategoriesExclude, unionExcludes(req.TypesExclude, req.BlockRolesExclude), grantedBlockIDs)
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

	// RRF (the last 500-capable stage) has succeeded. From here the keepalive
	// heartbeat runs UNCONDITIONALLY for the synthesis path (X4, F3-P2): with
	// pool chains ANY synthesis can exceed 60s (the CPU link runs minutes, a
	// failover attempt adds latency) — the old rerank-only gate left the
	// CPU-failover-without-rerank case unprotected against a 60s proxy. It
	// commits the 200 header now; every later return goes through hb.finish
	// (errors after commit become success:false in a 200 body). Retrieval-only
	// requests stay heartbeat-free (fast, no LLM in the path).
	useHeartbeat := req.Synthesize == nil || *req.Synthesize
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
		linearWeight, cyclicDims, cyclicWeightSum := collectDimWeights(dimWeights)

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
		// T40b (design/07 §4.2/§4.5): the query/RRF path is now live-wired for
		// block grants — the same resolved grantedBlockIDs that fed rrf.Search
		// flow into GraphExpand. T41 (already built) makes a grant-only block a
		// LEAF (visible via the neighbor OR-arm, never re-seeded), so passing the
		// grant set here surfaces granted neighbors without traversing through them.
		// T6: the SAME per-request visibleTypes allowlist that fed rrf.Search
		// gates the neighbor hydrate — one policy source per request.
		if expanded, gerr := rrf.GraphExpand(ctx, h.pool, results, ar.ReadScopes, grantedBlockIDs, visibleTypes, graphCfg); gerr != nil {
			slog.Warn("graph expand failed; using pre-expansion results",
				"error", gerr,
				"request_id", requestID,
			)
		} else {
			results = expanded
		}
	}

	// Step 6-fold: aggregate-to-parent fold (design/01 §4.6, WF T11). A block of
	// an aggregating type (retrieval.policy=aggregate-to-parent) is folded onto
	// its structural parent — the response carries the parent, not the child
	// (Comment→Issue). Placed BEFORE the sensitivity annotation on purpose: a
	// hydrated parent then flows through sensitivity/rerank/supersedes/
	// filterSuperseded via the existing machinery instead of carrying a
	// zero-value (credentials, over-blocking) sensitivity. Fast-path: no
	// aggregating type registered ⇒ zero DB calls ⇒ eval baseline byte-identical.
	results = h.foldAggregates(ctx, results, typeSet.AggregateTypes(), visibleTypes, ar.ReadScopes, grantedBlockIDs, requestID)

	// Step 6a-sens: batch sensitivity lookup over ALL candidate IDs — not
	// top-N: filterSuperseded (6d) and graph placement advance stragglers
	// from beyond any top-N window into the final llmSources; their zero
	// value would silently act as credentials (over-blocking) or, without
	// the unknown rule, as public (a downgrade inside the gate). Lookup
	// breadth ≠ gate breadth: this ANNOTATES every candidate, the synthesis
	// gate MEASURES only the final set (F3 §2.3). Lookup miss ⇒ credentials;
	// a failed lookup leaves all zero values = credentials (fail-closed,
	// local full-trust backends keep serving).
	sensIDs := make([]string, len(results))
	for i := range results {
		sensIDs[i] = results[i].ID
	}
	// T43 (design/07 §5.4): grant-mediated results get a GRANTEE-side egress floor.
	// Resolve the grantee tenant's strictest floor ONCE — only when grants are in
	// play (no grants ⇒ no result has a scope outside readScopes ⇒ the floor is
	// never consulted ⇒ the lookup is skipped and the path stays byte-identical).
	granteeFloor := backends.SensPublic
	if len(grantedBlockIDs) > 0 {
		granteeFloor = h.granteeScopeFloor(ctx, ar.TenantID, floor)
	}
	if sensMap, serr := store.FetchSensitivities(ctx, h.pool, sensIDs); serr != nil {
		slog.Warn("sensitivity lookup failed; candidates act as credentials",
			"error", serr, "request_id", requestID)
	} else {
		annotateSensitivities(results, sensMap, floor, ar.ReadScopes, granteeFloor)
	}

	// Step 6b: Rerank (skipped if disabled or fewer than 3 results). Dispatch
	// on the pool routing table (F3 §2.5): a backend carrying the rerank role
	// => cross-encoder sidecar (up to MaxDocs=50 candidates, the final
	// arbiter of graph-injected neighbors); role absent => the LLM-as-judge
	// over the synthesis chain (up to 15). Both fail open — on error or empty
	// chain the pre-rerank order is kept (the judge is a configuration
	// alternative, not a failover target).
	if rerankCfg.Enabled {
		if h.backendPool.RoleConfigured(backends.RoleRerank) {
			maxDocs := rerankCfg.MaxDocs
			if maxDocs <= 0 {
				maxDocs = rrf.RerankCrossEncoderMaxDocs
			}
			required := rerankRequired(querySens, results, maxDocs)
			if chain, cerr := h.backendPool.Chain(backends.RoleRerank, required, ar.HomeScope); cerr != nil {
				slog.Warn("rerank chain empty, using original order",
					"error", cerr, "request_id", requestID)
			} else {
				b := chain[0]
				model := b.ModelFor(backends.RoleRerank).Model
				rrStart := time.Now()
				reranked, rerankTel, rerr := rrf.RerankCrossEncoder(ctx, b.Host, b.APIKey, model, maxDocs, rerankCfg.BlendWeight, originalQuery, results, h.admission())
				if llm.IsAdmissionError(rerr) {
					// Acquire-error doctrine (MW5, design/01 §4.3): a rejected
					// admission is NOT an attempt — no Classify, no health
					// report, no llmlog row. The stage stays fail-open on the
					// pre-rerank order.
					slog.Warn("rerank admission rejected, using original order",
						"error", rerr, "request_id", requestID)
				} else {
					if rerr == nil {
						results = reranked
						h.backendPool.ReportSuccess(b.ID)
					} else {
						h.backendPool.ReportFailure(b.ID, backends.Classify(rerr, b.ProviderClass), 0)
						slog.Warn("rerank failed, using original order",
							"error", rerr, "request_id", requestID)
					}
					docCount := len(results)
					if docCount > maxDocs {
						docCount = maxDocs
					}
					llmlog.Record(h.pool, rerankLogEntry(model, b, required,
						sensIDs[:docCount], rerankTel, time.Since(rrStart), rerr, h.admission().Class))
				}
			}
		} else {
			required := rerankRequired(querySens, results, rrf.RerankMaxDocs)
			results, err = rrf.Rerank(ctx, h.pool, h.backendPool, required, originalQuery, results, ar.ApiKeyID, h.admission())
			if err != nil {
				slog.Warn("rerank failed, using original order",
					"error", err,
					"request_id", requestID,
				)
			}
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
			Sensitivity:      r.Sensitivity,
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
			Success:             true,
			Sources:             buildSourceResponses(sources, supersedesMap, req.IncludeContent),
			Confidence:          llm.ClassifyConfidence(maxScore, cfg.SynthesisSettings()),
			Translated:          translated,
			ActivatedDimWeights: activatedDimWeights(temporalResult),
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
	synthResult, err := llm.Synthesize(ctx, h.pool, h.backendPool, h.quota, cfg.SynthesisSettings(), querySens, originalQuery, sources, temporalDates, ar.ApiKeyID, ar.HomeScope, h.admission())
	if err != nil {
		slog.Error("synthesis failed",
			"error", err,
			"request_id", requestID,
		)
		// Quota exhausted (call budget, or cost budget under on_exceed=block):
		// 429 with a generic code — the budget kind (daily_calls/cost_budget)
		// is tenant policy and stays in slog/admin, never on the wire.
		var quotaErr *backends.ErrQuotaExceeded
		if errors.As(err, &quotaErr) {
			hb.finish(http.StatusTooManyRequests, map[string]any{
				"success": false, "error_code": "quota_exceeded",
			})
			return
		}
		// Dispatcher capacity rejection (design/03 §4.5.2 synthesis row): hooked
		// into the EXISTING mapper as a 429 — one mapper, one doctrine. The
		// heartbeat has already committed the 200 header on the synthesis path
		// (useHeartbeat is always true here), so the Retry-After header cannot
		// travel; the generic B6 body carries the signal, exactly as the
		// quota_exceeded 429 above does. Best-effort header set is a no-op after
		// commit but correct should the path ever run heartbeat-free.
		if dispatch.IsRejection(err) {
			setRejectRetryAfter(w, h.admitter, err)
			hb.finish(http.StatusTooManyRequests, rejectionBody())
			return
		}
		// Empty chain: generic client error — role + required sensitivity are
		// tenant-own information, the per-backend exclusion reasons (trust,
		// gaming, disabled) are topology disclosure and stay in slog/admin.
		var noElig *backends.ErrNoEligibleBackend
		if errors.As(err, &noElig) {
			hb.finish(http.StatusServiceUnavailable, map[string]any{
				"success": false, "error_code": "no_eligible_backend",
				"role": noElig.Role, "required_sensitivity": string(noElig.Required),
			})
			return
		}
		// In heartbeat mode the 200 is already committed -> success:false in body.
		hb.finish(http.StatusInternalServerError, map[string]any{"success": false, "error": "synthesis failed"})
		return
	}

	// Step 9: Access log (async, non-blocking).
	go h.logAccess(ar, results, query)

	// Step 10: Build response.
	respSources := buildSourceResponses(synthResult.Sources, supersedesMap, false)

	resp := queryResponse{
		Success:             true,
		Answer:              synthResult.Answer,
		Sources:             respSources,
		Confidence:          synthResult.Confidence,
		Model:               synthResult.Model,
		EvalCount:           synthResult.EvalCount,
		Translated:          translated,
		ActivatedDimWeights: activatedDimWeights(temporalResult),
	}

	// Set model even when LLM was skipped (for consistency): the model that
	// WOULD answer = highest-priority enabled synthesis backend in the pool.
	if resp.Model == "" {
		resp.Model = h.backendPool.PrimaryModel(backends.RoleSynthesis)
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
			`INSERT INTO context_access_log (api_key_id, block_id, action, query_text, metadata, principal_id)
			 VALUES ($1, $2, 'query', $3, $4,
			         (SELECT k.principal_id FROM context_api_keys k WHERE k.id = $1::uuid))`,
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
// Since F3-P3 the chain resolves PER BLOCK with that block's floor-adjusted
// sensitivity (gate table embed-backfill row) and each wire call leaves a
// slim llmlog row with the block id.
//
// Dispatch class (MW5, E-U5(a)): INTERACTIVE under the triggering caller's
// principal — the loop runs inside the request latency, so calling it
// background would let a foreign background job starve this user's own
// pre-search step. The known lopsidedness (an unbounded backfill loop ahead
// of the search) is NOT solved here, only honestly classified; the
// structural relief is EmbedChain's per-attempt acquire with a fresh queue
// position — a younger interactive query embed overtakes the waiting
// backfill rest between two blocks. (Dispatch attribution is separate from
// the T35b llmlog attribution below, which stays NULL/background in nature.)
func (h *QueryHandler) backfillPending(ctx context.Context, floor config.ScopeFloor, scope string, adm embedcache.Admission) int {
	count := 0
	for {
		var blockID, title, content, sens, scope string
		err := h.pool.QueryRow(ctx,
			`SELECT id, title, content, sensitivity, scope FROM context_blocks
			WHERE embedding IS NULL AND NOT is_archived
			LIMIT 1`).Scan(&blockID, &title, &content, &sens, &scope)
		if err != nil {
			break // No more pending blocks (or error).
		}

		required := floor.Apply(backends.Sensitivity(sens), scope)
		chain, cerr := h.backendPool.Chain(backends.RoleEmbed, required, scope)
		if cerr != nil {
			// Trust/profile-empty chain: the block stays unembedded (visible
			// via FTS only) — never escalate across the trust border.
			slog.Warn("query backfill: no eligible embed backend", "block_id", blockID, "error", cerr)
			break
		}

		embedText := title + "\n\n" + content
		// pool=nil: document embeddings land in the block row, not the cache
		// (today's semantics — the cache is for repeated query/keyword text).
		vec, served, attempts, wired, err := embedcache.EmbedChain(
			ctx, nil, chain, backends.RoleEmbed, embedText, embed.PrefixDocument,
			embedcache.ReportFunc(llm.PoolReporter(h.backendPool)), adm)
		if wired {
			// TENANT-DECISION(backfill-attribution): "" → NULL. backfillPending is
			// query-triggered but maintenance in nature — it embeds whatever blocks
			// in this scope still lack a vector, not the caller's own request.
			// Charging the random foreground key that happens to trigger it would
			// skew per-key cost/call accounting (one query could absorb hundreds of
			// embed calls). Treated as background, same as the scheduler backfill.
			// Reversible if backfill cost should follow the triggering caller (then
			// backfillPending gains an apiKeyID param alongside its T34 scope param).
			h.logEmbedWire(ctx, "embed-backfill", required, served, attempts, []string{blockID}, err, "")
		}
		if err != nil {
			slog.Warn("query backfill: embed failed", "block_id", blockID, "error", err)
			break // Embed backend likely unavailable, don't retry.
		}
		// Model is the ACTUALLY SERVING backend's role model (served, not the
		// configured/requested one — W04-1 provenance).
		if err := store.StoreEmbedding(ctx, h.pool, blockID, served.ModelFor(backends.RoleEmbed).Model, vec); err != nil {
			slog.Warn("query backfill: store failed", "block_id", blockID, "error", err)
			break
		}
		count++
	}
	return count
}

// GrantFloorDefault is the conservative egress lower bound applied to EVERY
// grant-mediated block, fail-closed INDEPENDENT of the grantee's own config — it
// closes the fail-OPEN rift of a naive max(owner, grantee) when the grantee has no
// floor configured (the normal case, design/07 §5.4). A grant-mediated block can
// never leave for an external backend below personal.
//
// TENANT-DECISION(grant-floor-default): personal — umentscheidbar, konservativer
// Richtwert (one step over public/internal; raise to credentials to forbid all
// grant-mediated external egress). design/07 §5.4 ED-B8.
const GrantFloorDefault = backends.SensPersonal

// annotateSensitivities writes the floor-adjusted sensitivity onto EVERY
// result — not top-N: filterSuperseded and graph placement advance stragglers
// from beyond any window into the final llmSources (F3 §2.3). IDs missing
// from the map (deleted/archived between RRF and lookup) act as credentials
// (fail-closed).
//
// T43 (design/07 §5.4, ED-B8): a GRANT-MEDIATED result — its scope is NOT in the
// caller's readScopes, so it is visible ONLY via a block grant — has its egress
// floor hung on the GRANTEE identity, not the owner's: effective sensitivity =
// max(ownerFloor, granteeFloor, GrantFloorDefault, block.sensitivity). granteeFloor
// is the grantee tenant's strictest per-scope floor (resolved at the call site);
// GrantFloorDefault is the config-independent backstop. A non-grant result (scope
// IN readScopes) keeps today's owner-only floor — byte-identical to the scope-only
// path.
func annotateSensitivities(results []rrf.SearchResult, sensMap map[string]store.BlockSensitivity, floor config.ScopeFloor, readScopes []string, granteeFloor backends.Sensitivity) {
	for i := range results {
		bs, ok := sensMap[results[i].ID]
		if !ok {
			results[i].Sensitivity = backends.SensCredentials
			continue
		}
		eff := floor.Apply(bs.Sensitivity, bs.Scope)
		if !contains(readScopes, bs.Scope) {
			eff = backends.MaxSensitivity(eff, granteeFloor, GrantFloorDefault)
		}
		results[i].Sensitivity = eff
	}
}

// granteeScopeFloor resolves the STRONGEST per-scope sensitivity floor configured
// across ALL scopes the grantee tenant owns (design/07 §5.4): a grant-mediated
// block is held to the grantee's strictest egress floor, never just its home
// scope. Unconfigured scopes contribute nothing (SensPublic is the neutral fold
// element). A lookup error is logged and returns SensPublic — the protection never
// DEPENDS on this lookup, because GrantFloorDefault still applies in
// annotateSensitivities (fail-closed by construction).
func (h *QueryHandler) granteeScopeFloor(ctx context.Context, tenantID string, floor config.ScopeFloor) backends.Sensitivity {
	scopes, err := store.TenantScopes(ctx, h.pool, tenantID)
	if err != nil {
		slog.Warn("query: grantee scope floor lookup failed; GrantFloorDefault still applies", "error", err)
		return backends.SensPublic
	}
	maxFloor := backends.SensPublic
	for _, s := range scopes {
		maxFloor = backends.MaxSensitivity(maxFloor, floor.Apply(backends.SensPublic, s))
	}
	return maxFloor
}

// rerankLogEntry builds the query-rerank llmlog row (MW11, design/05 §4.4b
// rerank row). Under a WIRED call, duration_ms is the wait-free physical
// span and queue_wait_ms the lease wait (§4.4a — the caller-side clock
// starts before the acquire and would double-count the wait under
// enforcement). A non-wired outcome (the early-outs inside
// RerankCrossEncoder: <3 results, empty doc set) keeps the caller span —
// there was no lease, nothing inflates — and carries no queue_wait_ms
// (fabricating a 0 there would pollute the wait distribution with rows
// that never entered a queue). Class is the handler's admission binding
// (interactive; rejected admissions never reach this builder).
func rerankLogEntry(model string, b backends.Backend, required backends.Sensitivity,
	blockIDs []string, tel rrf.RerankWire, callerSpan time.Duration, rerr error, class dispatch.Class,
) llmlog.Entry {
	entry := llmlog.Entry{
		Pipeline:            "query-rerank",
		Model:               model,
		Host:                b.Host,
		Duration:            callerSpan,
		Err:                 rerr,
		BlockIDs:            blockIDs,
		RequiredSensitivity: string(required),
		Attempt:             1,
		BackendName:         b.Name,
		BackendTrust:        string(b.Trust),
		BackendLocality:     b.Locality,
		DispatchClass:       class.String(),
	}
	if tel.Wired {
		entry.Duration = tel.WireDur
		w := tel.WaitMs
		entry.QueueWaitMs = &w
	}
	return entry
}

// rerankRequired folds the operation requirement of the rerank stage:
// max(query sensitivity, the candidates that actually go to the reranker —
// the top maxDocs, not the full 200-candidate set).
func rerankRequired(querySens backends.Sensitivity, results []rrf.SearchResult, maxDocs int) backends.Sensitivity {
	n := len(results)
	if n > maxDocs {
		n = maxDocs
	}
	parts := make([]backends.Sensitivity, 0, n+1)
	parts = append(parts, querySens)
	for i := 0; i < n; i++ {
		parts = append(parts, results[i].Sensitivity)
	}
	return backends.MaxSensitivity(parts...)
}

// logEmbedWire records one slim llmlog row for an embed wire-call sequence
// (no bodies; block_ids where the call embedded block content — §2.7.3).
// served is nil when every attempt failed. MW11: duration/queue_wait/abort
// derive from the attempts (§4.4a); the class is this handler's admission
// binding — interactive for BOTH pipelines here, including the pre-search
// backfill (E-U5(a): it runs inside the request latency; the dispatch class
// is deliberately separate from the T35b llmlog attribution).
func (h *QueryHandler) logEmbedWire(_ context.Context, pipeline string, required backends.Sensitivity, served *backends.Backend, attempts []embedcache.WireAttempt, blockIDs []string, err error, apiKeyID string) {
	llm.LogEmbedWire(h.pool, pipeline, backends.RoleEmbed, required, served, attempts, blockIDs, err, apiKeyID, h.embedAdmission().Class)
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
