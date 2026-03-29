package rrf

import (
	"strings"
	"time"
	"unicode/utf8"
)

// German weekday names indexed by time.Weekday (Sunday=0 .. Saturday=6).
var weekdayDE = [7]string{"Sonntag", "Montag", "Dienstag", "Mittwoch", "Donnerstag", "Freitag", "Samstag"}

type temporalKeyword struct {
	word    string
	resolve func(now time.Time) []time.Time
}

var temporalKeywords = []temporalKeyword{
	// Single-day references
	{"heute", func(now time.Time) []time.Time { return []time.Time{now} }},
	{"today", func(now time.Time) []time.Time { return []time.Time{now} }},
	{"gestern", func(now time.Time) []time.Time { return []time.Time{now.AddDate(0, 0, -1)} }},
	{"yesterday", func(now time.Time) []time.Time { return []time.Time{now.AddDate(0, 0, -1)} }},
	{"vorgestern", func(now time.Time) []time.Time { return []time.Time{now.AddDate(0, 0, -2)} }},
	{"morgen", func(now time.Time) []time.Time { return []time.Time{now.AddDate(0, 0, 1)} }},
	{"tomorrow", func(now time.Time) []time.Time { return []time.Time{now.AddDate(0, 0, 1)} }},
	{"übermorgen", func(now time.Time) []time.Time { return []time.Time{now.AddDate(0, 0, 2)} }},
}

// levenshtein computes the edit distance between two rune slices.
func levenshtein(a, b string) int {
	ra := []rune(strings.ToLower(a))
	rb := []rune(strings.ToLower(b))
	la := len(ra)
	lb := len(rb)

	prev := make([]int, lb+1)
	curr := make([]int, lb+1)

	for j := 0; j <= lb; j++ {
		prev[j] = j
	}

	for i := 1; i <= la; i++ {
		curr[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			del := prev[j] + 1
			ins := curr[j-1] + 1
			sub := prev[j-1] + cost
			m := del
			if ins < m {
				m = ins
			}
			if sub < m {
				m = sub
			}
			curr[j] = m
		}
		prev, curr = curr, prev
	}

	return prev[lb]
}

// maxEditDistance returns the Levenshtein tolerance based on keyword length.
func maxEditDistance(word string) int {
	n := utf8.RuneCountInString(word)
	if n <= 3 {
		return 0
	}
	if n <= 5 {
		return 1
	}
	return 2
}

// ExpandTemporal detects temporal references in the query with typo tolerance
// and returns a websearch_to_tsquery-compatible OR string for FTS expansion.
// Returns empty string if no temporal terms are detected.
func ExpandTemporal(query string, now time.Time) string {
	words := strings.Fields(strings.ToLower(query))

	var dates []time.Time
	seen := make(map[string]bool)

	for _, word := range words {
		word = strings.Trim(word, ".,;:!?\"'()[]{}–—")
		if word == "" {
			continue
		}

		for _, kw := range temporalKeywords {
			dist := levenshtein(word, kw.word)
			if dist <= maxEditDistance(kw.word) {
				for _, d := range kw.resolve(now) {
					key := d.Format("2006-01-02")
					if !seen[key] {
						dates = append(dates, d)
						seen[key] = true
					}
				}
				break // first match wins per word
			}
		}
	}

	if len(dates) == 0 {
		return ""
	}

	// Build OR terms: weekday DE, weekday EN, ISO date
	var terms []string
	for _, d := range dates {
		terms = append(terms,
			weekdayDE[d.Weekday()],
			d.Format("Monday"),
			d.Format("2006-01-02"),
		)
	}

	return strings.Join(terms, " OR ")
}
