// Package store — issue READ access paths for the REST /api/project issue surface
// (Achse 03, Welle W6, design/03-workflow-api-cli.md §4.2/§4.3/§6.1). These sit
// beside the shipped I-B board primitive (ListWorkflowBlocks / WorkflowStatusListSQL)
// and the I-D issue store (issues.go). The REST list endpoint routes to one of
// THREE access paths, each index-backed at the 10k-issues/repo target scale
// (§6.1 EXPLAIN gate). One project = one scope, so every path takes a SINGLE,
// handler-verified scope (scope ∈ ar.ReadScopes was already checked): the row set
// is bounded to the project's scope, never trusted from the client.
//
//   - default (no q, sort=updated) ⇒ the shipped ListIssues / ListWorkflowBlocks
//     per-status k-way merge over idx_blocks_workflow_board (Sort-free). NOT in
//     this file — the handler calls ListIssues directly (no duplication).
//   - q != "" (any sort)           ⇒ SearchIssues: FTS bitmap over the EXISTING
//     tsvector GIN indexes idx_context_ts_de / idx_context_ts_en (K4 decision,
//     §6.1: q binds to the existing FTS path, NOT a new trigram index). The FTS
//     result set is selective ⇒ a Top-N Sort over it is cheap (unlike the board
//     path, the "no Sort" rule does not apply to the small FTS set).
//   - sort=created (no q)          ⇒ ListIssuesByCreated: ordered range scan over
//     idx_blocks_workflow_created (M086), immutable keyset (created_at, id) —
//     lossless traversal (§6.1). created_at is monotone across statuses ⇒ ONE
//     scan serves both status-filtered and status-unfiltered (no merge).
//
// Board (BoardColumns): per config status, one board-index page + an index-only
// count(*) over idx_blocks_workflow_board (§6.1 — counts are index-only when no
// label filter is set; a label filter degrades the count to a heap filter, a
// documented edge). labels are an AND post-filter over metadata->'labels' (the
// I-B semantics, workflow.go §3.3 — NOT a dedicated GIN index; see §6.1 deviation
// note in docs/api.md).
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SortUpdated / SortCreated are the two keyset orderings the issue list exposes.
const (
	SortUpdated = "updated"
	SortCreated = "created"
)

// IssueReadQuery is the single-scope read request behind the REST list/search
// endpoints. Scope is required (the handler-verified project scope); TypeName is
// fixed to "issue" by the caller. Sort selects the keyset ordering.
type IssueReadQuery struct {
	Scope  string   // handler-verified project scope (single); fail-closed if empty
	Q      string   // FTS query (SearchIssues only; the handler routes on Q != "")
	Status string   // optional workflow_status equality filter ("" = all statuses)
	Labels []string // AND post-filter over metadata->'labels' (documented, §3.3)
	Sort   string   // SortUpdated (default) | SortCreated
	Limit  int      // clamped to [1, MaxWorkflowListLimit]
	Cursor *WorkflowCursor
}

// IssueSearchUpdatedSQL / IssueSearchCreatedSQL — the q (FTS) path. The FTS
// predicate (ts_de OR ts_en) drives a BitmapOr over idx_context_ts_de +
// idx_context_ts_en (the existing retrieval-core GIN indexes, M001/M044); scope,
// type_name, status, the keyset boundary and the label set are rechecked on the
// bitmap result, then a Top-N Sort orders the (selective) FTS set. Exported so
// the W6 EXPLAIN gate EXPLAINs THESE exact strings (no SQL copy, M072/M075 line).
//
// The deny-list conjunct (C2-2 / OPS-W1 review A3) is what keeps the FTS bitmap
// reachable: since migration 145 both GIN indexes are PARTIAL over
// `type_name NOT IN ('checkpoint','system-meta')`, and `b.type_name = $2` is a
// PARAMETER — it proves the index predicate only while the plan cache serves a
// custom plan. Under the generic plan (pgx statement cache, from the 6th
// execution per connection) the proof fails and both FTS indexes drop out of the
// plan: measured 774 → 14 126 at 100 000 rows, the FTS predicate demoted to a
// heap filter. The conjunct is a strict no-op on the row set — $2 is bound to
// IssueTypeName at the only call site (SearchIssues, below), which is not a
// deny-listed name (pinned by TestC22IssueFTSConjunctIsRedundant).
const IssueSearchUpdatedSQL = `
	SELECT b.id::text, b.scope, b.type_name, b.title, b.workflow_status, b.updated_at, b.created_at
	FROM context_blocks b
	WHERE b.scope = $1::text
	  AND b.type_name = $2
	  AND b.type_name NOT IN ` + hardFTSDenyValues + `
	  AND NOT b.is_archived
	  AND ($3::text = '' OR b.workflow_status = $3)
	  AND (b.ts_de @@ plainto_tsquery('german', $4) OR b.ts_en @@ plainto_tsquery('english', $4))
	  AND (b.updated_at, b.id) < ($5::timestamptz, $6::uuid)
	  AND ($7::text[] IS NULL OR b.metadata->'labels' ?& $7::text[])
	ORDER BY b.updated_at DESC, b.id DESC
	LIMIT $8`

