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

// Twins that fold to the same form (issue #528, point 2): the candidate whose
// title equals the question before folding — case and white space aside — is
// the named block, whatever the rank of its twin; the rank decides only when
// the question equals none of them that way.
func TestFloorTitleMatchTwinsPreferTheUnfoldedTitle(t *testing.T) {
	filler := []string{"a", "c", "e", "f", "g", "h", "i", "j", "k", "l"}
	list := func(higher, lower string) []rrf.SearchResult {
		titles := append([]string{filler[0], higher}, filler[1:]...)
		return titleFloorList(append(titles, lower)...)
	}
	cases := []struct {
		name, higher, lower, q, want string
	}{
		{"closing mark on the higher twin", "Release notes?", "Release notes", "Release notes", "b11"},
		{"the question names the twin with the mark", "Release notes?", "Release notes", "Release notes?", "b01"},
		{"full-width form", "Ｒelease notes", "Release notes", "Release notes", "b11"},
		{"superscript digit", "Plan v²", "Plan v2", "Plan v2", "b11"},
		{"typographic apostrophe", "Orbit’s report", "Orbit's report", "Orbit's report", "b11"},
		{"case and white space aside", "Release notes?", "Release notes", "  release   NOTES ", "b11"},
		{"canonically equivalent spelling", "Übergabe 2026-08-31?", "U\u0308bergabe 2026-08-31", "Übergabe 2026-08-31", "b11"},
		{"neither twin equals the question unfolded: the rank decides", "Release notes?", "Release notes.", "Release notes!", "b01"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := floorTitleMatch(list(c.higher, c.lower), c.q, nil)
			if out[0].ID != c.want {
				t.Errorf("first = %s (%q), want %s", out[0].ID, out[0].Title, c.want)
			}
		})
	}
}

// Issue #528, review round 1 (L1): a twin asked inside quotation marks or
// behind a leading phrase is told apart like the bare question — the
// comparison as typed runs on the question without its pair of marks (and the
// closing punctuation behind it) and without its phrase.
func TestFloorTitleMatchTwinsQuotedOrBehindAPhrase(t *testing.T) {
	filler := []string{"a", "c", "e", "f", "g", "h", "i", "j", "k", "l"}
	list := func(higher, lower string) []rrf.SearchResult {
		titles := append([]string{filler[0], higher}, filler[1:]...)
		return titleFloorList(append(titles, lower)...)
	}
	cases := []struct {
		name, higher, lower, q, want string
	}{
		{"low quote pair around the lower twin", "Notes v1", "Notes v1?", "„Notes v1?“", "b11"},
		{"ASCII pair around the lower twin", "Notes v1", "Notes v1?", "\"Notes v1?\"", "b11"},
		{"pair, then the question's own mark", "Notes v1?", "Notes v1", "„Notes v1“?", "b11"},
		{"single marks", "Notes v1", "Notes v1?", "'Notes v1?'", "b11"},
		{"pair around the higher twin", "Notes v1?", "Notes v1", "„Notes v1?“", "b01"},
		{"phrase before the lower twin", "Notes v1", "Notes v1?", "zeig mir Notes v1?", "b11"},
		{"phrase and pair", "Notes v1", "Notes v1?", "Was steht in „Notes v1?“", "b11"},
		{"typographic apostrophe in the phrase", "Notes v1", "Notes v1?", "What’s in Notes v1?", "b11"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := floorTitleMatch(list(c.higher, c.lower), c.q, nil)
			if out[0].ID != c.want {
				t.Errorf("%q: first = %s (%q), want %s", c.q, out[0].ID, out[0].Title, c.want)
			}
		})
	}
	// Twins behind a phrase that neither equals as typed name nothing — the
	// rule of exactly one candidate holds.
	in := list("Notes v1?", "Notes v1.")
	if out := floorTitleMatch(in, "zeig mir Notes v1!", nil); titleFloorIDs(out) != titleFloorIDs(in) {
		t.Errorf("phrase before neither twin as typed: order %s, want it unchanged", titleFloorIDs(out))
	}
}

