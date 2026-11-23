package handler

import (
	"fmt"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/llm"
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
	// The placed block takes the score fields of the row whose place it takes,
	// so the list stays sorted and both labels read what they read there.
	if out[9].RRFScore != in[9].RRFScore || out[9].RRFScoreOriginal != in[9].RRFScoreOriginal {
		t.Errorf("placed scores %v/%v, want the displaced row's %v/%v", out[9].RRFScore, out[9].RRFScoreOriginal, in[9].RRFScore, in[9].RRFScoreOriginal)
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
		{"of two candidates with the title the higher one already stands inside",
			titleFloorList("a", "Release notes", "c", "d", "e", "f", "g", "h", "i", "j", "k", "Release notes"),
			"Release notes", 10, nil},
		{"a shadow row is never placed", fam, "Orbit nightly report 2026-08-13", 10, []string{"knowledge"}},
		{"a title that differs from the question only in a closing mark other titles contain",
			titleFloorList("Deploy runbook 1", "a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "Deploy."),
			"deploy?", 10, nil},
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

// The place's rrf_score_original (the gravity's, the reranker's) comes with
// the place; the block's own original does not.
func TestFloorTitleMatchInheritsThePlacesOriginal(t *testing.T) {
	in := familyList()
	own, place := 0.5, 0.25
	in[12].RRFScoreOriginal = &own
	in[9].RRFScoreOriginal = &place
	out := floorTitleMatch(in, "Orbit nightly report 2026-08-13", 10, nil)
	if out[9].RRFScoreOriginal == nil || *out[9].RRFScoreOriginal != place {
		t.Errorf("rrf_score_original = %v, want the place's %v", out[9].RRFScoreOriginal, place)
	}
}

// The place of the floor (P2): the last place every synthesis prompt
// carries — the low_confidence cut — never past the limit.
func TestTitleFloorPlace(t *testing.T) {
	cases := []struct{ limit, cut, want int }{
		{10, 0, llm.LowConfidenceMaxSources}, {10, 2, 2}, {10, 3, 3}, {2, 5, 2}, {1, 2, 1}, {5, 5, 5},
	}
	for _, c := range cases {
		if got := titleFloorPlace(c.limit, llm.SynthesisSettings{LowConfidenceMaxSources: c.cut}); got != c.want {
			t.Errorf("titleFloorPlace(limit %d, cut %d) = %d, want %d", c.limit, c.cut, got, c.want)
		}
	}
}

// At place 2: the first result stays, the named block follows, the rest move
// down by one and the tenth leaves at the end of a limit of 10.
func TestFloorTitleMatchPlaceTwo(t *testing.T) {
	in := familyList()
	out := floorTitleMatch(in, "Orbit nightly report 2026-08-13", 2, nil)
	if got := titleFloorIDs(out[:11]); got != "b00,b12,b01,b02,b03,b04,b05,b06,b07,b08,b09" {
		t.Errorf("order = %s", got)
	}
	if out[1].RRFScore != in[1].RRFScore {
		t.Errorf("placed score %v, want place 2's %v", out[1].RRFScore, in[1].RRFScore)
	}
	// Already at the place or in front of it: nothing moves.
	if got := titleFloorIDs(floorTitleMatch(in, "Orbit nightly report 2026-08-02", 2, nil)); got != titleFloorIDs(in) {
		t.Errorf("a block at place 2 moved: %s", got)
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
	if out[9].RRFScoreOriginal != in[9].RRFScoreOriginal {
		t.Errorf("rrf_score_original = %v, want the place's %v", out[9].RRFScoreOriginal, in[9].RRFScoreOriginal)
	}
}

// A row the graph or cluster stage injected carries no derived score field;
// placed, it takes the place's (here none).
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

// Two candidates fold to the question (P4): the one the pipeline ranks higher
// is placed, the other stays where it was.
func TestFloorTitleMatchTwinsPlaceTheHigherOne(t *testing.T) {
	in := titleFloorList("a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "Release notes", "m", "Release notes.")
	out := floorTitleMatch(in, "release notes", 10, nil)
	if out[9].ID != "b11" {
		t.Fatalf("place 10 holds %s, want the higher-ranked b11", out[9].ID)
	}
	if got := titleFloorIDs(out[10:]); got != "b09,b10,b12,b13" {
		t.Errorf("tail = %s, want the lower one (b13) left where it was", got)
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