// IssueSearchCreatedSQL is IssueSearchUpdatedSQL with the immutable (created_at,
// id) keyset + ordering (the q + ?sort=created combination) — including its
// deny-list conjunct, for the same reason and with the same no-op argument.
const IssueSearchCreatedSQL = `
	SELECT b.id::text, b.scope, b.type_name, b.title, b.workflow_status, b.updated_at, b.created_at
	FROM context_blocks b
	WHERE b.scope = $1::text
	  AND b.type_name = $2
	  AND b.type_name NOT IN ` + hardFTSDenyValues + `
	  AND NOT b.is_archived
	  AND ($3::text = '' OR b.workflow_status = $3)
	  AND (b.ts_de @@ plainto_tsquery('german', $4) OR b.ts_en @@ plainto_tsquery('english', $4))
	  AND (b.created_at, b.id) < ($5::timestamptz, $6::uuid)
	  AND ($7::text[] IS NULL OR b.metadata->'labels' ?& $7::text[])
	ORDER BY b.created_at DESC, b.id DESC
	LIMIT $8`

// IssueCreatedListSQL — the ?sort=created path (no q). An ORDERED range scan over
// idx_blocks_workflow_created (M086): the leading scalar equalities (scope,
// type_name) + the partial predicate (workflow_status IS NOT NULL AND NOT
// is_archived) select the index, the all-DESC keyset (created_at, id) < (cur)
// walks it Sort-free. The status filter is an in-scan filter (created_at is not
// status-keyed — one scan for all statuses). Exported for the EXPLAIN gate.
const IssueCreatedListSQL = `
	SELECT b.id::text, b.scope, b.type_name, b.title, b.workflow_status, b.updated_at, b.created_at
	FROM context_blocks b
	WHERE b.scope = $1::text
	  AND b.type_name = $2
	  AND b.workflow_status IS NOT NULL
	  AND NOT b.is_archived
	  AND ($3::text = '' OR b.workflow_status = $3)
	  AND (b.created_at, b.id) < ($4::timestamptz, $5::uuid)
	  AND ($6::text[] IS NULL OR b.metadata->'labels' ?& $6::text[])
	ORDER BY b.created_at DESC, b.id DESC
	LIMIT $7`

// IssueBoardCountSQL — the per-status board count. Equality on all three leading
// board-index keys (scope, type_name, workflow_status) over the partial index
// idx_blocks_workflow_board ⇒ an Index Only Scan (no heap) once the visibility
// map is set (§6.1: counts are index-only). A label filter (last predicate)
// forces a heap Filter and forfeits index-only — a documented edge, gated only in
// the no-label case. Exported for the EXPLAIN gate.
const IssueBoardCountSQL = `
	SELECT count(*)
	FROM context_blocks b
	WHERE b.scope = $1::text
	  AND b.type_name = $2
	  AND b.workflow_status = $3
	  AND NOT b.is_archived
	  AND ($4::text[] IS NULL OR b.metadata->'labels' ?& $4::text[])`

// issueReadRow scans the 7-column read projection (WorkflowBlockRow + created_at
// for the created-sort cursor). created_at never reaches the wire on the updated
// path; it drives the cursor only on the created path.
type issueReadRow struct {
	WorkflowBlockRow
	CreatedAt time.Time
}

// firstPageKeyset returns the (+infinity, max-uuid) sentinel so a first-page
// tuple comparison admits every row while staying an ordered index range scan.
func firstPageKeyset(cursor *WorkflowCursor, sort string) (pgtype.Timestamptz, string) {
	if cursor == nil {
		return pgtype.Timestamptz{InfinityModifier: pgtype.Infinity, Valid: true}, maxUUID
	}
	if sort == SortCreated {
		return pgtype.Timestamptz{Time: cursor.CreatedAt, Valid: true}, cursor.ID
	}
	return pgtype.Timestamptz{Time: cursor.UpdatedAt, Valid: true}, cursor.ID
}

// labelArg maps an empty label set to a nil arg so the SQL `$n IS NULL` branch
// short-circuits the post-filter.
func labelArg(labels []string) any {
	if len(labels) > 0 {
		return labels
	}
	return nil
}

