package store

import (
	"context"
	"fmt"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ClassifyBlockAfterUpsert sets type_name and is_meta on a stored block from
// the resolved block-type policy set (WF T4, design/01 §4.4 #14/#15 + §4.5).
// Welle 44 introduced the hook with a hardcoded decision tree; since T4 the
// rules are REGISTRY DATA (classify.metadata_flags / source_prefixes /
// title_patterns per type, priority-ordered) and this function only applies
// set.Classify's verdict.
//
// Semantics (golden-pinned against the pre-T4 tree, classify_golden_test.go):
//
//  1. set.Classify(title, metadata) — first match in (priority, name) order.
//     Builtin seeds reproduce the old tree byte-equivalently: is_meta flag →
//     system-meta (prio 10); source "dream-*" or audit title pattern →
//     audit-trail (prio 20).
//
//  2. No rule match → no-op ("" returned, no UPDATE): the block keeps its
//     INSERT defaults respectively its current type. Deliberately asymmetric —
//     the hook promotes on match but never demotes back to the default type
//     when a later title/metadata update stops matching (pre-T4 behaviour,
//     kept: an auto audit-trail block whose title loses the pattern stays
//     audit-trail).
//
//  3. type_source='manual' overrides the auto-classifier permanently
//     (sensitivity_source pattern, §4.5): the UPDATE carries
//     `AND type_source = 'auto'` — a manual block is never re-classified.
//     applied=false in that case (rowsAffected 0).
//
// is_meta bridges the legacy column until M075 drops it (T9): it mirrors the
// metadata flag exactly as the old branch-1 did.
//
// The hook runs on /api/store, MCP store, digest topic-map, dream daily
// report AND (since T4) the manage-update path. Returns the applied type
// name ("" when nothing was written) for caller logging.
func ClassifyBlockAfterUpsert(ctx context.Context, pool *pgxpool.Pool, set *blocktype.Set, blockID, title string, metadata map[string]any) (typeName string, isMeta bool, err error) {
	if set == nil {
		// Loud, not silent: a nil set means the caller skipped the registry
		// wiring — falling back to any compiled-in behaviour here would hide
		// exactly the drift the T4 policy-effect gate probes.
		return "", false, fmt.Errorf("store: classify block: nil block-type set (registry not wired)")
	}

	name, matched := set.Classify(title, metadata)
	if !matched {
		return "", false, nil
	}
	if metaFlag, ok := metadata["is_meta"].(bool); ok && metaFlag {
		isMeta = true
	}

	tag, err := pool.Exec(ctx,
		`UPDATE context_blocks SET type_name = $1, is_meta = $2
		  WHERE id = $3::uuid AND type_source = 'auto'`,
		name, isMeta, blockID,
	)
	if err != nil {
		return "", false, fmt.Errorf("store: classify block: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// manual override (or vanished row): nothing applied.
		return "", false, nil
	}

	return name, isMeta, nil
}
