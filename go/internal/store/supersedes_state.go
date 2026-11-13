// Supersedes lifecycle side-effect (ApplySupersedes and its reverse), shared
// by every path that writes or deletes supersedes dream links: dream.WriteLinks
// (incl. its stale-link sweep), dream.CleanupDanglingLinks and the operator's
// DreamLinkResolve delete. It lives in store because dream imports store, not
// the other way round — one reconcile, no copy of its rules per caller.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"slices"

	"github.com/jackc/pgx/v5"
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
//     locks before any UPDATE also matters because every UPDATE on
//     context_blocks fires mark_guard_dirty, which locks the single
//     context_guard_state row until commit: a transaction that already holds
//     that row must not wait for a target lock afterwards.
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
