package store

import (
	"math/rand/v2"
	"reflect"
	"regexp"
	"regexp/syntax"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/GottZ/ctx/internal/util"
)

// Differential tests of ExtractDates against the regexp implementation
// (dates_ref_test.go), in two versions: every content against refForms, the
// reference of the current extraction, and every content outside the input
// classes of the deliberate changes (issue437Classes) also against
// refFormsV434, the frozen extraction before #434. The criterion is
// identity, not similarity: the same timestamps, in the same order, with the
// same representation (time.Time compared with ==, which also compares the
// location pointer), and nil where the reference returns nil.

// issue437Classes lists, per deliberate change since #434 (issues #437 and
// #451), the contents it may change. A content none of them touches gives
// what refFormsV434 gives: together the predicates cover every content on
// which the current reference and the frozen one differ (each names the
// shape a differing match needs, ignoring word boundaries and calendar
// validity).
// TestIssue437Classes_EachIsNeeded shows that each one is needed,
// TestIssue437Classes_LeaveOrdinaryContentsOutside that none has grown over
// contents the changes do not affect.
var issue437Classes = []struct {
	name    string
	touches func(string) bool
	witness string // a content the change affects
}{
	// m[aä]rz → märz|maerz: a match that differs is a "marz" or "maerz"
	// before whitespace and a year token. (A differing match whose
	// whitespace or year only the later changes accept is theirs.)
	{"#437/1 maerz and marz", regexp.MustCompile(`(?i)mae?rz\s+20[23]\d`).MatchString, "Maerz 2026"},
	// \s → [\s\x{A0}\x{202F}]: a match that differs has a no-break space in
	// the run of date spaces between a month name and a year token, or
	// between the last digit of a date and the "hh:" of its time.
	{"#437/2 no-break spaces", regexp.MustCompile(`(?i:` + issue437Months + `)[\s\x{A0}\x{202F}]*[\x{A0}\x{202F}][\s\x{A0}\x{202F}]*20[2-9]\d|` +
		`\d[T\s\x{A0}\x{202F}]*[\x{A0}\x{202F}][T\s\x{A0}\x{202F}]*\d\d:`).MatchString, "Mai" + nbsp + "2026"},
	// 20[2-3]\d → 20[2-9]\d, window 2020-2030 → 2020-2099: a match that
	// differs is an ISO date, a dot date or a month name and year whose year
	// is 2031-2099. (2031-2039 matched before, without a date.)
	{"#437/3 years 2031-2099", regexp.MustCompile(`20(?:3[1-9]|[4-9]\d)-(?:0[1-9]|1[0-2])-(?:0[1-9]|[12]\d|3[01])|` +
		`(?:0[1-9]|[12]\d|3[01])\.(?:0[1-9]|1[0-2])\.20(?:3[1-9]|[4-9]\d)|` +
		`(?i:` + issue437Months + `)[\s\x{A0}\x{202F}]+20(?:3[1-9]|[4-9]\d)`).MatchString, "Mai 2031"},
	// #451/2, the long s in the table lookup: a match that differs is an
	// "august" or a "september" with a long s for its s before date spaces
	// and a year token.
	{"#451/2 long s", regexp.MustCompile(`(?:(?i:augu)\x{17F}(?i:t)|\x{17F}(?i:eptember))[\s\x{A0}\x{202F}]+20[2-9]\d`).MatchString,
		"Augu" + longS + "t 2026"},
}

// issue437Months are the month names of both month forms, before and after
// #437/1, as one alternation.
const issue437Months = "januar|februar|märz|maerz|marz|april|mai|juni|juli|august|september|oktober|november|dezember|" +
	"january|february|march|may|june|july|october|december"

// issue437Touched returns the name of the first class of issue437Classes
// content belongs to, or "".
func issue437Touched(content string) string {
	for _, c := range issue437Classes {
		if c.touches(content) {
			return c.name
		}
	}
	return ""
}

// Runes the case-insensitive month patterns of the reference match beyond
// ASCII case. Written as byte escapes so no editor normalizes them.
const (
	kelvinSign = "\xe2\x84\xaa" // U+212A KELVIN SIGN, folds to k
	longS      = "\xc5\xbf"     // U+017F LATIN SMALL LETTER LONG S, folds to s
	nbsp       = "\xc2\xa0"     // U+00A0 NO-BREAK SPACE, not in \s, a date space since #437
	nnbsp      = "\xe2\x80\xaf" // U+202F NARROW NO-BREAK SPACE, a date space since #437
	umlautA    = "\xc3\xa4"     // ä
	umlautCapA = "\xc3\x84"     // Ä
)

