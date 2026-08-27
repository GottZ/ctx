package store

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/derived"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ClassifyBlockAfterUpsert sets type_name on a stored block from the
// resolved block-type policy set (WF T4, design/01 §4.4 #14/#15 + §4.5).
// Welle 44 introduced the hook with a hardcoded decision tree; since T4 the
// rules are REGISTRY DATA (classify.metadata_flags / source_prefixes /
// title_patterns per type, priority-ordered) and this function only applies
// set.Classify's verdict.
//
// Semantics (golden-pinned against the pre-T4 tree, classify_golden_test.go):
//
//  1. set.Classify(title, metadata) — first match in (priority, name) order.
//     Builtin seeds reproduce the old tree byte-equivalently: metadata
//     is_meta flag (a JSONB KEY — classify INPUT, not a column; M075/T9
//     dropped the materialised mirror) → system-meta (prio 10); source
//     "dream-*" or audit title pattern → audit-trail (prio 20).
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
// The hook runs on /api/store, MCP store, digest topic-map, dream daily
// report AND (since T4) the manage-update path. Returns the applied type
// name ("" when nothing was written) for caller logging.
func ClassifyBlockAfterUpsert(ctx context.Context, pool *pgxpool.Pool, set *blocktype.Set, blockID, title string, metadata map[string]any) (typeName string, err error) {
	if set == nil {
		// Loud, not silent: a nil set means the caller skipped the registry
		// wiring — falling back to any compiled-in behaviour here would hide
		// exactly the drift the T4 policy-effect gate probes.
		return "", fmt.Errorf("store: classify block: nil block-type set (registry not wired)")
	}

	name, matched := set.Classify(title, metadata)
	if !matched {
		return "", nil
	}

	// I7, fourth door (design D-01 §4.3.1 principle; seed-review finding #3):
	// the classifier may not GRANT a derivation level. S1 refuses a client that
	// NAMES a derived type, S2 its category, S3 its identity — none of them sees
	// a write that simply carries a title matching a derived type's
	// classify.title_patterns, and the registry rules would then hand it
	// type_name='catalog' with type_source='auto', i.e. guard.check=false,
	// guard.candidate=false, dream.linkable=false and, after the E-4 visibility
	// switch, the catalogue damping factor. The level would come from a title.
	//
	// §4.3.1 is a statement about the mechanism, not about a surface: the level
	// of a block is assigned by the system, never by a client, and a title
	// pattern is a client-supplied string on every write surface in this tree.
	// A derived writer is unaffected because it names its type on the write —
	// that stamps type_source='manual' and takes the row out of this hook for
	// good (the UPDATE below carries AND type_source = 'auto'). What the seed
	// wave called the "net for a writer that forgot the type" would produce a
	// derivative WITHOUT a provenance object, which §3.2/§4.5.3 forbid anyway,
	// so the net could only ever have caught malformed blocks.
	//
	// Refused, not applied: the block keeps the type it has (the no-op path
	// above is the same one an unmatched title takes). Logged because an
	// attempt is an I7 signal, and the write rate limit bounds how often a
	// single key can produce one.
	if derived.IsDerivedType(name) {
		slog.Warn("store: classify declined a derived type — the level is assigned by the writer, never by a title (I7/S4)",
			"block_id", blockID, "declined_type", name)
		return "", nil
	}

	tag, err := pool.Exec(ctx,
		`UPDATE context_blocks SET type_name = $1
		  WHERE id = $2::uuid AND type_source = 'auto'`,
		name, blockID,
	)
	if err != nil {
		return "", fmt.Errorf("store: classify block: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// manual override (or vanished row): nothing applied.
		return "", nil
	}

	return name, nil
}
