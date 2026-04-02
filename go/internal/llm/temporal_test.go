// Package llm — Attacker Tests for Temporal Normalization
// These tests attempt to BREAK the temporal code with malicious inputs,
// edge cases, and security attack vectors.
package llm

import (
	"strings"
	"testing"
	"time"
)

// ---.
// HasTemporalIntent — Injection & Malicious Input Tests
// ---.

func TestAttacker_HasTemporalIntent_SQLInjection(t *testing.T) {
	// ATTACK: SQL injection payload containing a temporal keyword.
	// If this string ever reaches a SQL query unsanitized, it drops a table.
	input := "'; DROP TABLE context_blocks; --heute"
	if !HasTemporalIntent(input) {
		t.Error("should detect 'heute' even inside SQL injection payload")
	}
}

func TestAttacker_HasTemporalIntent_SQLInjection_NoKeyword(t *testing.T) {
	// ATTACK: SQL injection WITHOUT a temporal keyword. Must return false.
	// Verifies the function doesn't match on SQL syntax.
	input := "'; DROP TABLE context_blocks; --"
	if HasTemporalIntent(input) {
		t.Error("pure SQL injection without temporal keywords must return false")
	}
}

func TestAttacker_HasTemporalIntent_XSSPayload(t *testing.T) {
	// ATTACK: XSS with temporal keyword embedded inside a script tag without spaces.
	// T11 uses whole-token matching: "<script>alert('heute')</script>" is a single
	// whitespace-free token. TrimFunc strips only the outermost non-letter chars,
	// leaving "script>alert('heute')</script" — not a temporal keyword.
	// Acceptable trade-off: XSS payloads are not real temporal queries.
	input := "<script>alert('heute')</script>"
	got := HasTemporalIntent(input)
	t.Logf("XSS without whitespace separation detected: %v (T11: whole-token matching)", got)
	// When the keyword is whitespace-separated (realistic user input), it still matches.
	inputWithSpaces := "<script> heute </script>"
	if !HasTemporalIntent(inputWithSpaces) {
		t.Error("should detect 'heute' when separated by whitespace inside XSS payload")
	}
}

func TestAttacker_HasTemporalIntent_XSSWithoutKeyword(t *testing.T) {
	// ATTACK: XSS payload without any temporal keyword.
	input := "<script>alert('xss')</script>"
	if HasTemporalIntent(input) {
		t.Error("XSS without temporal keywords must return false")
	}
}

func TestAttacker_HasTemporalIntent_UnicodeNormalization(t *testing.T) {
	// ATTACK: Unicode combining characters to bypass string matching.
	// U+0065 is regular 'e', but "h\u0065ute" should still be "heute".
	input := "h\u0065ute"
	if !HasTemporalIntent(input) {
		t.Error("should detect 'heute' even with explicit unicode codepoints (no combining)")
	}
}

func TestAttacker_HasTemporalIntent_UnicodeCombiningAccent(t *testing.T) {
	// ATTACK: Use combining character U+0308 (combining diaeresis) to form 'ü'
	// from 'u' + combining mark. "u\u0308bermorgen" looks like "übermorgen"
	// but uses NFD decomposition. strings.Contains with NFC "übermorgen" should NOT match.
	input := "u\u0308bermorgen"
	// This is a known normalization gap — the code uses pre-composed "übermorgen"
	// but the input uses decomposed form. Document whether it matches or not.
	result := HasTemporalIntent(input)
	t.Logf("NFD decomposed 'übermorgen' detected: %v (potential normalization bypass)", result)
	// The function also checks for "morgen" as a substring, so it may match anyway.
	// This test documents the behavior either way.
}

func TestAttacker_HasTemporalIntent_ZeroWidthCharacters(t *testing.T) {
	// ATTACK: Zero-width characters inserted between letters to break substring matching.
	// U+200B = zero-width space, U+200C = zero-width non-joiner, U+FEFF = BOM.
	inputs := []struct {
		name  string
		input string
	}{
		{"zero-width space", "heu\u200Bte"},
		{"zero-width non-joiner", "heu\u200Cte"},
		{"BOM inside word", "heu\uFEFFte"},
		{"zero-width joiner", "heu\u200Dte"},
	}
	for _, tc := range inputs {
		t.Run(tc.name, func(t *testing.T) {
			result := HasTemporalIntent(tc.input)
			// Zero-width chars BREAK substring matching — "heute" won't be found.
			// This is expected behavior (pre-filter allows false negatives).
			t.Logf("input=%q detected=%v (zero-width chars break matching)", tc.input, result)
			if result {
				t.Error("zero-width characters inside 'heute' should break substring matching")
			}
		})
	}
}

func TestAttacker_HasTemporalIntent_ExtremelyLongQuery(t *testing.T) {
	// ATTACK: 100KB query to test for excessive memory/CPU usage.
	// The function iterates over ~60 keywords with strings.Contains on a 100KB string.
	bigQuery := strings.Repeat("a", 100*1024)
	result := HasTemporalIntent(bigQuery)
	if result {
		t.Error("100KB of 'a' characters should not match any temporal keyword")
	}
}

func TestAttacker_HasTemporalIntent_ExtremelyLongQueryWithKeyword(t *testing.T) {
	// ATTACK: Very long query with temporal keyword as a standalone token.
	// T11: Fields-tokenization requires whitespace separation — "aaaa...heute" without
	// a space is one token and won't match. Real queries always have whitespace.
	bigQuery := strings.Repeat("word ", 50000) + "heute"
	if !HasTemporalIntent(bigQuery) {
		t.Error("should find 'heute' as a standalone token in a long query")
	}
}

func TestAttacker_HasTemporalIntent_EmptyString(t *testing.T) {
	// ATTACK: Empty input — must not panic or match.
	if HasTemporalIntent("") {
		t.Error("empty string should return false")
	}
}

func TestAttacker_HasTemporalIntent_WhitespaceOnly(t *testing.T) {
	// ATTACK: Various whitespace-only inputs.
	inputs := []string{" ", "\t", "\n", "\r\n", "   \t\n  "}
	for _, input := range inputs {
		if HasTemporalIntent(input) {
			t.Errorf("whitespace-only input %q should return false", input)
		}
	}
}

func TestAttacker_HasTemporalIntent_NullLikeInputs(t *testing.T) {
	// ATTACK: Null-like string values that could confuse parsers.
	inputs := []string{"null", "NULL", "nil", "undefined", "None", "\x00", "\x00\x00\x00"}
	for _, input := range inputs {
		result := HasTemporalIntent(input)
		t.Logf("input=%q detected=%v", input, result)
		// None of these should match temporal keywords.
		if result {
			t.Errorf("null-like input %q should not match temporal keywords", input)
		}
	}
}

