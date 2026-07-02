// Package visibility — THE single shared SQL fragment for "this block is
// visible" (WF T6, design/01-type-registry.md §7-T6).
//
// Before T6 the canonical triple lived in store.VisibilityPredicate while
// rrf/graph.go (neighbor hydrate) and overview/cluster.go (Louvain clustering,
// 4 sites) carried INLINE COPIES of the same conjuncts — exactly the copies
// the old visibility.go doc header promised not to exist. T6 consolidates:
// this leaf package owns the fragment; store.VisibilityPredicate delegates,
// rrf and overview embed it directly. It lives OUTSIDE internal/store because
// rrf cannot import store (store → blocktype → rrf would cycle).
//
// T6 also replaces the hardcoded `type_name <> 'system-meta'` EXCLUDE literal
// with the registry ALLOWLIST parameter (`type_name = ANY($n::text[])`,
// fail-closed): a block whose type is not in the resolved allowlist is
// invisible, matching ctx_rrf's M073 semantics (design/01 §3.5/§5.1a).
package visibility

import "fmt"

// TypeVisible returns the type-level visibility pair for a context_blocks
// alias:
//
//	NOT <alias>.is_archived AND <alias>.type_name = ANY(<typesParam>::text[])
//
// typesParam binds the resolved retrieval allowlist (blocktype.Set
// .VisibleTypes, or a derived cut such as the overview node sieve). A NULL or
// empty array matches ZERO rows — deliberately hard (fail-closed); Go callers
// guarantee a non-empty list and surface an empty one as a wiring error
// (rrf.Search pattern).
//
// This is the scope-free sub-fragment for call sites whose scope handling is
// structural rather than predicative (overview: scope partitioning is the
// GROUP BY; the rebuild is one global run, §9.6 caveat).
func TypeVisible(alias, typesParam string) string {
	return fmt.Sprintf(
		"NOT %[1]s.is_archived AND %[1]s.type_name = ANY(%[2]s::text[])",
		alias, typesParam,
	)
}

// Predicate returns the canonical visibility triple:
//
//	NOT <alias>.is_archived
//	AND <alias>.type_name = ANY(<typesParam>::text[])
//	AND ( <alias>.scope = ANY(<scopeParam>::text[])
//	      OR <alias>.id = ANY(<grantParam>::uuid[]) )
//
// scope is gated on context_blocks.scope (authoritative), never on
// context_dream_links.scope. All parameter placeholders are code-owned
// constants at every call site — never user input — so embedding the fragment
// via Sprintf adds no injection surface.
//
// CRITICAL — the mandatory parentheses (design/07 §4 / §5.5): the archived /
// type conjuncts stand BEFORE the parenthesised group; the additive
// block-level OR lives strictly INSIDE ( scope=ANY OR id=ANY ). SQL binds AND
// tighter than OR — without the parentheses a granted, archived (or
// type-invisible) block would leak through the bare `OR id = ANY(...)`. The
// grant arm is PURELY additive: an empty grant set ('{}'::uuid[]) makes
// `id = ANY('{}')` a deterministic FALSE, so the predicate collapses
// byte-equivalently to the scope-only state (pausability invariant).
func Predicate(alias, typesParam, scopeParam, grantParam string) string {
	return fmt.Sprintf(
		"%s AND ( %[2]s.scope = ANY(%[3]s::text[]) OR %[2]s.id = ANY(%[4]s::uuid[]) )",
		TypeVisible(alias, typesParam), alias, scopeParam, grantParam,
	)
}
