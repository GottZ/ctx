// temporal_rules.go — Deterministic Temporal Resolution
// Part of ctx by GottZ and Damien Moon — The memory your LLM pretends to have.
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
	"sort"
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

// lookupMonth returns the month for a German or English month name.
// The tables are util.MonthDE/MonthEN, shared with the date extraction in
// internal/store — the two packages carried identical copies (design D-04,
// Naht 9) — and so is the key a name is looked up by, util.MonthKey: a
// lowercased token keeps the long s ("auguſt"), which the key maps to s as it
// does for the write side (issue #463).
//
// German is consulted first, and the order decides nothing: the four keys that
// appear in both tables ("april", "august", "november", "september") are
// spelled the same in both languages and carry the same month.
func lookupMonth(tok string) (time.Month, bool) {
	tok = util.MonthKey(tok)
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
	// "since" and "until" are the English "seit" and "bis" (issue #463).
	reSeit            = regexp.MustCompile(`(?i)\b(seit|since)\s+(?:dem\s+)?((?:\S+\s+)?(?:\S+\s+)?\S+)`)
	reBis             = regexp.MustCompile(`(?i)\b(bis|until)\s+(?:zu[mr]\s+)?((?:\S+\s+)?(?:\S+\s+)?\S+)`)
	// "vom" is "von dem"; "bis zum"/"bis zur" as in reBis (issue #463).
	reVonBis          = regexp.MustCompile(`(?i)(?:von|\bvom)\s+((?:\S+\s+)?(?:\S+\s+)?\S+)\s+bis\s+(?:zu[mr]\s+)?((?:\S+\s+)?(?:\S+\s+)?\S+)`)
	// "from X to Y" is the English "von X bis Y"; "ab X bis Y" is "von X bis
	// Y" when X is a date (issue #463).
	reFromTo          = regexp.MustCompile(`(?i)\bfrom\s+((?:\S+\s+)?(?:\S+\s+)?\S+)\s+(?:to|until|through)\s+((?:\S+\s+)?(?:\S+\s+)?\S+)`)
	reAbBis           = regexp.MustCompile(`(?i)\bab\s+(?:dem\s+)?((?:\S+\s+)?(?:\S+\s+)?\S+)\s+bis\s+(?:zu[mr]\s+)?((?:\S+\s+)?(?:\S+\s+)?\S+)`)
	// "seit X bis Y" and "since X until Y": the window from X to Y (issue #463).
	reSeitBis         = regexp.MustCompile(`(?i)\b(?:seit|since)\s+(?:dem\s+)?((?:\S+\s+)?(?:\S+\s+)?\S+)\s+(?:bis|until)\s+(?:zu[mr]\s+)?((?:\S+\s+)?(?:\S+\s+)?\S+)`)
	// "ab", "vor", "nach", "before", "after" before a date (matchOpenEdge).
	reOpenEdge        = regexp.MustCompile(`(?i)\b(ab|vor|nach|before|after)\s+(?:(?:dem|the)\s+)?((?:\S+\s+)?(?:\S+\s+)?\S+)`)
	reThrough         = regexp.MustCompile(`(?i)(monday|tuesday|wednesday|thursday|friday|saturday|sunday)\s+through\s+(monday|tuesday|wednesday|thursday|friday|saturday|sunday)`)
	reAnfangWoche     = regexp.MustCompile(`(?i)anfang\s+(?:der|dieser)\s+woche`)
	reEndeWoche       = regexp.MustCompile(`(?i)ende\s+(?:n[äa]chster|dieser|der)?\s*woche`)
	reEndNextWeekDE   = regexp.MustCompile(`(?i)ende\s+n[äa]chster\s+woche`)
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

// The two day forms of the write side (store.ExtractDates, the expressions
// in the head of internal/store/dates.go): an ISO date with an optional time
// and a German dot date with an optional time. The read side takes the same
// shapes — the same digits, separators, spaces (\s and the two no-break
// spaces) and ASCII word boundaries, so the same text yields the same day on
// both sides (TestDayDates_AgreeWithTheWriteSide) — with its own year window,
// 2000-2099 instead of the write side's 2020-2099: a question about a day no
// content date can carry still resolves without the LLM, and its gravity
// finds nothing to reward (issue #463). The time is read past, not used.
var (
	reISODayDate = regexp.MustCompile(`\b(20\d{2})-(0[1-9]|1[0-2])-(0[1-9]|[12]\d|3[01])(?:[T\s\x{A0}\x{202F}]+(?:[01]\d|2[0-3]):[0-5]\d(?::[0-5]\d)?Z?)?\b`)
	reDotDayDate = regexp.MustCompile(`\b(0[1-9]|[12]\d|3[01])\.(0[1-9]|1[0-2])\.(20\d{2})(?:[\s\x{A0}\x{202F}]+(?:[01]\d|2[0-3]):[0-5]\d)?\b`)
)

// dayDate is one day date of a query: where its match stands (with the
// time), where its date stands (without it) and its day.
type dayDate struct {
	start, end         int
	dateStart, dateEnd int
	day                time.Time
}

// findDayDates returns the day dates of query in the order they stand, each
// form's matches as FindAll gives them. A date the calendar does not have
// (2024-02-30, 30.02.2024) is no day date, as on the write side.
func findDayDates(query string) []dayDate {
	var out []dayDate
	add := func(m []int, y, mo, d string) {
		if t, err := time.Parse("2006-01-02", y+"-"+mo+"-"+d); err == nil {
			out = append(out, dayDate{start: m[0], end: m[1], dateStart: m[2], dateEnd: m[7], day: t})
		}
	}
	for _, m := range reISODayDate.FindAllStringSubmatchIndex(query, -1) {
		add(m, query[m[2]:m[3]], query[m[4]:m[5]], query[m[6]:m[7]])
	}
	for _, m := range reDotDayDate.FindAllStringSubmatchIndex(query, -1) {
		add(m, query[m[6]:m[7]], query[m[4]:m[5]], query[m[2]:m[3]])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].start < out[j].start })
	return out
}

