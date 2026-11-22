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
// genuinely ctx-ahead rows, not the whole corpus. A listed row that projects to
// its base (a write that left the projection alone, e.g. tags) gets 0 wire
// writes and is advanced (AdvancePushBase), so it leaves the candidates.
package store

import (
	"context"
	"encoding/json"
	"errors"
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
// "#5" to the authoritative number. The title is part of the embed input, so
// every block whose title the rename changed has its vector invalidated in this
// transaction (InvalidateEmbeddingTx, issue #236): kept searchable, marked
// stale until the backfill embeds the new title. Locks: the issue row, the
// comment rows (the cascade), then their backfill memos — block before memo,
// the backfill's order.
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
			renamed, err := renamedBlockIDs(ctx, tx, `
			UPDATE context_blocks
			   SET title = regexp_replace(title, '^#L?\d+', $2), updated_at = now()
			 WHERE id = $1::uuid
			RETURNING id::text, old.title IS DISTINCT FROM new.title`, blockID, newPrefix)
			if err != nil {
				return fmt.Errorf("store: finalize push create rename: %w", err)
			}
			// Comment-title cascade — ONE UPDATE over all comments of this issue. Comment
			// base_hash is title-INDEPENDENT (projection {body}, §3.6), so only the titles
			// cascade; the "defensiv base rewrite" of §4.5.2 is a no-op for comments by
			// construction (the issue's own base was rewritten above).
			cascaded, err := renamedBlockIDs(ctx, tx, `
			UPDATE context_blocks
			   SET title = regexp_replace(title, '^#L?\d+', $2), updated_at = now()
			 WHERE parent_id = $1::uuid AND type_name = 'comment'
			RETURNING id::text, old.title IS DISTINCT FROM new.title`, blockID, newPrefix)
			if err != nil {
				return fmt.Errorf("store: finalize push create comment cascade: %w", err)
			}
			for _, id := range append(renamed, cascaded...) {
				if err := InvalidateEmbeddingTx(ctx, tx, id); err != nil {
					return fmt.Errorf("store: finalize push create: %w", err)
				}
			}
			// The forge state the create left (open, or closed after the
			// follow-up PATCH), issue #491.
			return StampForgeState(ctx, tx, blockID, ctxFields)
		})
}

// renamedBlockIDs runs a title UPDATE that returns (id, title changed) per row
// and answers the ids whose title it changed.
func renamedBlockIDs(ctx context.Context, tx pgx.Tx, sql string, args ...any) ([]string, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		var changed bool
		if err := rows.Scan(&id, &changed); err != nil {
			rows.Close()
			return nil, err
		}
		if changed {
			ids = append(ids, id)
		}
	}
	rows.Close()
	return ids, rows.Err()
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

// UnknownBaseHash is the base_hash of a mapping whose base an advance found
// moved off the base its listing read (AdvancePushBase, issue #486): neither
// side's version is known to be what the forge holds. Projection hashes are
// 64 lowercase hex digits (sha256), so no block or forge projection can equal
// it, and the 3-way decision sees both sides moved — converged when they
// agree, a conflict otherwise. It fits base_hash (VARCHAR(64) NOT NULL).
const UnknownBaseHash = "unknown"

// PushProjection hashes a block the way the push projected the version it
// listed (forge.CtxIssueBase / CtxCommentBase): AdvancePushBase compares it
// with the hash the push wrote. It reads Title, Content, Metadata and
// WorkflowStatus.
type PushProjection func(b *Block) string

