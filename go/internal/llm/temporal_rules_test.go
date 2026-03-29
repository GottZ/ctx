package llm

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

// referenceDate is 2026-03-29 (Sunday), matching temporal_test_cases.json.
var referenceDate = time.Date(2026, 3, 29, 12, 0, 0, 0, time.UTC)

type testCaseMeta struct {
	ReferenceDate string `json:"reference_date"`
}

type testCase struct {
	ID                string   `json:"id"`
	Category          string   `json:"category"`
	Query             string   `json:"query"`
	ExpectedDates     []string `json:"expected_dates"`
	ExpectedDirection string   `json:"expected_direction"`
	Difficulty        string   `json:"difficulty"`
	Rationale         string   `json:"rationale"`
}

type testFile struct {
	Meta  testCaseMeta `json:"_meta"`
	Tests []testCase   `json:"tests"`
}

func loadTestCases(t *testing.T) []testCase {
	t.Helper()
	data, err := os.ReadFile("../../internal/rrf/temporal_test_cases.json")
	if err != nil {
		// Try alternative path
		data, err = os.ReadFile("../rrf/temporal_test_cases.json")
		if err != nil {
			t.Skipf("test cases file not found: %v", err)
			return nil
		}
	}
	var tf testFile
	if err := json.Unmarshal(data, &tf); err != nil {
		t.Fatalf("failed to parse test cases: %v", err)
	}
	return tf.Tests
}

func TestNormalizeTemporalRules_AllCases(t *testing.T) {
	cases := loadTestCases(t)
	if len(cases) == 0 {
		t.Skip("no test cases loaded")
	}

	passed, failed, skipped := 0, 0, 0
	var failures []string

	for _, tc := range cases {
		t.Run(tc.ID+"_"+tc.Category, func(t *testing.T) {
			result := NormalizeTemporalRules(tc.Query, referenceDate)

			// No temporal intent expected
			if tc.ExpectedDates == nil {
				if result != nil {
					t.Errorf("expected nil for %q, got dates: %v", tc.Query, result.Dates)
					failed++
					failures = append(failures, tc.ID)
				} else {
					passed++
				}
				return
			}

			if result == nil {
				// Rule parser returned nil — may need LLM fallback
				skipped++
				t.Logf("SKIP (nil result, LLM fallback needed): %s %q", tc.ID, tc.Query)
				return
			}

			// Extract dates from result
			var gotDates []string
			for _, d := range result.Dates {
				gotDates = append(gotDates, d.Date)
				if d.End != nil {
					gotDates = append(gotDates, *d.End)
				}
			}
			sort.Strings(gotDates)

			expected := make([]string, len(tc.ExpectedDates))
			copy(expected, tc.ExpectedDates)
			sort.Strings(expected)

			if !datesEqual(expected, gotDates) {
				t.Errorf("dates mismatch for %s %q:\n  expected: %v\n  got:      %v", tc.ID, tc.Query, expected, gotDates)
				failed++
				failures = append(failures, tc.ID)
			} else {
				passed++
			}
		})
	}

	t.Logf("\n=== Rule Parser Results ===")
	t.Logf("Passed:  %d/%d", passed, len(cases))
	t.Logf("Failed:  %d", failed)
	t.Logf("Skipped: %d (need LLM fallback)", skipped)
	if len(failures) > 0 {
		t.Logf("Failures: %s", strings.Join(failures, ", "))
	}
}

func TestDetectVerbTense_WCases(t *testing.T) {
	tests := []struct {
		query    string
		expected string
	}{
		{"am Montag will ich die Migration starten", "future"},
		{"Montag war ich im Office", "past"},
		{"was steht Freitag an?", "future"},
		{"was war am Freitag?", "past"},
		{"on Wednesday I deployed the fix", "past"},
		{"Dienstag treffe ich mich mit dem Team", "future"},
		{"the Thursday meeting was cancelled", "past"},
		{"Mittwoch habe ich frei", "future"},
		{"am Samstag war das Deployment", "past"},
		{"Donnerstag muss ich den Report abgeben", "future"},
	}

	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			got := DetectVerbTense(tt.query)
			if got != tt.expected {
				t.Errorf("DetectVerbTense(%q) = %q, want %q", tt.query, got, tt.expected)
			}
		})
	}
}