// datesSeedCorpus is the hand-written part of the differential corpus and the
// seed corpus of FuzzExtractDates: the contents of the T25 table in
// dates_test.go, every date form at the start and the end of a text, the
// month names in every spelling the reference treats differently, matches
// that overlap, non-ASCII and invalid UTF-8 around a date, and long runs.
func datesSeedCorpus() []string {
	seeds := []string{
		"",
		// The T25 contents (dates_test.go).
		"deployed on 2026-03-29", "deployed on 2026-02-30", "deployed on 2026-02-29",
		"deployed on 2028-02-29", "meeting on 2026-04-31", "deadline 2026-06-31",
		"from 2026-03-15 to 2026-02-30", "am 30.02.2026 war nichts", "am 15.03.2026 war es",
		// ISO with and without time, at the edges and against word characters.
		"2026-03-29", "2026-03-29 14:30", "2026-03-29T14:30", "2026-03-29T14:30:00Z",
		"2026-03-29T14:30:00Zx", "2026-03-29T14:30Zabc", "2026-03-29 14:30:5x", "2026-03-29 14:30:00x",
		"2026-03-29TT\t14:30", "2026-03-29T\n14:30:59.", "x2026-03-29", "_2026-03-29", "2026-03-29_",
		"2026-03-2914:30", "2026-03-29t14:30", "2026-03-29t 14:30", "2026-03-29 24:00", "2026-03-29 23:59:59Z.", "2026-03-29 23:59:60",
		"2026-12-2026-01-01", "2026-03-29 20:20:2026-04-01", "2026-03-29 14:2026-04-01",
		"2019-01-01 2031-01-01 2039-12-31 2020-01-01 2030-12-31", "2026-13-01 2026-00-10 2026-01-32 2026-01-00",
		"ü2026-03-29ß", "\xff2026-03-29\xfe", "2026-03-29\v14:30", "2026-03-29" + nbsp + "14:30",
		"T2026-03-29", "2026-03-29T", "2026-03-29 ", "2026-03-29T14:30:00Z2026-03-30",
		// Dot format, including the overlap a match with a time swallows.
		"29.03.2026", "29.03.2026 14:30", "29.03.202614:30", "01.01.2026 12:15.03.2026", "01.01.2026\t\n23:59x",
		"30.02.2026", "1.3.2026", "29.03.20261", "a29.03.2026", "29.03.2026a", "31.12.2039",
		"29.02.2028 29.02.2027", "31.04.2026", "29.03.2026 24:00", "29.03.2026 14:30:00",
		"20.20.2026", "12.12.2026.12.2026", "29.03.2026\v14:30", "01.01.2030 00:00",
		// Month names: case, umlaut, the spellings the regexp matches but the tables lack.
		"März 2026", "märz 2026", "MÄRZ 2026", "M" + umlautCapA + "RZ 2026", "m" + umlautA + "rz 2026",
		"Marz 2026", "Maerz 2026", "maerz 2026", "MAERZ 2026", "MaErZ 2026", "Ma" + umlautA + "erz 2026",
		"Maerzmaerz 2026", "xmaerz 2026", "maerz_2026", "Maer 2026", "Maerz Maerz 2026", "mAi 2027", "Mai\t\n 2028", "Mai\v2028", "Mai" + nbsp + "2028",
		"Mai\f\r2029", "Januar 2030", "January 2031", "januar 2020", "JANUARY 2020",
		longS + "eptember 2026", "x" + longS + "eptember 2026", "Augu" + longS + "t 2026",
		"O" + kelvinSign + "tober 2026", "x" + "o" + kelvinSign + "tober 2026", "OKTOBER 2026", "October 2026",
		"DEZEMBER 2026", "december 2026", "Dezember 2026x", "Dezember 2026_", "Dezember 20261", "Dezember 2026.",
		"Mai 2026Mai 2027", "Maimai 2026", "Mai Mai 2026", "€März 2026€", "\xc3März 2026", "M\xc3rz 2026",
		"Ma\xcc\x88rz 2026", "april 2026 April 2026 APRIL 2026", "August 2026", "November   2026",
		"Juni 2026 Juli 2026 June 2026 July 2026 May 2026 March 2026 February 2026 Februar 2026",
		"Mai 202", "Mai 20", "Mai ", "Mai", "2026 Mai", "seit März", "im Mai 1999", "Mai 2019 Mai 2035",
		"ümärz 2026", "_März 2026", "1März 2026", "März_2026", "März-2026", "März2026", "Mai2026.", "März\n\n\n2026\n",
		// No-break spaces between the parts of a date (#437/2), next to word
		// characters, \s, T, other spaces, and broken or partial sequences.
		"Mai" + nnbsp + "2026", "Mai" + nbsp + nnbsp + " \t2026", nbsp + "Mai" + nbsp + "2026" + nbsp,
		"Mai" + nbsp + "2026x", "xMai" + nbsp + "2026", "Mai" + nbsp + "20261", "Mai" + nbsp, nbsp + "2026",
		"2026-03-29" + nnbsp + "14:30", "2026-03-29T" + nbsp + "14:30:00Z", "2026-03-29" + nbsp + "T" + nbsp + "14:30",
		"2026-03-29" + nbsp + "14:30" + nbsp + "x", "2026-03-29" + nbsp + "14:2", "2026-03-29" + nbsp + nbsp,
		"29.03.2026" + nnbsp + "14:30", "29.03.2026" + nbsp + "14:30x", "01.01.2026" + nbsp + "12:15.03.2026",
		"29.03.2026" + nbsp + "T14:30", "29.03.2026" + nbsp,
		"Mai\xa02026", "Mai\xc22026", "Mai\xc2 2026", "Mai\xe2\x80 2026", "Mai\xe2\x802026", "Mai\xe2\x80\xa02026",
		"Mai\xa0\xc2\xa02026", "Mai\xc2\xc2\xa02026", "Mai\xe2\xc2\xa02026", "Mai\xe2\x80\xaf\xaf2026",
		"Mai\xf0\x9f\xc2\xa02026", "M" + umlautA + "rz" + nbsp + "2026", "Ma" + umlautA + nbsp + "2026",
		"2026-03-29\xc2" + "14:30", "2026-03-29\xa014:30", "2026-03-29\xe2\x80\xaf\x8014:30",
		"Mai\u20092026", "Mai\u20072026", "Mai\u30002026", "2026-03-29\u200914:30",
		"Mai" + strings.Repeat(nbsp, 2000) + "2026", "2026-03-29" + strings.Repeat(nnbsp+"T", 1000) + "14:30",
		// The years 2031-2099 (#437/3), the bounds of the window and the Go
		// reference layout.
		"2031-01-01 2039-12-31T23:59:59Z 2040-06-15 08:00 2099-12-31 2100-01-01 2019-12-31",
		"15.06.2040 08:00 31.12.2099 01.01.2100 31.12.2019 29.02.2096 29.02.2100", "Dezember 2099 Januar 2100 May 2045",
		"2099-12-31T23:59:59Z2100-01-01", "01.01.2045 12:15.03.2026", "Mai 2045Mai 2046", "2006-01-02 15:04:05",
		"Mai 2031", "Mai" + nbsp + "2031", "Maerz 2031", "Maerz" + nnbsp + "2031", "2031-01-01" + nbsp + "12:00",
		// Mixed, dense.
		"Stand 21.11.2026 01:51, Release v5.71.0 am 2026-11-21T01:51:00Z; geplant: Dezember 2026 / December 2026.",
		"| 2026-01-01 | 01.01.2026 | Januar 2026 | January 2026 |\n| 2026-01-01 00:00 | 01.01.2026 00:00 |",
	}
	// Long runs: whitespace between month and year, a long line before a
	// date, and a text that is nothing but year tokens.
	seeds = append(seeds,
		"März"+strings.Repeat(" ", 5000)+"2026",
		"2026-03-29"+strings.Repeat("T", 3000)+"14:30",
		"29.03.2026"+strings.Repeat("\t", 3000)+"14:30x",
		strings.Repeat("a", 20000)+" März 2026",
		strings.Repeat("2020", 2000),
		strings.Repeat("2026-", 1000),
		strings.Repeat("01.", 1000)+"2026",
		strings.Repeat("Mai ", 1000)+"2026",
	)
	return seeds
}

