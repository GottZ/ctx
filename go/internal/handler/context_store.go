package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/sensitivity"
	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// StoreHandler handles POST /api/store.
type StoreHandler struct {
	pool       *pgxpool.Pool
	cfg        ConfigStore
	blocktypes *blocktype.Registry
}

// NewStoreHandler creates a new StoreHandler. The write rate limit comes from
// a config snapshot per request (F1-W7), not a boot copy. blocktypes feeds
// the T4 classify hook (registry-driven, never the compiled-in builtin set);
// nil in tests that don't exercise classification — the hook then logs and
// skips (block stays at the default type).
func NewStoreHandler(pool *pgxpool.Pool, cfg ConfigStore, blocktypes *blocktype.Registry) *StoreHandler {
	return &StoreHandler{pool: pool, cfg: cfg, blocktypes: blocktypes}
}

type storeRequest struct {
	Category string         `json:"category"`
	Title    string         `json:"title"`
	Content  string         `json:"content"`
	Tags     []string       `json:"tags"`
	Metadata map[string]any `json:"metadata"`
	Scope    string         `json:"scope"`
	// Sensitivity classifies the block for trust gating (F3 §3.5). Absent ⇒
	// settings default pool.default_block_sensitivity (source='default');
	// present ⇒ source='manual'. On upsert-conflict an explicit value applies
	// upgrade-only — downgrades go through the confirm-gated update path.
	Sensitivity string `json:"sensitivity,omitempty"`
	// Type sets the block's policy type explicitly (WF T10; REST only — the
	// MCP store tool deliberately carries no type field until the F6-C6
	// write-confirmation ships, decision D4). Registry-validated (422 on
	// unknown); sets type_source='manual', which permanently overrides the
	// auto-classifier (T4 semantics). Absent ⇒ auto-classification.
	Type string `json:"type,omitempty"`
}

