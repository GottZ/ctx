// graph_cache_condition_test.go — table-driven coverage of the design/05 §4.3
// rebuild condition (graphCacheDue). The W05.2 wave built the condition but left
// it ungated; W05.3 gate (d) needs it pinned, because from here on REAL
// Migration-116 NOTIFYs drive the clock this condition reads.
//
// Source: https://github.com/GottZ/ctx
package events

import (
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
)

// TestGraphCacheDueRebuildCondition pins the §4.3 condition:
//
//	signalDue = pending && (quiet ≥ DebounceWindow || pendingAge ≥ MaxPendingAge)
//	                    && sinceLastBuildStart ≥ MinRebuildInterval
//	hardDue   = sinceLastBuildStart ≥ RebuildInterval
//
// The load-bearing row is DenseWritesFireAtMaxPendingAge (gate d): with writes
// denser than DebounceWindow the quiet clock NEVER matures, so without
// MaxPendingAge the rebuild would starve for the whole hard interval.
func TestGraphCacheDueRebuildCondition(t *testing.T) {
	cfg := config.GraphCacheConfig{
		Enabled:            true,
		RebuildInterval:    6 * time.Hour,
		DebounceWindow:     60 * time.Second,
		MinRebuildInterval: 5 * time.Minute,
		MaxPendingAge:      10 * time.Minute,
		MaxStaleness:       15 * time.Minute,
		FailedThreshold:    3,
	}

	// t0 is the anchor; every case builds its clock relative to it.
	t0 := time.Now()

	cases := []struct {
		name string
		// dirtyAt are the MarkDirty stamps (oldest first); empty = clean.
		dirtyAt   []time.Duration // offsets from t0
		buildAt   time.Duration   // BeginBuild offset from t0
		now       time.Duration   // evaluation offset from t0
		want      bool
		rationale string
	}{
		{
			name: "CleanIdleNeverDue",
			// No signal at all, well inside the hard interval.
			buildAt: 0, now: 30 * time.Minute, want: false,
			rationale: "an idle cache does not age (§4.3 idle regime)",
		},
		{
			name:    "QuietDebounceMaturesFires",
			dirtyAt: []time.Duration{6 * time.Minute},
			buildAt: 0, now: 8 * time.Minute, want: true,
			rationale: "quiet 2min ≥ Debounce 60s, pendingAge 2min, sinceStart 8min ≥ MinRebuild 5min",
		},
		{
			name:    "DebounceNotYetQuietHolds",
			dirtyAt: []time.Duration{6 * time.Minute},
			buildAt: 0, now: 6*time.Minute + 30*time.Second, want: false,
			rationale: "quiet 30s < Debounce 60s and pendingAge 30s < MaxPendingAge",
		},
		{
			name:    "MinRebuildIntervalSuppresses",
			dirtyAt: []time.Duration{0},
			buildAt: 0, now: 2 * time.Minute, want: false,
			rationale: "quiet 2min ≥ Debounce but sinceStart 2min < MinRebuild 5min — frequency cap",
		},
		{
			name: "DenseWritesFireAtMaxPendingAge",
			// A write every 30s (denser than the 60s debounce) for 11 minutes:
			// quiet is always 30s at evaluation, pendingAge is 11 min.
			dirtyAt: denseStamps(0, 30*time.Second, 22),
			buildAt: 0, now: 11 * time.Minute, want: true,
			rationale: "gate (d): quiet 30s never matures, MaxPendingAge 10min breaks the starvation",
		},
		{
			name:    "DenseWritesHoldBeforeMaxPendingAge",
			dirtyAt: denseStamps(0, 30*time.Second, 12),
			buildAt: 0, now: 6 * time.Minute, want: false,
			rationale: "same dense regime, pendingAge 6min < MaxPendingAge 10min",
		},
		{
			name: "HardIntervalFiresWithoutSignal",
			// No dirty signal at all — only the unconditional hard interval.
			buildAt: 0, now: 6*time.Hour + time.Minute, want: true,
			rationale: "hardDue covers missed NOTIFYs and the signal-free past",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewScheduler(nil, config.NewStore(&config.Config{GraphCache: cfg}), backends.NewPool(nil, nil), StartupConfig{})
			s.graphCache.BeginBuild(t0.Add(tc.buildAt))
			for _, d := range tc.dirtyAt {
				s.graphCache.MarkDirty(t0.Add(d))
			}
			if got := s.graphCacheDue(t0.Add(tc.now), cfg); got != tc.want {
				pending, quiet, age := s.graphCache.Dirty(t0.Add(tc.now))
				t.Fatalf("graphCacheDue = %v, want %v (%s); pending=%v quiet=%v pendingAge=%v sinceStart=%v",
					got, tc.want, tc.rationale, pending, quiet, age, tc.now-tc.buildAt)
			}
		})
	}
}

// denseStamps returns n offsets starting at start, step apart.
func denseStamps(start, step time.Duration, n int) []time.Duration {
	out := make([]time.Duration, n)
	for i := range out {
		out[i] = start + time.Duration(i)*step
	}
	return out
}
