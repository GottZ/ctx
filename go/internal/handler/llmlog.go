package handler

import (
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

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
}

// HandleLLMLog serves the admin telemetry table. Admin-gated (RequireAdmin):
// hostnames via backend, and the error detail is a privacy surface even capped.
func (h *LLMLogHandler) HandleLLMLog(w http.ResponseWriter, r *http.Request) {
	cfg := h.cfg.Snapshot()
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

	// Explicit SELECT list — NEVER request_system/request_user/response_content.
	// id::text guarantees a string scan; backend = backend_name with host
	// fallback (design 04 §3.2). created_at DESC rides the hypertable path.
	rows, err := h.pool.Query(r.Context(), `
		SELECT id::text, created_at, pipeline, model,
		       COALESCE(backend_name, host) AS backend,
		       duration_ms, error, prompt_tokens, completion_tokens, cost_usd
		FROM context_llm_log
		WHERE ($1 = '' OR pipeline = $1)
		  AND (NOT $2 OR error IS NOT NULL)
		ORDER BY created_at DESC
		LIMIT $3`, pipeline, errorsOnly, limit)
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
			&e.DurationMs, &rawErr, &e.PromptTokens, &e.CompletionTokens, &e.CostUSD); err != nil {
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
