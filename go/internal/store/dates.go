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
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/GottZ/ctx/internal/util"
)

// The extraction knows four date forms. Until issue #434 each was a regular
// expression run over the whole content; the scanner below matches exactly
// what they matched, with the same submatches, and the frozen copy of the
// regexp implementation in dates_ref_test.go is the oracle of its tests:
//
//	ISO       \b(20[2-3]\d-(?:0[1-9]|1[0-2])-(?:0[1-9]|[12]\d|3[01]))(?:[T\s]+([01]\d|2[0-3]):([0-5]\d)(?::[0-5]\d)?Z?)?\b
//	dot       \b(0[1-9]|[12]\d|3[01])\.(0[1-9]|1[0-2])\.(20[2-3]\d)(?:\s+([01]\d|2[0-3]):([0-5]\d))?\b
//	German    (?i)\b(januar|februar|m[aä]rz|april|mai|juni|juli|august|september|oktober|november|dezember)\s+(20[2-3]\d)\b
//	English   (?i)\b(january|february|march|april|may|june|july|august|september|october|november|december)\s+(20[2-3]\d)\b
//
// Every match of every form contains a year token 20[2-3]\d at a place the
// form fixes: at the start of an ISO date, behind the second dot of a dot
// date, behind the whitespace after a month name. scanDates finds the year
// tokens with strings.Index and tries each form only at that one place,
// instead of running four automata, two of them case-insensitive, over every
// byte. The regexp semantics carried over: \b and \d and \s are ASCII (\s is
// [\t\n\f\r ], no \v); (?i) folds by Unicode simple case folding, so the
// Kelvin sign matches k and the long s matches s; the optional time parts are
// greedy and fall back in the regexp's order of preference; the matches of
// one form do not overlap, a later one starts at or after the end of the
// previous one.

// dateScan holds, per date form, the submatch offsets of its matches in
// content order — what FindAllStringSubmatchIndex of the form's regular
// expression returned.
type dateScan struct {
	iso, dot, de, en [][]int
}

// scanDates finds the matches of the four date forms in content.
func scanDates(content string) dateScan {
	var sc dateScan
	var isoEnd, dotEnd, deEnd, enEnd int
	accept := func(list *[][]int, end *int, m []int) {
		if m != nil && m[0] >= *end {
			*list = append(*list, m)
			*end = m[1]
		}
	}
	for off := 0; ; {
		i := strings.Index(content[off:], "20")
		if i < 0 {
			break
		}
		y := off + i
		off = y + 1
		if !isYearAt(content, y) {
			continue
		}
		accept(&sc.iso, &isoEnd, matchISODate(content, y))
		accept(&sc.dot, &dotEnd, matchDotDate(content, y))
		if w, ok := monthYearEnd(content, y); ok {
			accept(&sc.de, &deEnd, matchMonthYear(content, w, y, deMonthPatterns))
			accept(&sc.en, &enEnd, matchMonthYear(content, w, y, enMonthPatterns))
		}
	}
	return sc
}

// matchISODate matches the ISO form with its year token at s.
func matchISODate(c string, s int) []int {
	d := s + 10
	if d > len(c) || !isWordBoundary(c, s) || c[s+4] != '-' || !isMonth2(c[s+5], c[s+6]) ||
		c[s+7] != '-' || !isDay2(c[s+8], c[s+9]) {
		return nil
	}
	if h, end := matchISOTime(c, d); end >= 0 {
		return []int{s, end, s, d, h, h + 2, h + 3, h + 5}
	}
	if isWordBoundary(c, d) {
		return []int{s, d, s, d, -1, -1, -1, -1}
	}
	return nil
}

