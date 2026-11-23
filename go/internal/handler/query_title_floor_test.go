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

func TestFloorTitleMatchPlacesTheNamedBlockFirst(t *testing.T) {
	in := familyList()
	want := in[12]
	out := floorTitleMatch(in, "  orbit NIGHTLY report   2026-08-13 ", nil)
	if len(out) != len(in) {
		t.Fatalf("len %d, want %d", len(out), len(in))
	}
	if got := titleFloorIDs(out); got != "b12,b00,b01,b02,b03,b04,b05,b06,b07,b08,b09,b10,b11,b13,b14" {
		t.Fatalf("order = %s", got)
	}
	// The placed block takes the score fields of the row whose place it takes
	// (the former first), so the list stays sorted and both labels read what
	// they read there.
	if out[0].RRFScore != in[0].RRFScore || out[0].RRFScoreOriginal != in[0].RRFScoreOriginal {
		t.Errorf("placed scores %v/%v, want the former first's %v/%v", out[0].RRFScore, out[0].RRFScoreOriginal, in[0].RRFScore, in[0].RRFScoreOriginal)
	}
	if out[1].RRFScore != in[0].RRFScore {
		t.Errorf("the former first lost its score: %v", out[1].RRFScore)
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
		name string
		in   []rrf.SearchResult
		q    string
		skip []string
	}{
		{"already first", fam, "Orbit nightly report 2026-08-01", nil},
		{"question about the family's topic", fam, "Orbit nightly report", nil},
		{"question near a title but not equal", fam, "Orbit nightly report 2026-08-13 summary", nil},
		{"a title that only begins with the question",
			titleFloorList("a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "Orbit nightly report 2026-08-13 (rerun)"),
			"Orbit nightly report 2026-08-13", nil},
		{"question that contains a short title", append(familyList(), titleFloorList("Orbit")...),
			"Wie starte ich Orbit nach dem Update neu?", nil},
		{"title of one frequent word other titles contain",
			titleFloorList("Deploy runbook 1", "Deploy runbook 2", "Deploy runbook 3", "a", "b", "c", "d", "e", "f", "g", "h", "Deploy"),
			"deploy", nil},
		{"of two candidates with the title the higher one is already first",
			titleFloorList("Release notes", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "Release notes"),
			"Release notes", nil},
		{"a shadow row is never placed", fam, "Orbit nightly report 2026-08-13", []string{"knowledge"}},
		{"a title that differs from the question only in a closing mark other titles contain",
			titleFloorList("Deploy runbook 1", "a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "Deploy."),
			"deploy?", nil},
		{"one candidate only", fam[:1], "Orbit nightly report 2026-08-01", nil},
		{"empty question", fam, "   ", nil},
		{"punctuation-only question", fam, " ?! ", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := titleFloorIDs(c.in)
			if got := titleFloorIDs(floorTitleMatch(c.in, c.q, c.skip)); got != before {
				t.Errorf("order changed:\n got %s\nwant %s", got, before)
			}
		})
	}
}

// The former first's rrf_score_original (the gravity's, the reranker's) comes
// with the place; the block's own original does not.
func TestFloorTitleMatchInheritsTheFirstsOriginal(t *testing.T) {
	in := familyList()
	own, first := 0.5, 0.25
	in[12].RRFScoreOriginal = &own
	in[0].RRFScoreOriginal = &first
	out := floorTitleMatch(in, "Orbit nightly report 2026-08-13", nil)
	if out[0].RRFScoreOriginal == nil || *out[0].RRFScoreOriginal != first {
		t.Errorf("rrf_score_original = %v, want the former first's %v", out[0].RRFScoreOriginal, first)
	}
	if out[1].RRFScoreOriginal == nil || *out[1].RRFScoreOriginal != first {
		t.Errorf("the former first lost its original: %v", out[1].RRFScoreOriginal)
	}
}

// A block the pipeline ranked inside the limit but behind the first moves up
// as well; the results it passes move down by one, the rest stay.
func TestFloorTitleMatchFromInsideTheLimit(t *testing.T) {
	in := familyList()
	out := floorTitleMatch(in, "Orbit nightly report 2026-08-04", nil)
	if got := titleFloorIDs(out); got != "b03,b00,b01,b02,b04,b05,b06,b07,b08,b09,b10,b11,b12,b13,b14" {
		t.Errorf("order = %s", got)
	}
}

// A block the cross-encoder left behind its window carries score 0; placed,
// it takes the former first's score, so a score threshold downstream
// (query.score_threshold in the synthesis path) does not drop it.
func TestFloorTitleMatchScoreZeroTakesThePlace(t *testing.T) {
	in := familyList()
	in[12].RRFScore = 0
	out := floorTitleMatch(in, "Orbit nightly report 2026-08-13", nil)
	if out[0].ID != in[12].ID || out[0].RRFScore != in[0].RRFScore {
		t.Fatalf("first = %s with score %v, want %s with %v", out[0].ID, out[0].RRFScore, in[12].ID, in[0].RRFScore)
	}
}

// A row the graph or cluster stage injected is placed like any other.
func TestFloorTitleMatchInjectedRow(t *testing.T) {
	for _, via := range []string{"graph", "cluster"} {
		in := familyList()
		in[12].ViaGraph = via == "graph"
		in[12].ViaCluster = via == "cluster"
		if out := floorTitleMatch(in, "Orbit nightly report 2026-08-13", nil); out[0].ID != in[12].ID {
			t.Fatalf("%s: injected row not placed", via)
		}
	}
}

// Two candidates fold to the question (P4): the one the pipeline ranks higher
// is placed, the other stays where it was.
func TestFloorTitleMatchTwinsPlaceTheHigherOne(t *testing.T) {
	in := titleFloorList("a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "Release notes", "m", "Release notes.")
	out := floorTitleMatch(in, "release notes", nil)
	if got := titleFloorIDs(out); got != "b11,b00,b01,b02,b03,b04,b05,b06,b07,b08,b09,b10,b12,b13" {
		t.Errorf("order = %s, want the higher-ranked b11 first and b13 where it was", got)
	}
}

// The folded form (issue #507, round 1): what people add to a question or type
// differently folds away; what tells two titles of one family apart stays.
func TestFoldTitle(t *testing.T) {
	same := [][2]string{
		{"Orbit nightly report 2026-08-31?", "orbit nightly report 2026-08-31"},
		{"Orbit nightly report 2026-08-31 ?!", "Orbit nightly report 2026-08-31"},
		{"Orbit nightly report 2026-08-31…", "Orbit nightly report 2026-08-31"},
		{"Orbit nightly report 2026-08-31？", "Orbit nightly report 2026-08-31"},
		{"Release notes:", "release notes"},
		{"Plan „Zielbild B“", "Plan \"Zielbild B\""},
		{"Plan “Zielbild B”", "Plan \"Zielbild B\""},
		{"Plan »Zielbild B«", "Plan \"Zielbild B\""},
		{"Orbit’s report", "Orbit's report"},
		{"Übergabe 2026-08-31", "Übergabe 2026-08-31"},
		{"Ｏｒｂｉｔ report", "Orbit report"},
		{"  Orbit\tnightly\n report  ", "orbit nightly report"},
	}
	for _, c := range same {
		if foldTitle(c[0]) != foldTitle(c[1]) {
			t.Errorf("fold(%q) = %q, fold(%q) = %q: want equal", c[0], foldTitle(c[0]), c[1], foldTitle(c[1]))
		}
	}
	different := [][2]string{
		{"ctx v5.75.1 release", "ctx v5751 release"},
		{"Orbit nightly report (2026-08-31)", "Orbit nightly report 2026-08-31"},
		{"Orbit nightly report 2026.08.31", "Orbit nightly report 2026-08-31"},
		{"Plan \"Zielbild B\"", "Plan Zielbild B"},
		{"Straße 2026", "Strasse 2026"},
		{"? Orbit report", "Orbit report"},
	}
	for _, c := range different {
		if foldTitle(c[0]) == foldTitle(c[1]) {
			t.Errorf("fold(%q) = fold(%q) = %q: want different", c[0], c[1], foldTitle(c[0]))
		}
	}
	if got := foldTitle(" ?! "); got != "" {
		t.Errorf("fold of punctuation only = %q, want empty", got)
	}
}