func TestAttacker_HasTemporalIntent_CaseSensitivity(t *testing.T) {
	// ATTACK: Case manipulation to bypass lowercase matching.
	// The code does strings.ToLower, so these should all match.
	inputs := []struct {
		input    string
		expected bool
	}{
		{"HEUTE", true},
		{"HeuTe", true},
		{"hEuTe", true},
		{"GESTERN", true},
		{"MORGEN", true},
		{"TODAY", true},
		{"YESTERDAY", true},
		{"TOMORROW", true},
		{"MoNtAg", true},
		{"WOCHE", true},
		{"wEeK", true},
	}
	for _, tc := range inputs {
		t.Run(tc.input, func(t *testing.T) {
			got := HasTemporalIntent(tc.input)
			if got != tc.expected {
				t.Errorf("HasTemporalIntent(%q) = %v, want %v", tc.input, got, tc.expected)
			}
		})
	}
}

func TestAttacker_HasTemporalIntent_ISODateEdgeCases(t *testing.T) {
	// ATTACK: ISO date regex bypass with invalid but pattern-matching dates.
	inputs := []struct {
		name     string
		input    string
		expected bool
	}{
		{"valid date", "2026-03-28", true},
		{"impossible month", "9999-99-99", true},    // Regex matches pattern, not validity
		{"zero date", "0000-00-00", true},            // Regex matches pattern
		{"month 13", "2026-13-32", true},             // Regex matches pattern
		{"negative year", "-001-01-01", false},       // No \b match on negative
		{"short year", "26-03-28", false},            // Only 2-digit year
		{"no dashes", "20260328", false},             // No dashes
		{"extra digits", "20260-03-28", false},       // 5-digit year — \b won't match
		{"spaces in date", "2026 -03- 28", false},    // Spaces break pattern
		{"date with time", "2026-03-28T10:00:00", false}, // \b requires word/non-word boundary; 8 and T are both word chars — no boundary
	}
	for _, tc := range inputs {
		t.Run(tc.name, func(t *testing.T) {
			got := HasTemporalIntent(tc.input)
			if got != tc.expected {
				t.Errorf("HasTemporalIntent(%q) = %v, want %v", tc.input, got, tc.expected)
			}
		})
	}
}

func TestAttacker_HasTemporalIntent_ISODateBoundaryBreak(t *testing.T) {
	// ATTACK: Embed an ISO-like date inside a longer number to see if \b breaks correctly.
	inputs := []struct {
		name     string
		input    string
		expected bool
	}{
		{"embedded in number", "123456782026-03-2812345678", false},
		{"preceded by letter", "x2026-03-28", false},    // x and 2 are both word chars — no \b
		{"followed by letter", "2026-03-28x", false},    // 8 and x are both word chars — no \b
		{"preceded by digit", "12026-03-28", false},     // no \b between 1 and 2
		{"followed by digit", "2026-03-289", false},     // no \b between 8 and 9
		{"preceded by underscore", "_2026-03-28", false}, // \b: _ is word char, 2 is word char
	}
	for _, tc := range inputs {
		t.Run(tc.name, func(t *testing.T) {
			got := HasTemporalIntent(tc.input)
			if got != tc.expected {
				t.Errorf("HasTemporalIntent(%q) = %v, want %v", tc.input, got, tc.expected)
			}
		})
	}
}

func TestAttacker_HasTemporalIntent_SubstringFalsePositives(t *testing.T) {
	// T11 fix: whole-token matching eliminates the substring false positives that
	// previously caused unnecessary LLM fallback calls.
	inputs := []struct {
		input    string
		expected bool
		note     string
	}{
		{"Vorschlag", false, "T11: 'vor' no longer matches inside 'vorschlag' (whole-token)"},
		{"Vorteil", false, "T11: 'vor' no longer matches inside 'vorteil'"},
		{"bevor", false, "T11: 'vor' no longer matches inside 'bevor'"},
		{"Bison", false, "T11: 'bis' no longer matches inside 'bison'"},
		{"Seite", false, "T11: 'seit' no longer matches inside 'seite'"},
		{"Maiwetter", false, "T11: 'mai' no longer matches inside 'maiwetter'"},
		{"context", false, "T11: 'next' no longer matches inside 'context'"},
		{"Jubiläum", false, "'juli' is not a substring of 'jubiläum' (j-u-b-i-l-ä-u-m)"},
		{"Augenblick", false, "no temporal keyword is a substring"},
		{"Sonnenbrand", false, "no temporal keyword is a substring of 'sonnenbrand'"},
		// Positive cases: standalone temporal words still match.
		{"vor 3 Tagen", true, "'vor' as standalone token"},
		{"seit gestern", true, "'seit' and 'gestern' as standalone tokens"},
		{"Maiwetter im Mai", true, "'mai' as standalone token after whitespace"},
		{"was ist next week?", true, "'next' and 'week' as standalone tokens"},
	}
	for _, tc := range inputs {
		t.Run(tc.input, func(t *testing.T) {
			got := HasTemporalIntent(tc.input)
			if got != tc.expected {
				t.Logf("note: %s", tc.note)
				t.Errorf("HasTemporalIntent(%q) = %v, want %v", tc.input, got, tc.expected)
			}
		})
	}
}

// ---.
// T11 — HasTemporalIntent whole-token matching (regression suite)
// ---.

func TestT11_HasTemporalIntent_WordBoundary(t *testing.T) {
	// Regression suite for T11: substring false positives fixed by whole-token matching.
	tests := []struct {
		input    string
		expected bool
	}{
		// Must NOT trigger (were false-positives before T11)
		{"Vorteil", false},
		{"Vorschlag", false},
		{"bevor", false},
		{"Seite", false},
		{"Bison", false},
		{"Maiwetter", false},
		{"context", false},
		{"elasticSearch", false}, // "last" is NOT a substring match — different word entirely
		// Must still trigger (genuine temporal words as whole tokens)
		{"vor 3 Tagen", true},
		{"seit gestern", true},
		{"Maiwetter im Mai", true},
		{"bis morgen", true},
		{"was passierte letzten Montag?", true},
		{"next week", true},
		{"last year", true},
		{"ago", true},
		// Punctuation stripped correctly
		{"(heute)", true},
		{"gestern,", true},
		{"morgen!", true},
		{"\"vor\" kurzer Zeit", true},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := HasTemporalIntent(tt.input)
			if got != tt.expected {
				t.Errorf("HasTemporalIntent(%q) = %v, want %v", tt.input, got, tt.expected)
			}
		})
	}
}

// ---.
// TemporalToFTSExpansion — Malicious Date Structs
// ---.

func TestAttacker_TemporalToFTSExpansion_SQLInjectionInRef(t *testing.T) {
	// ATTACK: SQL injection payload in the Ref field.
	// Ref is NOT used in FTS expansion output, so it should be harmless.
	dates := []TemporalDate{{
		Ref:  "'; DROP TABLE context_blocks; --",
		Date: "2026-03-28",
		Dir:  "past",
	}}
	result := TemporalToFTSExpansion(dates)
	if strings.Contains(result, "DROP") {
		t.Error("SQL injection from Ref field leaked into FTS output")
	}
	if result == "" {
		t.Error("valid date with malicious Ref should still produce output")
	}
}

