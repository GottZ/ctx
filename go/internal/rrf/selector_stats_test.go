// W02-4 gate G3 (design/02-strategy-selektor.md §7 "W02-4", §4.3b): the
// interpretation of the pg_stats catalog row — estimate formula, residual,
// ExactMax+1 floor, the TTL/staleness mechanics of the snapshot cache and
// every declared edge case, all DB-free (the catalog READ is injected).
//
// The values in the "real shape" cases are not invented: they are the
// catalog rows the W02-4 integration fixtures actually produced
// (mcv=[w024big w024mid] freqs=[0.8108108 0.1891892] reltuples=370, and the
// 150-scope corpus reporting n_distinct=-0.25 over 600 rows).
//
//	go test ./internal/rrf/ -run 'TestScopeStats|TestStatsCache' -count=1 -v
package rrf

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// staticReader returns a statsReader that always yields the same row.
func staticReader(row scopeStatsRow) (statsReader, *int) {
	calls := 0
	return func(_ context.Context) (scopeStatsRow, bool, error) {
		calls++
		return row, true, nil
	}, &calls
}

// midCorpus is the real Ist-shape row of the W02-4 integration fixture:
// two scopes, both in the MCV list, 370 live rows.
func midCorpus() scopeStatsRow {
	return scopeStatsRow{
		mcVals:    []string{"w024big", "w024mid"},
		mcFreqs:   []float32{0.8108108, 0.1891892},
		nDistinct: 2,
		reltuples: 370,
	}
}

// TestScopeStatsEstimate_MCV: a listed scope is frequency × reltuples, and a
// multi-scope request sums.
func TestScopeStatsEstimate_MCV(t *testing.T) {
	snap := parseScopeStats(midCorpus(), nil, time.Now())
	if !snap.valid {
		t.Fatal("snapshot invalid for a well-formed catalog row")
	}

	cases := []struct {
		scopes []string
		want   int
	}{
		{[]string{"w024mid"}, 70},  // 0.1891892 × 370
		{[]string{"w024big"}, 300}, // 0.8108108 × 370
		{[]string{"w024mid", "w024big"}, 370},
	}
	for _, tc := range cases {
		got, ok := snap.estimate(tc.scopes, 4) // floor 5, never binding here
		if !ok {
			t.Fatalf("%v: estimate unavailable", tc.scopes)
		}
		if got != tc.want {
			t.Errorf("estimate(%v) = %d, want %d", tc.scopes, got, tc.want)
		}
	}
}

// TestScopeStatsEstimate_Residual pins the residual formula with a hand-
// computable expectation: reltuples 1000, one MCV scope at 0.5, n_distinct 5
// → residual = 1000 × (1−0.5) / (5−1) = 125 per unlisted scope.
func TestScopeStatsEstimate_Residual(t *testing.T) {
	row := scopeStatsRow{
		mcVals:    []string{"dominant"},
		mcFreqs:   []float32{0.5},
		nDistinct: 5,
		reltuples: 1000,
	}
	snap := parseScopeStats(row, nil, time.Now())
	if !snap.valid {
		t.Fatal("snapshot invalid for a well-formed catalog row")
	}
	if snap.residual != 125 {
		t.Errorf("residual = %v, want 125", snap.residual)
	}
	if got, _ := snap.estimate([]string{"unlisted"}, 4); got != 125 {
		t.Errorf("unlisted scope estimate = %d, want 125", got)
	}
	if got, _ := snap.estimate([]string{"unlisted", "other"}, 4); got != 250 {
		t.Errorf("two unlisted scopes = %d, want 250", got)
	}
	if got, _ := snap.estimate([]string{"dominant", "unlisted"}, 4); got != 625 {
		t.Errorf("mixed estimate = %d, want 625 (500 + 125)", got)
	}
}

