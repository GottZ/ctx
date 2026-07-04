// Manage family issue-* (Achse 02, Welle I-D, K2 "Store+Tier" form). This is
// the OPERATOR transport over the store issue logic (store.InsertIssueBlock,
// UpdateIssueBlock, InsertCommentBlock, GetIssue, ListIssues, structural links)
// — the exact mirror of the type-* operator transport (docs/api.md "one write
// logic, two transports"). The PRIMARY UI surface is the REST /api/project
// issue family built in W6/W7 over the SAME store functions; K2 ("REST wins,
// manage collapses onto store functions") is satisfied by that shared logic —
// this transport keeps the family reachable for MCP/CLI operators from day one
// (ctx manage passthrough) without duplicating any mutation logic.
//
// Every response of a body-bearing surface carries render:'untrusted' — issue
// and comment bodies are attacker-controlled markdown (§5.5); the UI MUST take
// the sanitising render path (a missing field cannot be silently overlooked).
//
// All actions are tierOpen (actionTier, S9-pinned): isolation is enforced in the
// store layer via writableBlockScopes (writes) and ar.ReadScopes+grants (reads),
// never by an admin tier (§5.2).
package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"regexp"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5"
)

// uuidRe validates a block id before it reaches the store — a malformed id reads
// as a uniform 404 (no oracle, no 500 on a bad uuid cast).
var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// issueCreatePayload / issueUpdatePayload / commentCreatePayload / linkPayload —
// strict shapes (unknown keys ⇒ 400, decodeStrict, §5.2 typo class).
type issueCreatePayload struct {
	Scope    string         `json:"scope,omitempty"`
	Title    string         `json:"title"`
	Content  string         `json:"content,omitempty"`
	Tags     []string       `json:"tags,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
	Status   string         `json:"status,omitempty"`
}

type issueUpdatePayload struct {
	Title    *string        `json:"title,omitempty"`
	Content  *string        `json:"content,omitempty"`
	Tags     []string       `json:"tags,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
	Status   *string        `json:"status,omitempty"`
}