// parseDayDate resolves a token that is exactly one day date of either form.
func parseDayDate(tok string) (time.Time, bool) {
	ds := findDayDates(tok)
	if len(ds) != 1 || ds[0].start != 0 || ds[0].end != len(tok) {
		return time.Time{}, false
	}
	return ds[0].day, true
}

// reISODateRangeIndicator matches a range word between two day dates: a
// dash, or "bis", "to" or "through" as a word of its own ("bis zum" counts,
// "Tomaten" does not).
var reISODateRangeIndicator = regexp.MustCompile(`(?i)(?:^|\s)(?:bis|to|through)(?:\s|$)|–|—`)

// reSeitBisBefore matches "seit"/"bis" (with the article German puts between,
// "seit dem", "bis zum") and "von"/"vom" right before a day date: the date is
// then the start or the end of the window matchSeit, matchBis or matchVonBis
// derive, not a day of its own. So do their English kin "since", "until" and
// "from", and the words of matchOpenEdge, "ab", "vor", "nach" (with "dem"),
// "before" and "after" (with "the"), issue #463.
var reSeitBisBefore = regexp.MustCompile(`(?i)\b(?:(?:seit|ab|vor|nach)(?:\s+dem)?|bis(?:\s+zu[mr])?|vo[nm]|since|until|from|(?:before|after)(?:\s+the)?)\s+$`)

// reSpacedHyphen matches a hyphen between spaces, the whole text between two
// day dates ("15.03.2026 - 20.03.2026"): a range word as a dash is. A
// hyphen without spaces stays none — it is a part of the ISO form.
var reSpacedHyphen = regexp.MustCompile(`^\s+-\s+$`)

// reBetweenStart and reBetweenAnd match "zwischen"/"between" right before
// the first of two day dates and "und"/"and" as the whole text between them
// ("zwischen dem 01.03.2024 und dem 31.03.2024"): the window they span.
var (
	reBetweenStart = regexp.MustCompile(`(?i)\b(?:zwischen(?:\s+dem)?|between(?:\s+the)?)\s+$`)
	reBetweenAnd   = regexp.MustCompile(`(?i)^\s+(?:und(?:\s+dem)?|and(?:\s+the)?)\s+$`)
)

// reYearlessStart matches a dot date without its year ("01.03.", "01.03")
// and a range word right before a day date, the German "01.03. bis
// 05.03.2026" and "01.03.–05.03.2026": the start takes the end's year. It
// stands at the start of the text or behind a character that is neither a
// letter, a digit nor a dot, so "2.01.03 bis" is no start.
var reYearlessStart = regexp.MustCompile(`(?i)(?:^|[^0-9A-Za-z_.])((0[1-9]|[12]\d|3[01])\.(0[1-9]|1[0-2])\.?)(?:\s*[–—]\s*|\s+(?:bis(?:\s+zu[mr])?|to|through)\s+)$`)

