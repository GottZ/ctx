// T43 (Achse 07-W6, design/07 §5.4 / ED-B8): the grant-fixed egress sensitivity
// floor. annotateSensitivities now hangs a grant-mediated block's floor on the
// GRANTEE identity (callerHomeScope/readScopes + granteeFloor) plus a config-
// independent GrantFloorDefault, instead of the owner's floor alone. These are
// pure-function gates (no DB) — they run under `go test -short` and pin the
// behaviour against the REAL new signature (G8c).
package handler

import (
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/rrf"
	"github.com/GottZ/ctx/internal/store"
)

// TestAnnotateSensitivities_GranteeFloor_HighFloor is G8(a): a grantee WITH a high
// own floor sees a grant-mediated (owner-scope, low-sensitivity) block raised to
// ITS floor — not left at the owner's lower floor. The owner block is `public`
// with NO owner-scope floor; the caller's readScopes do NOT contain the owner
// scope (so it is grant-mediated). granteeFloor=credentials must win.
//
// RED proof: drop the `granteeFloor` argument from the max-fold (the owner-only
// path) → the block stays `public` and the grantee's credentials floor is bypassed.
func TestAnnotateSensitivities_GranteeFloor_HighFloor(t *testing.T) {
	results := []rrf.SearchResult{{ID: "grant-b", Scope: "owner-scope"}}
	sensMap := map[string]store.BlockSensitivity{
		"grant-b": {Sensitivity: backends.SensPublic, Scope: "owner-scope"},
	}
	floor := config.ScopeFloor{} // owner scope has NO configured floor
	// Caller reads only its own scope; owner-scope is grant-mediated.
	annotateSensitivities(results, sensMap, floor, []string{"grantee-scope"}, backends.SensCredentials)

	if got := results[0].Sensitivity; got != backends.SensCredentials {
		t.Fatalf("grant-mediated block under high grantee floor = %q, want credentials (G8a: grantee floor must apply, not the owner's)", got)
	}
}

// TestAnnotateSensitivities_GrantFloorDefault_NoGranteeFloor is G8(b), THE
// fail-OPEN test: a grantee with NO configured floor (the normal case) still sees
// a grant-mediated `public` block raised to GrantFloorDefault (personal). Without
// the config-independent default, a naive max(owner, grantee) collapses to
// `public` and the block would be eligible for an external backend.
//
// RED proof: drop GrantFloorDefault from the max-fold → the block stays `public`.
func TestAnnotateSensitivities_GrantFloorDefault_NoGranteeFloor(t *testing.T) {
	results := []rrf.SearchResult{{ID: "grant-b", Scope: "owner-scope"}}
	sensMap := map[string]store.BlockSensitivity{
		"grant-b": {Sensitivity: backends.SensPublic, Scope: "owner-scope"},
	}
	floor := config.ScopeFloor{} // neither owner NOR grantee has a floor
	annotateSensitivities(results, sensMap, floor, []string{"grantee-scope"}, backends.SensPublic /* grantee floor unconfigured */)

	if got := results[0].Sensitivity; got != GrantFloorDefault {
		t.Fatalf("grant-mediated block with no grantee floor = %q, want %q (G8b fail-OPEN: GrantFloorDefault must apply)", got, GrantFloorDefault)
	}
	if GrantFloorDefault == backends.SensPublic {
		t.Fatalf("GrantFloorDefault is public — that is not a floor; design/07 §5.4 wants personal-or-higher")
	}
}

// TestAnnotateSensitivities_NonGrantUnchanged pins the pausability/byte-identical
// invariant: a result IN the caller's read scopes is NOT grant-mediated, so the
// grantee floor / default never touch it — it keeps today's owner-only floor. A
// `public` block in an owned scope with no floor stays `public`.
func TestAnnotateSensitivities_NonGrantUnchanged(t *testing.T) {
	results := []rrf.SearchResult{{ID: "own", Scope: "mine"}}
	sensMap := map[string]store.BlockSensitivity{
		"own": {Sensitivity: backends.SensPublic, Scope: "mine"},
	}
	// `mine` IS in readScopes ⇒ not grant-mediated ⇒ a high granteeFloor must be
	// ignored (else every same-scope block would over-block).
	annotateSensitivities(results, sensMap, config.ScopeFloor{}, []string{"mine"}, backends.SensCredentials)

	if got := results[0].Sensitivity; got != backends.SensPublic {
		t.Fatalf("owned-scope block = %q, want public unchanged (grant floor must not touch non-grant results)", got)
	}
}