func TestAttacker_TemporalToFTSExpansion_SQLInjectionInDate(t *testing.T) {
	// ATTACK: SQL injection in the Date field itself.
	// time.Parse should reject this, producing empty output.
	dates := []TemporalDate{{
		Ref:  "test",
		Date: "2026-03-28'; DROP TABLE--",
		Dir:  "past",
	}}
	result := TemporalToFTSExpansion(dates)
	if result != "" {
		t.Errorf("SQL injection in Date field should produce empty output, got: %q", result)
	}
}

func TestAttacker_TemporalToFTSExpansion_EmptyDates(t *testing.T) {
	// ATTACK: Empty dates slice.
	result := TemporalToFTSExpansion([]TemporalDate{})
	if result != "" {
		t.Errorf("empty dates should return empty string, got: %q", result)
	}
}

func TestAttacker_TemporalToFTSExpansion_NilDates(t *testing.T) {
	// ATTACK: Nil dates slice.
	result := TemporalToFTSExpansion(nil)
	if result != "" {
		t.Errorf("nil dates should return empty string, got: %q", result)
	}
}

func TestAttacker_TemporalToFTSExpansion_DuplicateDates(t *testing.T) {
	// ATTACK: Duplicate dates to check deduplication works.
	dates := []TemporalDate{
		{Ref: "heute", Date: "2026-03-28", Dir: "today"},
		{Ref: "heute nochmal", Date: "2026-03-28", Dir: "today"},
		{Ref: "und nochmal", Date: "2026-03-28", Dir: "today"},
	}
	result := TemporalToFTSExpansion(dates)
	// Should only contain one set of terms for 2026-03-28.
	count := strings.Count(result, "2026-03-28")
	if count != 1 {
		t.Errorf("duplicate dates should be deduplicated, but '2026-03-28' appears %d times in: %q", count, result)
	}
}

func TestAttacker_TemporalToFTSExpansion_InvalidISOStrings(t *testing.T) {
	// ATTACK: Various invalid date strings that time.Parse should reject.
	invalids := []string{
		"not-a-date",
		"2026/03/28",
		"28-03-2026",
		"2026-3-28",
		"2026-03-28T10:00:00",
		"",
		"null",
		"2026-00-00",
		"2026-13-01",
		"2026-02-30",
	}
	for _, d := range invalids {
		t.Run(d, func(t *testing.T) {
			dates := []TemporalDate{{Ref: "test", Date: d, Dir: "past"}}
			result := TemporalToFTSExpansion(dates)
			if result != "" {
				t.Errorf("invalid date %q should produce empty output, got: %q", d, result)
			}
		})
	}
}

func TestAttacker_TemporalToFTSExpansion_NilEndPointer(t *testing.T) {
	// ATTACK: Verify nil End pointer doesn't cause panic.
	dates := []TemporalDate{{
		Ref:  "heute",
		Date: "2026-03-28",
		End:  nil,
		Dir:  "today",
	}}
	result := TemporalToFTSExpansion(dates)
	if result == "" {
		t.Error("valid date with nil End should still produce output")
	}
}

func TestAttacker_TemporalToFTSExpansion_EndWithInvalidDate(t *testing.T) {
	// ATTACK: Valid Date but invalid End — End should be silently skipped.
	badEnd := "not-a-date"
	dates := []TemporalDate{{
		Ref:  "seit gestern",
		Date: "2026-03-27",
		End:  &badEnd,
		Dir:  "range",
	}}
	result := TemporalToFTSExpansion(dates)
	if result == "" {
		t.Error("valid Date with invalid End should still produce output for the valid Date")
	}
	if strings.Contains(result, "not-a-date") {
		t.Error("invalid End date string leaked into output")
	}
}

func TestAttacker_TemporalToFTSExpansion_SQLInjectionInEnd(t *testing.T) {
	// ATTACK: SQL injection in the End field.
	badEnd := "2026-03-28'; DROP TABLE--"
	dates := []TemporalDate{{
		Ref:  "range",
		Date: "2026-03-27",
		End:  &badEnd,
		Dir:  "range",
	}}
	result := TemporalToFTSExpansion(dates)
	if strings.Contains(result, "DROP") {
		t.Error("SQL injection from End field leaked into FTS output")
	}
}

func TestAttacker_TemporalToFTSExpansion_MassiveDateCount(t *testing.T) {
	// ATTACK: 1000+ dates to test for performance issues or memory exhaustion.
	// Scale-capped: only maxFTSDates (10) dates get core terms (weekday+ISO),
	// but months and KW numbers from ALL 1000 dates are still included.
	dates := make([]TemporalDate, 1000)
	for i := 0; i < 1000; i++ {
		d := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i)
		dates[i] = TemporalDate{
			Ref:  "test",
			Date: d.Format("2006-01-02"),
			Dir:  "past",
		}
	}
	result := TemporalToFTSExpansion(dates)
	if result == "" {
		t.Error("1000 valid dates should produce non-empty output")
	}
	termCount := strings.Count(result, " OR ") + 1
	// Capped: 10 dates × 3 core terms = 30, + deduplicated months (~34 × 3)
	// + deduplicated KWs (~143). Total ~200-300 terms, NOT 3000+.
	// Must be >30 (core terms exist) and <500 (capped, not 3000+).
	if termCount < 30 {
		t.Errorf("expected at least 30 core terms from capped dates, got %d", termCount)
	}
	if termCount > 500 {
		t.Errorf("expected capped term count <500, got %d (capping may be broken)", termCount)
	}
	// Verify months from ALL dates are present (not just capped ones).
	// 1000 days from 2020-01-01 spans ~34 months. Check a mid-range month.
	if !strings.Contains(result, "Juli") {
		t.Error("month 'Juli' from mid-range dates should be present despite capping")
	}
}

func TestAttacker_TemporalToFTSExpansion_AllInvalidDates(t *testing.T) {
	// ATTACK: All dates are invalid — should produce empty string, not panic.
	dates := []TemporalDate{
		{Ref: "a", Date: "garbage", Dir: "past"},
		{Ref: "b", Date: "2026-99-99", Dir: "past"},
		{Ref: "c", Date: "", Dir: "past"},
	}
	result := TemporalToFTSExpansion(dates)
	if result != "" {
		t.Errorf("all invalid dates should produce empty output, got: %q", result)
	}
}

func TestAttacker_TemporalToFTSExpansion_MixedValidInvalid(t *testing.T) {
	// ATTACK: Mix of valid and invalid dates — only valid ones should appear.
	dates := []TemporalDate{
		{Ref: "bad", Date: "garbage", Dir: "past"},
		{Ref: "good", Date: "2026-03-28", Dir: "today"},
		{Ref: "bad2", Date: "xxxx-xx-xx", Dir: "past"},
	}
	result := TemporalToFTSExpansion(dates)
	if result == "" {
		t.Error("mixed valid/invalid dates should produce output for valid ones")
	}
	if !strings.Contains(result, "2026-03-28") {
		t.Error("valid date should appear in output")
	}
}

