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

// TestComputeGravity_WindowEdgesCountWholeDays (issue #463): a window's edges
// are days, so a date outside it is as far from it as its day is — the day
// before the start and the day after the end day are both one day away,
// whatever the time of the date. Before, the distance after the window was
// measured from the end day's 00:00, so a time one hour before the start
// earned the gravity of a date half a day away and a time one hour after the
// end day that of a date a day and an hour away.
func TestComputeGravity_WindowEdgesCountWholeDays(t *testing.T) {
	start, end := mustDate("2024-03-01"), mustDate("2024-03-31")
	p := GravityParams{TargetDate: start, WindowStart: start, WindowEnd: end, Cutoff: 60, Power: 1.5}
	g := func(d time.Time) float64 { return ComputeGravity([]time.Time{d}, p) }
	hourBeforeStart, hourAfterEndDay := start.Add(-time.Hour), end.AddDate(0, 0, 1).Add(time.Hour)
	if a, b := g(hourBeforeStart), g(hourAfterEndDay); math.Abs(a-b) > 1e-12 {
		t.Errorf("an hour before the start %.4f, an hour after the end day %.4f: want the same", a, b)
	}
	// Every time of the day before the start counts as that day, every time of
	// the day after the end day as that day; dates at 00:00 count as they did.
	for k, day := range map[string]time.Time{"before": start.AddDate(0, 0, -1), "after": end.AddDate(0, 0, 1)} {
		want := g(day)
		if math.Abs(want-1.0) > 1e-12 {
			t.Errorf("%s at 00:00: gravity %.4f, want 1 (one day away)", k, want)
		}
		for _, h := range []time.Duration{time.Hour, 9*time.Hour + 30*time.Minute, 23*time.Hour + 59*time.Minute} {
			if got := g(day.Add(h)); math.Abs(got-want) > 1e-12 {
				t.Errorf("%s +%v: gravity %.4f, want %.4f (the same day)", k, h, got, want)
			}
		}
	}
	// Two days away on either side: the day two days before the start, the
	// day two days after the end day.
	if a, b := g(start.AddDate(0, 0, -1).Add(-time.Hour)), g(start.AddDate(0, 0, -2)); math.Abs(a-b) > 1e-12 {
		t.Errorf("23:00 two days before the start %.4f, 00:00 that day %.4f", a, b)
	}
	if a, b := g(end.AddDate(0, 0, 2).Add(22*time.Hour)), g(end.AddDate(0, 0, 2)); math.Abs(a-b) > 1e-12 {
		t.Errorf("22:00 two days after the end day %.4f, 00:00 that day %.4f", a, b)
	}
	// Open towards the past: the same rule at the end.
	open := GravityParams{TargetDate: end, WindowEnd: end, Cutoff: 60, Power: 1.5}
	if a, b := ComputeGravity([]time.Time{end.AddDate(0, 0, 1).Add(5 * time.Hour)}, open), ComputeGravity([]time.Time{end.AddDate(0, 0, 1)}, open); math.Abs(a-b) > 1e-12 {
		t.Errorf("open window: 05:00 the day after %.4f, 00:00 %.4f", a, b)
	}
}
