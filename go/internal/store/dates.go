// Package store — Date Extraction for content_dates enrichment
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
	"strings"
	"time"
)

var (
	// ISO dates: 2026-03-29
	isoDateExtract = regexp.MustCompile(`\b(20[2-3]\d-(?:0[1-9]|1[0-2])-(?:0[1-9]|[12]\d|3[01]))\b`)

	// German dot-format: 29.03.2026
	dotDateExtract = regexp.MustCompile(`\b(0[1-9]|[12]\d|3[01])\.(0[1-9]|1[0-2])\.(20[2-3]\d)\b`)

	// German month+year: "März 2026"
	deMonthYearExtract = regexp.MustCompile(`(?i)\b(januar|februar|m[aä]rz|april|mai|juni|juli|august|september|oktober|november|dezember)\s+(20[2-3]\d)\b`)

	// English month+year: "March 2026"
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

// ExtractDates extracts all recognizable dates from text content.
// Returns deduplicated, sorted dates. Rejects dates before 2020 or after 2030.
func ExtractDates(content string) []time.Time {
	seen := make(map[string]bool)
	var dates []time.Time

	add := func(t time.Time) {
		if t.Year() < 2020 || t.Year() > 2030 {
			return
		}
		key := t.Format("2006-01-02")
		if !seen[key] {
			seen[key] = true
			dates = append(dates, t)
		}
	}

	// 1. ISO dates
	for _, m := range isoDateExtract.FindAllString(content, -1) {
		if t, err := time.Parse("2006-01-02", m); err == nil {
			add(t)
		}
	}

	// 2. German dot-format: DD.MM.YYYY
	for _, m := range dotDateExtract.FindAllStringSubmatch(content, -1) {
		if len(m) == 4 {
			ds := m[3] + "-" + m[2] + "-" + m[1]
			if t, err := time.Parse("2006-01-02", ds); err == nil {
				add(t)
			}
		}
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

// ExtractDateStrings is a convenience wrapper that returns ISO date strings.
func ExtractDateStrings(content string) []string {
	dates := ExtractDates(content)
	strs := make([]string, len(dates))
	for i, d := range dates {
		strs[i] = d.Format("2006-01-02")
	}
	return strs
}
