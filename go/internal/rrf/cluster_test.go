// Wave C3 (Cluster-Topic-Map, design/03 §4.5 + §7 "C3") — the PURE half of the
// categorical stage. Everything here runs without a database, which is the whole
// point of the split: the four invariants of §4.5 are properties of the fusion,
// not of the read.
package rrf

import (
	"math"
	"testing"
)

// c3Cfg is the live default surface (design/03 §4.9): seed_count 10,
// top_clusters 2, min_share 0.25, boost_weight 0.12, size_damping on.
func c3Cfg() ClusterConfig {
	return ClusterConfig{
		Enabled: true, SeedCount: 10, TopClusters: 2,
		MinShare: 0.25, BoostWeight: 0.12, SizeDamping: true,
	}
}

// c3Results builds n results with strictly descending scores, ids r00…rNN.
func c3Results(n int) []SearchResult {
	out := make([]SearchResult, n)
	for i := range out {
		out[i] = SearchResult{
			ID:       c3ID(i),
			Scope:    "private",
			RRFScore: 1.0 - float64(i)*0.01,
		}
	}
	return out
}

func c3ID(i int) string {
	const digits = "0123456789"
	return "r" + string([]byte{digits[i/10], digits[i%10]})
}

// Gate (ii) + (iii): the stage never adds, removes or PUNISHES a result.
//
// ROT-PROBE (ii): append an injected block in fuseClusters ⇒ the length assert
// fails. ROT-PROBE (iii): multiply non-winners by 0.9 ⇒ the untouched-score
// assert fails on every unclustered result.
func TestFuseClustersNoNewResultsNoPunishment(t *testing.T) {
	in := c3Results(12)
	memberOf := map[string]string{
		c3ID(0): "cA", c3ID(1): "cA", c3ID(5): "cB",
		// r02..r04, r06..r11 deliberately have NO membership row: fresh blocks,
		// grant-only blocks, or blocks born after the last rebuild.
	}
	winners := map[string]float64{"cA": 0.6}

	out := fuseClusters(in, memberOf, winners, c3Cfg(), nil)

	if len(out) != len(in) {
		t.Fatalf("len(out) = %d, want %d — the stage must not introduce or drop results", len(out), len(in))
	}
	ids := make(map[string]int, len(out))
	for _, r := range out {
		ids[r.ID]++
	}
	for _, r := range in {
		if ids[r.ID] != 1 {
			t.Errorf("id %s appears %d times in the output, want exactly 1", r.ID, ids[r.ID])
		}
	}

	orig := make(map[string]float64, len(in))
	for _, r := range in {
		orig[r.ID] = r.RRFScore
	}
	for _, r := range out {
		switch memberOf[r.ID] {
		case "cA":
			if r.RRFScore <= orig[r.ID] {
				t.Errorf("winner member %s was not reinforced: %g -> %g", r.ID, orig[r.ID], r.RRFScore)
			}
			if r.ClusterBoost != 0.6 {
				t.Errorf("%s cluster_boost = %g, want 0.6 (provenance)", r.ID, r.ClusterBoost)
			}
		default:
			// BIT-exact, not approximately: "never punishment" also means "never
			// silently renormalised".
			if r.RRFScore != orig[r.ID] {
				t.Errorf("non-winner %s changed score: %v -> %v", r.ID, orig[r.ID], r.RRFScore)
			}
			if r.ClusterBoost != 0 {
				t.Errorf("non-winner %s carries provenance %g", r.ID, r.ClusterBoost)
			}
		}
	}
}

