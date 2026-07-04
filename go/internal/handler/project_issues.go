// /api/project/{id}/issues* — the project-scoped issue READ surface (workflow
// W6, design/03-workflow-api-cli.md §4.2/§4.3/§6.1). Member-gated (scope-read):
// the caller must hold the project's scope in ReadScopes, else a uniform 404 (no
// existence oracle for a foreign project/issue, §5.2). These GETs are the SPA's
// board + list + detail + comment-thread source; every body-bearing response
// carries render:'untrusted' (issue/comment bodies are attacker-controlled
// markdown, §5.4) plus the workflow_status/type fields (wire-consistent with the
// I-D manage transport, context_manage_issues.go).
//
//	GET /api/project/{id}/issues                          list (state,labels,q,sort,after)  member (scope-read)
//	GET /api/project/{id}/issues/{block_id}               detail + first N comments inline   member (scope-read)
//	GET /api/project/{id}/issues/{block_id}/comments      comment thread, ASC keyset          member (scope-read)
//	GET /api/project/{id}/board                           per-status column pages + counts    member (scope-read)
//
// Isolation: the project id resolves to exactly ONE scope; every store read is
// bounded to that single scope (never ar.ReadScopes wholesale), so a block id
// from another project is invisible. Reads use NO block-grants (grants widen
// cross-scope visibility; a project's issue view is deliberately scope-pure).
//
// Access-path routing (§6.1): q != "" ⇒ FTS (existing tsvector GIN, K4);
// sort=created ⇒ the immutable created-keyset (M086); default ⇒ the shipped I-B
// per-status merge over the board index. One mechanism per shape, no duplication.
package handler

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ProjectIssuesHandler serves the /api/project/{id}/issues* read + write surface.
// It holds the block-type registry so the status set (board columns, list merge,
// W7 transition validation) resolves from policy data, never a hardcoded list
// (§4.3 doctrine). cfg is OPTIONAL (nil on a read-only/test wiring): it drives
// only the W7 write throttle (query.rate_limit_write, §4.4) — reads never touch it.
type ProjectIssuesHandler struct {
	pool       *pgxpool.Pool
	blocktypes *blocktype.Registry
	cfg        ConfigStore
}

// NewProjectIssuesHandler wires the pool + the type registry. Use WithConfig to
// attach the runtime config that arms the W7 write rate limit.
func NewProjectIssuesHandler(pool *pgxpool.Pool, blocktypes *blocktype.Registry) *ProjectIssuesHandler {
	return &ProjectIssuesHandler{pool: pool, blocktypes: blocktypes}
}

// WithConfig attaches the runtime config store (arms the W7 write throttle) and
// returns the handler for chaining. Left unset, issue writes are not throttled
// (the read surface and tests that do not exercise the limit pass no config).
func (h *ProjectIssuesHandler) WithConfig(cfg ConfigStore) *ProjectIssuesHandler {
	h.cfg = cfg
	return h
}

// MountProjectIssues mounts the read (W6) AND write (W7) routes behind ONE
// RequireMember group (design/03 §5.1: the gate lives in the mount, so a missing
// gate is a missing route — 404, never fail-open). RequireMember admits; each
// handler then re-scopes to the project's scope — reads via ar.ReadScopes, writes
// via the per-project WRITE-SCOPE gate (resolveWriteScope, §4.6). Distinct from
// MountProject (W4); chi routes the deeper /issues* patterns independently.
func MountProjectIssues(r chi.Router, h *ProjectIssuesHandler) {
	r.Group(func(r chi.Router) {
		r.Use(RequireMember)
		// Reads (W6): member scope-read.
		r.Get("/api/project/{id}/issues", h.HandleList)
		r.Get("/api/project/{id}/issues/{block_id}", h.HandleDetail)
		r.Get("/api/project/{id}/issues/{block_id}/comments", h.HandleComments)
		r.Get("/api/project/{id}/board", h.HandleBoard)
		// Writes (W7): per-project write-scope gate inside each handler.
		r.Post("/api/project/{id}/issues", h.HandleCreate)
		r.Patch("/api/project/{id}/issues/{block_id}", h.HandlePatch)
		r.Post("/api/project/{id}/issues/{block_id}/comments", h.HandleCommentCreate)
	})
}

// resolveScope loads the project of {id} and returns its scope IFF the caller can
// read that scope; otherwise it writes the uniform 404 and returns ok=false. A
// malformed id, an unknown id and a foreign-scope project all share this 404 (no
// oracle, §5.2(1)).
func (h *ProjectIssuesHandler) resolveScope(w http.ResponseWriter, r *http.Request) (string, bool) {
	ctx := r.Context()
	ar := AuthResultFromContext(ctx)
	row, err := store.GetProjectByID(ctx, h.pool, chi.URLParam(r, "id"))
	if err != nil {
		slog.Error("project-issues: project load", "error", err, "request_id", RequestIDFromContext(ctx))
		writeInternal(w)
		return "", false
	}
	if row == nil || ar == nil || !slices.Contains(ar.ReadScopes, row.Scope) {
		writeIssueNotFound(w)
		return "", false
	}
	return row.Scope, true
}

