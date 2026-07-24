// Unit coverage for the W01-2 pure functions: the ties-robust,
// n_eff-normalized recall arithmetic (design/01 §4.2.3), the nearest-rank
// percentile, and the config parsers. DB-free — `go test -short` runs these.
package recall_test

import (
	"fmt"
	"testing"

	"github.com/GottZ/ctx/internal/recall"
)

func rowsOf(dists ...float64) []recall.LegRow {
	out := make([]recall.LegRow, len(dists))
	for i, d := range dists {
		out[i] = recall.LegRow{ID: fmt.Sprintf("id-%d", i), Dist: d}
	}
	return out
}

func TestComputeRecallExactMatch(t *testing.T) {
	exact := rowsOf(0.1, 0.2, 0.3)
	ann := rowsOf(0.1, 0.2, 0.3)
	r, nEff := recall.ComputeRecall(exact, ann, 3, 0)
	if r != 1.0 || nEff != 3 {
		t.Errorf("recall=%v nEff=%d, want 1.0/3", r, nEff)
	}
}

func TestComputeRecallMiss(t *testing.T) {
	exact := rowsOf(0.1, 0.2, 0.3, 0.4)
	// ANN found only two results within d_ref=0.4.
	ann := rowsOf(0.1, 0.3, 0.9, 1.2)
	r, nEff := recall.ComputeRecall(exact, ann, 4, 0)
	if nEff != 4 {
		t.Fatalf("nEff=%d, want 4", nEff)
	}
	if r != 0.5 {
		t.Errorf("recall=%v, want 0.5", r)
	}
}

// The small-scope case behind gate (d): a 17-row window at k=75 must be able
// to reach recall 1.0 — the /k arithmetic would sit at 17/75≈0.227 forever.
func TestComputeRecallSmallScopeNEff(t *testing.T) {
	var dists []float64
	for i := 0; i < 17; i++ {
		dists = append(dists, 0.01*float64(i+1))
	}
	exact := rowsOf(dists...)
	ann := rowsOf(dists...)
	r, nEff := recall.ComputeRecall(exact, ann, 75, 0)
	if nEff != 17 {
		t.Fatalf("nEff=%d, want 17 (min(k,|E|))", nEff)
	}
	if r != 1.0 {
		t.Errorf("recall=%v, want 1.0 — /k-style arithmetic would report %.3f and is broken", r, 17.0/75.0)
	}
}

// Distance ties at the k boundary: the ANN list may carry MORE rows within
// d_ref than n_eff (equal distances). That must clamp to 1.0, not exceed it,
// and never count as a miss.
func TestComputeRecallBoundaryTies(t *testing.T) {
	exact := rowsOf(0.1, 0.2, 0.3)
	ann := rowsOf(0.1, 0.2, 0.3, 0.3, 0.3)
	r, nEff := recall.ComputeRecall(exact, ann, 3, 0)
	if r != 1.0 || nEff != 3 {
		t.Errorf("recall=%v nEff=%d, want clamped 1.0/3", r, nEff)
	}
}

func TestComputeRecallEpsilon(t *testing.T) {
	exact := rowsOf(0.1, 0.2, 0.3)
	// 0.3005 is outside d_ref=0.3 at eps=0, inside at eps=0.001.
	ann := rowsOf(0.1, 0.2, 0.3005)
	r, _ := recall.ComputeRecall(exact, ann, 3, 0)
	if want := 2.0 / 3.0; r != want {
		t.Errorf("eps=0: recall=%v, want %v", r, want)
	}
	r, _ = recall.ComputeRecall(exact, ann, 3, 0.001)
	if r != 1.0 {
		t.Errorf("eps=0.001: recall=%v, want 1.0", r)
	}
}

func TestComputeRecallEmptyExact(t *testing.T) {
	r, nEff := recall.ComputeRecall(nil, rowsOf(0.1), 10, 0)
	if r != 0 || nEff != 0 {
		t.Errorf("empty exact list: recall=%v nEff=%d, want 0/0 (caller skips)", r, nEff)
	}
}

func TestPercentileNearestRank(t *testing.T) {
	xs := []float64{5, 1, 4, 2, 3}
	if got := recall.Percentile(xs, 0.5); got != 3 {
		t.Errorf("p50=%v, want 3", got)
	}
	if got := recall.Percentile(xs, 0.95); got != 5 {
		t.Errorf("p95=%v, want 5", got)
	}
	if got := recall.Percentile([]float64{7}, 0.95); got != 7 {
		t.Errorf("single-element p95=%v, want 7", got)
	}
	// Input order must survive (percentile sorts a copy).
	if xs[0] != 5 || xs[4] != 3 {
		t.Errorf("percentile mutated its input: %v", xs)
	}
}

func TestParseKList(t *testing.T) {
	ks, err := recall.ParseKList("75,10,10")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(ks) != 2 || ks[0] != 10 || ks[1] != 75 {
		t.Errorf("ks=%v, want [10 75] (sorted, deduplicated)", ks)
	}
	for _, bad := range []string{"", "0", "-5", "a,b", "10,,x"} {
		if _, err := recall.ParseKList(bad); err == nil {
			t.Errorf("ParseKList(%q) accepted invalid input", bad)
		}
	}
}

func TestParseStrataBounds(t *testing.T) {
	// The default is pinned to the E-02-1 selector thresholds (masterplan
	// K3): exact_max=4096 and grey_max=65536 ARE stratum boundaries.
	b1, b2, err := recall.ParseStrataBounds("4096,65536")
	if err != nil || b1 != 4096 || b2 != 65536 {
		t.Errorf("bounds=%d,%d err=%v, want 4096,65536", b1, b2, err)
	}
	for _, bad := range []string{"", "5", "10,5", "10,10", "0,5", "a,b", "1,2,3"} {
		if _, _, err := recall.ParseStrataBounds(bad); err == nil {
			t.Errorf("ParseStrataBounds(%q) accepted invalid input", bad)
		}
	}
}
