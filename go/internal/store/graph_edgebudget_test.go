// GB2/W3-G2: the edge-budget arbitration (design/01 §4.5) — ONE shared
// EdgeLimit, fact precedence WITH a dream floor. Pure function, no DB.
// Red probe (documented in the wave commit body): removing the floor
// (structSlots := min(|S|, edgeLimit)) starves dream at the dense-hub arm.
package store

import "testing"

func mkDream(n int) []GraphEdge {
	out := make([]GraphEdge, n)
	for i := range out {
		out[i] = GraphEdge{Src: i, Dst: i + 1, Rel: 0, Conf: 1 - float64(i)/1000}
	}
	return out
}

func mkStruct(n int) []structEdgeRow {
	out := make([]structEdgeRow, n)
	for i := range out {
		out[i] = structEdgeRow{src: "s", dst: "d", class: "references", origin: "system"}
	}
	return out
}

func TestArbitrateEdgeBudget(t *testing.T) {
	cases := []struct {
		name                 string
		dream, structN       int
		limit                int
		wantDream, wantStruc int
		wantTrunc            bool
	}{
		{"both fit — everything ships", 100, 50, 4000, 100, 50, false},
		{"exactly at budget", 100, 100, 200, 100, 100, false},
		// Dense hub (the §4.5 target-scale case): structural alone exceeds
		// the budget — WITHOUT the floor dream would be starved to zero.
		// |D| < EdgeLimit/2: dream keeps ALL its edges, the rest is fact slots.
		{"dense hub: small dream survives whole", 1000, 5000, 4000, 1000, 3000, true},
		// |D| >= EdgeLimit/2: the floor binds at exactly EdgeLimit/2 — the
		// discriminating case against a floorless fact-first cut (which would
		// leave dream 0 here).
		{"dense hub: floor binds at EdgeLimit/2", 3000, 5000, 4000, 2000, 2000, true},
		// Fact precedence in the normal case: structural takes what it needs,
		// dream keeps the rest (above its floor).
		{"fact precedence above floor", 3000, 1500, 4000, 2500, 1500, true},
		// Degeneration arm: little dream → the unused floor flows to
		// structural via min (red against a naive fixed 50/50 split).
		{"degeneration: unused floor flows to structural", 10, 5000, 4000, 10, 3990, true},
		{"empty structural — byte-identical dream behavior", 4000, 0, 4000, 4000, 0, false},
		{"empty dream — structural takes all", 0, 4000, 4000, 0, 4000, false},
		{"limit 1, both present: fact precedence carries the slot", 1, 2, 1, 0, 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, s, trunc := arbitrateEdgeBudget(mkDream(tc.dream), mkStruct(tc.structN), tc.limit)
			if len(d) != tc.wantDream || len(s) != tc.wantStruc || trunc != tc.wantTrunc {
				t.Errorf("got dream=%d struct=%d trunc=%v, want dream=%d struct=%d trunc=%v",
					len(d), len(s), trunc, tc.wantDream, tc.wantStruc, tc.wantTrunc)
			}
			if len(d)+len(s) > tc.limit {
				t.Errorf("budget exceeded: %d+%d > %d", len(d), len(s), tc.limit)
			}
			// Dream floor invariant: whenever a cut happens and dream had at
			// least EdgeLimit/2 candidates, dream keeps at least EdgeLimit/2.
			if trunc && tc.dream >= tc.limit/2 && len(d) < tc.limit/2 {
				t.Errorf("dream starved below its floor: kept %d < %d", len(d), tc.limit/2)
			}
		})
	}
}