// issueSet resolves the request's type snapshot, or nil (WARN) when the registry
// is unwired — the caller then fails closed (503), mirroring the manage transport.
func (h *ProjectIssuesHandler) issueSet(r *http.Request) *blocktype.Set {
	if h.blocktypes == nil {
		slog.Warn("project-issues: block-type registry not wired — failing closed")
		return nil
	}
	return h.blocktypes.SnapshotForRequest(r.Context())
}

// parseListParams pulls state/labels/q/sort/limit/after off the query string.
// after is a base64 opaque cursor; a malformed one is a 400 (never a 500 on a bad
// token). sort is validated to the closed {updated,created} set (default updated).
func parseListParams(r *http.Request) (state, q, sort string, labels []string, limit int, cursor *store.WorkflowCursor, errMsg string) {
	qv := r.URL.Query()
	state = strings.TrimSpace(qv.Get("state"))
	q = strings.TrimSpace(qv.Get("q"))
	sort = store.SortUpdated
	if qv.Get("sort") == store.SortCreated {
		sort = store.SortCreated
	}
	labels = parseLabels(qv["labels"])
	if s := qv.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			return "", "", "", nil, 0, nil, "limit must be an integer"
		}
		limit = n
	}
	cur, err := decodeCursor(qv.Get("after"))
	if err != nil {
		return "", "", "", nil, 0, nil, "malformed after cursor"
	}
	return state, q, sort, labels, limit, cur, ""
}

// parseLabels flattens repeated ?labels=a&labels=b AND comma-separated
// ?labels=a,b into a deduplicated, non-empty label set.
func parseLabels(raw []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, v := range raw {
		for _, part := range strings.Split(v, ",") {
			p := strings.TrimSpace(part)
			if p != "" && !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}

// HandleList implements GET /api/project/{id}/issues — one keyset page, filtered
// by state/labels/q, ordered by sort (updated|created). Routes to the FTS path
// (q), the created-keyset path (sort=created) or the shipped board merge (default).
func (h *ProjectIssuesHandler) HandleList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	scope, ok := h.resolveScope(w, r)
	if !ok {
		return
	}
	state, q, sort, labels, limit, cursor, errMsg := parseListParams(r)
	if errMsg != "" {
		writeBadRequest(w, errMsg)
		return
	}

	var (
		rows []store.WorkflowBlockRow
		next *store.WorkflowCursor
		err  error
	)
	switch {
	case q != "":
		rows, next, err = store.SearchIssues(ctx, h.pool, store.IssueReadQuery{
			Scope: scope, Q: q, Status: state, Labels: labels, Sort: sort, Limit: limit, Cursor: cursor,
		})
	case sort == store.SortCreated:
		rows, next, err = store.ListIssuesByCreated(ctx, h.pool, store.IssueReadQuery{
			Scope: scope, Status: state, Labels: labels, Limit: limit, Cursor: cursor,
		})
	default:
		set := h.issueSet(r)
		if set == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"success": false, "error": "type registry unavailable"})
			return
		}
		rows, next, err = store.ListIssues(ctx, h.pool, store.IssueListQuery{
			Scopes:   []string{scope},
			Status:   state,
			Statuses: set.WorkflowStates(store.IssueTypeName),
			Labels:   labels,
			Limit:    limit,
			Cursor:   cursor,
		})
	}
	if err != nil {
		if errors.Is(err, store.ErrNoScopes) {
			writeIssueListPage(w, []store.WorkflowBlockRow{}, nil)
			return
		}
		slog.Error("project-issues: list", "error", err, "request_id", RequestIDFromContext(ctx))
		writeInternal(w)
		return
	}
	if rows == nil {
		rows = []store.WorkflowBlockRow{}
	}
	writeIssueListPage(w, rows, next)
}

// HandleDetail implements GET /api/project/{id}/issues/{block_id} — full issue
// fields + the first page of the comment thread inline; the client fetches the
// rest via …/comments. A foreign/absent/malformed block id ⇒ uniform 404.
func (h *ProjectIssuesHandler) HandleDetail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	scope, ok := h.resolveScope(w, r)
	if !ok {
		return
	}
	blockID := chi.URLParam(r, "block_id")
	if !uuidRe.MatchString(blockID) {
		writeIssueNotFound(w)
		return
	}
	// Scope-pure read: readScopes = [project scope], grants = nil (a project's
	// issue view never widens via cross-scope grants, §5.2). A block id in another
	// scope reads as ErrIssueNotFound ⇒ 404 uniform.
	issue, err := store.GetIssue(ctx, h.pool, blockID, []string{scope}, nil)
	if err != nil {
		if errors.Is(err, store.ErrIssueNotFound) {
			writeIssueNotFound(w)
			return
		}
		slog.Error("project-issues: detail get", "error", err, "request_id", RequestIDFromContext(ctx))
		writeInternal(w)
		return
	}
	comments, next, err := store.ListCommentsPage(ctx, h.pool, issue.ID, scope, 0, nil)
	if err != nil {
		slog.Error("project-issues: detail comments", "error", err, "request_id", RequestIDFromContext(ctx))
		writeInternal(w)
		return
	}
	if comments == nil {
		comments = []*store.Block{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true, "render": "untrusted",
		"issue": issue, "comments": comments, "comments_cursor": encodeCursor(next),
	})
}