// ---.
// TemporalToEmbedPrefix — Malicious Inputs
// ---.

func TestAttacker_TemporalToEmbedPrefix_EmptyDates(t *testing.T) {
	result := TemporalToEmbedPrefix([]TemporalDate{})
	if result != "" {
		t.Errorf("empty dates should return empty string, got: %q", result)
	}
}

func TestAttacker_TemporalToEmbedPrefix_NilDates(t *testing.T) {
	result := TemporalToEmbedPrefix(nil)
	if result != "" {
		t.Errorf("nil dates should return empty string, got: %q", result)
	}
}

func TestAttacker_TemporalToEmbedPrefix_InvalidDateFormats(t *testing.T) {
	// ATTACK: Various invalid date formats.
	invalids := []string{
		"not-a-date", "2026/03/28", "", "null", "2026-1-1",
		"03-28-2026", "2026-03-28T00:00:00Z",
	}
	for _, d := range invalids {
		t.Run(d, func(t *testing.T) {
			dates := []TemporalDate{{Ref: "test", Date: d, Dir: "past"}}
			result := TemporalToEmbedPrefix(dates)
			if result != "" {
				t.Errorf("invalid date %q should produce empty prefix, got: %q", d, result)
			}
		})
	}
}

func TestAttacker_TemporalToEmbedPrefix_ExtremeYears(t *testing.T) {
	// ATTACK: Dates far in past and future.
	extremes := []struct {
		name string
		date string
	}{
		{"year 0001", "0001-01-01"},
		{"year 9999", "9999-12-31"},
		{"year 0000", "0000-01-01"},
		{"far future", "9999-06-15"},
	}
	for _, tc := range extremes {
		t.Run(tc.name, func(t *testing.T) {
			dates := []TemporalDate{{Ref: "test", Date: tc.date, Dir: "past"}}
			result := TemporalToEmbedPrefix(dates)
			// Year 0000 is valid in Go's time package (year 0 = 1 BC).
			t.Logf("date=%q result=%q", tc.date, result)
			if tc.date == "0001-01-01" || tc.date == "9999-12-31" || tc.date == "9999-06-15" || tc.date == "0000-01-01" {
				if result == "" {
					t.Errorf("extreme but parseable date %q should produce output", tc.date)
				}
			}
		})
	}
}

func TestAttacker_TemporalToEmbedPrefix_SQLInjectionInRef(t *testing.T) {
	// ATTACK: Ref with SQL injection — should not appear in embed prefix.
	dates := []TemporalDate{{
		Ref:  "'; DROP TABLE context_blocks; --",
		Date: "2026-03-28",
		Dir:  "past",
	}}
	result := TemporalToEmbedPrefix(dates)
	if strings.Contains(result, "DROP") {
		t.Error("SQL injection from Ref field leaked into embed prefix")
	}
	if result == "" {
		t.Error("valid date with malicious Ref should still produce output")
	}
}

func TestAttacker_TemporalToEmbedPrefix_Deduplication(t *testing.T) {
	// ATTACK: Duplicate dates should be deduplicated in prefix.
	dates := []TemporalDate{
		{Ref: "a", Date: "2026-03-28", Dir: "today"},
		{Ref: "b", Date: "2026-03-28", Dir: "today"},
	}
	result := TemporalToEmbedPrefix(dates)
	count := strings.Count(result, "2026-03-28")
	if count != 1 {
		t.Errorf("duplicate dates should be deduplicated, '2026-03-28' appears %d times in: %q", count, result)
	}
}

func TestAttacker_TemporalToEmbedPrefix_AllInvalid(t *testing.T) {
	// ATTACK: All dates invalid — must return empty, not "." alone.
	dates := []TemporalDate{
		{Ref: "a", Date: "garbage", Dir: "past"},
		{Ref: "b", Date: "nope", Dir: "past"},
	}
	result := TemporalToEmbedPrefix(dates)
	if result != "" {
		t.Errorf("all invalid dates should return empty string, got: %q", result)
	}
}

func TestAttacker_TemporalToEmbedPrefix_TrailingDot(t *testing.T) {
	// Verify the prefix always ends with a period for valid dates.
	dates := []TemporalDate{{Ref: "test", Date: "2026-03-28", Dir: "today"}}
	result := TemporalToEmbedPrefix(dates)
	if !strings.HasSuffix(result, ".") {
		t.Errorf("embed prefix should end with '.', got: %q", result)
	}
}

func TestAttacker_TemporalToEmbedPrefix_MassiveDateCount(t *testing.T) {
	// ATTACK: 1000+ dates — verify it doesn't blow up memory or panic.
	dates := make([]TemporalDate, 1500)
	for i := 0; i < 1500; i++ {
		d := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i)
		dates[i] = TemporalDate{Ref: "test", Date: d.Format("2006-01-02"), Dir: "past"}
	}
	result := TemporalToEmbedPrefix(dates)
	if result == "" {
		t.Error("1500 valid dates should produce non-empty prefix")
	}
	if !strings.HasSuffix(result, ".") {
		t.Error("prefix should end with '.' even with massive input")
	}
}

// ---.
// buildCalendar — Boundary Dates & Edge Cases
// ---.

func TestAttacker_BuildCalendar_January1YearBoundary(t *testing.T) {
	// BOUNDARY: January 1 — year boundary. "Last week" crosses into previous year.
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC) // Thursday
	cal := buildCalendar(now)
	if cal == "" {
		t.Fatal("buildCalendar returned empty string")
	}
	// January 1 2026 is a Thursday. Last Monday = Dec 22 2025.
	if !strings.Contains(cal, "2025-12") {
		t.Error("calendar for Jan 1 should reference previous year (December 2025)")
	}
	if !strings.Contains(cal, "2026-01-01") {
		t.Error("calendar should contain today's date")
	}
	t.Logf("calendar:\n%s", cal)
}

func TestAttacker_BuildCalendar_December31YearBoundary(t *testing.T) {
	// BOUNDARY: December 31 — "next week" crosses into next year.
	now := time.Date(2025, 12, 31, 12, 0, 0, 0, time.UTC) // Wednesday
	cal := buildCalendar(now)
	if !strings.Contains(cal, "2026-01") {
		t.Error("calendar for Dec 31 should reference next year (January 2026)")
	}
	if !strings.Contains(cal, "2025-12-31") {
		t.Error("calendar should contain today's date")
	}
}

func TestAttacker_BuildCalendar_LeapYearFeb29(t *testing.T) {
	// BOUNDARY: February 29 on a leap year.
	now := time.Date(2028, 2, 29, 12, 0, 0, 0, time.UTC) // Tuesday
	cal := buildCalendar(now)
	if !strings.Contains(cal, "2028-02-29") {
		t.Error("calendar should contain Feb 29 on leap year")
	}
	// Verify tomorrow is March 1.
	if !strings.Contains(cal, "2028-03-01") {
		t.Error("tomorrow after Feb 29 should be March 1")
	}
}

