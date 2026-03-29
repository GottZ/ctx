package rrf

import (
	"strings"
	"testing"
	"time"
)

// Fixed reference time: Wednesday, 2026-03-25 15:30:00 UTC
var pessimistRef = time.Date(2026, 3, 25, 15, 30, 0, 0, time.UTC)

// ─── levenshtein ────────────────────────────────────────────────────────────

func TestPessimist_Levenshtein_BothEmpty_ReturnsZeroNotPanic(t *testing.T) {
	if d := levenshtein("", ""); d != 0 {
		t.Fatalf("empty vs empty: got %d, want 0", d)
	}
}

func TestPessimist_Levenshtein_OneEmpty_ReturnsLengthOfOther(t *testing.T) {
	if d := levenshtein("", "abc"); d != 3 {
		t.Fatalf("empty vs 'abc': got %d, want 3", d)
	}
	if d := levenshtein("hello", ""); d != 5 {
		t.Fatalf("'hello' vs empty: got %d, want 5", d)
	}
}

func TestPessimist_Levenshtein_IdenticalStrings_AlwaysZero(t *testing.T) {
	for _, s := range []string{"a", "test", "Übermorgen", "日本語"} {
		if d := levenshtein(s, s); d != 0 {
			t.Fatalf("identical %q: got %d, want 0", s, d)
		}
	}
}

func TestPessimist_Levenshtein_SingleCharStrings_InsertDeleteSubstitute(t *testing.T) {
	if d := levenshtein("a", "b"); d != 1 {
		t.Fatalf("'a' vs 'b': got %d, want 1 (substitution)", d)
	}
	if d := levenshtein("a", "ab"); d != 1 {
		t.Fatalf("'a' vs 'ab': got %d, want 1 (insertion)", d)
	}
	if d := levenshtein("ab", "a"); d != 1 {
		t.Fatalf("'ab' vs 'a': got %d, want 1 (deletion)", d)
	}
}

func TestPessimist_Levenshtein_UnicodeUmlauts_RuneNotByteDistance(t *testing.T) {
	// "ä" and "a" differ by one rune, not by byte count
	if d := levenshtein("ä", "a"); d != 1 {
		t.Fatalf("'ä' vs 'a': got %d, want 1 — may be counting bytes not runes", d)
	}
	// "über" and "uber" differ by one rune substitution (ü→u)
	if d := levenshtein("über", "uber"); d != 1 {
		t.Fatalf("'über' vs 'uber': got %d, want 1", d)
	}
	// "straße" and "strasse" — ß→ss is 1 sub + 1 insert = 2
	if d := levenshtein("straße", "strasse"); d != 2 {
		t.Fatalf("'straße' vs 'strasse': got %d, want 2", d)
	}
}

func TestPessimist_Levenshtein_VeryLongStrings_DoesNotOOMOrHang(t *testing.T) {
	a := strings.Repeat("a", 1000)
	b := strings.Repeat("b", 1000)
	d := levenshtein(a, b)
	if d != 1000 {
		t.Fatalf("1000 'a's vs 1000 'b's: got %d, want 1000", d)
	}

	// Asymmetric lengths — distance should be max(len) when completely different chars
	c := strings.Repeat("a", 1000)
	e := strings.Repeat("b", 500)
	d2 := levenshtein(c, e)
	if d2 != 1000 {
		t.Fatalf("1000 'a's vs 500 'b's: got %d, want 1000", d2)
	}
}

func TestPessimist_Levenshtein_WhitespaceOnly_TreatedAsNormalChars(t *testing.T) {
	if d := levenshtein("   ", "   "); d != 0 {
		t.Fatalf("3 spaces vs 3 spaces: got %d, want 0", d)
	}
	if d := levenshtein(" ", "  "); d != 1 {
		t.Fatalf("1 space vs 2 spaces: got %d, want 1", d)
	}
}

func TestPessimist_Levenshtein_KnownDistance_KittenSitting(t *testing.T) {
	if d := levenshtein("kitten", "sitting"); d != 3 {
		t.Fatalf("'kitten' vs 'sitting': got %d, want 3", d)
	}
}

func TestPessimist_Levenshtein_CaseInsensitive_UpperLowerSame(t *testing.T) {
	if d := levenshtein("HELLO", "hello"); d != 0 {
		t.Fatalf("'HELLO' vs 'hello': got %d, want 0 — case should be folded", d)
	}
	if d := levenshtein("Heute", "heute"); d != 0 {
		t.Fatalf("'Heute' vs 'heute': got %d, want 0", d)
	}
}

// ─── maxEditDistance ────────────────────────────────────────────────────────

func TestPessimist_MaxEditDistance_EmptyString_ZeroTolerance(t *testing.T) {
	if d := maxEditDistance(""); d != 0 {
		t.Fatalf("empty string: got %d, want 0", d)
	}
}

