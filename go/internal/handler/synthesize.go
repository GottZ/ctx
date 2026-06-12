package handler

import (
	"log/slog"
	"net/http"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SynthesizeHandler exposes manual-trigger endpoints for the dream synthesis
// pipeline (Welle 38c / Welle 42). Tested via POST /api/synthesize/daily —
// the same code path that the daily scheduler goroutine calls at 03:00 local.
type SynthesizeHandler struct {
	pool        *pgxpool.Pool
	backendPool *backends.Pool
}

// NewSynthesizeHandler wires the daily-synthesis trigger handler. The LLM
// call chains over the pool's digest role at constant internal (G28/E6) —
// the same gate as the scheduler's 03:00 iteration. Without this explicit
// pool wiring the handler would re-attach to an env tuple on the next
// signature break and become a permanent Chain() bypass (gaming toggle and
// trust gate dead on this path — design 03 P4 step).
func NewSynthesizeHandler(pool *pgxpool.Pool, backendPool *backends.Pool) *SynthesizeHandler {
	return &SynthesizeHandler{pool: pool, backendPool: backendPool}
}

// HandleDaily triggers a single daily synthesis run for the caller's
// home_scope. Returns the new block_id (or empty string + ok=true when there
// was no activity in the last 24h). An empty digest chain stays a generic
// 500 — backend topology is admin-only (design 03 §2.4 digest row).
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

	// Digest needs no scope floor — the role gates at constant internal,
	// block contents never enter the prompt (E6).
	router := &dream.Router{Pool: h.backendPool, Report: llm.PoolReporter(h.backendPool)}

	blockID, err := dream.GenerateDailyReport(ctx, h.pool, router, dream.DreamOptions(), scope)
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
