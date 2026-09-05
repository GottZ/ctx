// dates.go — Date Extraction for content_dates enrichment
// Part of ctx by GottZ — The memory your LLM pretends to have.
//
// ExtractDates extracts all recognizable dates from text content
// for populating the content_dates column (GottZ Temporal Gravity).
//
// Source: https://github.com/GottZ/ctx
package store

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	// ISO dates with optional time: 2026-03-29, 2026-03-29 14:30, 2026-03-29T14:30, 2026-03-29T14:30:00Z.
	isoDateExtract = regexp.MustCompile(`\b(20[2-3]\d-(?:0[1-9]|1[0-2])-(?:0[1-9]|[12]\d|3[01]))(?:[T\s]+([01]\d|2[0-3]):([0-5]\d)(?::[0-5]\d)?Z?)?\b`)

	// German dot-format with optional time: 29.03.2026, 29.03.2026 14:30.
	dotDateExtract = regexp.MustCompile(`\b(0[1-9]|[12]\d|3[01])\.(0[1-9]|1[0-2])\.(20[2-3]\d)(?:\s+([01]\d|2[0-3]):([0-5]\d))?\b`)

	// German month+year: "März 2026".
	deMonthYearExtract = regexp.MustCompile(`(?i)\b(januar|februar|m[aä]rz|april|mai|juni|juli|august|september|oktober|november|dezember)\s+(20[2-3]\d)\b`)

	// English month+year: "March 2026".
	enMonthYearExtract = regexp.MustCompile(`(?i)\b(january|february|march|april|may|june|july|august|september|october|november|december)\s+(20[2-3]\d)\b`)
)

var deMonthMap = map[string]time.Month{
	"januar": time.January, "februar": time.February, "märz": time.March,
	"maerz": time.March, "april": time.April, "mai": time.May,
	"juni": time.June, "juli": time.July, "august": time.August,
	"september": time.September, "oktober": time.October,
	"november": time.November, "dezember": time.December,
}

var enMonthMap = map[string]time.Month{
	"january": time.January, "february": time.February, "march": time.March,
	"april": time.April, "may": time.May, "june": time.June,
	"july": time.July, "august": time.August, "september": time.September,
	"october": time.October, "november": time.November, "december": time.December,
}

// ExtractDates extracts all recognizable timestamps from text content.
// Parses ISO dates (with optional HH:MM time) and German dot-format dates.
// Returns deduplicated, sorted timestamps at UTC. Rejects dates before 2020 or after 2030.
// Timestamps without explicit time default to midnight UTC.
func ExtractDates(content string) []time.Time {
	seen := make(map[string]bool)
	var dates []time.Time

	add := func(t time.Time) {
		if t.Year() < 2020 || t.Year() > 2030 {
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
	for _, m := range isoDateExtract.FindAllStringSubmatch(content, -1) {
		dateStr := m[1]
		t, err := time.Parse("2006-01-02", dateStr)
		if err != nil || isDateNormalized(dateStr, t) {
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
	for _, m := range dotDateExtract.FindAllStringSubmatch(content, -1) {
		if len(m) < 4 {
			continue
		}
		ds := m[3] + "-" + m[2] + "-" + m[1]
		t, err := time.Parse("2006-01-02", ds)
		if err != nil || isDateNormalized(ds, t) {
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
	for _, m := range deMonthYearExtract.FindAllStringSubmatch(content, -1) {
		if len(m) == 3 {
			monthStr := strings.ToLower(m[1])
			if month, ok := deMonthMap[monthStr]; ok {
				ds := fmt.Sprintf("%s-%02d-01", m[2], int(month))
				if t, err := time.Parse("2006-01-02", ds); err == nil {
					add(t)
				}
			}
		}
	}

	// 4. English month+year → 1st of month
	for _, m := range enMonthYearExtract.FindAllStringSubmatch(content, -1) {
		if len(m) == 3 {
			monthStr := strings.ToLower(m[1])
			if month, ok := enMonthMap[monthStr]; ok {
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

// isDateNormalized returns true if Go's time.Parse silently normalized the date
// (e.g., "2026-02-30" parsed as 2026-03-02). Compares the parsed day/month against
// the input string's day/month components. T25: prevents silent invalid-date acceptance.
func isDateNormalized(isoInput string, parsed time.Time) bool {
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

// ExtractDateStrings is a convenience wrapper that returns ISO date strings.
func ExtractDateStrings(content string) []string {
	dates := ExtractDates(content)
	strs := make([]string, len(dates))
	for i, d := range dates {
		strs[i] = d.Format("2006-01-02")
	}
	return strs
}
