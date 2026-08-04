// Package clustersql — THE single shared SQL fragment for "cluster
// membership, scope-pure" (Cluster-Topic-Map, design/03 §4.1/§5.2).
//
// Leaf package for exactly the reason internal/visibility is one: rrf cannot
// import store (store → blocktype → rrf would cycle), and the cluster
// membership filter is needed by rrf (the retrieval boost), by store (ego
// annotation, cluster route, facet) and potentially by handler. The same
// constellation once produced three inline copies of the visibility predicate;
// this package exists so it does not produce copies of the scope conjunction.
//
// THE SCOPE CONJUNCTION IS NOT OPTIONAL. graph_cluster_member carries no
// visibility logic: block_id is the SOLE primary key (migration 057),
// cluster_id has no foreign key, and the only partition information is the
// scope column (migration 087). Without the conjunction a join returns the
// cluster affiliation of foreign-private blocks — and that is a side channel on
// foreign community structure, not merely extra rows: a caller holding a single
// block grant on a foreign block would see its cluster_id and could test which
// of its OWN blocks carry the same value, reconstructing foreign community
// boundaries without ever reading a foreign block (design/03 §5.2, risk R3).
//
// The conjunction also does the T41 leaf rule for free, one level up: a
// grant-only block's scope is by definition NOT in readScopes, so its
// membership row filters out and the block appears with "no visible cluster" —
// never with a foreign one. That falls out of the scope filter instead of
// needing a special case, which is why it survives refactors.
package clustersql

import "fmt"

// MemberOf returns the scope predicate for a graph_cluster_member alias:
//
//	<alias>.scope = ANY(<scopeParam>::text[])
//
// scopeParam binds the caller's resolved read scopes. An EMPTY array matches
// zero rows — deliberately hard (fail-closed, same posture as
// visibility.TypeVisible); Go callers guarantee a non-empty set through
// store.RequireScopes and surface an empty one as an error rather than letting
// PostgreSQL's `= ANY('{}')` collapse it into a silent empty result.
//
// Unlike visibility.Predicate this fragment needs no parentheses discipline: it
// is a single conjunct with no OR arm, so it composes safely behind any AND.
// Every placeholder at every call site is a code-owned constant, never user
// input — embedding via Sprintf adds no injection surface.
func MemberOf(alias, scopeParam string) string {
	return fmt.Sprintf("%s.scope = ANY(%s::text[])", alias, scopeParam)
}

// MembershipQuery is the batch read "which of these blocks sit in which
// cluster, as far as the caller may see":
//
//	$1 — block ids  (uuid[])
//	$2 — read scopes (text[])
//
// A PK probe over a bounded id set, so the cheapest query shape the system has
// — which is what makes it affordable on the ego cache arm too, where it is the
// only remaining roundtrip.
//
// It is a package-level VARIABLE rather than a constant on purpose: a constant
// cannot be built from MemberOf (Go const expressions cannot call functions),
// so a constant would mean typing the conjunction a SECOND time — exactly the
// duplication this package exists to prevent. The build-time composition is
// what lets the C1 negative probe break all consumers from one edit.
var MembershipQuery = `
	SELECT m.block_id::text, m.cluster_id::text
	FROM graph_cluster_member m
	WHERE m.block_id = ANY($1::uuid[])
	  AND ` + MemberOf("m", "$2")
