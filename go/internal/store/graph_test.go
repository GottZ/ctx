package store

import (
	"encoding/json"
	"testing"
	"time"
)

// SECURITY/WIRE PROPERTY: GraphEdge marshals as the exact fixed-width tuple
// with the confidence rounded to three decimals — float32-sourced values must
// not leak 0.8299999833106995-noise into the payload.
func TestGraphEdgeMarshalJSON(t *testing.T) {
	cases := []struct {
		name string
		edge GraphEdge
		want string
	}{
		{"spec example", GraphEdge{Src: 0, Dst: 3, Rel: 2, Conf: 0.83}, "[0,3,2,0.830]"},
		{"float32 noise", GraphEdge{Src: 0, Dst: 1, Rel: 0, Conf: float64(float32(0.83))}, "[0,1,0,0.830]"},
		{"zero conf", GraphEdge{Src: 5, Dst: 7, Rel: 4, Conf: 0}, "[5,7,4,0.000]"},
		{"full conf", GraphEdge{Src: 1, Dst: 0, Rel: 1, Conf: 1}, "[1,0,1,1.000]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(tc.edge)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}

// WIRE PROPERTY: the visibility triple is byte-exactly the canonical fragment
// from the design (§3.2) — Q1/Q3/focus-hydrate all reference this one string.
func TestVisibilityPredicate(t *testing.T) {
	got := VisibilityPredicate("b", "$2")
	want := "NOT b.is_archived AND b.block_role <> 'system-meta' AND b.scope = ANY($2::text[])"
	if got != want {
		t.Errorf("VisibilityPredicate(b,$2):\n got %q\nwant %q", got, want)
	}

	got = VisibilityPredicate("nb", "$2")
	want = "NOT nb.is_archived AND nb.block_role <> 'system-meta' AND nb.scope = ANY($2::text[])"
	if got != want {
		t.Errorf("VisibilityPredicate(nb,$2):\n got %q\nwant %q", got, want)
	}
}

func cand(id string, conf float64) hopCandidate {
	return hopCandidate{node: GraphNode{ID: id, Title: "t-" + id}, conf: conf}
}

// TRUNCATION ORDER: within one hop, candidates over budget are kept by
// confidence DESC, then id ASC; the rest is dropped and the hop reports
// truncation. Already-visited candidates never consume budget.
func TestTakeHop_TruncationOrder(t *testing.T) {
	visited := map[string]bool{"focus": true}
	cands := []hopCandidate{
		cand("dd", 0.5),
		cand("aa", 0.9),
		cand("cc", 0.7),
		cand("bb", 0.9), // ties with aa → id ASC: aa before bb
		cand("ee", 0.3),
	}

	got, truncated := takeHop(cands, visited, 3, 2)
	if !truncated {
		t.Fatal("expected truncated=true when budget < new candidates")
	}
	wantIDs := []string{"aa", "bb", "cc"}
	if len(got) != len(wantIDs) {
		t.Fatalf("got %d nodes, want %d", len(got), len(wantIDs))
	}
	for i, w := range wantIDs {
		if got[i].ID != w {
			t.Errorf("node[%d] = %s, want %s", i, got[i].ID, w)
		}
		if got[i].Hop != 2 {
			t.Errorf("node[%d].Hop = %d, want 2", i, got[i].Hop)
		}
		if !visited[w] {
			t.Errorf("node %s not marked visited", w)
		}
	}
}

// Exact fit is NOT truncation: when the budget covers all new candidates,
// the hop must not raise the flag.
func TestTakeHop_ExactFitNotTruncated(t *testing.T) {
	visited := map[string]bool{}
	cands := []hopCandidate{cand("aa", 0.9), cand("bb", 0.5)}

	got, truncated := takeHop(cands, visited, 2, 1)
	if truncated {
		t.Error("exact fit must not report truncation")
	}
	if len(got) != 2 {
		t.Fatalf("got %d nodes, want 2", len(got))
	}
}

// Visited candidates are skipped without consuming budget — a hop full of
// already-known neighbors yields nothing and no truncation, even at budget 0.
func TestTakeHop_VisitedSkippedWithoutBudget(t *testing.T) {
	visited := map[string]bool{"aa": true, "bb": true}
	cands := []hopCandidate{cand("aa", 0.9), cand("bb", 0.5)}

	got, truncated := takeHop(cands, visited, 0, 1)
	if truncated {
		t.Error("visited-only candidates must not report truncation")
	}
	if len(got) != 0 {
		t.Fatalf("got %d nodes, want 0", len(got))
	}

	// Budget 0 with one NEW candidate → truncated, nothing taken.
	got, truncated = takeHop([]hopCandidate{cand("cc", 0.4)}, visited, 0, 1)
	if !truncated || len(got) != 0 {
		t.Errorf("budget 0 with new candidate: got %d nodes truncated=%v, want 0/true", len(got), truncated)
	}
}

// traversalClasses: supersedes never traverses; nil stays nil (= all four);
// an explicit list keeps only positive types (possibly empty = match nothing).
func TestTraversalClasses(t *testing.T) {
	if got := traversalClasses(nil); got != nil {
		t.Errorf("nil → %v, want nil", got)
	}
	got := traversalClasses([]string{"topical", "supersedes", "causal"})
	if len(got) != 2 || got[0] != "topical" || got[1] != "causal" {
		t.Errorf("got %v, want [topical causal]", got)
	}
	got = traversalClasses([]string{"supersedes"})
	if got == nil || len(got) != 0 {
		t.Errorf("supersedes-only must yield empty non-nil slice (match nothing), got %v", got)
	}
}

// GraphNode payload has no content field — privacy/RAM contract of the
// graph wire format (content loads only via the scope-checked get API).
func TestGraphNodeHasNoContentField(t *testing.T) {
	n := GraphNode{
		ID: "019c9629-faac-75a5-98b6-de04dbfc8404", Title: "t", Category: "c",
		Scope: "private", Degree: 1, Hop: 0,
		CreatedAt: time.Date(2026, 2, 23, 11, 2, 41, 0, time.UTC),
	}
	b, err := json.Marshal(n)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m["content"]; ok {
		t.Error("GraphNode payload must not contain a content field")
	}
	for _, k := range []string{"id", "title", "category", "scope", "degree", "hop", "created_at"} {
		if _, ok := m[k]; !ok {
			t.Errorf("GraphNode payload missing field %q", k)
		}
	}
}
