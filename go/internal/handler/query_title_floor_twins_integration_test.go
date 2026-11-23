//go:build integration

// Issue #507, round 1 (P4): two candidates carry the title the question
// names. The floor places the one the pipeline ranks higher, and only that one.

package handler

import "testing"

func TestQueryTitleFloorTwins(t *testing.T) {
	e := titleFloorSetup(t)
	const q = "Build log batch 2026-08"
	fo := e.ask(t, q, 10, true).ArmRanks.FusionOrder
	p0, p1 := position(fo, e.twins[0]), position(fo, e.twins[1])
	if p0 == 0 || p1 == 0 || p0 <= 10 || p1 <= 10 {
		t.Fatalf("precondition: fusion positions of the two blocks %d and %d (want both inside the window, past 10)", p0, p1)
	}
	higher, lower := e.twins[0], e.twins[1]
	if p1 < p0 {
		higher, lower = lower, higher
	}
	ten := e.ask(t, q, 10, false).ids()
	if position(ten, higher) == 0 {
		t.Errorf("neither block of the shared title is among the 10 sources (fusion positions %d, %d)", p0, p1)
	}
	if p := position(ten, lower); p != 0 {
		t.Errorf("the lower-ranked block of the shared title was placed too (source %d)", p)
	}
}