// matchISOTime matches the optional time of the ISO form at i together with
// the \b that ends the form, and returns the offset of the hour and the end,
// or end -1. [T\s]+ is greedy and only its longest run can be followed by an
// hour digit. Of the optional seconds and Z the regexp prefers seconds with
// Z, then seconds, then Z, then neither; the first whose end is a word
// boundary is the match.
func matchISOTime(c string, i int) (h, end int) {
	h = i
	for h < len(c) && (c[h] == 'T' || isRegexpSpace(c[h])) {
		h++
	}
	if h == i || h+5 > len(c) || !isHour2(c[h], c[h+1]) || c[h+2] != ':' || !isMinute2(c[h+3], c[h+4]) {
		return 0, -1
	}
	e := h + 5
	if e+3 <= len(c) && c[e] == ':' && isMinute2(c[e+1], c[e+2]) {
		if e+3 < len(c) && c[e+3] == 'Z' && isWordBoundary(c, e+4) {
			return h, e + 4
		}
		if isWordBoundary(c, e+3) {
			return h, e + 3
		}
	}
	if e < len(c) && c[e] == 'Z' && isWordBoundary(c, e+1) {
		return h, e + 1
	}
	if isWordBoundary(c, e) {
		return h, e
	}
	return 0, -1
}

// matchDotDate matches the dot form with its year token at y.
func matchDotDate(c string, y int) []int {
	s := y - 6
	if s < 0 || c[y-1] != '.' || c[s+2] != '.' || !isDay2(c[s], c[s+1]) || !isMonth2(c[s+3], c[s+4]) ||
		!isWordBoundary(c, s) {
		return nil
	}
	d := y + 4
	h := d
	for h < len(c) && isRegexpSpace(c[h]) {
		h++
	}
	if h > d && h+5 <= len(c) && isHour2(c[h], c[h+1]) && c[h+2] == ':' && isMinute2(c[h+3], c[h+4]) &&
		isWordBoundary(c, h+5) {
		return []int{s, h + 5, s, s + 2, s + 3, s + 5, y, d, h, h + 2, h + 3, h + 5}
	}
	if isWordBoundary(c, d) {
		return []int{s, d, s, s + 2, s + 3, s + 5, y, d, -1, -1, -1, -1}
	}
	return nil
}

// monthYearEnd checks the part of the month forms behind the month name for
// the year token at y — \s+ before it, \b after it — and returns where the
// month name has to end: the start of the whitespace run before y.
func monthYearEnd(c string, y int) (w int, ok bool) {
	if !isWordBoundary(c, y+4) {
		return 0, false
	}
	w = y
	for w > 0 && isRegexpSpace(c[w-1]) {
		w--
	}
	return w, w < y
}

// matchMonthYear matches a month form whose month name ends at w and whose
// year token is at y. No alternative of a form ends with another one
// (TestMonthPatterns_NoAlternativeEndsAnother), so at most one matches.
func matchMonthYear(c string, w, y int, patterns []monthPattern) []int {
	for _, p := range patterns {
		if s := p.matchBefore(c, w); s >= 0 {
			return []int{s, y + 4, s, w, y, y + 4}
		}
	}
	return nil
}

// monthPattern is one alternative of a month form: per letter, the runes
// (?i) lets match there.
type monthPattern [][]rune

// matchBefore matches p backwards from w, decoding runes as the regexp does,
// and returns the start of the match, or -1. The start has to be a \b.
func (p monthPattern) matchBefore(c string, w int) int {
	i := w
	for k := len(p) - 1; k >= 0; k-- {
		r, size := utf8.DecodeLastRuneInString(c[:i])
		if size == 0 || !slices.Contains(p[k], r) {
			return -1
		}
		i -= size
	}
	if !isWordBoundary(c, i) {
		return -1
	}
	return i
}

var (
	deMonthPatterns = newMonthPatterns("januar", "februar", "m[aä]rz", "april", "mai", "juni", "juli",
		"august", "september", "oktober", "november", "dezember")
	enMonthPatterns = newMonthPatterns("january", "february", "march", "april", "may", "june", "july",
		"august", "september", "october", "november", "december")
)