func TestPessimist_MaxEditDistance_ShortWords_NoTypoTolerance(t *testing.T) {
	for _, w := range []string{"a", "ab", "abc"} {
		if d := maxEditDistance(w); d != 0 {
			t.Fatalf("%q (len %d): got %d, want 0 — short words must be exact", w, len(w), d)
		}
	}
}

func TestPessimist_MaxEditDistance_MediumWords_OneTypoAllowed(t *testing.T) {
	for _, w := range []string{"abcd", "abcde"} {
		if d := maxEditDistance(w); d != 1 {
			t.Fatalf("%q (len %d): got %d, want 1", w, len(w), d)
		}
	}
}

func TestPessimist_MaxEditDistance_LongWords_TwoTyposAllowed(t *testing.T) {
	for _, w := range []string{"abcdef", "abcdefghij", strings.Repeat("x", 100)} {
		if d := maxEditDistance(w); d != 2 {
			t.Fatalf("%q (len %d): got %d, want 2", w, len(w), d)
		}
	}
}

func TestPessimist_MaxEditDistance_UnicodeRuneCount_NotByteCount(t *testing.T) {
	// "über" = 4 runes, 5 bytes. If counting bytes, would get 1. Runes: should be 1 (4 runes → <=5 → 1).
	if d := maxEditDistance("über"); d != 1 {
		t.Fatalf("'über' (4 runes, 5 bytes): got %d, want 1 — must count runes not bytes", d)
	}
	// "ää" = 2 runes, 4 bytes. Byte-counting would give 1, rune-counting gives 0.
	if d := maxEditDistance("ää"); d != 0 {
		t.Fatalf("'ää' (2 runes, 4 bytes): got %d, want 0 — must count runes not bytes", d)
	}
	// "überall" = 7 runes, 8 bytes. Both give 2, but verify.
	if d := maxEditDistance("überall"); d != 2 {
		t.Fatalf("'überall' (7 runes): got %d, want 2", d)
	}
}

func TestPessimist_MaxEditDistance_BoundaryAt4_ExactlyFourRunesGetsOne(t *testing.T) {
	if d := maxEditDistance("abcd"); d != 1 {
		t.Fatalf("4-char word: got %d, want 1 (boundary at >3)", d)
	}
	if d := maxEditDistance("abc"); d != 0 {
		t.Fatalf("3-char word: got %d, want 0 (boundary at <=3)", d)
	}
}

func TestPessimist_MaxEditDistance_BoundaryAt6_ExactlySixRunesGetsTwo(t *testing.T) {
	if d := maxEditDistance("abcdef"); d != 2 {
		t.Fatalf("6-char word: got %d, want 2 (boundary at >5)", d)
	}
	if d := maxEditDistance("abcde"); d != 1 {
		t.Fatalf("5-char word: got %d, want 1 (boundary at <=5)", d)
	}
}

// ─── ExpandTemporal ─────────────────────────────────────────────────────────

func TestPessimist_ExpandTemporal_EmptyQuery_ReturnsEmptyNotPanic(t *testing.T) {
	result := ExpandTemporal("", pessimistRef)
	if result != "" {
		t.Fatalf("empty query: got %q, want empty", result)
	}
}

func TestPessimist_ExpandTemporal_NoTemporalWords_ReturnsEmpty(t *testing.T) {
	result := ExpandTemporal("wie funktioniert die Datenbank", pessimistRef)
	if result != "" {
		t.Fatalf("non-temporal query: got %q, want empty", result)
	}
}

func TestPessimist_ExpandTemporal_AllEightKeywords_ProduceCorrectDates(t *testing.T) {
	tests := []struct {
		keyword string
		wantDay int // day of March 2026
	}{
		{"heute", 25},
		{"today", 25},
		{"gestern", 24},
		{"yesterday", 24},
		{"vorgestern", 23},
		{"morgen", 26},
		{"tomorrow", 26},
		{"übermorgen", 27},
	}
	for _, tt := range tests {
		result := ExpandTemporal(tt.keyword, pessimistRef)
		wantDate := time.Date(2026, 3, tt.wantDay, 15, 30, 0, 0, time.UTC).Format("2006-01-02")
		if !strings.Contains(result, wantDate) {
			t.Errorf("%q: result %q does not contain expected date %s", tt.keyword, result, wantDate)
		}
		if result == "" {
			t.Errorf("%q: got empty result, expected expansion", tt.keyword)
		}
	}
}

func TestPessimist_ExpandTemporal_MultipleTemporalWords_AllDatesExpanded(t *testing.T) {
	result := ExpandTemporal("heute und morgen", pessimistRef)
	if !strings.Contains(result, "2026-03-25") {
		t.Errorf("missing heute date in %q", result)
	}
	if !strings.Contains(result, "2026-03-26") {
		t.Errorf("missing morgen date in %q", result)
	}
}

