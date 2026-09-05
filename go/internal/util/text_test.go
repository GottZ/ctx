package util

import (
	"testing"
	"time"
)

// TestPrimaryLanguageSubtag pins the normalization the language surfaces ride
// on. Moved here from internal/dream with T04-5 (it was
// TestReportLanguagePrimarySubtag): the reduction is one function now, and the
// test belongs where the function is. The dream and topiclabel suites keep
// their own tests for what they do WITH the subtag.
func TestPrimaryLanguageSubtag(t *testing.T) {
	cases := map[string]string{
		"":            "",
		"  ":          "",
		"de":          "de",
		"DE":          "de",
		" de-DE ":     "de",
		"zh-Hant-TW":  "zh",
		"pt-BR":       "pt",
		"en":          "en",
		"haw-us-x-yz": "haw",
		"-":           "",
		"-de":         "",
		"de-":         "de",
	}
	for in, want := range cases {
		if got := PrimaryLanguageSubtag(in); got != want {
			t.Errorf("PrimaryLanguageSubtag(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCollapseSpace pins the comparison form two packages share: every run of
// unicode.IsSpace becomes ONE U+0020 and edge runs vanish entirely.
func TestCollapseSpace(t *testing.T) {
	cases := map[string]string{
		"":                      "",
		" ":                     "",
		"\t":                    "",
		"\n\n\n":                "",
		"x":                     "x",
		"a b":                   "a b",
		"a  b":                  "a b",
		"a\t\tb":                "a b",
		"a \n\t b":              "a b",
		"a\rb":                  "a b",
		"a\vb":                  "a b",
		"a\fb":                  "a b",
		"  leading":             "leading",
		"trailing  ":            "trailing",
		"  both  ":              "both",
		"multi   word   string": "multi word string",
	}
	for in, want := range cases {
		if got := CollapseSpace(in); got != want {
			t.Errorf("CollapseSpace(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCollapseSpaceNonASCII is the reason this is unicode.IsSpace and not a
// byte test. NBSP, NEL, EM SPACE and IDEOGRAPHIC SPACE are what a paste out of
// a PDF or a CJK source carries, and a comparison form that leaves them intact
// reports two identical texts as different. ZERO WIDTH SPACE is the negative
// case: it is a joiner, not a separator, and must survive untouched.
func TestCollapseSpaceNonASCII(t *testing.T) {
	// Spelled as codepoints: these characters are invisible in a source
	// listing, and a literal would be unreviewable.
	around := func(r rune) string { return "a" + string(r) + "b" }
	for _, sep := range []rune{
		0x00A0, // NO-BREAK SPACE
		0x0085, // NEXT LINE
		0x2003, // EM SPACE
		0x3000, // IDEOGRAPHIC SPACE
	} {
		if got := CollapseSpace(around(sep)); got != "a b" {
			t.Errorf("CollapseSpace(a U+%04X b) = %q, want %q", sep, got, "a b")
		}
	}
	// ZERO WIDTH SPACE is NOT unicode.IsSpace and must survive untouched.
	if zwsp := around(0x200B); CollapseSpace(zwsp) != zwsp {
		t.Errorf("CollapseSpace(a U+200B b) = %q, want it unchanged", CollapseSpace(zwsp))
	}
	if got := CollapseSpace("ä  ö"); got != "ä ö" {
		t.Errorf("CollapseSpace(%q) = %q, want %q", "ä  ö", got, "ä ö")
	}
}

// TestMonthTables pins both month tables completely: the LLM temporal rules and
// the store date extraction read THESE maps, and a dropped or wrong entry is a
// silently mis-dated block rather than a failure.
func TestMonthTables(t *testing.T) {
	wantDE := map[string]time.Month{
		"januar": time.January, "februar": time.February, "märz": time.March,
		"maerz": time.March, "april": time.April, "mai": time.May,
		"juni": time.June, "juli": time.July, "august": time.August,
		"september": time.September, "oktober": time.October,
		"november": time.November, "dezember": time.December,
	}
	wantEN := map[string]time.Month{
		"january": time.January, "february": time.February, "march": time.March,
		"april": time.April, "may": time.May, "june": time.June,
		"july": time.July, "august": time.August, "september": time.September,
		"october": time.October, "november": time.November, "december": time.December,
	}
	check := func(label string, got, want map[string]time.Month) {
		if len(got) != len(want) {
			t.Errorf("%s has %d entries, want %d", label, len(got), len(want))
		}
		for k, w := range want {
			if g, ok := got[k]; !ok || g != w {
				t.Errorf("%s[%q] = %v, %v; want %v, true", label, k, g, ok, w)
			}
		}
		for k := range got {
			if _, ok := want[k]; !ok {
				t.Errorf("%s carries unexpected key %q", label, k)
			}
		}
	}
	check("MonthDE", MonthDE, wantDE)
	check("MonthEN", MonthEN, wantEN)

	// The four keys spelled identically in both languages must agree, because
	// lookupMonth in internal/llm consults German first and the order must not
	// decide anything.
	for _, k := range []string{"april", "august", "november", "september"} {
		de, okDE := MonthDE[k]
		en, okEN := MonthEN[k]
		if !okDE || !okEN || de != en {
			t.Errorf("shared key %q: DE=%v,%v EN=%v,%v — the tables disagree", k, de, okDE, en, okEN)
		}
	}

	// Misses stay misses: the tables are lowercase-only, and the callers
	// lowercase before the lookup.
	for _, k := range []string{"März", "MAI", "January", "", "foo", "mrz"} {
		if _, ok := MonthDE[k]; ok {
			t.Errorf("MonthDE must not match %q", k)
		}
		if _, ok := MonthEN[k]; ok {
			t.Errorf("MonthEN must not match %q", k)
		}
	}
}