// AdvancePushBase advances base := ctxH (+ snapshot) after a successful PATCH (the
// push made forge == the listed version). Also used for the 0-wire cases: only
// a never-pushed field (milestone) diverged, or the listed block projects to
// its base already (in sync).
//
// synced_at decides whether the next run lists the row again (candidate filter
// b.updated_at > m.synced_at), and the PATCH ran without a lock on the block,
// so a local write may have changed the block since the listing (issue #452). The
// advance therefore locks the block FOR NO KEY UPDATE — it waits for a writer
// still holding the row, and it may write the row's forge state below — and
// projects it again:
//
//   - The block still projects to ctxHash: synced_at := the block's updated_at,
//     the time of the version the forge now holds, not now(). A write whose
//     transaction began before this advance but takes the row after it stores
//     an updated_at that can lie below now() (updated_at is its transaction
//     start); against the version's own time it is newer and stays a candidate
//     — unless its transaction began even before the writer of that version
//     began, which no timestamp can tell apart (it needs a version that
//     advances in write order).
//   - It does not (a write committed while the PATCH was on the wire, or it
//     is gone): synced_at := -infinity, so the row stays a candidate whatever
//     updated_at the write carried, and the next run diffs the block against
//     the base this advance wrote — the version the forge holds.
//
// The base is written only while the mapping still holds listedBase, the base
// the listing read (issue #486), or ctxHash itself: another instance's pull
// that ran after the PATCH landed found block and forge at the sent version
// and converged on it, which is what this advance records anyway (issue
// #491). Another ctxd instance on the same database can also move it to a
// third base between the section's check (TryLockPushEntity) and this
// advance: its pull converges on what the PATCH sent and then pulls a remote
// edit. (Its push cannot: the section keeps other pushes of the entity out,
// issue #491.) What the forge holds is then unknown — keeping the moved base
// is wrong when this advance's PATCH landed after the pulled edit, writing
// the listed base is wrong when the pulled edit is newer. So the
// advance records UnknownBaseHash, drops base_fields and flags the mapping
// (conflict, conflict_at): the push lists no flagged mapping, and the next
// pull's 3-way decision converges when block and forge agree (base := their
// hash, flag cleared) and keeps the conflict otherwise. The returned moved
// reports that case. synced_at is then of no account: the push lists no
// flagged mapping, and the convergence that clears the flag sets it.
//
// synced_at is compared in SQL only (the candidate filter): '-infinity' is a
// valid timestamptz, but pgx cannot scan it into a time.Time, so a reader that
// selects the column has to cast or filter it in SQL
// (TestSyncedAtIsComparedInSQLOnly holds every mention to an assignment or
// the filter).
//
// A base it writes is the version the PATCH made the forge hold, so it also
// writes that version's state into the block's metadata.forge_state
// (StampForgeState, issue #491); an unknown base writes none.
//
// Locks: the block row (FOR NO KEY UPDATE, the lock StampForgeState's UPDATE
// takes — no lock upgrade), then the mapping row — the order of the pull's
// writes. The mapping row's base is compared in the UPDATE itself, on
// the row version the UPDATE locks. A mapping that is gone is no error.
// Tx-bound: the push runs it in a short transaction after the wire write, on
// the connection that holds the entity's push lock (TryLockPushEntity, issue
// #491).
func AdvancePushBase(ctx context.Context, tx pgx.Tx, blockID, listedBase, ctxHash string, ctxFields json.RawMessage, project PushProjection) (moved bool, err error) {
	var b Block
	var at time.Time
	holds := false
	err = tx.QueryRow(ctx, `
		SELECT title, content, metadata, COALESCE(workflow_status, ''), updated_at
		  FROM context_blocks WHERE id = $1::uuid
		   FOR NO KEY UPDATE`, blockID).Scan(&b.Title, &b.Content, &b.Metadata, &b.WorkflowStatus, &at)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return false, fmt.Errorf("store: advance push base: lock block: %w", err)
	default:
		holds = project(&b) == ctxHash
	}
	// In SET every column reference reads the row as it was before this
	// UPDATE, so every expression tests the stored base: the listed one ($6)
	// or the version the PATCH sent ($2), on which another instance's pull
	// may have converged in the meantime.
	err = tx.QueryRow(ctx, `
		UPDATE context_project_sync_map
		   SET base_hash = CASE WHEN base_hash IN ($6, $2) THEN $2 ELSE $7 END,
		       synced_at = CASE WHEN $4 THEN $5::timestamptz ELSE '-infinity'::timestamptz END,
		       metadata = CASE WHEN base_hash IN ($6, $2) THEN metadata || $3::jsonb ELSE metadata - 'base_fields' END,
		       conflict = conflict OR base_hash NOT IN ($6, $2),
		       conflict_at = CASE WHEN base_hash IN ($6, $2) THEN conflict_at ELSE COALESCE(conflict_at, now()) END
		 WHERE block_id = $1::uuid
		RETURNING old.base_hash NOT IN ($6, $2)`,
		blockID, ctxHash, baseFieldsMeta(ctxFields), holds, at, listedBase, UnknownBaseHash).Scan(&moved)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("store: advance push base: %w", err)
	}
	if moved {
		return true, nil
	}
	return false, StampForgeState(ctx, tx, blockID, ctxFields)
}

