package graphcache

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestTravClassTokens pins the stable snake_case wire/log tokens. They are
// correlation input downstream (Achse 01) — a rename is a wire change, and this
// table is the place it has to be argued.
func TestTravClassTokens(t *testing.T) {
	want := map[TravClass]string{
		TravOK:               "ok",
		TravNodeLimitReached: "node_limit_reached",
		TravEdgeLimitReached: "edge_limit_reached",
		TravDepthCapped:      "depth_capped",
		TravFrontierCapped:   "frontier_capped",
		TravVisitedCapped:    "visited_capped",
		TravCandidatesCapped: "candidates_capped",
		TravInjectCapped:     "inject_capped",
		TravSeedFloorCapped:  "seed_floor_capped",
		TravTimeCapped:       "time_capped",
		TravCacheStale:       "cache_stale",
		TravRecheckError:     "recheck_error",
	}
	for c, tok := range want {
		if got := c.String(); got != tok {
			t.Errorf("TravClass(%d).String() = %q, want %q", int(c), got, tok)
		}
		b, err := c.MarshalText()
		if err != nil {
			t.Fatalf("MarshalText(%v): %v", c, err)
		}
		if string(b) != tok {
			t.Errorf("MarshalText(%v) = %q, want %q", c, b, tok)
		}
	}
	if got := TravClass(99).String(); got != "unknown" {
		t.Errorf("unknown class renders %q, want \"unknown\"", got)
	}
}

// TestTravClassLayers pins the TWO-LAYER split that is the design leistung of
// §4.5: a client-contract exhaustion (p.Limit/p.EdgeLimit) is LIMITS, a server
// guard is BUDGETS. Collapsing them would give back exactly the undifferentiated
// bool this wave replaces.
func TestTravClassLayers(t *testing.T) {
	limits := []TravClass{TravNodeLimitReached, TravEdgeLimitReached}
	budgets := []TravClass{
		TravDepthCapped, TravFrontierCapped, TravVisitedCapped,
		TravCandidatesCapped, TravInjectCapped, TravSeedFloorCapped, TravTimeCapped,
	}
	operational := []TravClass{TravCacheStale, TravRecheckError}

	for _, c := range limits {
		if c.Layer() != LayerLimits {
			t.Errorf("%v is layer %v, want limits (API contract)", c, c.Layer())
		}
	}
	for _, c := range budgets {
		if c.Layer() != LayerBudgets {
			t.Errorf("%v is layer %v, want budgets (server protection)", c, c.Layer())
		}
	}
	for _, c := range operational {
		if c.Layer() != LayerOperational {
			t.Errorf("%v is layer %v, want operational", c, c.Layer())
		}
	}
	if TravOK.Layer() != LayerNone {
		t.Errorf("TravOK layer = %v, want none", TravOK.Layer())
	}
}

// TestBudgetReportAdd covers the bookkeeping: first trip appends to its layer
// array, repeats only raise the count, TravOK is never recorded, and a nil
// report absorbs Add without panicking (the pure-function call sites rely on it).
func TestBudgetReportAdd(t *testing.T) {
	var nilRep *BudgetReport
	nilRep.Add(TravNodeLimitReached) // must not panic
	if nilRep.Tripped() || nilRep.Count(TravNodeLimitReached) != 0 {
		t.Error("nil report must stay empty")
	}

	r := NewBudgetReport(SourceSQL)
	r.Add(TravOK)
	if r.Tripped() {
		t.Error("TravOK must not be recorded as a trip")
	}
	r.Add(TravEdgeLimitReached)
	r.Add(TravEdgeLimitReached)
	r.Add(TravNodeLimitReached)
	r.Add(TravInjectCapped)
	r.Add(TravCacheStale)

	if got := r.Count(TravEdgeLimitReached); got != 2 {
		t.Errorf("edge-limit count = %d, want 2", got)
	}
	if len(r.Limits) != 2 || r.Limits[0] != TravEdgeLimitReached || r.Limits[1] != TravNodeLimitReached {
		t.Errorf("Limits = %v, want [edge_limit_reached node_limit_reached] in trip order", r.Limits)
	}
	if len(r.Budgets) != 1 || r.Budgets[0] != TravInjectCapped {
		t.Errorf("Budgets = %v, want [inject_capped]", r.Budgets)
	}
	// Operational classes are counted but carry no layer array (§4.5 has two).
	if r.Count(TravCacheStale) != 1 {
		t.Error("cache_stale must be counted")
	}
	for _, c := range append(append([]TravClass{}, r.Limits...), r.Budgets...) {
		if c == TravCacheStale {
			t.Error("operational class leaked into a layer array")
		}
	}
}