// TestScopeStatsEstimate_RatioNDistinct is the §4.3b ratio form: a NEGATIVE
// n_distinct is |n_distinct| × reltuples, NOT a rejection. The values are the
// ones the 150-scope integration corpus really produced (n_distinct = -0.25,
// reltuples = 600 → 150 distinct scopes).
func TestScopeStatsEstimate_RatioNDistinct(t *testing.T) {
	row := scopeStatsRow{
		mcVals:    []string{"listed"},
		mcFreqs:   []float32{0.1},
		nDistinct: -0.25,
		reltuples: 600,
	}
	snap := parseScopeStats(row, nil, time.Now())
	if !snap.valid {
		t.Fatal("ratio-form n_distinct was rejected — it is the normal multi-tenant shape, not an error")
	}
	// residual = 600 × (1−0.1) / (150 − 1) = 540/149 = 3.6241...
	// Tolerance: the catalog values arrive as float4, so the expectation is a
	// float32-rounded one, not an exact rational.
	const want = 540.0 / 149.0
	if diff := snap.residual - want; diff > 1e-5 || diff < -1e-5 {
		t.Errorf("residual = %v, want %v (normalised n_distinct = 0.25 × 600 = 150)", snap.residual, want)
	}
	// Without normalisation the denominator would be (−0.25 − 1) < 0 and the
	// snapshot would have been rejected — assert the normalised path really ran.
	if got, _ := snap.estimate([]string{"unlisted"}, 0); got != 4 {
		t.Errorf("unlisted estimate = %d, want 4 (round(3.62))", got)
	}
}

// TestScopeStatsEstimate_Floor is the ExactMax+1 floor: the probe has already
// PROVEN the scope set is larger than ExactMax, so a smaller sampled estimate
// is lifted, never trusted.
func TestScopeStatsEstimate_Floor(t *testing.T) {
	row := scopeStatsRow{
		mcVals:    []string{"tiny"},
		mcFreqs:   []float32{0.001},
		nDistinct: 10,
		reltuples: 1000,
	}
	snap := parseScopeStats(row, nil, time.Now())
	if !snap.valid {
		t.Fatal("snapshot invalid")
	}
	// Raw MCV estimate is 0.001 × 1000 = 1 — far below the proven size.
	if got, ok := snap.estimate([]string{"tiny"}, 4096); !ok || got != 4097 {
		t.Errorf("floored estimate = (%d, %v), want (4097, true)", got, ok)
	}
	// Residual below the floor is lifted the same way: 1000×0.999/9 = 111.
	if got, ok := snap.estimate([]string{"unlisted"}, 4096); !ok || got != 4097 {
		t.Errorf("floored residual estimate = (%d, %v), want (4097, true)", got, ok)
	}
	// Above the floor the estimate passes through untouched.
	if got, _ := snap.estimate([]string{"unlisted"}, 64); got != 111 {
		t.Errorf("unfloored residual estimate = %d, want 111", got)
	}
}

