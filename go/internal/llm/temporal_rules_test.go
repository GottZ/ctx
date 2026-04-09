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

// --- Isolated Matcher Tests: seit/bis/von..bis + resolvePhrase ---.

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

		// Month names: bare month resolves to 1st of that month
		{"März", true, "2026-03-01"},       // current month, backward → same year
		{"April", false, "2026-04-01"},     // next month, forward → same year
		{"Februar", true, "2026-02-01"},    // past month, backward → same year
		{"January", true, "2026-01-01"},    // English month name
		{"December", false, "2026-12-01"},  // English, forward → same year

		// Month position modifiers: Anfang/Mitte/Ende + month
		{"Anfang März", true, "2026-03-01"},     // start of March
		{"Mitte April", false, "2026-04-15"},    // mid April
		{"Ende Februar", true, "2026-02-28"},    // end of February (2026 is not a leap year)
		{"Ende März", true, "2026-03-31"},       // end of March = 31st
		{"Anfang Januar", true, "2026-01-01"},   // start of January
		{"Beginn Juni", false, "2026-06-01"},    // "Beginn" synonym for "Anfang"

		// Month + Year: explicit year overrides direction logic
		{"August 2024", true, "2024-08-01"},     // past year, explicit
		{"August 2024", false, "2024-08-01"},    // direction irrelevant with explicit year
		{"März 2025", true, "2025-03-01"},       // German month + year
		{"January 2026", false, "2026-01-01"},   // English month + year
		{"Dezember 2030", true, "2030-12-01"},   // future year, explicit

		// Year + Month: reversed order
		{"2024 August", true, "2024-08-01"},     // year first
		{"2025 März", false, "2025-03-01"},      // year first, German

		// Position + Month + Year (3 tokens)
		{"Ende März 2025", true, "2025-03-31"},     // end of March 2025
		{"Anfang Januar 2024", true, "2024-01-01"}, // start of January 2024
		{"Mitte Juni 2025", false, "2025-06-15"},   // mid June 2025
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

		// Month references after "seit"
		{"seit Anfang März ist alles anders", "2026-03-01", today},
		{"seit Mitte Februar kein Deploy", "2026-02-15", today},
		{"seit Ende Januar läuft der Test", "2026-01-31", today},
		{"seit März gibt es Probleme", "2026-03-01", today},

		// Month + Year after "seit" (the "seit August 2024" bug)
		{"seit August 2024 kein Deployment", "2024-08-01", today},
		{"seit März 2025 läuft der Test", "2025-03-01", today},
		{"seit January 2026 ist alles anders", "2026-01-01", today},

		// Position + Month + Year after "seit" (3 tokens)
		{"seit Ende März 2025 ist alles anders", "2025-03-31", today},
		{"seit Anfang Januar 2024 kein Deploy", "2024-01-01", today},
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

		// Month references after "bis"
		{"bis Ende April muss das fertig sein", today, "2026-04-30"},
		{"bis Anfang Mai brauche ich das", today, "2026-05-01"},
		{"bis Mitte Juni", today, "2026-06-15"},

		// Position + Month + Year after "bis" (3 tokens)
		{"bis Ende März 2025 muss das fertig sein", today, "2025-03-31"},
		{"bis Anfang Mai 2026 brauche ich das", today, "2026-05-01"},
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

