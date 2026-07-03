// Package store — workflow board listing primitives (Achse 02, Welle I-B).
//
// context_blocks.workflow_status (migration 077) is the per-block workflow VALUE;
// the SET of valid states is Achse-01 type-config policy (blocktype registry).
// This file is the listing MECHANISM behind idx_blocks_workflow_board (077,
// partial: WHERE workflow_status IS NOT NULL AND NOT is_archived, keyset
// (scope, type_name, workflow_status, updated_at DESC, id)):
//
//   - Status GIVEN   ⇒ ONE ordered index range scan over the board index, keyset
//     (updated_at DESC, id DESC) — one board column at 10k+ issues/repo.
//   - Status EMPTY   ⇒ per-status merge: ONE range scan per config status, then
//     a k-way merge in Go on (updated_at DESC, id DESC). NEVER a Sort over the
//     whole scope — the index cannot order across workflow_status values (it is
//     a higher-order key than updated_at), so a naive single ORDER BY over all
//     statuses would force a Sort node; the merge avoids it (§3.3/§6.2).
//
// The keyset cursor for the merge is a single (status, updated_at, id) triple:
// block ids are globally unique, so the (updated_at, id) boundary applied
// uniformly to every per-status sub-scan yields exactly the not-yet-emitted rows
// — lossless and duplicate-free across page boundaries. Labels are a documented
// post-filter inside the range scan (metadata->'labels'), not an index (§3.3).
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Workflow list limit clamps (design/02 §3.3/§6.2: server-clamp ≤ 100/page).
const (
	// MaxWorkflowListLimit caps one page regardless of the requested limit.
	MaxWorkflowListLimit = 100
	// DefaultWorkflowListLimit applies when the caller passes limit ≤ 0.
	DefaultWorkflowListLimit = 50
)

// WorkflowCursor is the keyset position for the next page. For the status-filtered
// path Status echoes the filtered status; for the merge path it is the status of
// the last emitted row (informational — the (UpdatedAt, ID) boundary drives the
// resume uniformly across all per-status scans).
type WorkflowCursor struct {
	Status    string    `json:"status"`
	UpdatedAt time.Time `json:"updated_at"`
	ID        string    `json:"id"`
}

