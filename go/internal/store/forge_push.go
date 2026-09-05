// forge_push.go — I-H push-side writes (Achse 02, Welle I-H; design/02 §4.5.2,
// lines 416/565). The push materialises ctx-ahead entities back onto the forge:
// it ENUMERATES push candidates (mappings whose block changed since the last base
// write, or local-only forge_id=0 drafts), FINALISES a create (forge_id 0→number,
// "#L<seq>"→"#<nr>" title rename + comment-title cascade + base rewrite in ONE
// Tx), and ADVANCES the base after a successful PATCH (base := ctxH). The wire
// calls themselves live in package forge; these are the DB primitives.
//
// Candidate filter (§6, 10k+ issues/repo): a mapping is a candidate only when
// forge_id=0 (a local draft, always) OR the block's updated_at is NEWER than the
// mapping's synced_at — an in-sync block (last touched by the pull/base write, same
// transaction now()) can never satisfy that, so the enumeration is bounded to
// genuinely ctx-ahead rows, not the whole corpus. A base==ctxH row that slips
// through (clock-equal) is skipped Go-side (0 wire writes).
package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/GottZ/ctx/internal/pgxdb"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// IssuePushCandidate is one ctx-ahead issue mapping + the block fields the push
// field-diff needs. BaseFields is the last-synced canonical projection (nil on a
// legacy row); ForgeID 0 = a local draft (push-create).
type IssuePushCandidate struct {
	ForgeID    int64
	BaseHash   string
	BaseFields json.RawMessage
	Block      Block // ID, Title, Content, Metadata, Scope, WorkflowStatus
}

// CommentPushCandidate is one ctx-ahead comment mapping. ParentForgeID is the
// parent issue's forge number (0 = the parent is itself an unpushed draft — the
// comment waits for a later run, after the issue create writes the number).
type CommentPushCandidate struct {
	ForgeID       int64
	BaseHash      string
	BaseFields    json.RawMessage
	BlockID       string
	Content       string
	Metadata      map[string]any
	ParentForgeID int64
}