// yearlessStart returns the window a yearless start before the day date m
// opens: the start is the latest such day at or before the end, in the end's
// year or the year before ("28.12. bis 03.01.2027" starts in 2026).
// matched reports that the text before m has the shape; a start the calendar
// has in neither year ("29.02. bis 05.03.2027") opens no window (ok false).
func yearlessStart(query string, m dayDate) (start time.Time, ref string, matched, ok bool) {
	sm := reYearlessStart.FindStringSubmatchIndex(query[:m.start])
	if sm == nil {
		return time.Time{}, "", false, false
	}
	day, month := query[sm[4]:sm[5]], query[sm[6]:sm[7]]
	for _, y := range []int{m.day.Year(), m.day.Year() - 1} {
		t, err := time.Parse("2006-01-02", fmt.Sprintf("%04d-%s-%s", y, month, day))
		if err == nil && !t.After(m.day) {
			return t, query[sm[2]:sm[3]], true, true
		}
	}
	return time.Time{}, "", true, false
}

// leftToWindow reports whether the only day date of query stands right
// behind "seit", "bis" or "von" and is left to the window rules: not behind a
// yearless start, whose window matchDayDates opens itself.
func leftToWindow(query string, matches []dayDate) bool {
	if len(matches) != 1 || !reSeitBisBefore.MatchString(query[:matches[0].start]) {
		return false
	}
	_, _, yearless, _ := yearlessStart(query, matches[0])
	return !yearless
}

// dayDateLeftToWindow returns the day date of query left to the window
// rules (leftToWindow), formatted, or "".
func dayDateLeftToWindow(query string) string {
	if ds := findDayDates(query); leftToWindow(query, ds) {
		return fmtDate(ds[0].day)
	}
	return ""
}

// takesEdge reports whether a window rule's result stands: with a day date
// left to the window rules (edge), only a window that has it as an edge
// does, so a window another phrase of the question opens ("Seit Montag …,
// was bis 2026-12-31 …") does not drop the date (issue #463).
func takesEdge(r *TemporalResult, edge string) bool {
	if r == nil || len(r.Dates) == 0 {
		return false
	}
	d := r.Dates[0]
	return edge == "" || d.Date == edge || (d.End != nil && *d.End == edge)
}

// matchDayDates resolves the day dates of query. With deferToSeitBis a
// single date right behind "seit", "bis" or "von" is left to the window
// rules (leftToWindow); the rules call it again without, after them, so a
// date no window takes as an edge ("von Projektstart bis 2026-03-01") stays
// the day it names.
func matchDayDates(query string, deferToSeitBis bool) *TemporalResult {
	matches := findDayDates(query)
	if len(matches) == 0 {
		return nil
	}
	// A yearless start before the only day date: the window from it.
	if len(matches) == 1 {
		if start, sref, _, ok := yearlessStart(query, matches[0]); ok {
			return rangeResult(sref+" – "+query[matches[0].dateStart:matches[0].dateEnd], fmtDate(start), fmtDate(matches[0].day), "range")
		}
	}
	if deferToSeitBis && leftToWindow(query, matches) {
		return nil
	}
	tense := DetectVerbTense(query)
	dir := "past"
	if tense == "future" {
		dir = "future"
	}
	ref := func(m dayDate) string { return query[m.dateStart:m.dateEnd] }

	// Two day dates with a range word between them, a hyphen between spaces,
	// or "und" between them after "zwischen": the window they span, whichever
	// of the two the query names first.
	// A dot date and an ISO date can share their year ("29.03.2026-03-29"):
	// both are day dates, as on the write side, with nothing between them.
	if len(matches) == 2 && matches[0].dateEnd <= matches[1].dateStart &&
		spansWindow(query[:matches[0].start], query[matches[0].dateEnd:matches[1].dateStart]) {
		first, second := matches[0].day, matches[1].day
		if second.Before(first) {
			first, second = second, first
		}
		return rangeResult(ref(matches[0])+" – "+ref(matches[1]), fmtDate(first), fmtDate(second), "range")
	}

	// One date, two without a range word, or more: single entries.
	dates := make([]TemporalDate, 0, len(matches))
	for _, m := range matches {
		dates = append(dates, TemporalDate{Ref: ref(m), Date: fmtDate(m.day), Dir: dir})
	}
	return multiResult(dates)
}

// spansWindow reports whether two day dates with the text between and the
// text before the first make a window (matchDayDates).
func spansWindow(before, between string) bool {
	return reISODateRangeIndicator.MatchString(between) || reSpacedHyphen.MatchString(between) ||
		reBetweenAnd.MatchString(between) && reBetweenStart.MatchString(before)
}