// PushSkip names why a push section leaves an entity to the next run; ""
// means the section may write (TryLockPushEntity).
type PushSkip string

const (
	// PushSkipInFlight: another session holds the entity's push lock — an
	// instance whose wire write of the entity may be on its way now.
	PushSkipInFlight PushSkip = "in flight"
	// PushSkipOvertaken: the mapping's base moved since the push listed the
	// entity — another instance wrote it since, or a pull moved it.
	PushSkipOvertaken PushSkip = "overtaken"
	// PushSkipFlagged: the mapping was flagged since the listing (a pull saw
	// both sides move).
	PushSkipFlagged PushSkip = "flagged"
	// PushSkipGone: the mapping row is gone.
	PushSkipGone PushSkip = "gone"
)

// pushLockSQL and pushUnlockSQL take and give back the advisory lock of one
// entity's push section: the two-key form, class key first, keyed on the
// block id.
const (
	pushLockSQL   = `SELECT pg_try_advisory_lock(hashtext('ctx:forge-push'), hashtext($1::text))`
	pushUnlockSQL = `SELECT pg_advisory_unlock(hashtext('ctx:forge-push'), hashtext($1::text))`
)

// TryLockPushEntity opens the push's section of one entity on conn, a
// connection the caller holds out of the pool with no transaction open on
// it: it takes the entity's push lock — a session-level advisory lock keyed
// on the block id — without waiting, and answers why the section may not
// write (skip, "" when it may): the lock was held by another session, or the
// mapping row is no longer the candidate the push's listing read — at
// listedBase, the base the listing saw, and not conflict-flagged (issue
// #491). locked reports that this call holds the lock now; the caller gives
// it back with UnlockPushEntity on the same connection, whatever skip says.
//
// The push writes an entity to the forge and records that write (the
// advance) under the lock, so two ctxd instances never have the writes of
// one entity in flight at once. Without it, B's PATCH and advance of a newer
// local version could run while A's PATCH of an older one was on the wire;
// A's landing last left the forge at the older version, and a pull in
// between found the block at B's base and the forge ahead of it, and wrote
// the older version over the newer one on both sides.
//
// A session-level lock, not a transaction-level one: the wire write runs
// between the lock and the advance, and a transaction kept open across it
// held a snapshot (backend_xmin, the vacuum horizon of the whole database)
// and an AccessShareLock on context_project_sync_map for as long as the
// forge took to answer — an ALTER TABLE on the table queued behind it, and
// every later access of the table behind the ALTER. The base check below
// runs in a statement of its own after the lock is held, so it sees the row
// as the previous holder left it.
//
// A skip leaves the entity to the caller's next run, which lists it again
// against the row it finds. Not waiting is the bound: no push leg waits on
// another one's wire call, however long that one hangs. err with locked =
// false means the lock statement's outcome is unknown; the caller closes the
// connection, so a lock it may hold ends with the session.
func TryLockPushEntity(ctx context.Context, conn pgxdb.Rower, blockID, listedBase string) (locked bool, skip PushSkip, err error) {
	if err := conn.QueryRow(ctx, pushLockSQL, blockID).Scan(&locked); err != nil {
		return false, "", fmt.Errorf("store: push lock: %w", err)
	}
	if !locked {
		return false, PushSkipInFlight, nil
	}
	var base string
	var conflict bool
	err = conn.QueryRow(ctx,
		`SELECT base_hash, conflict FROM context_project_sync_map WHERE block_id = $1::uuid`, blockID).Scan(&base, &conflict)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return true, PushSkipGone, nil
	case err != nil:
		return true, "", fmt.Errorf("store: push lock: read base: %w", err)
	case base != listedBase:
		return true, PushSkipOvertaken, nil
	case conflict:
		return true, PushSkipFlagged, nil
	}
	return true, "", nil
}

// UnlockPushEntity gives back the push lock TryLockPushEntity took on conn
// and answers whether the session held it.
func UnlockPushEntity(ctx context.Context, conn pgxdb.Rower, blockID string) (bool, error) {
	var released bool
	if err := conn.QueryRow(ctx, pushUnlockSQL, blockID).Scan(&released); err != nil {
		return false, fmt.Errorf("store: push unlock: %w", err)
	}
	return released, nil
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