// TestScopeStatsEdgeCases walks the §4.3b rejection rules that kill the whole
// snapshot. Both land on "unusable", which the dispatch turns into
// stats_stale → plain ann.
func TestScopeStatsEdgeCases(t *testing.T) {
	cases := []struct {
		name string
		row  scopeStatsRow
	}{
		{
			// PG14+ writes -1 for "never analysed" (fresh DB before the first
			// autovacuum; also the post-pg_restore state).
			name: "reltuples = -1",
			row:  scopeStatsRow{mcVals: []string{"a"}, mcFreqs: []float32{0.5}, nDistinct: 10, reltuples: -1},
		},
		{
			name: "MCV arrays disagree in length",
			row:  scopeStatsRow{mcVals: []string{"a", "b"}, mcFreqs: []float32{0.5}, nDistinct: 10, reltuples: 100},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := parseScopeStats(tc.row, nil, time.Now())
			if snap.valid {
				t.Fatalf("row was accepted, want unusable → stats_stale")
			}
			if est, ok := snap.estimate([]string{"a"}, 4096); ok || est != 0 {
				t.Errorf("estimate on an invalid snapshot = (%d, %v), want (0, false)", est, ok)
			}
		})
	}

	// Residual-denominator cases (§4.3b rule 3): these do NOT kill the
	// snapshot, they make exactly the SCOPES OUTSIDE the MCV list
	// unanswerable — an MCV scope keeps its exact frequency estimate. See the
	// deviation note on parseScopeStats: at statistics target 1000 the state
	// "n_distinct == |MCV|" is what a SUCCESSFUL migration 117 produces, so a
	// snapshot-wide rejection would disable the stage it enables.
	denomCases := []struct {
		name string
		row  scopeStatsRow
	}{
		{
			// Normalised n_distinct (0.5 × 10 = 5) equals |MCV| → denominator 0.
			name: "normalised n_distinct == |MCV|",
			row: scopeStatsRow{
				mcVals:    []string{"a", "b", "c", "d", "e"},
				mcFreqs:   []float32{0.2, 0.2, 0.2, 0.2, 0.2},
				nDistinct: -0.5, reltuples: 10,
			},
		},
		{
			name: "n_distinct < |MCV| (self-contradictory catalog)",
			row:  scopeStatsRow{mcVals: []string{"a", "b"}, mcFreqs: []float32{0.5, 0.5}, nDistinct: 1, reltuples: 100},
		},
	}
	for _, tc := range denomCases {
		t.Run(tc.name, func(t *testing.T) {
			snap := parseScopeStats(tc.row, nil, time.Now())
			if snap.hasResidual {
				t.Fatal("a non-positive residual denominator produced a residual")
			}
			if est, ok := snap.estimate([]string{"not-in-mcv"}, 4096); ok || est != 0 {
				t.Errorf("unlisted scope = (%d, %v), want (0, false) — no invented residual", est, ok)
			}
			if est, ok := snap.estimate([]string{"a"}, 0); !ok || est == 0 {
				t.Errorf("MCV scope = (%d, %v), want its exact frequency estimate", est, ok)
			}
		})
	}

	// n_distinct = 0 ("unknown") with an empty MCV list: nothing is
	// answerable, which is the stats_stale outcome in practice.
	unknown := parseScopeStats(scopeStatsRow{nDistinct: 0, reltuples: 100}, nil, time.Now())
	if est, ok := unknown.estimate([]string{"a"}, 64); ok || est != 0 {
		t.Errorf("n_distinct = 0 with no MCV: estimate = (%d, %v), want (0, false)", est, ok)
	}

	// An empty MCV list with a usable n_distinct is NOT an edge case: every
	// scope falls into the residual (the shape of a large, uniform corpus).
	snap := parseScopeStats(scopeStatsRow{nDistinct: 10, reltuples: 1000}, nil, time.Now())
	if !snap.valid {
		t.Error("empty MCV list with usable n_distinct was rejected — it is the uniform-corpus shape")
	}
	if got, _ := snap.estimate([]string{"anything"}, 0); got != 100 {
		t.Errorf("residual-only estimate = %d, want 100 (1000/10)", got)
	}

	// reltuples = 0 (analysed, empty table) is usable; the floor then carries
	// the decision, which is legal: the probe proved the rows exist.
	empty := parseScopeStats(scopeStatsRow{nDistinct: 10, reltuples: 0}, nil, time.Now())
	if !empty.valid {
		t.Error("reltuples = 0 was rejected — an analysed empty table is a valid state")
	}
	if got, ok := empty.estimate([]string{"anything"}, 64); !ok || got != 65 {
		t.Errorf("empty-table estimate = (%d, %v), want (65, true) — the floor", got, ok)
	}
}

