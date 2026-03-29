package rrf

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// TestCase mirrors the JSON test case structure.
type TestCase struct {
	ID                string   `json:"id"`
	Category          string   `json:"category"`
	Query             string   `json:"query"`
	ExpectedDates     []string `json:"expected_dates"`     // ISO dates or null
	ExpectedDirection string   `json:"expected_direction"` // past/future/today/range/none/vague_past/vague_future
	Difficulty        string   `json:"difficulty"`
	Rationale         string   `json:"rationale"`
}

type TestSuite struct {
	Meta  json.RawMessage `json:"_meta"`
	Tests []TestCase      `json:"tests"`
}

// Reference date for all tests: Sunday 2026-03-29
var refTime = time.Date(2026, 3, 29, 12, 0, 0, 0, time.UTC)

func loadTestCases(t *testing.T) []TestCase {
	t.Helper()
	data, err := os.ReadFile("temporal_test_cases.json")
	if err != nil {
		t.Fatalf("failed to load test cases: %v", err)
	}
	var suite TestSuite
	if err := json.Unmarshal(data, &suite); err != nil {
		t.Fatalf("failed to parse test cases: %v", err)
	}
	return suite.Tests
}

// TestExpandTemporal_CurrentImplementation tests the currently implemented subset:
// simple single-day keywords (heute, gestern, morgen, vorgestern, übermorgen,
// today, yesterday, tomorrow) with typo tolerance.
func TestExpandTemporal_CurrentImplementation(t *testing.T) {
	// These are the test IDs that the current ExpandTemporal can handle.
	supported := map[string]bool{
		"S01": true, "S02": true, "S03": true, "S04": true, "S05": true,
		"S06": true, "S07": true, "S08": true, "S09": true, "S10": true,
		"E01": true, // multi-keyword (heute + morgen)
		"E07": true, // multi-keyword (gestern + vorgestern)
		"E08": true, // empty query
		"E09": true, // repeated keyword dedup
		// no_temporal_intent should all return empty
		"N01": true, "N02": true, "N03": true, "N04": true, "N05": true,
	}

	cases := loadTestCases(t)
	for _, tc := range cases {
		if !supported[tc.ID] {
			continue
		}
		t.Run(tc.ID+"_"+tc.Category, func(t *testing.T) {
			result := ExpandTemporal(tc.Query, refTime)

			if tc.ExpectedDirection == "none" {
				if result != "" {
					t.Errorf("[%s] expected no temporal match, got: %q", tc.ID, result)
				}
				return
			}

			if tc.ExpectedDates == nil {
				if result != "" {
					t.Errorf("[%s] expected empty result for null dates, got: %q", tc.ID, result)
				}
				return
			}

			if result == "" {
				t.Fatalf("[%s] expected temporal match for %q, got empty string", tc.ID, tc.Query)
			}

			// Verify each expected ISO date appears in the result
			for _, date := range tc.ExpectedDates {
				if !strings.Contains(result, date) {
					t.Errorf("[%s] expected date %s in result, got: %q", tc.ID, date, result)
				}
			}
		})
	}
}

// TestExpandTemporal_NoTemporalIntent verifies that non-temporal queries
// return empty strings — no false positives.
func TestExpandTemporal_NoTemporalIntent(t *testing.T) {
	cases := loadTestCases(t)
	for _, tc := range cases {
		if tc.Category != "no_temporal_intent" {
			continue
		}
		t.Run(tc.ID, func(t *testing.T) {
			result := ExpandTemporal(tc.Query, refTime)
			if result != "" {
				t.Errorf("[%s] query %q should have no temporal intent, got: %q",
					tc.ID, tc.Query, result)
			}
		})
	}
}

// TestExpandTemporal_Deduplication verifies that repeated keywords
// produce deduplicated date output.
func TestExpandTemporal_Deduplication(t *testing.T) {
	result := ExpandTemporal("heute heute heute", refTime)
	if result == "" {
		t.Fatal("expected non-empty result for 'heute heute heute'")
	}

	// Count ISO date occurrences — should appear exactly once
	isoDate := refTime.Format("2006-01-02")
	count := strings.Count(result, isoDate)
	if count != 1 {
		t.Errorf("expected ISO date %s to appear once, appeared %d times in: %q",
			isoDate, count, result)
	}
}

// TestExpandTemporal_EmptyQuery verifies empty input does not panic.
func TestExpandTemporal_EmptyQuery(t *testing.T) {
	result := ExpandTemporal("", refTime)
	if result != "" {
		t.Errorf("expected empty result for empty query, got: %q", result)
	}
}

// TestLevenshtein verifies the distance function against known pairs.
func TestLevenshtein(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"heute", "heute", 0},
		{"heite", "heute", 1},  // S03 typo
		{"morgrn", "morgen", 1}, // S09 typo
		{"todya", "today", 2},   // E03 typo
		{"yesrerday", "yesterday", 1}, // S10 typo (r→t at pos 4, single substitution)
		{"", "", 0},
		{"a", "", 1},
		{"", "b", 1},
		{"kitten", "sitting", 3},
	}
	for _, tt := range tests {
		t.Run(tt.a+"→"+tt.b, func(t *testing.T) {
			got := levenshtein(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("levenshtein(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

// TestMaxEditDistance verifies the tolerance tiers.
func TestMaxEditDistance(t *testing.T) {
	tests := []struct {
		word string
		want int
	}{
		{"ab", 0},       // 2 chars → 0
		{"abc", 0},      // 3 chars → 0
		{"abcd", 1},     // 4 chars → 1
		{"today", 1},    // 5 chars → 1
		{"morgen", 2},   // 6 chars → 2
		{"gestern", 2},  // 7 chars → 2
		{"yesterday", 2}, // 9 chars → 2
		{"übermorgen", 2}, // 10 chars (ü = 1 rune) → 2
	}
	for _, tt := range tests {
		t.Run(tt.word, func(t *testing.T) {
			got := maxEditDistance(tt.word)
			if got != tt.want {
				t.Errorf("maxEditDistance(%q) = %d, want %d", tt.word, got, tt.want)
			}
		})
	}
}

// Placeholder tests for future implementation categories.
// These are skipped until the corresponding features are built.

func TestExpandTemporal_WeekdayWithTense(t *testing.T) {
	t.Skip("weekday + tense resolution not yet implemented")
	// When implemented, load cases with category "weekday_with_tense"
	// and verify against expected_dates.
}

func TestExpandTemporal_RelativeRanges(t *testing.T) {
	t.Skip("relative range resolution not yet implemented")
	// When implemented, load cases with category "relative_range"
	// and verify range pairs.
}

func TestExpandTemporal_CompoundComplex(t *testing.T) {
	t.Skip("compound/complex temporal resolution not yet implemented")
	// When implemented, load cases with category "compound_complex"
	// and verify range/offset combinations.
}

func TestExpandTemporal_AmbiguousContextual(t *testing.T) {
	t.Skip("ambiguous/contextual temporal resolution not yet implemented")
	// When implemented, load cases with category "ambiguous_contextual"
	// and verify vague window handling.
}

func TestExpandTemporal_EdgeCases_Future(t *testing.T) {
	t.Skip("advanced edge cases not yet implemented")
	// E02 (range from relative-to-relative), E04 (Morgen vs morgen),
	// E05 (hyphenated compounds), E06 (ISO date literals), E10 (vor 0 Tagen)
}