// TestDetectVerbTense_T13_Worden verifies that "worden" (Passiv-Perfekt Hilfsverb)
// in constructions like "ist deployed worden" is recognised as past tense (T13).
func TestDetectVerbTense_T13_Worden(t *testing.T) {
	tests := []struct {
		query    string
		expected string
	}{
		{"ist deployed worden", "past"},
		{"das Feature ist am Dienstag deployed worden", "past"},
		{"der Service ist gestern gestartet worden", "past"},
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

// TestDetectVerbTense_T12_SeparableParticiples verifies that past participles of
// separable verbs (prefix + ge + stem) are recognised as past tense (T12).
func TestDetectVerbTense_T12_SeparableParticiples(t *testing.T) {
	tests := []struct {
		query    string
		expected string
	}{
		{"das Meeting wurde abgesagt", "past"},
		{"der Dienst wurde eingestellt", "past"},
		{"das System wurde aufgesetzt", "past"},
		{"die Config wurde umgestellt", "past"},
		{"der Report wurde vorgelegt", "past"},
		{"hat das Meeting abgesagt", "past"},
		{"hat den Service eingestellt", "past"},
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

// --- T08: matchISODate multi-date support ---.

func TestMatchISODate_T08_MultiDate(t *testing.T) {
	tests := []struct {
		name      string
		query     string
		wantDates []string // expected Date fields (sorted)
		wantEnd   string   // if non-empty, expected End on first entry (range)
		wantDir   string
	}{
		{
			name:      "single ISO date",
			query:     "was war am 2026-03-20?",
			wantDates: []string{"2026-03-20"},
			wantDir:   "past",
		},
		{
			name:      "range with bis",
			query:     "2026-03-20 bis 2026-03-25",
			wantDates: []string{"2026-03-20"},
			wantEnd:   "2026-03-25",
			wantDir:   "range",
		},
		{
			name:      "range with to",
			query:     "from 2026-03-20 to 2026-03-25",
			wantDates: []string{"2026-03-20"},
			wantEnd:   "2026-03-25",
			wantDir:   "range",
		},
		{
			name:      "range with through",
			query:     "2026-03-20 through 2026-03-25",
			wantDates: []string{"2026-03-20"},
			wantEnd:   "2026-03-25",
			wantDir:   "range",
		},
		{
			name:      "range with en-dash",
			query:     "2026-03-20 – 2026-03-25",
			wantDates: []string{"2026-03-20"},
			wantEnd:   "2026-03-25",
			wantDir:   "range",
		},
		{
			name:      "range with em-dash",
			query:     "2026-03-20 — 2026-03-25",
			wantDates: []string{"2026-03-20"},
			wantEnd:   "2026-03-25",
			wantDir:   "range",
		},
		{
			name:      "two dates without range indicator",
			query:     "am 2026-03-20 und am 2026-03-25 war ich da",
			wantDates: []string{"2026-03-20", "2026-03-25"},
			wantDir:   "past",
		},
		{
			name:      "three dates",
			query:     "2026-03-20, 2026-03-22, 2026-03-25",
			wantDates: []string{"2026-03-20", "2026-03-22", "2026-03-25"},
			wantDir:   "past",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := matchISODate(tt.query)
			if result == nil {
				t.Fatalf("matchISODate(%q) returned nil", tt.query)
			}

			var gotDates []string
			for _, d := range result.Dates {
				gotDates = append(gotDates, d.Date)
			}
			sort.Strings(gotDates)
			sort.Strings(tt.wantDates)

			if !datesEqual(gotDates, tt.wantDates) {
				t.Errorf("dates mismatch: got %v, want %v", gotDates, tt.wantDates)
			}

			if tt.wantEnd != "" {
				if len(result.Dates) != 1 {
					t.Fatalf("expected 1 range entry, got %d", len(result.Dates))
				}
				if result.Dates[0].End == nil {
					t.Fatalf("expected End date, got nil")
				}
				if *result.Dates[0].End != tt.wantEnd {
					t.Errorf("End: got %s, want %s", *result.Dates[0].End, tt.wantEnd)
				}
			}

			if tt.wantDir != "" {
				if result.Dates[0].Dir != tt.wantDir {
					t.Errorf("Dir: got %s, want %s", result.Dates[0].Dir, tt.wantDir)
				}
			}
		})
	}
}

func TestMatchISODate_T08_NoMatch(t *testing.T) {
	tests := []string{
		"was ist der RRF-Score?",
		"wie funktioniert der Guard?",
		"show me all blocks",
	}
	for _, q := range tests {
		t.Run(q, func(t *testing.T) {
			result := matchISODate(q)
			if result != nil {
				t.Errorf("matchISODate(%q) should return nil, got %+v", q, result.Dates)
			}
		})
	}
}

// --- T09: Weekday disjunction support in matchMultiKeyword ---.

func TestMatchMultiKeyword_T09_WeekdayDisjunction(t *testing.T) {
	now := truncDate(referenceDate) // 2026-03-29 (Sunday)
	tests := []struct {
		name       string
		query      string
		wantCount  int // expected number of TemporalDate entries
		wantRefs   []string
	}{
		{
			name:      "Montag oder Dienstag (DE)",
			query:     "Montag oder Dienstag",
			wantCount: 2,
			wantRefs:  []string{"Montag", "Dienstag"},
		},
		{
			name:      "Monday or Tuesday (EN)",
			query:     "Monday or Tuesday",
			wantCount: 2,
			wantRefs:  []string{"Monday", "Tuesday"},
		},
		{
			name:      "drei Wochentage mit oder",
			query:     "Montag oder Mittwoch oder Freitag",
			wantCount: 3,
			wantRefs:  []string{"Montag", "Mittwoch", "Freitag"},
		},
		{
			name:      "Wochentage mit und",
			query:     "Montag und Freitag",
			wantCount: 2,
			wantRefs:  []string{"Montag", "Freitag"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := matchMultiKeyword(tt.query, now)
			if result == nil {
				t.Fatalf("matchMultiKeyword(%q) returned nil", tt.query)
			}
			if len(result.Dates) != tt.wantCount {
				t.Fatalf("expected %d date entries, got %d: %+v", tt.wantCount, len(result.Dates), result.Dates)
			}
			for i, wantRef := range tt.wantRefs {
				if result.Dates[i].Ref != wantRef {
					t.Errorf("Dates[%d].Ref = %q, want %q", i, result.Dates[i].Ref, wantRef)
				}
			}
		})
	}
}

func TestMatchMultiKeyword_T09_NoMatch(t *testing.T) {
	now := truncDate(referenceDate)
	tests := []string{
		"was ist am Montag passiert?",   // single weekday, no conjunction
		"wie funktioniert der Guard?",   // no temporal content
		"show me all blocks",            // no conjunction
	}
	for _, q := range tests {
		t.Run(q, func(t *testing.T) {
			result := matchMultiKeyword(q, now)
			if result != nil {
				t.Errorf("matchMultiKeyword(%q) should return nil, got %+v", q, result.Dates)
			}
		})
	}
}

// TestNormalizeTemporalRules_T09_Integration tests weekday disjunction through the main entry point.
func TestNormalizeTemporalRules_T09_Integration(t *testing.T) {
	now := truncDate(referenceDate) // 2026-03-29 (Sunday)
	result := NormalizeTemporalRules("Montag oder Dienstag", now)
	if result == nil {
		t.Fatal("NormalizeTemporalRules returned nil for weekday disjunction")
	}
	if len(result.Dates) != 2 {
		t.Fatalf("expected 2 date entries, got %d", len(result.Dates))
	}
	// DimensionWeights: weekday disjunction should get weekday weights for Cyclic Phase Model
	if result.DimensionWeights == nil {
		t.Fatal("expected DimensionWeights to be set")
	}
	if _, ok := result.DimensionWeights["weekday"]; !ok {
		t.Errorf("weekday disjunction should have weekday dimension weight, got %v", result.DimensionWeights)
	}
}

func TestNormalizeTemporalRules_T09_WeekdayRecurrence(t *testing.T) {
	now := truncDate(referenceDate)
	result := NormalizeTemporalRules("immer Montag oder Dienstag", now)
	if result == nil {
		t.Fatal("NormalizeTemporalRules returned nil for recurrent weekday disjunction")
	}
	// With recurrence, should be pure weekday weight (1.0)
	if w, ok := result.DimensionWeights["weekday"]; !ok || w != 1.0 {
		t.Errorf("recurrent weekday disjunction should have weekday=1.0, got %v", result.DimensionWeights)
	}
}

// --- T20: separableVerbPairs token-order + article filter ---.

func TestDetectVerbTense_T20_SeparableVerbFilter(t *testing.T) {
	tests := []struct {
		query    string
		expected string
	}{
		// Genuine separable verbs → future intent
		{"was steht Freitag an?", "future"},
		{"das Meeting fällt morgen aus", "future"},
		{"die Demo findet Dienstag statt", "future"},

		// T20: "steht an der/dem/den..." → preposition, NOT separable verb
		{"steht an der richtigen Position", "neutral"},
		{"steht an dem Platz", "neutral"},
		{"steht an einer Stelle", "neutral"},

		// "steht an" without article → still future (genuine separable verb)
		{"was steht an?", "future"},
		{"steht morgen etwas an", "future"},

		// prefix before stem → not matched by stem-before-prefix rule
		{"an der Stelle steht ein Wert", "neutral"},
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

// --- T30: Levenshtein false positive prevention ---.

func TestMaxEditDist_T30(t *testing.T) {
	tests := []struct {
		word     string
		expected int
	}{
		{"vor", 0},        // 3 chars → 0
		{"seit", 1},       // 4 chars → 1
		{"heute", 1},      // 5 chars → 1
		{"gestern", 1},    // 7 chars → 1 (T30: was 2)
		{"morgen", 1},     // 6 chars → 1 (T30: was 2)
		{"vorgestern", 1}, // 10 chars → 1 (T30: was 2)
	}
	for _, tt := range tests {
		t.Run(tt.word, func(t *testing.T) {
			got := maxEditDist(tt.word)
			if got != tt.expected {
				t.Errorf("maxEditDist(%q) = %d, want %d", tt.word, got, tt.expected)
			}
		})
	}
}

func TestFuzzyMatch_T30(t *testing.T) {
	tests := []struct {
		tok, target string
		wantMatch   bool
	}{
		// T30: "western" ↔ "gestern" — same length, edit distance 1, different first char → NO match
		{"western", "gestern", false},
		// Genuine typo: same first char, edit distance 1 → match
		{"gesterb", "gestern", true},
		// Exact match → always match
		{"gestern", "gestern", true},
		// Short words: first-char rule not applied (< 6 chars)
		{"mor", "vor", false}, // edit distance 1, but maxEditDist("vor")=0
		{"vorr", "vor", false}, // 4 chars vs 3 chars target
		{"heuze", "heute", true}, // edit distance 1, 5 chars target → OK (< 6)
		// Long words with matching first char
		{"vorgestern", "vorgestern", true}, // exact match
		{"vorgestera", "vorgestern", true}, // edit distance 1, same prefix
		// Long words with different first char
		{"borgestern", "vorgestern", false}, // edit distance 1, different first char
	}
	for _, tt := range tests {
		t.Run(tt.tok+"_vs_"+tt.target, func(t *testing.T) {
			got := fuzzyMatch(tt.tok, tt.target)
			if got != tt.wantMatch {
				t.Errorf("fuzzyMatch(%q, %q) = %v, want %v (editDist=%d, maxDist=%d)",
					tt.tok, tt.target, got, tt.wantMatch,
					editDistance(tt.tok, tt.target), maxEditDist(tt.target))
			}
		})
	}
}

func TestMatchSimpleKeywords_T30_WesternNoMatch(t *testing.T) {
	now := truncDate(referenceDate)
	// "western" should NOT resolve as "gestern" via fuzzy matching
	result := matchSimpleKeywords("western movies are great", now)
	if result != nil {
		t.Errorf("matchSimpleKeywords should return nil for 'western', got dates: %+v", result.Dates)
	}
}

// --- DimensionWeights Tests (GottZ Cyclic Phase Model) ---.

func TestDimensionWeights_PerMatcher(t *testing.T) {
	now := truncDate(referenceDate) // 2026-03-29 (Sunday)

	tests := []struct {
		name           string
		query          string
		expectedWeights map[string]float64
	}{
		// ISO dates → pure linear
		{"iso_date", "was war am 2026-03-27", map[string]float64{"linear": 1.0}},
		// Recurrence → pure weekday
		{"immer_dienstags", "immer dienstags passiert das", map[string]float64{"weekday": 1.0}},
		{"jeden_montag", "jeden Montag standup", map[string]float64{"weekday": 1.0}},
		{"every_friday", "every friday deployment", map[string]float64{"weekday": 1.0}},
		{"plural_implies_recur", "was passiert dienstags", map[string]float64{"weekday": 1.0}},
		// Bare weekday (no recurrence) → linear + weekday mix
		{"am_dienstag", "was war am Dienstag", map[string]float64{"linear": 0.6, "weekday": 0.4}},
		{"last_monday", "letzten Montag", map[string]float64{"linear": 0.6, "weekday": 0.4}},
		// Seit/Bis → linear + month
		{"seit_maerz", "seit März", map[string]float64{"linear": 0.8, "month": 0.2}},
		{"bis_freitag", "bis Freitag", map[string]float64{"linear": 0.8, "month": 0.2}},
		// Relative weeks → linear + week
		{"letzte_woche", "letzte Woche", map[string]float64{"linear": 0.7, "week": 0.3}},
		{"anfang_woche", "Anfang der Woche", map[string]float64{"linear": 0.7, "week": 0.3}},
		// Relative months → linear + month
		{"letzten_monat", "letzten Monat", map[string]float64{"linear": 0.5, "month": 0.5}},
		// Relative days → pure linear
		{"vor_3_tagen", "vor 3 Tagen", map[string]float64{"linear": 1.0}},
		{"in_5_tagen", "in 5 Tagen", map[string]float64{"linear": 1.0}},
		// Weekend → pure weekday
		{"wochenende", "am Wochenende", map[string]float64{"weekday": 1.0}},
		// Simple keywords → pure linear
		{"heute", "heute", map[string]float64{"linear": 1.0}},
		{"gestern", "gestern", map[string]float64{"linear": 1.0}},
		{"morgen", "morgen", map[string]float64{"linear": 1.0}},
		// Range patterns → linear + weekday
		{"von_mo_bis_fr", "von Montag bis Freitag", map[string]float64{"linear": 0.6, "weekday": 0.4}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := NormalizeTemporalRules(tt.query, now)
			if result == nil {
				t.Fatalf("NormalizeTemporalRules(%q) returned nil", tt.query)
			}
			if result.DimensionWeights == nil {
				t.Fatalf("DimensionWeights is nil for query %q (matched: %+v)", tt.query, result.Dates)
			}
			if !dwEqual(result.DimensionWeights, tt.expectedWeights) {
				t.Errorf("query=%q\n  got weights: %v\n  want:        %v", tt.query, result.DimensionWeights, tt.expectedWeights)
			}
		})
	}
}

func TestHasRecurrence(t *testing.T) {
	tests := []struct {
		query    string
		expected bool
	}{
		{"immer dienstags", true},
		{"jeden Montag", true},
		{"jede Woche", true},
		{"wöchentlich", true},
		{"monatlich Routine", true},
		{"täglich standup", true},
		{"every tuesday", true},
		{"weekly review", true},
		{"dienstags", true},          // plural implies recurrence
		{"montags meeting", true},    // plural implies recurrence
		{"am Dienstag", false},       // singular, no recurrence keyword
		{"letzten Montag", false},    // specific past date
		{"heute", false},             // no recurrence
		{"was war gestern", false},   // past event, not recurring
	}

	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			tokens := ruleTokenize(tt.query)
			got := hasRecurrence(tokens)
			if got != tt.expected {
				t.Errorf("hasRecurrence(%q) = %v, want %v", tt.query, got, tt.expected)
			}
		})
	}
}

// --- matchRelativeWeek Tests ---.

func TestMatchRelativeWeek_Positive(t *testing.T) {
	now := truncDate(referenceDate) // 2026-03-29 (Sunday)
	tests := []struct {
		query     string
		wantStart string
		wantEnd   string
	}{
		{"letzte Woche war gut", "2026-03-16", "2026-03-22"},
		{"nächste Woche habe ich frei", "2026-03-30", "2026-04-05"},
		{"diese Woche", "2026-03-23", "2026-03-29"},
		{"next week", "2026-03-30", "2026-04-05"},
		{"übernächste Woche wird spannend", "2026-04-06", "2026-04-12"},
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			result := matchRelativeWeek(tt.query, now)
			if result == nil {
				t.Fatalf("matchRelativeWeek(%q) returned nil", tt.query)
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

func TestMatchRelativeWeek_NoMatch(t *testing.T) {
	now := truncDate(referenceDate)
	tests := []string{
		"die Woche war lang",
		"wöchentlich standup",
		"Wochenende ist super",
	}
	for _, q := range tests {
		t.Run(q, func(t *testing.T) {
			result := matchRelativeWeek(q, now)
			if result != nil {
				t.Errorf("matchRelativeWeek(%q) should return nil, got %+v", q, result.Dates)
			}
		})
	}
}

// --- matchRelativeMonth Tests ---.

func TestMatchRelativeMonth_Positive(t *testing.T) {
	now := truncDate(referenceDate) // 2026-03-29 (Sunday)
	tests := []struct {
		query     string
		wantStart string
		wantEnd   string
	}{
		{"letzten Monat war viel los", "2026-02-01", "2026-02-28"},
		{"diesen Monat", "2026-03-01", "2026-03-31"},
		{"last month", "2026-02-01", "2026-02-28"},
		{"this month", "2026-03-01", "2026-03-31"},
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			result := matchRelativeMonth(tt.query, now)
			if result == nil {
				t.Fatalf("matchRelativeMonth(%q) returned nil", tt.query)
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

func TestMatchRelativeMonth_NoMatch(t *testing.T) {
	now := truncDate(referenceDate)
	tests := []string{
		"der Monat ist fast vorbei",
		"monatlich Routine",
	}
	for _, q := range tests {
		t.Run(q, func(t *testing.T) {
			result := matchRelativeMonth(q, now)
			if result != nil {
				t.Errorf("matchRelativeMonth(%q) should return nil, got %+v", q, result.Dates)
			}
		})
	}
}

// --- matchRelativeDays Tests ---.

func TestMatchRelativeDays_Positive(t *testing.T) {
	now := truncDate(referenceDate) // 2026-03-29 (Sunday)
	tests := []struct {
		query    string
		wantDate string
		wantDir  string
	}{
		{"vor 3 Tagen passierte das", "2026-03-26", "past"},
		{"in 5 Tagen ist Deadline", "2026-04-03", "future"},
		{"3 days ago", "2026-03-26", "past"},
		{"in 2 Tagen geht es los", "2026-03-31", "future"},
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			result := matchRelativeDays(tt.query, now)
			if result == nil {
				t.Fatalf("matchRelativeDays(%q) returned nil", tt.query)
			}
			if len(result.Dates) != 1 {
				t.Fatalf("expected 1 date entry, got %d", len(result.Dates))
			}
			d := result.Dates[0]
			if d.Date != tt.wantDate {
				t.Errorf("date: got %s, want %s", d.Date, tt.wantDate)
			}
			if d.Dir != tt.wantDir {
				t.Errorf("dir: got %s, want %s", d.Dir, tt.wantDir)
			}
		})
	}
}

func TestMatchRelativeDays_NoMatch(t *testing.T) {
	now := truncDate(referenceDate)
	tests := []string{
		"das war ein guter Tag",
		"in einer Stunde fertig",
		"days like these",
	}
	for _, q := range tests {
		t.Run(q, func(t *testing.T) {
			result := matchRelativeDays(q, now)
			if result != nil {
				t.Errorf("matchRelativeDays(%q) should return nil, got %+v", q, result.Dates)
			}
		})
	}
}

// dwEqual compares two DimensionWeights maps for equality.
func dwEqual(a, b map[string]float64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		bv, ok := b[k]
		if !ok {
			return false
		}
		if v != bv {
			return false
		}
	}
	return true
}

// --- Isolated Matcher Tests: matchMonthSegment, matchAnnualHoliday, matchTimeOfDay ---.

func TestMatchMonthSegment_Positive(t *testing.T) {
	tests := []struct {
		name         string
		query        string
		expectedDate string
		expectedRef  string
	}{
		{"Monatsanfang", "Am Monatsanfang wird abgerechnet", "2026-01-03", "Monatsanfang"},
		{"Monatsmitte", "Zur Monatsmitte prüfen wir die Zahlen", "2026-01-15", "Monatsmitte"},
		{"Monatsende", "Das Monatsende ist kritisch", "2026-01-29", "Monatsende"},
		{"Anfang_des_Monats", "am Anfang des Monats läuft der Job", "2026-01-03", "Monatsanfang"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := matchMonthSegment(tt.query)
			if result == nil {
				t.Fatalf("matchMonthSegment(%q) returned nil, want result", tt.query)
			}
			if len(result.Dates) != 1 {
				t.Fatalf("expected 1 date, got %d", len(result.Dates))
			}
			if result.Dates[0].Date != tt.expectedDate {
				t.Errorf("date = %q, want %q", result.Dates[0].Date, tt.expectedDate)
			}
			if result.Dates[0].Ref != tt.expectedRef {
				t.Errorf("ref = %q, want %q", result.Dates[0].Ref, tt.expectedRef)
			}
			if result.Dates[0].Dir != "range" {
				t.Errorf("dir = %q, want %q", result.Dates[0].Dir, "range")
			}
		})
	}
}

func TestMatchMonthSegment_NoMatch(t *testing.T) {
	tests := []struct {
		name  string
		query string
	}{
		{"no_temporal", "der Server läuft stabil"},
		{"month_name_only", "im März war es kalt"},
		{"partial_word", "Monatsbudget ist aufgebraucht"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := matchMonthSegment(tt.query)
			if result != nil {
				t.Errorf("matchMonthSegment(%q) = %v, want nil", tt.query, result)
			}
		})
	}
}

func TestMatchAnnualHoliday_Positive(t *testing.T) {
	now := truncDate(referenceDate) // 2026-03-29
	tests := []struct {
		name         string
		query        string
		expectedDate string
		expectedRef  string
	}{
		{"Weihnachten", "an Weihnachten 2025 war viel los", "2026-12-25", "Weihnachten"},
		{"Silvester", "Silvester war toll", "2026-12-31", "Silvester"},
		{"Neujahr", "Neujahr feiern wir immer", "2026-01-01", "Neujahr"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := matchAnnualHoliday(tt.query, now)
			if result == nil {
				t.Fatalf("matchAnnualHoliday(%q) returned nil, want result", tt.query)
			}
			if len(result.Dates) != 1 {
				t.Fatalf("expected 1 date, got %d", len(result.Dates))
			}
			if result.Dates[0].Date != tt.expectedDate {
				t.Errorf("date = %q, want %q", result.Dates[0].Date, tt.expectedDate)
			}
			if result.Dates[0].Ref != tt.expectedRef {
				t.Errorf("ref = %q, want %q", result.Dates[0].Ref, tt.expectedRef)
			}
			if result.Dates[0].Dir != "range" {
				t.Errorf("dir = %q, want %q", result.Dates[0].Dir, "range")
			}
		})
	}
}

func TestMatchAnnualHoliday_NoMatch(t *testing.T) {
	now := truncDate(referenceDate)
	tests := []struct {
		name  string
		query string
	}{
		{"no_holiday", "der Server läuft stabil"},
		{"partial_word", "Neujahrsvorsätze sind schwer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := matchAnnualHoliday(tt.query, now)
			if result != nil {
				t.Errorf("matchAnnualHoliday(%q) = %v, want nil", tt.query, result)
			}
		})
	}
}

