package graphcache_test

import (
	"math"
	"testing"

	"github.com/GottZ/ctx/internal/graphcache"
)

func TestConfToFixFloorEndpoints(t *testing.T) {
	if got := graphcache.ConfToFix(0); got != 0 {
		t.Errorf("ConfToFix(0) = %d, want 0", got)
	}
	if got := graphcache.ConfToFix(1); got != 65535 {
		t.Errorf("ConfToFix(1) = %d, want 65535", got)
	}
	if got := graphcache.ConfToFix(-0.5); got != 0 {
		t.Errorf("ConfToFix(-0.5) clamp = %d, want 0", got)
	}
	if got := graphcache.ConfToFix(1.5); got != 65535 {
		t.Errorf("ConfToFix(1.5) clamp = %d, want 65535", got)
	}
	// floor, not round: 0.75*65535 = 49151.25 → 49151.
	if got := graphcache.ConfToFix(0.75); got != 49151 {
		t.Errorf("ConfToFix(0.75) = %d, want 49151 (floor)", got)
	}
}

func TestThresholdToFixCeilEndpoints(t *testing.T) {
	if got := graphcache.ThresholdToFix(0); got != 0 {
		t.Errorf("ThresholdToFix(0) = %d, want 0", got)
	}
	if got := graphcache.ThresholdToFix(1); got != 65535 {
		t.Errorf("ThresholdToFix(1) = %d, want 65535", got)
	}
	// ceil: 0.75*65535 = 49151.25 → 49152.
	if got := graphcache.ThresholdToFix(0.75); got != 49152 {
		t.Errorf("ThresholdToFix(0.75) = %d, want 49152 (ceil)", got)
	}
}

// TestU16GateStricterNotLaxer is the §7 W05.1 borderline probe: a value in the
// SAME floor bucket as the threshold must FAIL the cache gate (floor(value) >=
// ceil(threshold) is false) — the cache is strictly safer than SQL, never laxer.
func TestU16GateStricterNotLaxer(t *testing.T) {
	const s = 0.75
	// Value just below the threshold, sharing its floor bucket.
	v := 0.7499999

	// Same floor bucket: both floor to 49151.
	if math.Floor(v*65535) != math.Floor(s*65535) {
		t.Fatalf("test setup: %v and %v are not in the same floor bucket", v, s)
	}

	edge := graphcache.ConfToFix(v)        // FLOOR
	thresh := graphcache.ThresholdToFix(s) // CEIL
	floorThresh := graphcache.ConfToFix(s) // the WRONG (floor-on-both-sides) rule

	// Correct rule: edge FAILS the gate (fail-closed — cache stricter than SQL).
	if edge >= thresh {
		t.Errorf("gate leaked: ConfToFix(%v)=%d >= ThresholdToFix(%v)=%d — cache laxer than SQL", v, edge, s, thresh)
	}
	// The floor-on-both-sides rule would have WRONGLY passed it — proving the
	// asymmetry is load-bearing.
	if edge < floorThresh {
		t.Errorf("expected floor-on-both bug to pass the edge (ConfToFix=%d, floorThresh=%d)", edge, floorThresh)
	}
}