// assertMatchesReference fails t when ExtractDates and the reference of the
// current extraction disagree on content, or when the scanner's matches of
// one date form differ from what the form's reference expression finds
// (offsets of the match and of every group, FindAllStringSubmatchIndex). The
// second check sees a difference the deduplication and sorting of
// ExtractDates would hide. A content outside issue437Classes is held to the
// frozen version refFormsV434 the same way.
func assertMatchesReference(t testing.TB, content string) {
	t.Helper()
	assertMatchesForms(t, "reference", refForms, content)
	if issue437Touched(content) == "" {
		assertMatchesForms(t, "frozen #434 reference", refFormsV434, content)
	}
}

func assertMatchesForms(t testing.TB, label string, f refDateForms, content string) {
	t.Helper()
	got := ExtractDates(content)
	want := extractDatesRefWith(f, content)
	if (got == nil) != (want == nil) || !slices.Equal(got, want) {
		t.Fatalf("ExtractDates differs from the %s for %s:\n got %s\nwant %s",
			label, quoteHead(content), formatTimes(got), formatTimes(want))
	}
	sc := scanDates(content)
	for _, form := range []struct {
		name string
		got  [][]int
		re   *regexp.Regexp
	}{
		{"iso", sc.iso, f.iso},
		{"dot", sc.dot, f.dot},
		{"de", sc.de, f.de},
		{"en", sc.en, f.en},
	} {
		if want := form.re.FindAllStringSubmatchIndex(content, -1); !reflect.DeepEqual(form.got, want) {
			t.Fatalf("%s matches differ from the %s for %s:\n got %v\nwant %v",
				form.name, label, quoteHead(content), form.got, want)
		}
	}
}

// TestIssue437Classes_EachIsNeeded shows that every class of issue437Classes
// is needed: its witness belongs to it, belongs to no class listed before it,
// and gives a different result under the frozen version than now.
func TestIssue437Classes_EachIsNeeded(t *testing.T) {
	for i, c := range issue437Classes {
		if !c.touches(c.witness) {
			t.Errorf("%s: its witness %q is outside the class", c.name, c.witness)
		}
		for _, before := range issue437Classes[:i] {
			if before.touches(c.witness) {
				t.Errorf("%s: its witness %q already belongs to %s", c.name, c.witness, before.name)
			}
		}
		now, frozen := ExtractDates(c.witness), extractDatesRefWith(refFormsV434, c.witness)
		if slices.Equal(now, frozen) {
			t.Errorf("%s: its witness %q gives %s, as the frozen version does", c.name, c.witness, formatTimes(now))
		}
	}
}