func TestPessimist_ExpandTemporal_DuplicateTemporalWords_NoDuplicateDates(t *testing.T) {
	// "heute" and "today" both resolve to same date — must not duplicate
	result := ExpandTemporal("heute is today", pessimistRef)
	count := strings.Count(result, "2026-03-25")
	if count != 1 {
		t.Fatalf("date appears %d times in %q, want exactly 1 (deduplication broken)", count, result)
	}
}

func TestPessimist_ExpandTemporal_TypoHeite_FuzzyMatchesHeute(t *testing.T) {
	// "heite" has levenshtein distance 1 from "heute" (5 chars → tolerance 1)
	result := ExpandTemporal("heite", pessimistRef)
	if !strings.Contains(result, "2026-03-25") {
		t.Fatalf("typo 'heite' not matched: got %q", result)
	}
}

func TestPessimist_ExpandTemporal_TypoGester_FuzzyMatchesGestern(t *testing.T) {
	// "gester" has levenshtein distance 1 from "gestern" (7 chars → tolerance 2)
	result := ExpandTemporal("gester", pessimistRef)
	if !strings.Contains(result, "2026-03-24") {
		t.Fatalf("typo 'gester' not matched: got %q", result)
	}
}

func TestPessimist_ExpandTemporal_TypoMorge_FuzzyMatchesMorgen(t *testing.T) {
	// "morge" has levenshtein distance 1 from "morgen" (6 chars → tolerance 2)
	result := ExpandTemporal("morge", pessimistRef)
	if !strings.Contains(result, "2026-03-26") {
		t.Fatalf("typo 'morge' not matched: got %q", result)
	}
}

func TestPessimist_ExpandTemporal_PunctuationStripped_StillMatches(t *testing.T) {
	tests := []struct {
		query    string
		wantDate string
	}{
		{"heute!", "2026-03-25"},
		{"morgen?", "2026-03-26"},
		{"gestern.", "2026-03-24"},
		{"(heute)", "2026-03-25"},
		{"\"morgen\"", "2026-03-26"},
		{"[gestern]", "2026-03-24"},
		{"{heute}", "2026-03-25"},
		// NOTE: "heute—morgen" is NOT split by strings.Fields (em-dash is not whitespace).
		// The trim set only strips leading/trailing punctuation, so the whole token
		// becomes "heute—morgen" which matches nothing. This is a known limitation.
	}
	for _, tt := range tests {
		result := ExpandTemporal(tt.query, pessimistRef)
		if !strings.Contains(result, tt.wantDate) {
			t.Errorf("query %q: got %q, missing date %s", tt.query, result, tt.wantDate)
		}
	}
}

func TestPessimist_ExpandTemporal_EmDashBetweenWords_NotSplitByFields(t *testing.T) {
	// "heute—morgen" is a single token for strings.Fields (em-dash is not whitespace).
	// Trim only strips edges, so the inner em-dash stays. The whole token matches nothing.
	// This documents a known limitation — NOT a bug to fix (would need custom tokenizer).
	result := ExpandTemporal("heute—morgen", pessimistRef)
	if result != "" {
		t.Fatalf("em-dash joined words: expected empty (known limitation), got %q", result)
	}
}

func TestPessimist_ExpandTemporal_MidnightBoundary_DateStillCorrect(t *testing.T) {
	midnight := time.Date(2026, 3, 25, 0, 0, 0, 0, time.UTC)
	result := ExpandTemporal("heute", midnight)
	if !strings.Contains(result, "2026-03-25") {
		t.Fatalf("midnight boundary: got %q, want date 2026-03-25", result)
	}
	// "gestern" at midnight should be 2026-03-24
	result2 := ExpandTemporal("gestern", midnight)
	if !strings.Contains(result2, "2026-03-24") {
		t.Fatalf("gestern at midnight: got %q, want 2026-03-24", result2)
	}
}

func TestPessimist_ExpandTemporal_NearDayBoundary_2359_StillSameDay(t *testing.T) {
	late := time.Date(2026, 3, 25, 23, 59, 59, 0, time.UTC)
	result := ExpandTemporal("heute", late)
	if !strings.Contains(result, "2026-03-25") {
		t.Fatalf("23:59:59: got %q, want 2026-03-25", result)
	}
	// "morgen" at 23:59:59 should be 2026-03-26, not 2026-03-25
	result2 := ExpandTemporal("morgen", late)
	if !strings.Contains(result2, "2026-03-26") {
		t.Fatalf("morgen at 23:59:59: got %q, want 2026-03-26", result2)
	}
}