// WorkflowBlockRow is the minimal board row this primitive returns. I-D2 hydrates
// the full issue response (comments, duplicate flag, …) on top of these ids.
type WorkflowBlockRow struct {
	ID             string    `json:"id"`
	Scope          string    `json:"scope"`
	TypeName       string    `json:"type_name"`
	Title          string    `json:"title"`
	WorkflowStatus string    `json:"workflow_status"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// WorkflowListQuery describes one board page request.
type WorkflowListQuery struct {
	// Scopes is the caller-resolved ReadScopes intersection (fail-closed: empty
	// ⇒ error). Scope isolation is enforced here, never trusted from the client.
	Scopes []string
	// TypeName fixes the workflow type (e.g. "issue"); required.
	TypeName string
	// Status selects one board column. Empty ⇒ per-status merge over Statuses.
	Status string
	// Statuses is the type's config status set (blocktype.Set.WorkflowStates) —
	// required when Status is empty (the merge streams). Ignored otherwise.
	Statuses []string
	// Labels is an AND post-filter over metadata->'labels' (documented, §3.3);
	// nil/empty = no label filter.
	Labels []string
	// Limit is clamped to [1, MaxWorkflowListLimit].
	Limit int
	// Cursor resumes a previous page (nil = first page).
	Cursor *WorkflowCursor
}

// WorkflowStatusListSQL is the single production query for ONE (scope, status)
// board stream: an ORDERED index range scan over idx_blocks_workflow_board. ALL
// three leading index columns (scope, type_name, workflow_status) are matched by
// SCALAR equality — a `scope = ANY(array)` on the leading column would force a
// BitmapOr + Sort (PG cannot emit globally ordered rows across multiple leading
// values), so scopes are iterated in Go and k-way merged, exactly like statuses.
// The keyset is the row-comparison tuple (updated_at, id) < ($4, $5), matching
// the all-DESC index direction — no Sort, no bitmap. The first page passes the
// +infinity / max-uuid sentinel so ALL rows qualify and the same single string
// serves first and subsequent pages. Label AND post-filter on metadata->'labels'.
// Exported so the I-B EXPLAIN gate (store_test package, which cannot be in-package
// because testdb imports store) EXPLAINs THIS exact string — never a copy
// (M072/M075 no-duplication line).
const WorkflowStatusListSQL = `
	SELECT b.id::text, b.scope, b.type_name, b.title, b.workflow_status, b.updated_at
	FROM context_blocks b
	WHERE b.scope = $1::text
	  AND b.type_name = $2
	  AND b.workflow_status = $3
	  AND NOT b.is_archived
	  AND (b.updated_at, b.id) < ($4::timestamptz, $5::uuid)
	  AND ($6::text[] IS NULL OR b.metadata->'labels' ?& $6::text[])
	ORDER BY b.updated_at DESC, b.id DESC
	LIMIT $7`

// maxUUID is the keyset upper sentinel for the first page: no real block id
// exceeds it, so (updated_at, id) < (+inf, maxUUID) admits every row.
const maxUUID = "ffffffff-ffff-ffff-ffff-ffffffffffff"

func clampWorkflowLimit(n int) int {
	if n <= 0 {
		return DefaultWorkflowListLimit
	}
	if n > MaxWorkflowListLimit {
		return MaxWorkflowListLimit
	}
	return n
}

// ListWorkflowBlocks returns one keyset page of workflow blocks and the cursor
// for the next page (nil when the page is not full = end of data). It rides
// idx_blocks_workflow_board in both modes (see the package doc). Fail-closed on
// empty Scopes; TypeName required; merge mode requires Statuses.
func ListWorkflowBlocks(ctx context.Context, pool *pgxpool.Pool, q WorkflowListQuery) ([]WorkflowBlockRow, *WorkflowCursor, error) {
	if err := RequireScopes(q.Scopes); err != nil {
		return nil, nil, err
	}
	if q.TypeName == "" {
		return nil, nil, fmt.Errorf("workflow list: type_name required")
	}
	limit := clampWorkflowLimit(q.Limit)

	// Unify both modes: status-filtered = one status; status-los = the config
	// status set. Each (scope × status) pair is ONE ordered index range scan;
	// they k-way merge into the global page. A single-scope single-status board
	// (the common case) is exactly ONE scan (§6.2).
	statuses := q.Statuses
	if q.Status != "" {
		statuses = []string{q.Status}
	} else if len(statuses) == 0 {
		return nil, nil, fmt.Errorf("workflow list: status-unfiltered mode requires the config status set")
	}

	streams := make([][]WorkflowBlockRow, 0, len(statuses)*len(q.Scopes))
	for _, status := range statuses {
		for _, scope := range q.Scopes {
			rows, err := listWorkflowScopeStatus(ctx, pool, q, scope, status, limit)
			if err != nil {
				return nil, nil, err
			}
			if len(rows) > 0 {
				streams = append(streams, rows)
			}
		}
	}
	merged := kwayMergeWorkflow(streams, limit)
	return merged, nextWorkflowCursor(merged, limit), nil
}

// listWorkflowScopeStatus is ONE ordered index range scan over
// idx_blocks_workflow_board for a single (scope, status), keyset-ordered
// (updated_at DESC, id DESC) — no Sort (all leading index columns are scalar
// equality; see WorkflowStatusListSQL).
func listWorkflowScopeStatus(ctx context.Context, pool *pgxpool.Pool, q WorkflowListQuery, scope, status string, limit int) ([]WorkflowBlockRow, error) {
	// First page: +infinity / max-uuid sentinel so the tuple comparison admits
	// every row while staying a clean ordered index range scan.
	curUpdated := pgtype.Timestamptz{InfinityModifier: pgtype.Infinity, Valid: true}
	curID := maxUUID
	if q.Cursor != nil {
		curUpdated = pgtype.Timestamptz{Time: q.Cursor.UpdatedAt, Valid: true}
		curID = q.Cursor.ID
	}
	var labelArg any
	if len(q.Labels) > 0 {
		labelArg = q.Labels
	}

	rows, err := pool.Query(ctx, WorkflowStatusListSQL, scope, q.TypeName, status, curUpdated, curID, labelArg, limit)
	if err != nil {
		return nil, fmt.Errorf("workflow list (scope=%q status=%q): query: %w", scope, status, err)
	}
	defer rows.Close()
	var out []WorkflowBlockRow
	for rows.Next() {
		var r WorkflowBlockRow
		if err := rows.Scan(&r.ID, &r.Scope, &r.TypeName, &r.Title, &r.WorkflowStatus, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("workflow list (scope=%q status=%q): scan: %w", scope, status, err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// kwayMergeWorkflow merges the (scope × status) streams (each DESC by
// (updated_at, id DESC)) into the global top `limit` rows. Pure function —
// unit-tested without a DB.
func kwayMergeWorkflow(streams [][]WorkflowBlockRow, limit int) []WorkflowBlockRow {
	heads := make([]int, len(streams))
	out := make([]WorkflowBlockRow, 0, limit)
	for len(out) < limit {
		best := -1
		var bestRow WorkflowBlockRow
		for i := range streams {
			h := heads[i]
			if h >= len(streams[i]) {
				continue
			}
			cand := streams[i][h]
			if best == -1 || workflowRowBefore(cand, bestRow) {
				best = i
				bestRow = cand
			}
		}
		if best == -1 {
			break // all streams drained
		}
		out = append(out, bestRow)
		heads[best]++
	}
	return out
}

// workflowRowBefore reports whether a sorts before b in the board order:
// newer updated_at first, id DESC as the tie-break (matches the all-DESC index
// idx_blocks_workflow_board and the WorkflowStatusListSQL ORDER BY).
func workflowRowBefore(a, b WorkflowBlockRow) bool {
	if !a.UpdatedAt.Equal(b.UpdatedAt) {
		return a.UpdatedAt.After(b.UpdatedAt)
	}
	return a.ID > b.ID
}

// nextWorkflowCursor returns the resume cursor when the page is full (a full page
// means more rows may exist); a short page is the end of data ⇒ nil.
func nextWorkflowCursor(rows []WorkflowBlockRow, limit int) *WorkflowCursor {
	if len(rows) < limit {
		return nil
	}
	last := rows[len(rows)-1]
	return &WorkflowCursor{Status: last.WorkflowStatus, UpdatedAt: last.UpdatedAt, ID: last.ID}
}