// TestWireReportOracleBarrier is the W05.4 ORACLE PIN (§4.5, normative): a
// pre-recheck class must never reach the wire — not in a layer array, not in
// the counts — while the server-side report keeps it in full.
//
// Why it matters: candidates_capped fires on the RAW candidate set BEFORE the
// visibility recheck. On a shared block, its presence in a response would prove
// that >= MaxCandidates raw edges exist, including foreign PRIVATE ones — the
// existence/quantity oracle store/graph.go:823-825 excludes.
//
// RED PROBE (recorded 2026-07-25): with the PreRecheck filter removed from
// WireReport/filterWireClasses, this test fails with
//
//	wire budgets carry candidates_capped: [candidates_capped inject_capped]
//	wire counts carry candidates_capped: map[node_limit_reached:1 candidates_capped:3 inject_capped:1]
//	MarshalJSON leaked "candidates_capped": {"limits":["node_limit_reached"],
//	  "budgets":["candidates_capped","inject_capped"],"counts":{"candidates_capped":3,…}}
func TestWireReportOracleBarrier(t *testing.T) {
	r := NewBudgetReport(SourceSQL)
	r.Add(TravCandidatesCapped)
	r.Add(TravCandidatesCapped)
	r.Add(TravCandidatesCapped)
	r.Add(TravInjectCapped)
	r.Add(TravNodeLimitReached)

	// Server-side telemetry keeps the full picture.
	if r.Count(TravCandidatesCapped) != 3 {
		t.Fatalf("telemetry report lost the candidate trips: %v", r.Counts)
	}
	if len(r.Budgets) != 2 || r.Budgets[0] != TravCandidatesCapped {
		t.Fatalf("telemetry Budgets = %v, want candidates_capped first", r.Budgets)
	}

	w := r.WireReport()
	for _, c := range w.Budgets {
		if c == TravCandidatesCapped {
			t.Errorf("wire budgets carry candidates_capped: %v", w.Budgets)
		}
	}
	for _, c := range w.Limits {
		if c == TravCandidatesCapped {
			t.Errorf("wire limits carry candidates_capped: %v", w.Limits)
		}
	}
	if _, ok := w.Counts[TravCandidatesCapped]; ok {
		t.Errorf("wire counts carry candidates_capped: %v", w.Counts)
	}
	// The non-oracle trips survive — the barrier filters, it does not blank.
	if len(w.Budgets) != 1 || w.Budgets[0] != TravInjectCapped {
		t.Errorf("wire budgets = %v, want [inject_capped]", w.Budgets)
	}
	if len(w.Limits) != 1 || w.Limits[0] != TravNodeLimitReached {
		t.Errorf("wire limits = %v, want [node_limit_reached]", w.Limits)
	}

	// Second, structural half: even a DIRECT serialisation of the full report
	// goes through the wire projection.
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	if strings.Contains(string(b), "candidates_capped") {
		t.Errorf("MarshalJSON leaked %q: %s", "candidates_capped", b)
	}
	if !strings.Contains(string(b), "node_limit_reached") {
		t.Errorf("MarshalJSON dropped a wire-legal class: %s", b)
	}
}

// TestWireReportShape pins the JSON shape consumed by the ego envelope: arrays
// and counts are never null (GA8 additive-array discipline), source and
// cache_age_ms are always present.
func TestWireReportShape(t *testing.T) {
	r := NewBudgetReport(SourceCache)
	r.CacheAge = 1500 * time.Millisecond

	b, err := json.Marshal(r.WireReport())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	want := `{"limits":[],"budgets":[],"counts":{},"source":"cache","cache_age_ms":1500}`
	if got != want {
		t.Errorf("wire shape:\n got %s\nwant %s", got, want)
	}
}

// TestLogValueKeepsPreRecheck: logs are server telemetry and MUST keep the
// candidate cap visible — the barrier is a WIRE barrier, not a blindfold.
func TestLogValueKeepsPreRecheck(t *testing.T) {
	r := NewBudgetReport(SourceSQL)
	r.Add(TravCandidatesCapped)

	found := false
	for _, a := range r.LogValue().Group() {
		if a.Key == "n_candidates_capped" {
			found = true
		}
	}
	if !found {
		t.Errorf("LogValue dropped candidates_capped: %v", r.LogValue())
	}
}

// TestBudgetZeroValue documents that W05.4 only DECLARES the server budgets:
// nothing sets them, so the zero value is the live configuration until W05.5+.
func TestBudgetZeroValue(t *testing.T) {
	var b Budget
	if b.MaxDepth != 0 || b.MaxFrontier != 0 || b.MaxVisited != 0 || b.MaxCandidates != 0 || b.SoftDeadline != 0 {
		t.Errorf("Budget zero value must stay inert until the cache arms enforce it: %+v", b)
	}
}
