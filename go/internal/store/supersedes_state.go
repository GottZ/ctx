// Supersedes lifecycle side-effect (ApplySupersedes and its reverse), shared
// by every path that writes or deletes supersedes dream links: dream.WriteLinks
// (incl. its stale-link sweep), dream.CleanupDanglingLinks and the operator's
// DreamLinkResolve delete. It lives in store because dream imports store, not
// the other way round — one reconcile, no copy of its rules per caller.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/GottZ/ctx/internal/pgxdb"
)

const supersedesSnapshotConfidence = 0.7

// ReconcileSupersedesTargets is the one entry point for the supersedes
// lifecycle side-effect inside a transaction. Callers run it AFTER every link
// write and link delete of the transaction, so each reconcile sees the final
// link state of the transaction. It returns the targets it restored to
// knowledge (snapshot → knowledge/NULL).
//
// Lock discipline (PR #44 hardening):
//   - All targets are locked in ONE statement, in ascending id order, BEFORE
//     the first UPDATE. Two transactions that touch overlapping target sets
//     therefore acquire them in the same order and cannot deadlock on each
//     other (the model emits links in arbitrary order; locking per link in
//     that order produced 40P01 between concurrent dream workers). Taking the
//     locks before any UPDATE also matters because a transaction that has
//     written must not wait for a target lock afterwards: until migration 181
//     every UPDATE on context_blocks fired mark_guard_dirty, which held the
//     single context_guard_state row to commit, and any lock a block write
//     takes and keeps closes the same cycle (row locks first, block writes
//     after).
//   - FOR NO KEY UPDATE, not FOR UPDATE: the reconcile never changes a key
//     column, and FOR UPDATE would also block the FOR KEY SHARE that every
//     concurrent INSERT into context_dream_links takes on its target for the
//     foreign-key check (55P03 under a lock_timeout).
//   - Link rows are locked (written or deleted) before this pass, block rows
//     in it — callers keep that order.
//
// preferredSource is the writing source of a WriteLinks batch ("" for
// callers without one): a target that is not yet a snapshot and has that
// source among its valid superseders takes it as superseded_by — the
// ApplySupersedes semantics (the writer of the supersedes link becomes the
// pointer) that used to run as a separate per-link UPDATE.
func ReconcileSupersedesTargets(ctx context.Context, tx pgx.Tx, targetIDs []string, preferredSource string) (restored []string, err error) {
	if len(targetIDs) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx,
		`SELECT id::text
		 FROM context_blocks
		 WHERE id = ANY($1::uuid[])
		 ORDER BY id
		 FOR NO KEY UPDATE`, targetIDs)
	if err != nil {
		return nil, fmt.Errorf("store: lock supersedes targets: %w", err)
	}
	locked, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("store: lock supersedes targets: %w", err)
	}
	for _, targetID := range locked {
		wasRestored, err := reconcileSupersedesState(ctx, tx, targetID, preferredSource)
		if err != nil {
			return nil, err
		}
		if wasRestored {
			restored = append(restored, targetID)
		}
	}
	return restored, nil
}