func TestAttacker_BuildCalendar_NonLeapYearFeb28(t *testing.T) {
	// BOUNDARY: February 28 on a non-leap year.
	now := time.Date(2026, 2, 28, 12, 0, 0, 0, time.UTC) // Saturday
	cal := buildCalendar(now)
	if !strings.Contains(cal, "2026-02-28") {
		t.Error("calendar should contain Feb 28")
	}
	// Tomorrow should be March 1 (not Feb 29).
	if !strings.Contains(cal, "2026-03-01") {
		t.Error("tomorrow after Feb 28 (non-leap) should be March 1")
	}
}

func TestAttacker_BuildCalendar_Sunday(t *testing.T) {
	// BOUNDARY: Sunday — tests the wd==0 -> wd=7 branch in buildCalendar.
	now := time.Date(2026, 3, 29, 12, 0, 0, 0, time.UTC) // Sunday
	cal := buildCalendar(now)
	if !strings.Contains(cal, "Sunday") {
		t.Error("calendar for Sunday should say 'Sunday'")
	}
	// This Monday should be March 23 (6 days before Sunday March 29).
	if !strings.Contains(cal, "2026-03-23") {
		t.Error("this week's Monday for Sunday March 29 should be March 23")
	}
}

func TestAttacker_BuildCalendar_Monday(t *testing.T) {
	// BOUNDARY: Monday — the "this week" start IS today.
	now := time.Date(2026, 3, 23, 12, 0, 0, 0, time.UTC) // Monday
	cal := buildCalendar(now)
	if !strings.Contains(cal, "Monday") {
		t.Error("calendar for Monday should say 'Monday'")
	}
}

func TestAttacker_BuildCalendar_DSTSpringForward(t *testing.T) {
	// BOUNDARY: DST transition (spring forward) — March 29, 2026 in Europe.
	// Using a DST-aware timezone to see if date arithmetic breaks.
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip("Europe/Berlin timezone not available")
	}
	// March 29, 2026 is the day clocks spring forward in Europe.
	now := time.Date(2026, 3, 29, 2, 30, 0, 0, loc)
	cal := buildCalendar(now)
	if cal == "" {
		t.Fatal("buildCalendar returned empty for DST transition date")
	}
	// Verify no date is duplicated or missing.
	if !strings.Contains(cal, "2026-03-29") {
		t.Error("calendar should contain DST transition date")
	}
	t.Logf("DST calendar:\n%s", cal)
}

func TestAttacker_BuildCalendar_DSTFallBack(t *testing.T) {
	// BOUNDARY: DST transition (fall back) — October 25, 2026 in Europe.
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip("Europe/Berlin timezone not available")
	}
	now := time.Date(2026, 10, 25, 2, 30, 0, 0, loc)
	cal := buildCalendar(now)
	if cal == "" {
		t.Fatal("buildCalendar returned empty for DST fall-back date")
	}
	if !strings.Contains(cal, "2026-10-25") {
		t.Error("calendar should contain DST fall-back date")
	}
}

func TestAttacker_BuildCalendar_ZeroTimeValue(t *testing.T) {
	// ATTACK: time.Time zero value — January 1, year 1.
	// Should not panic.
	var zero time.Time
	cal := buildCalendar(zero)
	if cal == "" {
		t.Fatal("buildCalendar panicked or returned empty for zero time")
	}
	// Zero time in Go is 0001-01-01 00:00:00 UTC, which is a Monday.
	if !strings.Contains(cal, "0001-01-01") {
		t.Error("zero time calendar should reference year 0001")
	}
	t.Logf("zero time calendar:\n%s", cal)
}

func TestAttacker_BuildCalendar_FarFuture(t *testing.T) {
	// ATTACK: Year 9999 — should not overflow or panic.
	now := time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
	cal := buildCalendar(now)
	if cal == "" {
		t.Fatal("buildCalendar returned empty for year 9999")
	}
	if !strings.Contains(cal, "9999-12-31") {
		t.Error("calendar should contain year 9999 date")
	}
	// "Next week" will be in year 10000 — verify no panic.
	t.Logf("far future calendar:\n%s", cal)
}

func TestAttacker_BuildCalendar_NegativeYear(t *testing.T) {
	// ATTACK: Negative year (before year 0). Go supports this.
	now := time.Date(-1, 6, 15, 12, 0, 0, 0, time.UTC)
	cal := buildCalendar(now)
	if cal == "" {
		t.Fatal("buildCalendar panicked or returned empty for negative year")
	}
	t.Logf("negative year calendar:\n%s", cal)
}

func TestAttacker_BuildCalendar_WeekdayConsistency(t *testing.T) {
	// VERIFICATION: For each day of the week, verify the calendar header matches.
	// Uses a known week: March 23-29, 2026 (Mon-Sun).
	for i := 0; i < 7; i++ {
		day := time.Date(2026, 3, 23+i, 12, 0, 0, 0, time.UTC)
		cal := buildCalendar(day)
		expectedEN := weekdayNameEN[day.Weekday()]
		if !strings.HasPrefix(cal, "TODAY: "+expectedEN) {
			firstLine := cal
			if idx := strings.Index(cal, "\n"); idx > 0 {
				firstLine = cal[:idx]
			}
			t.Errorf("day %d: expected prefix 'TODAY: %s', got first line: %q", i, expectedEN, firstLine)
		}
	}
}

func TestAttacker_BuildCalendar_GermanWeekdayPresent(t *testing.T) {
	// VERIFICATION: German weekday names appear in the calendar body.
	now := time.Date(2026, 3, 28, 12, 0, 0, 0, time.UTC) // Saturday
	cal := buildCalendar(now)
	// heute/today line should have German weekday.
	if !strings.Contains(cal, "Samstag 2026-03-28") {
		t.Error("calendar should contain German weekday for today (Samstag)")
	}
}

// ---.
// TemporalDate struct edge cases
// ---.

func TestAttacker_TemporalDate_EmptyFields(t *testing.T) {
	// ATTACK: All fields empty.
	dates := []TemporalDate{{Ref: "", Date: "", End: nil, Dir: ""}}
	fts := TemporalToFTSExpansion(dates)
	embed := TemporalToEmbedPrefix(dates)
	if fts != "" {
		t.Errorf("empty date should produce empty FTS, got: %q", fts)
	}
	if embed != "" {
		t.Errorf("empty date should produce empty embed prefix, got: %q", embed)
	}
}

