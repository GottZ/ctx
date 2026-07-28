package handler

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestGuardGenNoMagicKey is probe S1(c): the grand total is reachable ONLY
// through globalRow(). An empty scope is the absence of a scope, not a lookup
// key — it must yield nil even when the map itself carries a "" entry, which is
// exactly the shape a "store the ROLLUP row under ''" implementation produces.
// Serving the global total to a scope lookup would hand a tenant every other
// tenant's flagged-block counts.
func TestGuardGenNoMagicKey(t *testing.T) {
	total := &guardReviewStatus{NeedsReview: 42, NearDuplicate: 7, PossibleDuplicate: 3}
	own := &guardReviewStatus{NeedsReview: 2}
	gen := &guardGen{
		global: total,
		byScope: map[string]*guardReviewStatus{
			// The magic-key variant: the ROLLUP row filed under the empty key.
			// forScope must not care — "" is not a scope.
			"":       total,
			"privat": own,
		},
		builtAt: time.Now(),
	}

	if got := gen.forScope(""); got != nil {
		t.Errorf("forScope(\"\") returned a section (%+v) — the empty scope must fail closed, never the global total", *got)
	}
	if got := gen.forScope("does-not-exist"); got != nil {
		t.Errorf("forScope on an unknown scope returned %+v, want nil (fail closed)", *got)
	}
	if got := gen.forScope("privat"); got != own {
		t.Errorf("forScope(\"privat\") = %v, want the scope's own row", got)
	}
	if got := gen.globalRow(); got != total {
		t.Errorf("globalRow() = %v, want the grand total", got)
	}

	// Same guarantee through the staleness-gated selectors the readers use.
	if got := guardSectionForScope(gen, "", time.Now(), time.Second); got != nil {
		t.Errorf("guardSectionForScope with an empty scope leaked %+v", *got)
	}
	if got := guardSectionGlobal(gen, time.Now(), time.Second); got != total {
		t.Errorf("guardSectionGlobal = %v, want the grand total", got)
	}
}

// TestGuardGenVisibleStaleness is probe S1(e): a generation that stops being
// rebuilt must become INVISIBLE, not silently pass for current.
//
// Two halves, both load-bearing:
//  1. a failed build never touches the live generation's SUCCESS stamp — so the
//     age below measures the real outage, not the last attempt;
//  2. past guardGenStaleFactor ticks every reader degrades to nil.
//
// The reference failure is the neighbouring per-tenant rollup (status_tenant.go),
// which serves an arbitrarily old generation as if it were fresh.
func TestGuardGenVisibleStaleness(t *testing.T) {
	tick := 5 * time.Second
	built := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	row := &guardReviewStatus{NeedsReview: 9, BuiltAt: &built}
	gen := &guardGen{
		global:  row,
		byScope: map[string]*guardReviewStatus{"privat": row},
		builtAt: built,
	}

	c := &StatusCollector{}
	c.guardGen.Store(gen)
	// Ten refreshes in a row that fail (the build returns nil).
	var attempts int
	c.guardGenBuild = func(context.Context, *pgxpool.Pool) *guardGen {
		attempts++
		return nil
	}
	for i := 0; i < 10; i++ {
		c.buildGuardGenInto(context.Background())
	}
	if attempts != 10 {
		t.Fatalf("build attempts = %d, want 10", attempts)
	}
	live := c.guardGen.Load()
	if live != gen {
		t.Fatalf("a failed build replaced the live generation (%v)", live)
	}
	if !live.builtAt.Equal(built) {
		t.Errorf("a failed build moved the SUCCESS stamp: %v, want %v", live.builtAt, built)
	}

	// Within the budget the section is still served...
	edge := built.Add(time.Duration(guardGenStaleFactor) * tick)
	if guardSectionGlobal(live, edge, tick) == nil {
		t.Errorf("section vanished AT the %dx tick budget (%v) — the boundary is inclusive", guardGenStaleFactor, edge)
	}
	if guardSectionForScope(live, "privat", edge, tick) == nil {
		t.Errorf("scope section vanished at the %dx tick budget", guardGenStaleFactor)
	}
	// ...and one nanosecond past it, it is gone on BOTH paths.
	past := edge.Add(time.Nanosecond)
	if got := guardSectionGlobal(live, past, tick); got != nil {
		t.Errorf("global section still served %+v after %v without a successful build — stale counts must disappear, not pass for current", *got, past.Sub(built))
	}
	if got := guardSectionForScope(live, "privat", past, tick); got != nil {
		t.Errorf("scope section still served %+v after %v without a successful build", *got, past.Sub(built))
	}
	// Ten ticks of outage: unmistakably gone.
	if guardSectionGlobal(live, built.Add(10*tick), tick) != nil {
		t.Error("global section survived a 10-tick refresh outage")
	}

	// A generation without a SUCCESS stamp is never fresh, whatever its age.
	unstamped := &guardGen{global: row, byScope: map[string]*guardReviewStatus{"privat": row}}
	if guardGenFresh(unstamped, built, tick) {
		t.Error("a generation with a zero builtAt counted as fresh")
	}
	if guardGenFresh(nil, built, tick) {
		t.Error("a nil generation counted as fresh")
	}
}

