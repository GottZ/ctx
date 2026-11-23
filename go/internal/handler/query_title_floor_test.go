package handler

import (
	"fmt"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/rrf"
)

// titleFloorList builds a candidate list in rank order with descending scores.
func titleFloorList(titles ...string) []rrf.SearchResult {
	out := make([]rrf.SearchResult, len(titles))
	for i, t := range titles {
		out[i] = rrf.SearchResult{ID: fmt.Sprintf("b%02d", i), Title: t, TypeName: "knowledge", RRFScore: 1.0 / float64(60+i)}
	}
	return out
}

func titleFloorIDs(rs []rrf.SearchResult) string {
	ids := make([]string, len(rs))
	for i := range rs {
		ids[i] = rs[i].ID
	}
	return strings.Join(ids, ",")
}

// familyList: 15 near-duplicate reports; the named one is rank 13 (index 12).
func familyList() []rrf.SearchResult {
	var titles []string
	for i := 1; i <= 15; i++ {
		titles = append(titles, fmt.Sprintf("Orbit nightly report 2026-08-%02d", i))
	}
	return titleFloorList(titles...)
}

func TestFloorTitleMatchPlacesTheNamedBlockAtTheCutOff(t *testing.T) {
	in := familyList()
	want := in[12]
	out := floorTitleMatch(in, "  orbit NIGHTLY report   2026-08-13 ", 10, nil)
	if len(out) != len(in) {
		t.Fatalf("len %d, want %d", len(out), len(in))
	}
	if out[9].ID != want.ID {
		t.Fatalf("place 10 holds %s, want the named block %s", out[9].ID, want.ID)
	}
	if titleFloorIDs(out[:9]) != titleFloorIDs(in[:9]) {
		t.Errorf("first nine moved: %s", titleFloorIDs(out[:9]))
	}
	// The displaced tenth and the rest follow in their order, nothing lost.
	if titleFloorIDs(out[10:]) != "b09,b10,b11,b13,b14" {
		t.Errorf("tail = %s", titleFloorIDs(out[10:]))
	}
	// The placed block takes the score of the place it takes, so the list
	// stays sorted and a score threshold judges the place; its own score is
	// kept as the original.
	if out[9].RRFScore != in[9].RRFScore {
		t.Errorf("placed score %v, want the displaced result's %v", out[9].RRFScore, in[9].RRFScore)
	}
	if out[9].RRFScoreOriginal == nil || *out[9].RRFScoreOriginal != want.RRFScore {
		t.Errorf("rrf_score_original = %v, want the block's own score %v", out[9].RRFScoreOriginal, want.RRFScore)
	}
	for i := 1; i < len(out); i++ {
		if out[i].RRFScore > out[i-1].RRFScore {
			t.Errorf("list not sorted by score at %d: %v > %v", i, out[i].RRFScore, out[i-1].RRFScore)
		}
	}
	if in[12].RRFScoreOriginal != nil || in[12].RRFScore != want.RRFScore {
		t.Error("input element was modified")
	}
	if titleFloorIDs(in) != titleFloorIDs(familyList()) {
		t.Error("input slice was modified")
	}
}

