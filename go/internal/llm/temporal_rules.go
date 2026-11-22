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
	"unicode"
	"unicode/utf8"

	"github.com/GottZ/ctx/internal/util"
)

// --- Tokenizer ---.

// ruleTokenize splits query on whitespace and hyphens, trims punctuation, lowercases.
func ruleTokenize(query string) []string {
	return clauseTokens(query).words
}

// wordTrim is the punctuation ruleTokenize trims off both ends of a word.
const wordTrim = ".,;:!?\"'()[]{}–—/\\"

// clauseWords is a question as the tense detection reads it (issue #487): its
// words as ruleTokenize gives them and, per word, whether it starts a clause
// (the first word and a word after one that ends with a comma, semicolon,
// colon or the end of a sentence), whether it starts with an upper-case letter
// in the question as written, and its byte offset in the lowercased question
// — the string the matchers run their expressions on.
type clauseWords struct {
	words  []string
	starts []bool
	caps   []bool
	offs   []int
	// sentence: the word starts a sentence (the first word, a word after one
	// that ends with a semicolon, colon or the end of a sentence; a comma
	// starts a clause, not a sentence). upper: the word is written in capitals
	// throughout, an acronym ("ES", "CI").
	sentence []bool
	upper    []bool
}

// clauseTokens tokenizes query into clauseWords.
func clauseTokens(query string) clauseWords {
	lower := strings.ToLower(query)
	expanded := strings.ReplaceAll(lower, "-", " ")
	sameLength := len(lower) == len(query)
	var c clauseWords
	start, sentence := true, true
	for i := 0; i < len(expanded); {
		r, size := utf8.DecodeRuneInString(expanded[i:])
		if unicode.IsSpace(r) {
			i += size
			continue
		}
		j := i
		for j < len(expanded) {
			r, size := utf8.DecodeRuneInString(expanded[j:])
			if unicode.IsSpace(r) {
				break
			}
			j += size
		}
		f := expanded[i:j]
		lead := len(f) - len(strings.TrimLeft(f, wordTrim))
		w := strings.Trim(f, wordTrim)
		if w != "" {
			c.words = append(c.words, w)
			c.starts = append(c.starts, start)
			c.sentence = append(c.sentence, sentence)
			c.offs = append(c.offs, i+lead)
			upper, allUpper := false, false
			if sameLength {
				orig := query[i+lead : i+lead+len(w)]
				r, _ := utf8.DecodeRuneInString(orig)
				upper = unicode.IsUpper(r)
				allUpper = utf8.RuneCountInString(orig) > 1 && strings.ToUpper(orig) == orig && strings.ToLower(orig) != orig
			}
			c.caps = append(c.caps, upper)
			c.upper = append(c.upper, allUpper)
			start, sentence = false, false
		}
		if r, _ := utf8.DecodeLastRuneInString(f); strings.ContainsRune(",;:.!?", r) && (r != '.' || !abbreviation(w)) {
			start, sentence = true, r != ','
		}
		i = j
	}
	// A question written in capitals, or with every word capitalized, says
	// nothing by its case.
	n, m := 0, 0
	for i, up := range c.caps {
		if !c.starts[i] {
			m++
			if up {
				n++
			}
		}
	}
	if 2*n > m {
		clear(c.caps)
	}
	return c
}

// abbreviations are words that end with a dot and end no clause ("Dr.
// Müller", "z.B.", "bzw."), issue #487.
var abbreviations = toWordSet("dr", "prof", "bzw", "ca", "etc", "usw", "vgl", "nr", "st", "hr", "fr", "ggf", "evtl",
	"inkl", "zzgl", "bspw", "dipl", "ing", "mr", "mrs", "ms", "vs", "approx", "jr", "sr", "abs", "allg", "geb", "max", "min",
	"std", "sek", "mio", "mrd", "tsd", "kw", "tel", "str", "abt", "bzgl", "gem", "lt", "mind", "uvm", "usf")

// abbreviation reports whether w, a word that ended with a dot, is an
// abbreviation or an ordinal rather than the end of a clause: one of
// abbreviations, a single letter, a word with a dot inside ("z.b", "d.h",
// "01.03"), or a number of one or two digits ("am 15. März").
func abbreviation(w string) bool {
	if abbreviations[w] || utf8.RuneCountInString(w) == 1 || strings.Contains(w, ".") {
		return true
	}
	return len(w) <= 2 && strings.Trim(w, "0123456789") == ""
}

// clauseOf returns the words [s, e) of the clause that holds word i.
func (c clauseWords) clauseOf(i int) (s, e int) {
	s = i
	for s > 0 && !c.starts[s] {
		s--
	}
	e = i + 1
	for e < len(c.words) && !c.starts[e] {
		e++
	}
	return s, e
}