// TestIssue437Classes_LeaveOrdinaryContentsOutside guards the frozen
// comparison against a class that has grown too wide: dates of every form in
// the old window with ASCII whitespace, and numbers that are no dates, belong
// to no class.
func TestIssue437Classes_LeaveOrdinaryContentsOutside(t *testing.T) {
	for _, c := range []string{
		"Mai 2026", "März 2026", "MÄRZ 2030", "December 2030", "Januar 2020", "2026-03-29", "2026-03-29 14:30",
		"2026-03-29T14:30:00Z", "29.03.2026", "29.03.2026 14:30", "Dezember\t\n2026", "August 2026", "SEPTEMBER 2030",
		"Stand 21.11.2026 01:51, Release v5.71.0 am 2026-11-21T01:51:00Z; geplant: Dezember 2026 / December 2026.",
		"Port 2049, 2048 MB, RFC 2046, 20 2031 Zeilen, ids 2040-2045, offset 2099, v2.0.2045",
		"5" + nbsp + "000 €", "um 14" + nbsp + "Uhr", "Seite" + nbsp + "2049", "Mai" + nbsp + "1999", "",
	} {
		if cl := issue437Touched(c); cl != "" {
			t.Errorf("%q is in class %s", c, cl)
		}
	}
}

// TestExtractDates_MatchesFrozenReference_OutsideTheClasses runs generated
// texts built like trickyDoc's but kept outside every class of
// issue437Classes, so the frozen comparison runs on each of them; the
// tricky corpus itself touches the classes often (its pieces are made of
// month names and year tokens). Enough of them must yield a date for the
// comparison to mean something. With -short it runs a quarter of the texts.
func TestExtractDates_MatchesFrozenReference_OutsideTheClasses(t *testing.T) {
	small, large := 3000, 60
	if testing.Short() {
		small, large = 750, 15
	}
	r := rand.New(rand.NewPCG(437, 1))
	withDates := 0
	for range small {
		c := trickyDocOutside437(r, 1+r.IntN(40))
		assertMatchesFrozen(t, c)
		if len(ExtractDates(c)) > 0 {
			withDates++
		}
	}
	for range large {
		assertMatchesFrozen(t, trickyDocOutside437(r, 200+r.IntN(800)))
	}
	if withDates*10 < small {
		t.Errorf("only %d of %d generated texts outside the classes yield a date", withDates, small)
	}
}

// TestExtractDates_MatchesFrozenReference_RealShapedTexts runs the documents
// of TestExtractDates_MatchesReference_RealShapedTexts against the frozen
// version as well, rewritten by outside437 so that no class touches them.
func TestExtractDates_MatchesFrozenReference_RealShapedTexts(t *testing.T) {
	seeds := uint64(4)
	if testing.Short() {
		seeds = 1
	}
	for _, dense := range []bool{false, true} {
		for _, n := range []int{2 << 10, 20 << 10, 50 << 10} {
			for seed := uint64(1); seed <= seeds; seed++ {
				assertMatchesFrozen(t, outside437(datesDoc(seed, n, dense)))
			}
		}
	}
}

var (
	outside437Years  = regexp.MustCompile(`20(?:3[1-9]|[4-9]\d)`)
	outside437Maerz  = regexp.MustCompile(`(?i)mae?rz`)
	outside437Spaces = strings.NewReplacer(nbsp, " ", nnbsp, " ")
)

// outside437 rewrites content out of every class of issue437Classes: the
// years 2031-2099 become 2029, "maerz" and "marz" become "März", and the
// no-break spaces become spaces.
func outside437(content string) string {
	content = outside437Years.ReplaceAllString(content, "2029")
	content = outside437Maerz.ReplaceAllString(content, "März")
	return outside437Spaces.Replace(content)
}

func assertMatchesFrozen(t *testing.T, content string) {
	t.Helper()
	if c := issue437Touched(content); c != "" {
		t.Fatalf("generated content %s is in class %s", quoteHead(content), c)
	}
	assertMatchesReference(t, content)
}

// trickyDocOutside437 builds a trickyDoc of up to k pieces and leaves out
// every piece that would put the text into a class of issue437Classes. A
// piece can only complete a class match that ends in it, and every class
// match ends with a digit or a colon, so only such pieces are checked, on
// the last 128 bytes, which hold every match the pieces form short of a run
// of over 60 whitespace pieces; assertMatchesFrozen checks the whole text
// again and fails if one slipped through.
func trickyDocOutside437(r *rand.Rand, k int) string {
	const tail = 128
	var b strings.Builder
	for range k {
		before := b.Len()
		piece := trickyDoc(r, 1)
		b.WriteString(piece)
		s := b.String()
		if strings.ContainsAny(piece, "0123456789:") && issue437Touched(s[max(0, len(s)-tail):]) != "" {
			b.Reset()
			b.WriteString(s[:before])
		}
	}
	return b.String()
}