// orderWindow orders a window whose end resolved before its start. An edge
// counted from a reference day (startRel/endRel: a bare weekday or month) is
// counted again from the other edge; a fixed edge (heute, gestern, "letzten
// Montag", a date) keeps its day. With both edges counted from a reference,
// looking forward (the default) keeps the start and counts the end on from
// it, laterEnd: the Monday after a Friday, the January after a December; in
// the past tense it keeps the end and counts the start back from it,
// earlierStart: "Was lief von Freitag bis Montag?" is the span that has
// happened. A fixed end keeps the end in either tense ("von März bis heute"
// is this year's March up to today), a fixed start the start. Two fixed edges
// in the wrong order swap: the window between two named days is what they
// span (issue #463); swapped reports it, so that the caller ends the window
// on the last day of what the start named.
func orderWindow(start, end time.Time, startRel, endRel, backward bool, earlierStart, laterEnd func(time.Time) time.Time) (s, e time.Time, swapped bool) {
	if !end.Before(start) {
		return start, end, false
	}
	if startRel && (backward || !endRel) {
		if s := earlierStart(end); !s.After(end) {
			return s, end, false
		}
	}
	if endRel {
		if e := laterEnd(start); !e.Before(start) {
			return start, e, false
		}
	}
	return end, start, true
}

// rangeForms are the forms of a window with a start and an end phrase:
// "von X bis Y" (also "vom X bis zum Y"), "from X to Y" (also "until",
// "through"), "ab X bis Y" and "seit X bis Y" ("since X until Y"). "from" and "ab" are words of their own far more
// often than the start of a window, so their X has to be a date spelt as
// such (exact, phraseEdge.fuzzy); "ab X bis Y" whose X is none stays the
// time up to Y, matchBis (issue #463).
var rangeForms = []struct {
	re    *regexp.Regexp
	ref   func(m []string) string
	exact bool
}{
	{reVonBis, func(m []string) string { return "von " + m[1] + " bis " + m[2] }, false},
	{reFromTo, func(m []string) string { return m[0] }, true},
	{reAbBis, func(m []string) string { return "ab " + m[1] + " bis " + m[2] }, true},
	{reSeitBis, func(m []string) string { return m[0] }, false},
}

func matchVonBis(query string, now time.Time) *TemporalResult {
	// "von Montag bis Mittwoch", "von letztem Montag bis nächsten Freitag", "von gestern bis übermorgen"
	lower := strings.ToLower(query)
	for _, f := range rangeForms {
		if m := f.re.FindStringSubmatch(lower); m != nil {
			if r := matchRangeForm(query, m, f.ref(m), f.exact, now); r != nil {
				return r
			}
		}
	}
	return nil
}

// nowWords start a range form at today: "von jetzt bis Freitag", "from now
// until Friday" (issue #463). They are no day keyword elsewhere — "now" in a
// question rarely names a day to rank by.
var nowWords = toWordSet("jetzt", "now")

// matchRangeForm derives the window of a range form whose start phrase is
// m[1] and whose end phrase is m[2].
func matchRangeForm(query string, m []string, ref string, exact bool, now time.Time) *TemporalResult {
	tense := DetectVerbTense(query)
	backward := tense == "past"
	startEdge := resolvePhraseEdge(m[1], backward, now)
	if w := phraseWords(m[1]); len(w) > 0 && nowWords[w[0]] {
		startEdge = phraseEdge{day: truncDate(now)}
	}
	endEdge := resolvePhraseEdge(m[2], backward, now)
	if startEdge.day.IsZero() || endEdge.day.IsZero() || exact && startEdge.fuzzy {
		return nil
	}
	if english := strings.HasPrefix(m[0], "from") || strings.HasPrefix(m[0], "since"); english &&
		(bareMonthWord(m[1], startEdge) || bareMonthWord(m[2], endEdge)) {
		return nil
	}
	// A weekday or month after a day date could count from today ("bis
	// Freitag") or from the date ("vom 01.03.2026 bis Ende März"); the rules
	// do not guess, the date stays the day it names.
	if endEdge.rel && len(findDayDates(strings.ToUpper(m[1]))) > 0 {
		return nil
	}
	start, end, swapped := orderWindow(startEdge.day, endEdge.day, startEdge.rel, endEdge.rel, backward,
		func(to time.Time) time.Time { return resolvePhrase(m[1], true, to) },
		func(from time.Time) time.Time { return resolvePhrase(m[2], false, from) })
	// A month at the end includes the month: the window ends on its last day
	// ("von März 2024 bis April 2024" ends on 2024-04-30, issue #463).
	if swapped && startEdge.month || !swapped && endEdge.month {
		end = lastOfMonth(end)
	}
	return rangeResult(ref, fmtDate(start), fmtDate(end), "range")
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
	// "letzten" names the start; only the end is counted again.
	d1, d2, _ = orderWindow(d1, d2, false, true, false, nil, func(from time.Time) time.Time { return resolveWeekdayDate(wd2, false, from) })
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
			d1, d2, _ = orderWindow(d1, d2, false, true, false, nil, func(from time.Time) time.Time { return resolveWeekdayDate(wd2, false, from) })
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
	d1, d2, _ = orderWindow(d1, d2, true, true, backward,
		func(to time.Time) time.Time { return resolveWeekdayDate(wd1, true, to) },
		func(from time.Time) time.Time { return resolveWeekdayDate(wd2, false, from) })
	return rangeResult(m[1]+" through "+m[2], fmtDate(d1), fmtDate(d2), "range")
}