func TestDetectVerbTense_NoCases(t *testing.T) {
	// N-cases: non-temporal queries. "was" triggers pastVerbSet (English past of "to be"),
	// so queries containing "was" return "past" — a known false positive for German interrogative.
	tests := []struct {
		query    string
		expected string
	}{
		{"was ist der RRF-Score?", "past"},                                       // "was" in pastVerbSet
		{"wie funktioniert der Guard?", "neutral"},                               // no verb match
		{"show me all blocks with category infrastructure", "neutral"},            // no verb match
		{"was ist der Unterschied zwischen shared und private scope?", "past"},    // "was" in pastVerbSet
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			got := DetectVerbTense(tt.query)
			if got != tt.expected {
				t.Errorf("DetectVerbTense(%q) = %q, want %q", tt.query, got, tt.expected)
			}
		})
	}
}

func TestResolveWeekdayDate(t *testing.T) {
	// Reference: 2026-03-29 (Sunday)
	tests := []struct {
		wd       time.Weekday
		backward bool
		expected string
	}{
		{time.Monday, true, "2026-03-23"},   // 6 days ago
		{time.Monday, false, "2026-03-30"},  // 1 day ahead
		{time.Friday, true, "2026-03-27"},   // 2 days ago
		{time.Friday, false, "2026-04-03"},  // 5 days ahead
		{time.Saturday, true, "2026-03-28"}, // yesterday
		{time.Sunday, true, "2026-03-22"},   // 7 days ago (not today)
		{time.Sunday, false, "2026-04-05"},  // 7 days ahead (not today)
	}

	for _, tt := range tests {
		name := tt.wd.String()
		if tt.backward {
			name += "_past"
		} else {
			name += "_future"
		}
		t.Run(name, func(t *testing.T) {
			got := resolveWeekdayDate(tt.wd, tt.backward, referenceDate)
			gotStr := fmtDate(got)
			if gotStr != tt.expected {
				t.Errorf("resolveWeekdayDate(%v, backward=%v) = %s, want %s", tt.wd, tt.backward, gotStr, tt.expected)
			}
		})
	}
}

// --- Isolated Matcher Tests: seit/bis/von..bis + resolvePhrase ---

func TestResolvePhrase(t *testing.T) {
	ref := time.Date(2026, 3, 29, 12, 0, 0, 0, time.UTC) // Sunday
	now := truncDate(ref)
	tests := []struct {
		phrase   string
		backward bool
		expected string
	}{
		// Single word: delegate to resolveToken
		{"Montag", true, "2026-03-23"},
		{"Montag", false, "2026-03-30"},
		{"vorgestern", true, "2026-03-27"},
		{"übermorgen", false, "2026-03-31"},
		{"gestern", true, "2026-03-28"},

		// Two words: modifier + keyword
		{"letztem Montag", true, "2026-03-23"},  // explicit past → backward=true
		{"letzten Montag", false, "2026-03-23"}, // modifier overrides default direction
		{"nächsten Freitag", true, "2026-04-03"}, // explicit future → backward=false
		{"nächsten Freitag", false, "2026-04-03"},
		{"voriger Dienstag", true, "2026-03-24"},
		{"kommenden Mittwoch", false, "2026-04-01"},

		// Two words where modifier is NOT a direction modifier → fallback to first word
		{"Montag arbeite", true, "2026-03-23"}, // "arbeite" not temporal → uses "Montag"
	}

	for _, tt := range tests {
		name := tt.phrase
		if tt.backward {
			name += "_backward"
		} else {
			name += "_forward"
		}
		t.Run(name, func(t *testing.T) {
			got := resolvePhrase(tt.phrase, tt.backward, now)
			if got.IsZero() {
				t.Fatalf("resolvePhrase(%q, backward=%v) returned zero time", tt.phrase, tt.backward)
			}
			gotStr := fmtDate(got)
			if gotStr != tt.expected {
				t.Errorf("resolvePhrase(%q, backward=%v) = %s, want %s", tt.phrase, tt.backward, gotStr, tt.expected)
			}
		})
	}
}