func quoteHead(s string) string {
	const limit = 300
	if len(s) > limit {
		return strconv.QuoteToASCII(s[:limit]) + "... (" + strconv.Itoa(len(s)) + " bytes)"
	}
	return strconv.QuoteToASCII(s)
}

func formatTimes(ts []time.Time) string {
	if ts == nil {
		return "nil"
	}
	parts := make([]string, len(ts))
	for i, t := range ts {
		parts[i] = t.Format(time.RFC3339Nano) + "@" + t.Location().String()
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func TestExtractDates_MatchesReference_SeedCorpus(t *testing.T) {
	for _, s := range datesSeedCorpus() {
		assertMatchesReference(t, s)
	}
}

// TestExtractDates_MatchesReference_RealShapedTexts runs the generated
// documents of the benchmark (German and English prose, Markdown, code
// blocks, tables; few and many dates) at every benchmark size. With -short
// (the race runs, where the reference is slow) it runs one document per size
// below 400 KB.
func TestExtractDates_MatchesReference_RealShapedTexts(t *testing.T) {
	seeds := uint64(4)
	if testing.Short() {
		seeds = 1
	}
	for _, dense := range []bool{false, true} {
		for _, n := range []int{2 << 10, 20 << 10, 50 << 10} {
			for seed := uint64(1); seed <= seeds; seed++ {
				assertMatchesReference(t, datesDoc(seed, n, dense))
			}
		}
		if !testing.Short() {
			assertMatchesReference(t, datesDoc(1, 400<<10, dense))
		}
	}
}

// TestExtractDates_MatchesReference_TrickyTexts runs generated texts built
// from the pieces the patterns distinguish: month names in random case and
// fold variants, year tokens in and out of range, two-digit fields, date
// shapes with and without time, and separators that are and are not \s or
// word characters, including invalid UTF-8. With -short it runs a quarter
// of the texts.
func TestExtractDates_MatchesReference_TrickyTexts(t *testing.T) {
	small, large := 6000, 150
	if testing.Short() {
		small, large = 1500, 40
	}
	r := rand.New(rand.NewPCG(434, 1))
	for range small {
		assertMatchesReference(t, trickyDoc(r, 1+r.IntN(40)))
	}
	for range large {
		assertMatchesReference(t, trickyDoc(r, 200+r.IntN(800)))
	}
}

func FuzzExtractDates(f *testing.F) {
	for _, s := range datesSeedCorpus() {
		f.Add(s)
	}
	r := rand.New(rand.NewPCG(434, 2))
	for range 40 {
		f.Add(trickyDoc(r, 1+r.IntN(30)))
	}
	f.Add(datesDoc(7, 2<<10, true))
	f.Fuzz(func(t *testing.T, content string) {
		assertMatchesReference(t, content)
	})
}

// TestMonthPatterns_FoldLikeRegexp checks the month tables of the scanner
// against the alternations of the reference expressions: the same
// alternatives in the same order, and at every position exactly the runes the
// compiled (?i) atom of the expression matches, over all of Unicode.
func TestMonthPatterns_FoldLikeRegexp(t *testing.T) {
	matchers := map[string][]rune{}
	for _, form := range []struct {
		re       *regexp.Regexp
		patterns []monthPattern
	}{{refForms.de, deMonthPatterns}, {refForms.en, enMonthPatterns}} {
		src := form.re.String()
		start := strings.Index(src, `\b(`) + 3
		alts := strings.Split(src[start:start+strings.IndexByte(src[start:], ')')], "|")
		if len(alts) != len(form.patterns) {
			t.Fatalf("%d alternatives in %s, %d month patterns", len(alts), src, len(form.patterns))
		}
		for i, alt := range alts {
			atoms := regexpAtoms(alt)
			if len(atoms) != len(form.patterns[i]) {
				t.Fatalf("alternative %q has %d atoms, pattern %d has %d positions", alt, len(atoms), i, len(form.patterns[i]))
			}
			for j, atom := range atoms {
				want, ok := matchers[atom]
				if !ok {
					want = runesMatchedBy(t, "(?i)"+atom)
					matchers[atom] = want
				}
				got := slices.Clone(form.patterns[i][j])
				slices.Sort(got)
				if !slices.Equal(got, want) {
					t.Errorf("alternative %q position %d (%s): pattern runes %q, regexp matches %q", alt, j, atom, got, want)
				}
			}
		}
	}
}

// regexpAtoms splits a month alternative of the reference expressions into
// its atoms: single letters and bracket classes.
func regexpAtoms(alt string) []string {
	var atoms []string
	for i := 0; i < len(alt); {
		if alt[i] == '[' {
			j := strings.IndexByte(alt[i:], ']')
			atoms = append(atoms, alt[i:i+j+1])
			i += j + 1
			continue
		}
		_, size := utf8.DecodeRuneInString(alt[i:])
		atoms = append(atoms, alt[i:i+size])
		i += size
	}
	return atoms
}

// runesMatchedBy returns, sorted, every rune the single rune instruction of
// the compiled expression matches.
func runesMatchedBy(t *testing.T, expr string) []rune {
	t.Helper()
	re, err := syntax.Parse(expr, syntax.Perl)
	if err != nil {
		t.Fatalf("parse %s: %v", expr, err)
	}
	prog, err := syntax.Compile(re.Simplify())
	if err != nil {
		t.Fatalf("compile %s: %v", expr, err)
	}
	var inst *syntax.Inst
	for i := range prog.Inst {
		switch prog.Inst[i].Op {
		case syntax.InstRune, syntax.InstRune1, syntax.InstRuneAny, syntax.InstRuneAnyNotNL:
			if inst != nil {
				t.Fatalf("%s compiles to more than one rune instruction", expr)
			}
			inst = &prog.Inst[i]
		}
	}
	if inst == nil {
		t.Fatalf("%s compiles to no rune instruction", expr)
	}
	var out []rune
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if inst.MatchRune(r) {
			out = append(out, r)
		}
	}
	return out
}

// TestMonthPatterns_AreTheTableKeys pins that the month patterns and the
// tables ExtractDates resolves their matches with name the same months: the
// spelling each pattern was built from (the first rune of every fold orbit)
// is a key of the table, every key has its pattern, and every spelling the
// pattern matches — any rune of the orbit at every position — resolves
// through monthKey to that key. Issue #437: m[aä]rz matched "Marz", absent
// from util.MonthDE, and missed its "maerz". Issue #451: "Auguſt" matched
// and strings.ToLower kept the long s, so no key resolved it.
func TestMonthPatterns_AreTheTableKeys(t *testing.T) {
	for _, form := range []struct {
		name     string
		patterns []monthPattern
		table    map[string]time.Month
	}{{"German", deMonthPatterns, util.MonthDE}, {"English", enMonthPatterns, util.MonthEN}} {
		var names, keys []string
		for _, p := range form.patterns {
			var b strings.Builder
			for _, orbit := range p {
				b.WriteRune(orbit[0])
			}
			name := b.String()
			names = append(names, name)
			for _, spelling := range monthSpellings(p) {
				if key := monthKey(spelling); key != name {
					t.Errorf("%s month pattern %q matches %q, which monthKey makes %q", form.name, name, spelling, key)
				}
			}
		}
		for k := range form.table {
			keys = append(keys, k)
		}
		slices.Sort(names)
		slices.Sort(keys)
		if !slices.Equal(names, keys) {
			t.Errorf("%s month patterns %q, table keys %q", form.name, names, keys)
		}
	}
}

// TestMonthPatterns_NoAlternativeEndsAnother pins the assumption of
// matchMonthYear: no alternative of a month form can match the end of
// another one, so matching backwards from the end of the month name finds at
// most one alternative, the one the regexp's leftmost-first search finds.
func TestMonthPatterns_NoAlternativeEndsAnother(t *testing.T) {
	for _, patterns := range [][]monthPattern{deMonthPatterns, enMonthPatterns} {
		for i, a := range patterns {
			for j, b := range patterns {
				if i == j || len(b) > len(a) {
					continue
				}
				overlap := true
				for k := range b {
					if !slices.ContainsFunc(b[k], func(r rune) bool { return slices.Contains(a[len(a)-len(b)+k], r) }) {
						overlap = false
						break
					}
				}
				if overlap {
					t.Errorf("month pattern %d can match the end of month pattern %d", j, i)
				}
			}
		}
	}
}

// TestMonthForms_ListEveryAlternativeByItsLastLetter checks the dispatch of
// matchMonthYear over all of Unicode: for every rune, endingWith returns
// exactly the alternatives whose last position admits it, in the order of the
// form. matchBefore checks that position first, so the dispatch leaves out
// only alternatives that cannot match.
func TestMonthForms_ListEveryAlternativeByItsLastLetter(t *testing.T) {
	for _, form := range []struct {
		name     string
		patterns []monthPattern
		form     *monthForm
	}{{"German", deMonthPatterns, deMonthForm}, {"English", enMonthPatterns, enMonthForm}} {
		listed := 0
		for r := rune(0); r <= unicode.MaxRune; r++ {
			var want []monthPattern
			for _, p := range form.patterns {
				if slices.Contains(p[len(p)-1], r) {
					want = append(want, p)
				}
			}
			if got := form.form.endingWith(r); !reflect.DeepEqual(got, want) {
				t.Fatalf("%s: endingWith(%q) = %q, want %q", form.name, r, got, want)
			}
			if want != nil {
				listed++
			}
		}
		if listed == 0 {
			t.Fatalf("%s: no rune lists an alternative", form.name)
		}
	}
}

// TestScanDates_EveryMonthSpellingMatches runs every spelling a month pattern
// matches — the product of the fold orbits of its letters — before a year,
// behind runs of date spaces of every kind, through the scanner: each one is
// the one match of its form, at the offsets of the name and the year, and
// agrees with the reference. The dispatch of matchMonthYear by the rune
// before the date spaces lets every one of them through.
func TestScanDates_EveryMonthSpellingMatches(t *testing.T) {
	for _, form := range []struct {
		name     string
		patterns []monthPattern
		matches  func(dateScan) [][]int
	}{
		{"German", deMonthPatterns, func(sc dateScan) [][]int { return sc.de }},
		{"English", enMonthPatterns, func(sc dateScan) [][]int { return sc.en }},
	} {
		n := 0
		for _, p := range form.patterns {
			for _, name := range monthSpellings(p) {
				// \b is ASCII: a name that starts with a rune beyond ASCII
				// starts a match only behind a word character.
				before := "am "
				if name[0] >= utf8.RuneSelf {
					before = "am x"
				}
				for _, sep := range []string{" ", "  \t\n", nbsp, " " + nbsp + nnbsp + "\r", nnbsp} {
					c := before + name + sep + "2026."
					s, y := len(before), len(c)-5
					want := [][]int{{s, y + 4, s, s + len(name), y, y + 4}}
					if got := form.matches(scanDates(c)); !reflect.DeepEqual(got, want) {
						t.Fatalf("%s matches of %s: got %v, want %v", form.name, quoteHead(c), got, want)
					}
					assertMatchesReference(t, c)
					n++
				}
			}
		}
		t.Logf("%s: %d contents", form.name, n)
	}
}

// monthSpellings returns every spelling p matches: one rune of each position.
func monthSpellings(p monthPattern) []string {
	out := []string{""}
	for _, orbit := range p {
		next := make([]string, 0, len(out)*len(orbit))
		for _, s := range out {
			for _, r := range orbit {
				next = append(next, s+string(r))
			}
		}
		out = next
	}
	return out
}

// Generators of the differential and benchmark corpora.

var (
	datesDEWords = strings.Fields("der die das und oder nicht mit für über unter Prüfung Übergabe Größe " +
		"Schlüssel Welle Datenbank Abfrage läuft gebaut geprüft Blöcke Mandant Bereich Ergebnis Messung " +
		"Fehler Ursache Lösung Änderung heute gestern morgen seit bis am im vom zum Stand Freigabe " +
		"Mai Juni März April August September Oktober Dezember Januar Woche Monat Jahr Zeile Spalte " +
		"Schreibpfad Nacharbeit Antwort gemessen verworfen übernommen ändert")
	datesENWords = strings.Fields("the and or not with for over under review handover size key wave " +
		"database query runs built checked blocks tenant scope result measurement error cause fix change " +
		"today yesterday since until on in from to state release deploy May June March April August " +
		"September October December January index cache vector embedding write path afterwork answer")
	datesCodeLines = []string{
		"func handle(ctx context.Context, id string) error { return nil }",
		"\tif err != nil { return fmt.Errorf(\"store: %w\", err) }",
		"SELECT id, title FROM context_blocks WHERE updated_at > '%s' LIMIT 20;",
		"{\"id\": \"01a1fbcf-481e-7f02-8ad0-e0bba7b7cfcf\", \"n\": 2048, \"ts\": \"%s\"}",
		"x := 0x2026 + 202 // offset %s",
		"docker compose -p n8n build ctx && curl -s http://127.0.0.1:8080/health",
		"const maxContent = 50 << 10 // 51200 bytes, %s",
		"git log --oneline origin/root..HEAD | head -20",
	}
	datesSeps = []string{
		" ", " ", " ", "  ", "\t", "\n", "\v", "\f", "\r", nbsp, "", "", "-", ".", ":", "T", "Z", "_",
		"x", "K", umlautA, "\xff", "\xc3", ",", "/", "1", "2", nnbsp, "\xc2", "\xa0", "\xe2\x80",
	}
	datesMonthWords = strings.Fields("januar februar märz maerz marz april mai juni juli august " +
		"september oktober november dezember january february march may june july october december")
)

// datesDate returns one date-shaped token: valid and invalid ISO and dot
// dates with and without time, month+year in German and English, and the
// near misses real texts carry (dates without year, versions, ids).
func datesDate(r *rand.Rand) string {
	year := 2018 + r.IntN(15)
	mo, d := 1+r.IntN(12), 1+r.IntN(31)
	h, mi, s := r.IntN(25), r.IntN(61), r.IntN(61)
	switch r.IntN(12) {
	case 0:
		return pad4(year) + "-" + pad2(mo) + "-" + pad2(d)
	case 1:
		return pad4(year) + "-" + pad2(mo) + "-" + pad2(d) + " " + pad2(h) + ":" + pad2(mi)
	case 2:
		return pad4(year) + "-" + pad2(mo) + "-" + pad2(d) + "T" + pad2(h) + ":" + pad2(mi) + ":" + pad2(s) + "Z"
	case 3:
		return pad2(d) + "." + pad2(mo) + "." + pad4(year)
	case 4:
		return pad2(d) + "." + pad2(mo) + "." + pad4(year) + " " + pad2(h) + ":" + pad2(mi)
	case 5:
		return datesMonthWords[r.IntN(len(datesMonthWords))] + " " + pad4(year)
	case 6:
		w := datesMonthWords[r.IntN(len(datesMonthWords))]
		return strings.ToUpper(w[:1]) + w[1:] + " " + pad4(year)
	case 7:
		return pad2(d) + "." + pad2(mo) + "."
	case 8:
		return "v5." + strconv.Itoa(r.IntN(80)) + "." + strconv.Itoa(r.IntN(5))
	case 9:
		return "01a1" + pad4(r.IntN(10000)) + "-" + pad4(r.IntN(10000))
	case 10:
		return strconv.Itoa(year) + strconv.Itoa(mo) + strconv.Itoa(d)
	default:
		return pad4(year) + "-" + pad2(mo)
	}
}

func pad2(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

func pad4(n int) string {
	s := strconv.Itoa(n)
	for len(s) < 4 {
		s = "0" + s
	}
	return s
}

// datesDoc returns a deterministic real-shaped content of n bytes (cut at a
// rune boundary): Markdown headings, paragraphs in German or English, bullet
// lists, fenced code blocks and tables. dense puts a date-shaped token into
// most sentences and code lines, otherwise into about one sentence in twelve.
func datesDoc(seed uint64, n int, dense bool) string {
	r := rand.New(rand.NewPCG(seed, 434))
	rate := 0.08
	if dense {
		rate = 0.8
	}
	sentence := func(b *strings.Builder) {
		words := datesDEWords
		if r.IntN(2) == 0 {
			words = datesENWords
		}
		k := 8 + r.IntN(12)
		at := -1
		if r.Float64() < rate {
			at = r.IntN(k)
		}
		for i := range k {
			if i > 0 {
				b.WriteByte(' ')
			}
			if i == at {
				b.WriteString(datesDate(r))
				continue
			}
			w := words[r.IntN(len(words))]
			if i == 0 {
				w = strings.ToUpper(w[:1]) + w[1:]
			}
			b.WriteString(w)
		}
		b.WriteString(". ")
	}
	var b strings.Builder
	for b.Len() < n {
		switch k := r.IntN(10); {
		case k == 0:
			b.WriteString("\n## ")
			sentence(&b)
			b.WriteString("\n\n")
		case k <= 2:
			for range 3 + r.IntN(4) {
				b.WriteString("- ")
				sentence(&b)
				b.WriteByte('\n')
			}
			b.WriteByte('\n')
		case k == 3:
			b.WriteString("```go\n")
			for range 3 + r.IntN(8) {
				line := datesCodeLines[r.IntN(len(datesCodeLines))]
				fill := pad4(r.IntN(10000))
				if r.Float64() < rate {
					fill = datesDate(r)
				}
				b.WriteString(strings.Replace(line, "%s", fill, 1))
				b.WriteByte('\n')
			}
			b.WriteString("```\n\n")
		case k == 4:
			b.WriteString("| Schritt | Stand | Notiz |\n|---|---|---|\n")
			for range 2 + r.IntN(5) {
				b.WriteString("| ")
				b.WriteString(datesDEWords[r.IntN(len(datesDEWords))])
				b.WriteString(" | ")
				if r.Float64() < rate {
					b.WriteString(datesDate(r))
				} else {
					b.WriteString(strconv.Itoa(r.IntN(500)) + " ms")
				}
				b.WriteString(" | ")
				b.WriteString(datesENWords[r.IntN(len(datesENWords))])
				b.WriteString(" |\n")
			}
			b.WriteByte('\n')
		default:
			for range 2 + r.IntN(5) {
				sentence(&b)
			}
			b.WriteString("\n\n")
		}
	}
	s := b.String()
	cut := n
	for cut > 0 && cut < len(s) && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut]
}

// trickyDoc concatenates k pieces the patterns distinguish, with random case
// per letter and random fold variants (Kelvin sign for k, long s for s,
// Ä for ä).
func trickyDoc(r *rand.Rand, k int) string {
	var b strings.Builder
	for range k {
		switch r.IntN(8) {
		case 0, 1:
			b.WriteString(foldNoise(r, datesMonthWords[r.IntN(len(datesMonthWords))]))
		case 2:
			b.WriteString("20" + strconv.Itoa(r.IntN(10)) + strconv.Itoa(r.IntN(10)))
		case 3:
			b.WriteString(strconv.Itoa(r.IntN(4)) + strconv.Itoa(r.IntN(10)))
		case 4:
			b.WriteString(datesDate(r))
		case 5:
			b.WriteString(foldNoise(r, datesDEWords[r.IntN(len(datesDEWords))]))
		default:
			b.WriteString(datesSeps[r.IntN(len(datesSeps))])
		}
	}
	return b.String()
}

func foldNoise(r *rand.Rand, w string) string {
	var b strings.Builder
	for _, c := range w {
		switch {
		case c == 'k' && r.IntN(4) == 0:
			b.WriteString(kelvinSign)
		case c == 's' && r.IntN(4) == 0:
			b.WriteString(longS)
		case c == 'ä' && r.IntN(2) == 0:
			b.WriteString(umlautCapA)
		case r.IntN(3) == 0:
			b.WriteString(strings.ToUpper(string(c)))
		default:
			b.WriteRune(c)
		}
	}
	return b.String()
}