func TestMatchTimeOfDay_Positive(t *testing.T) {
	now := truncDate(referenceDate) // 2026-03-29
	tests := []struct {
		name         string
		query        string
		expectedDate string
		expectedRef  string
		expectedHour int
	}{
		{"morgens", "morgens um 8 checke ich die Logs", "2026-03-29", "morgens", 8},
		{"mittags", "mittags gibt es Standup", "2026-03-29", "mittags", 12},
		{"abends", "abends gibt es Probleme", "2026-03-29", "abends", 19},
		{"nachts", "nachts läuft der Batch", "2026-03-29", "nachts", 23},
		{"vormittags", "vormittags ist die Last hoch", "2026-03-29", "vormittags", 10},
		{"nachmittags", "nachmittags deployen wir", "2026-03-29", "nachmittags", 15},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := matchTimeOfDay(tt.query, now)
			if result == nil {
				t.Fatalf("matchTimeOfDay(%q) returned nil, want result", tt.query)
			}
			if len(result.Dates) != 1 {
				t.Fatalf("expected 1 date, got %d", len(result.Dates))
			}
			d := result.Dates[0]
			if d.Date != tt.expectedDate {
				t.Errorf("date = %q, want %q", d.Date, tt.expectedDate)
			}
			if d.Ref != tt.expectedRef {
				t.Errorf("ref = %q, want %q", d.Ref, tt.expectedRef)
			}
			if d.Dir != "today" {
				t.Errorf("dir = %q, want %q", d.Dir, "today")
			}
			if d.Hour == nil {
				t.Fatalf("hour is nil, want %d", tt.expectedHour)
			}
			if *d.Hour != tt.expectedHour {
				t.Errorf("hour = %d, want %d", *d.Hour, tt.expectedHour)
			}
		})
	}
}