// HandleComments implements GET /api/project/{id}/issues/{block_id}/comments —
// the ASC keyset comment thread (design/03 §4.2). The parent must be a
// project-scope issue/comment the caller can read (else 404 uniform).
func (h *ProjectIssuesHandler) HandleComments(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	scope, ok := h.resolveScope(w, r)
	if !ok {
		return
	}
	blockID := chi.URLParam(r, "block_id")
	if !uuidRe.MatchString(blockID) {
		writeIssueNotFound(w)
		return
	}
	// Confirm the parent is a readable project-scope block BEFORE listing comments
	// (a foreign parent id ⇒ 404 uniform, no oracle) — GetIssue is scope-pure.
	if _, err := store.GetIssue(ctx, h.pool, blockID, []string{scope}, nil); err != nil {
		if errors.Is(err, store.ErrIssueNotFound) {
			writeIssueNotFound(w)
			return
		}
		slog.Error("project-issues: comments parent", "error", err, "request_id", RequestIDFromContext(ctx))
		writeInternal(w)
		return
	}
	cursor, err := decodeCursor(r.URL.Query().Get("after"))
	if err != nil {
		writeBadRequest(w, "malformed after cursor")
		return
	}
	limit := 0
	if s := r.URL.Query().Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			writeBadRequest(w, "limit must be an integer")
			return
		}
		limit = n
	}
	comments, next, err := store.ListCommentsPage(ctx, h.pool, blockID, scope, limit, cursor)
	if err != nil {
		slog.Error("project-issues: comments list", "error", err, "request_id", RequestIDFromContext(ctx))
		writeInternal(w)
		return
	}
	if comments == nil {
		comments = []*store.Block{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true, "render": "untrusted",
		"comments": comments, "cursor": encodeCursor(next),
	})
}

// HandleBoard implements GET /api/project/{id}/board — the config-status columns,
// each with an index-only count + a first page + per-column resume cursor. The
// status set comes from the type config (never hardcoded); a data status absent
// from the config is unmapped and not shown on the board (§7-W6, documented).
func (h *ProjectIssuesHandler) HandleBoard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	scope, ok := h.resolveScope(w, r)
	if !ok {
		return
	}
	set := h.issueSet(r)
	if set == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"success": false, "error": "type registry unavailable"})
		return
	}
	labels := parseLabels(r.URL.Query()["labels"])
	limit := 0
	if s := r.URL.Query().Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			writeBadRequest(w, "limit must be an integer")
			return
		}
		limit = n
	}
	columns, err := store.BoardColumns(ctx, h.pool, scope, set.WorkflowStates(store.IssueTypeName), labels, limit)
	if err != nil {
		slog.Error("project-issues: board", "error", err, "request_id", RequestIDFromContext(ctx))
		writeInternal(w)
		return
	}
	// Encode each column's cursor to the opaque after-token the list endpoint
	// consumes (?status=<col>&after=…), keeping columns wire-uniform with the list.
	cols := make([]map[string]any, 0, len(columns))
	for _, c := range columns {
		cols = append(cols, map[string]any{
			"status": c.Status, "count": c.Count, "issues": c.Issues, "cursor": encodeCursor(c.Cursor),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true, "render": "untrusted", "columns": cols,
	})
}

// writeIssueListPage writes the standard list envelope (render:'untrusted', the
// rows, and the next opaque cursor or null).
func writeIssueListPage(w http.ResponseWriter, rows []store.WorkflowBlockRow, next *store.WorkflowCursor) {
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true, "render": "untrusted",
		"issues": rows, "cursor": encodeCursor(next),
	})
}

// encodeCursor renders a keyset cursor as an opaque base64url token (or nil at
// end-of-data) — the client echoes it straight back as ?after=.
func encodeCursor(c *store.WorkflowCursor) any {
	if c == nil {
		return nil
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return nil
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// decodeCursor parses the opaque after-token back into a keyset cursor. An empty
// token = first page (nil, nil); a malformed token is an error (⇒ 400, not 500).
func decodeCursor(token string) (*store.WorkflowCursor, error) {
	if token == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, err
	}
	var c store.WorkflowCursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, err
	}
	return &c, nil
}