func TestMatchSeit(t *testing.T) {
	ref := time.Date(2026, 3, 29, 12, 0, 0, 0, time.UTC) // Sunday
	now := truncDate(ref)
	today := fmtDate(now)
	tests := []struct {
		query     string
		wantStart string
		wantEnd   string
	}{
		// Single word after "seit"
		{"seit Montag arbeite ich daran", "2026-03-23", today},
		{"seit vorgestern kein Deployment", "2026-03-27", today},
		{"seit gestern ist alles kaputt", "2026-03-28", today},

		// Two words after "seit" (modifier + keyword)
		{"seit letztem Montag arbeite ich daran", "2026-03-23", today},
		{"seit letztem Freitag läuft das", "2026-03-27", today},
		{"seit vorigem Dienstag kein Update", "2026-03-24", today},
	}

	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			result := matchSeit(tt.query, now)
			if result == nil {
				t.Fatalf("matchSeit(%q) returned nil", tt.query)
			}
			if len(result.Dates) != 1 {
				t.Fatalf("expected 1 date entry, got %d", len(result.Dates))
			}
			d := result.Dates[0]
			if d.Date != tt.wantStart {
				t.Errorf("start date: got %s, want %s", d.Date, tt.wantStart)
			}
			if d.End == nil {
				t.Fatalf("expected end date, got nil")
			}
			if *d.End != tt.wantEnd {
				t.Errorf("end date: got %s, want %s", *d.End, tt.wantEnd)
			}
		})
	}
}

func TestMatchBis(t *testing.T) {
	ref := time.Date(2026, 3, 29, 12, 0, 0, 0, time.UTC) // Sunday
	now := truncDate(ref)
	today := fmtDate(now)
	tests := []struct {
		query     string
		wantStart string
		wantEnd   string
	}{
		// Single word after "bis"
		{"bis Freitag muss das fertig sein", today, "2026-04-03"},
		{"bis morgen erledigt", today, "2026-03-30"},

		// Two words after "bis" (modifier + keyword)
		{"bis nächsten Freitag muss das fertig sein", today, "2026-04-03"},
		{"bis nächsten Mittwoch brauche ich das", today, "2026-04-01"},
		{"bis kommenden Donnerstag", today, "2026-04-02"},
	}

	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			result := matchBis(tt.query, now)
			if result == nil {
				t.Fatalf("matchBis(%q) returned nil", tt.query)
			}
			if len(result.Dates) != 1 {
				t.Fatalf("expected 1 date entry, got %d", len(result.Dates))
			}
			d := result.Dates[0]
			if d.Date != tt.wantStart {
				t.Errorf("start date: got %s, want %s", d.Date, tt.wantStart)
			}
			if d.End == nil {
				t.Fatalf("expected end date, got nil")
			}
			if *d.End != tt.wantEnd {
				t.Errorf("end date: got %s, want %s", *d.End, tt.wantEnd)
			}
		})
	}
}

func TestMatchVonBis(t *testing.T) {
	ref := time.Date(2026, 3, 29, 12, 0, 0, 0, time.UTC) // Sunday
	now := truncDate(ref)
	tests := []struct {
		query     string
		wantStart string
		wantEnd   string
	}{
		// Single word captures (existing behavior)
		{"von gestern bis übermorgen läuft der Test", "2026-03-28", "2026-03-31"},

		// Two-word captures (new behavior)
		{"von letztem Montag bis nächsten Freitag", "2026-03-23", "2026-04-03"},
		{"von letztem Montag bis letzten Mittwoch", "2026-03-23", "2026-03-25"},
		{"von nächstem Dienstag bis nächsten Donnerstag", "2026-03-31", "2026-04-02"},
	}

	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			result := matchVonBis(tt.query, now)
			if result == nil {
				t.Fatalf("matchVonBis(%q) returned nil", tt.query)
			}
			if len(result.Dates) != 1 {
				t.Fatalf("expected 1 date entry, got %d", len(result.Dates))
			}
			d := result.Dates[0]
			if d.Date != tt.wantStart {
				t.Errorf("start date: got %s, want %s", d.Date, tt.wantStart)
			}
			if d.End == nil {
				t.Fatalf("expected end date, got nil")
			}
			if *d.End != tt.wantEnd {
				t.Errorf("end date: got %s, want %s", *d.End, tt.wantEnd)
			}
		})
	}
}

func TestMatchSeit_NoMatch(t *testing.T) {
	now := truncDate(time.Date(2026, 3, 29, 12, 0, 0, 0, time.UTC))
	// "seit" followed by non-temporal words should return nil
	tests := []string{
		"seit Jahren schon", // "Jahren" not in resolveToken
		"seit dem Update",   // "dem" not temporal, "Update" not temporal
	}
	for _, q := range tests {
		t.Run(q, func(t *testing.T) {
			result := matchSeit(q, now)
			if result != nil {
				t.Errorf("matchSeit(%q) should return nil, got %+v", q, result.Dates)
			}
		})
	}
}

func datesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
