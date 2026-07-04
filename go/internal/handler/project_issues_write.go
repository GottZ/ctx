// /api/project/{id}/issues* — the project-scoped issue WRITE surface (workflow
// W7, design/03-workflow-api-cli.md §4.2/§4.6/§5.1/§5.2). These POST/PATCH routes
// share the ONE MountProjectIssues gate group with the W6 reads (RequireMember
// admits, §5.1: a missing gate is a missing route — 404, never fail-open), then
// each write handler runs the per-project WRITE-SCOPE gate before touching the
// store:
//
//	POST  /api/project/{id}/issues                       create issue          write-scope
//	PATCH /api/project/{id}/issues/{block_id}            field/state change     write-scope
//	POST  /api/project/{id}/issues/{block_id}/comments   create comment         write-scope
//
// Write-scope gate (§4.6 "Write-Scope-Gate", §5.2(3)): the project resolves to
// exactly ONE scope; that scope MUST be in writableBlockScopes(ar) (the single
// block-write formula, context_store.go — home ∪ write_scopes∩readable ∪
// shared-if-allowed) or the caller gets 403. A project the caller can not even
// READ (scope ∉ ReadScopes), a foreign block id, and an unknown project all read
// as a uniform 404 — no existence oracle (§5.2). 403 is only ever returned to a
// caller who already holds READ on the scope, so it reveals nothing new.
//
// Scope purity (§5.2, mirrors the W6 reads): the store call is bounded to the
// SINGLE project scope (writableScopes = []string{scope}), never the caller's
// whole writable set — so a block id that belongs to ANOTHER (even writable)
// project reads as not-found through THIS project's path, not silently mutated.
//
// State validation is policy DATA, not Go: the initial status and every
// transition come from set.WorkflowInitial / set.ValidateTransition over the
// resolved type registry (§4.3 doctrine) — swapping the registry status set
// changes the 422 verdict with no rebuild. Every body-bearing response carries
// render:'untrusted' + workflow_status/type_name, wire-consistent with W6/I-D.
//
// Rate limit (§4.4): issue writes ride the existing per-api_key_id write throttle
// (config query.rate_limit_write, tenant-overridable, the SAME 60 s window +
// context_access_log substrate as /api/store) — one write budget across
// /api/store and the issue surface. The project-scoped sync/webhook limits of
// §4.4 need a project dimension the I6 substrate lacks; those are W11/W13, not
// this path (a per-key issue-write throttle is exactly what the I6 mechanic
// carries). No new limiter is introduced.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/store"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// restIssueCreate is the POST /issues body. There is deliberately NO scope field
// — the write scope is the project's (path-resolved), never caller-supplied
// (scope-injection defense, §5.2). decodeStrict rejects unknown keys (so a
// stray "scope" is a 400, not a silently-ignored field).
type restIssueCreate struct {
	Title    string         `json:"title"`
	Content  string         `json:"content,omitempty"`
	Tags     []string       `json:"tags,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
	Status   string         `json:"status,omitempty"`
}

// restIssueUpdate is the PATCH body (nil = leave unchanged). No scope/id fields.
type restIssueUpdate struct {
	Title    *string        `json:"title,omitempty"`
	Content  *string        `json:"content,omitempty"`
	Tags     []string       `json:"tags,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
	Status   *string        `json:"status,omitempty"`
}

