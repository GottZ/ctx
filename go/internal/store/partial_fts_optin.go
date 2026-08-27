package store

import "slices"

// The partial FTS GIN indexes and what a consumer has to declare to reach them
// (C2-2, OPS-W1 review auflagen A2/A3).
//
// Migration 145 rebuilt idx_context_ts_de / idx_context_ts_en as PARTIAL indexes
// (145:300,334):
//
//	CREATE INDEX idx_context_ts_de ON context_blocks USING GIN(ts_de)
//	  WHERE type_name NOT IN ('checkpoint','system-meta')
//
// That takes 97 % of the index mass out of retrieval — and it moves the burden
// of proof to every reader: PostgreSQL only uses a partial index when it can
// PROVE the index predicate from the query's own quals. A restriction stated
// through a bind parameter (`type_name = ANY($5)`, `type_name = $2`) carries no
// such proof the moment the plan cache serves a GENERIC plan, and that plan is
// production-reachable: pgx runs the extended protocol with a statement cache
// and plancache.c (choose_custom_plan) switches from the 6th execution per
// connection. The review measured what the two consumers then lose at 100 000
// rows: SearchBlocks 721 → 8 304 (11,5×, full Seq Scan), issue FTS 774 → 14 126
// (18,2×, both FTS indexes gone from the plan).
//
// The fix is therefore not a filter but a DECLARATION: constant text that the
// planner can use as a premise, added exactly where it is already implied by
// what the statement says anyway. Nothing here changes a row set; every use site
// is pinned by an identity gate (partial_fts_optin_c22_integration_test.go).

// hardFTSDenyTypes is the deny-list migration 145 froze into both index
// predicates. Not a policy and not configurable — it is the same hard pair the
// measurement seam refuses (handler/query_shadow.go:38-53), and it is
// deliberately NOT "all retrieval-excluded types": an index predicate cannot
// carry the registry lookup that RetrievalExcludedTypePredicate
// (blocktypes.go:113-125) performs (PostgreSQL rejects a subquery in an index
// predicate, SQLSTATE 0A000).
//
// Kept in two shapes on purpose — a Go slice for the implication rule, SQL text
// for the statements — and pinned against each other by
// TestC22DenyListShapesAgree, so a change to one cannot travel without the other.
var hardFTSDenyTypes = []string{"checkpoint", "system-meta"}

const (
	// hardFTSDenyValues is the deny-list as the SQL IN-list, spelled exactly
	// like the index predicate.
	hardFTSDenyValues = `('checkpoint','system-meta')`

	// hardFTSDenyConjunct is the predicate a consumer declares to make the
	// partial index provable. Defined once and concatenated into the call sites
	// — the house pattern of RetrievalExcludedTypePredicate — so a deny-list
	// change cannot land in one statement and miss the other.
	hardFTSDenyConjunct = `type_name NOT IN ` + hardFTSDenyValues
)

// impliesHardFTSDeny reports whether the caller's own opt-in type filters
// already restrict the result to types outside the deny-list. Only then may a
// consumer add the static conjunct: the addition has to be redundant, because a
// non-redundant one would silently drop deny-listed blocks from the browse
// surfaces — the D5 asymmetry says the opposite (excluded-policy types stay
// browseable, only retrieval ranks them out; blocks.go:1276-1282).
//
// Both filters are known VALUES at statement-build time, which is what makes the
// implication decidable in Go while it is undecidable for the planner:
//
//   - types without any deny-listed name: `type_name = ANY(types)` restricts the
//     rows to that list, and the list is disjoint from the deny-list;
//   - typesExclude ⊇ deny-list: `NOT (type_name = ANY(typesExclude))` excludes at
//     least the deny-listed names.
//
// Either shape alone suffices — the statement ANDs its filters, so one true
// premise implies the conjunct for the whole WHERE clause.
func impliesHardFTSDeny(types, typesExclude []string) bool {
	if len(types) > 0 && !containsAnyDenyType(types) {
		return true
	}
	if len(typesExclude) == 0 {
		return false
	}
	for _, deny := range hardFTSDenyTypes {
		if !slices.Contains(typesExclude, deny) {
			return false
		}
	}
	return true
}

// containsAnyDenyType reports whether the caller explicitly asked for one of the
// deny-listed types — the case in which the conjunct would change the answer.
func containsAnyDenyType(types []string) bool {
	for _, deny := range hardFTSDenyTypes {
		if slices.Contains(types, deny) {
			return true
		}
	}
	return false
}
