package handler

import (
	"log/slog"
	"net/http"
	"sync"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/dispatch"
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
	blocktypes  *blocktype.Registry
	// admitter is the dispatch admission layer (MW3, I-D1): the API path
	// classifies interactive — valid ONLY together with the dailyInflight
	// brake below (E-U2 coupling, design/01 §4.6 N8a).
	admitter dispatch.Admitter

	// dailyInflight is the per-principal concurrency cap = 1 on the daily
	// endpoint (E-U2 coupling, pattern: webchat semaphore chat.go): one
	// running daily synthesis per ApiKeyID, a second concurrent call 429s
	// fail-fast BEFORE any DB or LLM work. Without this brake any valid
	// tenant key could fire heavy GPU synthesis arbitrarily often as the
	// never-preempted, never-rejected top class — the coupling condition
	// under which the path would have stayed background. The map holds only
	// keys with a call in flight.
	mu            sync.Mutex
	dailyInflight map[string]struct{}
}

// NewSynthesizeHandler wires the daily-synthesis trigger handler. The LLM
// call chains over the pool's digest role at constant internal (G28/E6) —
// the same gate as the scheduler's 03:00 iteration. Without this explicit
// pool wiring the handler would re-attach to an env tuple on the next
// signature break and become a permanent Chain() bypass (gaming toggle and
// trust gate dead on this path — design 03 P4 step). admitter is the ONE
// process-wide dispatch admission layer (MW3).
func NewSynthesizeHandler(pool *pgxpool.Pool, backendPool *backends.Pool, blocktypes *blocktype.Registry, admitter dispatch.Admitter) *SynthesizeHandler {
	return &SynthesizeHandler{
		pool:          pool,
		backendPool:   backendPool,
		blocktypes:    blocktypes,
		admitter:      admitter,
		dailyInflight: map[string]struct{}{},
	}
}

// acquireDaily reserves the single daily-synthesis slot of one ApiKeyID;
// false means a call of the same key is in flight (→ 429 fail-fast).
func (h *SynthesizeHandler) acquireDaily(apiKeyID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, busy := h.dailyInflight[apiKeyID]; busy {
		return false
	}
	h.dailyInflight[apiKeyID] = struct{}{}
	return true
}

// releaseDaily frees the key's slot (deferred by HandleDaily).
func (h *SynthesizeHandler) releaseDaily(apiKeyID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.dailyInflight, apiKeyID)
}

// dailyAdmission is the API path's dispatch binding (E-U2, design/01 §4.6
// N8a): interactive — a human waits synchronously on the response — with the
// caller's principal. Only valid together with the acquireDaily brake; the
// 03:00 scheduler path binds background through the same
// GenerateDailyReport.
func (h *SynthesizeHandler) dailyAdmission(ar *auth.AuthResult) llm.Admission {
	return llm.Admission{
		Admitter: h.admitter,
		Class:    dispatch.ClassInteractive,
		Principal: dispatch.Principal{
			ApiKeyID:  ar.ApiKeyID,
			TenantID:  ar.TenantID,
			HomeScope: ar.HomeScope,
		},
	}
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

	// E-U2 coupling (MW3, design/01 §4.6 N8a — BOTH sides land in the same
	// wave): the brake first, then the interactive classification. A second
	// concurrent call of the same key never reaches the DB or a chain.
	if !h.acquireDaily(authResult.ApiKeyID) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"ok": false, "error": "daily synthesis already running"})
		return
	}
	defer h.releaseDaily(authResult.ApiKeyID)

	// Digest needs no scope floor — the role gates at constant internal,
	// block contents never enter the prompt (E6). The API path classifies
	// interactive (a human waits synchronously — E-U2); the 03:00 scheduler
	// path binds background through the same GenerateDailyReport.
	router := &dream.Router{
		Pool:       h.backendPool,
		Report:     llm.PoolReporter(h.backendPool),
		Admit:      h.dailyAdmission(authResult),
		Blocktypes: h.blocktypes,
	}

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
