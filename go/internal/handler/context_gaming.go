package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"

	"github.com/GottZ/ctx/internal/store"
)

// ejectProfileName / ejectProfileScope identify the break-glass profile the
// eject/gaming shim maps onto (AM-7 canonical rename gaming→eject; 092 backfill
// creates it reserved). Both surfaces (eject-mode canonical, gaming-mode alias)
// toggle THIS profile.
const (
	ejectProfileName  = "eject"
	ejectProfileScope = "_global"
)

// gamingModeNote is the constant advisory in every eject/gaming-mode response:
// the toggle affects NEW chains only — a running 27B synthesis (≤60s) and an
// in-flight dream cycle (≤700s) finish normally (design 03 §2.6).
const gamingModeNote = "laufende Requests beenden normal; dream pausiert ab nächstem Zyklus"

// errEjectProfileMissing is returned by the eject-write when the reserved eject
// profile is gone (only reachable via psql break-glass — the reserved guard
// blocks API deletion). The shim then answers 422 rather than silently no-op'ing.
var errEjectProfileMissing = errors.New("eject profile missing")

// beforeGamingCommit is a test seam (nil in production): it fires inside the
// eject-write transaction, AFTER the profile write and BEFORE Commit. Returning
// an error there forces the deferred Rollback — the atomicity probe uses it to
// prove a commit-boundary failure leaves the profile row untouched (since U01-W5
// the write is profile-only; the legacy gaming.active dual-write is gone).
var beforeGamingCommit func() error

// gamingStateView is the eject/gaming-mode response payload (LEGACY shape, kept
// byte-identical for CLI/client compat, N19). disabled_backends is the eject
// profile's member names (sorted). unknown_backends is gone structurally (FK
// membership can neither dangle nor typo) — kept in the type as omitempty so the
// wire shape only ever shrinks.
type gamingStateView struct {
	Active           bool     `json:"active"`
	DisabledBackends []string `json:"disabled_backends"`
	UnknownBackends  []string `json:"unknown_backends,omitempty"`
	Note             string   `json:"note"`
}

// handleGamingMode is the eject/gaming toggle shim (AM-7). Both the canonical
// eject-mode action and the gaming-mode alias route here. A read ({} or absent
// data) returns the eject profile's state in the legacy gaming shape to any
// valid key (tierOpen, U01-E6); a mutation ({"mode":"on"|"off"}) is gated
// server-admin upstream and dual-writes the eject profile's active flag AND the
// gaming.active settings row ATOMICALLY in one transaction (§4.7-W3 cutover:
// both stores stay convergent). The write sets confirm_role_blackout IMPLICITLY
// — legacy gaming was never blackout-gated (herbert-rerank blackout is live and
// intended), so the shim must NOT route through the new blackout gate (gate c).
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
		if err := h.writeEjectActive(r, active); err != nil {
			if errors.Is(err, errEjectProfileMissing) {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
					"success": false,
					"error":   "eject profile missing — reserved break-glass profile was removed out of band (psql); restore it before toggling",
				})
				return
			}
			h.gamingInternalError(w, ctx, "eject/gaming-mode: persist failed", err)
			return
		}
		// Persisted but not live is never a plain success (settings.HandlePut
		// doctrine): a reload failure must surface, not silently report on. The
		// pool reload lands the profile flip in this process — that is the live
		// surface since U01-W5 (the eject profile is the single source of truth;
		// the legacy gaming.active settings arm is gone). settingsReload no longer
		// carries the toggle, but stays wired as an idempotent config refresh —
		// removing its plumbing would churn NewManageHandler's call sites and is
		// out of this wave's scope.
		h.reloadAfterMutation(ctx, "eject/gaming-mode")
		if h.settingsReload != nil {
			if err := h.settingsReload(ctx); err != nil {
				h.gamingInternalError(w, ctx, "eject/gaming-mode: persisted but reload failed", err)
				return
			}
		}
		slog.Info("eject/gaming-mode: toggled", "active", active,
			"request_id", RequestIDFromContext(ctx))
	}

	// Read OR post-write: render the LIVE eject profile state (after the reload
	// above, so a write echoes the value the next chain will see).
	view, err := h.ejectShapeView(ctx)
	if err != nil {
		h.gamingInternalError(w, ctx, "eject/gaming-mode: read failed", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"gaming":  view,
	})
}

// writeEjectActive flips the eject profile's active flag in its own transaction
// (U01-W5 cutover: the legacy gaming.active settings dual-write is gone — the
// eject profile is now the single source of truth for the exclusion). The write
// stays transactional so the 092 + 051 triggers emit audit + NOTIFY atomically
// with the row; SetTxRequestID stamps the request id for the trigger. The eject
// profile is toggled with scopes=nil (the mutation is server-admin by tier). A
// missing eject profile aborts the tx (errEjectProfileMissing).
func (h *ManageHandler) writeEjectActive(r *http.Request, active bool) error {
	ctx := r.Context()
	tx, err := h.pool.Begin(ctx) //nolint:forbidigo // handgebaute Tx-Klammer, fällt in T03-4b (K27)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op
	if err := store.SetTxRequestID(ctx, tx, RequestIDFromContext(ctx)); err != nil {
		return err
	}
	found, err := store.SetDisableProfileActive(ctx, tx, ejectProfileScope, ejectProfileName, active, actorID(r), nil)
	if err != nil {
		return err
	}
	if !found {
		return errEjectProfileMissing
	}
	if beforeGamingCommit != nil {
		if err := beforeGamingCommit(); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// ejectShapeView renders the eject profile in the legacy gaming shape: active +
// the member backend names (sorted, resolved against the pool snapshot). No
// unknown_backends — FK membership is always resolvable. A missing profile (only
// via psql break-glass) degrades to inactive/empty rather than erroring the read.
func (h *ManageHandler) ejectShapeView(ctx context.Context) (gamingStateView, error) {
	view := gamingStateView{DisabledBackends: []string{}, Note: gamingModeNote}
	p, err := store.GetDisableProfile(ctx, h.pool, ejectProfileScope, ejectProfileName)
	if err != nil {
		return view, err
	}
	if p == nil {
		return view, nil
	}
	view.Active = p.Active
	if h.backendPool != nil && len(p.MemberIDs) > 0 {
		nameByID := make(map[string]string)
		for _, b := range h.backendPool.Snapshot() {
			nameByID[b.ID] = b.Name
		}
		for _, id := range p.MemberIDs {
			if n, ok := nameByID[id]; ok {
				view.DisabledBackends = append(view.DisabledBackends, n)
			}
		}
		sort.Strings(view.DisabledBackends)
	}
	return view, nil
}

// gamingInternalError logs with the request id and returns a generic 500 —
// internal error strings never reach the client body.
func (h *ManageHandler) gamingInternalError(w http.ResponseWriter, ctx context.Context, msg string, err error) {
	slog.Error(msg, "error", err, "request_id", RequestIDFromContext(ctx))
	writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
}