// supersedesCandidates returns the sources of supersedes links into targetID
// that can carry the snapshot side-effect — source not archived, source in the
// target's scope. remaining holds all of them, valid the subset at or above
// the weighted gate; both in (created_at, source id) order.
func supersedesCandidates(ctx context.Context, tx pgx.Tx, targetID, scope string) (remaining, valid []string, err error) {
	rows, err := tx.Query(ctx,
		`SELECT dl.source_block_id::text,
		        -- confidence is REAL on disk; cast the threshold to REAL so the
		        -- persisted gate uses the same representation as ApplySupersedes.
		        dl.confidence >= $2::real
		 FROM context_dream_links dl
		 JOIN context_blocks src ON src.id = dl.source_block_id
		 WHERE dl.target_block_id = $1::uuid
		   AND dl.relationship = 'supersedes'
		   AND NOT src.is_archived
		   AND src.scope = $3
		 ORDER BY dl.created_at, dl.source_block_id`,
		targetID, supersedesSnapshotConfidence, scope,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("store: reconcile supersedes candidates: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var sourceID string
		var isValid bool
		if err := rows.Scan(&sourceID, &isValid); err != nil {
			return nil, nil, fmt.Errorf("store: reconcile supersedes candidate: %w", err)
		}
		remaining = append(remaining, sourceID)
		if isValid {
			valid = append(valid, sourceID)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("store: reconcile supersedes candidates: %w", err)
	}
	return remaining, valid, nil
}

// reconcileSupersedesState keeps the target lifecycle side-effect aligned with
// the current supersedes rows for that target. The confidence column stores
// the weighted confidence used by ApplySupersedes; raw_confidence remains the
// separate query-side map gate. Callers go through ReconcileSupersedesTargets,
// which holds the row lock already; the lock clause here only re-asserts it.
// restored reports a snapshot → knowledge transition.
//
// The state machine has an exit hysteresis (PR #44 review F3). The weighted
// confidence is raw × source quality × target quality, and qualities drift
// between dream cycles, so the same pair is re-written on both sides of the
// 0.7 gate; a plain "valid superseder ⇔ snapshot" rule makes the target
// flip-flop with every drift. Hence:
//   - enter: a target that is not a dream snapshot (lifecycle 'snapshot' with
//     superseded_by set) becomes one only when a VALID (≥ 0.7) superseder
//     exists;
//   - stay: a dream snapshot keeps state and pointer while its pointer's
//     source still has ANY supersedes link into it (confidence irrelevant);
//   - move: when the pointer's source no longer qualifies but others remain,
//     the pointer moves to the first valid one, else to the first remaining
//     one — the target stays a snapshot;
//   - leave: only when no supersedes link from a non-archived source of the
//     target's scope remains does the target return to knowledge/NULL.
//
// A chunk target has no side-effect at all (issue #191). A chunk row is a
// derived piece of its source and belongs to the ingest: InsertChunk finds
// its (source_id, chunk_index) slot only through the arbiter `NOT is_archived
// AND lifecycle_state = 'chunk'`, so a snapshot marking (and the 'knowledge'
// the leave arm hands back) took the slot away from the ingest for good — a
// changed-content re-ingest then failed on uq_context_category_title_scope.
// The supersedes statement itself is the link row, which stays: the query
// path reads the links (dream.SupersedesMap → filterSuperseded and the
// superseded_by of the response), not the lifecycle or the pointer column.
func reconcileSupersedesState(ctx context.Context, tx pgx.Tx, targetID, preferredSource string) (restored bool, err error) {
	var lifecycle, scope string
	var supersededBy sql.NullString
	var archived bool
	if err := tx.QueryRow(ctx,
		`SELECT lifecycle_state, superseded_by::text, is_archived, scope
		 FROM context_blocks
		 WHERE id = $1::uuid
		 FOR NO KEY UPDATE`, targetID,
	).Scan(&lifecycle, &supersededBy, &archived, &scope); err != nil {
		return false, fmt.Errorf("store: reconcile supersedes target: %w", err)
	}
	if archived || lifecycle == "chunk" {
		return false, nil
	}

	remaining, valid, err := supersedesCandidates(ctx, tx, targetID, scope)
	if err != nil {
		return false, err
	}

	if lifecycle == "snapshot" && supersededBy.Valid {
		pointer := supersededBy.String
		switch {
		case slices.Contains(remaining, pointer):
			return false, nil // stay
		case len(remaining) == 0: // leave
			if _, err := tx.Exec(ctx,
				`UPDATE context_blocks
				 SET lifecycle_state = 'knowledge', superseded_by = NULL
				 WHERE id = $1::uuid
				   AND lifecycle_state = 'snapshot'
				   AND superseded_by = $2::uuid`, targetID, pointer,
			); err != nil {
				return false, fmt.Errorf("store: reconcile supersedes restore: %w", err)
			}
			slog.Info("store: restored superseded block to knowledge (no superseder left)",
				"target_block_id", targetID, "previous_superseded_by", pointer)
			return true, nil
		}
		// move: a valid superseder first, else a remaining sub-threshold one.
		next := remaining[0]
		if len(valid) > 0 {
			next = valid[0]
		}
		if err := markSupersededSnapshot(ctx, tx, targetID, next); err != nil {
			return false, err
		}
		slog.Info("store: moved snapshot pointer to remaining superseder",
			"target_block_id", targetID, "previous_superseded_by", pointer, "superseded_by", next)
		return false, nil
	}

	if len(valid) == 0 {
		return false, nil
	}
	chosen := valid[0]
	switch {
	case lifecycle != "snapshot" && slices.Contains(valid, preferredSource):
		chosen = preferredSource
	case supersededBy.Valid && slices.Contains(valid, supersededBy.String):
		chosen = supersededBy.String
	}
	if err := markSupersededSnapshot(ctx, tx, targetID, chosen); err != nil {
		return false, err
	}
	if lifecycle != "snapshot" {
		slog.Info("store: marked target block as snapshot",
			"target_block_id", targetID,
			"superseded_by_source", chosen,
		)
	}
	return false, nil
}

// markSupersededSnapshot sets the snapshot state with the given pointer.
func markSupersededSnapshot(ctx context.Context, tx pgx.Tx, targetID, sourceID string) error {
	if _, err := tx.Exec(ctx,
		`UPDATE context_blocks
		 SET lifecycle_state = 'snapshot', superseded_by = $1::uuid
		 WHERE id = $2::uuid
		   AND NOT is_archived`, sourceID, targetID,
	); err != nil {
		return fmt.Errorf("store: reconcile supersedes snapshot: %w", err)
	}
	return nil
}

// errSupersedesSourceChanged: the block was in the scope whose supersedes locks
// lockSupersedesSource took, and the lock pass did not find it there any more —
// a scope move (or a purge) committed in between. Locking it in its new scope
// without that scope's locks would reopen the lock-order cycle with a
// declaration there, so the caller's transaction rolls back and runs again
// (writeSupersedesSource), and the next scope read names the new scope.
//
// The caller answers it as well when the lock pass missed the block — no live
// block of the write scopes had the id, so nothing was locked — and its own
// statement then changed a row with that id after all (issue #284): the block
// entered a write scope between the two statements, unlocked, and the targets
// of the supersedes links it may have brought along were never read. The run
// again reads it in that scope and locks it there.
var errSupersedesSourceChanged = errors.New("store: lock supersedes source: the block changed scope during the lock pass")

// supersedesSourceAttempts bounds those runs. A fixed bound, not a policy:
// each repeat needs another commit that moves the same block within the few
// statements between a scope read and the lock pass after it (or between the
// scope read and the caller's statement), so a third failure is not
// contention but a fault, and surfaces as the error — after a rollback of
// every attempt, nothing written.
const supersedesSourceAttempts = 3

// writeSupersedesSource is pgxdb.Write for a transaction that starts with
// lockSupersedesSource: it runs fn again while the lock pass reports
// errSupersedesSourceChanged, at most supersedesSourceAttempts times.
func writeSupersedesSource(ctx context.Context, db pgxdb.Beginner, s pgxdb.Stages, fn func(pgx.Tx) error) error {
	for attempt := 1; ; attempt++ {
		err := pgxdb.Write(ctx, db, s, fn)
		if !errors.Is(err, errSupersedesSourceChanged) || attempt >= supersedesSourceAttempts {
			return err
		}
	}
}

// lockSupersedesSource prepares a write that can end the supersedes of a block
// — archiving it, or moving it to another scope: it takes the block's scope's
// supersedes locks, then locks the block itself and every target of its
// supersedes links, in ONE statement and id order, and returns the targets. It
// runs BEFORE the caller's first write, because the reconcile after that
// write must not wait for a target lock (the lock discipline of
// ReconcileSupersedesTargets; until migration 181 the write also held the
// context_guard_state row to commit). The caller passes the targets to
// ReconcileSupersedesTargets after its write, and runs its transaction through
// writeSupersedesSource.
//
// The scope's supersedes lock comes first, EXCLUSIVE (LockSupersedesScope),
// before any row lock, so the transaction runs alone in its scope against
// every other holder of that lock (issue #284):
//   - a declaration locks its source before its target, while the lock pass
//     goes in id order — on a block that already supersedes a target sorting
//     before it the two orders disagree;
//   - a dream batch (dream.WriteLinks) and a supersedes resolve
//     (DreamLinkResolve) hold link rows and then lock target rows in their
//     reconcile, while a scope move holds the block and its targets and then
//     sweeps the block's link rows (sweepScopeMoveLinks);
//   - a dream batch with a supersedes guess could add a supersedes link from
//     the block while this transaction reads them.
//
// Each of these pairs ended one side with 40P01 (or missed a link) while the
// archive and the move took the lock shared. The lock is per scope and held
// for a few statements; dream batches, resolves and other archives of the
// scope wait for it, none of them holding a row lock. The scope is read
// without a lock first; the lock pass then holds only rows of that scope (its
// outer WHERE re-checks the scope on the row it locks), so a declaration of
// another scope never holds one of its rows. A block that left the scope in
// between answers errSupersedesSourceChanged.
//
// The block is matched only inside writeScopes, and so are the links (by the
// source's scope): a block the caller may not write locks nothing. A malformed
// or archived id locks nothing either; the caller's own statement answers it
// as before.
//
// missed reports that the scope read found no live block of writeScopes with
// the id, and nothing was locked. The caller's statement normally finds no row
// then either. If it changes one after all, the block entered a write scope in
// between (READ COMMITTED: the statement reads a later snapshot than the scope
// read), and the caller returns errSupersedesSourceChanged so that the
// transaction runs again with the block locked (issue #284). The flag costs
// the ordinary path nothing: no statement is added on a hit or a miss.
//
// The candidate ids are built as ONE array (the block, then its targets) and
// matched with = ANY: primary-key probes for a handful of ids. An `id = $1 OR
// id IN (subquery)` form is planned as a filter with a hashed subplan over a
// full primary-key scan (ORDER BY id) — every archive would read the corpus.
func lockSupersedesSource(ctx context.Context, tx pgx.Tx, blockID string, writeScopes []string) (targets []string, missed bool, err error) {
	parsed, err := uuid.Parse(blockID)
	if err != nil {
		// A malformed id never matches the caller's statement either.
		return nil, false, nil //nolint:nilerr // deliberate: the caller's statement decides a malformed id
	}
	// The statements below return ids as id::text, the canonical lowercase
	// form; a full id the caller typed in capitals (ResolveBlockID returns a
	// full id unchanged) is the same block and must compare equal to them.
	blockID = parsed.String()

	var scope string
	err = tx.QueryRow(ctx,
		`SELECT scope FROM context_blocks WHERE id = $1::uuid AND scope = ANY($2::text[]) AND NOT is_archived`,
		blockID, writeScopes).Scan(&scope)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, true, nil // not a live block the caller may write: nothing locked
	}
	if err != nil {
		return nil, false, fmt.Errorf("store: lock supersedes source: scope: %w", err)
	}
	if err := LockSupersedesScope(ctx, tx, scope); err != nil {
		return nil, false, err
	}

	rows, err := tx.Query(ctx, lockSupersedesSourceSQL, blockID, scope)
	if err != nil {
		return nil, false, fmt.Errorf("store: lock supersedes source: %w", err)
	}
	locked, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, false, fmt.Errorf("store: lock supersedes source: %w", err)
	}
	if !slices.Contains(locked, blockID) {
		return nil, false, errSupersedesSourceChanged
	}

	// The target list above comes from that statement's snapshot, taken
	// BEFORE it waited for the block's lock. Under the scope locks above no
	// declaration and no dream guess of the scope adds a supersedes link from
	// the block while this transaction runs, so the re-read normally finds no
	// target the statement did not lock; it stays for a writer outside that
	// discipline that held the block and committed while we waited. Now the
	// block is held, and this statement's fresh snapshot sees every such link;
	// its targets are locked as well (in id order among themselves), and the
	// re-read's list is the one returned. Both statements keep to the
	// locked scope like the lock pass: a target in another scope carries no
	// state from this block (the reconcile counts only sources of the
	// target's own scope), and its row may be held by a declaration there.
	if targets, err = lockLateSupersedesTargets(ctx, tx, []string{blockID}, []string{scope}, locked); err != nil {
		return nil, false, err
	}
	return targets, false, nil
}

