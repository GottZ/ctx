package store

import (
	"slices"

	"github.com/GottZ/ctx/internal/blocktype"
)

// SearchTypesExclude is the type exclusion a block search binds (issue #342,
// settings key search.leave_out_never_ranked). Every surface that reaches the
// search statement takes it from here — the shared core of REST /api/search and
// the MCP search tool (handler/search_core.go) and the web chat's ctx_search
// (chat/tools.go) — so the rule stands once.
//
// With leaveOutNeverRanked off it returns typesExclude untouched: the statement
// binds byte for byte what it bound before the key existed. With it on, a
// full-text search (query non-empty) that names no type also excludes the
// never-ranked types of set — every type whose retrieval policy is excluded,
// the types query never ranks: checkpoint, system-meta and the derived layers
// on a stock registry. set is the caller's tenant-resolved registry snapshot,
// the one query takes its visible types from (Set.VisibleTypes), so a tenant's
// own excluded type is left out for that tenant, and a type the snapshot ranks
// stays in, whatever another tenant's registry says. A type the snapshot does
// not know stays in: the MCP tool could not name it in types, so leaving it out
// would make it unreachable by full text.
//
// What it leaves alone, and why:
//
//   - A search that names types (types non-empty) asked for those types; the
//     filter it binds already says which rows it wants.
//   - A listing (empty query) and recent: the browse surfaces keep every type
//     (D5), and their cost does not depend on the full-text indexes.
//   - set nil (no registry wired): there is no policy to read, so nothing is
//     added.
//
// The index: the result lists typesExclude first, in its order, then the
// never-ranked names in name order, each once. When it holds both names of the
// partial indexes' deny-list, searchBlocksSQL declares the indexes' predicate
// as constant text (impliesHardFTSDeny), which is what lets the planner use
// them under a generic plan as well. The type API refuses a tenant overlay
// that lifts checkpoint or system-meta (handler/types_write.go,
// hardDenyOverlayViolation); should a registry rank one of them all the same (a
// row written around the API), the conjunct is not declared, the type stays in,
// and the search reads every vector of the remaining rows, as before.
func SearchTypesExclude(set *blocktype.Set, leaveOutNeverRanked bool, query string, types, typesExclude []string) []string {
	if !leaveOutNeverRanked || query == "" || len(types) > 0 || set == nil {
		return typesExclude
	}
	visible := set.VisibleTypes()
	out := slices.Clone(typesExclude)
	for _, name := range set.Names() {
		if _, ranked := slices.BinarySearch(visible, name); ranked || slices.Contains(out, name) {
			continue
		}
		out = append(out, name)
	}
	return out
}