// restCommentCreate is the POST /comments body. There is deliberately NO
// parent_id (the parent is the path {block_id}) and NO scope (inherited from the
// parent by the store, the comment-scope invariant §5.2).
type restCommentCreate struct {
	Author   string         `json:"author,omitempty"`
	Content  string         `json:"content,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// resolveWriteScope loads the project of {id} and returns its scope IFF the
// caller may WRITE it. The order is the no-oracle contract: a foreign/unknown/
// malformed project (scope ∉ ReadScopes) ⇒ uniform 404 (caller cannot even
// learn it exists); a readable-but-not-writable project scope ⇒ 403 (caller
// already knows it exists via read, so 403 leaks nothing). writableBlockScopes
// ⊆ ReadScopes always, so the two checks are consistent.
func (h *ProjectIssuesHandler) resolveWriteScope(w http.ResponseWriter, r *http.Request) (string, *auth.AuthResult, bool) {
	ctx := r.Context()
	ar := AuthResultFromContext(ctx)
	row, err := store.GetProjectByID(ctx, h.pool, chi.URLParam(r, "id"))
	if err != nil {
		slog.Error("project-issues: write scope project load", "error", err, "request_id", RequestIDFromContext(ctx))
		writeInternal(w)
		return "", nil, false
	}
	if row == nil || ar == nil || !slices.Contains(ar.ReadScopes, row.Scope) {
		writeIssueNotFound(w) // 404 uniform: no existence oracle for a foreign project
		return "", nil, false
	}
	if !slices.Contains(writableBlockScopes(ar), row.Scope) {
		// Readable but not writable: 403 (the caller IS authorized to see this
		// scope — this is a write-authorization boundary, not a missing row).
		writeScopeForbidden(w)
		return "", nil, false
	}
	return row.Scope, ar, true
}

// writeScopeForbidden writes the uniform 403 for the write-scope gate (§4.6).
func writeScopeForbidden(w http.ResponseWriter) {
	writeJSON(w, http.StatusForbidden, map[string]any{"success": false, "error": "scope not writable"})
}

// writeRateBlocked applies the per-api_key_id write throttle (the /api/store
// house pattern, §4.4). It returns true when it has ALREADY written a response
// (429 over-limit, or 500 on a limit-lookup error — fail-closed like the store
// path) and the caller must stop. cfg == nil (test wiring / a read-only handler)
// or a non-positive limit ⇒ throttle disabled, returns false. The counter is
// fed by the post-write LogAccess "write" below, shared with /api/store.
func (h *ProjectIssuesHandler) writeRateBlocked(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult) bool {
	if h.cfg == nil {
		return false
	}
	ctx := r.Context()
	limit := h.cfg.SnapshotForRequest(ctx).Query.RateLimitWrite
	if limit <= 0 {
		return false
	}
	count, err := store.CheckRateLimit(ctx, h.pool, ar.ApiKeyID)
	if err != nil {
		slog.Error("project-issues: rate limit check", "error", err, "request_id", RequestIDFromContext(ctx))
		writeInternal(w)
		return true
	}
	if count >= limit {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"success": false, "error": "rate limit exceeded: too many writes per 60 seconds",
		})
		return true
	}
	return false
}

// HandleCreate implements POST /api/project/{id}/issues. Scope is the project's
// (path-resolved, write-gated); the initial workflow status comes from policy
// (WorkflowInitial), a caller-supplied status must be a valid ENTRY transition
// ("" → status) or 422. render:'untrusted', wire-consistent with I-D.
func (h *ProjectIssuesHandler) HandleCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)
	scope, ar, ok := h.resolveWriteScope(w, r)
	if !ok {
		return
	}
	if h.writeRateBlocked(w, r, ar) {
		return
	}
	var p restIssueCreate
	if !decodeIssueBody(w, r, &p) {
		return
	}
	if p.Title == "" {
		writeBadRequest(w, "Missing required field: title")
		return
	}
	set := h.issueSet(r)
	if set == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"success": false, "error": "type registry unavailable"})
		return
	}
	// Resolve the initial status from policy DATA; a client-supplied status must be
	// a valid entry (transition from "") — swapping the registry set changes this
	// verdict with no Go rebuild (§4.3).
	status := set.WorkflowInitial(store.IssueTypeName)
	if p.Status != "" {
		if err := set.ValidateTransition(store.IssueTypeName, "", p.Status); err != nil {
			writeIssueStoreError(w, "issue-create", err, reqID)
			return
		}
		status = p.Status
	}

	b, err := issueTx(ctx, h.pool, func(tx pgx.Tx) (*store.Block, error) {
		// writableScopes = [project scope]: the create is bound to THIS project's
		// single scope, never the caller's whole writable set (scope purity).
		return store.InsertIssueBlock(ctx, tx, store.IssueFields{
			Scope: scope, Title: p.Title, Content: p.Content,
			Tags: p.Tags, Metadata: p.Metadata, Status: status,
		}, []string{scope})
	})
	if err != nil {
		writeIssueStoreError(w, "issue-create", err, reqID)
		return
	}
	logIssueWrite(h.pool, ar.ApiKeyID, b.ID, reqID)
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true, "render": "untrusted", "issue": b,
	})
}

// HandlePatch implements PATCH /api/project/{id}/issues/{block_id}. Fields +
// workflow status; a status transition is validated against POLICY DATA
// (UpdateIssueBlock → set.ValidateTransition) ⇒ 422 on an out-of-policy target.
// A block id outside THIS project's scope reads as 404 uniform (scope purity).
func (h *ProjectIssuesHandler) HandlePatch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)
	scope, ar, ok := h.resolveWriteScope(w, r)
	if !ok {
		return
	}
	blockID := chi.URLParam(r, "block_id")
	if !uuidRe.MatchString(blockID) {
		writeIssueNotFound(w) // malformed id ⇒ uniform 404 (no oracle, no 500 cast)
		return
	}
	if h.writeRateBlocked(w, r, ar) {
		return
	}
	var p restIssueUpdate
	if !decodeIssueBody(w, r, &p) {
		return
	}
	if p.Title == nil && p.Content == nil && p.Tags == nil && p.Metadata == nil && p.Status == nil {
		writeBadRequest(w, "No fields to update (title, content, tags, metadata, status)")
		return
	}
	set := h.issueSet(r)
	if set == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"success": false, "error": "type registry unavailable"})
		return
	}
	b, err := issueTx(ctx, h.pool, func(tx pgx.Tx) (*store.Block, error) {
		// writableScopes = [project scope]: a block in another scope ⇒ ErrIssueNotFound
		// (404 uniform), so the PATCH can never reach across projects.
		return store.UpdateIssueBlock(ctx, tx, blockID, store.IssueUpdate{
			Title: p.Title, Content: p.Content, Tags: p.Tags, Metadata: p.Metadata, Status: p.Status,
		}, set, []string{scope})
	})
	if err != nil {
		writeIssueStoreError(w, "issue-update", err, reqID)
		return
	}
	logIssueWrite(h.pool, ar.ApiKeyID, b.ID, reqID)
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true, "render": "untrusted", "issue": b,
	})
}

// HandleCommentCreate implements POST /api/project/{id}/issues/{block_id}/comments.
// The parent is the path {block_id}; the store forces the comment into the
// PARENT's scope (the comment-scope invariant, §5.2) and re-asserts that scope is
// writable in the same Tx. A parent outside THIS project's scope ⇒ 404 uniform.
func (h *ProjectIssuesHandler) HandleCommentCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)
	scope, ar, ok := h.resolveWriteScope(w, r)
	if !ok {
		return
	}
	parentID := chi.URLParam(r, "block_id")
	if !uuidRe.MatchString(parentID) {
		writeIssueNotFound(w) // malformed parent ⇒ uniform 404 (no oracle)
		return
	}
	if h.writeRateBlocked(w, r, ar) {
		return
	}
	var p restCommentCreate
	if !decodeIssueBody(w, r, &p) {
		return
	}
	b, err := issueTx(ctx, h.pool, func(tx pgx.Tx) (*store.Block, error) {
		// writableScopes = [project scope]: a parent in another scope ⇒
		// ErrLinkScopeViolation (404 uniform) — no cross-project comment.
		return store.InsertCommentBlock(ctx, tx, parentID, store.CommentFields{
			Author: p.Author, Content: p.Content, Metadata: p.Metadata,
		}, []string{scope})
	})
	if err != nil {
		writeIssueStoreError(w, "issue-comment-create", err, reqID)
		return
	}
	logIssueWrite(h.pool, ar.ApiKeyID, b.ID, reqID)
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true, "render": "untrusted", "comment": b,
	})
}

// decodeIssueBody reads the (group-capped 1 MB, server.go DefaultMaxBodySize)
// request body and strict-decodes it into dst. An over-cap body ⇒ 413 (the
// MaxBytesReader verdict of the enclosing group), a malformed/unknown-field body
// ⇒ 400. Returns false when it has written the error response.
func decodeIssueBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"success": false, "error": "request body exceeds 1 MB cap"})
			return false
		}
		writeBadRequest(w, "could not read request body")
		return false
	}
	if msg := decodeStrict(json.RawMessage(raw), dst); msg != "" {
		writeBadRequest(w, msg)
		return false
	}
	return true
}

// logIssueWrite records a "write" access-log row (fire-and-forget, the /api/store
// pattern) so issue writes feed the per-api_key_id rate-limit counter shared with
// /api/store. A logging failure never fails the write (it already committed).
func logIssueWrite(pool *pgxpool.Pool, apiKeyID, blockID, reqID string) {
	if apiKeyID == "" {
		return // unattributed caller (test wiring) — nothing to meter
	}
	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := store.LogAccess(bgCtx, pool, apiKeyID, blockID, "write"); err != nil {
			slog.Error("project-issues: write log", "error", err, "block_id", blockID, "request_id", reqID)
		}
	}()
}
