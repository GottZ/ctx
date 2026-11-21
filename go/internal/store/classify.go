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
//  4. The verdict is written only while the block still holds the inputs it
//     was computed from (issue #426): the title and the part of the metadata
//     set.Classify reads (classifyInputsSQL). The hook runs after the
//     write committed, outside its lock, from the block the write returned;
//     two writes A, B of one block that commit in that order can finish their
//     hooks in the order B, A, and A's verdict must not land on B's title and
//     metadata (in the reported case system-meta, retrieval=excluded, on a
//     block whose metadata classify as audit-trail). A's UPDATE then matches
//     no row and returns "". The version is the input itself, not updated_at:
//     a write between the two that changes tags, content or a metadata key no
//     rule reads (the guard's own keys among them) leaves the verdict valid.
//     The flags compared are those of the set the verdict came from: a write
//     under other type rules (another tenant's overlay on a shared scope, a
//     registry reload in between) that changes only a flag this set does not
//     know is not seen.
//
// The comparison sits in the WHERE of the one UPDATE (classifyUpdateSQL): no
// extra statement and no lock beyond the row lock the UPDATE takes anyway. A
// write of the block in flight makes the UPDATE wait; once it committed, READ
// COMMITTED re-checks the WHERE against the row's newest version, so the
// verdict is judged against the inputs that write left behind.
//
// The hook runs on /api/store (handler/context_store.go), MCP store
// (handler/mcp.go), confirmed staged writes (handler/confirm_core.go),
// block-mode /api/ingest (handler/ingest.go), the digest topic map and its
// stub (digest/digest.go), the root map (rootmap/run.go), the dream daily
// report (dream/synthesize_report.go) AND (since T4) the update afterwork
// (handler/mcp_update.go finishBlockUpdate: REST manage update, MCP update,
// confirmed updates). Returns the applied type name ("" when nothing was
// written) for caller logging.
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

	flags := set.ClassifyMetadataFlags()
	tag, err := pool.Exec(ctx, classifyUpdateSQL, name, blockID, flags, classifyInputs(flags, title, metadata))
	if err != nil {
		return "", fmt.Errorf("store: classify block: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// manual override, a later write replaced the inputs (point 4), or
		// a vanished row: nothing applied.
		return "", nil
	}

	return name, nil
}

// classifyUpdateSQL writes the verdict $1 on block $2 while it is an auto
// block that holds the inputs $4 (classifyInputs) under the flag keys $3.
const classifyUpdateSQL = `UPDATE context_blocks SET type_name = $1
	  WHERE id = $2::uuid AND type_source = 'auto'
	    AND ` + classifyInputsSQL + ` = $4::jsonb`

// classifyInputsSQL is what set.Classify reads of the stored row, as one jsonb
// object: the title, and of the metadata "source" when its value is a string
// and every flag key of $3 (Set.ClassifyMetadataFlags) whose value is true —
// the verdict depends on nothing else. classifyInputs builds the same object
// from the title and metadata the verdict was computed from, so the two are
// equal exactly when the stored row would classify the same.
//
// One expression, so that no index can serve it and the primary key stays the
// only way to the row: a bare `title = $n` beside `id = $2` is a condition the
// trigram indexes on title (idx_trgm_title, idx_trgm_title_gist) support, and
// on the reference instance the generic plan of that form scanned
// idx_trgm_title_gist with the id as a filter. The comparison is evaluated
// after the type_source test, so a manual block never reads its metadata here;
// the metadata is read once (jsonb_each), from TOAST when it is stored out of
// line. A NULL metadata reads as {}; one that is not an object, which no write
// path stores (every one binds a map), fails the statement and the verdict is
// not written.
const classifyInputsSQL = `jsonb_build_object('title', context_blocks.title, 'metadata',
	      (SELECT COALESCE(jsonb_object_agg(e.key, e.value), '{}'::jsonb)
	         FROM jsonb_each(context_blocks.metadata) AS e
	        WHERE (e.key = 'source' AND jsonb_typeof(e.value) = 'string')
	           OR (e.key = ANY($3::text[]) AND e.value = 'true'::jsonb)))`

// classifyInputs is classifyInputsSQL for the title and decoded metadata a
// verdict was computed from, read the way set.Classify reads them: the source
// as a string, a flag as the boolean true.
func classifyInputs(flags []string, title string, metadata map[string]any) map[string]any {
	md := map[string]any{}
	if source, ok := metadata["source"].(string); ok {
		md["source"] = source
	}
	for _, flag := range flags {
		if v, ok := metadata[flag].(bool); ok && v {
			md[flag] = true
		}
	}
	return map[string]any{"title": title, "metadata": md}
}