// TestScopeStatsWrongFrequencies is the "deliberately wrong snapshot" half of
// G3: a catalog row whose frequencies are nonsense must still produce a LEGAL
// decision — a bounded budget delta, never a wrong result set. The estimate is
// consumed only as a threshold input, and both error directions stay inside
// the mechanism (§4.3b): overestimate → plain ann, underestimate → grey with
// the GreyScanTuples cap, which is its own ceiling.
func TestScopeStatsWrongFrequencies(t *testing.T) {
	p := testPolicy()

	// Frequencies claiming a 300-block scope holds 99 % of a 10M table.
	huge := scopeStatsRow{mcVals: []string{"small"}, mcFreqs: []float32{0.99}, nDistinct: 100, reltuples: 10_000_000}
	// Frequencies claiming the dominant scope is empty.
	tiny := scopeStatsRow{mcVals: []string{"large"}, mcFreqs: []float32{0.0000001}, nDistinct: 100, reltuples: 10_000_000}

	for _, tc := range []struct {
		name       string
		row        scopeStatsRow
		scope      string
		wantMode   string
		wantReason string
	}{
		{"overestimate → plain ann", huge, "small", ModeANN, ReasonStatsLarge},
		{"underestimate → grey, capped by the budget", tiny, "large", ModeGrey, ReasonStatsGrey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cache statsCache
			read, _ := staticReader(tc.row)
			estimator := func(ctx context.Context, scopes []string, exactMax int, ttl time.Duration) (int, bool) {
				return cache.estimate(ctx, nil, read, scopes, exactMax, ttl)
			}
			probe, _, _ := countingProbe(p.ExactMax+1, nil)

			var buf bytes.Buffer
			restore := swapDefaultLogger(&buf)
			dec := decide(context.Background(), probe, estimator, []string{tc.scope}, nil, p)
			restore()

			if dec.Mode != tc.wantMode || dec.Reason != tc.wantReason {
				t.Errorf("decision = {%q, %q}, want {%q, %q}", dec.Mode, dec.Reason, tc.wantMode, tc.wantReason)
			}
			// The reason token IS the logged evidence for Achse 01 — a wrong
			// estimate must be attributable, not silent.
			if dec.Reason == "" {
				t.Error("decision carries no reason token — the misestimate would be invisible to the correlation")
			}
			// Whatever the estimate claimed, the SQL surface stays inside the
			// mechanism bounds: grey never exceeds the clamped budget.
			mode, scanTuples, exactCap := selectorSQLArgs(dec, p)
			if mode != ModeANN || exactCap != nil {
				t.Errorf("SQL args = (%q, %v, %v), want the ann surface", mode, scanTuples, exactCap)
			}
			if tc.wantMode == ModeGrey && scanTuples != p.GreyScanTuples {
				t.Errorf("grey budget = %v, want the clamped %d", scanTuples, p.GreyScanTuples)
			}
			if tc.wantMode == ModeANN && scanTuples != nil {
				t.Errorf("ann budget = %v, want nil", scanTuples)
			}
		})
	}
}

// TestStatsCacheTTL: the snapshot is re-read when the TTL elapsed and served
// from memory before that.
func TestStatsCacheTTL(t *testing.T) {
	var cache statsCache
	read, calls := staticReader(midCorpus())
	ctx := context.Background()

	if est, ok := cache.estimate(ctx, nil, read, []string{"w024mid"}, 0, time.Hour); !ok || est != 70 {
		t.Fatalf("first estimate = (%d, %v), want (70, true)", est, ok)
	}
	if *calls != 1 {
		t.Fatalf("catalog reads = %d, want 1", *calls)
	}
	for i := 0; i < 5; i++ {
		if _, ok := cache.estimate(ctx, nil, read, []string{"w024mid"}, 0, time.Hour); !ok {
			t.Fatal("cached estimate unavailable")
		}
	}
	if *calls != 1 {
		t.Errorf("catalog reads = %d after 6 estimates inside the TTL, want 1", *calls)
	}

	// Age the snapshot past the TTL → exactly one refresh.
	snap := cache.snap.Load()
	snap.fetchedAt = time.Now().Add(-2 * time.Hour)
	cache.snap.Store(snap)
	if _, ok := cache.estimate(ctx, nil, read, []string{"w024mid"}, 0, time.Hour); !ok {
		t.Fatal("estimate after expiry unavailable")
	}
	if *calls != 2 {
		t.Errorf("catalog reads = %d after expiry, want 2", *calls)
	}
}

