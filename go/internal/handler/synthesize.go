package handler

import (
	"log/slog"
	"net/http"

	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SynthesizeHandler exposes manual-trigger endpoints for the dream synthesis
// pipeline (Welle 38c / Welle 42). Tested via POST /api/synthesize/daily —
// the same code path that the daily scheduler goroutine calls at 03:00 local.
type SynthesizeHandler struct {
	pool      *pgxpool.Pool
	dreamHost string
	dreamKey  string
	dreamModel string
	dreamThink *bool
	dreamOpts  llm.Options
}

// NewSynthesizeHandler wires the daily-synthesis trigger handler with the same
// LLM coordinates the dream loop uses. dreamModel may be empty in which case
// the caller has already resolved a fallback (e.g. to chat model).
func NewSynthesizeHandler(pool *pgxpool.Pool, host, apiKey, model string, think *bool, opts llm.Options) *SynthesizeHandler {
	return &SynthesizeHandler{
		pool:       pool,
		dreamHost:  host,
		dreamKey:   apiKey,
		dreamModel: model,
		dreamThink: think,
		dreamOpts:  opts,
	}
}

// HandleDaily triggers a single daily synthesis run for the caller's
// home_scope. Returns the new block_id (or empty string + ok=true when there
// was no activity in the last 24h).
func (h *SynthesizeHandler) HandleDaily(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	requestID := RequestIDFromContext(ctx)

	authResult := AuthResultFromContext(ctx)
	if authResult == nil || !authResult.IsValid {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "unauthorized"})
		return
	}

	scope := authResult.HomeScope
	if scope == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "home scope unavailable"})
		return
	}

	blockID, err := dream.GenerateDailyReport(ctx, h.pool, h.dreamHost, h.dreamKey, h.dreamModel, h.dreamThink, h.dreamOpts, scope)
	if err != nil {
		slog.Error("synthesize: daily report failed",
			"error", err,
			"scope", scope,
			"request_id", requestID,
		)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "synthesis failed"})
		return
	}

	resp := map[string]any{
		"ok":       true,
		"block_id": blockID,
		"scope":    scope,
	}
	if blockID == "" {
		resp["reason"] = "no_activity"
	}
	writeJSON(w, http.StatusOK, resp)
}