func TestPessimist_ExpandTemporal_UppercaseQuery_CaseFolded(t *testing.T) {
	result := ExpandTemporal("HEUTE", pessimistRef)
	if !strings.Contains(result, "2026-03-25") {
		t.Fatalf("uppercase HEUTE: got %q, expected date match", result)
	}
	result2 := ExpandTemporal("MORGEN GESTERN", pessimistRef)
	if !strings.Contains(result2, "2026-03-26") || !strings.Contains(result2, "2026-03-24") {
		t.Fatalf("uppercase multi: got %q", result2)
	}
}

func TestPessimist_ExpandTemporal_MixedLanguageQuery_OnlyTemporalExpanded(t *testing.T) {
	result := ExpandTemporal("heute I will go tomorrow", pessimistRef)
	// Both "heute" and "tomorrow" are temporal keywords
	if !strings.Contains(result, "2026-03-25") {
		t.Errorf("missing 'heute' date in %q", result)
	}
	if !strings.Contains(result, "2026-03-26") {
		t.Errorf("missing 'tomorrow' date in %q", result)
	}
}

func TestPessimist_ExpandTemporal_OutputFormat_WeekdayDE_WeekdayEN_ISODate(t *testing.T) {
	// pessimistRef is Wednesday 2026-03-25
	result := ExpandTemporal("heute", pessimistRef)
	// Must contain German weekday, English weekday, and ISO date
	if !strings.Contains(result, "Mittwoch") {
		t.Errorf("missing German weekday 'Mittwoch' in %q", result)
	}
	if !strings.Contains(result, "Wednesday") {
		t.Errorf("missing English weekday 'Wednesday' in %q", result)
	}
	if !strings.Contains(result, "2026-03-25") {
		t.Errorf("missing ISO date '2026-03-25' in %q", result)
	}
	// Separated by " OR "
	parts := strings.Split(result, " OR ")
	if len(parts) != 3 {
		t.Errorf("expected 3 OR-separated parts, got %d: %q", len(parts), result)
	}
}

func TestPessimist_ExpandTemporal_WhitespaceOnlyQuery_ReturnsEmpty(t *testing.T) {
	result := ExpandTemporal("   \t\n  ", pessimistRef)
	if result != "" {
		t.Fatalf("whitespace-only query: got %q, want empty", result)
	}
}

func TestPessimist_ExpandTemporal_WordEntirelyPunctuation_SkippedNotPanic(t *testing.T) {
	// After trimming punctuation, word becomes empty — must not panic
	result := ExpandTemporal("... !!! ???", pessimistRef)
	if result != "" {
		t.Fatalf("punctuation-only words: got %q, want empty", result)
	}
}

func TestPessimist_ExpandTemporal_ZeroTime_DoesNotPanic(t *testing.T) {
	// time.Time{} is year 1, January 1 — weird but must not crash
	result := ExpandTemporal("heute", time.Time{})
	if result == "" {
		t.Fatal("zero time with 'heute': got empty, expected some output")
	}
	if !strings.Contains(result, "0001-01-01") {
		t.Errorf("zero time: got %q, expected date 0001-01-01", result)
	}
}

func TestPessimist_ExpandTemporal_HeuteMorgenProduceTwoDates(t *testing.T) {
	result := ExpandTemporal("heute und morgen abarbeiten", pessimistRef)
	parts := strings.Split(result, " OR ")
	// 2 dates × 3 terms each = 6 parts
	if len(parts) != 6 {
		t.Fatalf("heute+morgen: expected 6 OR-separated parts, got %d: %q", len(parts), result)
	}
}

// ─── weekdayDE ──────────────────────────────────────────────────────────────

func TestPessimist_WeekdayDE_AllSevenCorrectGermanNames(t *testing.T) {
	expected := [7]string{"Sonntag", "Montag", "Dienstag", "Mittwoch", "Donnerstag", "Freitag", "Samstag"}
	for i, want := range expected {
		if weekdayDE[i] != want {
			t.Errorf("weekdayDE[%d] = %q, want %q", i, weekdayDE[i], want)
		}
	}
}

func TestPessimist_WeekdayDE_IndexMatchesGoTimeWeekday(t *testing.T) {
	// Verify that weekdayDE indices align with time.Weekday constants
	mapping := map[time.Weekday]string{
		time.Sunday:    "Sonntag",
		time.Monday:    "Montag",
		time.Tuesday:   "Dienstag",
		time.Wednesday: "Mittwoch",
		time.Thursday:  "Donnerstag",
		time.Friday:    "Freitag",
		time.Saturday:  "Samstag",
	}
	for wd, want := range mapping {
		if weekdayDE[wd] != want {
			t.Errorf("weekdayDE[%s(%d)] = %q, want %q", wd, wd, weekdayDE[wd], want)
		}
	}
}

func TestPessimist_WeekdayDE_NoneEmpty(t *testing.T) {
	for i, name := range weekdayDE {
		if name == "" {
			t.Errorf("weekdayDE[%d] is empty — missing weekday name", i)
		}
	}
}
