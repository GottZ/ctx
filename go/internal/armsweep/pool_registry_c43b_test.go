package armsweep_test

// Wave C4-3b — the consistency seam between the driver that FILLS a pool and
// the tool that BUILDS the judgement template from it.
//
// Two places name the judged slices, and they have to name the same set:
//
//	armsweep.pooledSlice   decides which slices `prime` carries arm heads for
//	goldset.PooledSlices   decides which slices `ctx-goldset pool` accepts
//
// They are deliberately not one symbol — the first is a property of the
// measurement run, the second of the gold files, and the packages depend in one
// direction only (armsweep -> goldset). What keeps them together is this test,
// and it compares them through the PRODUCTION priming path rather than by
// reading one list against the other: a predicate that answered differently
// under a real run than in its own table would pass a list comparison.
//
// Drift in either direction is a live defect. A slice pooled but not offered is
// wasted bytes nobody can judge (that was Befund N1 in reverse); a slice
// offered but not pooled produces "no pool entry for case <slice>/0/<digest>"
// at the first case of a run somebody scheduled for hours.

import (
	"testing"

	"github.com/GottZ/ctx/internal/armsweep"
	"github.com/GottZ/ctx/internal/goldset"
)

func TestPrimePoolsExactlyTheJudgedSlices(t *testing.T) {
	t.Parallel()
	registry := armsweep.CanonicalSlices()
	cases := make([]goldset.Case, 0, len(registry))
	for i, s := range registry {
		cases = append(cases, c43Case(s, i))
	}

	pooled := map[string]bool{}
	for _, e := range c43Prime(t, c43Runner(t, false), cases) {
		pooled[e.Slice] = true
	}
	judged := map[string]bool{}
	for _, s := range goldset.PooledSlices() {
		judged[s] = true
	}

	for _, s := range registry {
		switch {
		case pooled[s] && !judged[s]:
			t.Errorf("%s: prime poolt den Slice, goldset.PooledSlices kennt ihn nicht — "+
				"die Kopf-Listen werden geschrieben und nie zu einer Vorlage", s)
		case judged[s] && !pooled[s]:
			t.Errorf("%s: goldset.PooledSlices bietet eine Vorlage an, prime poolt den Slice nicht — "+
				"`ctx-goldset pool -slice` bricht erst am ersten Fall ab", s)
		}
	}
}

// TestJudgedSliceFilesMatchTheSweepRegistry pins the second half: the gold file
// a template is built from must be the file the sweep measured against. Two
// tables naming one file is how a slice ends up judged on one population and
// scored on another.
func TestJudgedSliceFilesMatchTheSweepRegistry(t *testing.T) {
	t.Parallel()
	for _, s := range goldset.PooledSlices() {
		want, known := armsweep.SliceFileOf(s)
		if !known {
			t.Errorf("%s steht nicht in der Sweep-Registry", s)
			continue
		}
		got, ok := goldset.PoolSliceFile(s)
		if !ok {
			t.Errorf("%s: PooledSlices nennt ihn, PoolSliceFile kennt ihn nicht", s)
			continue
		}
		if got != want {
			t.Errorf("%s: Vorlage liest %q, der Sweep misst gegen %q", s, got, want)
		}
	}
}
