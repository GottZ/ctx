package store

// dates_ref_test.go — the regexp-based date extraction as the oracle of the
// differential tests, in two versions.
//
// refFormsV434 is the extraction as it stood before issue #434, frozen: the
// expressions and the year window of dates.go at 8d4a077a, verbatim. It never
// changes. Outside the input classes a deliberate change names
// (issue437Classes in dates_diff_test.go), ExtractDates has to return what
// it returns, byte for byte.
//
// refForms is the reference of the current extraction: refFormsV434 with the
// deliberate changes since, each one listed below with its issue. Edit it only
// for such a change, in the same commit as the production code, and add the
// input class the change affects to issue437Classes (or its successor), so
// the frozen version keeps guarding every other input.
//
//	#437/1  German month: m[aä]rz becomes märz|maerz — "Maerz" is March, the
//	        spelling util.MonthDE carries; "Marz", which no table carries, no
//	        longer matches.
//
// extractDatesRefWith is the body of extractDates at 8d4a077a, verbatim except
// that the four expressions and the bounds of the year window come from its
// refDateForms argument.

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/util"
)

// refDateForms is one version of the reference: the expressions of the four
// date forms and the year window add enforces.
type refDateForms struct {
	iso, dot, de, en *regexp.Regexp
	minYear, maxYear int
}

var (
	refFormsV434 = refDateForms{
		// ISO dates with optional time: 2026-03-29, 2026-03-29 14:30, 2026-03-29T14:30, 2026-03-29T14:30:00Z.
		iso: regexp.MustCompile(`\b(20[2-3]\d-(?:0[1-9]|1[0-2])-(?:0[1-9]|[12]\d|3[01]))(?:[T\s]+([01]\d|2[0-3]):([0-5]\d)(?::[0-5]\d)?Z?)?\b`),
		// German dot-format with optional time: 29.03.2026, 29.03.2026 14:30.
		dot: regexp.MustCompile(`\b(0[1-9]|[12]\d|3[01])\.(0[1-9]|1[0-2])\.(20[2-3]\d)(?:\s+([01]\d|2[0-3]):([0-5]\d))?\b`),
		// German month+year: "März 2026".
		de: regexp.MustCompile(`(?i)\b(januar|februar|m[aä]rz|april|mai|juni|juli|august|september|oktober|november|dezember)\s+(20[2-3]\d)\b`),
		// English month+year: "March 2026".
		en:      regexp.MustCompile(`(?i)\b(january|february|march|april|may|june|july|august|september|october|november|december)\s+(20[2-3]\d)\b`),
		minYear: 2020, maxYear: 2030,
	}

	refForms = refDateForms{
		iso:     refFormsV434.iso,
		dot:     refFormsV434.dot,
		de:      regexp.MustCompile(`(?i)\b(januar|februar|märz|maerz|april|mai|juni|juli|august|september|oktober|november|dezember)\s+(20[2-3]\d)\b`),
		en:      refFormsV434.en,
		minYear: 2020, maxYear: 2030,
	}
)

// extractDatesRefWith extracts all recognizable timestamps from text content.
// Parses ISO dates (with optional HH:MM time) and German dot-format dates.
// Returns deduplicated, sorted timestamps at UTC. Rejects dates before 2020 or after 2030.
// Timestamps without explicit time default to midnight UTC.
func extractDatesRefWith(f refDateForms, content string) []time.Time {
	seen := make(map[string]bool)
	var dates []time.Time

	add := func(t time.Time) {
		if t.Year() < f.minYear || t.Year() > f.maxYear {
			return
		}
		// Key by full timestamp (incl. hour+minute) so same-day different-hour are kept distinct.
		key := t.Format("2006-01-02T15:04")
		if !seen[key] {
			seen[key] = true
			dates = append(dates, t)
		}
	}

	// 1. ISO dates (with optional HH:MM time).
	// T25: time.Parse normalizes invalid dates silently (e.g., Feb 30 → Mar 2).
	// After parsing, verify that the parsed day/month matches the input to reject invalid dates.
	for _, m := range f.iso.FindAllStringSubmatch(content, -1) {
		dateStr := m[1]
		t, err := time.Parse("2006-01-02", dateStr)
		if err != nil || refIsDateNormalized(dateStr, t) {
			continue
		}
		// Apply hour/minute if present.
		if len(m) >= 4 && m[2] != "" && m[3] != "" {
			hour, _ := strconv.Atoi(m[2])
			minute, _ := strconv.Atoi(m[3])
			t = time.Date(t.Year(), t.Month(), t.Day(), hour, minute, 0, 0, time.UTC)
		}
		add(t)
	}

	// 2. German dot-format: DD.MM.YYYY (with optional HH:MM time).
	for _, m := range f.dot.FindAllStringSubmatch(content, -1) {
		if len(m) < 4 {
			continue
		}
		ds := m[3] + "-" + m[2] + "-" + m[1]
		t, err := time.Parse("2006-01-02", ds)
		if err != nil || refIsDateNormalized(ds, t) {
			continue
		}
		// Apply hour/minute if present (groups 4, 5).
		if len(m) >= 6 && m[4] != "" && m[5] != "" {
			hour, _ := strconv.Atoi(m[4])
			minute, _ := strconv.Atoi(m[5])
			t = time.Date(t.Year(), t.Month(), t.Day(), hour, minute, 0, 0, time.UTC)
		}
		add(t)
	}

	// 3. German month+year → 1st of month
	for _, m := range f.de.FindAllStringSubmatch(content, -1) {
		if len(m) == 3 {
			monthStr := strings.ToLower(m[1])
			if month, ok := util.MonthDE[monthStr]; ok {
				ds := fmt.Sprintf("%s-%02d-01", m[2], int(month))
				if t, err := time.Parse("2006-01-02", ds); err == nil {
					add(t)
				}
			}
		}
	}

	// 4. English month+year → 1st of month
	for _, m := range f.en.FindAllStringSubmatch(content, -1) {
		if len(m) == 3 {
			monthStr := strings.ToLower(m[1])
			if month, ok := util.MonthEN[monthStr]; ok {
				ds := fmt.Sprintf("%s-%02d-01", m[2], int(month))
				if t, err := time.Parse("2006-01-02", ds); err == nil {
					add(t)
				}
			}
		}
	}

	sort.Slice(dates, func(i, j int) bool {
		return dates[i].Before(dates[j])
	})

	return dates
}

// refIsDateNormalized returns true if Go's time.Parse silently normalized the date
// (e.g., "2026-02-30" parsed as 2026-03-02). Compares the parsed day/month against
// the input string's day/month components. T25: prevents silent invalid-date acceptance.
func refIsDateNormalized(isoInput string, parsed time.Time) bool {
	// isoInput is "YYYY-MM-DD" format
	parts := strings.Split(isoInput, "-")
	if len(parts) != 3 {
		return false
	}
	inputMonth, err1 := strconv.Atoi(parts[1])
	inputDay, err2 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil {
		return false
	}
	return int(parsed.Month()) != inputMonth || parsed.Day() != inputDay
}