// TestStatsCacheStalenessBound is the §4.3b hard bound: a snapshot older than
// 10 × TTL is unusable even though a refresh was attempted — the visible
// consequence of a persistently failing catalog read.
func TestStatsCacheStalenessBound(t *testing.T) {
	var cache statsCache
	ctx := context.Background()
	failing := func(_ context.Context) (scopeStatsRow, bool, error) {
		return scopeStatsRow{}, false, errors.New("connection reset")
	}

	// Seed a good snapshot, then let every refresh fail.
	good, _ := staticReader(midCorpus())
	if _, ok := cache.estimate(ctx, nil, good, []string{"w024mid"}, 0, time.Minute); !ok {
		t.Fatal("seed estimate unavailable")
	}

	// Inside the bound the previous snapshot keeps serving (a transient
	// catalog failure must not cost the strategy).
	snap := cache.snap.Load()
	snap.fetchedAt = time.Now().Add(-5 * time.Minute)
	cache.snap.Store(snap)
	var buf bytes.Buffer
	restore := swapDefaultLogger(&buf)
	est, ok := cache.estimate(ctx, nil, failing, []string{"w024mid"}, 0, time.Minute)
	restore()
	if !ok || est != 70 {
		t.Errorf("estimate at 5×TTL with a failing refresh = (%d, %v), want the previous snapshot (70, true)", est, ok)
	}
	if !strings.Contains(buf.String(), "catalog read failed") {
		t.Errorf("failing refresh was silent; log = %q", buf.String())
	}

	// Beyond 10×TTL it is stats_stale.
	snap = cache.snap.Load()
	snap.fetchedAt = time.Now().Add(-11 * time.Minute)
	cache.snap.Store(snap)
	buf.Reset()
	restore = swapDefaultLogger(&buf)
	est, ok = cache.estimate(ctx, nil, failing, []string{"w024mid"}, 0, time.Minute)
	restore()
	if ok {
		t.Errorf("estimate at 11×TTL = (%d, true), want unusable", est)
	}
	if !strings.Contains(buf.String(), "staleness bound") {
		t.Errorf("staleness degradation was silent; log = %q", buf.String())
	}
}

// TestStatsCacheEmptySnapshot: a table without a pg_stats row for scope is an
// EMPTY snapshot — unusable, but published, so the catalog is not re-read on
// every single search.
func TestStatsCacheEmptySnapshot(t *testing.T) {
	var cache statsCache
	calls := 0
	missing := func(_ context.Context) (scopeStatsRow, bool, error) {
		calls++
		return scopeStatsRow{}, false, nil
	}
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if est, ok := cache.estimate(ctx, nil, missing, []string{"a"}, 64, time.Hour); ok {
			t.Fatalf("estimate = (%d, true) without a stats row, want unusable", est)
		}
	}
	if calls != 1 {
		t.Errorf("catalog reads = %d, want 1 (the empty snapshot is cached, not re-read per search)", calls)
	}
}

// TestStatsCacheOwnerGuard: a snapshot never answers for a different pool.
// One process holds one pool in production; the guard is what keeps the
// process-wide cache honest when that is not true (tests, future pools).
func TestStatsCacheOwnerGuard(t *testing.T) {
	var cache statsCache
	ctx := context.Background()
	poolA, poolB := "pool-a", "pool-b"

	readA, callsA := staticReader(midCorpus())
	if est, _ := cache.estimate(ctx, poolA, readA, []string{"w024mid"}, 0, time.Hour); est != 70 {
		t.Fatalf("pool A estimate = %d, want 70", est)
	}

	other := scopeStatsRow{mcVals: []string{"w024mid"}, mcFreqs: []float32{0.5}, nDistinct: 4, reltuples: 1000}
	readB, _ := staticReader(other)
	if est, _ := cache.estimate(ctx, poolB, readB, []string{"w024mid"}, 0, time.Hour); est != 500 {
		t.Errorf("pool B estimate = %d, want 500 — pool A's snapshot leaked", est)
	}
	if *callsA != 1 {
		t.Errorf("pool A catalog reads = %d, want 1", *callsA)
	}
}

// TestStatsCacheZeroTTL: a non-positive StatsTTL falls back to the shipped
// default instead of making the stage permanently stale (a zero TTL would
// mean "every snapshot is older than 0 × 10").
func TestStatsCacheZeroTTL(t *testing.T) {
	var cache statsCache
	read, calls := staticReader(midCorpus())
	ctx := context.Background()
	if est, ok := cache.estimate(ctx, nil, read, []string{"w024mid"}, 0, 0); !ok || est != 70 {
		t.Errorf("estimate with TTL 0 = (%d, %v), want (70, true)", est, ok)
	}
	if _, ok := cache.estimate(ctx, nil, read, []string{"w024mid"}, 0, -time.Second); !ok {
		t.Error("estimate with a negative TTL unavailable")
	}
	if *calls != 1 {
		t.Errorf("catalog reads = %d, want 1 (the fallback TTL is the shipped 60s, not 0)", *calls)
	}
}
