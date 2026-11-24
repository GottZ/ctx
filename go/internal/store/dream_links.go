// Dream-link curation (review-governance wave 2026-07-26): the single-link
// resolve behind manage dream-link-resolve. Closes the gap the external
// review named — dream-review was read-only, no user-driven per-link revoke
// and no durable per-link justification existed. Mirrors the GuardResolve/
// GuardResolveBatch doctrine (blocks.go): fail-closed RequireScopes, one
// transaction, and NO existence oracle — a link that is foreign, invisible,
// malformed or simply absent collapses into the same nil result.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/GottZ/ctx/internal/pgxdb"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DreamLinkResolution reports the outcome of one DreamLinkResolve call.
type DreamLinkResolution struct {
	SourceID     string  `json:"source_id"`
	TargetID     string  `json:"target_id"`
	Relationship string  `json:"relationship"`
	Resolution   string  `json:"resolution"` // confirm | delete
	Pinned       bool    `json:"pinned"`
	Rationale    *string `json:"rationale,omitempty"`
	// SupersedesReverted is true when a delete-resolve of a supersedes link
	// returned its target from snapshot to knowledge — i.e. no superseder of
	// the target remained (ReconcileSupersedesTargets). A target another
	// block still supersedes stays a snapshot and reports false.
	SupersedesReverted bool `json:"supersedes_reverted"`
}

// DreamLinkResolve resolves ONE dream link identified by its primary key
// (source_block_id, target_block_id); relationship is matched as a guard, not
// an identifier (M016 PK carries no relationship — one link per pair).
//
//   - confirm: pinned=true (+ rationale when provided) — the link survives the
//     dream replace sweep (dream/writelinks.go deleteStaleLinks WHERE NOT
//     pinned, M119).
//   - delete: the link is removed; for relationship=supersedes the target's
//     ApplySupersedes side-effect is reconciled against the REMAINING
//     supersedes links by ReconcileSupersedesTargets — the same reconcile
//     dream.WriteLinks runs after its stale-link sweep: the target returns to
//     'knowledge' (superseded_by=NULL) only when no superseder is left; if
//     the pointer named THIS source and another superseder remains, the
//     pointer moves to it.
//
// Write gate: the SOURCE block's scope must be in writeScopes (visibility
// doctrine — curation requires write access to the block whose dream cycle
// owns the link). Foreign/invisible/absent/malformed ids AND a relationship
// mismatch all collapse into (nil, nil) — uniform not found, no existence
// oracle and no disclosure of the actual stored relationship: the operator
// resolves the link AS SEEN in dream-review; a link re-classified in between
// must be re-reviewed, not blindly resolved.
func DreamLinkResolve(ctx context.Context, pool *pgxpool.Pool, sourceID, targetID, relationship, resolution, rationale string, writeScopes []string) (*DreamLinkResolution, error) {
	if resolution != "confirm" && resolution != "delete" {
		return nil, fmt.Errorf("store: dream link resolve: invalid resolution %q (must be 'confirm' or 'delete')", resolution)
	}
	if err := RequireScopes(writeScopes); err != nil { // T07 fail-closed
		return nil, err
	}
	// Malformed ids degrade to not-found instead of a SQLSTATE 22P02 error —
	// uniform with foreign/absent (dedupeGuardBatchIDs pattern).
	if _, err := uuid.Parse(sourceID); err != nil {
		return nil, nil //nolint:nilerr // deliberate: a malformed id is not-found, not an error (see above)
	}
	if _, err := uuid.Parse(targetID); err != nil {
		return nil, nil //nolint:nilerr // deliberate: a malformed id is not-found, not an error (see above)
	}

	// storedRel outlives the transaction: the audit row below quotes it.
	var (
		res       *DreamLinkResolution
		storedRel string
	)
	// A supersedes resolve takes the source scope's supersedes lock first
	// (issue #284), so its transaction may be asked to run again
	// (writeSupersedesSource) when the source changed scope before the lock.
	supersedes := relationship == "supersedes"
	write := pgxdb.Write
	if supersedes {
		write = writeSupersedesSource
	}
	if err := write(ctx, pool, pgxdb.At("store: dream link resolve"), func(tx pgx.Tx) error {
		lockedScope, found, err := lockSupersedesResolve(ctx, tx, sourceID, writeScopes, supersedes, resolution == "confirm")
		if err != nil {
			return err
		}
		if !found {
			return pgxdb.ErrRollback // not found, as the link read below answers it
		}
		// Lock the link row so a concurrent dream replace sweep cannot delete it
		// between classification and update (classifyGuardBatch pattern).
		var pinned, settles bool
		var storedRationale *string
		var sourceScope string
		err = tx.QueryRow(ctx,
			`SELECT dl.relationship, dl.pinned, dl.rationale, s.scope,
			        dl.confidence >= $5::real AND NOT t.is_archived
			 FROM context_dream_links dl
			 JOIN context_blocks s ON s.id = dl.source_block_id
			 JOIN context_blocks t ON t.id = dl.target_block_id
			 WHERE dl.source_block_id = $1::uuid
			   AND dl.target_block_id = $2::uuid
			   AND dl.relationship = $3
			   AND s.scope = ANY($4::text[])
			 FOR UPDATE OF dl`,
			sourceID, targetID, relationship, writeScopes, supersedesSnapshotConfidence,
		).Scan(&storedRel, &pinned, &storedRationale, &sourceScope, &settles)
		if errors.Is(err, pgx.ErrNoRows) {
			// (nil, nil) before the commit in the straight-line form —
			// pgxdb.ErrRollback keeps that miss a rollback instead of a commit.
			return pgxdb.ErrRollback
		}
		if err != nil {
			return fmt.Errorf("store: dream link resolve: select: %w", err)
		}
		if supersedes && sourceScope != lockedScope {
			// Moved to another write scope between the scope read and the
			// lock: the lock held is not that scope's.
			return errSupersedesSourceChanged
		}

		res = &DreamLinkResolution{
			SourceID:     sourceID,
			TargetID:     targetID,
			Relationship: storedRel,
			Resolution:   resolution,
		}
		switch resolution {
		case "confirm":
			// A pinned supersedes link at the snapshot confidence settles its
			// target (SettledSupersedesSQL), as a declaration does, and a
			// target has at most one settling superseder: the confirm runs
			// the declaration's already-superseded gate and is refused like a
			// second declaration, with the head of the chain (issue #284). A
			// guess below that confidence settles nothing when pinned, and a
			// confirm of a link that already settles adds nothing; nor does a
			// link to an archived target, which has no lifecycle to settle
			// (the reconcile passes it by), so its confirm is not refused for
			// another settler of the archived block (issue #547).
			if supersedes && settles && !pinned {
				if err := supersedesSettledByOther(ctx, tx, sourceID, targetID, lockedScope); err != nil {
					return err
				}
			}
			// rationale only overwrites when provided — confirm without text
			// keeps an earlier justification instead of blanking it.
			err = tx.QueryRow(ctx,
				`UPDATE context_dream_links
				 SET pinned = true,
				     rationale = COALESCE(NULLIF($3, ''), rationale)
				 WHERE source_block_id = $1::uuid AND target_block_id = $2::uuid
				 RETURNING pinned, rationale`,
				sourceID, targetID, rationale,
			).Scan(&res.Pinned, &res.Rationale)
			if err != nil {
				return fmt.Errorf("store: dream link resolve: confirm: %w", err)
			}
		case "delete":
			if _, err := tx.Exec(ctx,
				`DELETE FROM context_dream_links
				 WHERE source_block_id = $1::uuid AND target_block_id = $2::uuid`,
				sourceID, targetID,
			); err != nil {
				return fmt.Errorf("store: dream link resolve: delete: %w", err)
			}
			if storedRel == "supersedes" {
				// Welle-46 convention "A supersedes B" → the TARGET was marked
				// snapshot. Whether it stays one depends on the supersedes
				// links that REMAIN, not on this one alone (PR #44 review F2):
				// a second superseder keeps the snapshot. Link row first (the
				// FOR UPDATE above), target row in the reconcile — the lock
				// order every supersedes path keeps.
				restored, err := ReconcileSupersedesTargets(ctx, tx, []string{targetID}, "")
				if err != nil {
					return fmt.Errorf("store: dream link resolve: supersedes reconcile: %w", err)
				}
				res.SupersedesReverted = len(restored) > 0
			}
		}
		return nil
	}); err != nil {
		if errors.Is(err, pgxdb.ErrRollback) {
			return nil, nil
		}
		return nil, err
	}

	// Audit outside the tx (WriteLinks pattern — a log failure never rolls
	// back the resolve). decision prefix 'dream_*' per migration 016.
	meta, _ := json.Marshal(map[string]any{
		"target_id":           targetID,
		"relationship":        storedRel,
		"resolution":          resolution,
		"supersedes_reverted": res.SupersedesReverted,
	})
	_, _ = pool.Exec(ctx,
		`INSERT INTO context_write_log
			(block_id, decision, similarity, scope, block_title, block_category, metadata)
		SELECT $1::uuid, 'dream_link_resolve', 0, scope, title, category, $2::jsonb
		FROM context_blocks WHERE id = $1::uuid`,
		sourceID, meta,
	)

	return res, nil
}

