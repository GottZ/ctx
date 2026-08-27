package handler

import (
	"slices"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/blocktype"
)

// OPS-W1 — the Go half of the no-op claim behind migration 145.
//
// 145 adds `AND cb.type_name NOT IN ('checkpoint','system-meta')` to the four
// FTS CTEs of ctx_rrf / ctx_rrf_arms so the planner can prove the partial GIN
// index predicate. That conjunct removes no row that the surrounding conjuncts
// would have let through — provided the two names can never reach
// p_types_visible. p_types_visible has exactly two sources, and these two tests
// pin one each:
//
//	production  -> Set.VisibleTypes()                       (blocktype/set.go:167)
//	measurement -> measureVisibleTypesFor(visible, shadow)  (query_shadow.go:80)
//
// The SQL half — identical candidate sets before and after the wave — lives in
// internal/rrf/partial_fts_gin_opsw1_integration_test.go (TestOPSW1SetIdentity).

// opsw1DenyNames is the list migration 145 hard-codes into both index predicates
// and both function bodies. It is deliberately spelled out here rather than
// derived from shadowDenyTypes: a change to that map must break this test, not
// travel silently into the SQL's assumption.
//
// "Must break this test" is a claim, so it is checked the only way that means
// anything — by asserting the REASON for the refusal, not just its status code.
// Both halves of G5 answer 400, and the deny-list names would keep being refused
// by the flag half alone (`retrieval.shadow_measurable` is absent on them
// today). Asserting the status code therefore stayed green with the deny-list
// gutted — measured, review OPS-W1 finding #5, sonde C. The detail string is the
// only signal that separates the two halves.
var opsw1DenyNames = []string{"checkpoint", "system-meta"}

// opsw1DenyDetail is the server-side detail shadowGate logs for the DENY-LIST
// half of G5 (query_shadow.go:190). The flag half produces a different string;
// distinguishing them is the whole point of asserting it.
const opsw1DenyDetail = "is on the hard deny-list"

// TestOPSW1DenyTypesAreNeverVisible is the production half: neither name is in
// the builtin registry's visible-type allowlist, which is what feeds
// p_types_visible on every production query (handler/query.go:904).
func TestOPSW1DenyTypesAreNeverVisible(t *testing.T) {
	visible := blocktype.NewRegistry().Snapshot().VisibleTypes()
	if len(visible) == 0 {
		t.Fatal("the builtin registry has no visible types — the probe is vacuous")
	}
	for _, name := range opsw1DenyNames {
		if slices.Contains(visible, name) {
			t.Errorf("%q is in VisibleTypes() %v — migration 145's FTS conjunct would REMOVE rows the arms deliver today",
				name, visible)
		}
	}
	t.Logf("visible types: %v; neither %v is among them", visible, opsw1DenyNames)
}

// TestOPSW1ShadowDenyIsFailClosed is the measurement half. measureVisibleTypesFor
// widens the visible slice with the request's shadow_types, so the conjunct's
// no-op claim rests on shadowGate refusing both names BEFORE the widening
// happens — and refusing them regardless of the registry row, because the
// deny-list check runs before the shadow_measurable flag check
// (query_shadow.go:189-196).
func TestOPSW1ShadowDenyIsFailClosed(t *testing.T) {
	set := blocktype.NewRegistry().Snapshot()

	for _, name := range opsw1DenyNames {
		if _, ok := set.Resolve(name); !ok {
			t.Fatalf("%q is not a registered type — G4 would refuse it for the wrong reason and this probe would be vacuous", name)
		}
		req := &queryRequest{ShadowTypes: []string{name}}
		syn := false
		req.Synthesize = &syn
		status, _, detail := shadowGate(req, true /*admin*/, true /*armRanks*/, set)
		if status != 400 {
			t.Errorf("shadowGate admitted %q with status %d (%s) — it could then reach measureVisibleTypes and migration 145's conjunct would silently cut it out of both FTS arms",
				name, status, detail)
			continue
		}
		// The load-bearing assertion: refused BY THE DENY-LIST, not merely
		// refused. Without this line, removing the name from shadowDenyTypes
		// leaves the test green (the flag half answers 400 too) and the SQL's
		// no-op premise would rest on a registry field any operator can flip.
		if !strings.Contains(detail, opsw1DenyDetail) {
			t.Errorf("shadowGate refused %q for the wrong reason: detail = %q, want it to contain %q — "+
				"the hard deny-list is what migration 145's conjunct relies on; a refusal that only comes from "+
				"the shadow_measurable flag is one registry write away from disappearing",
				name, detail, opsw1DenyDetail)
			continue
		}
		t.Logf("shadowGate refuses %q by deny-list (400: %s)", name, detail)
	}

	// The widening itself: a name that PASSES the gate does land in the list, so
	// the refusal above is what keeps the deny-list names out — not an inability
	// of the seam to widen at all.
	widened := measureVisibleTypesFor(set.VisibleTypes(), []string{"catalog"})
	if !slices.Contains(widened, "catalog") {
		t.Fatalf("measureVisibleTypesFor did not widen with a shadow-measurable type — the probe above proves nothing about the seam")
	}
	for _, name := range opsw1DenyNames {
		if slices.Contains(widened, name) {
			t.Errorf("%q appeared in the widened list %v without ever being requested", name, widened)
		}
	}
	t.Logf("widened list with catalog: %v", widened)
}