// Gate (iii)/pausability half: boost_weight 0 leaves the slice bit-identical in
// ids, order AND scores even with the stage fully armed and winners picked. This
// is the deterministic A/B the design asks for, in its pure form; the integration
// twin runs the same comparison through the DB read.
//
// ROT-PROBE: make the multiplier `1 + BoostWeight*share + epsilon` (or drop the
// stable sort in favour of an id tiebreak) ⇒ the comparison fails.
func TestFuseClustersZeroWeightIsIdentity(t *testing.T) {
	in := c3Results(8)
	// Two results deliberately share a score so the tie-handling is exercised.
	in[3].RRFScore = in[4].RRFScore
	memberOf := map[string]string{c3ID(4): "cA", c3ID(6): "cA"}
	cfg := c3Cfg()
	cfg.BoostWeight = 0

	out := fuseClusters(in, memberOf, map[string]float64{"cA": 0.9}, cfg, nil)

	if len(out) != len(in) {
		t.Fatalf("len drift: %d vs %d", len(out), len(in))
	}
	for i := range in {
		if out[i].ID != in[i].ID {
			t.Errorf("order drift at %d: %s, want %s (stable sort must keep ties in RRF order)", i, out[i].ID, in[i].ID)
		}
		if out[i].RRFScore != in[i].RRFScore {
			t.Errorf("score drift at %d: %v, want %v", i, out[i].RRFScore, in[i].RRFScore)
		}
	}
}

// Gate (iv): the closing re-sort is LOAD-BEARING. A boosted mid-list result must
// end up at position 0, and the downstream selectSeeds must see the same seed set
// it would see from an already-sorted slice — its loop breaks at the first result
// under the floor, so an unsorted output silently truncates the seed set.
//
// ROT-PROBE: drop the sort.SliceStable at the end of fuseClusters ⇒ out[0] is
// still the old top hit and the seed comparison fails.
func TestFuseClustersReSortsForDownstreamSeeds(t *testing.T) {
	in := c3Results(6)
	// r05 sits last; a 12 % boost on a 0.95 score does not reach 1.0, so give the
	// winner a share that clearly lifts it over the top hit.
	in[5].RRFScore = 0.98
	memberOf := map[string]string{c3ID(5): "cA"}
	cfg := c3Cfg()
	cfg.BoostWeight = 1.0

	out := fuseClusters(in, memberOf, map[string]float64{"cA": 0.5}, cfg, nil)

	if out[0].ID != c3ID(5) {
		t.Fatalf("out[0] = %s, want %s — the boosted result must lead", out[0].ID, c3ID(5))
	}
	for i := 1; i < len(out); i++ {
		if out[i-1].RRFScore < out[i].RRFScore {
			t.Fatalf("output is not sorted desc at %d: %v < %v", i, out[i-1].RRFScore, out[i].RRFScore)
		}
	}

	// Downstream contract: selectSeeds over the stage output equals selectSeeds
	// over the same set sorted by score — i.e. the stage handed the graph stage a
	// slice matching its documented precondition.
	gcfg := GraphConfig{SeedCount: 3, SeedScoreFloor: 0.5}
	got, _, _ := selectSeeds(out, []string{"private"}, gcfg, nil)
	want := []string{c3ID(5), c3ID(0), c3ID(1)}
	if len(got) != len(want) {
		t.Fatalf("seed set = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("seed set = %v, want %v", got, want)
		}
	}
}

// Gate (vi): RRFScoreOriginal is NOT overwritten. The gravity stages save the
// pre-boost value, the graph stage does not — this stage runs between them, so
// without a ruling `rrf_score_original` would mean different things depending on
// which flags are on. Ruling: follow graph.go, so it keeps meaning "before ANY
// post-RRF augmentation".
//
// ROT-PROBE: add the gravity.go pattern (`orig := out[i].RRFScore;
// out[i].RRFScoreOriginal = &orig`) ⇒ the pointer identity assert fails.
func TestFuseClustersLeavesRRFScoreOriginal(t *testing.T) {
	in := c3Results(3)
	pre := 0.42
	in[0].RRFScoreOriginal = &pre

	out := fuseClusters(in, map[string]string{c3ID(0): "cA"}, map[string]float64{"cA": 0.5}, c3Cfg(), nil)

	for _, r := range out {
		if r.ID != c3ID(0) {
			if r.RRFScoreOriginal != nil {
				t.Errorf("%s gained an rrf_score_original the stage never set", r.ID)
			}
			continue
		}
		if r.RRFScoreOriginal != &pre {
			t.Fatalf("rrf_score_original was replaced (%v), want the untouched pre-gravity pointer", r.RRFScoreOriginal)
		}
		if *r.RRFScoreOriginal != 0.42 {
			t.Errorf("rrf_score_original value = %v, want 0.42", *r.RRFScoreOriginal)
		}
	}
}