func TestFloorTitleMatchLeavesTheOrder(t *testing.T) {
	fam := familyList()
	cases := []struct {
		name  string
		in    []rrf.SearchResult
		q     string
		limit int
		skip  []string
	}{
		{"already inside the limit", fam, "Orbit nightly report 2026-08-04", 10, nil},
		{"question about the family's topic", fam, "Orbit nightly report", 10, nil},
		{"question near a title but not equal", fam, "Orbit nightly report 2026-08-13 summary", 10, nil},
		{"a title that only begins with the question",
			titleFloorList("a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "Orbit nightly report 2026-08-13 (rerun)"),
			"Orbit nightly report 2026-08-13", 10, nil},
		{"question that contains a short title", append(familyList(), titleFloorList("Orbit")...),
			"Wie starte ich Orbit nach dem Update neu?", 10, nil},
		{"title of one frequent word other titles contain",
			titleFloorList("Deploy runbook 1", "Deploy runbook 2", "Deploy runbook 3", "a", "b", "c", "d", "e", "f", "g", "h", "Deploy"),
			"deploy", 10, nil},
		{"two candidates carry the title",
			titleFloorList("a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "Release notes", "Release notes"),
			"Release notes", 10, nil},
		{"a shadow row is never placed", fam, "Orbit nightly report 2026-08-13", 10, []string{"knowledge"}},
		{"no more candidates than the limit", fam[:10], "Orbit nightly report 2026-08-10", 10, nil},
		{"empty question", fam, "   ", 10, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := titleFloorIDs(c.in)
			if got := titleFloorIDs(floorTitleMatch(c.in, c.q, c.limit, c.skip)); got != before {
				t.Errorf("order changed:\n got %s\nwant %s", got, before)
			}
		})
	}
}

// An original score an earlier stage recorded (the gravity's) is kept.
func TestFloorTitleMatchKeepsAnEarlierOriginal(t *testing.T) {
	in := familyList()
	pre := 0.5
	in[12].RRFScoreOriginal = &pre
	out := floorTitleMatch(in, "Orbit nightly report 2026-08-13", 10, nil)
	if out[9].RRFScoreOriginal == nil || *out[9].RRFScoreOriginal != pre {
		t.Errorf("rrf_score_original = %v, want the earlier stage's %v", out[9].RRFScoreOriginal, pre)
	}
}

// The block right behind the limit (place 11) is placed too.
func TestFloorTitleMatchPlaceRightBehindTheLimit(t *testing.T) {
	in := familyList()
	out := floorTitleMatch(in, "Orbit nightly report 2026-08-11", 10, nil)
	if out[9].ID != in[10].ID || out[10].ID != in[9].ID {
		t.Errorf("places 10/11 = %s/%s, want %s/%s", out[9].ID, out[10].ID, in[10].ID, in[9].ID)
	}
}

// A block the cross-encoder left behind its window carries score 0; placed,
// it takes the displaced result's score, so a score threshold downstream
// (query.score_threshold in the synthesis path) does not drop the place.
func TestFloorTitleMatchScoreZeroTakesThePlace(t *testing.T) {
	in := familyList()
	in[12].RRFScore = 0
	orig := 0.0066
	in[12].RRFScoreOriginal = &orig
	out := floorTitleMatch(in, "Orbit nightly report 2026-08-13", 10, nil)
	if out[9].ID != in[12].ID || out[9].RRFScore != in[9].RRFScore {
		t.Fatalf("place 10 = %s with score %v, want %s with %v", out[9].ID, out[9].RRFScore, in[12].ID, in[9].RRFScore)
	}
	if out[9].RRFScoreOriginal == nil || *out[9].RRFScoreOriginal != orig {
		t.Errorf("rrf_score_original = %v, want the reranker's record %v", out[9].RRFScoreOriginal, orig)
	}
}

// A row the graph or cluster stage injected carries no derived score field;
// placed, it stays without one.
func TestFloorTitleMatchInjectedRowGetsNoOriginal(t *testing.T) {
	for _, via := range []string{"graph", "cluster"} {
		in := familyList()
		in[12].ViaGraph = via == "graph"
		in[12].ViaCluster = via == "cluster"
		out := floorTitleMatch(in, "Orbit nightly report 2026-08-13", 10, nil)
		if out[9].ID != in[12].ID {
			t.Fatalf("%s: injected row not placed", via)
		}
		if out[9].RRFScoreOriginal != nil {
			t.Errorf("%s: injected row got rrf_score_original %v", via, *out[9].RRFScoreOriginal)
		}
	}
}

// With limit 1 the named block is the answer.
func TestFloorTitleMatchLimitOne(t *testing.T) {
	in := familyList()
	out := floorTitleMatch(in, "Orbit nightly report 2026-08-13", 1, nil)
	if out[0].ID != in[12].ID || titleFloorIDs(out[1:4]) != "b00,b01,b02" {
		t.Errorf("limit 1: %s", titleFloorIDs(out[:4]))
	}
}
