//go:build integration

// W05.4 (design/05 §4.5): the store half of the budget-report gate. EgoGraph
// keeps setting the one Truncated bool exactly as before AND now records the
// CAUSE of each cut in a BudgetReport. These tests pin the mapping of the three
// truncation sources the SQL path detects today:
//
//	takeHopMerged / pre-hop p.Limit check → TravNodeLimitReached  (layer LIMITS)
//	Q2 overflow, Q2s overflow, arbitrateEdgeBudget → TravEdgeLimitReached (LIMITS)
//
// The layer assignment is the point: p.Limit and p.EdgeLimit are what the CLIENT
// asked for (API contract, ceilings enforced in the handler), so their
// exhaustion is LIMITS — never TravVisitedCapped, which would claim a server
// OOM guard bit (and no such guard is enforced before W05.5+).

package store_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/GottZ/ctx/internal/graphcache"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// gbHub is the fixture hub for the budget-report tests (own id space so the
// test can run beside the other store_test integration files).
const gbHub = "019e0007-0000-7000-9000-0000000000c1"

// gbHasClass reports whether a class is present in a report layer array.
func gbHasClass(list []graphcache.TravClass, c graphcache.TravClass) bool {
	for _, x := range list {
		if x == c {
			return true
		}
	}
	return false
}

// TestEgoGraph_BudgetReportNodeLimit: the node budget cut is declared as
// node_limit_reached in the LIMITS layer, and Truncated keeps its old value.
func TestEgoGraph_BudgetReportNodeLimit(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	gInsertBlock(t, pool, gbHub, "shared", "graphbudget")
	nb := make([]string, 6)
	for i := range nb {
		nb[i] = fmt.Sprintf("019e0007-0000-7000-9000-0000000003%02x", i+1)
		gInsertBlock(t, pool, nb[i], "shared", "graphbudget")
		gInsertLink(t, pool, gbHub, nb[i], "topical", 0.90-float64(i)*0.01, 0.95)
	}

	p := gEgoParams(gbHub)
	p.Hops = 1
	p.Limit = 3 // focus + 2 neighbours, the rest is cut
	res, err := store.EgoGraph(ctx, pool, p, gScopesA, nil, gVisibleTypes)
	if err != nil {
		t.Fatalf("ego: %v", err)
	}

	if !res.Truncated {
		t.Fatalf("setup: Truncated = false at limit=3 over 6 neighbours")
	}
	if res.Budget == nil {
		t.Fatal("EgoGraph returned no budget report")
	}
	if !gbHasClass(res.Budget.Limits, graphcache.TravNodeLimitReached) {
		t.Errorf("Limits = %v, want node_limit_reached", res.Budget.Limits)
	}
	if gbHasClass(res.Budget.Budgets, graphcache.TravVisitedCapped) {
		t.Errorf("Budgets = %v — p.Limit is an API contract (LIMITS), not a server visited guard", res.Budget.Budgets)
	}
	if gbHasClass(res.Budget.Limits, graphcache.TravEdgeLimitReached) {
		t.Errorf("Limits = %v — a pure node cut must not claim an edge cut", res.Budget.Limits)
	}
	if res.Budget.Source != graphcache.SourceSQL {
		t.Errorf("Source = %q, want %q (the ego cache arm is W05.5)", res.Budget.Source, graphcache.SourceSQL)
	}
	if res.Budget.CacheAge != 0 {
		t.Errorf("CacheAge = %v, want 0 on the SQL arm", res.Budget.CacheAge)
	}
}

// TestEgoGraph_BudgetReportEdgeLimit: an induced-edge cut is declared as
// edge_limit_reached — and does NOT claim a node cut.
func TestEgoGraph_BudgetReportEdgeLimit(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	gInsertBlock(t, pool, gbHub, "shared", "graphbudget")
	nb := make([]string, 4)
	for i := range nb {
		nb[i] = fmt.Sprintf("019e0007-0000-7000-9000-0000000004%02x", i+1)
		gInsertBlock(t, pool, nb[i], "shared", "graphbudget")
		gInsertLink(t, pool, gbHub, nb[i], "topical", 0.90, 0.95)
	}

	p := gEgoParams(gbHub)
	p.Hops = 1
	p.EdgeLimit = 2 // 4 induced edges exist, only 2 fit
	res, err := store.EgoGraph(ctx, pool, p, gScopesA, nil, gVisibleTypes)
	if err != nil {
		t.Fatalf("ego: %v", err)
	}

	if !res.Truncated {
		t.Fatalf("setup: Truncated = false at edge_limit=2 over 4 edges")
	}
	if res.Budget == nil {
		t.Fatal("EgoGraph returned no budget report")
	}
	if !gbHasClass(res.Budget.Limits, graphcache.TravEdgeLimitReached) {
		t.Errorf("Limits = %v, want edge_limit_reached", res.Budget.Limits)
	}
	if gbHasClass(res.Budget.Limits, graphcache.TravNodeLimitReached) {
		t.Errorf("Limits = %v — the node budget was not exhausted", res.Budget.Limits)
	}
}

// TestEgoGraph_BudgetReportQuiet: an untruncated traversal reports nothing.
// Guards against an always-full report (which would be as uninformative as the
// single bool it refines).
func TestEgoGraph_BudgetReportQuiet(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	gInsertBlock(t, pool, gbHub, "shared", "graphbudget")
	n1 := "019e0007-0000-7000-9000-000000000501"
	gInsertBlock(t, pool, n1, "shared", "graphbudget")
	gInsertLink(t, pool, gbHub, n1, "topical", 0.9, 0.95)

	res, err := store.EgoGraph(ctx, pool, gEgoParams(gbHub), gScopesA, nil, gVisibleTypes)
	if err != nil {
		t.Fatalf("ego: %v", err)
	}
	if res.Truncated {
		t.Fatalf("setup: unexpected truncation")
	}
	if res.Budget == nil {
		t.Fatal("EgoGraph returned no budget report")
	}
	if res.Budget.Tripped() {
		t.Errorf("quiet traversal reports trips: %v", res.Budget.Counts)
	}
}

// TestEgoGraph_BudgetOracleBarrierOnWire pins the §4.5 oracle barrier at the
// store→wire seam: whatever the traversal records internally, the wire
// projection never carries a pre-recheck class. Simulated by adding the class
// to a live report (no code path produces it before W05.5+).
func TestEgoGraph_BudgetOracleBarrierOnWire(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	gInsertBlock(t, pool, gbHub, "shared", "graphbudget")
	res, err := store.EgoGraph(ctx, pool, gEgoParams(gbHub), gScopesA, nil, gVisibleTypes)
	if err != nil {
		t.Fatalf("ego: %v", err)
	}
	res.Budget.Add(graphcache.TravCandidatesCapped)

	w := res.Budget.WireReport()
	for _, c := range append(append([]graphcache.TravClass{}, w.Limits...), w.Budgets...) {
		if c == graphcache.TravCandidatesCapped {
			t.Errorf("pre-recheck class reached the wire projection: %v / %v", w.Limits, w.Budgets)
		}
	}
	if _, ok := w.Counts[graphcache.TravCandidatesCapped]; ok {
		t.Errorf("pre-recheck class reached the wire counts: %v", w.Counts)
	}
	if res.Budget.Count(graphcache.TravCandidatesCapped) != 1 {
		t.Errorf("server-side telemetry lost the trip: %v", res.Budget.Counts)
	}
}