// wordAt returns the index of the word that holds the byte offset off of
// the lowercased question, or -1 before the first word.
func (c clauseWords) wordAt(off int) int {
	i := sort.SearchInts(c.offs, off+1) - 1
	return i
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

// germanPastVerbs are German verbs in the simple past (Präteritum); each
// counts as past wherever it stands.
var germanPastVerbs = toWordSet(
	"war", "ging", "hatte", "wollte", "musste", "wurde", "konnte", "sollte",
	"machte", "fand", "gab", "kam", "nahm", "lief", "fiel", "tat",
	"sagte", "sah", "wusste", "stand", "lag", "blieb",
	"schrieb", "las", "fing", "schlug", "traf", "begann", "brach",
	"sprach", "fuhr", "zog", "hielt", "rief", "stieg", "verlor",
	"waren",
	// The verbs of a German past question that leaned on its "Was" until the
	// word stopped counting (issue #487), and the plural of the forms above
	// it asks with — not "sollten" and "wollten", which are the subjunctive as
	// well ("Sollten wir am Freitag deployen?").
	"geschah", "geschahen", "passierte", "passierten", "ereignete", "ereigneten",
	"gingen", "hatten", "mussten", "wurden", "konnten",
	"machten", "fanden", "kamen", "nahmen", "liefen", "fielen", "taten",
	"sagten", "sahen", "wussten", "standen", "blieben", "schrieben",
	"begannen", "sprachen", "hielten",
)

// englishPastVerbs are English verbs in the simple past. A German question
// takes the English participles as its own ("Was wurde am Montag
// deployed?", "Wird am Montag deployed?"), so in a German question they count
// as nothing; "was" counts only as englishWasAt reads it (issue #487).
var englishPastVerbs = toWordSet(
	"was", "were", "had", "went", "did",
	"fixed", "deployed", "cancelled", "canceled", "happened",
	"broke", "crashed", "failed", "finished", "started", "created",
	"moved", "changed", "removed", "deleted", "updated",
	"merged", "pushed", "reviewed", "shipped", "ran", "wrote", "found", "saw", "made", "got",
)

// pastVerbSet is every verb form the tense detection reads as past, of both
// languages (yearFollower takes them as words that follow a year).
var pastVerbSet = func() map[string]bool {
	m := make(map[string]bool, len(germanPastVerbs)+len(englishPastVerbs))
	for w := range germanPastVerbs {
		m[w] = true
	}
	for w := range englishPastVerbs {
		m[w] = true
	}
	return m
}()

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

// The language of a question, as questionLanguage reads it (issue #487).
const (
	langMixed = iota // words of both languages, or of neither
	langGerman
	langEnglish
)

// germanMarkers and englishMarkers are the words questionLanguage counts:
// function words, auxiliaries and day keywords that belong to one of the two
// languages and are no word of the other. A word both languages spell alike
// ("was", "in", "an", "so", "will", "hat", "war", "man", "also") is in
// neither; the weekday names and the month names that differ between the
// languages are counted by questionLanguage itself.
var (
	germanMarkers = toWordSet(
		"der", "die", "das", "den", "dem", "des", "ein", "eine", "einen", "einem", "einer", "eines",
		"ist", "sind", "waren", "wird", "werden", "wurde", "wurden", "haben", "habe", "hast", "habt",
		"hatte", "hatten", "bist", "seid", "und", "oder", "aber", "nicht", "kein", "keine", "auch",
		"noch", "schon", "nur", "ich", "du", "er", "es", "wir", "ihr", "sie", "uns", "mir", "dir",
		"mich", "dich", "sich", "am", "im", "um", "zum", "zur", "vom", "beim", "ins", "auf", "mit",
		"nach", "von", "bei", "für", "über", "vor", "seit", "bis", "ab", "aus", "zu", "wie", "wo",
		"wer", "wann", "warum", "wieso", "weshalb", "welche", "welcher", "welches", "welchen",
		"gibt", "gab", "gilt", "galt", "steht", "kommt", "kam", "läuft", "lief", "geht", "ging",
		"muss", "soll", "kann", "darf", "heute", "gestern", "morgen", "vorgestern", "übermorgen",
		"woche", "monat", "jahr", "wochenende",
	)
	englishMarkers = toWordSet(
		"the", "a", "is", "are", "were", "been", "be", "did", "do", "does", "done", "has", "have",
		"had", "on", "at", "of", "to", "for", "from", "with", "by", "it", "its", "this", "that",
		"these", "those", "there", "what", "which", "who", "whom", "whose", "when", "where", "why",
		"how", "we", "you", "they", "he", "she", "i", "my", "our", "your", "their", "his",
		"not", "no", "any", "anyone", "anything", "everything", "everyone", "someone", "something",
		"nothing", "nobody", "would", "can", "could", "should", "must", "may", "might", "shall",
		"and", "or", "but", "today", "yesterday", "tomorrow", "week", "month", "year", "weekend",
		"last", "next", "since", "until", "after", "before",
	)
)

// questionLanguage reads the language of a question from its words
// (issue #487): German when it has German marker words, weekday or month
// names and no English ones, English the other way round, and mixed
// otherwise. A word in capitals is an acronym and "am" or "um" after a
// number or "I" English, so neither counts. DetectGerman, the detection of the translation step, does not
// decide it: it answers only "German or not", counts the umlaut of a name
// ("Was Jürgen there on Monday?" is German to it) and needs two stopwords or
// a domain term ("Was kommt am Montag?" is not German to it).
func questionLanguage(c clauseWords) int {
	de, en := 0, 0
	for i, w := range c.words {
		if c.upper[i] || (w == "am" || w == "um") && i > 0 && (c.words[i-1] == "i" || strings.Trim(c.words[i-1], "0123456789") == "") {
			continue // an acronym ("ES"), the English "3 am" and "I am"
		}
		_, wdDE := weekdayMapDE[w]
		_, wdEN := weekdayMapEN[w]
		switch {
		case germanMarkers[w] || wdDE || monthOnlyIn(w, util.MonthDE, util.MonthEN):
			de++
		case englishMarkers[w] || wdEN || monthOnlyIn(w, util.MonthEN, util.MonthDE):
			en++
		}
	}
	switch {
	case de > 0 && en == 0:
		return langGerman
	case en > 0 && de == 0:
		return langEnglish
	}
	return langMixed
}

// monthOnlyIn reports whether w is a month name of the table in and of no
// month in the table other ("mai" is German, "may" English, "april" both).
func monthOnlyIn(w string, in, other map[string]time.Month) bool {
	_, ok := in[w]
	_, both := other[w]
	return ok && !both
}

// englishWhWords are the English words a "was" behind them makes the past of
// "to be" in a question of mixed language ("What was im Februar los?").
var englishWhWords = toWordSet("what", "when", "where", "who", "why", "how", "which", "it", "there", "that", "this")

// englishSubjectAfterWas are the words an English yes/no question puts right
// behind its "was" ("Was it …", "Was there …", "Was the build …"): in a
// question of mixed language a "was" before one of them is the English past
// of "to be" (englishWasAt).
var englishSubjectAfterWas = toWordSet(
	"it", "there", "this", "that", "these", "those", "the", "a",
	"i", "he", "she", "we", "you", "they",
	"my", "our", "your", "his", "her", "its", "their",
	"anyone", "anybody", "anything", "everyone", "everybody", "everything",
	"someone", "somebody", "something", "nobody", "nothing",
	"no", "any", "every", "each",
)

// englishWasAt reports whether words[i], a "was", is the English past of "to
// be" (issue #487): in an English question it is ("Was Damien in the standup
// on Friday?", "Was test coverage green on Friday?"), in a German one it is
// the interrogative or relative pronoun ("Was ist am Montag?", "Weißt du was
// am Dienstag ist?"), and in a question of both languages only with an
// English word right before or behind it ("What was im Februar los?", "Was it
// am Montag?"); otherwise it says nothing of the tense.
func englishWasAt(words []string, lang, i int) bool {
	switch lang {
	case langEnglish:
		return true
	case langGerman:
		return false
	}
	return i > 0 && englishWhWords[words[i-1]] || i+1 < len(words) && englishSubjectAfterWas[words[i+1]]
}

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

// DetectVerbTense returns "past", "future", or "neutral" based on deterministic
// verb analysis of the whole question.
func DetectVerbTense(query string) string {
	c := clauseTokens(query)
	return c.tense(0, len(c.words), questionLanguage(c))
}

// tenseAt is the tense a date at the byte offset off of the lowercased
// question counts by (issue #487): the tense of the clause it stands in when
// that clause has a verb of its own ("Sie sagten, wir treffen uns am Montag"
// asks for the coming Monday, "Ich will wissen, was am Montag passiert ist"
// for the last one), and otherwise — the question is one clause, the date
// stands in a clause without a verb ("Am Montag, was ist da passiert?") —
// the tense of the whole question (DetectVerbTense). A clause whose Perfekt
// a later clause closes ("Was haben wir am Montag, nach dem Standup,
// besprochen?") reads that participle (hasPerfektForm). The language is the
// whole question's.
func tenseAt(query string, off int) string {
	c := clauseTokens(query)
	// The offsets are those of the lowercased question; where lowercasing
	// changes the length (a few letters beyond ASCII), the whole question.
	if i := c.wordAt(off); i >= 0 && len(strings.ToLower(query)) == len(query) {
		if s, e := c.clauseOf(i); (s > 0 || e < len(c.words)) && c.hasOwnVerb(s, e) {
			return c.tense(s, e, questionLanguage(c))
		}
	}
	return DetectVerbTense(query)
}

// tense scores the words [s, e) of c in the language lang.
func (c clauseWords) tense(s, e, lang int) string {
	words := c.words[s:e]
	joined := strings.Join(words, " ")
	pastScore, futureScore := 0, 0

	// Explicit direction markers (strong signal)
	for _, tok := range words {
		if explicitPastSet[tok] {
			pastScore += 2
		}
		if explicitFutureSet[tok] {
			futureScore += 2
		}
	}

	// Past verbs: German ones always, English ones outside a German question,
	// "was" as englishWasAt reads it (issue #487), and the German simple past
	// in the place of the finite verb behind a German "was" (wasPastVerb).
	if lang != langEnglish && c.wasPastVerb(s, e) {
		pastScore++
	}
	for i, tok := range words {
		switch {
		case germanPastVerbs[tok]:
			pastScore++
		case tok == "was":
			if englishWasAt(words, lang, i) {
				pastScore++
			}
		case englishPastVerbs[tok] && lang != langGerman:
			pastScore++
		}
	}

	// Future verbs
	for _, tok := range words {
		if futureVerbSet[tok] {
			futureScore++
		}
	}

	// The German Perfekt and the past subjunctive (hasPerfektForm).
	if c.hasPerfektForm(s, e) {
		pastScore += 2
	}

	// Future-intent phrases: "habe ich frei", "treffe ich", etc.
	for _, phrase := range futureIntentPhraseList {
		if strings.Contains(joined, phrase) {
			futureScore += 2
			break
		}
	}

	// Separable verbs: "steht...an", "fällt...aus", "findet...statt"
	// Stem must appear BEFORE prefix in token order.
	// T20: Filter false positives where the "prefix" is actually a preposition
	// followed by an article (e.g., "steht an der Stelle" — "an" + article = preposition).
	if hasSeparableVerb(words) {
		futureScore += 2
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

// wasParticles stand between a clause-opening German "was" and its finite
// verb ("Was genau fehlte am Montag?").
var wasParticles = toWordSet("genau", "denn", "noch", "sonst", "eigentlich", "alles", "so")

// wasPastVerb reports whether a clause of [s, e) opens with the German "was"
// and has a verb in the simple past in the place of its finite verb (issue
// #487): a German question asked the past with it before the word stopped
// counting as the English "was" ("Was fehlte am Montag?", "Was änderte sich
// am Montag?", "Was besprachen wir am Montag?"). The verbs of that place are
// read by their shape (germanPastShape), since the simple past of the weak
// verbs is an open class no list holds.
func (c clauseWords) wasPastVerb(s, e int) bool {
	for cs := s; cs < e; {
		_, ce := c.clauseOf(cs)
		if c.words[cs] == "was" {
			v := cs + 1
			for v < ce && wasParticles[c.words[v]] {
				v++
			}
			if v < ce && v < e && germanPastShape(c.words[v]) {
				return true
			}
		}
		cs = ce
	}
	return false
}

// strongPastForms are the simple past of the strong and mixed German verbs a
// question asks with; a verb behind "was" ending in one is in the past
// ("besprachen", "entschieden", "ergab", "verlief").
var strongPastForms = []string{
	"sprach", "sprachen", "schied", "schieden", "gab", "gaben", "kam", "kamen", "nahm", "nahmen", "fand", "fanden",
	"schloss", "schlossen", "zog", "zogen", "hielt", "hielten", "ließ", "ließen", "lief", "liefen", "fiel", "fielen",
	"ging", "gingen", "stand", "standen", "sah", "sahen", "trat", "traten", "brach", "brachen", "warf", "warfen",
	"wies", "wiesen", "rief", "riefen", "schrieb", "schrieben", "blieb", "blieben", "stieg", "stiegen", "fuhr", "fuhren",
	"trug", "trugen", "bot", "boten", "bat", "baten", "galt", "galten", "schlug", "schlugen", "traf", "trafen",
	"begann", "begannen", "gewann", "gewannen", "verlor", "wuchs", "wuchsen", "sank", "sanken", "lag", "lagen",
	"tat", "taten", "las", "lasen", "aß", "saß", "saßen", "flog", "flogen", "floss", "flossen", "schien", "schienen",
	"stieß", "stießen",
}

// presentTForms are present forms of German verbs whose stem ends in -t,
// which take the shape of the weak simple past ("Was testen wir am Montag?",
// "Was kosten die Server?"); germanPastShape reads them as present.
var presentTForms = []string{
	"warten", "warte", "halten", "halte", "starten", "starte", "kosten", "koste", "testen", "teste", "leisten",
	"leiste", "richten", "richte", "arbeiten", "arbeite", "gelten", "schalten", "schalte",
	"gestalten", "verwalten", "raten", "rate", "bitten", "bitte", "fürchten", "fürchte", "melden", "bieten", "biete",
	"treten", "trete", "posten", "poste", "hosten", "hoste", "listen", "liste", "mieten", "miete", "beten",
}

// germanPastShape reports whether w has the shape of the German simple
// past: a strong form of strongPastForms at its end, or the weak ending
// -te/-ten — after a stem in -t or -d as -ete/-eten ("testete") — but not a
// present form of a verb whose stem ends in -t (presentTForms) and not the
// ending -tet, which is the present of such a verb ("kostet").
func germanPastShape(w string) bool {
	for _, f := range strongPastForms {
		if strings.HasSuffix(w, f) {
			return true
		}
	}
	for _, f := range presentTForms {
		if strings.HasSuffix(w, f) {
			return false
		}
	}
	if utf8.RuneCountInString(w) < 4 {
		return false
	}
	for _, end := range []string{"ete", "eten"} {
		if strings.HasSuffix(w, end) {
			stem := strings.TrimSuffix(w, end)
			return strings.HasSuffix(stem, "t") || strings.HasSuffix(stem, "d")
		}
	}
	return strings.HasSuffix(w, "te") || strings.HasSuffix(w, "ten")
}

// hasSeparableVerb reports a separable verb of separableVerbPairs with its
// prefix behind its stem (T20: not a prefix that is a preposition before an
// article).
func hasSeparableVerb(tokens []string) bool {
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
			if prefixIdx+1 < len(tokens) && separableArticleSet[tokens[prefixIdx+1]] {
				continue
			}
			return true
		}
	}
	return false
}

// subjectWords and clauseOpeners make a clause one with a verb of its own
// (hasOwnVerb): a subject pronoun anywhere in it, or an interrogative or a
// subordinating conjunction as its first word.
var (
	subjectWords  = toWordSet("ich", "du", "er", "sie", "es", "wir", "ihr", "man", "i", "you", "he", "she", "it", "we", "they")
	clauseOpeners = toWordSet(
		"was", "wer", "wie", "wann", "wo", "warum", "wieso", "weshalb", "welche", "welcher", "welches",
		"dass", "ob", "weil", "wenn", "als", "bevor", "nachdem", "während", "obwohl", "damit", "sobald", "wie",
		"what", "who", "how", "when", "where", "why", "which",
		"that", "if", "because", "whether",
	)
	// copulaWords are the forms of "sein", "werden", "to be" and the modals
	// that need no other verb to make a clause.
	copulaWords = toWordSet(
		"bin", "bist", "ist", "sind", "seid", "war", "waren", "wird", "werden", "wurde", "wurden",
		"muss", "soll", "kann", "darf", "mag", "will", "is", "are", "was", "were", "be", "been",
		"would", "can", "could", "should", "must", "may", "might", "do", "does", "did",
	)
)

// hasOwnVerb reports whether the words [s, e) of c make a clause with a verb
// of its own (tenseAt): one opened by an interrogative or a subordinating
// conjunction, one with a subject pronoun, a verb form the tense detection
// knows, a German participle at its end, or "zu" before an infinitive ("am
// Montag zu deployen"). A clause with none of them ("Am Montag,", "On
// Monday,") takes the tense of the question.
func (c clauseWords) hasOwnVerb(s, e int) bool {
	if clauseOpeners[c.words[s]] {
		return true
	}
	for i := s; i < e; i++ {
		w := c.words[i]
		if subjectWords[w] || copulaWords[w] || pastVerbSet[w] || futureVerbSet[w] || perfektAux[w] ||
			w == "zu" && i+1 < e {
			return true
		}
	}
	return c.participleAt(s, e-1)
}

// perfektAux are the finite forms of "haben" and "sein", and the past
// subjunctive of both ("hätte gemacht", "wäre gegangen"), that make the
// Perfekt with a participle (hasPerfektForm).
var perfektAux = toWordSet(
	"habe", "hab", "hast", "hat", "haben", "habt", "hatte", "hatten", "hattest", "hattet",
	"hätte", "hätten", "hättest", "hättet",
	"bin", "bist", "ist", "sind", "seid", "war", "waren",
	"wäre", "wären", "wärst", "wärt",
)

// seinAux are the forms of "sein" in perfektAux: behind them a participle is
// the Perfekt only of a verb that takes "sein" (seinParticiple); "Ist das
// Büro am Samstag geöffnet?" is a state, the Zustandspassiv, which the form
// cannot tell from the Perfekt, and a state is no past (issue #487).
var seinAux = toWordSet("bin", "bist", "ist", "sind", "seid", "war", "waren", "wäre", "wären", "wärst", "wärt")

// subjunctiveAux are the forms of the past subjunctive of "haben" in
// perfektAux; with a modal infinitive at the end of the clause they ask for
// the past as well ("Was hätten wir am Montag tun sollen?").
var subjunctiveAux = toWordSet("hätte", "hätten", "hättest", "hättet")

// modalInfinitives end a clause of the past subjunctive with a double
// infinitive ("hätten tun sollen").
var modalInfinitives = toWordSet("sollen", "müssen", "können", "dürfen", "wollen", "lassen", "mögen")

// seinParticipleEndings and seinParticiples are the participles of the verbs
// that take "sein" in the Perfekt — verbs of motion and of a change of state,
// the verbs a question about what went wrong asks with ("ist passiert", "ist
// abgestürzt", "ist kaputtgegangen", "sind aufgetreten") — by the ending of
// the participle where the ending names the verb, and as words where it does
// not. Behind a form of "sein" every other participle may be a state (issue
// #487).
var (
	seinParticipleEndings = []string{
		"gangen", "kommen", "laufen", "fallen", "treten", "stürzt", "scheitert", "schehen",
		"schienen", "schwunden", "blieben", "stiegen", "wachsen", "sunken", "storben", "flogen",
		"sprungen", "lungen", "glückt", "platzt", "taucht", "crasht",
	}
	seinParticiples = toWordSet(
		"passiert", "eskaliert", "explodiert", "kollabiert", "gewesen", "geworden", "gefahren",
		"gereist", "verreist", "abgereist", "gelandet", "gestartet", "gefolgt", "begegnet",
		"entstanden", "aufgestanden", "eingeschlafen", "aufgewacht", "umgezogen", "fehlgeschlagen",
		"gekippt", "abgebrochen", "zusammengebrochen", "eingebrochen", "ausgebrochen", "worden", "abgeschmiert",
		"gewechselt",
	)
)

// seinParticiple reports whether w is the participle of a verb that takes
// "sein" in the Perfekt.
func seinParticiple(w string) bool {
	if seinParticiples[w] {
		return true
	}
	for _, end := range seinParticipleEndings {
		if strings.HasSuffix(w, end) {
			return true
		}
	}
	return false
}

// perfektParticiplePat is the participle with "ge" the rules read since
// before issue #487, and broadParticiplePat the forms the German Perfekt puts
// at the end of its clause as well (issue #487): a separable prefix before
// "ge" ("festgelegt", "zurückgerollt", "hängengeblieben"), an inseparable
// prefix instead of it ("übersehen", "vergessen", "bemerkt", "entfernt",
// "zerstört"), both ("vorbereitet"), and the letters beyond ASCII
// ("geändert", "gelöscht").
var (
	perfektParticiplePat = regexp.MustCompile(`^(ab|an|auf|aus|bei|ein|mit|nach|um|vor|weg|zu|über|unter)?ge\w+(t|en)$`)
	broadParticiplePat   = regexp.MustCompile(`^(?:` + separablePrefixes + `)?(?:ge|be|emp|ent|er|miss|ver|zer|über|unter|hinter|wider)\pL{2,}(?:t|en)$`)
)

// separablePrefixes are the separable prefixes of broadParticiplePat.
const separablePrefixes = `ab|an|auf|aus|bei|dar|durch|ein|fehl|fest|fertig|fort|frei|heim|her|herab|heran|herauf|heraus|herein|herum|herunter|hervor|hin|hinab|hinauf|hinaus|hinein|hoch|hängen|kaputt|kennen|los|mit|nach|nieder|rauf|raus|rein|rüber|rum|runter|schief|statt|teil|um|vor|voran|voraus|vorbei|weg|weiter|wieder|zu|zurecht|zurück|zusammen`

// participleExceptions are words of the shape of broadParticiplePat that a
// German question puts at the end of its clause as an adjective or adverb,
// and "geöffnet", which with "haben" asks for opening hours ("Hat das Büro am
// Samstag geöffnet?").
var participleExceptions = toWordSet("bereit", "bestimmt", "beliebt", "berühmt", "gesamt", "verrückt", "verwandt", "geöffnet", "gestern", "genau")

var knownParticiples = toWordSet(
	"gewesen", "gemacht", "gegangen", "gehabt", "gewollt", "gemusst",
	"geworden", "gekonnt", "gesollt", "gesagt", "gesehen", "gefunden",
	"gegeben", "gekommen", "genommen", "gelaufen", "gefallen", "getan",
	"geschrieben", "gelesen", "gefangen", "geschlagen", "getroffen",
	"begonnen", "gebrochen", "gesprochen", "gefahren", "gezogen",
	"gehalten", "gerufen", "gestiegen", "verloren",
	"deployed", "gefixt", "geplant", "gestartet", "besprochen", "gearbeitet",
	"worden",
	// Participles a German past question asks with that the "ge" pattern does
	// not read — without "ge", or with an umlaut (issue #487).
	"verändert", "erstellt", "erledigt", "behoben", "entschieden", "beschlossen",
	"veröffentlicht", "erschienen", "entstanden", "erklärt",
	"geändert", "gelöscht", "geprüft", "geführt", "gehört", "gewählt",
)

// articleBefore are the words before a noun that never stand before a
// participle: a word behind one of them at the end of a clause is a noun
// ("Haben wir am Montag einen Bericht?").
var articleBefore = toWordSet(
	"der", "die", "das", "den", "dem", "des", "ein", "eine", "einen", "einem", "einer", "eines",
	"kein", "keine", "keinen", "keinem", "keiner", "am", "im", "zum", "zur", "vom", "beim", "ins",
	"mein", "meine", "meinen", "unser", "unsere", "unseren", "dein", "deine", "sein", "seine", "ihr", "ihre",
)

// nounEndings are endings of German nouns that broadParticiplePat would read
// as a participle ("Bereitschaft", "Entwicklungen", "Versionen",
// "Ergebnissen").
var nounEndings = []string{"schaft", "schaften", "heit", "heiten", "keit", "keiten", "ungen", "ionen", "täten", "nissen"}

// participleUngen are the endings of participles on -ungen, which nouns do
// not take ("eingesprungen", "erzwungen", "misslungen").
var participleUngen = []string{"sprungen", "zwungen", "gelungen", "misslungen", "gedrungen", "gesungen", "besungen"}

// nounEnding reports whether w ends with one of nounEndings and is none of
// the participles of participleUngen.
func nounEnding(w string) bool {
	for _, end := range participleUngen {
		if strings.HasSuffix(w, end) {
			return false
		}
	}
	for _, end := range nounEndings {
		if strings.HasSuffix(w, end) {
			return true
		}
	}
	return false
}

// participleAt reports whether word i of c, inside the clause [s, …), is a
// participle at a place the German Perfekt puts one: of the shape of
// broadParticiplePat or a known one, not an exception, not behind an article,
// without the ending of a noun and — written with an upper-case letter in a
// clause it does not open — not a noun.
func (c clauseWords) participleAt(s, i int) bool {
	if i < s || i < 0 {
		return false
	}
	w := c.words[i]
	if participleExceptions[w] || i > s && articleBefore[c.words[i-1]] || c.caps[i] && i > s || nounEnding(w) {
		return false
	}
	return knownParticiples[w] || broadParticiplePat.MatchString(w) || ierenParticiple(w) || englishParticiple(w)
}

// englishParticiple reports an English participle a German Perfekt takes as
// its own ("Was haben wir am Montag released?", "gemerged"), issue #487.
func englishParticiple(w string) bool {
	return utf8.RuneCountInString(w) >= 5 && strings.HasSuffix(w, "ed") && !strings.HasSuffix(w, "eed")
}

// hasPerfektForm reports the German Perfekt or the past subjunctive in the
// words [s, e) of c, clause by clause (issue #487):
//   - a main clause puts the auxiliary (perfektAux) in front and the
//     participle at its end ("Was haben wir am Montag im Meeting
//     besprochen?"), and as before an auxiliary with a participle of the old
//     patterns in the five words behind it counts too;
//   - a subordinate clause puts the auxiliary last, right behind the
//     participle ("…, was wir am Montag festgelegt haben");
//   - behind a form of "sein" the participle has to be one of a verb that
//     takes "sein" (seinParticiple), else the clause may name a state;
//   - the past subjunctive of "haben" with a modal infinitive at the end
//     ("Was hätten wir am Montag tun sollen?").
func (c clauseWords) hasPerfektForm(s, e int) bool {
	var closing []int
	for cs := s; cs < e; {
		_, ce := c.clauseOf(cs)
		if ce > e {
			ce = e
		}
		if c.clausePerfekt(cs, ce, &closing) {
			return true
		}
		cs = ce
	}
	return false
}

// clausePerfekt is hasPerfektForm for the one clause [s, e); an auxiliary
// whose clause does not end with its participle may find it at the end of a
// later clause of the sentence (closingParticiples, a table the first
// clause that needs it fills).
func (c clauseWords) clausePerfekt(s, e int, closing *[]int) bool {
	last := e - 1
	if last > s && perfektAux[c.words[last]] && c.participleAt(s, last-1) &&
		(!seinAux[c.words[last]] || seinParticiple(c.words[last-1])) {
		return true
	}
	for i := s; i < last; i++ {
		aux := c.words[i]
		if !perfektAux[aux] {
			continue
		}
		if subjunctiveAux[aux] && modalInfinitives[c.words[last]] && last > i+1 {
			return true
		}
		if c.closedBy(aux, s, last) {
			return true
		}
		if *closing == nil {
			*closing = c.closingParticiples()
		}
		if cs, ok := closingAt(*closing, e); ok && c.closedBy(aux, cs, c.clauseEnd(cs)-1) {
			return true
		}
		if seinAux[aux] {
			continue
		}
		for j := i + 1; j < e && j <= i+5; j++ {
			if w := c.words[j]; knownParticiples[w] && !participleExceptions[w] || perfektParticiplePat.MatchString(w) || ierenParticiple(w) {
				return true
			}
		}
	}
	return false
}

// closedBy reports whether word last of the clause starting at s is a
// participle that closes the bracket of aux: behind a form of "sein" one of
// a verb that takes "sein".
func (c clauseWords) closedBy(aux string, s, last int) bool {
	return c.participleAt(s, last) && (!seinAux[aux] || seinParticiple(c.words[last]))
}

// clauseEnd returns the end of the clause that starts at s.
func (c clauseWords) clauseEnd(s int) int {
	_, e := c.clauseOf(s)
	return e
}

// closingParticiples returns, for every clause start e of c, the closing
// participle of the clause that ends at e: the start of the clause behind it
// whose last word may close the bracket an auxiliary opened before e, within
// the same sentence (issue #487), the first following clause
// without an auxiliary that ends with a participle ("…, nach dem Standup,
// besprochen?", "…, glaube ich, was beschlossen?"). Clauses without either
// are passed, and so are clauses with an auxiliary that a subordinating word
// or an interrogative opens ("…, bevor du gegangen bist, deployed?"); any
// other clause with an auxiliary of its own ends the search ("…, und was ist
// geplant?"). The entry of a clause start is the start found, -1 for none.
// The table is filled from the last clause to the first, each clause read
// once (issue #506): searched clause by clause from every auxiliary, the
// search cost time quadratic in the number of clauses.
func (c clauseWords) closingParticiples() []int {
	res := make([]int, len(c.words)+1)
	for i := range res {
		res[i] = -1
	}
	for ce := len(c.words); ce > 0; {
		cs, _ := c.clauseOf(ce - 1)
		scanned(ce - cs)
		aux := false
		for k := cs; k < ce; k++ {
			aux = aux || perfektAux[c.words[k]]
		}
		switch {
		case c.sentence[cs], aux && !clauseOpeners[c.words[cs]]:
			res[cs] = -1
		case !aux && c.participleAt(cs, ce-1):
			res[cs] = cs
		default:
			res[cs] = res[ce]
		}
		ce = cs
	}
	return res
}

// closingAt reads the closing participle of the clause that ends at e from
// the table of closingParticiples.
func closingAt(table []int, e int) (int, bool) {
	if e >= len(table) || table[e] < 0 {
		return 0, false
	}
	return table[e], true
}

// ierenParticiple reports whether tok is the participle of a German verb on
// -ieren, which takes no "ge" ("passiert", "migriert"): behind an auxiliary
// it makes the Perfekt ("Wer hat die Datenbank migriert?", issue #487).
// Without one the same form is the present tense ("Was passiert am
// Montag?"), which hasPerfektForm never reads.
func ierenParticiple(tok string) bool {
	return utf8.RuneCountInString(tok) > len("iert") && strings.HasSuffix(tok, "iert")
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
	tense := tenseAt(query, matches[0].start)
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
//
// "zwischen X und Y" and "between X and Y" are a range form when X and Y are
// both years (years): with two day dates matchDayDates spans them, and the
// form with other edges stays as it was — "zwischen Montag und Freitag" is
// the two days (issue #487).
var rangeForms = []struct {
	re    *regexp.Regexp
	ref   func(m []string) string
	exact bool
	years bool
}{
	{reVonBis, func(m []string) string { return "von " + m[1] + " bis " + m[2] }, false, false},
	{reFromTo, func(m []string) string { return m[0] }, true, false},
	{reAbBis, func(m []string) string { return "ab " + m[1] + " bis " + m[2] }, true, false},
	{reSeitBis, func(m []string) string { return m[0] }, false, false},
	{reBetweenYears, func(m []string) string { return m[0] }, true, true},
}

// reBetweenYears matches "zwischen X und Y" and "between X and Y" with one
// word as X, and as Y one word with the word after it (yearEdge reads it):
// the form of rangeForms for two years.
var reBetweenYears = regexp.MustCompile(`(?i)\b(?:zwischen|between)\s+(\S+)\s+(?:und|and)\s+((?:\S+\s+)?\S+)`)

func matchVonBis(query string, now time.Time) *TemporalResult {
	// "von Montag bis Mittwoch", "von letztem Montag bis nächsten Freitag", "von gestern bis übermorgen"
	lower := strings.ToLower(query)
	for _, f := range rangeForms {
		if idx := f.re.FindStringSubmatchIndex(lower); idx != nil {
			m := submatches(lower, idx)
			if r := matchRangeForm(query, m, idx[0], f.ref(m), f.exact, f.years, now); r != nil {
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

// submatches returns the groups of a match in s as FindStringSubmatch does,
// from the indexes FindStringSubmatchIndex gives.
func submatches(s string, idx []int) []string {
	m := make([]string, len(idx)/2)
	for i := range m {
		if idx[2*i] >= 0 {
			m[i] = s[idx[2*i]:idx[2*i+1]]
		}
	}
	return m
}

// matchRangeForm derives the window of a range form whose start phrase is
// m[1] and whose end phrase is m[2], the match standing at the byte offset off
// of the lowercased question; with years, only of two years.
func matchRangeForm(query string, m []string, off int, ref string, exact, years bool, now time.Time) *TemporalResult {
	tense := tenseAt(query, off)
	backward := tense == "past"
	startEdge := resolvePhraseEdge(m[1], backward, now)
	if w := phraseWords(m[1]); len(w) > 0 && nowWords[w[0]] {
		startEdge = phraseEdge{day: truncDate(now)}
	}
	endEdge := resolvePhraseEdge(m[2], backward, now)
	if !rangeEdgesStand(query, m, off, startEdge, endEdge, exact, years) {
		return nil
	}
	startEdge = startInEndYear(m, startEdge, endEdge, now)
	// A year at the end is ordered by its last day, and a year before the
	// start swaps from its first (issue #487).
	endDay := endEdge.day
	if endEdge.year {
		endDay = endEdge.last()
	}
	start, end, swapped := orderWindow(startEdge.day, endDay, startEdge.rel, endEdge.rel, backward,
		func(to time.Time) time.Time { return resolvePhrase(m[1], true, to) },
		func(from time.Time) time.Time { return resolvePhrase(m[2], false, from) })
	if swapped && endEdge.year {
		start = endEdge.day
	}
	// A month at the end includes the month: the window ends on its last day
	// ("von März 2024 bis April 2024" ends on 2024-04-30, issue #463), and a
	// year the year ("von 2024 bis 2026" ends on 2026-12-31, issue #487).
	first, last := startEdge, endEdge
	if swapped {
		first, last = endEdge, startEdge
	}
	switch {
	case last.year:
		end = lastOfYear(end)
	case last.month:
		end = lastOfMonth(end)
	}
	return yearWeighted(rangeResult(ref, fmtDate(start), fmtDate(end), "range"), first, last)
}

// rangeEdgesStand reports whether the edges a range form's phrases resolved
// to make a window (matchRangeForm).
func rangeEdgesStand(query string, m []string, off int, startEdge, endEdge phraseEdge, exact, years bool) bool {
	if startEdge.day.IsZero() || endEdge.day.IsZero() || exact && startEdge.fuzzy || years && (!startEdge.year || !endEdge.year) {
		return false
	}
	if english := strings.HasPrefix(m[0], "from") || strings.HasPrefix(m[0], "since"); english &&
		(bareMonthWord(m[1], startEdge) || bareMonthWord(m[2], endEdge)) {
		return false
	}
	// A weekday or month after a day date could count from today ("bis
	// Freitag") or from the date ("vom 01.03.2026 bis Ende März"); the rules
	// do not guess, the date stays the day it names.
	if endEdge.rel && len(findDayDates(strings.ToUpper(m[1]))) > 0 {
		return false
	}
	if !startEdge.year && !endEdge.year {
		return true
	}
	lower := strings.ToLower(query)
	return yearEdgeStands(lower, clauseTokens(lower), off)
}

// startInEndYear returns the start edge of a range form whose end is a year
// (issue #487): a month without its year at the start lies in that year,
// asked in any month ("von Juni bis 2026" starts on 2026-06-01, also in
// January 2026) — after "seit" only a month that has begun, since "seit"
// names no time to come. Any other start is returned as it is.
func startInEndYear(m []string, startEdge, endEdge phraseEdge, now time.Time) phraseEdge {
	if !endEdge.year || !startEdge.rel || !monthPhrase(m[1], startEdge) {
		return startEdge
	}
	since := strings.HasPrefix(m[0], "seit") || strings.HasPrefix(m[0], "since")
	if s := resolvePhrase(m[1], false, endEdge.day); !s.IsZero() && (!since || !s.After(truncDate(now))) {
		startEdge.day, startEdge.rel = s, false
	}
	return startEdge
}

// monthPhrase reports whether phrase, resolved to e, names a month without
// its year: a bare month name or a position in one ("Anfang März").
func monthPhrase(phrase string, e phraseEdge) bool {
	w := phraseWords(phrase)
	if e.month || len(w) < 2 {
		return e.month
	}
	_, pos := monthPositionModifiers[w[0]]
	_, month := lookupMonth(w[1])
	return pos && month
}

// yearWeighted gives a window with an edge that is a year (issue #487) — the
// edge of "seit", "bis" and the open edges, either edge of a range form —
// linear weights alone: the year names no month, week or weekday, and the
// window's edge on the 1st of January or the 31st of December is a phase the
// question does not name, which a cyclic dimension would reward.
// NormalizeTemporalRules keeps weights a matcher set (withWeights). Other
// windows pass as they are.
func yearWeighted(r *TemporalResult, edges ...phraseEdge) *TemporalResult {
	for _, e := range edges {
		if e.year {
			r.DimensionWeights = dwLinear()
		}
	}
	return r
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
	lower := strings.ToLower(query)
	idx := reThrough.FindStringSubmatchIndex(lower)
	if idx == nil {
		return nil
	}
	m = submatches(lower, idx)
	wd1, ok1 := weekdayMapEN[m[1]]
	wd2, ok2 := weekdayMapEN[m[2]]
	if !ok1 || !ok2 {
		return nil
	}
	tense := tenseAt(query, idx[0])
	backward := tense == "past"
	d1 := resolveWeekdayDate(wd1, backward, now)
	d2 := resolveWeekdayDate(wd2, backward, now)
	d1, d2, _ = orderWindow(d1, d2, true, true, backward,
		func(to time.Time) time.Time { return resolveWeekdayDate(wd1, true, to) },
		func(from time.Time) time.Time { return resolveWeekdayDate(wd2, false, from) })
	return rangeResult(m[1]+" through "+m[2], fmtDate(d1), fmtDate(d2), "range")
}

func matchSeit(query string, now time.Time) *TemporalResult {
	lower := strings.ToLower(query)
	idx := reSeit.FindStringSubmatchIndex(lower)
	if idx == nil {
		return nil
	}
	m := submatches(lower, idx)
	e := resolvePhraseEdge(m[2], true, now)
	start := e.day
	// A start after today has no time since it yet ("seit morgen").
	if start.IsZero() || start.After(truncDate(now)) || m[1] == "since" && bareMonthWord(m[2], e) ||
		e.year && !yearEdgeStands(lower, clauseTokens(lower), idx[0]) {
		return nil
	}
	return yearWeighted(rangeResult(m[1]+" "+m[2], fmtDate(start), fmtDate(truncDate(now)), "range"), e)
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
	idx := reBis.FindStringSubmatchIndex(lower)
	if idx == nil {
		return nil
	}
	m := submatches(lower, idx)
	e := resolvePhraseEdge(m[2], false, now)
	if e.day.IsZero() || m[1] == "until" && bareMonthWord(m[2], e) || e.year && !yearEdgeStands(lower, clauseTokens(lower), idx[0]) {
		return nil
	}
	// An end before today ("bis März 2015", "bis gestern") is the time up to
	// it, open towards the past: a window from today would start after it
	// ends (issue #463). A later end is a deadline, the window from today. A
	// month includes the month: the window ends on its last day, and a month
	// that began before today is the time up to its end.
	if e.day.Before(truncDate(now)) {
		return yearWeighted(&TemporalResult{Dates: []TemporalDate{{Ref: m[1] + " " + m[2], Date: fmtDate(e.last()), Dir: DirUntil}}}, e)
	}
	return yearWeighted(rangeResult(m[1]+" "+m[2], fmtDate(truncDate(now)), fmtDate(e.last()), "range"), e)
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
	var proof *yearProof // the year check of the question's candidates, read once
	stands := func(off int) bool {
		if proof == nil {
			proof = newYearProof(lower, clauseTokens(lower))
		}
		return proof.stands(off)
	}
	for _, m := range reOpenEdge.FindAllStringSubmatchIndex(lower, -1) {
		word, phrase := lower[m[2]:m[3]], lower[m[4]:m[5]]
		before := wordBefore(lower, m[0])
		if lookBehind(reTimeUnitBefore, before) || word != "ab" && lookBehind(reNearBefore, before) {
			continue
		}
		// A year first is an edge here only as the year (yearEdge, issue
		// #487): "2024 Oktober" has no use, and "after 2025 may be deleted"
		// names no month but the year.
		yearFirst := false
		if w := phraseWords(phrase); len(w) > 0 {
			_, yearFirst = isYear(w[0])
		}
		e := resolvePhraseEdge(phrase, false, now)
		if e.day.IsZero() || e.fuzzy || e.rel || yearFirst && !e.year || edge != "" && fmtDate(e.day) != edge ||
			e.year && !stands(m[0]) {
			continue
		}
		ref := word + " " + phrase
		switch word {
		case "ab":
			return yearWeighted(&TemporalResult{Dates: []TemporalDate{{Ref: ref, Date: fmtDate(e.day), Dir: DirFrom}}}, e)
		case "nach", "after":
			return yearWeighted(&TemporalResult{Dates: []TemporalDate{{Ref: ref, Date: fmtDate(e.last().AddDate(0, 0, 1)), Dir: DirFrom}}}, e)
		default: // vor, before
			// A day to come is a deadline, the window from today to the day
			// before it, as "bis" with a later end is one; a day that has
			// begun is the time up to the day before it.
			if today := truncDate(now); e.day.After(today) {
				return yearWeighted(rangeResult(ref, fmtDate(today), fmtDate(e.day.AddDate(0, 0, -1)), "range"), e)
			}
			return yearWeighted(&TemporalResult{Dates: []TemporalDate{{Ref: ref, Date: fmtDate(e.day.AddDate(0, 0, -1)), Dir: DirUntil}}}, e)
		}
	}
	return nil
}

// asciiSpace are the runes \s matches in the rules' expressions (RE2's
// \s: tab, line feed, form feed, carriage return and space).
const asciiSpace = "\t\n\f\r "

// wordBefore returns the end of lower before the byte offset at that an
// expression anchored at the end ("…\s+$") can match: the whitespace right
// before at and the run of other runes before it, the last word with what is
// glued to it (issue #506). Matching reTimeUnitBefore or reNearBefore on it
// answers as on lower[:at] — the word they look for has no space in it and
// stands right before the whitespace, and the start of the returned text is a
// word boundary as the space before it is — but reads only that word, not the
// whole text before every candidate: over the whole text, the check cost
// time quadratic in the length of a question with many candidates ("nach
// Hause" repeated to 10 KB took two seconds).
func wordBefore(lower string, at int) string {
	i := at
	for i > 0 && strings.IndexByte(asciiSpace, lower[i-1]) >= 0 {
		i--
	}
	if i == at {
		return ""
	}
	j := i
	for j > 0 && strings.IndexByte(asciiSpace, lower[j-1]) < 0 {
		j--
	}
	return lower[j:at]
}

// ruleScanHook is the test seam of the rules' cost (issue #506): nil in
// production; a test that does not run in parallel sets it to count the
// bytes, words and clauses the per-candidate checks read
// (TestRules_CostIsLinearInTheQuestion), a measure no load on the host
// changes. The runtimeIndexDDLHook precedent.
var ruleScanHook func(n int)

// scanned reports n units of work to ruleScanHook.
func scanned(n int) {
	if ruleScanHook != nil {
		ruleScanHook(n)
	}
}

// lookBehind matches re, an expression anchored at the end, on the text
// before a candidate (wordBefore).
func lookBehind(re *regexp.Regexp, before string) bool {
	scanned(len(before))
	return before != "" && re.MatchString(before)
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
	loc := reWochenende.FindStringIndex(strings.ToLower(query))
	if loc == nil {
		return nil
	}
	tense := tenseAt(query, loc[0])

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
		tense := tenseAt(query, clauseTokens(query).offs[weekdays[0].idx])
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

	tense := tenseAt(query, clauseTokens(query).offs[wdIdx])
	// "vom Montag" names where something comes from — the Monday that has
	// been ("das Protokoll vom Montag", "der Test vom Freitag"), unless a
	// word of the question points forward (issue #487).
	backward := tense == "past" || wdIdx > 0 && tokens[wdIdx-1] == "vom" && tense != "future"
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
	// year: the phrase names a whole year (a bare year, yearEdge); day is its
	// 1st of January, and as a window's end it stands for its 31st of
	// December (last), as a month does for its last day (issue #487).
	year bool
}

// last is the last day of what the edge names: its day, for a month the
// month's last day and for a year the year's.
func (e phraseEdge) last() time.Time {
	switch {
	case e.year:
		return lastOfYear(e.day)
	case e.month:
		return lastOfMonth(e.day)
	}
	return e.day
}

// lastOfYear returns the 31st of December of t's year.
func lastOfYear(t time.Time) time.Time {
	return time.Date(t.Year(), time.December, 31, 0, 0, 0, 0, t.Location())
}

// The years a bare year may name as a window's edge (yearEdge): those a
// content date can carry, the write side's window (store.ExtractDates,
// minDateYear to maxDateYear; TestYearEdges_TakeTheYearsAContentDateCarries
// holds the two together), and of the years to come no more than
// maxYearsAhead after the current one (issue #487): a window edged by another
// year would reward every dated block or none, and the four-digit numbers of
// this domain that are no year — 2048 tokens or bytes, the ports of the
// 2000s, the times of day of 20:00 and later written without a colon — lie
// after the current year. The numbers below 2020 are far more often counts
// ("nach 2000 Blöcken") than years. The day dates of a question read
// 2000-2099 (reISODayDate): their form says they are dates, a bare number
// does not.
const (
	minYearEdge   = 2020
	maxYearEdge   = 2099
	maxYearsAhead = 2
)

// yearFollowers are the words that may follow a bare year as a window's edge
// without making it a count: words of their own that never are the counted
// noun — conjunctions, prepositions, articles, pronouns, auxiliaries and
// modals, a few adverbs and the verbs a question about a time asks with —
// beside the verbs and participles the tense detection knows (yearFollower).
// A noun after the number makes it a count ("seit 2024 Zeilen", "bis 2048
// Tokens"); since the nouns cannot be listed, the words that may follow are,
// and a year before any other word is no edge (the LLM fallback reads it).
// Behind a count "in" and "im" name the place it is counted in ("nach 2024
// in der Queue", "ab 2025 im Kontext"): the place is one of countWords.
var yearFollowers = toWordSet(
	// German
	"und", "oder", "bzw", "sowie", "aber", "bis", "im", "in", "am", "an", "auf", "aus", "bei", "mit",
	"nach", "von", "vom", "zu", "zum", "zur", "für", "über", "unter", "um", "gegen", "ohne", "seit",
	"ab", "vor", "durch", "der", "die", "das", "den", "dem", "des", "ein", "eine", "einen", "einem",
	"einer", "eines", "es", "ich", "wir", "man", "er", "sie", "du", "ihr", "noch", "schon", "auch",
	"nur", "immer", "nie", "bereits", "wieder", "so", "da", "dann", "als", "wie", "wo", "was", "wer",
	"nicht", "kein", "keine", "hier", "jetzt", "heute", "ist", "sind", "wird", "hat", "haben", "kann",
	"können", "muss", "müssen", "darf", "dürfen", "will", "wollen", "gibt", "gilt", "galt", "gelten",
	"kommt", "kommen", "geht", "gehen", "läuft", "laufen", "steht", "stehen", "bleibt", "fehlt",
	"fehlen", "passiert", "existiert",
	// English
	"and", "or", "but", "to", "through", "till", "on", "at", "for", "from", "of", "by", "with",
	"about", "into", "over", "under", "during", "onwards", "onward", "the", "a", "it", "i", "we",
	"they", "you", "he", "she", "there", "this", "that", "these", "those", "is", "are", "be", "been",
	"has", "have", "will", "would", "can", "could", "should", "must", "may", "might", "shall", "do",
	"does", "not", "ever", "still", "already", "only", "also", "again", "yet", "then", "when", "where",
	"what", "which", "who", "how", "why", "here", "now",
)

// yearFollower reports whether w may follow a bare year at a window's edge
// (yearFollowers). The "ge" pattern of the participles is no follower: German
// nouns take its shape as well ("nach 2048 Generationen", "nach 2025
// Angeboten").
func yearFollower(w string) bool {
	return yearFollowers[w] || pastVerbSet[w] || futureVerbSet[w] || perfektAux[w] || knownParticiples[w] ||
		ierenParticiple(w)
}

// yearConjunctions and quantityWords: a conjunction behind a number with a
// word of quantity behind it makes the number a count ("ab 2024 und mehr",
// "after 2025 or more requests"), issue #487.
// rangeWords before a second number make the year a range's start, which
// the range forms decide alone ("ab 2026 bis 2028 Dimensionen" is no window
// from 2026, issue #487).
var (
	rangeWords       = toWordSet("bis", "und", "to", "and", "until", "through", "till")
	yearConjunctions = toWordSet("und", "oder", "bzw", "and", "or")
	quantityWords    = toWordSet("mehr", "weniger", "darüber", "darunter", "drüber", "drunter", "aufwärts", "abwärts", "more", "less", "fewer", "above", "below", "higher", "lower", "upward", "upwards", "downward", "downwards")
)

// clauseEnd are the runes after which a year ends its clause: a count's noun
// would follow it within the clause ("since 2024, lines have grown").
const clauseEnd = ",.;:!?)"

// yearEdge resolves a phrase whose words (phraseWords) start with a bare
// year in the window of yearInWindow to that year as a window's edge (issue
// #487): the year alone, before the end of its clause, or before a word that
// is no counted noun (yearFollower) and is no conjunction before a word of
// quantity. A month name behind the year makes the phrase the month ("2024,
// Oktober", resolvePhraseEdge). "2024-03", "2024,5", "#2024" and "2024ms"
// start with no year. What a year needs beyond its phrase — no word of the
// question that makes it a count, a position or a name — yearEdgeStands
// decides.
func yearEdge(phrase string, words []string, now time.Time) (phraseEdge, bool) {
	if len(words[0]) != 4 {
		return phraseEdge{}, false
	}
	y, err := strconv.Atoi(words[0])
	if err != nil || !yearInWindow(y, now) {
		return phraseEdge{}, false
	}
	if len(words) > 1 {
		if _, month := lookupMonth(words[1]); month && !monthWords[words[1]] {
			return phraseEdge{}, false
		}
		if !endsClause(phrase) && (!yearFollower(words[1]) ||
			yearConjunctions[words[1]] && len(words) > 2 && quantityWords[words[2]] ||
			rangeWords[words[1]] && len(words) > 2 && strings.Trim(words[2], "0123456789") == "") {
			return phraseEdge{}, false
		}
	}
	return phraseEdge{day: time.Date(y, time.January, 1, 0, 0, 0, 0, now.Location()), year: true}, true
}

// yearInWindow reports whether y is a year a bare number may name as a
// window's edge at now: minYearEdge to maxYearEdge, and no later than
// maxYearsAhead after now's year.
func yearInWindow(y int, now time.Time) bool {
	return y >= minYearEdge && y <= maxYearEdge && y <= now.Year()+maxYearsAhead
}

// endsClause reports whether the first word of phrase (phraseWords), as it
// stands in the phrase, ends with one of the runes of clauseEnd.
func endsClause(phrase string) bool {
	for _, f := range strings.Fields(phrase) {
		if strings.Trim(f, phrasePunct) != "" {
			r, _ := utf8.DecodeLastRuneInString(f)
			return strings.ContainsRune(clauseEnd, r)
		}
	}
	return false
}

// countWords are the words that make a four-digit number of the question a
// count, a position or a name rather than a year (yearEdgeStands, issue
// #487): the nouns that are counted, numbered or named by a number — lines,
// tokens, ports, issues, IDs, values, entries, bytes, requests, versions —,
// the places a count stands in (a list, a table, a file, a queue, a buffer),
// the units of money and of the time of day, and the verbs that count, cost,
// skip or read to a position. countWordEndings are the German heads of
// compounds of them ("Logzeilen", "Fehlercode", "Portnummer").
var (
	countWords = toWordSet(
		"port", "ports", "token", "tokens", "zeile", "zeilen", "line", "lines", "row", "rows", "spalte",
		"spalten", "column", "columns", "issue", "issues", "pr", "prs", "ticket", "tickets", "id", "ids",
		"nummer", "nummern", "number", "numbers", "nr", "wert", "werte", "werten", "value", "values",
		"eingabe", "eingaben", "input", "inputs", "element", "elemente", "elementen", "elements",
		"eintrag", "einträge", "einträgen", "entry", "entries", "item", "items", "byte", "bytes", "kb",
		"mb", "gb", "ms", "zeichen", "character", "characters", "chars", "request", "requests",
		"anfrage", "anfragen", "version", "versionen", "versions", "code", "codes", "kommentar",
		"kommentare", "comment", "comments", "index", "offset", "position", "seite", "seiten", "page",
		"pages", "treffer", "hits", "versuch", "versuche", "versuchen", "retry", "retries",
		"iteration", "iterationen", "iterations", "durchlauf", "durchläufe", "durchläufen", "limit",
		"liste", "list", "tabelle", "table", "datei", "file", "queue", "warteschlange", "buffer",
		"puffer", "batch", "kontext", "context", "ausgabe", "output", "speicher", "ram", "cache", "prompt", "memory",
		"stück", "score", "scores", "schleife", "loop", "size", "größe", "länge", "length", "kontextfenster", "einmal",
		"once", "platte", "disk", "folge", "heap", "parallel", "uhr", "euro", "eur", "dollar", "dollars", "cent", "budget", "preis",
		"price", "kostet", "kosten", "cost", "costs", "zähle", "zählen", "zähl", "gezählt", "count",
		"counted", "counting", "scroll", "scrolle", "skip", "überspring", "überspringe", "spring",
		"springe", "lies", "lese", "read", "reading",
	)
	countWordEndings = []string{"zeile", "zeilen", "nummer", "nummern", "code", "codes", "token", "tokens", "ticket", "tickets", "request", "requests", "anfrage", "anfragen", "eintrag", "einträge", "einträgen", "kommentar", "kommentare", "version", "versionen"}
	// countPhrases ask for a count: "wie viele", "how many".
	countPhrases = []string{"wie viel", "wieviel", "how many", "how much"}
)

// yearEdgeStands reports whether a bare year after the word at the byte
// offset off of the lowercased question lower, whose words are c
// (clauseTokens of lower), may stand as a window's edge
// (issue #487): a year needs, beside the window of yearInWindow and the word
// behind it (yearEdge), a clause without a word that makes the number a
// count, a position or a name (countWords) and a question that asks for no
// count — the end of its clause alone is no proof ("Zeig mir die Zeilen ab
// 2024.", "Nach wie vielen Requests, ab 2024?"), while "since 2024, lines
// have grown" names the year. Such a question goes to the LLM fallback, as
// before.
func yearEdgeStands(lower string, c clauseWords, off int) bool {
	return newYearProof(lower, c).stands(off)
}

// yearProof answers yearEdgeStands for the year candidates of one question
// (matchOpenEdge asks it per candidate), reading the question for a count
// phrase once and each clause for a count word once (issue #506): read anew
// per candidate, the check cost time quadratic in the length of a question
// with many candidates.
type yearProof struct {
	lower string
	c     clauseWords
	// asked: count holds whether the question asks for a count.
	asked, count bool
	// counted holds, per clause start, whether the clause has a count word.
	counted map[int]bool
}

func newYearProof(lower string, c clauseWords) *yearProof {
	return &yearProof{lower: lower, c: c, counted: map[int]bool{}}
}

// stands is yearEdgeStands for the year after the word at the byte offset
// off.
func (p *yearProof) stands(off int) bool {
	if !p.asked {
		p.asked = true
		scanned(len(p.lower))
		for _, phrase := range countPhrases {
			if strings.Contains(p.lower, phrase) {
				p.count = true
			}
		}
	}
	if p.count {
		return false
	}
	s, e := 0, len(p.c.words)
	if i := p.c.wordAt(off); i >= 0 {
		s, e = p.c.clauseOf(i)
	}
	counted, ok := p.counted[s]
	if !ok {
		scanned(e - s)
		counted = hasCountWord(p.c.words[s:e])
		p.counted[s] = counted
	}
	return !counted
}

// hasCountWord reports whether words hold a word of countWords or one that
// ends with a head of countWordEndings.
func hasCountWord(words []string) bool {
	for _, w := range words {
		if countWords[w] {
			return true
		}
		for _, end := range countWordEndings {
			if len(w) > len(end)+2 && strings.HasSuffix(w, end) {
				return true
			}
		}
	}
	return false
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

	// A bare year: the whole year, not before a counted noun (issue #487). A
	// month name after it is the month ("2024 Oktober", below): no month name
	// is a yearFollower but "may", which after a year is the modal
	// (monthWords).
	if e, ok := yearEdge(phrase, words, now); ok {
		return e
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
		return withWeights(r, dwLinearRange)
	}

	// Priority 3: Seit/Bis and the open edges ab/vor/nach — linear-dominant +
	// month (spans weeks/months; effectiveDimWeights drops the month for a
	// window longer than a year and for an open one); a window edged by a
	// year is linear alone (yearWeighted)
	if r := matchSeit(query, now); takes(r) {
		return withWeights(r, dwLinearMonthSeit)
	}
	if r := matchBis(query, now); takes(r) {
		return withWeights(r, dwLinearMonthSeit)
	}
	if r := matchOpenEdge(query, now, edge); r != nil {
		return withWeights(r, dwLinearMonthSeit)
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

// withWeights gives r the weights of its matcher, dw, unless the matcher set
// its own (yearWeighted).
func withWeights(r *TemporalResult, dw func() map[string]float64) *TemporalResult {
	if r.DimensionWeights == nil {
		r.DimensionWeights = dw()
	}
	return r
}

func toWordSet(words ...string) map[string]bool {
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}