// nextIssueCursor returns the resume cursor for a full page (a short page = end
// of data ⇒ nil). The boundary column matches the sort so the next page resumes
// on the same keyset the SQL walks.
func nextIssueCursor(rows []issueReadRow, limit int, sort string) *WorkflowCursor {
	if len(rows) < limit {
		return nil
	}
	last := rows[len(rows)-1]
	c := &WorkflowCursor{Status: last.WorkflowStatus, ID: last.ID}
	if sort == SortCreated {
		c.CreatedAt = last.CreatedAt
	} else {
		c.UpdatedAt = last.UpdatedAt
	}
	return c
}

// scanIssueReadRows drains a 7-column result into read rows + the wire rows.
func scanIssueReadRows(rows pgx.Rows) ([]issueReadRow, []WorkflowBlockRow, error) {
	defer rows.Close()
	var raw []issueReadRow
	var wire []WorkflowBlockRow
	for rows.Next() {
		var r issueReadRow
		if err := rows.Scan(&r.ID, &r.Scope, &r.TypeName, &r.Title, &r.WorkflowStatus, &r.UpdatedAt, &r.CreatedAt); err != nil {
			return nil, nil, fmt.Errorf("issue read: scan: %w", err)
		}
		raw = append(raw, r)
		wire = append(wire, r.WorkflowBlockRow)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("issue read: rows: %w", err)
	}
	return raw, wire, nil
}

// SearchIssues returns one keyset page of issue blocks matching the FTS query q,
// riding the tsvector GIN indexes (K4/§6.1). Sort selects the keyset ordering.
// Fail-closed on an empty scope or an empty q (the handler routes here only when
// q != "").
func SearchIssues(ctx context.Context, pool *pgxpool.Pool, q IssueReadQuery) ([]WorkflowBlockRow, *WorkflowCursor, error) {
	if q.Scope == "" {
		return nil, nil, ErrNoScopes
	}
	if q.Q == "" {
		return nil, nil, fmt.Errorf("issue search: empty query")
	}
	limit := clampWorkflowLimit(q.Limit)
	curTS, curID := firstPageKeyset(q.Cursor, q.Sort)
	sql := IssueSearchUpdatedSQL
	if q.Sort == SortCreated {
		sql = IssueSearchCreatedSQL
	}
	rows, err := pool.Query(ctx, sql, q.Scope, IssueTypeName, q.Status, q.Q, curTS, curID, labelArg(q.Labels), limit)
	if err != nil {
		return nil, nil, fmt.Errorf("issue search: query: %w", err)
	}
	raw, wire, err := scanIssueReadRows(rows)
	if err != nil {
		return nil, nil, err
	}
	return wire, nextIssueCursor(raw, limit, q.Sort), nil
}

// ListIssuesByCreated returns one keyset page ordered by the IMMUTABLE created_at
// (the ?sort=created lossless traversal, §6.1), riding idx_blocks_workflow_created
// (M086). Handles both status-filtered and status-unfiltered in ONE scan.
func ListIssuesByCreated(ctx context.Context, pool *pgxpool.Pool, q IssueReadQuery) ([]WorkflowBlockRow, *WorkflowCursor, error) {
	if q.Scope == "" {
		return nil, nil, ErrNoScopes
	}
	limit := clampWorkflowLimit(q.Limit)
	curTS, curID := firstPageKeyset(q.Cursor, SortCreated)
	rows, err := pool.Query(ctx, IssueCreatedListSQL, q.Scope, IssueTypeName, q.Status, curTS, curID, labelArg(q.Labels), limit)
	if err != nil {
		return nil, nil, fmt.Errorf("issue created-list: query: %w", err)
	}
	raw, wire, err := scanIssueReadRows(rows)
	if err != nil {
		return nil, nil, err
	}
	return wire, nextIssueCursor(raw, limit, SortCreated), nil
}

// minUUID is the keyset LOWER sentinel for a first-page ASC scan: no real id is
// below it, so (created_at, id) > (-inf, minUUID) admits every row.
const minUUID = "00000000-0000-0000-0000-000000000000"

// CommentPageSQL — one ASC keyset page of a comment thread (oldest first, thread
// order; comments are immutable in order, §4.3). Rides idx_parent_id (M001) on
// parent_id equality; the (created_at, id) keyset walks the small per-issue set.
// scope-restricted to the parent's project scope (the handler passes the verified
// scope) so a foreign comment id can never surface. Uses issueScanCols (issues.go)
// so the wire shape is byte-identical to the inline thread in issue-detail.
const CommentPageSQL = `
	SELECT ` + issueScanCols + `
	FROM context_blocks
	WHERE parent_id = $1::uuid
	  AND NOT is_archived
	  AND type_name = '` + CommentTypeName + `'
	  AND scope = $2::text
	  AND (created_at, id) > ($3::timestamptz, $4::uuid)
	ORDER BY created_at ASC, id ASC
	LIMIT $5`