// TestGuardTickFallback pins the cadence fallback on cheapNow's value, so the
// generation and the cheap snapshot turn over together when tick_interval is
// unset.
func TestGuardTickFallback(t *testing.T) {
	if got := guardTick(0); got != 5*time.Second {
		t.Errorf("guardTick(0) = %v, want 5s (cheapNow's fallback)", got)
	}
	if got := guardTick(-1); got != 5*time.Second {
		t.Errorf("guardTick(-1) = %v, want 5s", got)
	}
	if got := guardTick(2 * time.Second); got != 2*time.Second {
		t.Errorf("guardTick(2s) = %v, want 2s", got)
	}
}

// TestGuardScopeVector pins the RC-1 wave S6 read-scope vector — the compare
// surface of the /guard live channel — against the four ways it could quietly
// lie to that channel.
//
// Mutation probes for this test (each one turns it red):
//   - seeding an unknown scope with &guardReviewStatus{} → unknown_scope_omitted
//     fails: a scope that does not exist would render as "queue clear".
//   - dropping the guardGenFresh gate → stale_generation_yields_nothing fails:
//     a stale vector is a diff signal that says "something changed" when nothing
//     did, and frozen counts presented as current (the S1(e) posture).
//   - reading g.byScope directly instead of g.forScope → empty_scope_never_a_key
//     fails: "" would resolve to whatever a build path files under it — on the
//     magic-key shape below, the grand total.
func TestGuardScopeVector(t *testing.T) {
	tick := time.Second
	built := time.Now()
	home := &guardReviewStatus{NeedsReview: 2}
	shared := &guardReviewStatus{NeedsReview: 1, NearDuplicate: 3}
	total := &guardReviewStatus{NeedsReview: 10}
	gen := &guardGen{
		global: total,
		byScope: map[string]*guardReviewStatus{
			// The magic-key shape again (S1(c)): "" must never be a lookup key.
			"":       total,
			"privat": home,
			"shared": shared,
			"work":   {},
		},
		builtAt: built,
	}

	t.Run("carries_one_row_per_read_scope", func(t *testing.T) {
		got := guardScopeVector(gen, []string{"privat", "shared", "work"}, built, tick)
		if len(got) != 3 {
			t.Fatalf("vector has %d rows, want 3: %v", len(got), got)
		}
		if got["privat"] != home || got["shared"] != shared {
			t.Errorf("vector rows are not the generation's own rows: %v", got)
		}
		// A read scope with nothing flagged is PRESENT with zeros — the client
		// must be able to tell "queue clear" from "no data" (B10).
		if row, ok := got["work"]; !ok || row.NeedsReview != 0 {
			t.Errorf("an empty read scope must render {0,0,0,null}, got %v (present=%v)", row, ok)
		}
	})

	t.Run("unknown_scope_omitted", func(t *testing.T) {
		got := guardScopeVector(gen, []string{"privat", "does-not-exist"}, built, tick)
		if _, ok := got["does-not-exist"]; ok {
			t.Errorf("a scope the generation does not know was seeded with a row: %v", got)
		}
		if len(got) != 1 {
			t.Errorf("vector = %v, want only the known scope", got)
		}
	})

	t.Run("empty_scope_never_a_key", func(t *testing.T) {
		got := guardScopeVector(gen, []string{""}, built, tick)
		if got != nil {
			t.Errorf("an empty scope resolved to %v — never a lookup key, least of all onto the grand total", got)
		}
	})

	t.Run("no_read_scopes_no_vector", func(t *testing.T) {
		if got := guardScopeVector(gen, nil, built, tick); got != nil {
			t.Errorf("a caller without read scopes got a vector: %v", got)
		}
	})

	t.Run("stale_generation_yields_nothing", func(t *testing.T) {
		// Same staleness budget the single-slot readers use: past
		// guardGenStaleFactor ticks the WHOLE vector disappears rather than
		// serving frozen counts as current (S1(e) carried into S6).
		past := built.Add(time.Duration(guardGenStaleFactor)*tick + time.Millisecond)
		if got := guardScopeVector(gen, []string{"privat", "shared"}, past, tick); got != nil {
			t.Errorf("a stale generation still served a vector: %v", got)
		}
		edge := built.Add(time.Duration(guardGenStaleFactor) * tick)
		if got := guardScopeVector(gen, []string{"privat"}, edge, tick); got == nil {
			t.Errorf("the vector vanished AT the %dx tick budget — the boundary is inclusive", guardGenStaleFactor)
		}
		if got := guardScopeVector(nil, []string{"privat"}, built, tick); got != nil {
			t.Errorf("a nil generation served a vector: %v", got)
		}
	})

	t.Run("duplicate_read_scopes_collapse", func(t *testing.T) {
		got := guardScopeVector(gen, []string{"privat", "privat", "shared"}, built, tick)
		if len(got) != 2 {
			t.Errorf("duplicate read scopes produced %d rows, want 2: %v", len(got), got)
		}
	})
}