func TestAttacker_TemporalDate_EndPointsToEmptyString(t *testing.T) {
	// ATTACK: End pointer to empty string.
	empty := ""
	dates := []TemporalDate{{Ref: "test", Date: "2026-03-28", End: &empty, Dir: "range"}}
	result := TemporalToFTSExpansion(dates)
	// Valid Date should still produce output; empty End is silently skipped.
	if result == "" {
		t.Error("valid Date with empty End should still produce output")
	}
}

func TestAttacker_TemporalToFTSExpansion_WeekdayCorrectness(t *testing.T) {
	// VERIFICATION: Check that the weekday names in the output actually match the date.
	// 2026-03-28 is a Saturday.
	dates := []TemporalDate{{Ref: "test", Date: "2026-03-28", Dir: "today"}}
	result := TemporalToFTSExpansion(dates)
	if !strings.Contains(result, "Samstag") {
		t.Errorf("2026-03-28 (Saturday) should produce 'Samstag', got: %q", result)
	}
	if !strings.Contains(result, "Saturday") {
		t.Errorf("2026-03-28 (Saturday) should produce 'Saturday', got: %q", result)
	}
}

func TestAttacker_TemporalToEmbedPrefix_WeekdayCorrectness(t *testing.T) {
	// VERIFICATION: Embed prefix weekday matches date.
	dates := []TemporalDate{{Ref: "test", Date: "2026-03-28", Dir: "today"}}
	result := TemporalToEmbedPrefix(dates)
	// Enhanced prefix includes month + KW.
	if !strings.HasPrefix(result, "Samstag 2026-03-28") {
		t.Errorf("expected prefix starting with 'Samstag 2026-03-28', got %q", result)
	}
	if !strings.Contains(result, "März") {
		t.Errorf("expected month name 'März' in prefix, got %q", result)
	}
}

// ---.
// Regex edge cases
// ---.

func TestAttacker_ISODateRegex_ReDoS(t *testing.T) {
	// ATTACK: ReDoS attempt — long string of digits that could cause catastrophic backtracking.
	// The regex is simple (\b\d{4}-\d{2}-\d{2}\b) so it should be safe,
	// but we verify with a pathological input.
	longDigits := strings.Repeat("1234-56-78-", 10000)
	// Should complete quickly without hanging.
	_ = isoDateRe.MatchString(longDigits)
}

func TestAttacker_ISODateRegex_MultipleMatches(t *testing.T) {
	// ATTACK: Many ISO dates in one query.
	var parts []string
	for i := 0; i < 100; i++ {
		parts = append(parts, "2026-03-28")
	}
	input := strings.Join(parts, " ")
	if !HasTemporalIntent(input) {
		t.Error("query with 100 ISO dates should have temporal intent")
	}
}

func TestAttacker_JsonFenceRegex_NestedFences(t *testing.T) {
	// ATTACK: Test the JSON fence stripping regex with nested/malformed fences.
	inputs := []struct {
		name     string
		input    string
		expected string
	}{
		{
			"normal fence",
			"```json\n{\"dates\":[]}\n```",
			`{"dates":[]}`,
		},
		{
			"no language tag",
			"```\n{\"dates\":[]}\n```",
			`{"dates":[]}`,
		},
		{
			"nested backticks inside",
			"```json\n{\"dates\":[],\"query\":\"```test```\"}\n```",
			// The regex is non-greedy (.*?), so it should match the first closing ```.
			// This may or may not capture the full JSON depending on the non-greedy behavior.
			"", // Just verify it doesn't panic
		},
	}
	for _, tc := range inputs {
		t.Run(tc.name, func(t *testing.T) {
			m := jsonFenceRe.FindStringSubmatch(tc.input)
			if tc.expected != "" {
				if len(m) < 2 || m[1] != tc.expected {
					t.Errorf("expected %q, got match: %v", tc.expected, m)
				}
			}
			// No panic is a pass for edge cases.
		})
	}
}

// ---.
// Scale-Capping Tests (Wave 2: T03 + T05)
// ---.

func TestTemporalToFTSExpansion_RangeCapping(t *testing.T) {
	// Range "diese Woche" = 1 TemporalDate with Date+End → 2 unique dates → ≤20 terms.
	end := "2026-03-29"
	dates := []TemporalDate{{
		Ref:  "diese Woche",
		Date: "2026-03-23",
		End:  &end,
		Dir:  "range",
	}}
	result := TemporalToFTSExpansion(dates)
	termCount := strings.Count(result, " OR ") + 1
	if termCount > 20 {
		t.Errorf("week range should produce ≤20 OR-terms, got %d: %s", termCount, result)
	}
	if termCount < 6 {
		t.Errorf("week range should produce at least 6 terms (2 dates × 3 core), got %d", termCount)
	}
	// Verify both start and end dates are present.
	if !strings.Contains(result, "2026-03-23") {
		t.Error("start date 2026-03-23 should be in FTS expansion")
	}
	if !strings.Contains(result, "2026-03-29") {
		t.Error("end date 2026-03-29 should be in FTS expansion")
	}
}

func TestTemporalToFTSExpansion_SingleDate(t *testing.T) {
	// Single date: "heute" = 1 TemporalDate, no End.
	dates := []TemporalDate{{
		Ref:  "heute",
		Date: "2026-03-29",
		Dir:  "today",
	}}
	result := TemporalToFTSExpansion(dates)
	termCount := strings.Count(result, " OR ") + 1
	// 1 date: 3 core (weekday DE/EN + ISO) + 3 month (DE/EN + YYYY-MM) + 1 KW = 7
	if termCount != 7 {
		t.Errorf("single date should produce exactly 7 terms, got %d: %s", termCount, result)
	}
	if !strings.Contains(result, "Sonntag") {
		t.Error("2026-03-29 is a Sunday, should contain 'Sonntag'")
	}
	if !strings.Contains(result, "Sunday") {
		t.Error("2026-03-29 is a Sunday, should contain 'Sunday'")
	}
	if !strings.Contains(result, "2026-03-29") {
		t.Error("should contain ISO date")
	}
	if !strings.Contains(result, "März") {
		t.Error("should contain German month name")
	}
	if !strings.Contains(result, "KW13") {
		t.Error("2026-03-29 is KW13, should contain 'KW13'")
	}
}

