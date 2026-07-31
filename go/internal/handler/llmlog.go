package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/store"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// LLMLogHandler serves GET /api/llmlog — the admin telemetry table.
type LLMLogHandler struct {
	pool *pgxpool.Pool
	cfg  ConfigStore
}

// NewLLMLogHandler creates the GET /api/llmlog handler.
func NewLLMLogHandler(pool *pgxpool.Pool, cfg ConfigStore) *LLMLogHandler {
	return &LLMLogHandler{pool: pool, cfg: cfg}
}

// llmlogError is the normalized error: a coarse class + a length-capped detail.
// The raw error string can embed up to 1 KiB of provider response body
// (client.go io.LimitReader) which may carry prompt fragments (e.g. OpenRouter
// moderation metadata). The cap + class is the contract so a later tenant-stats
// surface (O6/O9) cannot reopen the leak by accident.
type llmlogError struct {
	Class  string `json:"class"`
	Detail string `json:"detail"`
}

// llmlogEntry is one telemetry row. It carries NO request_system/request_user/
// response_content — the M025 body columns (full prompts incl. block content =
// a shadow corpus) are NEVER in the SELECT list (pinned by TestLLMLogNoPrompts).
// The three dispatch-telemetry columns (queue_wait_ms/dispatch_class/
// dispatch_abort, MW10/091) are pure Lease telemetry — nullable, body-free — so
// they join the list without touching the body-exclusion invariant (A5-W6).
type llmlogEntry struct {
	ID               string       `json:"id"`
	CreatedAt        time.Time    `json:"created_at"`
	Pipeline         string       `json:"pipeline"`
	Model            string       `json:"model"`
	Backend          string       `json:"backend"`
	DurationMs       *int         `json:"duration_ms"`
	Error            *llmlogError `json:"error"`
	PromptTokens     *int         `json:"prompt_tokens"`
	CompletionTokens *int         `json:"completion_tokens"`
	CostUSD          *float64     `json:"cost_usd"`
	QueueWaitMs      *int         `json:"queue_wait_ms"`
	DispatchClass    *string      `json:"dispatch_class"`
	DispatchAbort    *string      `json:"dispatch_abort"`
}

// HandleLLMLog serves the admin telemetry table. Admin-gated (RequireAdmin):
// hostnames via backend, and the error detail is a privacy surface even capped.
func (h *LLMLogHandler) HandleLLMLog(w http.ResponseWriter, r *http.Request) {
	cfg := h.cfg.Snapshot() //nolint:forbidigo // MT 06 BLIND: admin-only telemetry cap (llmlog.max_limit) is a server-global policy, not tenant-scoped.
	maxLimit := cfg.LLMLog.MaxLimit
	if maxLimit <= 0 {
		maxLimit = 200
	}
	limit := maxLimit
	if q := r.URL.Query().Get("limit"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 && n < limit {
			limit = n
		}
	}
	pipeline := r.URL.Query().Get("pipeline")
	errorsOnly := r.URL.Query().Get("errors_only") == "true"

	// Per-tenant scoping (T37b, 04-W5 §4.6): a server-admin sees every row; a
	// tenant-admin (the RequireAdminOrTenantAdmin gate guarantees one or the
	// other) sees ONLY rows attributed to its own tenant's keys. The keys are
	// resolved to a literal uuid[] FIRST (§6.4 — `api_key_id = ANY($keys)` rides
	// the apikey index, where `IN (subquery)` would hash-join past it). keyFilter
	// stays nil for a server-admin (no predicate); a non-nil (even empty) slice
	// gates: a tenant with zero keys gets an empty filter → zero rows (fail-
	// closed, never an unfiltered view). Background rows (api_key_id NULL) drop
	// out of any tenant view by construction and only a server-admin sees them.
	ar := AuthResultFromContext(r.Context())
	var keyFilter []string
	if ar == nil || !ar.IsServerAdmin() {
		tenant := ""
		if ar != nil {
			tenant = ar.TenantID
		}
		keys, kerr := store.TenantAPIKeyIDs(r.Context(), h.pool, tenant)
		if kerr != nil {
			slog.Error("llmlog: tenant key resolve failed", "error", kerr, "request_id", RequestIDFromContext(r.Context()))
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"success": false, "error": "internal error",
			})
			return
		}
		keyFilter = keys // non-nil ([]string{} when the tenant has no keys → zero rows)
	}

	// Explicit SELECT list — NEVER request_system/request_user/response_content.
	// id::text guarantees a string scan; backend = backend_name with host
	// fallback (design 04 §3.2). created_at DESC rides the hypertable path.
	rows, err := h.pool.Query(r.Context(), `
		SELECT id::text, created_at, pipeline, model,
		       COALESCE(backend_name, host) AS backend,
		       duration_ms, error, prompt_tokens, completion_tokens, cost_usd,
		       queue_wait_ms, dispatch_class, dispatch_abort
		FROM context_llm_log
		WHERE ($1 = '' OR pipeline = $1)
		  AND (NOT $2 OR error IS NOT NULL)
		  AND ($4::uuid[] IS NULL OR api_key_id = ANY($4))
		ORDER BY created_at DESC
		LIMIT $3`, pipeline, errorsOnly, limit, keyFilter)
	if err != nil {
		slog.Error("llmlog: query failed", "error", err, "request_id", RequestIDFromContext(r.Context()))
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "internal error",
		})
		return
	}
	defer rows.Close()

	entries := []llmlogEntry{}
	for rows.Next() {
		var e llmlogEntry
		var rawErr *string
		if err := rows.Scan(&e.ID, &e.CreatedAt, &e.Pipeline, &e.Model, &e.Backend,
			&e.DurationMs, &rawErr, &e.PromptTokens, &e.CompletionTokens, &e.CostUSD,
			&e.QueueWaitMs, &e.DispatchClass, &e.DispatchAbort); err != nil {
			slog.Error("llmlog: scan failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"success": false, "error": "internal error",
			})
			return
		}
		e.Error = normalizeLLMError(rawErr)
		entries = append(entries, e)
	}
	if rows.Err() != nil {
		slog.Error("llmlog: rows error", "error", rows.Err())
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "internal error",
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"success": true, "entries": entries})
}

