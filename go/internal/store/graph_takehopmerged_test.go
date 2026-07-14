// GB3/W4-G2: takeHopMerged — the E2 zipper. Fairness (no class starvation in
// either direction), determinism, the empty-structural regression anchor
// (byte-identical to legacy takeHop) and the one-node-one-slot dedupe.
// Red probe (commit body): naively sorting structural conf=1.0 candidates
// into the single takeHop list fills ALL budget slots with facts.
package store

import (
	"fmt"
	"testing"
)

func dreamCand(id string, conf float64) hopCandidate {
	return hopCandidate{node: GraphNode{ID: id, Title: id}, conf: conf}
}

func structCand(id string) hopCandidate {
	return hopCandidate{node: GraphNode{ID: id, Title: id}, conf: 1}
}

func TestTakeHopMerged_ZipperFairness(t *testing.T) {
	// W4-G2 core: 30 dream + 30 structural, budget 20 → 10/10. A naive
	// conf-sorted single list would hand all 20 slots to the 1.0 facts.
	var dream, structural []hopCandidate
	for i := 0; i < 30; i++ {
		dream = append(dream, dreamCand(fmt.Sprintf("d%02d", i), 0.9-float64(i)/100))
		structural = append(structural, structCand(fmt.Sprintf("s%02d", i)))
	}
	out, truncated := takeHopMerged(dream, structural, map[string]bool{}, 20, 1)
	if len(out) != 20 || !truncated {
		t.Fatalf("out=%d truncated=%v, want 20/true", len(out), truncated)
	}
	var d, s int
	for _, n := range out {
		if n.ID[0] == 'd' {
			d++
		} else {
			s++
		}
	}
	if d != 10 || s != 10 {
		t.Errorf("zipper split = %d dream / %d structural, want 10/10 (class starvation)", d, s)
	}
	// Determinism + convention: dream starts.
	if out[0].ID[0] != 'd' || out[1].ID[0] != 's' {
		t.Errorf("zipper start = %s,%s — want dream first (deterministic convention)", out[0].ID, out[1].ID)
	}
}

func TestTakeHopMerged_EmptyStructural_RegressionAnchor(t *testing.T) {
	// Empty structural side ⇒ byte-identical to legacy takeHop: conf DESC,
	// id ASC, visited skips free, budget cut sets truncated.
	dream := []hopCandidate{
		dreamCand("b", 0.5), dreamCand("a", 0.9), dreamCand("c", 0.9), dreamCand("v", 0.99),
	}
	visited := map[string]bool{"v": true}
	out, truncated := takeHopMerged(dream, nil, visited, 2, 3)
	if len(out) != 2 || !truncated {
		t.Fatalf("out=%d truncated=%v, want 2/true", len(out), truncated)
	}
	// a and c share conf 0.9 → id ASC; v is visited (free skip); b cut.
	if out[0].ID != "a" || out[1].ID != "c" {
		t.Errorf("order = %s,%s, want a,c (conf DESC, id ASC — legacy takeHop)", out[0].ID, out[1].ID)
	}
	if out[0].Hop != 3 {
		t.Errorf("Hop = %d, want 3", out[0].Hop)
	}
}

func TestTakeHopMerged_DualListNode_OneSlot(t *testing.T) {
	// A node in BOTH lists (dream AND structural edge to the frontier) takes
	// ONE slot; the second appearance is a free skip that must NOT shift the
	// zipper turn — the other side keeps its alternating share.
	dream := []hopCandidate{dreamCand("x", 0.9), dreamCand("d1", 0.8), dreamCand("d2", 0.7)}
	structural := []hopCandidate{structCand("x"), structCand("s1"), structCand("s2")}
	out, truncated := takeHopMerged(dream, structural, map[string]bool{}, 4, 1)
	if !truncated {
		t.Error("truncated = false — 5 distinct nodes at budget 4 must flag the cut")
	}
	ids := map[string]int{}
	for i, n := range out {
		ids[n.ID] = i
	}
	if len(out) != 4 {
		t.Fatalf("out = %d, want 4", len(out))
	}
	if _, dup := ids["x"]; !dup {
		t.Fatal("dual-list node x missing")
	}
	// x consumed dream's first turn; structural's turns deliver s1, s2 — the
	// free skip of x on the structural side must not cost structural a slot.
	if _, ok := ids["s1"]; !ok {
		t.Error("s1 missing — structural lost a turn to the dual-list skip")
	}
	if _, ok := ids["s2"]; !ok {
		t.Error("s2 missing — structural lost a turn to the dual-list skip")
	}
}

func TestTakeHopMerged_DrainAndBudget(t *testing.T) {
	// One side exhausted → the other drains; truncated only when an unvisited
	// candidate actually fails the budget.
	dream := []hopCandidate{dreamCand("d1", 0.9)}
	structural := []hopCandidate{structCand("s1"), structCand("s2"), structCand("s3")}
	out, truncated := takeHopMerged(dream, structural, map[string]bool{}, 10, 1)
	if len(out) != 4 || truncated {
		t.Fatalf("out=%d truncated=%v, want 4/false (drain, budget ample)", len(out), truncated)
	}
	out2, truncated2 := takeHopMerged(nil, structural, map[string]bool{}, 2, 1)
	if len(out2) != 2 || !truncated2 {
		t.Fatalf("structural-only: out=%d truncated=%v, want 2/true", len(out2), truncated2)
	}
	// SQL order is the total order for structural — no Go re-sort.
	if out2[0].ID != "s1" || out2[1].ID != "s2" {
		t.Errorf("structural order = %s,%s, want s1,s2 (SQL order preserved)", out2[0].ID, out2[1].ID)
	}
}
