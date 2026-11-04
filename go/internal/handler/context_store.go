package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/pgxdb"
	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// StoreHandler handles POST /api/store.
type StoreHandler struct {
	pool       *pgxpool.Pool
	cfg        ConfigStore
	blocktypes *blocktype.Registry
	rejectLog  writeRejectLog // issue #66: the throttled, key-attributed rejection line
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
	// ParentID hangs the written block under an existing one
	// (context_blocks.parent_id, the ONE structural parent — K3). Absent ⇒ no
	// parent, exactly as before this field existed; the answer shape is
	// unchanged either way (NZ-3 point 8, the follow-up T02-11 named).
	//
	// It is the first request shape in the tree that can satisfy
	// parent.mode=required, so the fourth claim gate (parentRequiredReject,
	// stage_gates.go) is a CONDITION on this route and stays a total refusal on
	// the surfaces that carry no parent. It is NOT the type's domain path: a
	// comment created here is a block with a parent, not an issue comment with
	// the title, the local sequence and the scope the issue verbs derive.
	//
	// The value is a full block uuid in the SAME scope this write resolves to
	// (the comment-scope invariant of §5.2, read as "child and parent share a
	// scope"). The write scope is decided by the scope gate, not by the parent —
	// the upsert key is (category, title, scope), so letting the parent pick the
	// scope would silently move the block a repeat save addresses. Unknown,
	// archived, foreign-scope, malformed and self-referencing ids answer ONE
	// class (422 unknown_parent), so the field is no existence oracle.
	ParentID string `json:"parent_id,omitempty"`
}