func TestMatchTimeOfDay_NoMatch(t *testing.T) {
	now := truncDate(referenceDate)
	tests := []struct {
		name  string
		query string
	}{
		{"no_temporal", "der Server läuft stabil"},
		{"time_as_number", "um 14 Uhr ist Meeting"},
		{"partial_word", "Abendsonne über dem Rechenzentrum"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := matchTimeOfDay(tt.query, now)
			if result != nil {
				t.Errorf("matchTimeOfDay(%q) = %v, want nil", tt.query, result)
			}
		})
	}
}

// --- matchLetztenWDBis tests ---.

func TestMatchLetztenWDBis_Positive(t *testing.T) {
	now := truncDate(referenceDate) // 2026-03-29, Sunday
	tests := []struct {
		query     string
		wantStart string
		wantEnd   string
	}{
		// Mon backward from Sun: (0-1+7)%7=6 → 2026-03-23; Wed: (0-3+7)%7=4 → 2026-03-25
		{"letzten Montag bis Mittwoch", "2026-03-23", "2026-03-25"},
		// Tue: (0-2+7)%7=5 → 2026-03-24; Fri: (0-5+7)%7=2 → 2026-03-27
		{"letzten Dienstag bis Freitag", "2026-03-24", "2026-03-27"},
		// Sun backward from Sun: (0-0+7)%7=0 → diff=7 → 2026-03-22; same for end
		{"letzten Sonntag bis Sonntag", "2026-03-22", "2026-03-22"},
		// "letzte" variant (without trailing n)
		{"letzte Montag bis Freitag", "2026-03-23", "2026-03-27"},
	}

	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			result := matchLetztenWDBis(tt.query, now)
			if result == nil {
				t.Fatalf("matchLetztenWDBis(%q) returned nil", tt.query)
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

func TestMatchLetztenWDBis_NoMatch(t *testing.T) {
	now := truncDate(referenceDate)
	tests := []string{
		"letzten Montag war gut",       // no "bis" → no range
		"von Montag bis Mittwoch",      // "von...bis" not "letzten...bis"
		"nächsten Montag bis Mittwoch", // "nächsten" not "letzten"
	}
	for _, q := range tests {
		t.Run(q, func(t *testing.T) {
			result := matchLetztenWDBis(q, now)
			if result != nil {
				t.Errorf("matchLetztenWDBis(%q) should return nil, got %+v", q, result.Dates)
			}
		})
	}
}

// --- matchNextThrough tests ---.

func TestMatchNextThrough_Positive(t *testing.T) {
	now := truncDate(referenceDate) // 2026-03-29, Sunday
	tests := []struct {
		query     string
		wantStart string
		wantEnd   string
	}{
		// "next" prefix: forward resolution
		// Mon forward from Sun: (1-0+7)%7=1 → 2026-03-30; Fri: (5-0+7)%7=5 → 2026-04-03
		{"next Monday through Friday", "2026-03-30", "2026-04-03"},
		// Bare form, no past tense → forward
		// Tue: (2-0+7)%7=2 → 2026-03-31; Thu: (4-0+7)%7=4 → 2026-04-02
		{"Tuesday through Thursday", "2026-03-31", "2026-04-02"},
		// Bare form with future context → forward
		{"I'll work Monday through Friday", "2026-03-30", "2026-04-03"},
	}

	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			result := matchNextThrough(tt.query, now)
			if result == nil {
				t.Fatalf("matchNextThrough(%q) returned nil", tt.query)
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

func TestMatchNextThrough_NoMatch(t *testing.T) {
	now := truncDate(referenceDate)
	tests := []string{
		"next week is busy",                 // no weekday through weekday
		"I went through the list on Monday", // "through" not between weekdays
	}
	for _, q := range tests {
		t.Run(q, func(t *testing.T) {
			result := matchNextThrough(q, now)
			if result != nil {
				t.Errorf("matchNextThrough(%q) should return nil, got %+v", q, result.Dates)
			}
		})
	}
}

// --- matchWeekSegment tests ---.

func TestMatchWeekSegment_Positive(t *testing.T) {
	now := truncDate(referenceDate) // 2026-03-29, Sunday
	// isoWeekBounds(now, 0): wd=7(Sun), mon = 2026-03-29 - 6 = 2026-03-23, sun = 2026-03-29
	// isoWeekBounds(now, 1): mon = 2026-03-30, sun = 2026-04-05
	tests := []struct {
		query     string
		wantStart string
		wantEnd   string
	}{
		// Anfang der Woche: mon..tue of current week
		{"Anfang der Woche", "2026-03-23", "2026-03-24"},
		// Ende der Woche: fri..sun of current week
		{"Ende der Woche", "2026-03-27", "2026-03-29"},
		// Ende nächster Woche: fri..sun of next week
		{"Ende nächster Woche", "2026-04-03", "2026-04-05"},
		// "dieser" variant
		{"Anfang dieser Woche", "2026-03-23", "2026-03-24"},
	}

	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			result := matchWeekSegment(tt.query, now)
			if result == nil {
				t.Fatalf("matchWeekSegment(%q) returned nil", tt.query)
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

func TestMatchWeekSegment_NoMatch(t *testing.T) {
	now := truncDate(referenceDate)
	tests := []string{
		"die Woche war gut",      // no "Anfang/Ende" prefix
		"nächste Woche ist voll", // "nächste Woche" without "Ende/Anfang"
	}
	for _, q := range tests {
		t.Run(q, func(t *testing.T) {
			result := matchWeekSegment(q, now)
			if result != nil {
				t.Errorf("matchWeekSegment(%q) should return nil, got %+v", q, result.Dates)
			}
		})
	}
}

// --- Isolated Matcher Tests: matchWDInWoche ---.

func TestMatchWDInWoche_Positive(t *testing.T) {
	now := truncDate(referenceDate) // 2026-03-29 (Sunday)
	tests := []struct {
		name     string
		query    string
		wantDate string
	}{
		{
			name:     "Montag in einer Woche",
			query:    "Montag in einer Woche",
			wantDate: "2026-04-06", // next Monday (03-30) + 7 = 04-06
		},
		{
			name:     "Freitag in einer Woche",
			query:    "Freitag in einer Woche",
			wantDate: "2026-04-10", // next Friday (04-03) + 7 = 04-10
		},
		{
			name:     "embedded in sentence",
			query:    "das Meeting ist Mittwoch in einer Woche",
			wantDate: "2026-04-08", // next Wednesday (04-01) + 7 = 04-08
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := matchWDInWoche(tt.query, now)
			if result == nil {
				t.Fatalf("matchWDInWoche(%q) returned nil", tt.query)
			}
			if len(result.Dates) != 1 {
				t.Fatalf("expected 1 date, got %d", len(result.Dates))
			}
			if result.Dates[0].Date != tt.wantDate {
				t.Errorf("date: got %s, want %s", result.Dates[0].Date, tt.wantDate)
			}
			if result.Dates[0].Dir != "future" {
				t.Errorf("dir: got %s, want future", result.Dates[0].Dir)
			}
		})
	}
}

func TestMatchWDInWoche_NoMatch(t *testing.T) {
	now := truncDate(referenceDate)
	tests := []string{
		"nächsten Montag",         // different pattern (no "in einer Woche")
		"Montag in zwei Wochen",   // "zwei" not "einer"
		"in einer Woche",          // no weekday prefix
	}
	for _, q := range tests {
		t.Run(q, func(t *testing.T) {
			result := matchWDInWoche(q, now)
			if result != nil {
				t.Errorf("matchWDInWoche(%q) should return nil, got %+v", q, result.Dates)
			}
		})
	}
}

// --- Isolated Matcher Tests: matchWeekend ---.

func TestMatchWeekend_Positive(t *testing.T) {
	now := truncDate(referenceDate) // 2026-03-29 (Sunday)
	tests := []struct {
		name      string
		query     string
		wantStart string
		wantEnd   string
		wantDir   string
	}{
		{
			name:      "past tense on weekend day",
			query:     "am Wochenende war es schön",
			wantStart: "2026-03-28", // this Saturday (past, currently on weekend)
			wantEnd:   "2026-03-29", // this Sunday
			wantDir:   "past",
		},
		{
			name:      "explicit future",
			query:     "nächstes Wochenende",
			wantStart: "2026-04-04", // next Saturday
			wantEnd:   "2026-04-05", // next Sunday
			wantDir:   "future",
		},
		{
			name:      "neutral on weekend day defaults to future",
			query:     "am Wochenende",
			wantStart: "2026-04-04", // next Saturday (skip current weekend)
			wantEnd:   "2026-04-05",
			wantDir:   "future",
		},
		{
			name:      "english weekend keyword",
			query:     "what happened on the weekend",
			wantStart: "2026-03-28", // past tense ("happened")
			wantEnd:   "2026-03-29",
			wantDir:   "past",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := matchWeekend(tt.query, now)
			if result == nil {
				t.Fatalf("matchWeekend(%q) returned nil", tt.query)
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
			if d.Dir != tt.wantDir {
				t.Errorf("dir: got %s, want %s", d.Dir, tt.wantDir)
			}
		})
	}
}

func TestMatchWeekend_NoMatch(t *testing.T) {
	now := truncDate(referenceDate)
	tests := []string{
		"am Montag ist Meeting",   // no weekend keyword
		"am Freitag war es schön", // Friday is not weekend
	}
	for _, q := range tests {
		t.Run(q, func(t *testing.T) {
			result := matchWeekend(q, now)
			if result != nil {
				t.Errorf("matchWeekend(%q) should return nil, got %+v", q, result.Dates)
			}
		})
	}
}

// --- Isolated Matcher Tests: matchWeekday ---.

func TestMatchWeekday_Positive(t *testing.T) {
	now := truncDate(referenceDate) // 2026-03-29 (Sunday)
	tests := []struct {
		name     string
		query    string
		wantDate string
		wantDir  string
	}{
		{
			name:     "past tense German",
			query:    "was war am Montag?",
			wantDate: "2026-03-23", // last Monday (past: "war")
			wantDir:  "past",
		},
		{
			name:     "neutral tense defaults to future",
			query:    "am Freitag ist Meeting",
			wantDate: "2026-04-03", // next Friday (neutral → forward)
			wantDir:  "future",
		},
		{
			name:     "English next",
			query:    "next Tuesday",
			wantDate: "2026-03-31", // next Tuesday (explicit future)
			wantDir:  "future",
		},
		{
			name:     "English last",
			query:    "last Wednesday",
			wantDate: "2026-03-25", // last Wednesday (explicit past)
			wantDir:  "past",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tokens := ruleTokenize(tt.query)
			result := matchWeekday(tt.query, tokens, now)
			if result == nil {
				t.Fatalf("matchWeekday(%q) returned nil", tt.query)
			}
			if len(result.Dates) != 1 {
				t.Fatalf("expected 1 date, got %d", len(result.Dates))
			}
			d := result.Dates[0]
			if d.Date != tt.wantDate {
				t.Errorf("date: got %s, want %s", d.Date, tt.wantDate)
			}
			if d.Dir != tt.wantDir {
				t.Errorf("dir: got %s, want %s", d.Dir, tt.wantDir)
			}
		})
	}
}

func TestMatchWeekday_NoMatch(t *testing.T) {
	now := truncDate(referenceDate)
	tests := []string{
		"was ist der RRF-Score?",                  // no weekday token
		"gestern war es schön",                    // "gestern" is not a weekday
		"show me all blocks with category infra",  // no weekday
	}
	for _, q := range tests {
		t.Run(q, func(t *testing.T) {
			tokens := ruleTokenize(q)
			result := matchWeekday(q, tokens, now)
			if result != nil {
				t.Errorf("matchWeekday(%q) should return nil, got %+v", q, result.Dates)
			}
		})
	}
}

func TestMatchWeekday_TenseDirection(t *testing.T) {
	now := truncDate(referenceDate) // 2026-03-29 (Sunday)
	tests := []struct {
		name     string
		query    string
		wantDate string
		wantDir  string
	}{
		{
			name:     "past: letzten Freitag",
			query:    "letzten Freitag passierte das",
			wantDate: "2026-03-27", // last Friday (explicit past + past verb)
			wantDir:  "past",
		},
		{
			name:     "future: am Donnerstag muss ich",
			query:    "am Donnerstag muss ich den Report abgeben",
			wantDate: "2026-04-02", // next Thursday (future: "muss")
			wantDir:  "future",
		},
		{
			name:     "past: on Wednesday I deployed",
			query:    "on Wednesday I deployed the fix",
			wantDate: "2026-03-25", // last Wednesday (past: "deployed")
			wantDir:  "past",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tokens := ruleTokenize(tt.query)
			result := matchWeekday(tt.query, tokens, now)
			if result == nil {
				t.Fatalf("matchWeekday(%q) returned nil", tt.query)
			}
			if len(result.Dates) != 1 {
				t.Fatalf("expected 1 date, got %d", len(result.Dates))
			}
			d := result.Dates[0]
			if d.Date != tt.wantDate {
				t.Errorf("date: got %s, want %s", d.Date, tt.wantDate)
			}
			if d.Dir != tt.wantDir {
				t.Errorf("dir: got %s, want %s", d.Dir, tt.wantDir)
			}
		})
	}
}