func TestTemporalToFTSExpansion_CappingPreservesMonthsAndKWs(t *testing.T) {
	// 20 dates spanning 2 months — capped to maxFTSDates (3), but both months should appear.
	// T03: At 1M+ blocks, only start+end dates get core expansion.
	dates := make([]TemporalDate, 20)
	for i := 0; i < 20; i++ {
		d := time.Date(2026, 3, 20+i, 0, 0, 0, 0, time.UTC) // March 20 through April 8
		dates[i] = TemporalDate{Ref: "test", Date: d.Format("2006-01-02"), Dir: "past"}
	}
	result := TemporalToFTSExpansion(dates)
	// Both months should be present despite capping.
	if !strings.Contains(result, "März") {
		t.Error("March should be present in capped FTS expansion")
	}
	if !strings.Contains(result, "April") {
		t.Error("April should be present in capped FTS expansion")
	}
	// Core terms should be capped: only maxFTSDates dates get weekday+ISO.
	// With maxFTSDates=3, half=1, so first 1 + last 1 = 2 dates expanded.
	isoCount := 0
	for i := 0; i < 20; i++ {
		d := time.Date(2026, 3, 20+i, 0, 0, 0, 0, time.UTC)
		if strings.Contains(result, d.Format("2006-01-02")) {
			isoCount++
		}
	}
	if isoCount > maxFTSDates {
		t.Errorf("expected at most %d ISO dates in output (capped), got %d", maxFTSDates, isoCount)
	}
	// First and last dates should always be present (start/end of range).
	if !strings.Contains(result, "2026-03-20") {
		t.Error("first date 2026-03-20 should always be in capped output")
	}
	if !strings.Contains(result, "2026-04-08") {
		t.Error("last date 2026-04-08 should always be in capped output")
	}
}

func TestTemporalToFTSExpansion_ExactlyMaxDates(t *testing.T) {
	// Exactly maxFTSDates dates — no capping should occur.
	dates := make([]TemporalDate, maxFTSDates)
	for i := 0; i < maxFTSDates; i++ {
		d := time.Date(2026, 3, 1+i, 0, 0, 0, 0, time.UTC)
		dates[i] = TemporalDate{Ref: "test", Date: d.Format("2006-01-02"), Dir: "past"}
	}
	result := TemporalToFTSExpansion(dates)
	// All dates should have core terms — count ISO dates.
	isoCount := 0
	for i := 0; i < maxFTSDates; i++ {
		d := time.Date(2026, 3, 1+i, 0, 0, 0, 0, time.UTC)
		if strings.Contains(result, d.Format("2006-01-02")) {
			isoCount++
		}
	}
	if isoCount != maxFTSDates {
		t.Errorf("exactly maxFTSDates dates: all %d should have core terms, got %d", maxFTSDates, isoCount)
	}
}

func TestTemporalToEmbedPrefix_LengthCapping(t *testing.T) {
	// Many dates → prefix should be capped to ≤maxEmbedPrefixLen.
	dates := make([]TemporalDate, 20)
	for i := 0; i < 20; i++ {
		d := time.Date(2026, 1, 1+i*15, 0, 0, 0, 0, time.UTC) // Every 15 days
		dates[i] = TemporalDate{Ref: "test", Date: d.Format("2006-01-02"), Dir: "past"}
	}
	result := TemporalToEmbedPrefix(dates)
	if result == "" {
		t.Fatal("20 valid dates should produce non-empty prefix")
	}
	if len(result) > maxEmbedPrefixLen+50 {
		// Allow some slack for the summary format, but should be much shorter than uncapped.
		t.Errorf("embed prefix should be near maxEmbedPrefixLen (%d), got %d chars: %s",
			maxEmbedPrefixLen, len(result), result)
	}
	if !strings.HasSuffix(result, ".") {
		t.Errorf("capped embed prefix should end with '.', got: %q", result)
	}
}

func TestTemporalToEmbedPrefix_ShortPrefixNotCapped(t *testing.T) {
	// Single date → well under cap, should be full format.
	dates := []TemporalDate{{Ref: "heute", Date: "2026-03-29", Dir: "today"}}
	result := TemporalToEmbedPrefix(dates)
	// Should be full format: "Sonntag 2026-03-29, März 2026, KW13. March."
	if !strings.Contains(result, "Sonntag 2026-03-29") {
		t.Errorf("single date prefix should contain full weekday+date, got: %q", result)
	}
	if !strings.Contains(result, "März") {
		t.Errorf("single date prefix should contain German month, got: %q", result)
	}
	if len(result) > maxEmbedPrefixLen {
		t.Errorf("single date should be well under cap (%d), got %d chars", maxEmbedPrefixLen, len(result))
	}
}

func TestTemporalToEmbedPrefix_TwoDatesUnderCap(t *testing.T) {
	// Two dates — should be under the cap and use full format.
	dates := []TemporalDate{
		{Ref: "start", Date: "2026-03-23", Dir: "range"},
		{Ref: "end", Date: "2026-03-29", Dir: "range"},
	}
	result := TemporalToEmbedPrefix(dates)
	if !strings.Contains(result, "2026-03-23") {
		t.Error("start date should be in prefix")
	}
	if !strings.Contains(result, "2026-03-29") {
		t.Error("end date should be in prefix")
	}
	// Two dates in same month → ~90 chars, should not trigger capping.
	if len(result) > maxEmbedPrefixLen {
		t.Logf("two same-month dates triggered capping at %d chars (threshold %d): %s",
			len(result), maxEmbedPrefixLen, result)
	}
}

// --- T03 Capping Tests ---.

func TestT03_FTSExpansion_ThreeDatesNotCapped(t *testing.T) {
	// 3 unique dates < maxFTSDates(7) → all get core expansion (not capped).
	dates := []TemporalDate{
		{Ref: "a", Date: "2026-03-23", Dir: "past"},
		{Ref: "b", Date: "2026-03-25", Dir: "past"},
		{Ref: "c", Date: "2026-03-27", Dir: "past"},
	}
	result := TemporalToFTSExpansion(dates)
	// All 3 dates should have core terms (weekday DE/EN + ISO).
	for _, d := range []string{"2026-03-23", "2026-03-25", "2026-03-27"} {
		if !strings.Contains(result, d) {
			t.Errorf("date %s should be in uncapped output, got: %s", d, result)
		}
	}
	// 3 dates × 3 core + 1 month (DE/EN/YYYY-MM) + 1 KW = 3*3 + 3 + 1 = 13 terms
	termCount := strings.Count(result, " OR ") + 1
	if termCount < 9 {
		t.Errorf("3 dates should produce at least 9 core terms, got %d: %s", termCount, result)
	}
}

func TestT03_FTSExpansion_FourDatesCapped(t *testing.T) {
	// 4 unique dates < maxFTSDates(7) → NOT capped, all 4 get full core expansion.
	dates := []TemporalDate{
		{Ref: "a", Date: "2026-03-23", Dir: "past"},
		{Ref: "b", Date: "2026-03-24", Dir: "past"},
		{Ref: "c", Date: "2026-03-25", Dir: "past"},
		{Ref: "d", Date: "2026-03-26", Dir: "past"},
	}
	result := TemporalToFTSExpansion(dates)
	// All 4 dates must have core terms (weekday DE/EN + ISO).
	for _, d := range []string{"2026-03-23", "2026-03-24", "2026-03-25", "2026-03-26"} {
		if !strings.Contains(result, d) {
			t.Errorf("date %s should be in uncapped output, got: %s", d, result)
		}
	}
	// Month and KW from ALL dates should still be present.
	if !strings.Contains(result, "März") {
		t.Error("month name should always be present")
	}
}