// newMonthPatterns builds the alternatives of a month form from lowercase
// names; a bracket lists the letters one position admits, as [aä] in the
// regular expression. Each letter admits its Unicode simple case folding
// orbit, the set (?i) matches.
func newMonthPatterns(names ...string) []monthPattern {
	patterns := make([]monthPattern, 0, len(names))
	for _, name := range names {
		var p monthPattern
		var class []rune
		inClass := false
		for _, r := range name {
			switch {
			case r == '[':
				inClass, class = true, nil
			case r == ']':
				inClass = false
				p = append(p, class)
			case inClass:
				class = append(class, foldOrbit(r)...)
			default:
				p = append(p, foldOrbit(r))
			}
		}
		patterns = append(patterns, p)
	}
	return patterns
}

// foldOrbit returns r and every rune unicode.SimpleFold reaches from it, the
// runes regexp's (?i) treats as equal to r.
func foldOrbit(r rune) []rune {
	orbit := []rune{r}
	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		orbit = append(orbit, f)
	}
	return orbit
}

// isYearAt reports whether a year token 20[2-3]\d starts at i.
func isYearAt(c string, i int) bool {
	return i+4 <= len(c) && c[i] == '2' && c[i+1] == '0' && (c[i+2] == '2' || c[i+2] == '3') && isDigit(c[i+3])
}

// isWordBoundary reports whether regexp's \b holds at offset i: an ASCII word
// character on exactly one side. Any byte outside ASCII belongs to a rune that
// is not a word character, whatever the rune decodes to.
func isWordBoundary(c string, i int) bool {
	before := i > 0 && isWordByte(c[i-1])
	after := i < len(c) && isWordByte(c[i])
	return before != after
}

func isWordByte(b byte) bool {
	return b == '_' || isDigit(b) || 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z'
}

// isRegexpSpace reports whether b is in regexp's \s, [\t\n\f\r ].
func isRegexpSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\f' || b == '\r'
}

func isDigit(b byte) bool { return '0' <= b && b <= '9' }

// isMonth2 matches 0[1-9]|1[0-2].
func isMonth2(a, b byte) bool {
	return a == '0' && '1' <= b && b <= '9' || a == '1' && '0' <= b && b <= '2'
}

// isDay2 matches 0[1-9]|[12]\d|3[01].
func isDay2(a, b byte) bool {
	return a == '0' && '1' <= b && b <= '9' || (a == '1' || a == '2') && isDigit(b) || a == '3' && (b == '0' || b == '1')
}

// isHour2 matches [01]\d|2[0-3].
func isHour2(a, b byte) bool {
	return (a == '0' || a == '1') && isDigit(b) || a == '2' && '0' <= b && b <= '3'
}

// isMinute2 matches [0-5]\d.
func isMinute2(a, b byte) bool { return '0' <= a && a <= '5' && isDigit(b) }

// submatchStrings turns submatch offsets into the strings
// FindAllStringSubmatch returns for them: "" for a group that did not take
// part in the match.
func submatchStrings(c string, all [][]int) [][]string {
	if all == nil {
		return nil
	}
	out := make([][]string, len(all))
	for i, idx := range all {
		m := make([]string, len(idx)/2)
		for j := range m {
			if idx[2*j] >= 0 {
				m[j] = c[idx[2*j]:idx[2*j+1]]
			}
		}
		out[i] = m
	}
	return out
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

	sc := scanDates(content)

	// 1. ISO dates (with optional HH:MM time).
	// T25: time.Parse normalizes invalid dates silently (e.g., Feb 30 → Mar 2).
	// After parsing, verify that the parsed day/month matches the input to reject invalid dates.
	for _, m := range submatchStrings(content, sc.iso) {
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
	for _, m := range submatchStrings(content, sc.dot) {
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
	for _, m := range submatchStrings(content, sc.de) {
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
	for _, m := range submatchStrings(content, sc.en) {
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