// Body-state vocabulary for the gated detail fetch (D1b). The LIST stays
// body-free (TestLLMLogNoPrompts); the bodies live ONLY behind this per-id
// endpoint, and the state tells the client WHY a body is absent so it can
// render an honest affordance instead of a blank card.
const (
	// bodyPresent — at least one of request_system/request_user/response_content
	// is non-empty and is returned.
	bodyPresent = "present"
	// bodySealed — the row is a credentials-class call: bodies were never stored
	// (Entry.Slimmed at write time, E4). Not a loss — a deliberate no-shadow-
	// corpus policy for the hottest tier.
	bodySealed = "sealed"
	// bodyEvicted — a non-credentials row with a NULL body column: only
	// llmlog.EvictBodies writes NULL (the insert path stores Go strings, so an
	// unrecorded body lands as ''), so NULL is proof that retention removed
	// bodies that once existed.
	bodyEvicted = "evicted"
	// bodyBodyless — a non-credentials row whose bodies are all EMPTY ('' —
	// never NULL): the pipeline never records a wire body (embed, translate,
	// backfill, K9 rejection lines). Nothing was removed; there was never
	// anything to show. Split from bodyEvicted (llmlog W1): with retention
	// disabled the live corpus carried thousands of these rows, and labeling
	// them "evicted" claimed a loss that never happened.
	bodyBodyless = "bodyless"
)

// llmlogDetail is the STRICTLY GATED per-id body view (D1b, design/05 §4.5 /
// DECISIONS D1). Unlike llmlogEntry it MAY carry the M025 bodies — but only for
// a single row the caller is authorized to see (server-admin: any row; tenant-
// admin: only rows attributed to its own keys, mirroring the list filter). The
// bodies are pointers so a sealed/evicted row renders null (never ""), and
// BodyState names the reason. This is the ONLY handler in the package that
// SELECTs the body columns; it is per-id and gated, so it never becomes the
// bulk shadow-corpus surface the list invariant forbids.
type llmlogDetail struct {
	ID                  string    `json:"id"`
	CreatedAt           time.Time `json:"created_at"`
	Pipeline            string    `json:"pipeline"`
	Model               string    `json:"model"`
	Backend             string    `json:"backend"`
	RequiredSensitivity string    `json:"required_sensitivity"`
	BodyState           string    `json:"body_state"`
	RequestSystem       *string   `json:"request_system"`
	RequestUser         *string   `json:"request_user"`
	ResponseContent     *string   `json:"response_content"`
}

