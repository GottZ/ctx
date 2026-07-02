// Package store — canonical block-visibility predicate (F5-W1).
//
// This file defines THE single shared SQL fragment for "this block is visible
// to the caller". The graph endpoint references it from every hop leg, the
// degree counter and the focus hydrate; ctx_rrf carries the same triple on the
// retrieval side. When type_name graduates from a hard CHECK enum to
// data-driven policy (workflow-engine line), exactly this function and ctx_rrf
// change — no inline copies to chase.
package store

import "fmt"

// VisibilityPredicate returns the canonical visibility triple for a
// context_blocks alias, a scope bind-parameter placeholder and a block-grant
// bind-parameter placeholder:
//
//	NOT <alias>.is_archived
//	AND <alias>.type_name <> 'system-meta'
//	AND ( <alias>.scope = ANY(<scopeParam>::text[])
//	      OR <alias>.id = ANY(<grantParam>::uuid[]) )
//
// scope is gated on context_blocks.scope (authoritative), never on
// context_dream_links.scope. All three arguments are code-owned constants at
// every call site — never user input — so embedding the fragment via Sprintf
// adds no injection surface.
//
// CRITICAL — the mandatory parentheses (design/07 §4 / §5.5): the archived /
// system-meta conjuncts stand BEFORE the parenthesised group; the additive
// block-level OR lives strictly INSIDE ( scope=ANY OR id=ANY ). SQL binds AND
// tighter than OR — without the parentheses a granted, archived (or system-meta)
// block would leak through the bare `OR id = ANY(...)`. The grant arm is
// PURELY additive: an empty grant set ('{}'::uuid[]) makes `id = ANY('{}')` a
// deterministic FALSE, so the predicate collapses byte-equivalently to the
// scope-only state (pausability invariant).
//
// block-level note (multi-tenant, T40a): visibility is a flat readScopes list
// PLUS a resolved per-tenant block-grant set (context_block_grants, 067). This
// function (plus ctx_rrf in T40b) is the designated single switch point for the
// row-level read grant. Scope-level tenant resolution stays the readScopes axis.
func VisibilityPredicate(alias, scopeParam, grantParam string) string {
	return fmt.Sprintf(
		"NOT %[1]s.is_archived AND %[1]s.type_name <> 'system-meta' AND ( %[1]s.scope = ANY(%[2]s::text[]) OR %[1]s.id = ANY(%[3]s::uuid[]) )",
		alias, scopeParam, grantParam,
	)
}