// ListIssuePushCandidates returns up to `limit` ctx-ahead issue mappings for a
// project, forge_id ASC (drafts first). conflict rows are excluded (a conflict is
// the user's domain — 0 wire writes, §4.5.2).
func ListIssuePushCandidates(ctx context.Context, pool *pgxpool.Pool, projectID string, limit int) ([]IssuePushCandidate, error) {
	rows, err := pool.Query(ctx, `
		SELECT m.forge_id, m.base_hash, m.metadata->'base_fields',
		       b.id::text, b.title, b.content, b.metadata, b.scope, COALESCE(b.workflow_status,'')
		  FROM context_project_sync_map m
		  JOIN context_blocks b ON b.id = m.block_id
		 WHERE m.project_id = $1::uuid AND m.entity_kind = 'issue' AND NOT m.conflict
		   AND NOT b.is_archived
		   AND (m.forge_id = 0 OR b.updated_at > m.synced_at)
		 ORDER BY m.forge_id ASC
		 LIMIT $2`, projectID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list issue push candidates: %w", err)
	}
	defer rows.Close()
	var out []IssuePushCandidate
	for rows.Next() {
		var c IssuePushCandidate
		if err := rows.Scan(&c.ForgeID, &c.BaseHash, &c.BaseFields,
			&c.Block.ID, &c.Block.Title, &c.Block.Content, &c.Block.Metadata, &c.Block.Scope, &c.Block.WorkflowStatus); err != nil {
			return nil, fmt.Errorf("store: scan issue push candidate: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListCommentPushCandidates returns up to `limit` ctx-ahead comment mappings for a
// project, forge_id ASC. It resolves each comment's parent issue forge number via
// the parent block's issue mapping (0 = parent not pushed yet). Run AFTER the
// issue leg so a freshly written parent number is visible (§4.5.2 issue-first).
func ListCommentPushCandidates(ctx context.Context, pool *pgxpool.Pool, projectID string, limit int) ([]CommentPushCandidate, error) {
	rows, err := pool.Query(ctx, `
		SELECT m.forge_id, m.base_hash, m.metadata->'base_fields',
		       b.id::text, b.content, b.metadata, COALESCE(pm.forge_id, 0)
		  FROM context_project_sync_map m
		  JOIN context_blocks b ON b.id = m.block_id
		  LEFT JOIN context_project_sync_map pm
		         ON pm.block_id = b.parent_id AND pm.project_id = m.project_id AND pm.entity_kind = 'issue'
		 WHERE m.project_id = $1::uuid AND m.entity_kind = 'comment' AND NOT m.conflict
		   AND NOT b.is_archived
		   AND (m.forge_id = 0 OR b.updated_at > m.synced_at)
		 ORDER BY m.forge_id ASC
		 LIMIT $2`, projectID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list comment push candidates: %w", err)
	}
	defer rows.Close()
	var out []CommentPushCandidate
	for rows.Next() {
		var c CommentPushCandidate
		if err := rows.Scan(&c.ForgeID, &c.BaseHash, &c.BaseFields,
			&c.BlockID, &c.Content, &c.Metadata, &c.ParentForgeID); err != nil {
			return nil, fmt.Errorf("store: scan comment push candidate: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// FinalizePushCreateIssue closes a push-create in ONE transaction (§4.5.2 line
// 416): forge_id 0→number on the mapping, base := ctxH + the base-field snapshot,
// the "#L<seq>"→"#<nr>" block-title rename, AND the comment-title cascade
// ("#L<seq>.cL<n>"→"#<nr>.cL<n>", ONE UPDATE over all the issue's comments —
// identity-neutral, the mapping is the identity). Idempotent: a re-run never
// reaches here (after this the mapping is forge_id>0 and updated_at==synced_at, so
// the candidate filter drops it — no double-rename, no base drift). The prefix
// regex `^#L?\d+` rewrites both the local draft "#L5" and (defensively) a stale
// "#5" to the authoritative number.
//
// The GitHub POST happened BEFORE this Tx (a wire create can not roll back); if
// this commit fails the forge issue exists without a local number and a re-run
// would create a duplicate — the documented dual-write window (v1; §4.5.3).
func FinalizePushCreateIssue(ctx context.Context, pool *pgxpool.Pool, blockID string, number int64, ctxHash string, ctxFields json.RawMessage) error {
	return pgxdb.Write(ctx, pool,
		pgxdb.Stages{Begin: "store: finalize push create begin", Commit: "store: finalize push create commit"},
		func(tx pgx.Tx) error {
			// Mapping: forge_id 0→number, base := ctxH (+ snapshot). Keyed on block_id
			// (the per-block unique index) so the PK forge_id change is a plain UPDATE.
			if _, err := tx.Exec(ctx, `
			UPDATE context_project_sync_map
			   SET forge_id = $2, base_hash = $3, synced_at = now(),
			       conflict = false, conflict_at = NULL,
			       metadata = metadata || $4::jsonb
			 WHERE block_id = $1::uuid AND entity_kind = 'issue'`,
				blockID, number, ctxHash, baseFieldsMeta(ctxFields)); err != nil {
				return fmt.Errorf("store: finalize push create mapping: %w", err)
			}
			// Block title rename "#L<seq>"→"#<nr>" ($2 is the fully built "#<nr>" prefix,
			// passed as text to avoid an int→text concat encode).
			newPrefix := fmt.Sprintf("#%d", number)
			if _, err := tx.Exec(ctx, `
			UPDATE context_blocks
			   SET title = regexp_replace(title, '^#L?\d+', $2), updated_at = now()
			 WHERE id = $1::uuid`, blockID, newPrefix); err != nil {
				return fmt.Errorf("store: finalize push create rename: %w", err)
			}
			// Comment-title cascade — ONE UPDATE over all comments of this issue. Comment
			// base_hash is title-INDEPENDENT (projection {body}, §3.6), so only the titles
			// cascade; the "defensiv base rewrite" of §4.5.2 is a no-op for comments by
			// construction (the issue's own base was rewritten above).
			if _, err := tx.Exec(ctx, `
			UPDATE context_blocks
			   SET title = regexp_replace(title, '^#L?\d+', $2), updated_at = now()
			 WHERE parent_id = $1::uuid AND type_name = 'comment'`, blockID, newPrefix); err != nil {
				return fmt.Errorf("store: finalize push create comment cascade: %w", err)
			}
			return nil
		})
}

// FinalizePushCreateComment closes a comment push-create: forge_id 0→id, base :=
// ctxH + snapshot. No title rename (the comment title was already cascaded to
// "#<nr>.cL<n>" when its parent issue was pushed). Keyed on block_id.
func FinalizePushCreateComment(ctx context.Context, pool *pgxpool.Pool, blockID string, commentID int64, ctxHash string, ctxFields json.RawMessage) error {
	_, err := pool.Exec(ctx, `
		UPDATE context_project_sync_map
		   SET forge_id = $2, base_hash = $3, synced_at = now(),
		       conflict = false, conflict_at = NULL,
		       metadata = metadata || $4::jsonb
		 WHERE block_id = $1::uuid AND entity_kind = 'comment'`,
		blockID, commentID, ctxHash, baseFieldsMeta(ctxFields))
	if err != nil {
		return fmt.Errorf("store: finalize push create comment: %w", err)
	}
	return nil
}

// AdvancePushBase advances base := ctxH (+ snapshot) after a successful PATCH (the
// push made forge == ctx). synced_at = now() > the block's local-edit updated_at,
// so the next run's candidate filter drops the row (idempotent). Also used for the
// 0-wire case where only a never-pushed field (milestone) diverged.
func AdvancePushBase(ctx context.Context, pool *pgxpool.Pool, blockID, ctxHash string, ctxFields json.RawMessage) error {
	_, err := pool.Exec(ctx, `
		UPDATE context_project_sync_map
		   SET base_hash = $2, synced_at = now(),
		       metadata = metadata || $3::jsonb
		 WHERE block_id = $1::uuid`, blockID, ctxHash, baseFieldsMeta(ctxFields))
	if err != nil {
		return fmt.Errorf("store: advance push base: %w", err)
	}
	return nil
}

// FlagPushConflict flags a mapping conflict from the push side (a truncated body
// that diverged locally — pushing it would overwrite up to ~15 KB of forge
// content, §4.5.2 data-loss guard). Reuses the 3-way conflict primitive so the
// surface (forge-sync-status/CLI/UI) is identical to a pull-side conflict.
func FlagPushConflict(ctx context.Context, pool *pgxpool.Pool, blockID string, at time.Time) error {
	return pgxdb.Write(ctx, pool, pgxdb.Stages{}, func(tx pgx.Tx) error {
		return FlagSyncMapConflict(ctx, tx, blockID, at)
	})
}
