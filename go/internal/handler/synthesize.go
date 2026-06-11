package handler

import (
	"log/slog"
	"net/http"

	"github.com/GottZ/ctx/internal/dream"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SynthesizeHandler exposes manual-trigger endpoints for the dream synthesis
// pipeline (Welle 38c / Welle 42). Tested via POST /api/synthesize/daily —
// the same code path that the daily scheduler goroutine calls at 03:00 local.
type SynthesizeHandler struct {
	pool *pgxpool.Pool
	cfg  ConfigStore
}

// NewSynthesizeHandler wires the daily-synthesis trigger handler. The LLM
// backend tuple is derived per request from a config snapshot via
// cfg.DreamBackend() — the same single derivation the dream loop and the
// 03:00 scheduler iteration use (F1-W7: the boot-time dreamB/dreamOpts copy
// died here).
func NewSynthesizeHandler(pool *pgxpool.Pool, cfg ConfigStore) *SynthesizeHandler {
	return &SynthesizeHandler{pool: pool, cfg: cfg}
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

	// One snapshot per request. The chat-num_ctx carry keeps every chat-model
	// call site on one runner (distinct num_ctx → extra 27B runner → VRAM OOM)
	// — same pattern as the scheduler's daily iteration.
	dreamB := h.cfg.Snapshot().DreamBackend()
	dreamOpts := dream.DreamOptions()
	if dreamB.NumCtx > 0 {
		dreamOpts.NumCtx = dreamB.NumCtx
	}

	blockID, err := dream.GenerateDailyReport(ctx, h.pool, dreamB, dreamOpts, scope)
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
