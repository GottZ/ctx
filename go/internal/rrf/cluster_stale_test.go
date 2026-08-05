// Wave C4 (Cluster-Topic-Map, design/03 §4.7/§5.5 + §7 "C4") — the staleness
// gate of the categorical stage, in its pure form.
//
// The premise, primary-source verified in §4.7: the scheduler's arm stamp
// (lastOverviewNs / LastArmRuns) is set BEFORE the rebuild and survives every
// skip, so it reports "the arm ran", never "the map is fresh". The only
// trustworthy source is graph_overview_meta.computed_at, and this gate consumes
// it through a narrow seam so no query is added per request.
package rrf

import (
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/graphcache"
)

// fakeFreshness is the seam under test. perScope models the real per-partition
// meta rows; a scope missing from the map is a partition that never built.
type fakeFreshness struct {
	perScope map[string]time.Time
	calls    int
}

func (f *fakeFreshness) ClusterMapComputedAt(readScopes []string) (time.Time, bool) {
	f.calls++
	var oldest time.Time
	for _, s := range readScopes {
		at, ok := f.perScope[s]
		if !ok || at.IsZero() {
			return time.Time{}, false
		}
		if oldest.IsZero() || at.Before(oldest) {
			oldest = at
		}
	}
	return oldest, !oldest.IsZero()
}

func c4Cfg() ClusterConfig {
	return ClusterConfig{
		Enabled: true, SeedCount: 10, TopClusters: 2,
		MinShare: 0.25, BoostWeight: 0.12, SizeDamping: true,
		MaxStaleness: 24 * time.Hour,
	}
}

// Gate (i): a map computed seven days ago switches the stage off and records the
// trip.
//
// ROT-PROBE: drop the MaxStaleness branch from clusterMapUsable ⇒ a week-old map
// is treated as usable and this test fails.
func TestClusterMapUsableRejectsStale(t *testing.T) {
	rep := graphcache.NewBudgetReport(graphcache.SourceSQL)
	fresh := &fakeFreshness{perScope: map[string]time.Time{"private": time.Now().Add(-7 * 24 * time.Hour)}}

	if clusterMapUsable(c4Cfg(), fresh, []string{"private"}, rep) {
		t.Fatal("a 7-day-old cluster map must switch the stage off")
	}
	if rep.Count(graphcache.TravClusterStale) != 1 {
		t.Errorf("cluster_stale trip = %d, want 1", rep.Count(graphcache.TravClusterStale))
	}
}

// Gate (ii): NO meta row is a no-op, never "infinitely fresh". A zero time must
// not read as "just built" — that is the failure mode a naive
// `time.Since(zero) > max` inversion produces on a system that never rebuilt.
//
// ROT-PROBE: treat the zero time as fresh (drop the `at.IsZero()` arm) ⇒ a
// system that never built a map boosts from an empty one.
func TestClusterMapUsableRejectsMissingRow(t *testing.T) {
	rep := graphcache.NewBudgetReport(graphcache.SourceSQL)
	fresh := &fakeFreshness{perScope: map[string]time.Time{}}

	if clusterMapUsable(c4Cfg(), fresh, []string{"private"}, rep) {
		t.Fatal("a missing meta row must switch the stage off, not read as infinitely fresh")
	}
	if rep.Count(graphcache.TravClusterStale) != 1 {
		t.Errorf("cluster_stale trip = %d, want 1", rep.Count(graphcache.TravClusterStale))
	}
}

// Gate (iii): an unwired seam is a no-op — and a LOUD one. A silently unwired
// seam that kills the feature is the quiet permanent outage §4.6 warns about, so
// the trip is recorded here too.
func TestClusterMapUsableRejectsUnwiredSeam(t *testing.T) {
	rep := graphcache.NewBudgetReport(graphcache.SourceSQL)
	if clusterMapUsable(c4Cfg(), nil, []string{"private"}, rep) {
		t.Fatal("an unwired freshness seam must switch the stage off")
	}
	if rep.Count(graphcache.TravClusterStale) != 1 {
		t.Errorf("cluster_stale trip = %d, want 1", rep.Count(graphcache.TravClusterStale))
	}
}

// Gate (v): MULTI-SCOPE AGGREGATION IS THE MINIMUM. Two scopes, one built a
// minute ago, one frozen for a week ⇒ no-op. max() — what the landkarte's
// DISPLAY path takes, correctly, because a human reads it — would hide the
// frozen partition behind the fresh one on a path with no reader.
//
// ROT-PROBE: switch the seam (or the scheduler implementation) to max ⇒ the
// stale partition disappears behind the fresh one and this test fails.
func TestClusterMapUsableAggregatesByMinimum(t *testing.T) {
	rep := graphcache.NewBudgetReport(graphcache.SourceSQL)
	fresh := &fakeFreshness{perScope: map[string]time.Time{
		"private": time.Now().Add(-1 * time.Minute),
		"work":    time.Now().Add(-7 * 24 * time.Hour),
	}}

	if clusterMapUsable(c4Cfg(), fresh, []string{"private", "work"}, rep) {
		t.Fatal("one frozen partition must disqualify the whole read set (min, not max)")
	}
	// The fresh scope on its own stays usable — the gate rejects the SET, not the
	// feature.
	rep2 := graphcache.NewBudgetReport(graphcache.SourceSQL)
	if !clusterMapUsable(c4Cfg(), fresh, []string{"private"}, rep2) {
		t.Fatal("a fresh single-scope read must stay enabled")
	}
	if rep2.Tripped() {
		t.Errorf("a fresh map must not trip anything: %v", rep2.Counts)
	}
}

// A fresh map passes and costs exactly one seam call — the point of the seam is
// that the gate adds NO query per request.
func TestClusterMapUsableAcceptsFresh(t *testing.T) {
	rep := graphcache.NewBudgetReport(graphcache.SourceSQL)
	fresh := &fakeFreshness{perScope: map[string]time.Time{"private": time.Now().Add(-time.Hour)}}

	if !clusterMapUsable(c4Cfg(), fresh, []string{"private"}, rep) {
		t.Fatal("an hour-old map inside a 24h budget must stay enabled")
	}
	if fresh.calls != 1 {
		t.Errorf("seam calls = %d, want exactly 1 per request", fresh.calls)
	}
	if rep.Tripped() {
		t.Errorf("no trip expected: %v", rep.Counts)
	}
}

// MaxStaleness 0 disables the AGE check only — the two structural branches
// (missing row, unwired seam) still gate. Turning the age budget off must not
// turn the fail-safe off.
func TestClusterMapUsableZeroBudgetKeepsStructuralGates(t *testing.T) {
	cfg := c4Cfg()
	cfg.MaxStaleness = 0
	ancient := &fakeFreshness{perScope: map[string]time.Time{"private": time.Now().Add(-365 * 24 * time.Hour)}}

	if !clusterMapUsable(cfg, ancient, []string{"private"}, nil) {
		t.Error("with the age budget disabled an old map is accepted — that is what 0 means")
	}
	if clusterMapUsable(cfg, nil, []string{"private"}, nil) {
		t.Error("an unwired seam must gate regardless of the age budget")
	}
	if clusterMapUsable(cfg, &fakeFreshness{perScope: map[string]time.Time{}}, []string{"private"}, nil) {
		t.Error("a missing row must gate regardless of the age budget")
	}
}