// lockSupersedesResolve takes, for the resolve of a supersedes link
// (supersedes true), the supersedes lock of the source's scope and returns
// that scope; for any other relationship it does nothing and returns ("",
// true). Neither kind of resolve may run beside the exclusive holders of the
// scope (issue #284):
//
//   - delete locks the link row and then the target in its reconcile, while
//     a declaration locks the target and then writes the link row, and an
//     archive or scope move locks the source and its targets and then, for a
//     move, sweeps the link rows. SHARED, like a dream batch, whose row-lock
//     order it keeps;
//   - confirm (exclusive true) pins the link, which settles the target, and
//     runs the already-superseded gate first, which reads the pins of OTHER
//     sources — no row lock of its own covers them, as for a declaration.
//     Side by side with a declaration, or with another confirm on the same
//     target, each checked before the other committed and both settled the
//     target. EXCLUSIVE, like a declaration: one of them runs first, and the
//     other's gate sees its pin and refuses.
//
// The scope is read without a lock, within writeScopes; a source outside them
// answers found=false (not found, no lock taken, as before). The caller
// compares the scope it then reads under the link lock with the returned one.
func lockSupersedesResolve(ctx context.Context, tx pgx.Tx, sourceID string, writeScopes []string, supersedes, exclusive bool) (scope string, found bool, err error) {
	if !supersedes {
		return "", true, nil
	}
	err = tx.QueryRow(ctx,
		`SELECT scope FROM context_blocks WHERE id = $1::uuid AND scope = ANY($2::text[])`,
		sourceID, writeScopes).Scan(&scope)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: dream link resolve: source scope: %w", err)
	}
	lock := ShareSupersedesScope
	if exclusive {
		lock = LockSupersedesScope
	}
	if err := lock(ctx, tx, scope); err != nil {
		return "", false, err
	}
	return scope, true, nil
}
