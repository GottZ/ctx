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
// (NULL when the registry is not resolvable — the metadata-only fallback §4.5.4);
// an update leaves the block's own status where the forge state did not change
// (ForgeIssueContent.KeepWorkflowStatus, issue #486).
package store

import (
	"context"
	"encoding/json"
	"errors"
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
//
// KeepWorkflowStatus makes PullUpdateIssueBlock leave workflow_status as the
// row holds it instead of writing WorkflowStatus (issue #486): the caller sets
// it when the forge state did not change, so a local move between two statuses
// of one forge state survives a remote edit. PullCreateIssueBlock ignores it.
type ForgeIssueContent struct {
	Number             int64
	Title              string
	Body               string
	Truncated          bool
	ForgeState         string
	Labels             []string
	Assignees          []string
	Milestone          string
	WorkflowStatus     string
	KeepWorkflowStatus bool
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

// LockPulledBlock reads an issue or comment block of scopes inside the pull's
// transaction and locks it FOR NO KEY UPDATE — the lock the pull's own UPDATE
// takes, which waits for a local writer still holding the row (UpdateIssueBlock
// takes FOR UPDATE, UpdateBlock FOR NO KEY UPDATE) and keeps the next one out
// until the pull commits. The pull decided its direction on a read without a
// lock; it projects this row again before it writes (issue #452). A block that
// is archived, gone or out of scopes reads as ErrIssueNotFound.
//
// targets are the blocks the pull links the block to (its "#<nr>"
// references), which PutStructuralLink's validation locks FOR SHARE: the
// targets whose id lies below the block's are locked here first, FOR SHARE
// and in ascending id order, before the block (issue #503); those above it
// the reference write takes after the block. Two pulls of issues that
// reference each other locked their own block first and the other's FOR
// SHARE in the reference write, each waiting for the other's FOR NO KEY
// UPDATE: a deadlock (40P01). Now a pull holds no block when it waits for
// one below its own, and it waits for one above its own only while that
// block's pull is not waiting for its block — the later of the two waits
// before it holds a row the earlier one needs. FOR SHARE stays: a scope move
// takes the row FOR NO KEY UPDATE (a scope is no key column), which FOR KEY
// SHARE would let through, and the target's scope must not change under the
// link.
func LockPulledBlock(ctx context.Context, tx pgx.Tx, blockID string, scopes, targets []string) (*Block, error) {
	if err := lockLinkTargetsBelow(ctx, tx, targets, blockID); err != nil {
		return nil, err
	}
	b, err := scanIssue(tx.QueryRow(ctx, `
		SELECT `+issueScanCols+`
		  FROM context_blocks
		 WHERE id = $1::uuid AND NOT is_archived AND scope = ANY($2::text[])
		   AND type_name = ANY(ARRAY['`+IssueTypeName+`','`+CommentTypeName+`'])
		   FOR NO KEY UPDATE`, blockID, scopes))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrIssueNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: lock pulled block: %w", err)
	}
	return b, nil
}

// PullUpdateIssueBlock applies a forge-ahead update to an existing issue block
// (§4.5.2 pull row): title (the "#<nr>" prefix re-derived from the stable
// number), content, the metadata patch (JSONB-merged so forge/local_seq survive),
// workflow_status (unless c.KeepWorkflowStatus: then the status the row holds
// stays — the caller holds the row locked, LockPulledBlock, so that is the
// status it decided on), and — when the title or the body now differs — the vector
// invalidated for re-embedding (InvalidateEmbeddingTx: it stays searchable,
// marked stale, until the backfill writes the new one; a state- or label-only
// change keeps it current, issue #236). Tx-bound (commits with the base_hash
// rewrite). A local_seq (from a former #L draft) is left untouched.
//
// The guard's check mark, which the invalidation drops for a changed title or
// body (issue #452), is stripped in the metadata expression this statement
// writes anyway, the way UpdateBlock strips it for a content write
// (updateMetadataExpr): the invalidation then finds no mark and passes the
// document through, so the row's metadata is written once, not twice (one
// TOAST value instead of two for a large document). title and content in the
// SET expression are the stored row's values.
func PullUpdateIssueBlock(ctx context.Context, tx pgx.Tx, blockID string, c ForgeIssueContent) (*Block, error) {
	title := fmt.Sprintf("#%d %s", c.Number, c.Title)
	var status any
	if c.WorkflowStatus != "" {
		status = c.WorkflowStatus
	}
	row := tx.QueryRow(ctx, `
		UPDATE context_blocks
		   SET title = $2, content = $3,
		       metadata = CASE WHEN title IS DISTINCT FROM $2 OR content IS DISTINCT FROM $3
		                       THEN (metadata || $4::jsonb) - 'guard_checked_at'
		                       ELSE metadata || $4::jsonb END,
		       workflow_status = CASE WHEN $6 THEN workflow_status ELSE $5 END, updated_at = now()
		 WHERE id = $1::uuid
		RETURNING `+issueScanCols+`, `+textChangedCol,
		blockID, title, c.Body, forgeIssueMeta(c), status, c.KeepWorkflowStatus)
	var textChanged bool
	b, err := scanIssue(row, &textChanged)
	if err != nil {
		return nil, fmt.Errorf("store: pull-update issue: %w", err)
	}
	if textChanged {
		if err := InvalidateEmbeddingTx(ctx, tx, blockID); err != nil {
			return nil, fmt.Errorf("store: pull-update issue: %w", err)
		}
	}
	return b, nil
}

// StampForgeState writes the state of a base snapshot (the "state" field of
// the canonical projection the caller records as the mapping's base) into the
// block's metadata.forge_state, the state an issue projects when no workflow
// resolves (§4.5.4). The pull writes it with every block write; the push
// (create and update) and the convergence record a base without writing the
// block, and left it at what the last pull saw — "open" after a pushed or a
// converged close, which with the workflow lost made any later local edit
// project the issue as open against the closed base and the push reopen it
// (issue #491). They call this with the snapshot they record: what the PATCH
// made the forge hold, what the pull read from it.
//
// A snapshot without a state (a comment's {body}) writes nothing, and so does
// a value the block already holds: the row is rewritten — and its write
// notifications fire — only when the forge state changed. updated_at stays:
// the stamp is the forge's state, not a local edit, and moving updated_at
// would list the mapping as a push candidate. The caller holds the block row
// locked (FOR NO KEY UPDATE), Tx-bound.
func StampForgeState(ctx context.Context, tx pgx.Tx, blockID string, baseFields json.RawMessage) error {
	var snap struct {
		State *string `json:"state"`
	}
	if len(baseFields) == 0 {
		return nil
	}
	if err := json.Unmarshal(baseFields, &snap); err != nil {
		return fmt.Errorf("store: stamp forge state: decode base snapshot: %w", err)
	}
	if snap.State == nil || *snap.State == "" {
		return nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE context_blocks
		   SET metadata = jsonb_set(metadata, '{forge_state}', to_jsonb($2::text))
		 WHERE id = $1::uuid AND metadata->>'forge_state' IS DISTINCT FROM $2`, blockID, *snap.State); err != nil {
		return fmt.Errorf("store: stamp forge state: %w", err)
	}
	return nil
}

// PullUpdateCommentBlock applies a forge-ahead body change to a comment block
// (comment projection is {body} only, §3.6). Tx-bound; the vector is
// invalidated (InvalidateEmbeddingTx) when the body differs (issue #236).
func PullUpdateCommentBlock(ctx context.Context, tx pgx.Tx, blockID, body string) (*Block, error) {
	row := tx.QueryRow(ctx, `
		UPDATE context_blocks
		   SET content = $2, updated_at = now()
		 WHERE id = $1::uuid
		RETURNING `+issueScanCols+`, `+textChangedCol, blockID, body)
	var textChanged bool
	b, err := scanIssue(row, &textChanged)
	if err != nil {
		return nil, fmt.Errorf("store: pull-update comment: %w", err)
	}
	if textChanged {
		if err := InvalidateEmbeddingTx(ctx, tx, blockID); err != nil {
			return nil, fmt.Errorf("store: pull-update comment: %w", err)
		}
	}
	return b, nil
}
