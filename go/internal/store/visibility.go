// Package store — canonical block-visibility predicate (F5-W1).
//
// This file defines THE single shared SQL fragment for "this block is visible
// to the caller". The graph endpoint references it from every hop leg, the
// degree counter and the focus hydrate; ctx_rrf carries the same triple on the
// retrieval side. When block_role graduates from a hard CHECK enum to
// data-driven policy (workflow-engine line), exactly this function and ctx_rrf
// change — no inline copies to chase.
package store

import "fmt"

// VisibilityPredicate returns the canonical visibility triple for a
// context_blocks alias and a scope bind-parameter placeholder:
//
//	NOT <alias>.is_archived
//	AND <alias>.block_role <> 'system-meta'
//	AND <alias>.scope = ANY(<scopeParam>::text[])
//
// scope is gated on context_blocks.scope (authoritative), never on
// context_dream_links.scope. Both arguments are code-owned constants at every
// call site — never user input — so embedding the fragment via Sprintf adds
// no injection surface.
//
// TODO(multi-tenant): visibility is a flat readScopes list from the API key
// today. A real tenant model resolves scopes per tenant — this function (plus
// ctx_rrf) is the designated single switch point for that change.
func VisibilityPredicate(alias, scopeParam string) string {
	return fmt.Sprintf(
		"NOT %[1]s.is_archived AND %[1]s.block_role <> 'system-meta' AND %[1]s.scope = ANY(%[2]s::text[])",
		alias, scopeParam,
	)
}