type commentCreatePayload struct {
	ParentID string         `json:"parent_id"`
	Author   string         `json:"author,omitempty"`
	Content  string         `json:"content,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

type issueListPayload struct {
	Status string                `json:"status,omitempty"`
	Labels []string              `json:"labels,omitempty"`
	Limit  int                   `json:"limit,omitempty"`
	Cursor *store.WorkflowCursor `json:"cursor,omitempty"`
}

type linkPayload struct {
	SourceID  string `json:"source_id"`
	TargetID  string `json:"target_id"`
	LinkClass string `json:"link_class"`
}

// dispatchIssueAction fans the issue-* family out (split from HandleManage for
// the cyclomatic budget, mirrors dispatchTypeAction). Tier gating happened
// upstream in enforceActionTier (all tierOpen).
func (h *ManageHandler) dispatchIssueAction(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	switch req.Action {
	// Achse-02 forge sync family (I-F) shares this Achse-02 dispatch arm (cyclop
	// budget in HandleManage). Routing only — the tier is decided in actionTier.
	case "forge-token-set", "forge-sync-start", "forge-sync-status":
		h.dispatchForgeAction(w, r, req)
		return
	}
	switch req.Action {
	case "issue-create":
		h.handleIssueCreate(w, r, ar, req)
	case "issue-update":
		h.handleIssueUpdate(w, r, ar, req)
	case "issue-get":
		h.handleIssueGet(w, r, ar, req)
	case "issue-list":
		h.handleIssueList(w, r, ar, req)
	case "issue-comment-create":
		h.handleIssueCommentCreate(w, r, ar, req)
	case "issue-link-create":
		h.handleIssueLinkCreate(w, r, ar, req)
	case "issue-link-delete":
		h.handleIssueLinkDelete(w, r, ar, req)
	}
}

// issueSet returns the request's resolved type registry snapshot, or nil (with a
// WARN) when the registry is not wired — the caller then fails closed.
func (h *ManageHandler) issueSet(ctx context.Context) *blocktype.Set {
	if h.blocktypes == nil {
		slog.Warn("manage: block-type registry not wired — issue family fails closed")
		return nil
	}
	return h.blocktypes.SnapshotForRequest(ctx)
}

func (h *ManageHandler) handleIssueCreate(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)
	var p issueCreatePayload
	if msg := decodeStrict(req.Data, &p); msg != "" {
		writeBadRequest(w, msg)
		return
	}
	if p.Title == "" {
		writeBadRequest(w, "Missing required field: title")
		return
	}
	set := h.issueSet(ctx)
	if set == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"success": false, "error": "type registry unavailable"})
		return
	}
	scope := p.Scope
	if scope == "" {
		scope = ar.HomeScope
	}
	// Resolve the initial workflow status from policy (never a hardcoded set);
	// a client-supplied status must be a valid ENTRY (transition from "").
	status := set.WorkflowInitial(store.IssueTypeName)
	if p.Status != "" {
		if err := set.ValidateTransition(store.IssueTypeName, "", p.Status); err != nil {
			h.writeIssueError(w, "issue-create", err, reqID)
			return
		}
		status = p.Status
	}

	b, err := h.inIssueTx(ctx, func(tx pgx.Tx) (*store.Block, error) {
		return store.InsertIssueBlock(ctx, tx, store.IssueFields{
			Scope: scope, Title: p.Title, Content: p.Content,
			Tags: p.Tags, Metadata: p.Metadata, Status: status,
		}, writableBlockScopes(ar))
	})
	if err != nil {
		h.writeIssueError(w, "issue-create", err, reqID)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"action": "issue-create", "success": true, "render": "untrusted", "issue": b,
	})
}

func (h *ManageHandler) handleIssueUpdate(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)
	if !uuidRe.MatchString(req.ID) {
		writeIssueNotFound(w)
		return
	}
	var p issueUpdatePayload
	if msg := decodeStrict(req.Data, &p); msg != "" {
		writeBadRequest(w, msg)
		return
	}
	if p.Title == nil && p.Content == nil && p.Tags == nil && p.Metadata == nil && p.Status == nil {
		writeBadRequest(w, "No fields to update (title, content, tags, metadata, status)")
		return
	}
	set := h.issueSet(ctx)
	if set == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"success": false, "error": "type registry unavailable"})
		return
	}
	b, err := h.inIssueTx(ctx, func(tx pgx.Tx) (*store.Block, error) {
		return store.UpdateIssueBlock(ctx, tx, req.ID, store.IssueUpdate{
			Title: p.Title, Content: p.Content, Tags: p.Tags, Metadata: p.Metadata, Status: p.Status,
		}, set, writableBlockScopes(ar))
	})
	if err != nil {
		h.writeIssueError(w, "issue-update", err, reqID)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"action": "issue-update", "success": true, "render": "untrusted", "issue": b,
	})
}

func (h *ManageHandler) handleIssueGet(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)
	if !uuidRe.MatchString(req.ID) {
		writeIssueNotFound(w)
		return
	}
	grants := resolveGrants(ctx, h.pool, ar)
	issue, err := store.GetIssue(ctx, h.pool, req.ID, ar.ReadScopes, grants)
	if err != nil {
		h.writeIssueError(w, "issue-get", err, reqID)
		return
	}
	comments, err := store.ListComments(ctx, h.pool, issue.ID, ar.ReadScopes, grants)
	if err != nil {
		slog.Error("manage: issue-get comments", "error", err, "request_id", reqID)
		writeInternal(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"action": "issue-get", "success": true, "render": "untrusted",
		"issue": issue, "comments": comments,
	})
}

func (h *ManageHandler) handleIssueList(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)
	var p issueListPayload
	if len(req.Data) > 0 {
		if msg := decodeStrict(req.Data, &p); msg != "" {
			writeBadRequest(w, msg)
			return
		}
	}
	set := h.issueSet(ctx)
	if set == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"success": false, "error": "type registry unavailable"})
		return
	}
	rows, cursor, err := store.ListIssues(ctx, h.pool, store.IssueListQuery{
		Scopes:   ar.ReadScopes, // fail-closed: empty ⇒ store RequireScopes error
		Status:   p.Status,
		Statuses: set.WorkflowStates(store.IssueTypeName),
		Labels:   p.Labels,
		Limit:    p.Limit,
		Cursor:   p.Cursor,
	})
	if err != nil {
		if errors.Is(err, store.ErrNoScopes) {
			// No readable scope ⇒ empty result, never an error to the client.
			writeJSON(w, http.StatusOK, map[string]any{
				"action": "issue-list", "success": true, "render": "untrusted",
				"issues": []any{}, "cursor": nil,
			})
			return
		}
		slog.Error("manage: issue-list", "error", err, "request_id", reqID)
		writeInternal(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"action": "issue-list", "success": true, "render": "untrusted",
		"issues": rows, "cursor": cursor,
	})
}

func (h *ManageHandler) handleIssueCommentCreate(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)
	var p commentCreatePayload
	if msg := decodeStrict(req.Data, &p); msg != "" {
		writeBadRequest(w, msg)
		return
	}
	if p.ParentID == "" {
		writeBadRequest(w, "Missing required field: parent_id")
		return
	}
	if !uuidRe.MatchString(p.ParentID) {
		writeIssueNotFound(w) // malformed parent = uniform 404 (no oracle)
		return
	}
	b, err := h.inIssueTx(ctx, func(tx pgx.Tx) (*store.Block, error) {
		return store.InsertCommentBlock(ctx, tx, p.ParentID, store.CommentFields{
			Author: p.Author, Content: p.Content, Metadata: p.Metadata,
		}, writableBlockScopes(ar))
	})
	if err != nil {
		h.writeIssueError(w, "issue-comment-create", err, reqID)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"action": "issue-comment-create", "success": true, "render": "untrusted", "comment": b,
	})
}

func (h *ManageHandler) handleIssueLinkCreate(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)
	var p linkPayload
	if msg := decodeStrict(req.Data, &p); msg != "" {
		writeBadRequest(w, msg)
		return
	}
	if !uuidRe.MatchString(p.SourceID) || !uuidRe.MatchString(p.TargetID) || p.LinkClass == "" {
		// Malformed ids read as a uniform 404 (no oracle); a missing class is 400.
		if p.LinkClass == "" {
			writeBadRequest(w, "Missing required field: link_class")
			return
		}
		writeIssueNotFound(w)
		return
	}
	set := h.issueSet(ctx)
	if set == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"success": false, "error": "type registry unavailable"})
		return
	}
	_, err := h.inIssueTx(ctx, func(tx pgx.Tx) (*store.Block, error) {
		// Establish the source is writable AND resolve its type FIRST — a
		// foreign/absent source reads as ErrLinkScopeViolation (uniform 404),
		// BEFORE any class check, so the class error cannot become an existence
		// oracle for a foreign block (§5.2).
		typeName, err := issueLinkSourceType(ctx, tx, p.SourceID, writableBlockScopes(ar))
		if err != nil {
			return nil, err
		}
		if err := validateLinkClass(set, typeName, p.LinkClass); err != nil {
			return nil, err
		}
		return nil, store.PutStructuralLink(ctx, tx, store.StructuralLink{
			SourceID: p.SourceID, TargetID: p.TargetID, LinkClass: p.LinkClass, Origin: "manual",
		}, writableBlockScopes(ar))
	})
	if err != nil {
		h.writeIssueError(w, "issue-link-create", err, reqID)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"action": "issue-link-create", "success": true,
		"link": map[string]string{"source_id": p.SourceID, "target_id": p.TargetID, "link_class": p.LinkClass},
	})
}

func (h *ManageHandler) handleIssueLinkDelete(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)
	var p linkPayload
	if msg := decodeStrict(req.Data, &p); msg != "" {
		writeBadRequest(w, msg)
		return
	}
	if p.LinkClass == "" {
		writeBadRequest(w, "Missing required field: link_class")
		return
	}
	if !uuidRe.MatchString(p.SourceID) || !uuidRe.MatchString(p.TargetID) {
		writeIssueNotFound(w)
		return
	}
	_, err := h.inIssueTx(ctx, func(tx pgx.Tx) (*store.Block, error) {
		return nil, store.DeleteStructuralLink(ctx, tx, p.SourceID, p.TargetID, p.LinkClass, writableBlockScopes(ar))
	})
	if err != nil {
		h.writeIssueError(w, "issue-link-delete", err, reqID)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"action": "issue-link-delete", "success": true,
		"link": map[string]string{"source_id": p.SourceID, "target_id": p.TargetID, "link_class": p.LinkClass},
	})
}

// issueLinkSourceType returns the source block's type_name IFF it exists AND its
// scope is writable by the caller; otherwise ErrLinkScopeViolation (uniform, no
// oracle — identical to PutStructuralLink's own verdict).
func issueLinkSourceType(ctx context.Context, tx pgx.Tx, sourceID string, writableScopes []string) (string, error) {
	var scope, typeName string
	err := tx.QueryRow(ctx,
		`SELECT scope, type_name FROM context_blocks WHERE id = $1::uuid AND NOT is_archived FOR SHARE`,
		sourceID).Scan(&scope, &typeName)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", store.ErrLinkScopeViolation
	}
	if err != nil {
		return "", err
	}
	for _, s := range writableScopes {
		if s == scope {
			return typeName, nil
		}
	}
	return "", store.ErrLinkScopeViolation
}

// validateLinkClass enforces the type's structural_link_classes allowlist
// (design/02 §4.1/§4.3): the class must be one the source type declares. A type
// that declares none permits no structural links (fail-closed).
func validateLinkClass(set *blocktype.Set, typeName, class string) error {
	p, ok := set.Resolve(typeName)
	if !ok {
		return store.ErrLinkScopeViolation // unknown type = treat as foreign
	}
	for _, c := range p.StructuralLinkClasses {
		if c == class {
			return nil
		}
	}
	return errInvalidLinkClass
}

// errInvalidLinkClass ⇒ 422 (the source exists and is writable, but the class is
// not permitted for its type). Distinct from ErrLinkScopeViolation (404).
var errInvalidLinkClass = errors.New("link_class not permitted for the source block type")

// inIssueTx runs fn in a transaction, committing on success and rolling back on
// error. Returns fn's block (nil for link/void writes).
func (h *ManageHandler) inIssueTx(ctx context.Context, fn func(tx pgx.Tx) (*store.Block, error)) (*store.Block, error) {
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op
	b, err := fn(tx)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return b, nil
}

// writeIssueError maps the issue store sentinels onto HTTP statuses: not-found/
// scope violations ⇒ uniform 404 (no oracle); transition + body-cap + class ⇒
// 422; everything else ⇒ 500 (logged, no wire detail).
func (h *ManageHandler) writeIssueError(w http.ResponseWriter, action string, err error, reqID string) {
	switch {
	case errors.Is(err, store.ErrIssueNotFound), errors.Is(err, store.ErrLinkScopeViolation):
		writeIssueNotFound(w)
	case errors.Is(err, store.ErrIssueScope):
		// Requested a write scope the key cannot write — 403 (the caller IS
		// authenticated; this is an authorization boundary, not a missing row).
		writeJSON(w, http.StatusForbidden, map[string]any{"success": false, "error": "scope not writable"})
	case errors.Is(err, store.ErrCommentParentRequired):
		writeBadRequest(w, "comment requires a parent_id")
	case errors.Is(err, store.ErrIssueBody):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"success": false, "error": "body exceeds 50 KB cap"})
	case errors.Is(err, blocktype.ErrInvalidTransition),
		errors.Is(err, blocktype.ErrNoWorkflow),
		errors.Is(err, blocktype.ErrUnknownType):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"success": false, "error": err.Error()})
	case errors.Is(err, errInvalidLinkClass):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"success": false, "error": err.Error()})
	default:
		slog.Error(action+" error", "error", err, "request_id", reqID)
		writeInternal(w)
	}
}

func writeBadRequest(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": msg})
}

func writeIssueNotFound(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "not found"})
}

func writeInternal(w http.ResponseWriter) {
	writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "Internal server error"})
}