// supersedesTargetsOfSQL reads the targets of the supersedes links of blocks
// ($1) that live in the source's own scope, one of the locked scopes ($2), in
// id order; idx_dream_links_supersedes serves the links, the primary key the
// scopes.
const supersedesTargetsOfSQL = `SELECT DISTINCT dl.target_block_id::text
	  FROM context_dream_links dl
	  JOIN context_blocks s ON s.id = dl.source_block_id
	  JOIN context_blocks t ON t.id = dl.target_block_id
	 WHERE dl.source_block_id = ANY($1::uuid[]) AND dl.relationship = 'supersedes'
	   AND s.scope = ANY($2::text[]) AND t.scope = s.scope
	 ORDER BY 1`

// lockLateSupersedesTargets reads the targets of the sources' supersedes links
// again — the sources are locked, so this statement's snapshot sees every link
// a writer committed while the lock pass waited for them — locks the ones not
// in locked (the rows the lock pass holds) in id order, within scopes (the
// locked scopes), and returns every current target the transaction now holds,
// in id order: the targets to reconcile after the caller's write.
func lockLateSupersedesTargets(ctx context.Context, tx pgx.Tx, sources, scopes, locked []string) ([]string, error) {
	rows, err := tx.Query(ctx, supersedesTargetsOfSQL, sources, scopes)
	if err != nil {
		return nil, fmt.Errorf("store: lock supersedes source: re-read: %w", err)
	}
	current, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("store: lock supersedes source: re-read: %w", err)
	}
	var held, late []string
	for _, id := range current {
		if slices.Contains(locked, id) {
			held = append(held, id)
		} else {
			late = append(late, id)
		}
	}
	if len(late) == 0 {
		return held, nil
	}
	rows, err = tx.Query(ctx,
		`SELECT id::text FROM context_blocks WHERE id = ANY($1::uuid[]) AND scope = ANY($2::text[]) ORDER BY id FOR NO KEY UPDATE`,
		late, scopes)
	if err != nil {
		return nil, fmt.Errorf("store: lock supersedes source: late targets: %w", err)
	}
	lateLocked, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("store: lock supersedes source: late targets: %w", err)
	}
	held = append(held, lateLocked...)
	slices.Sort(held)
	return held, nil
}

// lockSupersedesSourceSQL is lockSupersedesSource's statement ($1 = the block,
// $2 = the scope whose supersedes locks the transaction holds). Only a block
// in that scope contributes itself or its targets (the inner query joins on
// its scope), and only rows in that scope are locked: the outer b.scope is
// re-checked on the newest row version when the statement waited for it, so a
// block that moved away meanwhile is not locked.
const lockSupersedesSourceSQL = `SELECT b.id::text
	   FROM context_blocks b
	  WHERE b.id = ANY(
	          ARRAY(SELECT x.id FROM context_blocks x
	                 WHERE x.id = $1::uuid AND x.scope = $2)
	          || ARRAY(SELECT dl.target_block_id
	                     FROM context_dream_links dl
	                     JOIN context_blocks s ON s.id = dl.source_block_id
	                    WHERE dl.source_block_id = $1::uuid
	                      AND dl.relationship = 'supersedes'
	                      AND s.scope = $2))
	    AND b.scope = $2
	  ORDER BY b.id
	  FOR NO KEY UPDATE OF b`
