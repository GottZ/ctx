// temporal_rules.go — Deterministic Temporal Resolution
// Part of ctx by GottZ — The memory your LLM pretends to have.
//
// Rule-based temporal parser as PRIMARY resolution path.
// The LLM (NormalizeTemporal) is fallback only — called when rules return nil
// but HasTemporalIntent is true.
//
// Design rationale: 7-agent armada (Session 10) proved that 55/55 temporal
// test cases are solvable with rules + calendar arithmetic. The 9B model
// fails at arithmetic, not language understanding. time.AddDate() never fails.
//
// Source: https://github.com/GottZ/ctx
package llm

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/GottZ/ctx/internal/util"
)

// --- Tokenizer ---.

// ruleTokenize splits query on whitespace and hyphens, trims punctuation, lowercases.
func ruleTokenize(query string) []string {
	lower := strings.ToLower(query)
	expanded := strings.ReplaceAll(lower, "-", " ")
	raw := strings.Fields(expanded)
	out := make([]string, 0, len(raw))
	for _, w := range raw {
		w = strings.Trim(w, ".,;:!?\"'()[]{}–—/\\")
		if w != "" {
			out = append(out, w)
		}
	}
	return out
}

// --- Levenshtein (lightweight) ---.

func editDistance(a, b string) int {
	ra := []rune(a)
	rb := []rune(b)
	la, lb := len(ra), len(rb)
	prev := make([]int, lb+1)
	curr := make([]int, lb+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		curr[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			del, ins, sub := prev[j]+1, curr[j-1]+1, prev[j-1]+cost
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

func maxEditDist(word string) int {
	n := utf8.RuneCountInString(word)
	if n <= 3 {
		return 0
	}
	if n <= 5 {
		return 1
	}
	// T30: For 6+ char words, allow edit distance 1 but with first-char guard
	// (see fuzzyMatch). Previous threshold of 2 caused false positives.
	return 1
}

// fuzzyMatch returns true if tok is a plausible typo of target.
// T30: For words >= 6 chars, requires the first rune to match, preventing
// false positives like "western" ↔ "gestern" (edit distance 1, different initial).
func fuzzyMatch(tok, target string) bool {
	dist := editDistance(tok, target)
	maxDist := maxEditDist(target)
	if dist > maxDist {
		return false
	}
	if dist == 0 {
		return true
	}
	// T30: For 6+ char words, require first character match to eliminate
	// unrelated words that happen to be 1 edit away.
	n := utf8.RuneCountInString(target)
	if n >= 6 {
		rTok := []rune(tok)
		rTarget := []rune(target)
		if len(rTok) == 0 || len(rTarget) == 0 {
			return false
		}
		if rTok[0] != rTarget[0] {
			return false
		}
	}
	return true
}

// --- Month Maps ---.

// lookupMonth returns the month for a German or English month name (lowercased).
// The tables are util.MonthDE/MonthEN, shared with the date extraction in
// internal/store — the two packages carried identical copies (design D-04,
// Naht 9).
//
// German is consulted first, and the order decides nothing: the four keys that
// appear in both tables ("april", "august", "november", "september") are
// spelled the same in both languages and carry the same month.
func lookupMonth(tok string) (time.Month, bool) {
	if m, ok := util.MonthDE[tok]; ok {
		return m, true
	}
	if m, ok := util.MonthEN[tok]; ok {
		return m, true
	}
	return 0, false
}

// resolveMonthDate returns the 1st of the given month, picking the nearest occurrence
// based on backward/forward direction. If backward: same or previous year. If forward: same or next year.
func resolveMonthDate(m time.Month, backward bool, now time.Time) time.Time {
	y := now.Year()
	candidate := time.Date(y, m, 1, 0, 0, 0, 0, now.Location())
	if backward {
		if candidate.After(now) {
			candidate = candidate.AddDate(-1, 0, 0)
		}
	} else {
		if candidate.Before(now) {
			candidate = candidate.AddDate(1, 0, 0)
		}
	}
	return candidate
}

// monthPositionModifiers: "anfang" → 1st, "mitte" → 15th, "ende" → last day.
var monthPositionModifiers = map[string]string{
	"anfang": "start", "beginn": "start",
	"mitte": "mid",
	"ende": "end",
}

// --- Weekday Maps ---.

var weekdayMapDE = map[string]time.Weekday{
	"montag": time.Monday, "dienstag": time.Tuesday, "mittwoch": time.Wednesday,
	"donnerstag": time.Thursday, "freitag": time.Friday, "samstag": time.Saturday, "sonntag": time.Sunday,
	// Plural forms signal recurrence ("immer dienstags") — same weekday, handled by hasRecurrence
	"montags": time.Monday, "dienstags": time.Tuesday, "mittwochs": time.Wednesday,
	"donnerstags": time.Thursday, "freitags": time.Friday, "samstags": time.Saturday, "sonntags": time.Sunday,
}

var weekdayMapEN = map[string]time.Weekday{
	"monday": time.Monday, "tuesday": time.Tuesday, "wednesday": time.Wednesday,
	"thursday": time.Thursday, "friday": time.Friday, "saturday": time.Saturday, "sunday": time.Sunday,
	// Plural forms for recurrence ("every Tuesdays", "on Mondays")
	"mondays": time.Monday, "tuesdays": time.Tuesday, "wednesdays": time.Wednesday,
	"thursdays": time.Thursday, "fridays": time.Friday, "saturdays": time.Saturday, "sundays": time.Sunday,
}

// findWeekday returns the first weekday found in tokens.
func findWeekday(tokens []string) (time.Weekday, int, bool) {
	for i, tok := range tokens {
		if wd, ok := weekdayMapDE[tok]; ok {
			return wd, i, true
		}
		if wd, ok := weekdayMapEN[tok]; ok {
			return wd, i, true
		}
	}
	return 0, -1, false
}

// --- Verb Tense Detection ---.

var pastVerbSet = toWordSet(
	"war", "ging", "hatte", "wollte", "musste", "wurde", "konnte", "sollte",
	"machte", "fand", "gab", "kam", "nahm", "lief", "fiel", "tat",
	"sagte", "sah", "wusste", "stand", "lag", "blieb",
	"schrieb", "las", "fing", "schlug", "traf", "begann", "brach",
	"sprach", "fuhr", "zog", "hielt", "rief", "stieg", "verlor",
	"waren",
	"was", "were", "had", "went", "did",
	"fixed", "deployed", "cancelled", "canceled", "happened",
	"broke", "crashed", "failed", "finished", "started", "created",
	"moved", "changed", "removed", "deleted", "updated",
	"merged", "pushed", "reviewed", "shipped", "ran", "wrote", "found", "saw", "made", "got",
)

var futureVerbSet = toWordSet(
	"will", "werde", "werden", "wird",
	"gehe", "mache", "soll", "plane", "treffe",
	"starte", "brauche", "möchte",
	"need", "plan",
)

var explicitPastSet = toWordSet(
	"letzten", "letzte", "letztem", "letzter", "letztes",
	"vorigen", "vorige", "vorigem", "voriger", "voriges",
	"vergangenen", "vergangene", "vergangenem", "vergangener", "vergangenes",
	"last", "previous",
)

var explicitFutureSet = toWordSet(
	"nächsten", "nächste", "nächstem", "nächster", "nächstes",
	"kommenden", "kommende", "kommendem", "kommender", "kommendes",
	"next", "upcoming",
)

var perfektAux = toWordSet("habe", "hab", "hat", "haben", "hatte", "hatten", "bin", "ist", "sind")

var perfektParticiplePat = regexp.MustCompile(`^(ab|an|auf|aus|bei|ein|mit|nach|um|vor|weg|zu|über|unter)?ge\w+(t|en)$`)

var knownParticiples = toWordSet(
	"gewesen", "gemacht", "gegangen", "gehabt", "gewollt", "gemusst",
	"geworden", "gekonnt", "gesollt", "gesagt", "gesehen", "gefunden",
	"gegeben", "gekommen", "genommen", "gelaufen", "gefallen", "getan",
	"geschrieben", "gelesen", "gefangen", "geschlagen", "getroffen",
	"begonnen", "gebrochen", "gesprochen", "gefahren", "gezogen",
	"gehalten", "gerufen", "gestiegen", "verloren",
	"deployed", "gefixt", "geplant", "gestartet", "besprochen", "gearbeitet",
	"worden",
)

var futureIntentPhraseList = []string{
	"habe vor", "hab vor",
	"bin unterwegs",
	"habe ich frei", "hab ich frei",
	"habe frei", "hab frei",
	"treffe ich", "treffe mich",
	"muss ich", "muss das",
	"going to", "need to", "have to", "plan to",
}

// separableVerbPairs are German separable verbs where prefix and stem can be apart.
// "steht Freitag an" = "anstehen" (future intent), stem and prefix separated by object.
var separableVerbPairs = [][2]string{
	{"steht", "an"},
	{"fällt", "aus"}, {"faellt", "aus"},
	{"findet", "statt"},
}

// separableArticleSet contains German articles/demonstratives. When a separable verb prefix
// is followed by one of these, it's a preposition phrase, not a separable verb (T20).
// E.g., "steht an der Stelle" = preposition "an" + article "der", NOT "anstehen".
var separableArticleSet = toWordSet(
	"der", "die", "das", "dem", "den", "des",
	"einer", "einem", "einen", "eine", "eines",
	"dieser", "diesem", "diesen", "diese", "dieses",
)

// DetectVerbTense returns "past", "future", or "neutral" based on deterministic verb analysis.
func DetectVerbTense(query string) string {
	lower := strings.ToLower(query)
	tokens := ruleTokenize(query)

	pastScore := 0
	futureScore := 0

	// Explicit direction markers (strong signal)
	for _, tok := range tokens {
		if explicitPastSet[tok] {
			pastScore += 2
		}
		if explicitFutureSet[tok] {
			futureScore += 2
		}
	}

	// Past verbs
	for _, tok := range tokens {
		if pastVerbSet[tok] {
			pastScore++
		}
	}

	// Future verbs
	for _, tok := range tokens {
		if futureVerbSet[tok] {
			futureScore++
		}
	}

	// Perfekt constructions: "habe ... gemacht", "bin ... gewesen"
	if hasPerfektForm(tokens) {
		pastScore += 2
	}

	// Future-intent phrases: "habe ich frei", "treffe ich", etc.
	for _, phrase := range futureIntentPhraseList {
		if strings.Contains(lower, phrase) {
			futureScore += 2
			break
		}
	}

	// Separable verbs: "steht...an", "fällt...aus", "findet...statt"
	// Stem must appear BEFORE prefix in token order.
	// T20: Filter false positives where the "prefix" is actually a preposition
	// followed by an article (e.g., "steht an der Stelle" — "an" + article = preposition).
	for _, pair := range separableVerbPairs {
		stemIdx, prefixIdx := -1, -1
		for i, tok := range tokens {
			if tok == pair[0] {
				stemIdx = i
			}
			if tok == pair[1] {
				prefixIdx = i
			}
		}
		if stemIdx >= 0 && prefixIdx > stemIdx {
			// T20: if the token after the prefix is an article, this is a
			// preposition phrase, not a separable verb — skip.
			if prefixIdx+1 < len(tokens) && separableArticleSet[tokens[prefixIdx+1]] {
				continue
			}
			futureScore += 2
			break
		}
	}

	if pastScore == 0 && futureScore == 0 {
		return "neutral"
	}
	if pastScore > futureScore {
		return "past"
	}
	if futureScore > pastScore {
		return "future"
	}
	return "past" // tie: conservative
}

func hasPerfektForm(tokens []string) bool {
	for i, tok := range tokens {
		if !perfektAux[tok] {
			continue
		}
		end := i + 6
		if end > len(tokens) {
			end = len(tokens)
		}
		for j := i + 1; j < end; j++ {
			if knownParticiples[tokens[j]] || perfektParticiplePat.MatchString(tokens[j]) {
				return true
			}
		}
	}
	return false
}

// --- Date Computation ---.

// resolveWeekdayDate finds the last or next occurrence of a weekday.
func resolveWeekdayDate(wd time.Weekday, backward bool, now time.Time) time.Time {
	today := int(now.Weekday())
	target := int(wd)
	if backward {
		diff := (today - target + 7) % 7
		if diff == 0 {
			diff = 7
		}
		return truncDate(now.AddDate(0, 0, -diff))
	}
	diff := (target - today + 7) % 7
	if diff == 0 {
		diff = 7
	}
	return truncDate(now.AddDate(0, 0, diff))
}

func isoWeekBounds(now time.Time, offset int) (time.Time, time.Time) {
	wd := int(now.Weekday())
	if wd == 0 {
		wd = 7
	}
	monday := truncDate(now.AddDate(0, 0, -(wd-1)+offset*7))
	sunday := monday.AddDate(0, 0, 6)
	return monday, sunday
}

func monthBounds(now time.Time, offset int) (time.Time, time.Time) {
	y, m, _ := now.Date()
	first := time.Date(y, m+time.Month(offset), 1, 0, 0, 0, 0, now.Location())
	last := first.AddDate(0, 1, -1)
	return first, last
}

func truncDate(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

func fmtDate(t time.Time) string {
	return t.Format("2006-01-02")
}

func ptrStr(s string) *string {
	return &s
}

// --- Simple Keyword Table ---.

type simpleKW struct {
	word    string
	resolve func(now time.Time) time.Time
	dir     string
}

var simpleKeywords = []simpleKW{
	{"heute", func(now time.Time) time.Time { return now }, "today"},
	{"today", func(now time.Time) time.Time { return now }, "today"},
	{"gestern", func(now time.Time) time.Time { return now.AddDate(0, 0, -1) }, "past"},
	{"yesterday", func(now time.Time) time.Time { return now.AddDate(0, 0, -1) }, "past"},
	{"vorgestern", func(now time.Time) time.Time { return now.AddDate(0, 0, -2) }, "past"},
	{"morgen", func(now time.Time) time.Time { return now.AddDate(0, 0, 1) }, "future"},
	{"tomorrow", func(now time.Time) time.Time { return now.AddDate(0, 0, 1) }, "future"},
	{"übermorgen", func(now time.Time) time.Time { return now.AddDate(0, 0, 2) }, "future"},
}

// --- Regex Patterns ---.

var (
	reVorNTagen       = regexp.MustCompile(`(?i)vor\s+(\d+)\s+tag(?:en)?`)
	reInNTagen        = regexp.MustCompile(`(?i)in\s+(\d+)\s+tag(?:en)?`)
	reNDaysAgo        = regexp.MustCompile(`(?i)(\d+)\s+days?\s+ago`)
	reInEinerWoche    = regexp.MustCompile(`(?i)in\s+einer\s+woche`)
	reLastWeekDE     = regexp.MustCompile(`(?i)letzte[n]?\s+woche`)
	reThisWeekDE      = regexp.MustCompile(`(?i)diese[rn]?\s+woche`)
	reNextWeekDE   = regexp.MustCompile(`(?i)n[äa]chste[n]?\s+woche`)
	reWeekAfterNextDE = regexp.MustCompile(`(?i)[üu]bern[äa]chste[n]?\s+woche`)
	reLastMonthDE    = regexp.MustCompile(`(?i)letzte[n]?\s+monat`)
	reThisMonthDE     = regexp.MustCompile(`(?i)diese[nrms]?\s+monat`)
	reThisWeek        = regexp.MustCompile(`(?i)this\s+week`)
	reNextWeek        = regexp.MustCompile(`(?i)next\s+week`)
	reLastMonth       = regexp.MustCompile(`(?i)last\s+month(?:'s)?`)
	reThisMonth       = regexp.MustCompile(`(?i)this\s+month`)
	reSeit            = regexp.MustCompile(`(?i)\bseit\s+((?:\S+\s+)?(?:\S+\s+)?\S+)`)
	reBis             = regexp.MustCompile(`(?i)\bbis\s+((?:\S+\s+)?(?:\S+\s+)?\S+)`)
	reVonBis          = regexp.MustCompile(`(?i)von\s+((?:\S+\s+)?(?:\S+\s+)?\S+)\s+bis\s+((?:\S+\s+)?(?:\S+\s+)?\S+)`)
	reThrough         = regexp.MustCompile(`(?i)(monday|tuesday|wednesday|thursday|friday|saturday|sunday)\s+through\s+(monday|tuesday|wednesday|thursday|friday|saturday|sunday)`)
	reAnfangWoche     = regexp.MustCompile(`(?i)anfang\s+(?:der|dieser)\s+woche`)
	reEndeWoche       = regexp.MustCompile(`(?i)ende\s+(?:n[äa]chster|dieser|der)?\s*woche`)
	reEndNextWeekDE   = regexp.MustCompile(`(?i)ende\s+n[äa]chster\s+woche`)
	reISODatePat      = regexp.MustCompile(`\b(20\d{2}-(?:0[1-9]|1[0-2])-(?:0[1-9]|[12]\d|3[01]))\b`)
	reWeekdayInOneWeek       = regexp.MustCompile(`(?i)(montag|dienstag|mittwoch|donnerstag|freitag|samstag|sonntag)\s+in\s+einer\s+woche`)
	reWochenende      = regexp.MustCompile(`(?i)\bwochenende\b|\bweekend\b`)
	reLastWDToWD    = regexp.MustCompile(`(?i)letzten?\s+(montag|dienstag|mittwoch|donnerstag|freitag|samstag|sonntag)\s+bis\s+(montag|dienstag|mittwoch|donnerstag|freitag|samstag|sonntag)`)
	reNextWDThrough   = regexp.MustCompile(`(?i)next\s+(monday|tuesday|wednesday|thursday|friday|saturday|sunday)\s+through\s+(monday|tuesday|wednesday|thursday|friday|saturday|sunday)`)
)

// --- Recurrence Detection (GottZ Cyclic Phase Model) ---.

// recurrenceKeywords signal that a query is about a cyclic pattern rather than
// a specific date. "immer dienstags" vs "am Dienstag" — the former activates
// pure weekday dimension, the latter mixes linear + weekday.
var recurrenceKeywords = map[string]bool{
	// German
	"immer": true, "jeden": true, "jede": true, "jedes": true,
	"regelmäßig": true, "regelmaessig": true,
	"wöchentlich": true, "woechentlich": true,
	"monatlich": true, "täglich": true, "taeglich": true,
	"jährlich": true, "jaehrlich": true, "quartalsweise": true,
	// Common plural -s suffix: "dienstags", "montags" — handled by hasRecurrenceSuffix
	// English
	"always": true, "every": true, "each": true,
	"weekly": true, "monthly": true, "daily": true,
	"yearly": true, "quarterly": true,
}

// hasRecurrence returns true if any token signals cyclic/recurring intent.
// Catches explicit keywords (immer, jeden, every) AND German weekday -s suffix
// (dienstags, montags) which implies "on Tuesdays" = recurring.
func hasRecurrence(tokens []string) bool {
	for _, tok := range tokens {
		if recurrenceKeywords[tok] {
			return true
		}
		// German weekday plural: "dienstags", "montags" etc.
		if isWeekdayPlural(tok) {
			return true
		}
	}
	return false
}

// isWeekdayPlural returns true for German weekday names with -s suffix
// (dienstags, montags, mittwochs, donnerstags, freitags, samstags, sonntags).
func isWeekdayPlural(tok string) bool {
	switch tok {
	case "montags", "dienstags", "mittwochs", "donnerstags",
		"freitags", "samstags", "sonntags":
		return true
	}
	return false
}

// --- DimensionWeights Factories ---.

func dwLinear() map[string]float64 {
	return map[string]float64{"linear": 1.0}
}

func dwLinearWeekday() map[string]float64 {
	return map[string]float64{"linear": 0.6, "weekday": 0.4}
}

func dwWeekday() map[string]float64 {
	return map[string]float64{"weekday": 1.0}
}

func dwLinearWeek() map[string]float64 {
	return map[string]float64{"linear": 0.7, "week": 0.3}
}

func dwLinearMonth() map[string]float64 {
	return map[string]float64{"linear": 0.5, "month": 0.5}
}

func dwLinearMonthSeit() map[string]float64 {
	return map[string]float64{"linear": 0.8, "month": 0.2}
}

func dwLinearRange() map[string]float64 {
	return map[string]float64{"linear": 0.6, "weekday": 0.4}
}

func dwMonthday() map[string]float64 {
	return map[string]float64{"monthday": 1.0}
}

func dwHoliday() map[string]float64 {
	// Annual holidays combine month (coarse) + seasonal (fine) for dual-anchor matching.
	return map[string]float64{"month": 0.4, "seasonal": 0.6}
}

func dwDaily() map[string]float64 {
	return map[string]float64{"daily": 1.0}
}

// --- Result Helpers ---.

func singleResult(ref, date, dir string) *TemporalResult {
	return &TemporalResult{
		Dates: []TemporalDate{{Ref: ref, Date: date, Dir: dir}},
	}
}

func rangeResult(ref, start, end, dir string) *TemporalResult {
	return &TemporalResult{
		Dates: []TemporalDate{{Ref: ref, Date: start, End: ptrStr(end), Dir: dir}},
	}
}

func multiResult(dates []TemporalDate) *TemporalResult {
	return &TemporalResult{Dates: dates}
}

// --- Pattern Matchers ---.

// reISODateRangeIndicator matches range words between two ISO dates.
var reISODateRangeIndicator = regexp.MustCompile(`(?i)\s*(?:bis|to|through|–|—)\s*`)

func matchISODate(query string) *TemporalResult {
	matches := reISODatePat.FindAllString(query, -1)
	if len(matches) == 0 {
		return nil
	}
	tense := DetectVerbTense(query)
	dir := "past"
	if tense == "future" {
		dir = "future"
	}

	if len(matches) == 1 {
		return singleResult(matches[0], matches[0], dir)
	}

	// Two ISO dates: check for range indicator between them
	if len(matches) == 2 {
		idx1 := strings.Index(query, matches[0])
		afterFirst := idx1 + len(matches[0])
		idx2 := strings.Index(query[afterFirst:], matches[1])
		if idx2 >= 0 {
			between := query[afterFirst : afterFirst+idx2]
			if reISODateRangeIndicator.MatchString(between) {
				return rangeResult(matches[0]+" – "+matches[1], matches[0], matches[1], "range")
			}
		}
		// Two dates without range indicator → multiple single entries
		var dates []TemporalDate
		for _, m := range matches {
			dates = append(dates, TemporalDate{Ref: m, Date: m, Dir: dir})
		}
		return multiResult(dates)
	}

	// 3+ ISO dates → multiple single entries
	var dates []TemporalDate
	for _, m := range matches {
		dates = append(dates, TemporalDate{Ref: m, Date: m, Dir: dir})
	}
	return multiResult(dates)
}

func matchVonBis(query string, now time.Time) *TemporalResult {
	// "von Montag bis Mittwoch", "von letztem Montag bis nächsten Freitag", "von gestern bis übermorgen"
	m := reVonBis.FindStringSubmatch(strings.ToLower(query))
	if m == nil {
		return nil
	}
	tense := DetectVerbTense(query)
	backward := tense == "past"
	start := resolvePhrase(m[1], backward, now)
	end := resolvePhrase(m[2], backward, now)
	if start.IsZero() || end.IsZero() {
		return nil
	}
	return rangeResult("von "+m[1]+" bis "+m[2], fmtDate(start), fmtDate(end), "range")
}

func matchLetztenWDBis(query string, now time.Time) *TemporalResult {
	m := reLastWDToWD.FindStringSubmatch(strings.ToLower(query))
	if m == nil {
		return nil
	}
	wd1, ok1 := weekdayMapDE[m[1]]
	wd2, ok2 := weekdayMapDE[m[2]]
	if !ok1 || !ok2 {
		return nil
	}
	d1 := resolveWeekdayDate(wd1, true, now)
	d2 := resolveWeekdayDate(wd2, true, now)
	return rangeResult("letzten "+m[1]+" bis "+m[2], fmtDate(d1), fmtDate(d2), "range")
}

func matchNextThrough(query string, now time.Time) *TemporalResult {
	m := reNextWDThrough.FindStringSubmatch(strings.ToLower(query))
	if m != nil {
		wd1, ok1 := weekdayMapEN[m[1]]
		wd2, ok2 := weekdayMapEN[m[2]]
		if ok1 && ok2 {
			d1 := resolveWeekdayDate(wd1, false, now)
			d2 := resolveWeekdayDate(wd2, false, now)
			return rangeResult("next "+m[1]+" through "+m[2], fmtDate(d1), fmtDate(d2), "range")
		}
	}
	// Also try bare "X through Y" without "next"
	m = reThrough.FindStringSubmatch(strings.ToLower(query))
	if m == nil {
		return nil
	}
	wd1, ok1 := weekdayMapEN[m[1]]
	wd2, ok2 := weekdayMapEN[m[2]]
	if !ok1 || !ok2 {
		return nil
	}
	tense := DetectVerbTense(query)
	backward := tense == "past"
	d1 := resolveWeekdayDate(wd1, backward, now)
	d2 := resolveWeekdayDate(wd2, backward, now)
	return rangeResult(m[1]+" through "+m[2], fmtDate(d1), fmtDate(d2), "range")
}

func matchSeit(query string, now time.Time) *TemporalResult {
	m := reSeit.FindStringSubmatch(strings.ToLower(query))
	if m == nil {
		return nil
	}
	start := resolvePhrase(m[1], true, now)
	if start.IsZero() {
		return nil
	}
	return rangeResult("seit "+m[1], fmtDate(start), fmtDate(truncDate(now)), "range")
}

func matchBis(query string, now time.Time) *TemporalResult {
	lower := strings.ToLower(query)
	// Skip if "von...bis" (handled by matchVonBis)
	if reVonBis.MatchString(lower) {
		return nil
	}
	// Skip if "letzten X bis Y" (handled by matchLetztenWDBis)
	if reLastWDToWD.MatchString(lower) {
		return nil
	}
	m := reBis.FindStringSubmatch(lower)
	if m == nil {
		return nil
	}
	end := resolvePhrase(m[1], false, now)
	if end.IsZero() {
		return nil
	}
	return rangeResult("bis "+m[1], fmtDate(truncDate(now)), fmtDate(end), "range")
}

func matchWeekSegment(query string, now time.Time) *TemporalResult {
	lower := strings.ToLower(query)
	if reAnfangWoche.MatchString(lower) {
		mon, _ := isoWeekBounds(now, 0)
		tue := mon.AddDate(0, 0, 1)
		return rangeResult("Anfang der Woche", fmtDate(mon), fmtDate(tue), "past")
	}
	if reEndNextWeekDE.MatchString(lower) {
		mon, sun := isoWeekBounds(now, 1)
		fri := mon.AddDate(0, 0, 4) // Fri-Sun = "Ende der Woche"
		return rangeResult("Ende nächster Woche", fmtDate(fri), fmtDate(sun), "future")
	}
	if reEndeWoche.MatchString(lower) {
		mon, sun := isoWeekBounds(now, 0)
		fri := mon.AddDate(0, 0, 4) // Fri-Sun = "Ende der Woche"
		return rangeResult("Ende der Woche", fmtDate(fri), fmtDate(sun), "future")
	}
	return nil
}

func matchRelativeWeek(query string, now time.Time) *TemporalResult {
	lower := strings.ToLower(query)
	if reWeekAfterNextDE.MatchString(lower) {
		mon, sun := isoWeekBounds(now, 2)
		return rangeResult("übernächste Woche", fmtDate(mon), fmtDate(sun), "range")
	}
	if reLastWeekDE.MatchString(lower) {
		mon, sun := isoWeekBounds(now, -1)
		return rangeResult("letzte Woche", fmtDate(mon), fmtDate(sun), "range")
	}
	if reNextWeekDE.MatchString(lower) || reNextWeek.MatchString(lower) {
		mon, sun := isoWeekBounds(now, 1)
		return rangeResult("nächste Woche", fmtDate(mon), fmtDate(sun), "range")
	}
	if reThisWeekDE.MatchString(lower) || reThisWeek.MatchString(lower) {
		mon, sun := isoWeekBounds(now, 0)
		return rangeResult("diese Woche", fmtDate(mon), fmtDate(sun), "range")
	}
	return nil
}

func matchRelativeMonth(query string, now time.Time) *TemporalResult {
	lower := strings.ToLower(query)
	if reLastMonthDE.MatchString(lower) || reLastMonth.MatchString(lower) {
		first, last := monthBounds(now, -1)
		return rangeResult("letzten Monat", fmtDate(first), fmtDate(last), "range")
	}
	if reThisMonthDE.MatchString(lower) || reThisMonth.MatchString(lower) {
		first, last := monthBounds(now, 0)
		return rangeResult("diesen Monat", fmtDate(first), fmtDate(last), "range")
	}
	return nil
}

// matchMonthSegment detects day-of-month patterns: Monatsanfang, Monatsmitte,
// Monatsende. Returns a reference date within the current month for monthday
// phase calculation. Pure monthday dimension (no linear component) because
// "Monatsanfang" is cyclical across months.
var (
	reMonthStart = regexp.MustCompile(`(?i)\b(monatsanfang|anfang\s+des\s+monats|zum\s+(?:monats)?ersten|beginning\s+of\s+(?:the\s+)?month|start\s+of\s+(?:the\s+)?month|month[- ]?start)\b`)
	reMonthEnd   = regexp.MustCompile(`(?i)\b(monatsende|ende\s+des\s+monats|end\s+of\s+(?:the\s+)?month|month[- ]?end)\b`)
	reMonthMid   = regexp.MustCompile(`(?i)\b(monatsmitte|mitte\s+des\s+monats|mid[- ]?month)\b`)
)

func matchMonthSegment(query string) *TemporalResult {
	lower := strings.ToLower(query)
	switch {
	case reMonthStart.MatchString(lower):
		// Reference: day 3 (middle of first week of month)
		return &TemporalResult{
			Dates: []TemporalDate{{Ref: "Monatsanfang", Date: "2026-01-03", Dir: "range"}},
		}
	case reMonthEnd.MatchString(lower):
		// Reference: day 29 (last few days)
		return &TemporalResult{
			Dates: []TemporalDate{{Ref: "Monatsende", Date: "2026-01-29", Dir: "range"}},
		}
	case reMonthMid.MatchString(lower):
		return &TemporalResult{
			Dates: []TemporalDate{{Ref: "Monatsmitte", Date: "2026-01-15", Dir: "range"}},
		}
	}
	return nil
}

// matchAnnualHoliday detects fixed-date annual holidays. Returns a reference
// date with month + seasonal dimensions. Phase calculation uses day-of-year
// so the same date in any year matches.
var (
	reWeihnachten = regexp.MustCompile(`(?i)\b(weihnachten|heiligabend|christmas|xmas)\b`)
	reSilvester   = regexp.MustCompile(`(?i)\b(silvester|new\s*year'?s?\s*eve|nye)\b`)
	reNeujahr     = regexp.MustCompile(`(?i)\b(neujahr|new\s*year(?:'?s(?:\s*day)?)?)\b`)
)

func matchAnnualHoliday(query string, now time.Time) *TemporalResult {
	lower := strings.ToLower(query)
	year := now.Year()
	switch {
	case reWeihnachten.MatchString(lower):
		return singleResult("Weihnachten", fmt.Sprintf("%d-12-25", year), "range")
	case reSilvester.MatchString(lower):
		return singleResult("Silvester", fmt.Sprintf("%d-12-31", year), "range")
	case reNeujahr.MatchString(lower):
		return singleResult("Neujahr", fmt.Sprintf("%d-01-01", year), "range")
	}
	return nil
}

// matchTimeOfDay detects time-of-day patterns: morgens, mittags, abends, nachts.
// Returns a reference date at today with the appropriate hour. The daily
// dimension uses hour-of-day for cyclic matching.
var (
	reMorgens     = regexp.MustCompile(`(?i)\b(morgens|fr[üu]h|morning|early)\b`)
	reVormittags  = regexp.MustCompile(`(?i)\b(vormittags|late\s*morning)\b`)
	reMittags     = regexp.MustCompile(`(?i)\b(mittags|noon|midday|lunchtime)\b`)
	reNachmittags = regexp.MustCompile(`(?i)\b(nachmittags|afternoon)\b`)
	reAbends      = regexp.MustCompile(`(?i)\b(abends|evening)\b`)
	reNachts      = regexp.MustCompile(`(?i)\b(nachts|night|sp[äa]t|late)\b`)
)

func matchTimeOfDay(query string, now time.Time) *TemporalResult {
	lower := strings.ToLower(query)
	today := now.Format("2006-01-02")
	// Reference hour per pattern (middle of each daypart).
	mk := func(ref string, hour int) *TemporalResult {
		h := hour
		return &TemporalResult{
			Dates: []TemporalDate{{Ref: ref, Date: today, Dir: "today", Hour: &h}},
		}
	}
	switch {
	case reMorgens.MatchString(lower):
		return mk("morgens", 8) // 07:00-10:00
	case reVormittags.MatchString(lower):
		return mk("vormittags", 10) // 10:00-12:00
	case reMittags.MatchString(lower):
		return mk("mittags", 12) // 12:00-14:00
	case reNachmittags.MatchString(lower):
		return mk("nachmittags", 15) // 14:00-17:00
	case reAbends.MatchString(lower):
		return mk("abends", 19) // 17:00-22:00
	case reNachts.MatchString(lower):
		return mk("nachts", 23) // 22:00-06:00
	}
	return nil
}

func matchRelativeDays(query string, now time.Time) *TemporalResult {
	lower := strings.ToLower(query)
	if m := reVorNTagen.FindStringSubmatch(lower); m != nil {
		n, _ := strconv.Atoi(m[1])
		d := truncDate(now.AddDate(0, 0, -n))
		dir := "past"
		if n == 0 {
			dir = "today"
		}
		return singleResult("vor "+m[1]+" Tagen", fmtDate(d), dir)
	}
	if m := reInNTagen.FindStringSubmatch(lower); m != nil {
		n, _ := strconv.Atoi(m[1])
		d := truncDate(now.AddDate(0, 0, n))
		return singleResult("in "+m[1]+" Tagen", fmtDate(d), "future")
	}
	if m := reNDaysAgo.FindStringSubmatch(lower); m != nil {
		n, _ := strconv.Atoi(m[1])
		d := truncDate(now.AddDate(0, 0, -n))
		return singleResult(m[1]+" days ago", fmtDate(d), "past")
	}
	if reInEinerWoche.MatchString(lower) {
		d := truncDate(now.AddDate(0, 0, 7))
		return singleResult("in einer Woche", fmtDate(d), "future")
	}
	return nil
}

func matchWDInWoche(query string, now time.Time) *TemporalResult {
	m := reWeekdayInOneWeek.FindStringSubmatch(strings.ToLower(query))
	if m == nil {
		return nil
	}
	wd, ok := weekdayMapDE[m[1]]
	if !ok {
		return nil
	}
	nextOccurrence := resolveWeekdayDate(wd, false, now)
	d := nextOccurrence.AddDate(0, 0, 7)
	return singleResult(m[1]+" in einer Woche", fmtDate(d), "future")
}

func matchWeekend(query string, now time.Time) *TemporalResult {
	if !reWochenende.MatchString(strings.ToLower(query)) {
		return nil
	}
	tense := DetectVerbTense(query)

	todayWD := now.Weekday()
	isWeekend := todayWD == time.Saturday || todayWD == time.Sunday

	if tense == "past" {
		// Past: this weekend if currently on weekend, else last weekend
		if isWeekend {
			sat := resolveWeekdayDate(time.Saturday, true, now)
			// If today is Saturday, "this weekend" = today+tomorrow
			if todayWD == time.Saturday {
				sat = truncDate(now)
			}
			sun := sat.AddDate(0, 0, 1)
			return rangeResult("am Wochenende", fmtDate(sat), fmtDate(sun), "past")
		}
		// Last weekend
		lastSat := resolveWeekdayDate(time.Saturday, true, now)
		lastSun := lastSat.AddDate(0, 0, 1)
		return rangeResult("am Wochenende", fmtDate(lastSat), fmtDate(lastSun), "past")
	}

	// Future or neutral: next weekend (skip current if on weekend)
	if isWeekend {
		// Next weekend, not this one
		nextSat := resolveWeekdayDate(time.Saturday, false, now)
		// If today is Saturday, nextSat is next Saturday (7 days)
		// If today is Sunday, nextSat is next Saturday (6 days)
		nextSun := nextSat.AddDate(0, 0, 1)
		return rangeResult("am Wochenende", fmtDate(nextSat), fmtDate(nextSun), "future")
	}
	nextSat := resolveWeekdayDate(time.Saturday, false, now)
	nextSun := nextSat.AddDate(0, 0, 1)
	return rangeResult("am Wochenende", fmtDate(nextSat), fmtDate(nextSun), "future")
}

// findAllWeekdays returns all weekday tokens found in the query with their positions.
func findAllWeekdays(tokens []string) []struct {
	wd  time.Weekday
	idx int
	ref string
} {
	var results []struct {
		wd  time.Weekday
		idx int
		ref string
	}
	for i, tok := range tokens {
		if wd, ok := weekdayMapDE[tok]; ok {
			results = append(results, struct {
				wd  time.Weekday
				idx int
				ref string
			}{wd, i, weekdayNameDE[wd]})
		} else if wd, ok := weekdayMapEN[tok]; ok {
			results = append(results, struct {
				wd  time.Weekday
				idx int
				ref string
			}{wd, i, weekdayNameEN[wd]})
		}
	}
	return results
}

func matchMultiKeyword(query string, now time.Time) *TemporalResult {
	lower := strings.ToLower(query)
	tokens := ruleTokenize(query)

	hasConjunction := strings.Contains(lower, " und ") || strings.Contains(lower, " and ")
	hasDisjunction := strings.Contains(lower, " oder ") || strings.Contains(lower, " or ")

	if !hasConjunction && !hasDisjunction {
		return nil
	}

	// First: check for weekday disjunction/conjunction ("Montag oder Dienstag", "Monday or Tuesday")
	weekdays := findAllWeekdays(tokens)
	if len(weekdays) >= 2 {
		tense := DetectVerbTense(query)
		backward := tense == "past"
		dir := "future"
		if backward {
			dir = "past"
		}
		var dates []TemporalDate
		seen := make(map[string]bool)
		for _, w := range weekdays {
			d := resolveWeekdayDate(w.wd, backward, now)
			key := fmtDate(d)
			if !seen[key] {
				seen[key] = true
				dates = append(dates, TemporalDate{
					Ref: w.ref, Date: key, Dir: dir,
				})
			}
		}
		if len(dates) >= 2 {
			return multiResult(dates)
		}
	}

	// Then: simple keyword conjunction/disjunction (heute und morgen, etc.)
	var dates []TemporalDate
	seen := make(map[string]bool)
	for _, kw := range simpleKeywords {
		if strings.Contains(lower, kw.word) {
			d := truncDate(kw.resolve(now))
			key := fmtDate(d)
			if !seen[key] {
				seen[key] = true
				dates = append(dates, TemporalDate{
					Ref: kw.word, Date: key, Dir: kw.dir,
				})
			}
		}
	}
	if len(dates) >= 2 {
		// Determine overall direction
		dir := "range"
		return &TemporalResult{
			Dates: []TemporalDate{{
				Ref:  dates[0].Ref + " und " + dates[len(dates)-1].Ref,
				Date: dates[0].Date,
				End:  ptrStr(dates[len(dates)-1].Date),
				Dir:  dir,
			}},
		}
	}
	return nil
}

func matchWeekday(query string, tokens []string, now time.Time) *TemporalResult {
	wd, wdIdx, found := findWeekday(tokens)
	if !found {
		return nil
	}

	// Disambiguate "Morgen" as time-of-day vs "morgen" as tomorrow.
	// If a weekday precedes "morgen", it's morning, not tomorrow.
	// "Mittwoch Morgen" — morgen is morning; no action needed here.

	tense := DetectVerbTense(query)
	backward := tense == "past"
	// neutral = forward (bare weekday default)
	d := resolveWeekdayDate(wd, backward, now)
	dir := "future"
	if backward {
		dir = "past"
	}

	ref := weekdayNameDE[wd]
	if _, ok := weekdayMapEN[tokens[wdIdx]]; ok {
		ref = weekdayNameEN[wd]
	}
	return singleResult(ref, fmtDate(d), dir)
}

func matchSimpleKeywords(query string, now time.Time) *TemporalResult {
	tokens := ruleTokenize(query)
	var dates []TemporalDate
	seen := make(map[string]bool)

	for _, tok := range tokens {
		for _, kw := range simpleKeywords {
			if fuzzyMatch(tok, kw.word) {
				d := truncDate(kw.resolve(now))
				key := fmtDate(d)
				if !seen[key] {
					seen[key] = true
					dates = append(dates, TemporalDate{
						Ref: kw.word, Date: key, Dir: kw.dir,
					})
				}
				break
			}
		}
	}

	if len(dates) == 0 {
		return nil
	}
	if len(dates) == 1 {
		return &TemporalResult{Dates: dates}
	}
	// Multiple keywords: combine as range or multi
	dir := dates[0].Dir
	for _, d := range dates[1:] {
		if d.Dir != dir {
			dir = "range"
		}
	}
	return multiResult(dates)
}

// isYear returns true if tok is a 4-digit year between 1900 and 2100.
func isYear(tok string) (int, bool) {
	if len(tok) != 4 {
		return 0, false
	}
	y, err := strconv.Atoi(tok)
	if err != nil {
		return 0, false
	}
	if y >= 1900 && y <= 2100 {
		return y, true
	}
	return 0, false
}

// resolveMonthYear returns the 1st of the given month in the explicit year.
func resolveMonthYear(m time.Month, year int, now time.Time) time.Time {
	return time.Date(year, m, 1, 0, 0, 0, 0, now.Location())
}

// resolveMonthPosition applies a position modifier (start/mid/end) to a month date.
func resolveMonthPosition(pos string, year int, m time.Month, loc *time.Location) time.Time {
	switch pos {
	case "mid":
		return time.Date(year, m, 15, 0, 0, 0, 0, loc)
	case "end":
		return time.Date(year, m+1, 0, 0, 0, 0, 0, loc) // last day
	default: // "start"
		return time.Date(year, m, 1, 0, 0, 0, 0, loc)
	}
}

// resolveThreeWordPhrase handles 3-token patterns: "Ende März 2025", "letzten August 2024".
func resolveThreeWordPhrase(words []string, now time.Time) (time.Time, bool) {
	// Position + Month + Year: "Ende März 2025" → last day of March 2025
	if pos, ok := monthPositionModifiers[words[0]]; ok {
		if m, mOk := lookupMonth(words[1]); mOk {
			if y, yOk := isYear(words[2]); yOk {
				return resolveMonthPosition(pos, y, m, now.Location()), true
			}
		}
	}
	// Modifier + Month + Year: "letzten August 2024" — modifier is redundant with explicit year
	if explicitPastSet[words[0]] || explicitFutureSet[words[0]] {
		if m, mOk := lookupMonth(words[1]); mOk {
			if y, yOk := isYear(words[2]); yOk {
				return resolveMonthYear(m, y, now), true
			}
		}
	}
	return time.Time{}, false
}

// resolvePhrase resolves a 1-3 word temporal phrase like "letztem Montag", "nächsten Freitag",
// "vorgestern", "August 2024", "Ende März 2025".
// For multi-word phrases, tries specific patterns first, then falls back to fewer words.
func resolvePhrase(phrase string, defaultBackward bool, now time.Time) time.Time {
	words := strings.Fields(strings.ToLower(strings.Trim(phrase, ".,;:!?")))
	if len(words) == 0 {
		return time.Time{}
	}

	// Single word: delegate to resolveToken
	if len(words) == 1 {
		return resolveToken(words[0], defaultBackward, now)
	}

	// --- 3-word patterns ---
	if len(words) >= 3 {
		if t, ok := resolveThreeWordPhrase(words[:3], now); ok {
			return t
		}
	}

	// --- 2-word patterns ---
	w0, w1 := words[0], words[1]

	// Month + Year: "August 2024" → 2024-08-01 (explicit year, no direction guessing)
	if m, mOk := lookupMonth(w0); mOk {
		if y, yOk := isYear(w1); yOk {
			return resolveMonthYear(m, y, now)
		}
	}

	// Year + Month: "2024 August" → 2024-08-01
	if y, yOk := isYear(w0); yOk {
		if m, mOk := lookupMonth(w1); mOk {
			return resolveMonthYear(m, y, now)
		}
	}

	// Direction modifier + keyword: "letztem Montag", "nächsten Freitag"
	if explicitPastSet[w0] || explicitFutureSet[w0] {
		if !resolveToken(w1, defaultBackward, now).IsZero() {
			backward := explicitPastSet[w0]
			return resolveToken(w1, backward, now)
		}
	}

	// Month position modifier: "Anfang März" → 1st March, "Mitte April" → 15th, "Ende Februar" → last day
	if pos, ok := monthPositionModifiers[w0]; ok {
		if m, mOk := lookupMonth(w1); mOk {
			base := resolveMonthDate(m, defaultBackward, now)
			return resolveMonthPosition(pos, base.Year(), base.Month(), base.Location())
		}
	}

	// Fallback: try first word as standalone token (greedy regex captured a non-temporal second word)
	return resolveToken(words[0], defaultBackward, now)
}

// resolveToken resolves a single token to a date (for compound patterns).
func resolveToken(tok string, backward bool, now time.Time) time.Time {
	tok = strings.ToLower(strings.Trim(tok, ".,;:!?"))
	// Simple keywords
	for _, kw := range simpleKeywords {
		if fuzzyMatch(tok, kw.word) {
			return truncDate(kw.resolve(now))
		}
	}
	// Weekdays
	if wd, ok := weekdayMapDE[tok]; ok {
		return resolveWeekdayDate(wd, backward, now)
	}
	if wd, ok := weekdayMapEN[tok]; ok {
		return resolveWeekdayDate(wd, backward, now)
	}
	// Month names
	if m, ok := lookupMonth(tok); ok {
		return resolveMonthDate(m, backward, now)
	}
	// ISO date
	if t, err := time.Parse("2006-01-02", tok); err == nil {
		return t
	}
	return time.Time{} // zero = unresolved
}

// --- Main Entry Point ---.

// NormalizeTemporalRules performs deterministic temporal resolution.
// Returns nil if no temporal references are found.
// This is the PRIMARY path — LLM (NormalizeTemporal) is fallback only.
//
// Each matcher returns dates; this function tags the result with DimensionWeights
// based on which matcher fired (GottZ Cyclic Phase Model). Recurrence keywords
// ("immer", "jeden", "dienstags") upgrade weekday-matchers to pure-weekday weights.
func NormalizeTemporalRules(query string, now time.Time) *TemporalResult {
	if strings.TrimSpace(query) == "" {
		return nil
	}
	now = truncDate(now)
	tokens := ruleTokenize(query)
	recur := hasRecurrence(tokens)

	// Priority 1: ISO dates (most specific) — pure linear
	if r := matchISODate(query); r != nil {
		r.DimensionWeights = dwLinear()
		return r
	}

	// Priority 2: Compound range patterns — linear + weekday (range endpoints)
	if r := matchLetztenWDBis(query, now); r != nil {
		r.DimensionWeights = dwLinearRange()
		return r
	}
	if r := matchNextThrough(query, now); r != nil {
		r.DimensionWeights = dwLinearRange()
		return r
	}
	if r := matchVonBis(query, now); r != nil {
		r.DimensionWeights = dwLinearRange()
		return r
	}

	// Priority 3: Seit/Bis — linear-dominant + month (spans weeks/months)
	if r := matchSeit(query, now); r != nil {
		r.DimensionWeights = dwLinearMonthSeit()
		return r
	}
	if r := matchBis(query, now); r != nil {
		r.DimensionWeights = dwLinearMonthSeit()
		return r
	}

	// Priority 4: Week segments — linear + week
	if r := matchWeekSegment(query, now); r != nil {
		r.DimensionWeights = dwLinearWeek()
		return r
	}

	// Priority 5: Relative weeks — linear + week
	if r := matchRelativeWeek(query, now); r != nil {
		r.DimensionWeights = dwLinearWeek()
		return r
	}

	// Priority 6: Relative months — linear + month
	if r := matchRelativeMonth(query, now); r != nil {
		r.DimensionWeights = dwLinearMonth()
		return r
	}

	// Priority 7: Weekday in einer Woche (BEFORE relative days)
	if r := matchWDInWoche(query, now); r != nil {
		r.DimensionWeights = dwLinearWeekday()
		return r
	}

	// Priority 8: Relative days (vor N Tagen, in N Tagen) — pure linear
	if r := matchRelativeDays(query, now); r != nil {
		r.DimensionWeights = dwLinear()
		return r
	}

	// Priority 8a: Annual holidays — month+seasonal (fixed-date recurring)
	if r := matchAnnualHoliday(query, now); r != nil {
		r.DimensionWeights = dwHoliday()
		return r
	}

	// Priority 8b: Month segments (Monatsanfang/Monatsende/Monatsmitte) — monthday
	if r := matchMonthSegment(query); r != nil {
		r.DimensionWeights = dwMonthday()
		return r
	}

	// Priority 8c: Time-of-day (morgens/abends/nachts) — daily dimension
	if r := matchTimeOfDay(query, now); r != nil {
		r.DimensionWeights = dwDaily()
		return r
	}

	// Priority 9: Weekend — weekday-dominant (Sat+Sun are THE weekend)
	if r := matchWeekend(query, now); r != nil {
		r.DimensionWeights = dwWeekday()
		return r
	}

	// Priority 10: Multi-keyword (heute und morgen, Montag oder Dienstag)
	if r := matchMultiKeyword(query, now); r != nil {
		if len(findAllWeekdays(tokens)) >= 2 {
			if recur {
				r.DimensionWeights = dwWeekday()
			} else {
				r.DimensionWeights = dwLinearWeekday()
			}
		} else {
			r.DimensionWeights = dwLinear()
		}
		return r
	}

	// Priority 11: Weekday + tense — cyclic if recurrence detected
	if r := matchWeekday(query, tokens, now); r != nil {
		if recur {
			r.DimensionWeights = dwWeekday()
		} else {
			r.DimensionWeights = dwLinearWeekday()
		}
		return r
	}

	// Priority 12: Simple keywords (heute/gestern/morgen + typos) — pure linear
	if r := matchSimpleKeywords(query, now); r != nil {
		r.DimensionWeights = dwLinear()
		return r
	}

	return nil
}

// --- Helpers ---.

func toWordSet(words ...string) map[string]bool {
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}

