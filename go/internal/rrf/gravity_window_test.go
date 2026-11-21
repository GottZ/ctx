package rrf

import (
	"math"
	"testing"
	"time"
)

// Issue #463: a window target. Inside it every date is at distance 0 and
// earns what a date on a point target's day earns; outside it the distance is
// measured to the nearer edge, and Direction and Cutoff measure from there.

func TestComputeGravity_WindowRewardsItsWholeSpan(t *testing.T) {
	// TargetDate lies inside the window on purpose: with a window, distances
	// are measured from its edges, never from TargetDate.
	p := GravityParams{
		TargetDate: mustDate("2025-01-01"), WindowStart: mustDate("2024-03-01"), WindowEnd: mustDate("2026-11-21"),
		Cutoff: 60, Power: 1.5,
	}
	full := maxDateGravity(1.5)
	for _, d := range []string{"2024-03-01", "2024-03-15", "2025-06-10", "2026-11-21"} {
		if g := ComputeGravity([]time.Time{mustDate(d)}, p); math.Abs(g-full) > 1e-9 {
			t.Errorf("%s inside the window: gravity %.4f, want the full %.4f", d, g, full)
		}
	}
	// The end day is inside until its last instant.
	late := mustDate("2026-11-21").Add(23*time.Hour + 59*time.Minute)
	if g := ComputeGravity([]time.Time{late}, p); math.Abs(g-full) > 1e-9 {
		t.Errorf("end day 23:59: gravity %.4f, want the full %.4f", g, full)
	}
	// Outside: the distance to the nearer edge, the same as a point there.
	for _, c := range []struct{ date, edge string }{{"2024-02-20", "2024-03-01"}, {"2026-11-30", "2026-11-21"}} {
		got := ComputeGravity([]time.Time{mustDate(c.date)}, p)
		want := ComputeGravity([]time.Time{mustDate(c.date)}, GravityParams{TargetDate: mustDate(c.edge), Cutoff: 60, Power: 1.5})
		if got <= 0 || math.Abs(got-want) > 1e-12 {
			t.Errorf("%s outside: gravity %.6f, want %.6f (a point on the edge %s)", c.date, got, want, c.edge)
		}
	}
	// The cutoff counts from the edge, not from TargetDate.
	if g := ComputeGravity([]time.Time{mustDate("2023-12-01")}, p); g != 0 {
		t.Errorf("91 days before the start: gravity %v, want 0", g)
	}
}

func TestComputeGravity_WindowOpenTowardsThePast(t *testing.T) {
	p := GravityParams{TargetDate: mustDate("2024-03-01"), WindowEnd: mustDate("2024-03-01"), Cutoff: 60, Power: 1.5}
	full := maxDateGravity(1.5)
	for _, d := range []string{"2020-01-01", "2023-06-01", "2024-03-01"} {
		if g := ComputeGravity([]time.Time{mustDate(d)}, p); math.Abs(g-full) > 1e-9 {
			t.Errorf("%s before the end: gravity %.4f, want the full %.4f", d, g, full)
		}
	}
	if g := ComputeGravity([]time.Time{mustDate("2024-03-02")}, p); g <= 0 || g >= full {
		t.Errorf("a day after the end: gravity %.4f, want in (0, %.4f)", g, full)
	}
	if g := ComputeGravity([]time.Time{mustDate("2025-01-01")}, p); g != 0 {
		t.Errorf("months after the end: gravity %v, want 0", g)
	}
}

func TestComputeGravity_WindowDirectionCountsFromTheEdges(t *testing.T) {
	base := GravityParams{TargetDate: mustDate("2026-11-16"), WindowStart: mustDate("2026-11-16"), WindowEnd: mustDate("2026-11-17"), Cutoff: 60, Power: 1.5}
	past, future := base, base
	past.Direction, future.Direction = "past", "future"
	tue := []time.Time{mustDate("2026-11-17")}
	if ComputeGravity(tue, past) == 0 || ComputeGravity(tue, future) == 0 {
		t.Error("a date inside the window must count whatever the direction")
	}
	if g := ComputeGravity([]time.Time{mustDate("2026-11-19")}, past); g != 0 {
		t.Errorf("past: a date after the window counts %v", g)
	}
	if g := ComputeGravity([]time.Time{mustDate("2026-11-14")}, future); g != 0 {
		t.Errorf("future: a date before the window counts %v", g)
	}
}

// TestComputeGravity_NoWindowIsThePoint: without a window nothing changes —
// the distance is measured from TargetDate (the zero value of both fields).
func TestComputeGravity_NoWindowIsThePoint(t *testing.T) {
	p := GravityParams{TargetDate: mustDate("2024-03-01"), Cutoff: 14, Power: 1.5}
	if g := ComputeGravity([]time.Time{mustDate("2024-03-15")}, p); g <= 0 || g >= maxDateGravity(1.5)/10 {
		t.Errorf("14 days from a point: gravity %.6f, want a small positive value", g)
	}
}
