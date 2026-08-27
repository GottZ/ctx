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
		writeJSONReject(w, classUnauthorized.reject("unauthorized"))
		return
	}

	// Parse body.
	var req storeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		slog.Warn("store: invalid request body", "error", err, "request_id", reqID)
		writeJSONReject(w, classInvalidBody.reject("Invalid request body"))
		return
	}

	// Validate required fields.
	if req.Category == "" || req.Title == "" || req.Content == "" {
		writeJSONReject(w, classMissingFields.reject("Missing required fields: category, title, content"))
		return
	}

	// Size limits (HTTP 413).
	if msg := blockSizeLimit(req.Category, req.Title, req.Content); msg != "" {
		writeJSONReject(w, classSizeCap.reject(msg))
		return
	}

	// Sensitivity: request > settings default (F3 §2.3b precedence). MT 06-C5:
	// the per-tenant generation resolves from the request context (tenant scope
	// from the auth result), so the default honors a tenant's own override.
	sens, sensErr := storeSensitivity(h.cfg.SnapshotForRequest(ctx).Pool.DefaultBlockSensitivity, req.Sensitivity)
	if sensErr != "" {
		writeJSONReject(w, classInvalidSensitivity.reject(sensErr))
		return
	}

	// What the client may CLAIM about this block, BEFORE any write: the
	// category it occupies (I7/S2), the provenance key it carries (I7/S3) and
	// the type it names (I7/S1 + the WF T10 registry check, fail-closed on a
	// nil registry — an unvalidated name must never reach the manual-provenance
	// write path). Same function, same order and same position as the MCP
	// surfaces' gate chain (runStageWriteGates): that is what makes it ONE
	// chain. Raw request metadata, before the detector adds its own key.
	if rej := claimReject(h.classifySet(ctx), req.Category, req.Type, req.Metadata); rej != nil {
		writeJSONReject(w, rej)
		return
	}

	// G40 credentials detector: a content pattern hit forces credentials
	// (upgrade-only, source='pattern'). See applyWriteDetector.
	sens, req.Metadata = applyWriteDetector(req.Content, reqID, sens, req.Metadata)

	// Scope validation: the write scope must be one the key may write — its
	// home_scope or 'shared' if allowed (writableBlockScopes, the same gate as
	// manage update/delete). Since E-M4 the rule lives in resolveWriteScope,
	// shared verbatim with the stage gates and the blob write core.
	writeScope, scopeExplicit, scopeRej := resolveWriteScope(authResult, req.Scope)
	if scopeRej != nil {
		writeJSONReject(w, scopeRej)
		return
	}

	// Rate limit check (writes/min, 0 = disabled). MT 06-C5: the limit now
	// resolves per-tenant from the request context — a tenant's own
	// RateLimitWrite override applies, falling back to the _global value.
	if limit := h.cfg.SnapshotForRequest(ctx).Query.RateLimitWrite; limit > 0 {
		writeCount, err := store.CheckRateLimit(ctx, h.pool, authResult.ApiKeyID)
		if err != nil {
			slog.Error("store: rate limit check error", "error", err, "request_id", reqID)
			writeJSONReject(w, classInternal.reject("Internal server error"))
			return
		}
		if writeCount >= limit {
			writeJSONReject(w, classRateLimit.reject(
				fmt.Sprintf("Rate limit exceeded: max %d writes per 60 seconds", limit)))
			return
		}
	}

	// Hash NOOP check: skip if identical content already exists.
	existingID, err := store.HashNOOPCheck(ctx, h.pool, req.Content, writeScope, req.Category, req.Title)
	if err != nil {
		slog.Error("store: hash noop check error", "error", err, "request_id", reqID)
		writeJSONReject(w, classInternal.reject("Internal server error"))
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
		// I7/S3: the conflicting row is a derivative. The refusal is decided in
		// the store (atomic with the row lock) and answered here as 403.
		writeJSONReject(w, upsertFailureReject(err, reqID))
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

// applyWriteDetector is the request-scoped wrapper around the ONE detector
// implementation, store.ApplyWriteDetector (Wissens-Ebenen V-W8): the verdict
// itself — upgrade-only to credentials, Detector set (source='pattern' in the
// upsert), metadata carrying the secret-free reason and never the matched
// secret — lives in the store, where UpsertBlock applies it to every write
// path. The handler keeps this call because the STAGED path needs the verdict
// before the write: it is pinned into the hash-bound canonical payload
// (store/confirm_payload.go:47-51). Only the logging is handler-local — this is
// the sole caller that owns a request id.
func applyWriteDetector(content, reqID string, sens store.SensitivityWrite, metadata map[string]any) (store.SensitivityWrite, map[string]any) {
	sens, metadata, m := store.ApplyWriteDetector(content, sens, metadata)
	if m != nil {
		slog.Info("store: credentials pattern detected — sensitivity forced to credentials",
			"kind", m.Kind, "request_id", reqID)
	}
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

// resolveWriteScope is the ONE evaluation of the write-scope gate on top of
// that set: an absent (empty) request scope resolves to the key's home scope,
// a NAMED one has to lie in writableBlockScopes(ar) or the write is refused
// with the single scope_denied verdict.
//
// It became a function on 2026-08-25 (E-M4), when the MCP `store` and
// `blob_store` tools grew an optional `scope` of their own. Until then the
// rule was spelled out inline at three sites (REST /api/store, the stage
// gates, blobWriteGate) — and a fourth and fifth surface reaching the gate is
// exactly the moment a copied formula starts to drift. The blob surface
// already paid that bill once (Gap-C0-c, wave B3: a second, narrower formula
// refused every blob write to a scope whose BLOCKS the same key could write),
// so the formula now has one name and one call site per surface.
//
// explicit reports whether the caller NAMED the scope — it is what
// store.UpsertBlock takes as scopeExplicit. The function is PURE: a surface
// that has to know the resolved scope before the gate chain runs (the MCP
// store tool's scoped hash-NOOP check) may call it twice without risking a
// second, disagreeing verdict.
func resolveWriteScope(ar *auth.AuthResult, requested string) (scope string, explicit bool, rej *writeReject) {
	if requested == "" {
		return ar.HomeScope, false, nil
	}
	if !contains(writableBlockScopes(ar), requested) {
		return "", false, classScopeDenied.reject("Cannot write to requested scope")
	}
	return requested, true, nil
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

// upsertFailureReject renders a failed store.UpsertBlock: the I7/S3 sentinel
// becomes 403 provenance_protected, every other fault stays the opaque 500 and
// is logged. Split out of HandleStore so the refusal and the fault do not share
// a branch at the call site — and so the MCP arm can answer the identical class
// through provenanceRejectOr.
//
// Note the asymmetry that is deliberate: a refusal is NOT logged at error
// level. It is a client being told no, not a server fault, and at the target
// corpus scale a rejected write must not be able to fill the error log.
func upsertFailureReject(err error, reqID string) *writeReject {
	if rej := provenanceRejectOr(err, nil); rej != nil {
		return rej
	}
	slog.Error("store: upsert error", "error", err, "request_id", reqID)
	return classInternal.reject("Internal server error")
}