func matchSeit(query string, now time.Time) *TemporalResult {
	m := reSeit.FindStringSubmatch(strings.ToLower(query))
	if m == nil {
		return nil
	}
	e := resolvePhraseEdge(m[2], true, now)
	start := e.day
	// A start after today has no time since it yet ("seit morgen").
	if start.IsZero() || start.After(truncDate(now)) || m[1] == "since" && bareMonthWord(m[2], e) {
		return nil
	}
	return rangeResult(m[1]+" "+m[2], fmtDate(start), fmtDate(truncDate(now)), "range")
}

func matchBis(query string, now time.Time) *TemporalResult {
	lower := strings.ToLower(query)
	// Skip if "von...bis" (handled by matchVonBis). A "from … until" whose
	// start the rules cannot read keeps its "until": "from the start until
	// 2026-03-05" is the time up to that day, and in "from backlog to done
	// until Friday" the "from … to" is no window at all (issue #463).
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
	e := resolvePhraseEdge(m[2], false, now)
	if e.day.IsZero() || m[1] == "until" && bareMonthWord(m[2], e) {
		return nil
	}
	// An end before today ("bis März 2015", "bis gestern") is the time up to
	// it, open towards the past: a window from today would start after it
	// ends (issue #463). A later end is a deadline, the window from today. A
	// month includes the month: the window ends on its last day, and a month
	// that began before today is the time up to its end.
	if e.day.Before(truncDate(now)) {
		return &TemporalResult{Dates: []TemporalDate{{Ref: m[1] + " " + m[2], Date: fmtDate(e.last()), Dir: DirUntil}}}
	}
	return rangeResult(m[1]+" "+m[2], fmtDate(truncDate(now)), fmtDate(e.last()), "range")
}

// matchOpenEdge reads "ab", "vor", "nach", "before" and "after" right before
// a date (issue #463): "ab X" is the time from X on and "nach X" the time
// from the day after X on, open towards the future; "vor X" is the time up to
// the day before X, open towards the past, and with X after today the
// deadline from today to the day before X. A month as X is the whole month
// ("nach März 2024" starts on 2024-04-01). "ab" with a day to come is the
// window into the future it names ("ab nächstem Montag bin ich im Urlaub"),
// and with a past day the time from it on — unlike "seit", which names the
// time up to today and so has none for a day to come.
//
// The five are words of their own far more often than words before a date
// ("nach Hause", "vor allem", "ab Werk", "after 3 retries"): a rule takes the
// phrase only when it starts with a date spelt as such that names its day
// itself — a day date, a month with its year, a day keyword like "heute", a
// weekday or month after "nächsten"/"letzten" — and of several such words the
// first whose phrase is one. A bare weekday or month is no such date: which
// one it means would have to be guessed from the tense, and a month name is a
// word of its own as well ("nach August", "after may I ask"); neither is a
// typo resolveTokenEdge forgives. A count of a time unit before the word
// ("zwei Tage vor dem 15.03.2024", "a week after 2024-03-15") makes it a
// distance from the date, not an edge: the date stays the day it names. With
// a day date the rules left to the windows (edge), only a phrase naming that
// day counts.
func matchOpenEdge(query string, now time.Time, edge string) *TemporalResult {
	lower := strings.ToLower(query)
	for _, m := range reOpenEdge.FindAllStringSubmatchIndex(lower, -1) {
		word, phrase := lower[m[2]:m[3]], lower[m[4]:m[5]]
		if reTimeUnitBefore.MatchString(lower[:m[0]]) || word != "ab" && reNearBefore.MatchString(lower[:m[0]]) {
			continue
		}
		// A year first is no edge here: "2024 Oktober" has no use, and "after
		// 2025 may be deleted" names no month.
		if w := phraseWords(phrase); len(w) > 0 {
			if _, year := isYear(w[0]); year {
				continue
			}
		}
		e := resolvePhraseEdge(phrase, false, now)
		if e.day.IsZero() || e.fuzzy || e.rel || edge != "" && fmtDate(e.day) != edge {
			continue
		}
		ref := word + " " + phrase
		switch word {
		case "ab":
			return &TemporalResult{Dates: []TemporalDate{{Ref: ref, Date: fmtDate(e.day), Dir: DirFrom}}}
		case "nach", "after":
			return &TemporalResult{Dates: []TemporalDate{{Ref: ref, Date: fmtDate(e.last().AddDate(0, 0, 1)), Dir: DirFrom}}}
		default: // vor, before
			// A day to come is a deadline, the window from today to the day
			// before it, as "bis" with a later end is one; a day that has
			// begun is the time up to the day before it.
			if today := truncDate(now); e.day.After(today) {
				return rangeResult(ref, fmtDate(today), fmtDate(e.day.AddDate(0, 0, -1)), "range")
			}
			return &TemporalResult{Dates: []TemporalDate{{Ref: ref, Date: fmtDate(e.day.AddDate(0, 0, -1)), Dir: DirUntil}}}
		}
	}
	return nil
}