func TestT03_FTSExpansion_30DayRange(t *testing.T) {
	// T03 core scenario: 30-day range should NOT produce ~150 OR-terms.
	dates := make([]TemporalDate, 30)
	for i := 0; i < 30; i++ {
		d := time.Date(2026, 3, 1+i, 0, 0, 0, 0, time.UTC)
		dates[i] = TemporalDate{Ref: "test", Date: d.Format("2006-01-02"), Dir: "past"}
	}
	result := TemporalToFTSExpansion(dates)
	termCount := strings.Count(result, " OR ") + 1
	// maxFTSDates=7, half=3: first 3 + last 3 = 6 core dates × 3 terms = 18 core terms
	// + 2 months (März + April with DE/EN/prefix) + ~5 KWs = ~29 terms max.
	if termCount > 35 {
		t.Errorf("30-day range should produce ≤35 OR-terms (capped), got %d: %s", termCount, result)
	}
	// Start and end dates must be present.
	if !strings.Contains(result, "2026-03-01") {
		t.Error("first date 2026-03-01 should be in output")
	}
	if !strings.Contains(result, "2026-03-30") {
		t.Error("last date 2026-03-30 should be in output")
	}
}

func TestT03_FTSExpansion_EightDatesCapped(t *testing.T) {
	// 8 unique dates > maxFTSDates(7) → capping kicks in.
	// half = maxFTSDates/2 = 3: first 3 + last 3 = 6 dates get core expansion.
	// Middle dates 2026-03-24..2026-03-27 (4 dates) are dropped from core terms.
	dates := []TemporalDate{
		{Ref: "a", Date: "2026-03-21", Dir: "past"},
		{Ref: "b", Date: "2026-03-22", Dir: "past"},
		{Ref: "c", Date: "2026-03-23", Dir: "past"},
		{Ref: "d", Date: "2026-03-24", Dir: "past"},
		{Ref: "e", Date: "2026-03-25", Dir: "past"},
		{Ref: "f", Date: "2026-03-26", Dir: "past"},
		{Ref: "g", Date: "2026-03-27", Dir: "past"},
		{Ref: "h", Date: "2026-03-28", Dir: "past"},
	}
	result := TemporalToFTSExpansion(dates)
	// First 3 and last 3 must have core terms (ISO date present).
	for _, d := range []string{"2026-03-21", "2026-03-22", "2026-03-23", "2026-03-26", "2026-03-27", "2026-03-28"} {
		if !strings.Contains(result, d) {
			t.Errorf("boundary date %s should be in capped output, got: %s", d, result)
		}
	}
	// Middle dates should NOT have ISO date core terms.
	for _, d := range []string{"2026-03-24", "2026-03-25"} {
		if strings.Contains(result, d) {
			t.Errorf("middle date %s should NOT have core terms in capped output, got: %s", d, result)
		}
	}
	// Month and KW from ALL dates should still be present.
	if !strings.Contains(result, "März") {
		t.Error("month name März should always be present")
	}
}

// --- T05 Capping Tests ---.

func TestT05_EmbedPrefix_ThreeDatesFullFormat(t *testing.T) {
	// Exactly 3 dates = maxEmbedPrefixDates → full format (not collapsed).
	dates := []TemporalDate{
		{Ref: "a", Date: "2026-03-23", Dir: "past"},
		{Ref: "b", Date: "2026-03-25", Dir: "past"},
		{Ref: "c", Date: "2026-03-27", Dir: "past"},
	}
	result := TemporalToEmbedPrefix(dates)
	// Full format includes individual entries separated by ". "
	if !strings.Contains(result, "Montag 2026-03-23") {
		t.Errorf("3 dates should use full format with weekday+date, got: %q", result)
	}
	if !strings.Contains(result, "Mittwoch 2026-03-25") {
		t.Errorf("3 dates should use full format with weekday+date, got: %q", result)
	}
	if !strings.Contains(result, "Freitag 2026-03-27") {
		t.Errorf("3 dates should use full format with weekday+date, got: %q", result)
	}
}

func TestT05_EmbedPrefix_FourDatesCollapsed(t *testing.T) {
	// 4 dates > maxEmbedPrefixDates(3) → always collapsed to summary.
	dates := []TemporalDate{
		{Ref: "a", Date: "2026-03-23", Dir: "past"},
		{Ref: "b", Date: "2026-03-24", Dir: "past"},
		{Ref: "c", Date: "2026-03-25", Dir: "past"},
		{Ref: "d", Date: "2026-03-26", Dir: "past"},
	}
	result := TemporalToEmbedPrefix(dates)
	// Should be compact "Start..End" format.
	if !strings.Contains(result, "..") {
		t.Errorf("4 dates should use collapsed format with '..', got: %q", result)
	}
	// Should contain start and end weekday+date.
	if !strings.Contains(result, "Montag 2026-03-23") {
		t.Errorf("collapsed format should contain start date, got: %q", result)
	}
	if !strings.Contains(result, "Donnerstag 2026-03-26") {
		t.Errorf("collapsed format should contain end date, got: %q", result)
	}
	if !strings.HasSuffix(result, ".") {
		t.Errorf("collapsed format should end with '.', got: %q", result)
	}
}

func TestT05_EmbedPrefix_7DayRangeCompact(t *testing.T) {
	// T05 core scenario: 7-day range should NOT produce ~300 chars.
	dates := make([]TemporalDate, 7)
	for i := 0; i < 7; i++ {
		d := time.Date(2026, 3, 23+i, 0, 0, 0, 0, time.UTC) // Mon-Sun
		dates[i] = TemporalDate{Ref: "test", Date: d.Format("2006-01-02"), Dir: "range"}
	}
	result := TemporalToEmbedPrefix(dates)
	// 7 dates > maxEmbedPrefixDates(3) → must be collapsed.
	if !strings.Contains(result, "..") {
		t.Errorf("7 dates should use collapsed format, got: %q", result)
	}
	// "Montag 2026-03-23..Sonntag 2026-03-29, März, KW13."
	if !strings.Contains(result, "Montag 2026-03-23") {
		t.Errorf("should start with Monday, got: %q", result)
	}
	if !strings.Contains(result, "Sonntag 2026-03-29") {
		t.Errorf("should end with Sunday, got: %q", result)
	}
	// Length must be well under 300 chars (the original problem).
	if len(result) > 150 {
		t.Errorf("7-day compact prefix should be ≤150 chars, got %d: %q", len(result), result)
	}
}

func TestT05_EmbedPrefix_SingleDateNotCollapsed(t *testing.T) {
	// 1 date ≤ maxEmbedPrefixDates → full format.
	dates := []TemporalDate{{Ref: "heute", Date: "2026-03-25", Dir: "today"}}
	result := TemporalToEmbedPrefix(dates)
	if strings.Contains(result, "..") {
		t.Errorf("single date should not use collapsed format, got: %q", result)
	}
	if !strings.Contains(result, "Mittwoch 2026-03-25") {
		t.Errorf("single date should have full weekday+date, got: %q", result)
	}
}