// parentNotLinkableMsg is the ONE answer to every parent_id this route cannot
// hang a block under — unknown id, archived block, block in another scope,
// malformed uuid, and the write's own identity — and it is deliberately the same
// sentence for all five: told apart, they would answer "does this uuid exist"
// and "is it yours" for any id a caller cares to try. It is the /api/store
// wording of the rule the issue arms keep with their uniform 404
// (store.ErrLinkScopeViolation, structlinks.go).
const parentNotLinkableMsg = "parent_id: no parent of this write's scope with that id — unknown, " +
	"archived, foreign-scope, malformed and self-referencing ids answer alike, so the field is no " +
	"existence oracle"

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

	// The write gates: required fields → size caps → sensitivity → the I7 claim
	// gates (category, provenance key, type + the WF T10 registry check) → the
	// G40 credentials detector → write scope → write rate limit. Until T03-2
	// this route spelled that chain out a second time, in the same order and
	// with the same prose, while runStageWriteGates already claimed to be the
	// one chain of REST, MCP-direct and MCP-staged (stage_gates.go). The claim
	// is now wired: the doc comment there and the parity probes hold because
	// this arm CALLS the chain, not because four copies happen to agree.
	//
	// res is what the gates DECIDED, not merely what they let pass: the
	// resolved scope, whether the caller named it, the post-detector
	// sensitivity and the post-detector metadata. Everything below reads THOSE
	// values (mcp.go does the same at its own call site) — reading req.Metadata
	// again would drop the detector's annotation for a caller that sent none.
	//
	// ONE config snapshot for the whole chain — still per tenant, resolved from
	// the request context (MT 06-C5), so a tenant's own default sensitivity and
	// its own RateLimitWrite override still apply. What changes is the COUNT:
	// the two values used to be read at two points (F3 §2.3b precedence for the
	// default, the limit further down), so a settings Replace in between could
	// serve them from two generations — the rule context_search.go states for
	// its own surface, now held here too.
	snap := h.cfg.SnapshotForRequest(ctx)
	res, rej := runStageWriteGates(ctx, h.pool, h.classifySet(ctx), authResult, req,
		snap.Pool.DefaultBlockSensitivity, snap.Query.RateLimitWrite, reqID)
	if rej != nil {
		h.reject(w, ctx, authResult, req, rej)
		return
	}

	// The named parent is resolved BEFORE anything is written (NZ-3 point 8).
	if rej := parentPreflight(ctx, h.pool, req, res.WriteScope, reqID); rej != nil {
		h.reject(w, ctx, authResult, req, rej)
		return
	}

	// Hash NOOP check: skip if identical content already exists.
	//
	// A write that NAMES a parent never takes the shortcut: the shortcut answers
	// "identical_content" without touching the row, so a repeat save that adds a
	// parent would be told "nothing to do" while the parent it asked for was
	// never written. The condition costs one upsert on a payload that is
	// otherwise a no-op and is the only way the field can mean what it says.
	if req.ParentID == "" {
		existingID, err := store.HashNOOPCheck(ctx, h.pool, req.Content, res.WriteScope, req.Category, req.Title)
		if err != nil {
			slog.Error("store: hash noop check error", "error", err, "request_id", reqID)
			h.reject(w, ctx, authResult, req, classInternal.reject("Internal server error"))
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
	block, err := store.UpsertBlock(ctx, h.pool, req.Category, req.Title, req.Content, req.Tags, res.Metadata, res.WriteScope, res.ScopeExplicit, res.Sens, req.Type)
	if err != nil {
		// I7/S3: the conflicting row is a derivative. The refusal is decided in
		// the store (atomic with the row lock) and answered here as 403.
		h.reject(w, ctx, authResult, req, upsertFailureReject(err, reqID))
		return
	}

	// Hang the block under the parent it named (nothing named ⇒ no-op).
	if rej := linkParent(ctx, h.pool, block.ID, req.ParentID, res.WriteScope, reqID); rej != nil {
		h.reject(w, ctx, authResult, req, rej)
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
	if res.Sens.Manual && block.Sensitivity != string(res.Sens.Value) {
		// Upsert-conflict downgrade rejected (upgrade-only write path): say so
		// instead of silently keeping the higher level.
		resp["warnings"] = []string{fmt.Sprintf(
			"sensitivity %s not applied: existing block is %s — downgrades need manage update with confirm_sensitivity_downgrade",
			res.Sens.Value, block.Sensitivity)}
	}
	writeJSON(w, http.StatusOK, resp)
}

// reject answers a write refused after its body parsed and writes the
// key-attributed rejection line (issue #66, write_reject_log.go). Every refusal
// of HandleStore past the body parse goes through it; the two before it do not
// — an unauthenticated request has no key to name, and an unparsable body has
// no write to name (it keeps its own WARN line).
func (h *StoreHandler) reject(w http.ResponseWriter, ctx context.Context, ar *auth.AuthResult, req storeRequest, rej *writeReject) {
	h.rejectLog.record(ctx, ar.ApiKeyID, req.Category, req.Scope, rej)
	writeJSONReject(w, rej)
}

// parentPreflight resolves the parent a write NAMED, before anything is
// written. nil = this write names no parent, or names one it may hang under.
//
// It exists because store.UpsertBlock owns its own transaction, so the link
// (linkParent below) is a SECOND bracket. Without this check the routine
// refusals — a typo, a parent in another scope, an archived one — would arrive
// AFTER the row exists, and the only answers left would be an error standing
// next to a stored block, or a 200 that silently dropped the parent. Neither is
// an answer this route may give for a field it accepts.
//
// It grants nothing: store.PutBlockParent re-validates inside the transaction
// that performs the write, and store.ParentLinkable answers unknown, archived,
// foreign-scope, malformed and self-referencing ids with ONE error, so the
// pre-check cannot become an existence oracle either. The self-reference is why
// it takes the whole request: (category, title, scope) is the upsert key, so a
// parent carrying that identity IS the row this write is about, and only a check
// that knows both can refuse it before the upsert instead of after.
func parentPreflight(ctx context.Context, pool *pgxpool.Pool, req storeRequest, writeScope, reqID string) *writeReject {
	parentID := req.ParentID
	if parentID == "" {
		return nil
	}
	if err := store.ParentLinkable(ctx, pool, parentID, writeScope, req.Category, req.Title); err != nil {
		if !errors.Is(err, store.ErrLinkScopeViolation) {
			slog.Error("store: parent lookup error", "error", err, "request_id", reqID)
			return classInternal.reject("Internal server error")
		}
		return classUnknownParent.reject(parentNotLinkableMsg)
	}
	return nil
}

// linkParent hangs the freshly written block under the parent the request named.
// nil = nothing named, or linked.
//
// store.PutBlockParent is the ONE parent_id write path (structlinks.go) and
// re-asserts the scope invariant inside this transaction — the same primitive
// store.InsertCommentBlock composes, so the two parent-bearing paths share their
// verdict instead of agreeing by accident. The allowed scope is the write's own
// resolved scope, which makes "child and parent share a scope" (§5.2) the
// condition here; the WRITE scope is not derived from the parent, because the
// upsert key is (category, title, scope) and a parent-derived scope would move
// the block a repeat save addresses.
//
// WINDOW, deliberate and named: the upsert committed in a transaction of its
// own, so this is a second bracket. parentPreflight removed the routine reasons
// for a failure here, which leaves a process death between the two commits, or a
// parent archived/deleted/moved in between — and then the block exists without
// the parent its type demands, the orphan the gate prevents. The answer names
// the failure instead of hiding it. Closing the window means handing the parent
// to store.UpsertBlock so both writes share ONE transaction, exactly as
// InsertCommentBlock does; that edit belongs to store/blocks.go, which the wave
// that added this field does not own (NZ-3 Übergabe).
func linkParent(ctx context.Context, pool *pgxpool.Pool, blockID, parentID, writeScope, reqID string) *writeReject {
	if parentID == "" {
		return nil
	}
	err := pgxdb.Write(ctx, pool, pgxdb.At("store: link block parent"), func(tx pgx.Tx) error {
		return store.PutBlockParent(ctx, tx, blockID, parentID, []string{writeScope})
	})
	if err == nil {
		return nil
	}
	slog.Error("store: parent link failed after the upsert — the block exists without its parent",
		"error", err, "block_id", blockID, "parent_id", parentID, "request_id", reqID)
	if errors.Is(err, store.ErrLinkScopeViolation) {
		return classUnknownParent.reject(parentNotLinkableMsg)
	}
	return classInternal.reject("Internal server error")
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