// ListCommentsPage returns one ASC keyset page of the comment thread under
// parentID within scope, plus the next cursor (nil = end of thread). Fail-closed
// on an empty scope. Unlike the shipped ListComments (which returns the whole
// thread for the inline hydrate), this paginates for large threads (§4.2 comments
// endpoint). The cursor keys on the IMMUTABLE (created_at, id) — comments are not
// reordered, so the ASC traversal is lossless.
func ListCommentsPage(ctx context.Context, pool *pgxpool.Pool, parentID, scope string, limit int, cursor *WorkflowCursor) ([]*Block, *WorkflowCursor, error) {
	if scope == "" {
		return nil, nil, ErrNoScopes
	}
	limit = clampWorkflowLimit(limit)
	curTS := pgtype.Timestamptz{InfinityModifier: pgtype.NegativeInfinity, Valid: true}
	curID := minUUID
	if cursor != nil {
		curTS = pgtype.Timestamptz{Time: cursor.CreatedAt, Valid: true}
		curID = cursor.ID
	}
	rows, err := pool.Query(ctx, CommentPageSQL, parentID, scope, curTS, curID, limit)
	if err != nil {
		return nil, nil, fmt.Errorf("comment page: query: %w", err)
	}
	defer rows.Close()
	var out []*Block
	for rows.Next() {
		b, err := scanIssue(rows)
		if err != nil {
			return nil, nil, fmt.Errorf("comment page: scan: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("comment page: rows: %w", err)
	}
	var next *WorkflowCursor
	if len(out) == limit {
		last := out[len(out)-1]
		next = &WorkflowCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return out, next, nil
}

// IssueBoardColumn is one board column: the config status, the total live count
// in that status (index-only, no-label case), the first page of issues (board
// order, updated_at DESC) and the per-column resume cursor (nil = column fully on
// the page). The UI loads more of a column via GET …/issues?status=<col>&after=.
type IssueBoardColumn struct {
	Status string             `json:"status"`
	Count  int                `json:"count"`
	Issues []WorkflowBlockRow `json:"issues"`
	Cursor *WorkflowCursor    `json:"cursor"`
}

// BoardColumns returns one board page: for each config status a first page (board
// index, updated order) + an index-only count. statuses is the type's config set
// (resolver-provided; the handler never hardcodes it). A status present in the
// data but ABSENT from statuses is not returned — its rows are unreachable via
// the board (documented "unmapped status" behavior, §7-W6); the flat list
// (GET …/issues without status) still surfaces them via the config-set merge.
func BoardColumns(ctx context.Context, pool *pgxpool.Pool, scope string, statuses, labels []string, limit int) ([]IssueBoardColumn, error) {
	if scope == "" {
		return nil, ErrNoScopes
	}
	limit = clampWorkflowLimit(limit)
	top := pgtype.Timestamptz{InfinityModifier: pgtype.Infinity, Valid: true}
	out := make([]IssueBoardColumn, 0, len(statuses))
	for _, status := range statuses {
		col := IssueBoardColumn{Status: status, Issues: []WorkflowBlockRow{}}

		var count int
		if err := pool.QueryRow(ctx, IssueBoardCountSQL, scope, IssueTypeName, status, labelArg(labels)).Scan(&count); err != nil {
			return nil, fmt.Errorf("board count (status=%q): %w", status, err)
		}
		col.Count = count

		rows, err := pool.Query(ctx, WorkflowStatusListSQL, scope, IssueTypeName, status, top, maxUUID, labelArg(labels), limit)
		if err != nil {
			return nil, fmt.Errorf("board page (status=%q): %w", status, err)
		}
		page := make([]WorkflowBlockRow, 0, limit)
		for rows.Next() {
			var r WorkflowBlockRow
			if err := rows.Scan(&r.ID, &r.Scope, &r.TypeName, &r.Title, &r.WorkflowStatus, &r.UpdatedAt); err != nil {
				rows.Close()
				return nil, fmt.Errorf("board scan (status=%q): %w", status, err)
			}
			page = append(page, r)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("board rows (status=%q): %w", status, err)
		}
		rows.Close()
		if len(page) > 0 {
			col.Issues = page
		}
		col.Cursor = nextWorkflowCursor(page, limit)
		out = append(out, col)
	}
	return out, nil
}