// HandleStore processes upsert requests with auto-embedding.
func (h *StoreHandler) HandleStore(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)

	// Auth from middleware context.
	authResult := AuthResultFromContext(ctx)
	if authResult == nil || !authResult.IsValid {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "unauthorized"})
		return
	}

	// Parse body.
	var req storeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		slog.Warn("store: invalid request body", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "Invalid request body",
		})
		return
	}

	// Validate required fields.
	if req.Category == "" || req.Title == "" || req.Content == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "Missing required fields: category, title, content",
		})
		return
	}

	// Size limits (HTTP 413).
	if msg := blockSizeLimit(req.Category, req.Title, req.Content); msg != "" {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
			"success": false, "error": msg,
		})
		return
	}

	// Sensitivity: request > settings default (F3 §2.3b precedence). MT 06-C5:
	// the per-tenant generation resolves from the request context (tenant scope
	// from the auth result), so the default honors a tenant's own override.
	sens, sensErr := storeSensitivity(h.cfg.SnapshotForRequest(ctx).Pool.DefaultBlockSensitivity, req.Sensitivity)
	if sensErr != "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": sensErr})
		return
	}

	// Explicit type (WF T10): validate against the registry snapshot BEFORE
	// any write. Fail-closed on a nil registry (test wiring): an unvalidated
	// name must never reach the manual-provenance write path.
	if req.Type != "" {
		if msg := h.validateStoreTypeName(ctx, req.Type); msg != "" {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"success": false, "error": msg,
			})
			return
		}
	}

	// G40 credentials detector: a content pattern hit forces credentials
	// (upgrade-only, source='pattern'). See applyWriteDetector.
	sens, req.Metadata = applyWriteDetector(req.Content, reqID, sens, req.Metadata)

	// Scope validation: the write scope must be one the key may write — its
	// home_scope or 'shared' if allowed (writableBlockScopes, the same gate as
	// manage update/delete).
	writeScope := authResult.HomeScope
	scopeExplicit := false
	if req.Scope != "" {
		scopeExplicit = true
		if contains(writableBlockScopes(authResult), req.Scope) {
			writeScope = req.Scope
		} else {
			writeJSON(w, http.StatusForbidden, map[string]any{
				"success": false, "error": "Cannot write to requested scope",
			})
			return
		}
	}

	// Rate limit check (writes/min, 0 = disabled). MT 06-C5: the limit now
	// resolves per-tenant from the request context — a tenant's own
	// RateLimitWrite override applies, falling back to the _global value.
	if limit := h.cfg.SnapshotForRequest(ctx).Query.RateLimitWrite; limit > 0 {
		writeCount, err := store.CheckRateLimit(ctx, h.pool, authResult.ApiKeyID)
		if err != nil {
			slog.Error("store: rate limit check error", "error", err, "request_id", reqID)
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"success": false, "error": "Internal server error",
			})
			return
		}
		if writeCount >= limit {
			writeJSON(w, http.StatusTooManyRequests, map[string]any{
				"success": false, "error": fmt.Sprintf("Rate limit exceeded: max %d writes per 60 seconds", limit),
			})
			return
		}
	}

	// Hash NOOP check: skip if identical content already exists.
	existingID, err := store.HashNOOPCheck(ctx, h.pool, req.Content, writeScope, req.Category, req.Title)
	if err != nil {
		slog.Error("store: hash noop check error", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Internal server error",
		})
		return
	}
	if existingID != "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"success":     true,
			"action":      "noop",
			"reason":      "identical_content",
			"existing_id": existingID,
		})
		return
	}

	// F6-C6 scope boundary (D-E1, DECISIONS §Klarstellung): REST stays a
	// DIRECT write path — deliberately. The stage-then-confirm dance lives on
	// the LLM surfaces only (MCP store, D-W5; chat tools, D-W6+); a
	// confirm_writes key writing over REST is not gated here. Gating LLM
	// writes is the calling harness's job — this endpoint serves human-driven
	// clients (CLI, scripts). The handler-level placement (never in
	// store.UpsertBlock) still matters: digest/dream write through UpsertBlock
	// internally and must never self-stage.

	// Execute upsert.
	block, err := store.UpsertBlock(ctx, h.pool, req.Category, req.Title, req.Content, req.Tags, req.Metadata, writeScope, scopeExplicit, sens, req.Type)
	if err != nil {
		slog.Error("store: upsert error", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Internal server error",
		})
		return
	}

	// Welle 44 / WF T4: Auto-classify type_name from the registry
	// snapshot (rules are data now; type_source='manual' blocks are never
	// re-classified). No-op when no rule matches (default type stays).
	if _, err := store.ClassifyBlockAfterUpsert(ctx, h.pool, h.classifySet(ctx), block.ID, block.Title, block.Metadata); err != nil {
		slog.Warn("store: auto-classify failed", "error", err, "block_id", block.ID, "request_id", reqID)
	}

	// Log write (fire-and-forget in background).
	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if logErr := store.LogAccess(bgCtx, h.pool, authResult.ApiKeyID, block.ID, "write"); logErr != nil {
			slog.Error("store: write log error", "error", logErr, "request_id", reqID)
		}
	}()

	// Enrich temporal data (fire-and-forget).
	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		times := store.ExtractDates(block.Content)
		if err := store.UpdateContentTimes(bgCtx, h.pool, block.ID, times); err != nil {
			slog.Error("store: content_times update failed", "error", err, "block_id", block.ID, "request_id", reqID)
		}
		// Always populate: createdAt is included as meta-anchor even without content times.
		if err := store.PopulateTemporal(bgCtx, h.pool, block.ID, times, block.CreatedAt); err != nil {
			slog.Error("store: temporal populate failed", "error", err, "block_id", block.ID, "request_id", reqID)
		}
	}()

	// Embedding generated async by scheduler backfill loop.
	resp := map[string]any{
		"success": true,
		"block":   block,
	}
	if sens.Manual && block.Sensitivity != string(sens.Value) {
		// Upsert-conflict downgrade rejected (upgrade-only write path): say so
		// instead of silently keeping the higher level.
		resp["warnings"] = []string{fmt.Sprintf(
			"sensitivity %s not applied: existing block is %s — downgrades need manage update with confirm_sensitivity_downgrade",
			sens.Value, block.Sensitivity)}
	}
	writeJSON(w, http.StatusOK, resp)
}

// blockSizeLimit checks the shared write-path size caps. Empty = within
// limits; otherwise the 413 message. Update path checks the same caps on its
// optional fields.
func blockSizeLimit(category, title, content string) string {
	switch {
	case len(category) > 100:
		return "Category exceeds 100 characters"
	case len(title) > 500:
		return "Title exceeds 500 characters"
	case len(content) > 50*1024:
		return "Content exceeds 50KB"
	}
	return ""
}

