// forge_apply.go — I-G Pull-APPLY block writes (Achse 02, Welle I-G; design/02
// §3.1/§3.6/§4.5.2/§4.5.4). These primitives materialise a FETCHED forge issue
// as a context_block and keep it in sync. They are deliberately SEPARATE from the
// I-D InsertIssueBlock/UpdateIssueBlock local-write path:
//
//   - The local path mints a "#L<seq>" title (a per-scope draft sequence) and
//     REJECTS an over-cap body (the local caller controls its own size).
//   - The forge path mints the AUTHORITATIVE "#<nr> <title>" title (the GitHub
//     number is the identity, §3.5) and TRUNCATES an over-cap body + flags
//     metadata.truncated (issue bodies are attacker-controlled markdown, §5.5 —
//     GitHub allows 65 536 chars, the ctx cap is 50 KB; the pull must not fail
//     the whole run on one huge body).
//
// The canonical hash projection (§3.6) reads title (prefix-stripped), body,
// state, labels, assignees, milestone; the forge path stores state as
// metadata.forge_state (always — the §4.5.4 fail-safe) and labels/assignees/
// milestone at metadata top level (the board label post-filter reads
// metadata->'labels', §3.3). workflow_status is the registry-mapped board value
// (NULL when the registry is not resolvable — the metadata-only fallback §4.5.4).
package store

import (
	"context"
	"fmt"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// MaxForgeBodyBytes is the persistence cap for a pulled body (mirror of the
// /api/store 50 KB cap, design/02 §5.5). Over-cap is TRUNCATED on a rune
// boundary (PG text must stay valid UTF-8) and flagged, never rejected.
const MaxForgeBodyBytes = maxIssueBodyBytes

// CapForgeBody truncates s to MaxForgeBodyBytes on a UTF-8 rune boundary and
// reports whether it was truncated. The hash projection (§3.6) MUST run over the
// truncated body, so the same capped string feeds both the block write and the
// forge_hash — otherwise a >50 KB issue would drift forever (base != ctxH).
func CapForgeBody(s string) (string, bool) {
	if len(s) <= MaxForgeBodyBytes {
		return s, false
	}
	cut := s[:MaxForgeBodyBytes]
	// Back off to the last valid rune boundary (a naive byte cut can split a
	// multi-byte rune and yield invalid UTF-8, which PG text rejects).
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut, true
}

// ForgeIssueContent is the resolved shape of one fetched issue, ready to persist.
// Title is the PREFIX-FREE forge title; the primitive derives the "#<nr>" block
// title. Body is ALREADY capped by the caller (CapForgeBody) so the stored body
// and the base_hash agree. WorkflowStatus "" ⇒ NULL (the §4.5.4 fallback).
type ForgeIssueContent struct {
	Number         int64
	Title          string
	Body           string
	Truncated      bool
	ForgeState     string
	Labels         []string
	Assignees      []string
	Milestone      string
	WorkflowStatus string
}

// forgeIssueMeta builds the metadata patch for a pulled issue. labels/assignees
// are normalised to non-nil slices so the JSONB shape is stable across a create
// and an update (a nil slice would marshal to null, an empty one to []). The
// forge object carries only the number — owner/repo/kind live once on the
// project register (context_projects.forge), not duplicated across 10k blocks
// (documented deviation from §3.1's kind/owner/repo/number sketch; the projection
// §3.6 needs none of them, and cross-repo refs read the body string, not this).
func forgeIssueMeta(c ForgeIssueContent) map[string]any {
	labels := c.Labels
	if labels == nil {
		labels = []string{}
	}
	assignees := c.Assignees
	if assignees == nil {
		assignees = []string{}
	}
	return map[string]any{
		"forge":       map[string]any{"number": c.Number},
		"forge_state": c.ForgeState,
		"labels":      labels,
		"assignees":   assignees,
		"milestone":   c.Milestone,
		"truncated":   c.Truncated,
	}
}

// PullCreateIssueBlock inserts a forge-pulled issue as a new block (§4.5.2
// creation case). The title is the authoritative "#<nr> <title>"; type_source is
// 'manual' so the auto-classifier never re-types it; embedding stays NULL for the
// scheduler backfill. Tx-bound (the caller couples it with InsertSyncMap in the
// SAME Tx so a block can not exist without its mapping row).
func PullCreateIssueBlock(ctx context.Context, tx pgx.Tx, scope string, c ForgeIssueContent) (*Block, error) {
	title := fmt.Sprintf("#%d %s", c.Number, c.Title)
	var status any
	if c.WorkflowStatus != "" {
		status = c.WorkflowStatus
	}
	row := tx.QueryRow(ctx, `
		INSERT INTO context_blocks
		  (category, tags, title, content, metadata, scope, type_name, type_source, workflow_status)
		VALUES ($1, '{}', $2, $3, $4, $5, '`+IssueTypeName+`', 'manual', $6)
		RETURNING `+issueScanCols,
		IssueTypeName, title, c.Body, forgeIssueMeta(c), scope, status)
	b, err := scanIssue(row)
	if err != nil {
		return nil, fmt.Errorf("store: pull-create issue: %w", err)
	}
	return b, nil
}

// PullUpdateIssueBlock applies a forge-ahead update to an existing issue block
// (§4.5.2 pull row): title (the "#<nr>" prefix re-derived from the stable
// number), content, the metadata patch (JSONB-merged so forge/local_seq survive),
// workflow_status, and embedding cleared for re-embedding. Tx-bound (commits with
// the base_hash rewrite). A local_seq (from a former #L draft) is left untouched.
func PullUpdateIssueBlock(ctx context.Context, tx pgx.Tx, blockID string, c ForgeIssueContent) (*Block, error) {
	title := fmt.Sprintf("#%d %s", c.Number, c.Title)
	var status any
	if c.WorkflowStatus != "" {
		status = c.WorkflowStatus
	}
	row := tx.QueryRow(ctx, `
		UPDATE context_blocks
		   SET title = $2, content = $3, metadata = metadata || $4::jsonb,
		       workflow_status = $5, embedding = NULL, updated_at = now()
		 WHERE id = $1::uuid
		RETURNING `+issueScanCols,
		blockID, title, c.Body, forgeIssueMeta(c), status)
	b, err := scanIssue(row)
	if err != nil {
		return nil, fmt.Errorf("store: pull-update issue: %w", err)
	}
	return b, nil
}

// PullUpdateCommentBlock applies a forge-ahead body change to a comment block
// (comment projection is {body} only, §3.6). Tx-bound; embedding cleared.
func PullUpdateCommentBlock(ctx context.Context, tx pgx.Tx, blockID, body string) (*Block, error) {
	row := tx.QueryRow(ctx, `
		UPDATE context_blocks
		   SET content = $2, embedding = NULL, updated_at = now()
		 WHERE id = $1::uuid
		RETURNING `+issueScanCols, blockID, body)
	b, err := scanIssue(row)
	if err != nil {
		return nil, fmt.Errorf("store: pull-update comment: %w", err)
	}
	return b, nil
}
