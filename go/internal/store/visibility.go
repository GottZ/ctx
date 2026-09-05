// visibility.go — canonical block-visibility predicate (F5-W1, typ-param T6).
//
// This file is the store-side entry to THE single shared SQL fragment for
// "this block is visible to the caller" (internal/visibility since WF T6).
// The graph endpoint references it from every hop leg, the degree counter and
// the focus hydrate; ctx_rrf carries the same triple on the retrieval side,
// and rrf/graph.go + overview/cluster.go embed the same fragment directly.
// With T6 the type conjunct graduated from the hard `<> 'system-meta'` enum
// literal to the data-driven registry allowlist (workflow-engine line) —
// exactly the change the pre-T6 header reserved for this function and
// ctx_rrf. No inline copies remain to chase.
package store

import "github.com/GottZ/ctx/internal/visibility"

// VisibilityPredicate returns the canonical visibility triple for a
// context_blocks alias, a type-allowlist bind-parameter placeholder, a scope
// bind-parameter placeholder and a block-grant bind-parameter placeholder:
//
//	NOT <alias>.is_archived
//	AND <alias>.type_name = ANY(<typesParam>::text[])
//	AND ( <alias>.scope = ANY(<scopeParam>::text[])
//	      OR <alias>.id = ANY(<grantParam>::uuid[]) )
//
// typesParam binds the resolved retrieval allowlist (blocktype.Set
// .VisibleTypes) — fail-closed: an unregistered type name matches nothing
// (design/01 §3.5 invariant 1; Go callers reject an empty list loudly).
//
// scope is gated on context_blocks.scope (authoritative), never on
// context_dream_links.scope. All four arguments are code-owned constants at
// every call site — never user input — so embedding the fragment via Sprintf
// adds no injection surface.
//
// CRITICAL — the mandatory parentheses (design/07 §4 / §5.5): the archived /
// type conjuncts stand BEFORE the parenthesised group; the additive
// block-level OR lives strictly INSIDE ( scope=ANY OR id=ANY ). See
// visibility.Predicate for the full contract.
//
// block-level note (multi-tenant, T40a): visibility is a flat readScopes list
// PLUS a resolved per-tenant block-grant set (context_block_grants, 067). This
// function (plus ctx_rrf) is the designated single switch point for the
// row-level read grant. Scope-level tenant resolution stays the readScopes axis.
func VisibilityPredicate(alias, typesParam, scopeParam, grantParam string) string {
	return visibility.Predicate(alias, typesParam, scopeParam, grantParam)
}