// HandleLLMLogDetail serves GET /api/llmlog/{id} — the gated prompt/reply body
// fetch behind a history-row click (D1b). Admin OR tenant-admin (same mount as
// the list); a tenant-admin sees a row ONLY if it is attributed to one of its
// own keys — a foreign or unknown id answers a uniform 404 (no existence oracle,
// §4.6/R6). Sealed (credentials-class) and evicted (retention-NULLed) rows
// return 200 with null bodies + the reason in body_state, so the client shows
// why rather than a blank card.
func (h *LLMLogHandler) HandleLLMLogDetail(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !uuidRe.MatchString(id) {
		// A malformed id is treated exactly like an unknown one: uniform 404, no
		// separate "bad id" branch that would let a probe distinguish the two.
		writeLLMLogNotFound(w)
		return
	}

	// Same per-tenant gate as the list (T37b): server-admin → no key predicate;
	// anyone else → api_key_id = ANY(its keys). A tenant with zero keys resolves
	// to an empty slice → the WHERE never matches → uniform 404 (fail-closed).
	ar := AuthResultFromContext(r.Context())
	var keyFilter []string
	if ar == nil || !ar.IsServerAdmin() {
		tenant := ""
		if ar != nil {
			tenant = ar.TenantID
		}
		keys, kerr := store.TenantAPIKeyIDs(r.Context(), h.pool, tenant)
		if kerr != nil {
			slog.Error("llmlog: detail tenant key resolve failed", "error", kerr, "request_id", RequestIDFromContext(r.Context()))
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"success": false, "error": "internal error",
			})
			return
		}
		keyFilter = keys
	}

	var d llmlogDetail
	var reqSystem, reqUser, respContent *string
	var sensitivity *string
	err := h.pool.QueryRow(r.Context(), `
		SELECT id::text, created_at, pipeline, model,
		       COALESCE(backend_name, host) AS backend,
		       required_sensitivity, request_system, request_user, response_content
		FROM context_llm_log
		WHERE id = $1::uuid
		  AND ($2::uuid[] IS NULL OR api_key_id = ANY($2))`,
		id, keyFilter,
	).Scan(&d.ID, &d.CreatedAt, &d.Pipeline, &d.Model, &d.Backend,
		&sensitivity, &reqSystem, &reqUser, &respContent)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Unknown id OR a row the caller may not see — one uniform answer.
			writeLLMLogNotFound(w)
			return
		}
		slog.Error("llmlog: detail query failed", "error", err, "request_id", RequestIDFromContext(r.Context()))
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "internal error",
		})
		return
	}
	if sensitivity != nil {
		d.RequiredSensitivity = *sensitivity
	}
	d.BodyState, d.RequestSystem, d.RequestUser, d.ResponseContent =
		classifyBodies(d.RequiredSensitivity, reqSystem, reqUser, respContent)

	writeJSON(w, http.StatusOK, map[string]any{"success": true, "detail": d})
}

// classifyBodies decides the body_state and which bodies to return. A
// credentials-class row is SEALED (bodies never stored — Entry.Slimmed, E4);
// otherwise a row with any non-empty body is PRESENT and returns them. For the
// rest, NULL vs '' is the discriminator (llmlog W1): the insert path stores Go
// strings and therefore never writes NULL, so a nil column is EvictBodies'
// signature — EVICTED. All-empty-but-not-nil means the pipeline never recorded
// a wire body — BODYLESS. Returning nil pointers for a non-present row keeps
// the wire bodies null, never "".
func classifyBodies(sensitivity string, sys, user, resp *string) (state string, outSys, outUser, outResp *string) {
	if sensitivity == "credentials" {
		return bodySealed, nil, nil, nil
	}
	if nonEmpty(sys) || nonEmpty(user) || nonEmpty(resp) {
		return bodyPresent, sys, user, resp
	}
	if sys == nil || user == nil || resp == nil {
		return bodyEvicted, nil, nil, nil
	}
	return bodyBodyless, nil, nil, nil
}

// nonEmpty reports whether a *string carries actual content (non-nil, non-"").
func nonEmpty(s *string) bool { return s != nil && *s != "" }

// writeLLMLogNotFound is the uniform 404 for the detail endpoint — the same
// shape for an unknown id and for a row the caller is not authorized to see, so
// the response is not an existence oracle (R6).
func writeLLMLogNotFound(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "not found"})
}

// errDetailCap is the hard cap on the error detail (design 04 §3.2 + R3): the
// raw string can carry up to 1 KiB of provider body with prompt fragments.
const errDetailCap = 256

var statusCodeRe = regexp.MustCompile(`unexpected status (\d{3})`)

// normalizeLLMError turns the raw stored error into class + capped detail, or
// nil when there is no error.
func normalizeLLMError(raw *string) *llmlogError {
	if raw == nil {
		return nil
	}
	s := strings.TrimSpace(*raw)
	if s == "" {
		return nil
	}
	return &llmlogError{
		Class:  classifyLLMError(s),
		Detail: truncateRunes(s, errDetailCap),
	}
}

// classifyLLMError maps the raw error string to a coarse class. Best-effort —
// the F3 error-class catalog can refine it later; the detail (capped) is the
// real payload, the class is a filter aid.
func classifyLLMError(s string) string {
	if m := statusCodeRe.FindStringSubmatch(s); m != nil {
		return "http_" + m[1]
	}
	low := strings.ToLower(s)
	switch {
	case strings.Contains(low, "context deadline"), strings.Contains(low, "timeout"):
		return "timeout"
	case strings.Contains(low, "connection refused"), strings.Contains(low, "no such host"),
		strings.Contains(low, "dial "), strings.Contains(low, "no route"):
		return "unreachable"
	case strings.Contains(low, "no_eligible_backend"), strings.Contains(low, "no eligible backend"):
		return "no_backend"
	case strings.HasSuffix(low, "eof"), strings.Contains(low, "unexpected eof"):
		return "eof"
	default:
		return "error"
	}
}

// truncateRunes caps s at max runes (rune-safe so the cut never splits a
// multibyte character).
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}