// reTimeUnitBefore matches a time unit or a time of day right before one of
// the words of matchOpenEdge ("zwei Tage vor", "a week after", "am Abend
// vor", "the night before", "das Wochenende vor"), reNearBefore a word of
// nearness ("kurz vor", "shortly after"): the word then measures a distance
// from the date, and the question keeps the day it names (matchOpenEdge).
// Only the word right before counts ("Welche Tage waren vor dem …" is an
// edge).
var (
	reTimeUnitBefore = regexp.MustCompile(`(?i)\b(?:sekunde|sekunden|minute|minuten|stunde|stunden|tag|tage|tagen|tages|nacht|nächte|abend|abende|morgen|vormittag|mittag|nachmittag|woche|wochen|wochenende|wochenenden|monat|monate|monaten|monats|quartal|quartale|quartals|jahr|jahre|jahren|jahres|` +
		`second|seconds|minutes|hour|hours|day|days|night|nights|evening|evenings|morning|mornings|noon|afternoon|week|weeks|weekend|weekends|month|months|quarter|quarters|year|years)\s+$`)
	reNearBefore = regexp.MustCompile(`(?i)\b(?:kurz|knapp|direkt|unmittelbar|gleich|shortly|right|just|immediately|soon)\s+$`)
)

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
		// The window from the earliest to the latest day, whichever the
		// query names first ("gestern und vorgestern").
		first, last := dates[0].Date, dates[0].Date
		for _, d := range dates[1:] {
			first, last = min(first, d.Date), max(last, d.Date)
		}
		return &TemporalResult{
			Dates: []TemporalDate{{
				Ref:  dates[0].Ref + " und " + dates[len(dates)-1].Ref,
				Date: first,
				End:  ptrStr(last),
				Dir:  "range",
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

// phraseEdge is what a phrase at a window's edge names (issue #463).
type phraseEdge struct {
	// day is the day the phrase names, the 1st for a month; zero: the phrase
	// names none.
	day time.Time
	// rel: the day is counted from the reference day the phrase is resolved
	// at, so that a window may count it again from its other edge — a bare
	// weekday or month name and a month position without a year ("Ende
	// März"); a word naming a day from today (heute, gestern, "letzten
	// Montag") and a phrase naming its own day (a day date, a month with its
	// year) are fixed.
	rel bool
	// month: the phrase names a whole month (a month name with or without
	// its year, not a day in it); as a window's end it stands for the month's
	// last day (last).
	month bool
	// fuzzy: the day comes from a word the rules take for a day keyword by
	// its typo tolerance (fuzzyMatch), not by its spelling.
	fuzzy bool
}

// last is the last day of what the edge names: its day, and for a month
// the month's last day.
func (e phraseEdge) last() time.Time {
	if e.month {
		return lastOfMonth(e.day)
	}
	return e.day
}

// lastOfMonth returns the last day of t's month.
func lastOfMonth(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month()+1, 0, 0, 0, 0, 0, t.Location())
}

// resolveThreeWordPhrase handles 3-token patterns: "Ende März 2025", "letzten August 2024".
func resolveThreeWordPhrase(words []string, now time.Time) (phraseEdge, bool) {
	// Position + Month + Year: "Ende März 2025" → last day of March 2025
	if pos, ok := monthPositionModifiers[words[0]]; ok {
		if m, mOk := lookupMonth(words[1]); mOk {
			if y, yOk := isYear(words[2]); yOk {
				return phraseEdge{day: resolveMonthPosition(pos, y, m, now.Location())}, true
			}
		}
	}
	// Modifier + Month + Year: "letzten August 2024" — modifier is redundant with explicit year
	if explicitPastSet[words[0]] || explicitFutureSet[words[0]] {
		if m, mOk := lookupMonth(words[1]); mOk {
			if y, yOk := isYear(words[2]); yOk {
				return phraseEdge{day: resolveMonthYear(m, y, now), month: true}, true
			}
		}
	}
	return phraseEdge{}, false
}

// monthWords are the month names that are words of their own as well: the
// English modal "may", the verb and noun "march", the adjective "august". A
// year before one of them is no date ("Tests before 2024 may fail"); after
// it, a year is ("May 2024"), as on the write side.
var monthWords = toWordSet("may", "march", "august")

// bareMonthWord reports whether phrase names a month by a bare name that is a
// word of its own (monthWords). After the English "since", "until" and
// "from" that word is far more often the modal, verb or adjective than the
// month ("Wait until may I ask"), so those rules take it for no month; with
// its year ("since May 2024") it is one, and "seit August" keeps its month
// (issue #463).
func bareMonthWord(phrase string, e phraseEdge) bool {
	w := phraseWords(phrase)
	return e.rel && e.month && len(w) > 0 && monthWords[w[0]]
}

// resolvePhrase resolves a 1-3 word temporal phrase like "letztem Montag", "nächsten Freitag",
// "vorgestern", "August 2024", "Ende März 2025".
// For multi-word phrases, tries specific patterns first, then falls back to fewer words.
func resolvePhrase(phrase string, defaultBackward bool, now time.Time) time.Time {
	return resolvePhraseEdge(phrase, defaultBackward, now).day
}

// resolvePhraseEdge is resolvePhrase with everything a window's edge needs
// to know of the phrase (phraseEdge).
func resolvePhraseEdge(phrase string, defaultBackward bool, now time.Time) phraseEdge {
	words := phraseWords(phrase)
	if len(words) == 0 {
		return phraseEdge{}
	}

	// Single word: delegate to resolveToken
	if len(words) == 1 {
		return resolveTokenEdge(words[0], defaultBackward, now)
	}

	// --- 3-word patterns ---
	if len(words) >= 3 {
		if e, ok := resolveThreeWordPhrase(words[:3], now); ok {
			return e
		}
	}

	// --- 2-word patterns ---
	w0, w1 := words[0], words[1]

	// Month + Year: "August 2024" → 2024-08-01 (explicit year, no direction guessing)
	if m, mOk := lookupMonth(w0); mOk {
		if y, yOk := isYear(w1); yOk {
			return phraseEdge{day: resolveMonthYear(m, y, now), month: true}
		}
	}

	// Year + Month: "2024 Oktober" → 2024-10-01 — not with a month name that
	// is a word of its own ("Logs after 2025 may be deleted", issue #463).
	if y, yOk := isYear(w0); yOk {
		if m, mOk := lookupMonth(w1); mOk && !monthWords[w1] {
			return phraseEdge{day: resolveMonthYear(m, y, now), month: true}
		}
	}

	// Direction modifier + keyword: "letztem Montag", "nächsten Freitag"
	if explicitPastSet[w0] || explicitFutureSet[w0] {
		if !resolveToken(w1, defaultBackward, now).IsZero() {
			e := resolveTokenEdge(w1, explicitPastSet[w0], now)
			e.rel = false
			return e
		}
	}

	// Month position modifier: "Anfang März" → 1st March, "Mitte April" → 15th, "Ende Februar" → last day
	if pos, ok := monthPositionModifiers[w0]; ok {
		if m, mOk := lookupMonth(w1); mOk {
			base := resolveMonthDate(m, defaultBackward, now)
			return phraseEdge{day: resolveMonthPosition(pos, base.Year(), base.Month(), base.Location()), rel: true}
		}
	}

	// Fallback: try first word as standalone token (greedy regex captured a non-temporal second word)
	return resolveTokenEdge(words[0], defaultBackward, now)
}

// phrasePunct are the runes a word of a phrase is trimmed of at both ends:
// the punctuation ruleTokenize trims, and the quotation marks and brackets a
// text puts around a date ("seit „März 2024“", "seit (März 2024)"), which the
// write side reads past as well (issue #463).
const phrasePunct = ".,;:!?\"'()[]{}\u201e\u201c\u201d\u201a\u2018\u2019\u00ab\u00bb\u2039\u203a"

// phraseWords splits a phrase into its lowercased words, each trimmed of
// phrasePunct; a word that is nothing else is left out.
func phraseWords(phrase string) []string {
	var words []string
	for _, w := range strings.Fields(strings.ToLower(phrase)) {
		if w = strings.Trim(w, phrasePunct); w != "" {
			words = append(words, w)
		}
	}
	return words
}

// resolveToken resolves a single token to a date (for compound patterns).
func resolveToken(tok string, backward bool, now time.Time) time.Time {
	return resolveTokenEdge(tok, backward, now).day
}

// resolveTokenEdge is resolveToken with what a window's edge needs to know
// of the token (phraseEdge), as resolvePhraseEdge.
func resolveTokenEdge(tok string, backward bool, now time.Time) phraseEdge {
	tok = strings.ToLower(strings.Trim(tok, ".,;:!?"))
	// Simple keywords
	for _, kw := range simpleKeywords {
		if fuzzyMatch(tok, kw.word) {
			return phraseEdge{day: truncDate(kw.resolve(now)), fuzzy: tok != kw.word}
		}
	}
	// Weekdays
	if wd, ok := weekdayMapDE[tok]; ok {
		return phraseEdge{day: resolveWeekdayDate(wd, backward, now), rel: true}
	}
	if wd, ok := weekdayMapEN[tok]; ok {
		return phraseEdge{day: resolveWeekdayDate(wd, backward, now), rel: true}
	}
	// Month names
	if m, ok := lookupMonth(tok); ok {
		return phraseEdge{day: resolveMonthDate(m, backward, now), rel: true, month: true}
	}
	// A day date of either form the write side extracts, and an ISO date of
	// any year, which this resolver took before the dot form joined it.
	// The token is lowercased; the T and Z of an ISO time are upper case.
	if t, ok := parseDayDate(strings.ToUpper(tok)); ok {
		return phraseEdge{day: t}
	}
	if t, err := time.Parse("2006-01-02", tok); err == nil {
		return phraseEdge{day: t}
	}
	return phraseEdge{} // zero = unresolved
}

// --- Main Entry Point ---.

// dateSpaces turns the two no-break spaces into spaces before the rules read
// a question: the write side reads U+00A0 and U+202F as the space between
// the parts of a date (store.ExtractDates), and a word processor puts them
// between a preposition and a date as well (U+00A0 after "seit"); the
// rules' expressions, whose \s is ASCII, would not see them (issue #463).
var dateSpaces = strings.NewReplacer("\u00a0", " ", "\u202f", " ")

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
	query = dateSpaces.Replace(query)
	now = truncDate(now)
	tokens := ruleTokenize(query)
	recur := hasRecurrence(tokens)

	// Priority 1: day dates, ISO and dot form (most specific) — pure linear
	if r := matchDayDates(query, true); r != nil {
		r.DimensionWeights = dwLinear()
		return r
	}

	// A day date Priority 1 left to the window rules is an edge of the window
	// that stands (takesEdge); Priority 3b resolves the day otherwise.
	edge := dayDateLeftToWindow(query)
	takes := func(r *TemporalResult) bool { return takesEdge(r, edge) }

	// Priority 2: Compound range patterns — linear + weekday (range endpoints)
	if r := matchLetztenWDBis(query, now); takes(r) {
		r.DimensionWeights = dwLinearRange()
		return r
	}
	if r := matchNextThrough(query, now); takes(r) {
		r.DimensionWeights = dwLinearRange()
		return r
	}
	if r := matchVonBis(query, now); takes(r) {
		r.DimensionWeights = dwLinearRange()
		return r
	}

	// Priority 3: Seit/Bis and the open edges ab/vor/nach — linear-dominant +
	// month (spans weeks/months; effectiveDimWeights drops the month for a
	// window longer than a year and for an open one)
	if r := matchSeit(query, now); takes(r) {
		r.DimensionWeights = dwLinearMonthSeit()
		return r
	}
	if r := matchBis(query, now); takes(r) {
		r.DimensionWeights = dwLinearMonthSeit()
		return r
	}
	if r := matchOpenEdge(query, now, edge); r != nil {
		r.DimensionWeights = dwLinearMonthSeit()
		return r
	}
	// Priority 3b: a day date Priority 1 left to the window rules that no
	// window took as an edge — the day itself, as Priority 1 resolves it.
	if r := matchDayDates(query, false); r != nil {
		r.DimensionWeights = dwLinear()
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

