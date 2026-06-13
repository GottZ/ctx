package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/GottZ/ctx/internal/store"
)

// gamingModeNote is the constant advisory in every gaming-mode response: the
// toggle affects NEW chains only — a running 27B synthesis (≤60s) and an
// in-flight dream cycle (≤700s) finish normally (design 03 §2.6).
const gamingModeNote = "laufende Requests beenden normal; dream pausiert ab nächstem Zyklus"

// gamingStateView is the gaming-mode response payload. The per-backend runtime
// state (effective_state, cooldown) stays in the admin-gated backend-list
// (design 03 §2.6/§2.9) — this view is the toggle plus the typo guard.
type gamingStateView struct {
	Active           bool     `json:"active"`
	DisabledBackends []string `json:"disabled_backends"`
	UnknownBackends  []string `json:"unknown_backends,omitempty"`
	Note             string   `json:"note"`
}

// handleGamingMode is the gaming toggle (F3-P6, design 03 §2.6). A read ({} or
// absent data) returns the current state to any valid key; a mutation
// ({"mode":"on"|"off"}) is admin-gated upstream (actionRequiresAdmin) and
// persists gaming.active through the F2 settings layer — NOT an atomic, the
// dream-mode break path (restart ⇒ the GPU lock is gone) is the anti-pattern
// this avoids. The write ends with a SYNCHRONOUS settings reload so the toggle
// hits the next chain without a restart; the 051 NOTIFY trigger additionally
// converges other processes / psql break-glass edits.
func (h *ManageHandler) handleGamingMode(w http.ResponseWriter, r *http.Request, req manageRequest) {
	ctx := r.Context()

	if isGamingModeMutation(req) {
		var d struct {
			Mode string `json:"mode"`
		}
		if err := json.Unmarshal(req.Data, &d); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"success": false, "error": "invalid gaming-mode data"})
			return
		}
		var active bool
		switch d.Mode {
		case "on":
			active = true
		case "off":
			active = false
		default:
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"success": false, "error": `gaming mode must be "on" or "off"`})
			return
		}
		if err := h.writeGamingActive(r, active); err != nil {
			h.gamingInternalError(w, ctx, "gaming-mode: persist failed", err)
			return
		}
		// Persisted but not live is never a plain success (settings.HandlePut
		// doctrine): a reload failure must surface, not silently report on.
		if h.settingsReload != nil {
			if err := h.settingsReload(ctx); err != nil {
				h.gamingInternalError(w, ctx, "gaming-mode: persisted but reload failed", err)
				return
			}
		}
		slog.Info("gaming-mode: toggled", "active", active,
			"request_id", RequestIDFromContext(ctx))
	}

	// Read OR post-write: render the LIVE snapshot (after the reload above, so
	// a write echoes the value the next chain will see).
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"gaming":  h.gamingSnapshot(),
	})
}

// writeGamingActive persists gaming.active in an attributed transaction (the
// 051 triggers emit audit + NOTIFY atomically with the row), mirroring
// SettingsHandler.writeSetting — the value is a settings override, not a
// bespoke table.
func (h *ManageHandler) writeGamingActive(r *http.Request, active bool) error {
	ctx := r.Context()
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op
	if err := store.SetTxRequestID(ctx, tx, RequestIDFromContext(ctx)); err != nil {
		return err
	}
	val := json.RawMessage("false")
	if active {
		val = json.RawMessage("true")
	}
	if err := store.UpsertSetting(ctx, tx, "gaming.active", store.GlobalScope, val, actorID(r)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// gamingSnapshot reads the current gaming state from the config snapshot and
// cross-checks the disabled-backends list against the live pool names. A name
// that matches no backend makes the toggle silently ineffective (a typo ⇒ the
// GPU stays busy, design 03 risk 6.6) — it surfaces as unknown_backends.
func (h *ManageHandler) gamingSnapshot() gamingStateView {
	gs := h.cfg.Snapshot().GamingState()
	view := gamingStateView{
		Active:           gs.Active,
		DisabledBackends: gs.DisabledBackends,
		Note:             gamingModeNote,
	}
	if view.DisabledBackends == nil {
		view.DisabledBackends = []string{}
	}
	if h.backendPool != nil {
		known := make(map[string]bool)
		for _, b := range h.backendPool.Snapshot() {
			known[b.Name] = true
		}
		for _, name := range view.DisabledBackends {
			if !known[name] {
				view.UnknownBackends = append(view.UnknownBackends, name)
			}
		}
	}
	return view
}

// gamingInternalError logs with the request id and returns a generic 500 —
// internal error strings never reach the client body.
func (h *ManageHandler) gamingInternalError(w http.ResponseWriter, ctx context.Context, msg string, err error) {
	slog.Error(msg, "error", err, "request_id", RequestIDFromContext(ctx))
	writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
}
