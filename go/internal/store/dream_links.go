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
	// undid the target's snapshot marking (the ApplySupersedes side-effect).
	SupersedesReverted bool `json:"supersedes_reverted"`
}

// DreamLinkResolve resolves ONE dream link identified by its primary key
// (source_block_id, target_block_id); relationship is matched as a guard, not
// an identifier (M016 PK carries no relationship — one link per pair).
//
//   - confirm: pinned=true (+ rationale when provided) — the link survives the
//     dream replace sweep (dream/writelinks.go deleteStaleLinks WHERE NOT
//     pinned, M119).
//   - delete: the link is removed; for relationship=supersedes the
//     ApplySupersedes side-effect is reverted (lifecycle_state 'snapshot' →
//     'knowledge', superseded_by=NULL — only while it still points at THIS
//     source), mirroring replaceStaleLinks' revert byte-for-byte.
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
	if err := pgxdb.Write(ctx, pool, pgxdb.At("store: dream link resolve"), func(tx pgx.Tx) error {
		// Lock the link row so a concurrent dream replace sweep cannot delete it
		// between classification and update (classifyGuardBatch pattern).
		var pinned bool
		var storedRationale *string
		err := tx.QueryRow(ctx,
			`SELECT dl.relationship, dl.pinned, dl.rationale
			 FROM context_dream_links dl
			 JOIN context_blocks s ON s.id = dl.source_block_id
			 WHERE dl.source_block_id = $1::uuid
			   AND dl.target_block_id = $2::uuid
			   AND dl.relationship = $3
			   AND s.scope = ANY($4::text[])
			 FOR UPDATE OF dl`,
			sourceID, targetID, relationship, writeScopes,
		).Scan(&storedRel, &pinned, &storedRationale)
		if errors.Is(err, pgx.ErrNoRows) {
			// (nil, nil) before the commit in the straight-line form —
			// pgxdb.ErrRollback keeps that miss a rollback instead of a commit.
			return pgxdb.ErrRollback
		}
		if err != nil {
			return fmt.Errorf("store: dream link resolve: select: %w", err)
		}

		res = &DreamLinkResolution{
			SourceID:     sourceID,
			TargetID:     targetID,
			Relationship: storedRel,
			Resolution:   resolution,
		}
		switch resolution {
		case "confirm":
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
				// Mirror of replaceStaleLinks' revert (dream/writelinks.go):
				// Welle-46 convention "A supersedes B" → the TARGET was marked
				// snapshot with superseded_by=source; undo only while that
				// marking still points at THIS source (a supersedes by another
				// block must not be reverted).
				tag, err := tx.Exec(ctx,
					`UPDATE context_blocks
					 SET lifecycle_state = 'knowledge', superseded_by = NULL
					 WHERE id = $1::uuid
					   AND lifecycle_state = 'snapshot'
					   AND superseded_by = $2::uuid`,
					targetID, sourceID,
				)
				if err != nil {
					return fmt.Errorf("store: dream link resolve: supersedes revert: %w", err)
				}
				res.SupersedesReverted = tag.RowsAffected() > 0
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
