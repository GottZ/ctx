// Wave C8 (Cluster-Topic-Map, design/03 §4.6 M2) — the PURE half: the min-max
// normalisation of the centroid window and the two-arm fusion. No database.
package rrf

import (
	"math"
	"testing"
)

func cm(topic, cluster string, cos float64) centroidMatch {
	return centroidMatch{topicID: topic, clusterID: cluster, cos: cos}
}

// Min-max over the WINDOW is the discrimination: cosines against an averaged
// centroid live in a narrow band, so the ranking information is the spread
// INSIDE the top-K, not the absolute value. The weakest of the K is 0 by
// construction — the probe's own floor, not a tuned threshold.
//
// ROT-PROBE: normalise against an absolute scale (`norm := m.cos`) ⇒ every
// member of the window scores ~0.8 and the assertion on the weakest match fails.
func TestCentroidSharesMinMaxOverWindow(t *testing.T) {
	got := centroidShares([]centroidMatch{
		cm("t1", "cA", 0.82),
		cm("t2", "cB", 0.80),
		cm("t3", "cC", 0.78),
	})
	if math.Abs(got["cA"]-1.0) > 1e-12 {
		t.Errorf("best match share = %g, want 1", got["cA"])
	}
	if math.Abs(got["cB"]-0.5) > 1e-9 {
		t.Errorf("middle match share = %g, want 0.5", got["cB"])
	}
	if got["cC"] != 0 {
		t.Errorf("weakest match share = %g, want 0 (the window's own floor)", got["cC"])
	}
}

// A window with no spread has no ranking information — but it DID find
// something, and reporting 0 would discard the only signal the probe produced.
// The documented answer is 1.0 for all, including the single-match case.
func TestCentroidSharesNoSpreadIsFullShare(t *testing.T) {
	single := centroidShares([]centroidMatch{cm("t1", "cA", 0.61)})
	if math.Abs(single["cA"]-1.0) > 1e-12 {
		t.Errorf("single match share = %g, want 1", single["cA"])
	}
	flat := centroidShares([]centroidMatch{cm("t1", "cA", 0.7), cm("t2", "cB", 0.7)})
	for _, c := range []string{"cA", "cB"} {
		if math.Abs(flat[c]-1.0) > 1e-12 {
			t.Errorf("flat window share[%s] = %g, want 1", c, flat[c])
		}
	}
	if len(centroidShares(nil)) != 0 {
		t.Error("empty probe must produce no shares (cold start is a valid state)")
	}
}

// Two topics of ONE cluster — one per visible scope partition — collapse to the
// MAXIMUM, never to a sum or an average. The query matched the best-fitting
// partition; a distant sibling partition must not dilute that, and a sum would
// let a cluster win by being spread across scopes rather than by being relevant.
//
// ROT-PROBE: replace the max with `out[m.clusterID] += norm` ⇒ cA scores 1.0+0.0
// through the sum path and the ordering against a single-partition cluster of
// equal similarity silently flips.
func TestCentroidSharesTakesMaxPerCluster(t *testing.T) {
	got := centroidShares([]centroidMatch{
		cm("t-private", "cA", 0.90),
		cm("t-work", "cA", 0.70),
		cm("t-other", "cB", 0.80),
	})
	if len(got) != 2 {
		t.Fatalf("shares = %v, want exactly one entry per CLUSTER", got)
	}
	if math.Abs(got["cA"]-1.0) > 1e-12 {
		t.Errorf("share[cA] = %g, want 1 (the closer partition wins)", got["cA"])
	}
}

// The A/B control: weight 0 returns the seed map ITSELF, so "centroid arm on,
// weight 0" is bit-identical to C3 — no float arithmetic, hence no rounding that
// could move a result across a MinShare boundary.
func TestFuseSharesWeightZeroIsIdentity(t *testing.T) {
	seed := map[string]float64{"cA": 0.4, "cB": 0.3}
	got := fuseShares(seed, map[string]float64{"cC": 1.0}, 0)
	if len(got) != 2 || got["cA"] != 0.4 || got["cB"] != 0.3 {
		t.Fatalf("weight 0 changed the seed shares: %v", got)
	}
	// The SECOND identity arm is the cold start (gate i): an empty probe means
	// the arm did not run, NOT that everything is similarity 0. Scaling every
	// seed share by (1-w) against an empty table would push winners below
	// MinShare — arming the feature would weaken the signal it extends.
	//
	// ROT-PROBE: drop the `len(centroid) == 0` arm ⇒ 0.2/0.15 instead of 0.4/0.3.
	cold := fuseShares(seed, nil, 0.5)
	if len(cold) != 2 || cold["cA"] != 0.4 || cold["cB"] != 0.3 {
		t.Fatalf("an empty centroid probe changed the seed shares: %v", cold)
	}
}

// The UNION is the point of the arm: a cluster the seeds never voted for can win
// on centroid evidence alone. That is exactly the circularity C8 exists to
// break — a query whose RRF hits are poor would otherwise get a poor prior built
// from those same poor hits.
//
// ROT-PROBE: iterate only over `seed` (intersection semantics) ⇒ cC vanishes and
// the arm can never contribute anything RRF did not already find.
func TestFuseSharesUnionAdmitsCentroidOnlyCluster(t *testing.T) {
	got := fuseShares(map[string]float64{"cA": 0.6}, map[string]float64{"cC": 1.0}, 0.5)
	if math.Abs(got["cA"]-0.3) > 1e-12 {
		t.Errorf("share[cA] = %g, want 0.3 ((1-w)*0.6)", got["cA"])
	}
	if math.Abs(got["cC"]-0.5) > 1e-12 {
		t.Errorf("share[cC] = %g, want 0.5 (w*1.0) — a centroid-only cluster must be able to win", got["cC"])
	}
}

// An out-of-range weight is CLAMPED, not rejected: a mistyped knob must not turn
// a working query into an error, and both ends of the range are meaningful
// (0 = pure seeds, 1 = pure centroid).
func TestFuseSharesClampsWeight(t *testing.T) {
	high := fuseShares(map[string]float64{"cA": 0.6}, map[string]float64{"cB": 1.0}, 4.2)
	if high["cA"] != 0 {
		t.Errorf("weight > 1 must clamp to pure centroid, share[cA] = %g", high["cA"])
	}
	if math.Abs(high["cB"]-1.0) > 1e-12 {
		t.Errorf("share[cB] = %g, want 1", high["cB"])
	}
	low := fuseShares(map[string]float64{"cA": 0.6}, map[string]float64{"cB": 1.0}, -3)
	if low["cA"] != 0.6 || len(low) != 1 {
		t.Errorf("weight < 0 must clamp to pure seeds: %v", low)
	}
}

// The size read's parameter set is the UNION of both arms and it is SORTED: a
// candidate list whose order changes between two identical requests makes the
// plan, the log and every downstream comparison unreliable for no benefit.
func TestCandidateClustersUnionSorted(t *testing.T) {
	in := c3Results(3)
	memberOf := map[string]string{in[0].ID: "cB", in[1].ID: "cB", in[2].ID: "cA"}
	cfg := c3Cfg()

	got := candidateClusters(in, memberOf, map[string]float64{"cC": 1, "cA": 0.5}, cfg)
	want := []string{"cA", "cB", "cC"}
	if len(got) != len(want) {
		t.Fatalf("candidates = %v, want %v (deduplicated union)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("candidates = %v, want %v (sorted)", got, want)
		}
	}
}