// A title asked inside one pair of quotation marks names the block (issue
// #528, point 3); a question that contains a quotation is no title.
func TestFloorTitleMatchQuotedTitle(t *testing.T) {
	for _, q := range []string{
		"„Orbit nightly report 2026-08-13“",
		"\"Orbit nightly report 2026-08-13\"",
		"“Orbit nightly report 2026-08-13”",
		"»Orbit nightly report 2026-08-13«",
		"«Orbit nightly report 2026-08-13»",
		"'Orbit nightly report 2026-08-13'",
		"‚Orbit nightly report 2026-08-13‘",
		"`Orbit nightly report 2026-08-13`",
		"\"Orbit nightly report 2026-08-13\"?",
		"„Orbit nightly report 2026-08-13?“",
		" \" Orbit nightly report 2026-08-13 \" ",
	} {
		if out := floorTitleMatch(familyList(), q, nil); out[0].ID != "b12" {
			t.Errorf("%q: first = %s, want b12", q, out[0].ID)
		}
	}
	// A title that holds the mark it is asked in.
	apos := titleFloorList("a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "Orbit's report")
	if out := floorTitleMatch(apos, "'Orbit's report'", nil); out[0].ID != "b11" {
		t.Errorf("title with an apostrophe asked in single quotes: first = %s, want b11", out[0].ID)
	}
	for _, q := range []string{
		"Was steht in „Orbit nightly report 2026-08-13“ über den Build?",
		"„Orbit nightly report 2026-08-13“ und „Orbit nightly report 2026-08-14“",
		"\"Orbit nightly report 2026-08-13\" vs \"Orbit nightly report 2026-08-14\"",
		"\"Orbit nightly report 2026-08-13",
		"Orbit nightly report 2026-08-13\"",
		"„Orbit nightly“ report 2026-08-13",
		"\"Orbit nightly report 2026-08-13'",
		"\"\"",
	} {
		before := titleFloorIDs(familyList())
		if got := titleFloorIDs(floorTitleMatch(familyList(), q, nil)); got != before {
			t.Errorf("%q: order changed:\n got %s\nwant %s", q, got, before)
		}
	}
}

// A title asked behind a leading phrase (issue #528, point 4) names the block
// when the rest equals the title of exactly one candidate and no other
// candidate's title contains it; a topic question with the same start moves
// nothing.
func TestFloorTitleMatchLeadingPhrase(t *testing.T) {
	for _, q := range []string{
		"zeig mir Orbit nightly report 2026-08-13",
		"Zeig mir Orbit nightly report 2026-08-13",
		"zeige mir Orbit nightly report 2026-08-13",
		"was steht in Orbit nightly report 2026-08-13?",
		"Was steht in „Orbit nightly report 2026-08-13“?",
		"show me Orbit nightly report 2026-08-13",
		"Show me \"Orbit nightly report 2026-08-13\"",
		"what's in Orbit nightly report 2026-08-13?",
		"What’s in Orbit nightly report 2026-08-13",
		"what is in Orbit nightly report 2026-08-13",
	} {
		if out := floorTitleMatch(familyList(), q, nil); out[0].ID != "b12" {
			t.Errorf("%q: first = %s, want b12", q, out[0].ID)
		}
	}
	// Two candidates carry the rest as their title, byte for byte (twins that
	// differ as typed are told apart: TestFloorTitleMatchTwinsQuotedOrBehindAPhrase).
	twins := titleFloorList("a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "Release notes", "m", "Release notes")
	deploy := titleFloorList("Deploy runbook 1", "Deploy runbook 2", "a", "b", "c", "d", "e", "f", "g", "h", "i", "Deploy")
	for _, c := range []struct {
		name string
		in   []rrf.SearchResult
		q    string
	}{
		{"all of a family", familyList(), "zeig mir alle Orbit nightly reports"},
		{"the family's topic", familyList(), "zeig mir Orbit nightly report"},
		{"the rest is in the middle of a sentence", familyList(), "zeig mir was in Orbit nightly report 2026-08-13 steht"},
		{"the phrase without a rest", familyList(), "zeig mir"},
		{"a phrase that is not at the start", familyList(), "bitte zeig mir Orbit nightly report 2026-08-13"},
		{"the rest is a title other titles contain", deploy, "zeig mir Deploy"},
		{"the rest is the title of two candidates", twins, "zeig mir Release notes"},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := titleFloorIDs(c.in)
			if got := titleFloorIDs(floorTitleMatch(c.in, c.q, nil)); got != before {
				t.Errorf("%q: order changed:\n got %s\nwant %s", c.q, got, before)
			}
		})
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
		// Issue #528, point 3: one enclosing pair of quotation marks.
		{"„Release notes“", "Release notes"},
		{"\"Release notes\"?", "release notes"},
		{"„Release notes?“", "Release notes"},
		{"`Release notes`", "Release notes"},
		{"'Release notes'", "Release notes"},
		// The mark may occur inside the title (an apostrophe, a quotation).
		{"'Orbit's report'", "Orbit's report"},
		{"„Plan „Zielbild B“ final“", "Plan \"Zielbild B\" final"},
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
		// Issue #528, point 3: only a pair that encloses the whole text.
		{"\"Release\" notes", "Release notes"},
		{"\"Release notes", "Release notes"},
		{"\"Release notes'", "Release notes"},
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

// The placed block keeps its own score in TitleFloorScore (issue #528, point
// 7): the score fields are the first place's, this one is the score it
// carried where the pipeline put it. No other result carries one, and the
// input is not modified.
func TestFloorTitleMatchKeepsTheOwnScore(t *testing.T) {
	in := familyList()
	out := floorTitleMatch(in, "Orbit nightly report 2026-08-13", nil)
	if out[0].TitleFloorScore == nil || *out[0].TitleFloorScore != in[12].RRFScore {
		t.Fatalf("own score %v, want the block's %v", out[0].TitleFloorScore, in[12].RRFScore)
	}
	for i, r := range out[1:] {
		if r.TitleFloorScore != nil {
			t.Errorf("result %d carries an own score", i+2)
		}
	}
	if in[12].TitleFloorScore != nil {
		t.Error("input element was modified")
	}
	if out := floorTitleMatch(familyList(), "Orbit nightly report 2026-08-01", nil); out[0].TitleFloorScore != nil {
		t.Error("a block already first was not placed and carries no own score")
	}
}

// Issue #528, review round 1 (L2): a leading phrase ends at a word boundary —
// a question whose first word only begins like the phrase's last word ("show
// meNotes", "zeig mirNotes") is no phrase, and its rest names nothing.
func TestFloorTitleMatchPhraseEndsAtAWordBoundary(t *testing.T) {
	in := titleFloorList("a", "b", "c", "Notes")
	for _, p := range titlePhrases {
		q := strings.TrimSuffix(p, " ") + "Notes"
		if out := floorTitleMatch(in, q, nil); titleFloorIDs(out) != titleFloorIDs(in) {
			t.Errorf("%q: order %s, want it unchanged", q, titleFloorIDs(out))
		}
		if out := floorTitleMatch(in, p+"Notes", nil); out[0].Title != "Notes" {
			t.Errorf("%q: first %q, want Notes", p+"Notes", out[0].Title)
		}
	}
}

// Issue #528, review round 1 (L2): only a quotation mark encloses a title — a
// title that begins and ends with the same letter keeps both.
func TestFoldTitleKeepsAnEnclosingLetter(t *testing.T) {
	for in, want := range map[string]string{
		"Level":      "level",
		"Level?":     "level",
		"abba":       "abba",
		"'Level'":    "level",
		"`Level`":    "level",
		"1 to 1":     "1 to 1",
		"(Level)":    "(level)",
	} {
		if got := foldTitle(in); got != want {
			t.Errorf("foldTitle(%q) = %q, want %q", in, got, want)
		}
	}
}