// Gate (v): SIZE DAMPING (decision UD-04-03, armed from day one). Two clusters
// with NEARLY equal raw vote shares — the mega cluster holds ~6 % of the visible
// map and votes on the even ranks, so its raw share is marginally the HIGHER one
// (0,3 % ahead). It wins undamped. Damped it loses, because "this cluster is
// relevant" becomes distinguishable from "this cluster is large" — the whole
// point at the §3.3 "few mega clusters" end of the scale range, which is where
// the 10M target sits.
//
// ROT-PROBE: cfg.SizeDamping=false — the second half of this test IS the red
// state, asserted rather than described: the mega cluster takes the single
// winner slot on a query where it is barely ahead on evidence and far ahead on
// size alone.
func TestClusterSharesSizeDamping(t *testing.T) {
	const mega, small = "c9-mega", "c0-small" // small sorts first: no tiebreak luck
	in := c3Results(10)
	memberOf := map[string]string{}
	for i := range in {
		if i%2 == 0 {
			memberOf[c3ID(i)] = mega // even ranks carry the marginally larger weight
		} else {
			memberOf[c3ID(i)] = small
		}
	}
	sizes := map[string]int{mega: 60000, small: 1000}
	const total = int64(1000000) // mega 6 %, small 0,1 %

	cfg := c3Cfg()
	cfg.TopClusters = 1

	cfg.SizeDamping = false
	raw := clusterShares(in, memberOf, sizes, total, cfg)
	if raw[mega] <= raw[small] {
		t.Fatalf("fixture broken: undamped mega %g must lead small %g", raw[mega], raw[small])
	}
	if w := pickWinners(raw, cfg); len(w) != 1 || w[mega] == 0 {
		t.Fatalf("undamped winner = %v, want %s — without damping the large cluster takes the slot", w, mega)
	}

	cfg.SizeDamping = true
	damped := clusterShares(in, memberOf, sizes, total, cfg)
	if damped[mega] >= damped[small] {
		t.Fatalf("damped shares: mega %g, small %g — size must not decide a near-tie", damped[mega], damped[small])
	}
	if w := pickWinners(damped, cfg); len(w) != 1 || w[small] == 0 {
		t.Fatalf("damped winner = %v, want only %s", w, small)
	}
}

// The share denominator spans ALL seeds, not only the clustered ones: a query
// whose top hits are mostly unclustered must produce a WEAK share, not have one
// incidental membership normalised up to 1.0.
//
// ROT-PROBE: sum the denominator only over seeds that have a membership ⇒ the
// share becomes 1.0 and clears min_share on a single incidental hit.
func TestClusterSharesDenominatorSpansAllSeeds(t *testing.T) {
	in := c3Results(10)
	cfg := c3Cfg()
	cfg.SizeDamping = false

	shares := clusterShares(in, map[string]string{c3ID(9): "cA"}, nil, 0, cfg)
	if shares["cA"] >= 0.2 {
		t.Fatalf("share = %g for ONE last-ranked seed out of ten — want a weak share", shares["cA"])
	}
	if w := pickWinners(shares, cfg); len(w) != 0 {
		t.Fatalf("a single tail membership must not win: %v", w)
	}
}

// The bounded-effect invariant (§4.5 nr. 3): share <= 1 by construction, so the
// maximum factor is 1+BoostWeight — 12 % at the default, below the live
// Dream-graph boost and far below rerank authority.
func TestClusterSharesBoundedByOne(t *testing.T) {
	in := c3Results(10)
	memberOf := map[string]string{}
	for i := range in {
		memberOf[in[i].ID] = "cA" // every seed votes for the same cluster
	}
	cfg := c3Cfg()
	cfg.SizeDamping = false

	shares := clusterShares(in, memberOf, nil, 0, cfg)
	if shares["cA"] > 1.0+1e-12 {
		t.Fatalf("share = %g, must never exceed 1", shares["cA"])
	}
	out := fuseClusters(in, memberOf, pickWinners(shares, cfg), cfg, nil)
	if got, want := out[0].RRFScore, in[0].RRFScore*(1+cfg.BoostWeight); math.Abs(got-want) > 1e-12 {
		t.Fatalf("maximum boost = %g, want %g (= 1+boost_weight)", got, want)
	}
}