// applyWriteDetector runs the G40 credentials scanner over content. On a hit it
// returns the sensitivity raised UPGRADE-ONLY to credentials with Detector set
// (source='pattern' in the upsert) and metadata carrying the secret-free reason
// — never the matched secret. A hit only ever RAISES: it overrides a too-low
// manual/default classification but leaves an already-credentials block intact.
// No hit ⇒ inputs returned unchanged.
func applyWriteDetector(content, reqID string, sens store.SensitivityWrite, metadata map[string]any) (store.SensitivityWrite, map[string]any) {
	m, hit := sensitivity.Scan(content)
	if !hit {
		return sens, metadata
	}
	if sens.Value.Rank() < backends.SensCredentials.Rank() {
		sens.Value = backends.SensCredentials
	}
	sens.Manual = false
	sens.Detector = true
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadata["sensitivity_detector"] = map[string]any{"kind": m.Kind, "reason": m.Reason}
	slog.Info("store: credentials pattern detected — sensitivity forced to credentials",
		"kind", m.Kind, "request_id", reqID)
	return sens, metadata
}

// storeSensitivity resolves the write intent: explicit request value (manual,
// validated against the hard level set) over the settings default. Non-empty
// errMsg = 400.
func storeSensitivity(def backends.Sensitivity, requested string) (store.SensitivityWrite, string) {
	if requested == "" {
		return store.SensitivityWrite{Value: def}, ""
	}
	s := backends.Sensitivity(requested)
	if !backends.ValidSensitivity(s) {
		return store.SensitivityWrite{}, "Invalid sensitivity: must be credentials|personal|internal|public"
	}
	return store.SensitivityWrite{Value: s, Manual: true}, ""
}

// contains checks if a string slice contains a value.
func contains(slice []string, val string) bool {
	for _, s := range slice {
		if s == val {
			return true
		}
	}
	return false
}

// writableBlockScopes returns the scopes a key may WRITE blocks to. It is the
// SINGLE source of truth for the block write gate — store create, manage update,
// manage delete and guard-resolve all use it, so a key can mutate exactly the
// scopes this formula yields. The formula (078, E4b) is:
//
//	[home_scope] ∪ (write_scopes ∩ (allowed_scopes ∪ {home_scope})) ∪ {shared-if-allowed}
//
//   - home_scope is always writable and always element [0] (the minimal view, never
//     empty) — a read-only grant never widens it (v4.0.1 line).
//   - 'shared' stays writable only when it is in allowed_scopes (the collaboration
//     scope of the default tenant) — unchanged from pre-078.
//   - write_scopes widen the set, but ONLY intersected with what the key may READ
//     (allowed ∪ home). This is enforcement path (b) of the double invariant: a
//     write_scope left STALE by a later allowed_scopes shrink falls out of the
//     intersection HERE — fail-closed at one eval point, not re-checked at N write
//     sites. Other allowed_scopes with no matching write_scope stay read-only.
func writableBlockScopes(ar *auth.AuthResult) []string {
	scopes := []string{ar.HomeScope}
	if contains(ar.AllowedScopes, "shared") {
		scopes = append(scopes, "shared")
	}
	// write_scopes ∩ (allowed_scopes ∪ {home_scope}): a write_scope is honoured only
	// if the key also holds a read right for it. Dedup against what is already in the
	// set (home / shared) keeps the output stable.
	for _, ws := range ar.WriteScopes {
		if contains(scopes, ws) {
			continue
		}
		if ws == ar.HomeScope || contains(ar.AllowedScopes, ws) {
			scopes = append(scopes, ws)
		}
	}
	return scopes
}

// writeJSON writes a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		slog.Error("failed to encode JSON response", "error", err)
	}
}

// classifySet resolves the caller's block-type policy set for the classify
// hook. nil registry (tests without classify wiring) → nil set: the hook
// fails loudly in its error return instead of silently using a compiled-in
// set — the T4 policy-effect gate depends on the DB registry being the source.
func (h *StoreHandler) classifySet(ctx context.Context) *blocktype.Set {
	if h.blocktypes == nil {
		return nil
	}
	return h.blocktypes.SnapshotForRequest(ctx)
}

// validateStoreTypeName checks an explicit store `type` value against the
// registry snapshot (WF T10). Empty msg = registered. Fail-closed on a nil
// registry: an unvalidated name must never reach the manual write path
// (§5.1(b) — Go-side write validation is verification layer b). Delegates to
// validateTypeNameAgainstSet — the same check the stage gates run (D-W2).
func (h *StoreHandler) validateStoreTypeName(ctx context.Context, name string) string {
	if h.blocktypes == nil {
		return "type: block-type registry not wired — cannot validate type names"
	}
	return validateTypeNameAgainstSet(h.blocktypes.SnapshotForRequest(ctx), name)
}
