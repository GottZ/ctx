package store

import (
	"testing"
	"time"
)

// --- T25: ExtractDates rejects Go-normalized invalid dates ---.

func TestExtractDates_T25_InvalidDateRejection(t *testing.T) {
	tests := []struct {
		name      string
		content   string
		wantDates []string
	}{
		{
			name:      "valid ISO date",
			content:   "deployed on 2026-03-29",
			wantDates: []string{"2026-03-29"},
		},
		{
			name:      "Feb 30 is invalid, Go normalizes to Mar 2",
			content:   "deployed on 2026-02-30",
			wantDates: nil,
		},
		{
			name:      "Feb 29 in non-leap year (2026) is invalid",
			content:   "deployed on 2026-02-29",
			wantDates: nil,
		},
		{
			name:      "Feb 29 in leap year (2028) is valid",
			content:   "deployed on 2028-02-29",
			wantDates: []string{"2028-02-29"},
		},
		{
			name:      "Apr 31 is invalid, Go normalizes to May 1",
			content:   "meeting on 2026-04-31",
			wantDates: nil,
		},
		{
			name:      "Jun 31 is invalid",
			content:   "deadline 2026-06-31",
			wantDates: nil,
		},
		{
			name:      "valid dates pass through, invalid ones filtered",
			content:   "from 2026-03-15 to 2026-02-30",
			wantDates: []string{"2026-03-15"},
		},
		{
			name:      "dot format: 30.02.2026 is invalid",
			content:   "am 30.02.2026 war nichts",
			wantDates: nil,
		},
		{
			name:      "dot format: valid date passes",
			content:   "am 15.03.2026 war es",
			wantDates: []string{"2026-03-15"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractDateStrings(tt.content)
			if len(tt.wantDates) == 0 {
				if len(got) != 0 {
					t.Errorf("expected no dates, got %v", got)
				}
				return
			}
			if len(got) != len(tt.wantDates) {
				t.Fatalf("expected %d dates, got %d: %v", len(tt.wantDates), len(got), got)
			}
			for i, want := range tt.wantDates {
				if got[i] != want {
					t.Errorf("date[%d]: got %s, want %s", i, got[i], want)
				}
			}
		})
	}
}

func TestIsDateNormalized(t *testing.T) {
	// Test isDateNormalized using time.Date (which DOES normalize, unlike time.Parse).
	// This validates the defense-in-depth guard works correctly.
	tests := []struct {
		input    string
		year     int
		month    time.Month
		day      int
		wantNorm bool
	}{
		{"2026-03-15", 2026, 3, 15, false}, // valid → not normalized
		{"2026-02-30", 2026, 2, 30, true},  // invalid → time.Date gives 2026-03-02
		{"2026-02-29", 2026, 2, 29, true},  // non-leap year → time.Date gives 2026-03-01
		{"2028-02-29", 2028, 2, 29, false}, // leap year → valid
		{"2026-04-31", 2026, 4, 31, true},  // Apr has 30 days → time.Date gives May 1
		{"2026-01-31", 2026, 1, 31, false}, // Jan has 31 days → valid
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			// Use time.Date which normalizes (unlike time.Parse which rejects)
			parsed := time.Date(tt.year, tt.month, tt.day, 0, 0, 0, 0, time.UTC)
			got := isDateNormalized(tt.input, parsed)
			if got != tt.wantNorm {
				t.Errorf("isDateNormalized(%q, %s) = %v, want %v",
					tt.input, parsed.Format("2006-01-02"), got, tt.wantNorm)
			}
		})
	}
}
