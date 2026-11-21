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
// what these expressions match, with the same submatches, and the regexp
// implementation in dates_ref_test.go is the oracle of its tests (it also
// keeps the expressions as they stood before #434, frozen, and the input
// classes the deliberate changes since then affect):
//
//	ISO       \b(20[2-9]\d-(?:0[1-9]|1[0-2])-(?:0[1-9]|[12]\d|3[01]))(?:[T\s\x{A0}\x{202F}]+([01]\d|2[0-3]):([0-5]\d)(?::[0-5]\d)?Z?)?\b
//	dot       \b(0[1-9]|[12]\d|3[01])\.(0[1-9]|1[0-2])\.(20[2-9]\d)(?:[\s\x{A0}\x{202F}]+([01]\d|2[0-3]):([0-5]\d))?\b
//	German    (?i)\b(januar|februar|märz|maerz|april|mai|juni|juli|august|september|oktober|november|dezember)[\s\x{A0}\x{202F}]+(20[2-9]\d)\b
//	English   (?i)\b(january|february|march|april|may|june|july|august|september|october|november|december)[\s\x{A0}\x{202F}]+(20[2-9]\d)\b
//
// Every match of every form contains a year token 20[2-9]\d at a place the
// form fixes: at the start of an ISO date, behind the second dot of a dot
// date, behind the whitespace after a month name. scanDates finds the year
// tokens with strings.Index and tries each form only at that one place,
// instead of running four automata, two of them case-insensitive, over every
// byte. The regexp semantics carried over: \b and \d and \s are ASCII (\s is
// [\t\n\f\r ], no \v), and the date spaces between the parts of a date are \s
// and the two no-break spaces U+00A0 and U+202F (dateSpaceAt); (?i) folds by
// Unicode simple case folding, so the Kelvin sign matches k and the long s
// matches s; the optional time parts are greedy and fall back in the regexp's
// order of preference; the matches of one form do not overlap, a later one
// starts at or after the end of the previous one. A month form is tried
// only with the month names that can end with the rune before the date
// spaces (monthForm).

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
			accept(&sc.de, &deEnd, matchMonthYear(content, w, y, deMonthForm))
			accept(&sc.en, &enEnd, matchMonthYear(content, w, y, enMonthForm))
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
// or end -1. The run of T and date spaces is greedy and only its longest run
// can be followed by an hour digit. Of the optional seconds and Z the regexp
// prefers seconds with Z, then seconds, then Z, then neither; the first whose
// end is a word boundary is the match.
func matchISOTime(c string, i int) (h, end int) {
	h = i
	for h < len(c) {
		if c[h] == 'T' {
			h++
		} else if n := dateSpaceAt(c, h); n > 0 {
			h += n
		} else {
			break
		}
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
	for n := dateSpaceAt(c, h); n > 0; n = dateSpaceAt(c, h) {
		h += n
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
// the year token at y — a run of date spaces before it, \b after it — and
// returns where the month name has to end: the start of that run.
func monthYearEnd(c string, y int) (w int, ok bool) {
	if !isWordBoundary(c, y+4) {
		return 0, false
	}
	w = y
	for n := dateSpaceBefore(c, w); n > 0; n = dateSpaceBefore(c, w) {
		w -= n
	}
	return w, w < y
}

// The no-break spaces that separate the parts of a date as \s does (issue
// #437): a word processor, a web page (&nbsp;) or a typesetting system puts
// U+00A0 between a month and its year or a date and its time to keep them on
// one line, and U+202F, the narrow one, is what typographic and locale-aware
// date formats use there.
const (
	noBreakSpace       = "\u00a0"
	narrowNoBreakSpace = "\u202f"
)

// dateSpaceAt returns the length in bytes of the date space that starts at
// c[i]: 1 for a byte of regexp's \s, the length of a no-break space, or 0.
// The bytes of a no-break space are a lead byte and continuation bytes, so
// they decode to that rune wherever they stand, whatever bytes precede them;
// a lone byte of one is no space.
func dateSpaceAt(c string, i int) int {
	switch {
	case i >= len(c):
		return 0
	case isRegexpSpace(c[i]):
		return 1
	case strings.HasPrefix(c[i:], noBreakSpace):
		return len(noBreakSpace)
	case strings.HasPrefix(c[i:], narrowNoBreakSpace):
		return len(narrowNoBreakSpace)
	}
	return 0
}

// dateSpaceBefore returns the length in bytes of the date space that ends at
// c[i], as dateSpaceAt does forwards.
func dateSpaceBefore(c string, i int) int {
	switch {
	case i <= 0:
		return 0
	case isRegexpSpace(c[i-1]):
		return 1
	case strings.HasSuffix(c[:i], noBreakSpace):
		return len(noBreakSpace)
	case strings.HasSuffix(c[:i], narrowNoBreakSpace):
		return len(narrowNoBreakSpace)
	}
	return 0
}

// matchMonthYear matches a month form whose month name ends at w and whose
// year token is at y. No alternative of a form ends with another one
// (TestMonthPatterns_NoAlternativeEndsAnother), so at most one matches, and
// only the alternatives the form lists for the rune before w are tried (issue
// #451: most numbers 2020-2099 behind a space are no date, and trying all 25
// alternatives on each made number-heavy text up to 15 times slower once the
// window reached 2099).
func matchMonthYear(c string, w, y int, form *monthForm) []int {
	r, _ := utf8.DecodeLastRuneInString(c[:w])
	for _, p := range form.endingWith(r) {
		if s := p.matchBefore(c, w); s >= 0 {
			return []int{s, y + 4, s, w, y, y + 4}
		}
	}
	return nil
}

// monthForm holds the alternatives of a month form by the runes their last
// letter admits. matchBefore checks the rune before the date spaces against
// the last letter first, so an alternative not listed for that rune cannot
// match there, and behind a rune no month name ends with (a digit, most
// letters) none is tried at all.
type monthForm struct {
	ascii [utf8.RuneSelf][]monthPattern
	other map[rune][]monthPattern
}

func newMonthForm(patterns []monthPattern) *monthForm {
	f := &monthForm{other: map[rune][]monthPattern{}}
	for _, p := range patterns {
		for _, r := range p[len(p)-1] {
			if r < utf8.RuneSelf {
				f.ascii[r] = append(f.ascii[r], p)
			} else {
				f.other[r] = append(f.other[r], p)
			}
		}
	}
	return f
}

// endingWith returns, in the order of the form, the alternatives whose last
// letter admits r.
func (f *monthForm) endingWith(r rune) []monthPattern {
	if 0 <= r && r < utf8.RuneSelf {
		return f.ascii[r]
	}
	return f.other[r]
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

// The month names of the two month forms are the keys of util.MonthDE and
// util.MonthEN, the tables ExtractDates reads the month from, so every name
// a pattern matches is one the table resolves (issue #437: the expression
// m[aä]rz matched "Marz", which no table carries, and missed "Maerz", which
// util.MonthDE does).
var (
	deMonthPatterns = newMonthPatterns("januar", "februar", "märz", "maerz", "april", "mai", "juni", "juli",
		"august", "september", "oktober", "november", "dezember")
	enMonthPatterns = newMonthPatterns("january", "february", "march", "april", "may", "june", "july",
		"august", "september", "october", "november", "december")

	deMonthForm = newMonthForm(deMonthPatterns)
	enMonthForm = newMonthForm(enMonthPatterns)
)

// newMonthPatterns builds the alternatives of a month form from lowercase
// names. Each letter admits its Unicode simple case folding orbit, the set
// (?i) matches.
func newMonthPatterns(names ...string) []monthPattern {
	patterns := make([]monthPattern, 0, len(names))
	for _, name := range names {
		var p monthPattern
		for _, r := range name {
			p = append(p, foldOrbit(r))
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

// The years a content date may carry, minDateYear to maxDateYear: the year
// token 20[2-9]\d (isYearAt, pinned to these bounds by
// TestIsYearAt_IsTheDateYearWindow). The window is fixed, not relative to
// the current time, so ExtractDates stays a pure function of the content:
// the write's afterwork, the dream's re-extraction and the backfill compute
// the same dates for the same content whenever they run, and the content
// guard of their writes compares contents, not dates.
//
// The upper bound is the last year that starts with 20 (until issue #437 it
// was 2030, and from 2031 on a block's own current dates would have given
// nothing). The lower bound stays 2020, measured: on the repository's own
// documents, code and test logs (127 MB) the years 2000-2019 add 87 matches,
// nearly all of them Go's reference time layout "2006-01-02 15:04:05" and
// time_bucket origins such as "2000-01-01 00:00:00", which are no dates;
// 2031-2099 add 7, all of them date literals.
const (
	minDateYear = 2020
	maxDateYear = 2099
)

// isYearAt reports whether a year token 20[2-9]\d starts at i.
func isYearAt(c string, i int) bool {
	return i+4 <= len(c) && c[i] == '2' && c[i+1] == '0' && '2' <= c[i+2] && c[i+2] <= '9' && isDigit(c[i+3])
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
// Returns deduplicated, sorted timestamps at UTC. Rejects dates before 2020 or after 2099
// (minDateYear, maxDateYear).
// Timestamps without explicit time default to midnight UTC.
func ExtractDates(content string) []time.Time {
	seen := make(map[string]bool)
	var dates []time.Time

	add := func(t time.Time) {
		if t.Year() < minDateYear || t.Year() > maxDateYear {
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
