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

// ruleTokenize splits query on whitespace and hyphens, trims punctuation,
// lowercases — the words of clauseTokens, without a conjunction that joins
// two clauses (coordinate).
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
	// joinedNext: the word is joined to the next one by a hyphen
	// ("Wochenende-Flag", "friday-deploy-script"), issue #506.
	joinedNext []bool
	// written: the word starts with an upper-case letter as written, also in
	// a question whose case caps disregards (afterCapital, issue #517).
	written []bool
	// asks: the clause of the word, as its punctuation ends it, ends with a
	// question mark (barePast, issue #532).
	asks []bool
	// pastForms: a word has the shape of an English simple past that its
	// place decides (englishParticiple, englishIrregularPast); without one
	// barePasts and englishPastIn read nothing (issue #532).
	pastForms bool
	// titled: every word inside a clause is written with its capital
	// ("Who Is Playing Tonight?"), so that a capital names nothing
	// (nameWordAt, issue #532).
	titled bool
	// closing caches the table of closingParticiples, filled once per
	// question by the first clause that needs it (issue #506).
	closing *[]int
	// lang is the language of the whole question (questionLanguage), read
	// once by clauseTokens.
	lang int
	// clauseS and clauseE hold, per word, the start and the end of its
	// clause (clauseOf), filled by clauseTokens once the clauses are final
	// (issue #517): walked per call, the bounds cost time quadratic in a long
	// clause that a check asks once per candidate (the year proof per year,
	// the German Perfekt per auxiliary for its closing clause).
	clauseS, clauseE []int
}

// clauseTokens tokenizes query into clauseWords.
func clauseTokens(query string) clauseWords {
	lower := strings.ToLower(query)
	expanded := strings.ReplaceAll(lower, "-", " ")
	sameLength := len(lower) == len(query)
	c := clauseWords{closing: new([]int)}
	start, sentence := true, true
	for i := 0; i < len(expanded); {
		r, size := utf8.DecodeRuneInString(expanded[i:])
		if unicode.IsSpace(r) {
			// A hyphen between spaces is a dash: it starts a clause
			// ("monday outage - what caused it?"), issue #506.
			start = start || dashAt(lower, i)
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
			c.pastForms = c.pastForms || englishParticiple(w) || englishIrregularPast[w]
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
			for len(c.asks) < len(c.words) {
				c.asks = append(c.asks, r == '?')
			}
		} else if w == "" && strings.Trim(f, "\u2013\u2014") == "" {
			start = true // a dash of its own ("Seit 2022 – wie viele …?")
		}
		i = j
	}
	for len(c.asks) < len(c.words) {
		c.asks = append(c.asks, false)
	}
	c.markJoins(lower)
	c.lang = c.languageOf(0, len(c.words))
	c.written = append([]bool(nil), c.caps...)
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
	c.titled = m > 0 && n == m
	c.colloquial()
	// The spoken forms colloquial maps are words of the language as well
	// ("gab's" counts as "gab").
	c.lang = c.languageOf(0, len(c.words))
	c.coordinate()
	c.indexClauses()
	return c
}

// indexClauses fills clauseS and clauseE from starts.
func (c *clauseWords) indexClauses() {
	n := len(c.words)
	c.clauseS, c.clauseE = make([]int, n), make([]int, n)
	for i := 0; i < n; i++ {
		if i > 0 && !c.starts[i] {
			c.clauseS[i] = c.clauseS[i-1]
		} else {
			c.clauseS[i] = i
		}
	}
	for i := n - 1; i >= 0; i-- {
		if i+1 < n && !c.starts[i+1] {
			c.clauseE[i] = c.clauseE[i+1]
		} else {
			c.clauseE[i] = i + 1
		}
	}
}

// clauseConjunctions join words, phrases and clauses. Between two clauses
// that each have a finite verb of their own the conjunction starts a clause
// (coordinate, issue #517): "Was wurde am Dienstag gemerged und was wird am
// Donnerstag deployed?" asks for a Tuesday that has been and a Thursday to
// come; between words and phrases it joins them ("am Montag und Dienstag",
// "Wer hat am Montag gemerged und am Dienstag deployed?").
var clauseConjunctions = toWordSet("und", "and", "oder", "or", "aber", "but")

// englishFiniteWords are the English auxiliaries, modals and forms of "to
// be", and the forms of the simple past that are no participle: a clause
// with one has a finite verb (finiteClause). The other forms on -ed are the
// participle as often ("Will it be fixed and deployed on Monday?").
var englishFiniteWords = toWordSet(append([]string{"will", "would", "shall", "should", "can", "could", "may", "might", "must",
	"is", "are", "am", "was", "were", "has", "have", "had", "do", "does", "did",
	"won't", "wouldn't", "can't", "cannot", "couldn't", "shouldn't", "isn't", "aren't", "wasn't", "weren't",
	"hasn't", "haven't", "hadn't", "don't", "doesn't", "didn't",
	"broke", "ran", "went", "saw", "wrote", "came", "took", "began", "fell"}, englishStrongPast...)...)

// germanSubjects are the German pronouns that are the subject wherever they
// stand in a clause ("und am Dienstag testen wir"); "es" and "sie" are the
// object as often and count only where they open it, and "ihr" is the
// possessive and the dative as often ("Wir haben Max und ihr Team am Montag
// getroffen") and counts nowhere.
var germanSubjects = toWordSet("ich", "du", "er", "wir", "man")

// bothLanguageWords are words of finiteWord's sets that are another word in
// the other language: the German "was" and "am" are the English "was" and
// "am", the English "war", "bin", "man" and "er" the German ones.
var bothLanguageWords = toWordSet("was", "am", "war", "bin", "man", "er")

// finiteWord reports whether w, in a sentence of the language lang, is a
// finite verb a clause has of its own, or a German subject pronoun, which a
// finite verb comes with (coordinate): a German sentence reads the German
// sets, an English one the English, one of both languages either without
// the words both languages have (bothLanguageWords).
func finiteWord(w string, lang int) bool {
	if futureVerbSet[w] {
		return true
	}
	switch lang {
	case langGerman:
		return germanFiniteVerb(w) || germanSubjects[w]
	case langEnglish:
		return englishFiniteWords[w]
	}
	return !bothLanguageWords[w] && (germanFiniteVerb(w) || germanSubjects[w] || englishFiniteWords[w])
}

// finiteOpening reports whether the words [s, e) of c open with a subject or
// an interrogative before its verb: an English subject pronoun ("and we will
// ship", "and it broke"), "es" or "sie" ("und es läuft"), or an
// interrogative before a word that is no preposition and no date ("und wer
// reviewt am Donnerstag?", not "und was am Dienstag?").
func (c clauseWords) finiteOpening(s, e, lang int) bool {
	if s >= e {
		return false
	}
	w := c.words[s]
	if lang != langGerman && englishSubjects[w] && w != "who" || lang != langEnglish && (w == "es" || w == "sie") {
		return true
	}
	if (germanInterrogatives[w] || englishEmbedOpeners[w]) && s+1 < e {
		next := c.words[s+1]
		return !firstPrepositions[next] && !englishNPJoiners[next] && !dateWord(next)
	}
	return false
}

// englishPerfectAux are the English auxiliaries of the perfect.
var englishPerfectAux = toWordSet("has", "have", "had", "hasn't", "haven't", "hadn't", "'ve")

// perfectBracket reports whether the words [s, e) of c, a conjunct in a
// sentence of the language lang, have the verb of a Perfekt or a state: a
// German auxiliary of the Perfekt with a participle at the conjunct's end
// ("und am Dienstag ist der Build gebrochen", "und sind am Dienstag
// zurückgerollt"), or an English auxiliary of the perfect before "been" or a
// form on -ed ("and on Tuesday it has stopped"). The rules read "sein" with
// the participle of a verb that does not take it as a state, which names no
// time; behind a clause in the past the conjunct reports what followed, so
// it keeps the tense it shares with that clause rather than a neutral one of
// its own (issue #517).
func (c clauseWords) perfectBracket(s, e, lang int) bool {
	last := c.words[e-1]
	// The participle of a plan names the date of what is planned, which lies
	// ahead: no state that shares a past ("…, und was ist für Weihnachten
	// geplant?"), issue #532.
	if planParticiples[last] {
		return false
	}
	closes := c.participleAt(s, e-1) || seinParticiple(last) || strings.Contains(last, "ge") && (strings.HasSuffix(last, "t") || strings.HasSuffix(last, "en"))
	for i := s; i < e; i++ {
		w := c.words[i]
		if lang != langEnglish && perfektAux[w] && closes {
			return true
		}
		if lang != langGerman && englishPerfectAux[w] && i+1 < e && englishPerfectParticiple(c.words[i+1]) {
			return true
		}
	}
	return false
}

// englishPerfectParticiple reports whether w, right behind an English
// auxiliary of the perfect, has the shape of a participle: a form on -ed, or
// one on -en, -ne, -wn or -un of at least four letters ("broken", "been",
// "gone", "done", "shown", "begun") that is no determiner or pronoun.
func englishPerfectParticiple(w string) bool {
	if englishParticiple(w) {
		return true
	}
	if utf8.RuneCountInString(w) < 4 || englishDeterminers[w] || englishSubjects[w] {
		return false
	}
	for _, end := range []string{"en", "ne", "wn", "un"} {
		if strings.HasSuffix(w, end) {
			return true
		}
	}
	return false
}

// pastWord reports whether word k of c, in a conjunct that starts at s and a
// clause that ends at e, in a sentence of the language lang, is a verb in the
// past the conjunct names its time by (coordinate): a German simple past the
// rules know, an English past form ("was", "did", "deployed") that no form of
// "be" makes a passive, or an English verb on -ed right behind its subject
// ("It worked on Monday").
func (c clauseWords) pastWord(s, k, e, lang int) bool {
	w := c.words[k]
	if lang != langEnglish && germanPastVerbs[w] {
		return true
	}
	if lang == langGerman || lang == langMixed && w == "was" {
		return false
	}
	words := c.words[s:e]
	return (englishPastVerbs[w] || englishFinitePast(words, k-s) || c.barePast(s, k, e)) && !englishPassiveAt(words, k-s) &&
		!englishAttributive(words, k-s) && !englishReports(words, k-s, false)
}

// coordinate starts a clause at each conjunction of clauseConjunctions that
// stands between two clauses with a finite verb each (finiteWord or
// finiteOpening, in the language of the sentence), issue #517, and takes the
// conjunction out of the words: like a comma it is no word of either clause.
// The conjuncts are the words from the last such start (or the clause's) to
// the conjunction and from the conjunction to the next one (or the clause's
// end); each word is read once.
func (c *clauseWords) coordinate() {
	var drop []int
	lang, se := langMixed, 0
	for s := 0; s < len(c.words); {
		e := s + 1
		for e < len(c.words) && !c.starts[e] {
			e++
		}
		if s >= se {
			var ss int
			ss, se = c.sentenceOf(s)
			lang = c.lang // the language of a question of one sentence
			if ss > 0 || se < len(c.words) {
				lang = c.sentenceLanguage(ss, se)
			}
		}
		drop = c.coordinateClause(s, e, lang, drop)
		s = e
	}
	c.dropWords(drop)
}

// conjunct is what coordinate reads of the words of a conjunct: a finite verb
// of its own (finiteWord, finiteOpening), a verb in the past (pastWord), an
// auxiliary of the German Perfekt.
type conjunct struct{ finite, past, aux bool }

// add reads word i of c into the conjunct [s, e).
func (k conjunct) add(c *clauseWords, s, i, e, lang int) conjunct {
	k.finite = k.finite || finiteWord(c.words[i], lang) || lang != langGerman && c.englishPresentAt(s, i, e)
	k.past = k.past || c.pastWord(s, i, e, lang)
	k.aux = k.aux || lang != langEnglish && perfektAux[c.words[i]]
	return k
}

// coordinateClause appends to drop the conjunctions of the clause [s, e)
// that start a clause (coordinate) and marks the word behind each as a
// clause start.
func (c *clauseWords) coordinateClause(s, e, lang int, drop []int) []int {
	ls := s
	left := conjunct{finite: c.finiteOpening(s, e, lang)}
	// The words of a conjunct are read up to its end, the first conjunction
	// (fc) or the next one (re): a word behind the conjunction is the other
	// conjunct's (finiteAfter, issue #532).
	fc := s + 1
	for fc < e && !clauseConjunctions[c.words[fc]] {
		fc++
	}
	for j := s; j < e; {
		if j == ls || !clauseConjunctions[c.words[j]] {
			left = left.add(c, ls, j, fc, lang)
			j++
			continue
		}
		re := j + 1
		for re < e && !clauseConjunctions[c.words[re]] {
			re++
		}
		scanned(re - j)
		right := conjunct{finite: c.finiteOpening(j+1, re, lang)}
		for k := j + 1; k < re; k++ {
			right = right.add(c, j+1, k, re, lang)
		}
		// A conjunct whose verb is a Perfekt or a state behind one in the
		// past reports what followed and shares that past ("Am Montag war
		// alles grün und am Dienstag ist der Build gebrochen"); behind one in
		// the present it has a tense of its own.
		shared := j+1 < re && c.perfectBracket(j+1, re, lang) && (left.past || left.aux && c.participleAt(ls, j-1))
		// An interrogative without a verb behind a clause with one is a
		// clause of its own that shares the verb of the clause before it
		// ("Was haben wir am Dienstag gemerged und was am Donnerstag?"),
		// issue #532.
		ellipsis := (left.finite || left.past) && j+1 < re && c.whEllipsis(j+1, re, lang)
		if (left.finite || left.past) && right.finite && j+1 < re && !shared || ellipsis {
			drop = append(drop, j)
			c.starts[j+1] = true
			ls, left = j+1, right
			left.finite = true
		} else {
			left = conjunct{left.finite || right.finite, left.past || right.past, left.aux || right.aux}
		}
		j = re
	}
	return drop
}

// dropWords takes the words at the sorted indexes drop out of c.
func (c *clauseWords) dropWords(drop []int) {
	if len(drop) == 0 {
		return
	}
	n, k := 0, 0
	for i := range c.words {
		if k < len(drop) && drop[k] == i {
			k++
			continue
		}
		c.words[n], c.starts[n], c.caps[n], c.offs[n] = c.words[i], c.starts[i], c.caps[i], c.offs[i]
		c.sentence[n], c.upper[n], c.joinedNext[n], c.written[n] = c.sentence[i], c.upper[i], c.joinedNext[i], c.written[i]
		c.asks[n] = c.asks[i]
		n++
	}
	c.words, c.starts, c.caps, c.offs = c.words[:n], c.starts[:n], c.caps[:n], c.offs[:n]
	c.sentence, c.upper, c.joinedNext, c.written = c.sentence[:n], c.upper[:n], c.joinedNext[:n], c.written[:n]
	c.asks = c.asks[:n]
}

// colloquialForms are spoken forms of German auxiliaries and verbs a
// question writes, mapped to the form the tense detection reads (issue #506):
// "ham", "hamma", "hamse" (haben, haben wir, haben sie), "isses" (ist es).
var colloquialForms = map[string]string{"ham": "haben", "hamma": "haben", "hamse": "haben", "isses": "ist"}

// germanFiniteVerb reports whether w is a finite German verb the rules know.
func germanFiniteVerb(w string) bool {
	return germanPastVerbs[w] || perfektAux[w] || germanFiniteWords[w]
}

// germanWordsOnS are the words that are a finite verb the rules know
// (germanFiniteVerb) followed by "s" and a German word of their own (issue
// #506): the imperative of "lassen" ("lass uns am Freitag releasen" asks for
// the coming Friday, "las" is the simple past of "lesen"); the genitive or
// plural of a noun ("des Stands", "des Lags", "des Stiegs", "des Solls"); the
// genitive of an infinitive used as a noun ("des Könnens"); and the
// participle used as an adjective ("ein passiertes Problem"). Some are the
// verb with "es" as well ("woran lag's", "was soll's"); the word decides, as
// it did before the glued "s" was cut at all. Every other word "<verb>s" of
// the verbs the rules know is no German word.
var germanWordsOnS = toWordSet(
	"lass",
	"stands", "lags", "stiegs", "solls",
	"habens", "werdens", "müssens", "sollens", "könnens", "dürfens", "wollens", "brauchens", "kriegens",
	"passiertes", "ereignetes",
)

// colloquial maps the words of c that are a spoken form of a German verb to
// the verb (issue #506): a word of colloquialForms; a known German finite
// verb with the clitic "es" or "denn" glued to it ("gab's", "hat's", "war's",
// "gibt's", "war'n", written with ' or ’), which the English "it's" or
// "what's" is not; in a German sentence a known finite verb with "s" glued to
// it ("gabs", "wars", "gings", "gibts") where the word is no German word of
// its own (germanWordsOnS: "lass" stays); and in a German sentence "is", which
// spoken German writes for "ist" and which, as
// the English copula as well, says nothing of the language of the question
// (questionLanguage). An English sentence starts a clause at each embedded
// clause it opens (embeddedClauses).
func (c *clauseWords) colloquial() {
	for i, w := range c.words {
		if f, ok := colloquialForms[w]; ok {
			c.words[i] = f
			continue
		}
		for _, clitic := range []string{"'s", "\u2019s", "'n", "\u2019n"} {
			if stem, ok := strings.CutSuffix(w, clitic); ok && (germanPastVerbs[stem] || perfektAux[stem] || germanMarkers[stem]) {
				c.words[i] = stem
			}
		}
	}
	for ss := 0; ss < len(c.words); {
		_, se := c.sentenceOf(ss)
		switch c.sentenceLanguage(ss, se) {
		case langGerman:
			for i := ss; i < se; i++ {
				w := c.words[i]
				if w == "is" {
					c.words[i] = "ist"
				} else if stem, ok := strings.CutSuffix(w, "s"); ok && germanFiniteVerb(stem) && !germanWordsOnS[w] {
					c.words[i] = stem // "gabs", "wars", "gings", "gibts", "hats": the verb with "es"
				}
			}
		case langEnglish:
			c.embeddedClauses(ss, se)
		}
		ss = se
	}
}

// englishEmbedOpeners open an English embedded clause in the middle of a
// sentence: "I need to know which commits landed on Monday", "Can you check
// whether someone wiped the cache?". "that" and "if" are not among them —
// "that" is far more often the determiner ("Did that deploy on Monday
// fail?").
var englishEmbedOpeners = toWordSet("which", "what", "who", "whom", "whose", "when", "where", "why", "how", "whether", "because")

// embeddedClauses starts a clause at each English embedded-clause opener of
// the sentence [s, e) of c (issue #506): an English sentence marks its
// subordinate clauses by the word that opens them, not by a comma, and the
// tense of the clause a date stands in decides (tenseAt).
func (c *clauseWords) embeddedClauses(s, e int) {
	relative := -1
	for i := s + 1; i < e; i++ {
		w := c.words[i]
		if englishEmbedOpeners[w] {
			c.starts[i] = true
		}
		if c.reducedRelative(i) || w == "that" && i+1 < e && (reducedSubjects[c.words[i+1]] || englishMainAux[c.words[i+1]]) {
			// "that" before a subject or an auxiliary opens a relative or
			// a reported clause ("the feature that we built on Monday",
			// "the PRs that are due on Friday"), issue #532.
			c.starts[i] = true
		}
		switch {
		case englishRelativeOpeners[w]:
			relative = i
		case relative >= 0 && i >= relative+3 && englishMainAux[w] && !englishSubjects[c.words[i-1]] &&
			(w != "be" || c.words[i-1] != "to" && !englishFiniteWords[c.words[i-1]]):
			// The auxiliary of the main clause behind a relative clause
			// without a comma ends it: "Which PRs that we merged on Monday
			// will ship on Friday?" (issue #532).
			c.starts[i] = true
			relative = -1
		}
	}
}

// reducedSubjects are the subject pronouns that open an English relative
// clause without its pronoun (reducedRelative).
var reducedSubjects = toWordSet("we", "you", "they", "i", "he", "she")

// reducedRelative reports whether word i of c, a subject pronoun, opens an
// English relative clause without its pronoun: it stands right behind a noun
// that a determiner opens one or two words before it ("the bug we had on
// Monday", "the issue you raised"), issue #532.
func (c clauseWords) reducedRelative(i int) bool {
	if i < 2 || !reducedSubjects[c.words[i]] || c.starts[i-1] {
		return false
	}
	noun := c.words[i-1]
	if englishNPJoiners[noun] || englishFiniteWords[noun] || englishSubjects[noun] || englishParticiple(noun) ||
		englishIrregularPast[noun] || strings.HasSuffix(noun, "ly") || dateWord(noun) || dayKeywords[noun] {
		return false
	}
	return englishDeterminers[c.words[i-2]] || i >= 3 && !c.starts[i-2] && englishDeterminers[c.words[i-3]]
}

// englishRelativeOpeners open an English relative clause inside a sentence,
// and englishMainAux are the auxiliaries and modals that, two words or more
// behind one and not behind a subject, are the verb of the clause the relative
// clause stands in (embeddedClauses) — "be" of a modal before the relative
// clause as well ("Will the service that froze on Tuesday be restarted on
// Thursday?", issue #532), not one behind "to" or an auxiliary of the
// relative clause ("the tickets that have to be closed on Monday").
var (
	englishRelativeOpeners = toWordSet("that", "which", "who")
	englishMainAux         = toWordSet("will", "would", "shall", "should", "can", "could", "may", "might", "must",
		"won't", "wouldn't", "can't", "cannot", "couldn't", "shouldn't", "is", "are", "was", "were", "isn't", "aren't",
		"wasn't", "weren't", "be")
)

// compoundWords are the words a weekday or the weekend may be joined to by a
// hyphen and still name a day: another weekday ("Montag-Freitag") or a time
// of day ("Montag-Abend").
var compoundWords = toWordSet("wochenende", "weekend", "morgen", "vormittag", "mittag", "nachmittag", "abend", "nacht",
	"früh", "morgens", "vormittags", "mittags", "nachmittags", "abends", "nachts", "morning", "afternoon", "evening",
	"night", "noon", "midnight")

// inCompound reports whether word i of c is joined by a hyphen to a word
// that names no day — a weekday or the weekend in "Wochenende-Flag",
// "Mittwochs-Job" or "friday-deploy-script" names a thing, not a day (issue
// #506).
func (c clauseWords) inCompound(i int) bool {
	names := func(w string) bool {
		_, de := weekdayMapDE[w]
		_, en := weekdayMapEN[w]
		return de || en || compoundWords[w] || strings.Trim(w, "0123456789.") == ""
	}
	return c.joinedNext[i] && !names(c.words[i+1]) || i > 0 && c.joinedNext[i-1] && !names(c.words[i-1])
}

// titleWords are the words of address and the titles a surname follows:
// "Herr Freitag", "Frau Dr. Sonntag", "Prof. Montag", "Mr Friday", "Mrs.
// Monday" (issue #517). titleArticles before "Herr" or "Frau" make it the
// noun, which a weekday as a date may follow ("Kam die Frau Montag oder
// Dienstag?").
var (
	titleWords    = toWordSet("herr", "herrn", "frau", "dr", "prof", "mr", "mrs", "ms", "miss")
	titleArticles = toWordSet("der", "die", "das", "den", "dem", "des", "ein", "eine", "einer", "einem", "einen", "the", "a")
)

// surnameAt reports whether word i of c, a weekday, is a surname: right
// behind a title word in its clause (issue #517) — "Was hat Herr Freitag
// gesagt?" names no Friday. A title written in capitals is an acronym ("DR
// Monday", "MS Teams"), and one behind an article the noun.
func (c clauseWords) surnameAt(i int) bool {
	if i == 0 || c.starts[i] || !titleWords[c.words[i-1]] || c.upper[i-1] || commonTitleWords[c.words[i-1]] && !c.caps[i-1] {
		return false
	}
	return i < 2 || !titleArticles[c.words[i-2]]
}

// commonTitleWords are titles that are other words as well when written in
// lower case: "ms" the milliseconds, "miss" the verb ("Did you miss
// Monday?"); they are a title only with their capital (issue #517).
var commonTitleWords = toWordSet("ms", "miss")

// notADay reports whether word i of c, a weekday or the weekend, names no day:
// it is joined to a word that names none (inCompound) or a surname
// (surnameAt).
func (c clauseWords) notADay(i int) bool {
	return c.inCompound(i) || c.surnameAt(i)
}

// dayWeekdays returns the indexes of the words of c that are a weekday naming
// a day (notADay), with the weekday each names.
func (c clauseWords) dayWeekdays() []struct {
	wd  time.Weekday
	idx int
	ref string
} {
	all := findAllWeekdays(c.words)
	n := 0
	for _, w := range all {
		if !c.notADay(w.idx) {
			all[n] = w
			n++
		}
	}
	return all[:n]
}

// dayPartHours are the words of a part of the day that may follow a weekday
// and belong to it, with the hour matchTimeOfDay gives the part (issue
// #517): "Tuesday morning", "saturday night", "Freitag Abend", "Montag früh",
// "Montag-Abend". dayPartsBefore are those English puts before the weekday
// ("early Tuesday", "late Friday"); behind it "early" and "late" say how a
// thing came, not when ("Was the Monday release late?" is no night).
var (
	dayPartHours = map[string]int{
		"morgen": 8, "früh": 8, "vormittag": 10, "mittag": 12, "nachmittag": 15, "abend": 19, "nacht": 23, "spät": 23,
		"morgens": 8, "vormittags": 10, "mittags": 12, "nachmittags": 15, "abends": 19, "nachts": 23,
		"morning": 8, "noon": 12, "midday": 12, "afternoon": 15, "evening": 19, "night": 23,
	}
	dayPartsBefore = map[string]int{"early": 8, "late": 23}
	// compoundDayParts are the parts of the day German compounds a weekday
	// with: "Montagmorgen", "Dienstagabend", "Freitagnachmittag".
	compoundDayParts = map[string]int{"morgen": 8, "früh": 8, "vormittag": 10, "mittag": 12, "nachmittag": 15, "abend": 19, "nacht": 23}
)

// weekdayCompound returns the weekday and the hour of a German weekday
// compounded with a part of the day ("montagmorgen", "dienstagabend"), issue
// #517.
func weekdayCompound(w string) (time.Weekday, int, bool) {
	if len(w) < len("montagfrüh") {
		return 0, 0, false
	}
	for wd, name := range weekdayStemsDE {
		if rest, ok := strings.CutPrefix(w, name); ok {
			if h, ok := compoundDayParts[rest]; ok {
				return time.Weekday(wd), h, true
			}
		}
	}
	return 0, 0, false
}

// weekdayStemsDE are the German weekday names in lower case, by weekday.
var weekdayStemsDE = func() (out [7]string) {
	for i, name := range weekdayNameDE {
		out[i] = strings.ToLower(name)
	}
	return out
}()

// weekdayWord returns the weekday w names: a weekday name of either language
// or a German weekday compounded with a part of the day.
func weekdayWord(w string) (time.Weekday, bool) {
	if wd, ok := weekdayMapDE[w]; ok {
		return wd, true
	}
	if wd, ok := weekdayMapEN[w]; ok {
		return wd, true
	}
	wd, _, ok := weekdayCompound(w)
	return wd, ok
}

// dayPartAt returns the hour of the part of the day that belongs to word i
// of c, a weekday (issue #517): the part it is compounded with
// ("Montagmorgen"), the word of a part of the day right behind it in its
// clause ("Tuesday morning", "Freitag Abend", "Montag-Abend") or, in
// English, right before it ("early Tuesday", "late Friday").
func (c clauseWords) dayPartAt(i int) (int, bool) {
	if _, h, ok := weekdayCompound(c.words[i]); ok {
		return h, true
	}
	if i+1 < len(c.words) && !c.starts[i+1] {
		if h, ok := dayPartHours[c.words[i+1]]; ok {
			return h, true
		}
	}
	if i > 0 && !c.starts[i] {
		if h, ok := dayPartsBefore[c.words[i-1]]; ok {
			return h, true
		}
	}
	return 0, false
}

// dashAt reports whether the byte i of lower is a hyphen between spaces, a
// dash.
func dashAt(lower string, i int) bool {
	return lower[i] == '-' && (i == 0 || strings.IndexByte(asciiSpace, lower[i-1]) >= 0) &&
		(i+1 == len(lower) || strings.IndexByte(asciiSpace, lower[i+1]) >= 0)
}

// markJoins fills joinedNext from lower, the lowercased question c was read
// from.
func (c *clauseWords) markJoins(lower string) {
	c.joinedNext = make([]bool, len(c.words))
	for k := 0; k+1 < len(c.words); k++ {
		end := c.offs[k] + len(c.words[k])
		c.joinedNext[k] = end < len(lower) && lower[end] == '-' && c.offs[k+1] == end+1
	}
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

// clauseOf returns the words [s, e) of the clause that holds word i: from
// the table of indexClauses, or — for words whose clauses are not final yet
// — walked, which counts as the words it reads (ruleScanHook).
func (c clauseWords) clauseOf(i int) (s, e int) {
	if c.clauseS != nil {
		scanned(1)
		return c.clauseS[i], c.clauseE[i]
	}
	s = i
	for s > 0 && !c.starts[s] {
		s--
	}
	e = i + 1
	for e < len(c.words) && !c.starts[e] {
		e++
	}
	scanned(e - s)
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
var englishPastVerbs = toWordSet(append([]string{
	"was", "were", "had", "went", "did",
	"fixed", "deployed", "cancelled", "canceled", "happened",
	"broke", "crashed", "failed", "finished", "started", "created",
	"moved", "changed", "removed", "deleted", "updated",
	"merged", "pushed", "reviewed", "shipped", "ran", "wrote", "found", "saw", "made", "got",
}, englishStrongPast...)...)

// englishStrongPast are English forms of the simple past that are neither a
// participle nor another word (issue #532): wherever they stand they name a
// past, as "went" and "broke" do. Forms that are a noun as well ("rose",
// "bore", "bit") and those whose present is the same form ("read", "set",
// "put", "cut", "hit", "let") are not among them, and neither are the forms
// that are another word as well (englishPastHomographs).
var englishStrongPast = []string{
	"knew", "grew", "threw", "flew", "blew", "chose", "froze", "wore", "swore", "gave", "ate", "became", "sank",
	"drank", "swam", "shook", "forgot", "forgave", "arose", "overtook", "undertook", "mistook", "withdrew",
	"overran", "overthrew", "rewrote", "underwent", "outgrew", "took", "came", "began",
}

// englishPastHomographs are English forms of the simple past that are
// another word as well: a name ("Drew", "Sang", "Rode"), a noun ("spoke",
// "stole", "drove", "tore", and the German "Rang" and "Tore") or an
// adjective ("woke"). Read wherever they stand they turned "Will Drew join
// the retro on Monday?" into a past; like the forms of englishIrregularPast
// they are a past only where a simple past stands (englishFinitePast,
// barePast), issue #532. "fell" is the present of "to fell" as well
// ("Crews fell the trees on Monday"), which takes an object: it is a past
// where a simple past stands and no object follows it (transitiveFell:
// "Which node fell over on Saturday?", "Throughput fell sharply").
// "found" and "saw" of the older list are a noun or a present as well
// ("Lost and found opens on Saturday?"), but read by their place they lose
// the simple past before an object without an article ("The QA team found
// three bugs on Friday?"), which 5a28c475 read; they stay where they stand.
var englishPastHomographs = []string{"drew", "spoke", "stole", "woke", "drove", "sang", "rode", "rang", "tore", "fell"}

// transitiveFell reports whether words[i] is "fell" before its object, the
// present of "to fell" (englishPastHomographs).
func transitiveFell(words []string, i int) bool {
	return words[i] == "fell" && i+1 < len(words) && imperativeObjects[words[i+1]]
}

// englishReportVerbs are the simple pasts of the English verbs of saying,
// hearing, thinking and knowing a clause can follow without "that", and
// englishReportRegular those on -ed among them, which only the reading of
// a bare subject (barePast) leaves unread: a clause behind one is reported
// and has a tense of its own ("I heard the release is on Friday", "They said
// the fix lands on Monday"), so the verb names no past of a date in it
// (englishReports, issue #532). reportObjects are the pronouns "told" and
// "promised" take before the clause, reportOpeners the words that open one.
var (
	englishReportVerbs   = toWordSet("said", "told", "heard", "thought", "knew", "understood", "meant", "felt")
	englishReportRegular = toWordSet("believed", "assumed", "guessed", "figured", "hoped", "claimed", "expected",
		"promised", "learned", "realized", "realised", "noticed", "confirmed", "announced", "mentioned")
	reportObjects = toWordSet("me", "us", "him", "her", "them", "you")
	reportOpeners = toWordSet("that", "the", "this", "these", "those", "my", "our", "your", "their", "his", "its",
		"we", "you", "they", "i", "he", "she", "it", "i'd", "we'd", "you'd", "they'd", "he'd", "she'd", "i'll",
		"we'll", "you'll", "they'll", "he'll", "she'll", "it'll", "we're", "you're", "they're", "it's", "he's",
		"she's", "i'm")
)

// englishReports reports whether words[i], a verb of englishReportVerbs (and
// with regular of englishReportRegular), has a reported clause behind it:
// the word behind it, or behind its pronoun object, opens one.
func englishReports(words []string, i int, regular bool) bool {
	if !englishReportVerbs[words[i]] && (!regular || !englishReportRegular[words[i]]) {
		return false
	}
	j := i + 1
	if j+1 < len(words) && reportObjects[words[j]] && (words[i] == "told" || words[i] == "promised") && reportOpeners[words[j+1]] {
		j++
	}
	return j < len(words) && reportOpeners[words[j]]
}

// englishIrregularPast are English forms of the simple past that are the
// participle as well, and the short forms on -ed englishParticiple does not
// read: like a form on -ed they are a past only where a simple past stands
// (englishFinitePast) — "We met on Tuesday", not "the lost node" (issue
// #532). The homographs of englishPastHomographs are read the same way.
var englishIrregularPast = toWordSet(append([]string{
	"met", "sent", "built", "lost", "paid", "held", "kept", "left", "told", "sold", "bought", "brought", "caught",
	"taught", "thought", "meant", "spent", "won", "heard", "said", "slept", "stood", "understood", "stuck", "struck",
	"hung", "dug", "fought", "sought", "dealt", "lent", "fled", "used", "died", "tied", "owed",
}, englishPastHomographs...)...)

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
// neither, and so is "is", which spoken German writes for "ist" (issue #506); the weekday names and the month names that differ between the
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
		"the", "a", "are", "were", "been", "be", "did", "do", "does", "done", "has", "have",
		"had", "on", "at", "of", "to", "for", "from", "with", "by", "it", "its", "this", "that",
		"these", "those", "there", "what", "which", "who", "whom", "whose", "when", "where", "why",
		"how", "we", "you", "they", "he", "she", "i", "my", "our", "your", "their", "his",
		"not", "no", "any", "anyone", "anything", "everything", "everyone", "someone", "something",
		"nothing", "nobody", "would", "can", "could", "should", "must", "may", "might", "shall",
		"and", "or", "but", "today", "yesterday", "tomorrow", "week", "month", "year", "weekend",
		"last", "next", "since", "until", "after", "before",
	)
)

// questionLanguage is the language of a question, read from its words once
// by clauseTokens (languageOf; issue #506: read anew per sentence, the
// fallback of sentenceLanguage cost time quadratic in the number of
// sentences). It reads it from its words
// (issue #487): German when it has German marker words, weekday or month
// names and no English ones, English the other way round, and mixed
// otherwise. A word in capitals is an acronym and "am" or "um" after a
// number or "I" English, so neither counts. NeedsTranslation, the detection of the translation step, does not
// decide it: it answers only "translate or not" (German and the Nordic
// languages yes, English no) and needs two stopwords, a domain term, a Nordic
// function word or a word with ä ö ü ß å æ ø ("Was kommt am Montag?" is not
// translated).
func questionLanguage(c clauseWords) int {
	return c.lang
}

// languageOf is questionLanguage for the words [s, e) of c.
func (c clauseWords) languageOf(s, e int) int {
	scanned(e - s)
	de, en := 0, 0
	for i := s; i < e; i++ {
		w := c.words[i]
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

// sentenceOf returns the words [s, e) of the sentence that holds word i.
func (c clauseWords) sentenceOf(i int) (s, e int) {
	s = i
	for s > 0 && !c.sentence[s] {
		s--
	}
	e = i + 1
	for e < len(c.words) && !c.sentence[e] {
		e++
	}
	return s, e
}

// sentenceLanguage is the language of the sentence [s, e) of c where its
// words decide it, and the language of the whole question where they do not
// (issue #506): "Was Robert available on Monday? Ist das klar?" asks its
// first sentence in English and its second in German, and the question as a
// whole is of both.
func (c clauseWords) sentenceLanguage(s, e int) int {
	if l := c.languageOf(s, e); l != langMixed {
		return l
	}
	return questionLanguage(c)
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
// verb analysis: the tense the question's first date word counts by
// (tenseAt), so that a clause that frames the question ("I need to know …",
// "Weißt du noch, …", "Can you check whether …") does not decide for the
// clause that asks (issue #506), and for a question without a date word the
// tense of the whole question.
func DetectVerbTense(query string) string {
	c := clauseTokens(query)
	if off, ok := c.firstDateWord(query); ok {
		return tenseAt(query, off)
	}
	return c.wholeTense()
}

// wholeTense is the tense of the whole question c, each sentence scored in
// its own language (sentenceLanguage).
func (c clauseWords) wholeTense() string {
	past, future := 0, 0
	for ss := 0; ss < len(c.words); {
		_, se := c.sentenceOf(ss)
		p, f := c.scores(ss, se, c.sentenceLanguage(ss, se))
		past, future = past+p, future+f
		ss = se
	}
	return verdict(past, future)
}

// firstDateWord returns the byte offset of the first word of query that
// names a day, a week, a month or a year — a weekday, the weekend, a day
// keyword, a month name that is no word of its own (monthWords), a four-digit
// year or a day date —, as clauseTokens' offsets count.
func (c clauseWords) firstDateWord(query string) (int, bool) {
	for i, w := range c.words {
		_, de := weekdayWord(w)
		_, en := weekdayMapEN[w]
		_, month := lookupMonth(w)
		_, year := isYear(w)
		// A weekday or the weekend that names no day (notADay: in a
		// compound, a surname) is no date word: "The friday-deploy-script
		// failed, but we will ship on Monday" asks for Monday (issue #517).
		if (de || en || w == "wochenende" || w == "weekend") && c.notADay(i) {
			continue
		}
		if de || en || w == "wochenende" || w == "weekend" || month && !monthWords[w] || year || dayKeywords[w] {
			if ds := findDayDates(query); len(ds) > 0 && ds[0].start < c.offs[i] && len(query) == len(strings.ToLower(query)) {
				return ds[0].start, true
			}
			return c.offs[i], true
		}
	}
	if ds := findDayDates(query); len(ds) > 0 && len(query) == len(strings.ToLower(query)) {
		return ds[0].start, true
	}
	return 0, false
}

// dayKeywords are the words of simpleKeywords.
var dayKeywords = func() map[string]bool {
	m := make(map[string]bool, len(simpleKeywords))
	for _, kw := range simpleKeywords {
		m[kw.word] = true
	}
	return m
}()

// tenseAt is the tense a date at the byte offset off of the lowercased
// question counts by (issue #487): the tense of the clause it stands in when
// that clause has a verb of its own ("Sie sagten, wir treffen uns am Montag"
// asks for the coming Monday, "Ich will wissen, was am Montag passiert ist"
// for the last one), and otherwise — the question is one clause, the date
// stands in a clause without a verb ("Am Montag, was ist da passiert?") —
// the tense of the whole question (wholeTense). A clause whose Perfekt
// a later clause closes ("Was haben wir am Montag, nach dem Standup,
// besprochen?") reads that participle (hasPerfektForm). The language is the
// sentence's (sentenceLanguage).
func tenseAt(query string, off int) string {
	return newTenseReader(query).at(off)
}

// tenseReader answers tenseAt for every date of one question from one
// reading of it (issue #506): the words once, each clause's and each
// sentence's tense and language once, the whole question's tense at most
// once. Read anew per date, a question with many dates cost time quadratic in
// its length.
type tenseReader struct {
	c clauseWords
	// offsets: the offsets of c are those of the question as written;
	// where lowercasing changes the length (a few letters beyond ASCII), every
	// date counts by the whole question.
	offsets bool
	// clauseStart and sentenceStart hold, per word, the start of its clause
	// and of its sentence.
	clauseStart, sentenceStart []int
	clauses                    map[int]string // per clause start: its tense, "" without a verb of its own
	ellipses                   map[int]bool   // the clause starts of whEllipsis clauses read so far
	langs                      map[int]int    // per sentence start: its language
	whole                      string
}

func newTenseReader(query string) *tenseReader {
	r := &tenseReader{c: clauseTokens(query), offsets: len(strings.ToLower(query)) == len(query),
		clauses: map[int]string{}, ellipses: map[int]bool{}, langs: map[int]int{}}
	r.clauseStart = make([]int, len(r.c.words))
	r.sentenceStart = make([]int, len(r.c.words))
	for i := range r.c.words {
		if i > 0 && !r.c.starts[i] {
			r.clauseStart[i] = r.clauseStart[i-1]
		} else {
			r.clauseStart[i] = i
		}
		if i > 0 && !r.c.sentence[i] {
			r.sentenceStart[i] = r.sentenceStart[i-1]
		} else {
			r.sentenceStart[i] = i
		}
	}
	return r
}

// at is tenseAt for the date at the byte offset off.
func (r *tenseReader) at(off int) string {
	if i := r.c.wordAt(off); i >= 0 && r.offsets {
		if t := r.clauseTense(r.clauseStart[i]); t != "" {
			return t
		}
	}
	if r.whole == "" {
		r.whole = r.c.wholeTense()
	}
	return r.whole
}

// clauseTense is the tense of the clause that starts at s, read once: ""
// for a clause without a verb of its own, which counts by the whole
// question; an interrogative without a verb (whEllipsis) counts by the
// clause before it.
func (r *tenseReader) clauseTense(s int) string {
	if t, ok := r.clauses[s]; ok {
		return t
	}
	e := s + 1
	for e < len(r.c.words) && !r.c.starts[e] {
		e++
	}
	scanned(e - s)
	t := ""
	switch {
	case r.c.whEllipsis(s, e, r.language(r.sentenceStart[s])):
		r.ellipses[s] = true
		t = r.ellipsisTense(s)
	case (s > 0 || e < len(r.c.words)) && (r.c.hasOwnVerb(s, e) || r.c.englishPastIn(s, e, r.language(r.sentenceStart[s])) ||
		r.c.englishPresentIn(s, e, r.language(r.sentenceStart[s]))):
		t = r.c.tense(s, e, r.language(r.sentenceStart[s]))
	}
	r.clauses[s] = t
	return t
}

// ellipsisTense is the tense of the clause that starts at s, an
// interrogative without a verb (whEllipsis): the tense of the nearest clause
// before it in its sentence that has a verb of its own, or that is such an
// ellipsis itself; "" without one.
func (r *tenseReader) ellipsisTense(s int) string {
	for p := s; p > r.sentenceStart[s]; {
		p = r.clauseStart[p-1]
		if t := r.clauseTense(p); t != "" || r.ellipses[p] {
			return t
		}
	}
	return ""
}

// language is sentenceLanguage for the sentence that starts at ss, read
// once.
func (r *tenseReader) language(ss int) int {
	if l, ok := r.langs[ss]; ok {
		return l
	}
	_, se := r.c.sentenceOf(ss)
	scanned(se - ss)
	l := r.c.sentenceLanguage(ss, se)
	r.langs[ss] = l
	return l
}

// tense scores the words [s, e) of c in the language lang.
func (c clauseWords) tense(s, e, lang int) string {
	return verdict(c.scores(s, e, lang))
}

// verdict is the tense of a past and a future score: neutral without
// either, the higher one, and past for a tie (conservative).
func verdict(pastScore, futureScore int) string {
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

// scores is the past and the future score of the words [s, e) of c in the
// language lang (tense).
func (c clauseWords) scores(s, e, lang int) (pastScore, futureScore int) {
	scanned(e - s)
	words := c.words[s:e]
	joined := strings.Join(words, " ")

	// Explicit direction markers (strong signal)
	for _, tok := range words {
		if explicitPastSet[tok] {
			pastScore += 2
		}
		if explicitFutureSet[tok] {
			futureScore += 2
		}
	}

	// Past verbs: German ones always, English ones outside a German question
	// unless "be" makes them a passive or a state (englishPassiveAt) and,
	// beyond the list, a verb on -ed right behind its subject
	// (englishFinitePast); "was" as englishWasAt reads it (issue #487); and
	// the German simple past in the place of a finite verb (finitePast).
	if lang != langEnglish && c.finitePast(s, e) {
		pastScore++
	}
	pastScore += pastVerbs(words, lang)
	if lang != langGerman {
		pastScore += c.barePasts(s, e)
	}

	// Future verbs
	for _, tok := range words {
		if futureVerbSet[tok] {
			futureScore++
		}
	}

	// The German Perfekt and the past subjunctive (hasPerfektForm), and the
	// Perfekt a German question writes without its auxiliary
	// (ellipticPerfekt).
	if c.hasPerfektForm(s, e) || lang != langEnglish && c.ellipticPerfekt(s, e) {
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
	return pastScore, futureScore
}

// pastVerbs counts the past verbs of words in the language lang (tense).
func pastVerbs(words []string, lang int) int {
	n := 0
	for i, tok := range words {
		switch {
		case germanPastVerbs[tok]:
			n++
		case tok == "was":
			if englishWasAt(words, lang, i) {
				n++
			}
		case lang != langGerman && (englishPastVerbs[tok] || englishFinitePast(words, i)) && !englishPassiveAt(words, i) &&
			!englishAttributive(words, i) && !englishReports(words, i, false):
			n++
		}
	}
	return n
}

// wasParticles stand between a clause-opening German interrogative and its
// finite verb ("Was genau fehlte am Montag?", "Wann genau startete …?").
var wasParticles = toWordSet("genau", "denn", "noch", "sonst", "eigentlich", "alles", "so")

// The words that place a German finite verb (finitePast): an interrogative
// opens a main clause with the verb right behind it, a "welche" phrase a main
// clause with the verb behind its noun, a subordinating conjunction a clause
// with the verb at its end. The first word of a yes/no question is not read:
// there the past subjunctive asks for the present far more often than a
// simple past for the past ("Sollten wir am Freitag deployen?", "Könnten wir
// am Montag releasen?").
var (
	germanInterrogatives = toWordSet("was", "wer", "wen", "wem", "wessen", "wann", "wie", "wo", "warum", "wieso", "weshalb",
		"weswegen", "woran", "worauf", "worüber", "wobei", "wodurch", "womit", "wofür", "wozu", "wohin", "woher", "wovon",
		"worin", "woraus", "wogegen")
	germanWhich         = toWordSet("welche", "welcher", "welches", "welchen", "welchem")
	germanSubordinators = toWordSet("dass", "ob", "weil", "wenn", "als", "bevor", "nachdem", "während", "obwohl",
		"sobald", "falls", "seitdem", "solange", "sodass", "damit")
	// modalSubjunctives are the past subjunctive of the modals and of
	// "haben" and "wissen", which take the shape of the weak simple past
	// ("Was könnten wir am Montag tun?"), and "sollten" and "wollten", whose
	// simple past and subjunctive are one form: a closed class the shape
	// cannot tell from a simple past, so finitePastAt reads none of them.
	modalSubjunctives = toWordSet("könnte", "könnten", "könntest", "könntet", "müsste", "müssten", "müsstest", "müsstet",
		"dürfte", "dürften", "dürftest", "dürftet", "möchte", "möchten", "möchtest", "möchtet", "hätte", "hätten",
		"hättest", "hättet", "wüsste", "wüssten", "bräuchte", "bräuchten", "sollten", "wollten", "solltest", "wolltest")
)

// finitePast reports whether a clause of [s, e) has a verb in the simple past
// in the place of its finite verb (issues #487 and #506): behind the
// interrogative that opens it ("Was fehlte am Montag?", "Wer deployte am
// Dienstag?", "Wann genau startete der Container neu?"), behind the noun of a
// "welche" phrase ("Welche Jobs scheiterten am Donnerstag?"), at its end when a
// subordinating conjunction opens it ("…, ob der Job am Montag scheiterte")
// or an interrogative behind a clause with a verb of its own ("Mich
// interessiert, warum der Deploy am Mittwoch fehlschlug."), and behind the
// first constituent of a main clause where its end can be told
// (afterFirstConstituent: "Am Donnerstag crashte der Server").
// The verbs of that place are read by their shape (finitePastAt), since the
// simple past of the weak and the loan verbs is an open class no list holds;
// the place keeps out the nouns and adjectives on -te ("die Liste", "die
// letzte Version"), which stand elsewhere.
func (c clauseWords) finitePast(s, e int) bool {
	for cs := s; cs < e; {
		_, ce := c.clauseOf(cs)
		if ce > e {
			ce = e
		}
		if c.clauseFinitePast(cs, ce) {
			return true
		}
		cs = ce
	}
	return false
}

// clauseFinitePast is finitePast for the one clause [s, e).
func (c clauseWords) clauseFinitePast(s, e int) bool {
	// An interrogative behind a clause with a verb of its own opens an
	// indirect question, which puts its verb last; behind a phrase without
	// one ("Am Montag, was …?") it opens the main clause.
	indirect := !c.sentence[s] && s > 0 && c.hasOwnVerb(c.clauseOf(s-1))
	switch w := c.words[s]; {
	case germanInterrogatives[w]:
		v := s + 1
		for v < e && wasParticles[c.words[v]] {
			v++
		}
		return c.finitePastAt(v, e) || indirect && c.finitePastAt(e-1, e) || c.sentence[s] && c.verbLast(v, e) && c.verbLastPastAt(e-1, e)
	case germanWhich[w]:
		return c.finitePastAt(s+2, e) || indirect && c.finitePastAt(e-1, e)
	case germanSubordinators[w]:
		return c.finitePastAt(e-1, e)
	case c.relativeClause(s, e):
		return c.verbLastPastAt(e-1, e)
	}
	if v := c.afterFirstConstituent(s, e); v > s {
		return c.finitePastAt(v, e)
	}
	return false
}

// verbLastPastAt is finitePastAt for the last word of a clause the readings
// of issue #532 find to put its verb last (relativeClause, an interrogative
// at the start of a sentence): a strong form at its end counts only as the
// whole word or behind a prefix ("hing", "auftraten", "kaputtging"), since a
// present ends like one as often ("schließen" like "ließen", "vorschlagen"
// like "lagen"); the weak shape decides the rest.
func (c clauseWords) verbLastPastAt(i, e int) bool {
	if !c.finitePastAt(i, e) {
		return false
	}
	w := c.words[i]
	return strongPastWord(w) || weakPastShape(w)
}

// strongPastWord reports whether w is a form of strongPastForms as the whole
// word or behind a prefix of pastPrefixPat ("hing", "auftraten", "ergab").
func strongPastWord(w string) bool {
	for _, f := range strongPastForms {
		if rest, ok := strings.CutSuffix(w, f); ok && (rest == "" || pastPrefixPat.MatchString(rest)) {
			return true
		}
	}
	return false
}

// pastPrefixPat are the prefixes a strong simple past takes in a verb-last
// clause (verbLastPastAt).
var pastPrefixPat = regexp.MustCompile(`^(?:` + separablePrefixes + `|be|emp|ent|er|miss|ver|zer|über|unter|hinter|wider|dazu|hinzu)+$`)

// verbLast reports whether word v of a clause [_, e) that an interrogative
// or a relative pronoun opens is no finite verb, so that the clause puts its
// verb last (issue #532): a preposition, an article, a subject pronoun, a day
// keyword or a date word ("Schreib Anna, was am Freitag schiefging", "Was am
// Freitag kaputtging, reparieren wir am Montag").
func (c clauseWords) verbLast(v, e int) bool {
	if v >= e-1 {
		return false
	}
	w := c.words[v]
	return firstPrepositions[w] || firstArticles[w] || firstSubjects[w] || firstDayKeywords[w] || dateWord(w)
}

// relativePronouns open a German relative clause behind a comma.
var relativePronouns = toWordSet("der", "die", "das", "den", "dem", "denen", "deren", "dessen")

// relativeClause reports whether the clause [s, e) of c is a German relative
// clause that puts its verb last: a relative pronoun opens it inside a
// sentence, before a word that is no noun (verbLast) — "Die Fehler, die am
// Montag auftraten, …", not "…, die Tickets am Montag" (issue #532).
func (c clauseWords) relativeClause(s, e int) bool {
	return relativePronouns[c.words[s]] && !c.sentence[s] && c.verbLast(s+1, e)
}

// The first constituents of a German main clause whose end
// afterFirstConstituent finds: a subject pronoun, a day keyword, a
// preposition before a date word, and an article before its noun.
var (
	firstSubjects     = toWordSet("ich", "du", "er", "sie", "es", "wir", "ihr", "man", "jemand", "keiner", "niemand")
	firstPrepositions = toWordSet("am", "im", "an", "in", "seit", "bis", "nach", "vor", "ab", "um", "letzten", "letzte", "vergangenen", "vorigen")
	firstArticles     = toWordSet("der", "die", "das", "den", "dem", "ein", "eine", "einen", "kein", "keine", "unser", "unsere")
	firstDayKeywords  = toWordSet("gestern", "vorgestern", "heute", "vorhin", "damals", "neulich")
)

// afterFirstConstituent returns the word behind the first constituent of
// the clause [s, e), the place of the finite verb of a German main clause
// (issue #506): "Wir mergten am Donnerstag …", "Gestern scheiterte …", "Am
// Donnerstag crashte der Server", "Der Job scheiterte am Mittwoch". Of the
// constituents only the forms it can delimit count — a subject pronoun, a
// day keyword, a preposition or "letzten" before a date word, an article
// before one word —; for any other first word it returns s.
func (c clauseWords) afterFirstConstituent(s, e int) int {
	if s+1 >= e {
		return s
	}
	w := c.words[s]
	switch {
	case firstSubjects[w] || firstDayKeywords[w]:
		return s + 1
	case firstPrepositions[w] && dateWord(c.words[s+1]), firstArticles[w]:
		return s + 2
	}
	return s
}

// dateWord reports whether w names a day, a week, a month or a year: a
// weekday, the weekend, a month name, a four-digit year or a day date.
func dateWord(w string) bool {
	_, de := weekdayWord(w)
	_, en := weekdayMapEN[w]
	_, month := lookupMonth(w)
	_, year := isYear(w)
	_, day := parseDayDate(strings.ToUpper(w))
	return de || en || month || year || day || w == "wochenende" || w == "woche" || w == "monat" || w == "jahr"
}

// finitePastAt reports whether word i, before e, has the shape of the German
// simple past (germanPastShape) and is no noun — written with an upper-case
// letter where no clause starts —, no day word on -te ("heute", "letzte",
// "nächste", "Mitte") and no past subjunctive of a modal (modalSubjunctives).
// A clause has one finite verb: a word right before a finite verb the rules
// know is none, whatever its shape ("ab 2050 punkten gilt der Block als
// relevant": "punkten" is the noun the number counts), issue #517.
func (c clauseWords) finitePastAt(i, e int) bool {
	if i < 0 || i >= e || c.caps[i] && !c.starts[i] || i+1 < e && germanFiniteVerb(c.words[i+1]) {
		return false
	}
	w := c.words[i]
	if _, pos := monthPositionModifiers[w]; pos || explicitPastSet[w] || explicitFutureSet[w] || w == "heute" || modalSubjunctives[w] {
		return false
	}
	return germanPastShape(w)
}

// The words around an English verb on -ed (englishPassiveAt,
// englishFinitePast): "be" before it makes it a passive or a state ("will be
// deployed", "is cancelled"); a perfect, a past or another verb before it
// does not ("has been deployed", "was deployed"). A subject pronoun or "who"
// right before it makes it the simple past; behind "what"/"which" and a noun
// it is one only before a word a noun cannot be ("which commits landed on
// Monday", not "what scheduled jobs").
var (
	englishBeForms    = toWordSet("be", "is", "are", "being", "isn't", "aren't", "get", "gets", "getting")
	englishNotPassive = toWordSet("been", "was", "were", "has", "have", "had", "did", "do", "does", "wasn't", "weren't", "hasn't", "haven't", "hadn't", "didn't")
	englishSubjects   = toWordSet("i", "we", "you", "they", "he", "she", "it", "someone", "somebody", "anyone", "anybody", "everyone", "everybody", "nobody", "who")
	englishWhSubjects = toWordSet("what", "which", "whose", "that")
	// englishDeterminers start a noun phrase; behind a word that is no
	// englishNPJoiners (a preposition, a conjunction or a determiner) it
	// starts the subject of a relative clause without its pronoun
	// (englishPassiveAt).
	englishDeterminers = toWordSet("the", "a", "an", "this", "these", "those", "my", "our", "your", "their", "his", "her", "its")
	englishNPJoiners   = toWordSet("of", "on", "at", "in", "for", "from", "to", "by", "with", "about", "into", "during",
		"after", "before", "since", "until", "over", "under", "and", "or", "the", "a", "an", "this", "that", "these", "those",
		"my", "our", "your", "their", "his", "her", "its", "all", "both", "what", "which", "whose")
	// englishClauseOpeners open a relative or embedded clause, which ends
	// the search of englishPassiveAt.
	englishClauseOpeners = toWordSet("that", "which", "who", "whom", "whose", "where", "when", "whether", "if", "because")
	englishAfterFinite   = toWordSet("the", "a", "an", "this", "that", "these", "those", "my", "our", "your", "their", "his",
		"her", "its", "some", "any", "all", "no", "it", "them", "him", "me", "us", "you", "on", "at", "in", "to", "from",
		"by", "for", "with", "into", "during", "after", "before", "since", "until", "over", "up", "down", "out", "off",
		"back", "again", "yesterday", "last", "today", "there", "here", "when", "while", "because", "and", "or", "but")
)

// englishPassiveAt reports whether words[i], an English past form, is the
// participle of a passive or a state (issue #506): the nearest auxiliary
// before it is a form of "be" — "What will be deployed on Wednesday?", "What
// must be finished by Wednesday?", "Is the meeting on Wednesday cancelled?".
// A perfect, a past or another verb before it ends the search, and so does
// the clause the form stands in: the opener of a relative or embedded
// clause ("What is the name of the job that failed on Friday?") or its
// subject ("Is this the PR you merged on Tuesday?"), unless a form of "be"
// right before the subject asks the question ("Is it planned?"), and a
// determiner right behind a noun, which starts a relative clause without its
// pronoun ("What is the reason the job crashed on Saturday?"; "Is the build
// of the service deployed?" joins its noun phrase by "of").
func englishPassiveAt(words []string, i int) bool {
	joined, shared := false, false
	for j := i - 1; j >= 0; j-- {
		w := words[j]
		switch {
		case englishBeForms[w]:
			scanned(i - j)
			return true
		case w == "and" || w == "or":
			joined = true
		case (pastVerbSet[w] || englishParticiple(w)) && joined && !shared:
			// A participle coordinated with this one shares its auxiliary,
			// with or without words between: "Will it be fixed and deployed
			// on Monday?" (issue #517), "Will it be fixed on Monday and
			// deployed on Tuesday?" (issue #532). One is passed, so that a
			// chain of them is read in time linear in its length.
			shared = true
		case englishNotPassive[w] || pastVerbSet[w] || englishParticiple(w) || englishClauseOpeners[w],
			englishSubjects[w] && w != "who" && (j == 0 || !englishBeForms[words[j-1]]),
			englishDeterminers[w] && j > 0 && !englishBeForms[words[j-1]] && !englishNPJoiners[words[j-1]]:
			scanned(i - j)
			return false
		}
	}
	scanned(i)
	return false
}

// englishFinitePast reports whether words[i] is an English verb on -ed
// (englishParticiple) in the place of the simple past (issue #506): right
// behind a subject pronoun or "who" ("Who committed …", "we discussed",
// "someone wiped"), or behind "what"/"which"/"whose", with at most one word
// between, before the end of the words or a word a noun cannot be ("which
// commits landed on Monday", "what caused it").
func englishFinitePast(words []string, i int) bool {
	if i == 0 || !englishParticiple(words[i]) && !englishIrregularPast[words[i]] || transitiveFell(words, i) {
		return false
	}
	if englishSubjects[words[i-1]] {
		return true
	}
	if !englishWhSubjects[words[i-1]] && (i < 2 || !englishWhSubjects[words[i-2]]) {
		return false
	}
	return beforeNoNoun(words, i)
}

// barePast reports whether word k of c, in the clause [s, e), is an English
// simple past behind a bare subject (issue #532): a form on -ed or of
// englishIrregularPast behind a noun phrase without a pronoun that opens the
// clause (englishBareSubject: "The release slipped on Thursday", "Latency
// spiked on Tuesday", "Mrs. Friday called", "tuesday merged") before a word no
// noun can be — not a participle of an arrangement, not before a word that
// names where it moves a thing, and not with a finite verb behind it in the
// clause, which makes it the participle of its subject ("Tickets closed on
// Friday will be reopened on Monday"). A clause that asks with nothing but a
// date behind the form ("Office closed on Monday?") asks for a state as
// often, and stays unread.
func (c clauseWords) barePast(s, k, e int) bool {
	words := c.words[s:e]
	i := k - s
	if i <= 0 || !englishParticiple(words[i]) && !englishIrregularPast[words[i]] || !englishBareSubject(words, i) ||
		!beforeNoNoun(words, i) && !c.nameAt(k+1) || englishPlanForms[words[i]] || i+1 < len(words) && englishTargetWords[words[i+1]] ||
		englishReports(words, i, true) || finiteAfter(words, i) || c.nameAt(k) || transitiveFell(words, i) {
		return false
	}
	return !c.asks[k] || !datePhraseAfter(words, i)
}

// nameAt reports whether word i of c is written with its capital inside its
// clause, where English writes a name and no verb: "Meet Drew at the office
// on Monday" has no simple past, "The pager woke Anna on Thursday" a verb
// before a name (barePast, issue #532). A question that capitalizes most of
// its words has no such capitals (caps).
func (c clauseWords) nameAt(i int) bool {
	return i > 0 && i < len(c.words) && c.caps[i] && !c.starts[i]
}

// datePhraseAfter reports whether the words behind words[i] are a phrase of
// a date and nothing else: a date word or a day keyword with prepositions,
// determiners, parts of the day and words that place a day ("on Monday",
// "last Friday night"). Without words behind it the date, if any, is the
// subject's ("wednesday reverted?").
func datePhraseAfter(words []string, i int) bool {
	scanned(len(words) - i)
	date := false
	for _, w := range words[i+1:] {
		_, part := dayPartHours[w]
		d := dateWord(w) || dayKeywords[w]
		if !datePhraseWords[w] && !d && !part {
			return false
		}
		date = date || d
	}
	return date
}

// datePhraseWords are the words of a phrase of a date besides the date word
// (datePhraseAfter).
var datePhraseWords = toWordSet("on", "at", "in", "by", "during", "the", "this", "last", "next", "early", "late")

// barePasts counts the words of [s, e) of c that are a simple past behind a
// bare subject (barePast) and that pastVerbs does not count, each read in its
// clause.
func (c clauseWords) barePasts(s, e int) int {
	if !c.pastForms {
		return 0
	}
	scanned(e - s)
	n := 0
	for k := s; k < e; k++ {
		if w := c.words[k]; englishPastVerbs[w] || !englishParticiple(w) && !englishIrregularPast[w] {
			continue
		}
		cs, ce := c.clauseOf(k)
		cs, ce = max(cs, s), min(ce, e)
		if c.barePast(cs, k, ce) && !englishFinitePast(c.words[cs:ce], k-cs) && !englishPassiveAt(c.words[cs:ce], k-cs) {
			n++
		}
	}
	return n
}

// englishPastIn reports whether the clause [s, e) of c, in a sentence of the
// language lang that is not German, has an English simple past the rules read
// by its place (englishFinitePast): a verb of its own (issue #532), "Usage
// dropped on Saturday, is that normal?".
func (c clauseWords) englishPastIn(s, e, lang int) bool {
	if lang == langGerman || !c.pastForms {
		return false
	}
	scanned(e - s)
	words := c.words[s:e]
	for k := range words {
		if englishFinitePast(words, k) && !englishReports(words, k, false) || c.barePast(s, s+k, e) {
			return true
		}
	}
	return false
}

// englishPresentIn reports whether the clause [s, e) of c, in a sentence of
// the language lang that is not German, has an English present of the third
// person behind its subject (englishPresentAt): a verb of its own that the
// rules read as no tense, so that the clause keeps its own rather than
// taking the question's (issue #532, "The cluster died on Saturday; the
// vendor comes on Tuesday").
func (c clauseWords) englishPresentIn(s, e, lang int) bool {
	if lang == langGerman {
		return false
	}
	scanned(e - s)
	for k := s; k < e; k++ {
		if c.englishPresentAt(s, k, e) {
			return !c.ellipticTail(k+1, e)
		}
	}
	return false
}

// ellipsisTails are the words that, behind the date of a clause, make it
// one that repeats what the clause before it says ("…, the error rates on
// Monday too?", "…, the test runs on Monday all green?"): a plural noun
// there has the shape of a present (englishPresentAt), issue #532.
var ellipsisTails = toWordSet("too", "also", "either", "as", "all", "still")

// ellipticTail reports whether a word of ellipsisTails follows the first
// date word of the words [s, e) of c.
func (c clauseWords) ellipticTail(s, e int) bool {
	date := false
	for i := s; i < e; i++ {
		w := c.words[i]
		if date && ellipsisTails[w] {
			return true
		}
		date = date || dateWord(w) || dayKeywords[w]
	}
	return false
}

// englishThirdPersons are the pronouns a present of the third person
// follows; englishAfterPresent the words besides englishAfterFinite and a
// date that follow one ("goes live", "comes online", "takes place");
// englishNounsOnS the words on -s a noun phrase ends with that are no plural
// and no verb.
var (
	englishThirdPersons = toWordSet("he", "she", "it", "this", "that", "everyone", "everybody", "someone", "somebody",
		"nobody", "anyone", "anybody", "one")
	englishAfterPresent = toWordSet("live", "online", "place", "effect", "next", "early", "late", "tomorrow", "tonight",
		"ahead", "soon")
	englishNounsOnS = toWordSet("news", "series", "chaos", "canvas", "atlas", "alias", "bias", "lens", "perhaps",
		"always", "sometimes", "towards", "afterwards", "besides", "whereas", "thanks", "kubernetes", "postgres",
		"jenkins", "devops", "ops", "sales", "means", "headquarters")
)

// englishPresentAt reports whether word k of c, in the words [s, e), is an
// English present of the third person on -s behind its subject (issue
// #532): a pronoun of englishThirdPersons, or a noun phrase of one to three
// words behind a determiner ("the vendor comes", "her successor starts", "On
// Thursday the team presents") or without one (bareSubjectVerb: "Billing
// goes live", "marketing launches the campaign"), before the end of the
// words, a date or a word no noun follows (englishAfterFinite,
// englishAfterPresent). A plural noun of a noun phrase has the same shape
// ("the release notes on Friday"); the clause it reads then keeps a tense of
// its own, neutral, where it took the question's.
func (c clauseWords) englishPresentAt(s, k, e int) bool {
	words := c.words[s:e]
	k -= s
	if k == 0 || !presentShape(words[k]) {
		return false
	}
	if next := k + 1; next < len(words) && !englishAfterFinite[words[next]] && !englishAfterPresent[words[next]] && !dateWord(words[next]) {
		return false
	}
	if englishThirdPersons[words[k-1]] {
		return true
	}
	n := 0
	for j := k - 1; j >= 0 && n < 3; j-- {
		x := words[j]
		switch {
		case englishDeterminers[x] || attributeOpeners[x]:
			return n > 0
		case dateWord(x) || dayKeywords[x]:
			return n > 0 && c.bareSubjectVerb(s+j+1, s+k)
		case !subjectNoun(x):
			return false
		}
		n++
		if j == 0 {
			return c.bareSubjectVerb(s, s+k)
		}
	}
	return false
}

// presentShape reports whether w has the shape of an English present of the
// third person: four letters or more on -s, not on -ss, -us, -is or 's, and
// no word of englishNounsOnS.
func presentShape(w string) bool {
	return len(w) >= 4 && strings.HasSuffix(w, "s") && !englishNounsOnS[w] && !strings.HasSuffix(w, "ss") &&
		!strings.HasSuffix(w, "us") && !strings.HasSuffix(w, "is") && !strings.HasSuffix(w, "'s")
}

// subjectNoun reports whether x may be a word of the noun phrase of a subject
// (englishPresentAt): no function word, auxiliary, pronoun, opener, past or
// adverb on -ly.
func subjectNoun(x string) bool {
	return !englishNPJoiners[x] && !englishFiniteWords[x] && !englishSubjects[x] && !futureVerbSet[x] && !englishBeForms[x] &&
		!englishEmbedOpeners[x] && !englishPastVerbs[x] && !hortatives[x] && x != "not" && x != "please" &&
		!strings.HasSuffix(x, "ly")
}

// bareSubjectVerb reports whether the noun phrase without a determiner from
// word f of c up to word k, a form on -s, is the subject of that form
// (englishPresentAt): its first word is written with its capital, a name or
// the start of a sentence ("Anna starts on Friday", "Billing goes live"), or
// an object follows the form ("marketing launches the campaign"). Without
// either the words are a noun of two words as often ("…, check runs on Friday
// as well?").
func (c clauseWords) bareSubjectVerb(f, k int) bool {
	return c.written[f] || k+1 < len(c.words) && imperativeObjects[c.words[k+1]]
}

// finiteAfter reports whether a finite word of English follows words[i]: the
// form behind a bare subject is then the participle of a noun phrase without
// its pronoun ("Tickets closed on Friday will be reopened on Monday"), the
// finite verb the later word (issue #532).
func finiteAfter(words []string, i int) bool {
	scanned(len(words) - i)
	for _, w := range words[i+1:] {
		if englishFiniteWords[w] || w == "get" || w == "gets" {
			return true
		}
	}
	return false
}

// englishPlanForms are the participles of an arrangement, which name the
// date of what is arranged rather than a past ("Deploy scheduled for
// Friday?", "Release postponed to Monday"), and englishTargetWords the words
// behind a verb that name where it moves a thing ("slipped to Friday"):
// behind a bare subject neither makes a simple past (issue #532).
var (
	englishPlanForms = toWordSet("scheduled", "planned", "booked", "expected", "slated", "postponed", "rescheduled",
		"delayed", "moved", "pushed", "shifted", "deferred", "reserved", "arranged", "proposed", "set")
	englishTargetWords = toWordSet("to", "for", "until", "till", "into")
)

// englishBareSubject reports whether the words before words[i] are a noun
// phrase without a pronoun that opens the clause, the subject of a simple
// past at words[i] (issue #532): one to three words that are no function
// word, auxiliary, pronoun, opener or verb form, behind an optional
// determiner — "The release slipped", "Our CI timed out", "Latency spiked",
// "Mrs. Friday called", "tuesday merged". A determiner right before the form
// makes it an attribute ("the failed jobs"), which no word between leaves.
func englishBareSubject(words []string, i int) bool {
	j := 0
	// A quantifier opens the noun phrase as a determiner does: "Any thought
	// on Friday?" is no simple past (issue #532).
	if englishDeterminers[words[0]] || attributeOpeners[words[0]] {
		j = 1
	}
	if n := i - j; n < 1 || n > 3 {
		return false
	}
	for k := j; k < i; k++ {
		w := words[k]
		if englishNPJoiners[w] || englishFiniteWords[w] || englishSubjects[w] || englishParticiple(w) ||
			englishIrregularPast[w] || futureVerbSet[w] || englishBeForms[w] || englishEmbedOpeners[w] || hortatives[w] ||
			w == "not" || w == "please" {
			return false
		}
	}
	return true
}

// attributeOpeners are the words before an attribute of a noun phrase
// besides the determiners and interrogatives (englishAttributive).
var attributeOpeners = toWordSet("all", "some", "any", "no", "both", "each", "every")

// englishAttributive reports whether words[i], an English form on -ed, is an
// attribute between the word that opens a noun phrase and its noun ("the
// failed jobs", "Which updated docs are due on Thursday?"), no past of its
// own (issue #532).
func englishAttributive(words []string, i int) bool {
	if i == 0 || !englishParticiple(words[i]) {
		return false
	}
	prev := words[i-1]
	// "that" behind a noun is the pronoun of a relative clause and the form
	// its verb: "The patch that crashed prod on Saturday …" (issue #532).
	if prev == "that" && i >= 2 && !englishFiniteWords[words[i-2]] && !englishNPJoiners[words[i-2]] &&
		!englishSubjects[words[i-2]] && !englishParticiple(words[i-2]) {
		return false
	}
	return (englishDeterminers[prev] || englishWhSubjects[prev] || attributeOpeners[prev]) && !beforeNoNoun(words, i)
}

// beforeNoNoun reports whether the word behind words[i] is none a noun can
// be, so that words[i] is a verb and no attribute of it: the end of the
// words, a word of englishAfterFinite, an adverb on -ly, or a date word
// ("Which alerts fired Thursday night?", issue #532).
func beforeNoNoun(words []string, i int) bool {
	if i+1 == len(words) {
		return true
	}
	next := words[i+1]
	return englishAfterFinite[next] || englishAfterVerb[next] || strings.HasSuffix(next, "ly") || dateWord(next)
}

// englishAfterVerb are adverbs that follow a verb and no noun ("fired twice
// on Monday", "left early on Friday"), and the pronouns that are its object
// ("The pager woke everyone on Sunday"), issue #532.
var englishAfterVerb = toWordSet("twice", "once", "early", "late", "overnight", "everyone", "everybody", "someone",
	"somebody", "anyone", "anybody", "nobody")

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
	"stieß", "stießen", "hing", "hingen",
}

// presentTForms are present forms of German verbs whose stem ends in -t,
// which take the shape of the weak simple past ("Was testen wir am Montag?",
// "Was kosten die Server?"); germanPastShape reads them as present.
var presentTForms = []string{
	"warten", "warte", "halten", "halte", "starten", "starte", "kosten", "koste", "testen", "teste", "leisten",
	"leiste", "richten", "richte", "arbeiten", "arbeite", "gelten", "schalten", "schalte",
	"gestalten", "verwalten", "raten", "rate", "bitten", "bitte", "fürchten", "fürchte", "melden", "bieten", "biete",
	"treten", "trete", "posten", "poste", "hosten", "hoste", "listen", "liste", "mieten", "miete", "beten",
	// The present of verbs on -werten and of the verbs of attention on
	// -achten whose simple past has no form of "machen" or "denken" in it,
	// read where a finite verb stands (finitePast, issue #506).
	"werten", "werte", "beachten", "beachte", "betrachten", "betrachte", "beobachten", "beobachte",
	"lauten", "laute", "antworten", "antworte",
	// The present of verbs whose stem ends in -t after a consonant, which
	// no weak verb without that -t has for its simple past (issue #532):
	// "punkten" is no "punk-ten", while "rasten" is the past of "rasen" and
	// stays out. Their simple past ends in -eten ("punkteten").
	"punkten", "punkte", "lasten", "laste", "sichten", "schichten", "dichten", "lüften", "rüsten", "fasten", "husten",
	"heften", "haften", "spalten", "falten", "texten", "casten", "boosten", "shiften", "liften", "driften", "tasten",
	"stiften", "schuften", "fristen", "rosten", "pachten", "fechten", "flechten", "leuchten",
}

// germanPastShape reports whether w has the shape of the German simple
// past: a strong form of strongPastForms at its end, or the weak ending
// -te/-ten — after a stem in -t or -d as -ete/-eten ("testete") — but not a
// present form of a verb whose stem ends in -t (presentTForms, and the forms
// whose stem shows it, tStemPresent) and not the ending -tet, which is the
// present of such a verb ("kostet").
func germanPastShape(w string) bool {
	for _, f := range strongPastForms {
		if strings.HasSuffix(w, f) {
			return true
		}
	}
	return weakPastShape(w)
}

// weakPastShape is germanPastShape without the strong forms: the weak
// ending -te/-ten (-ete/-eten behind a stem on -t or -d) of a verb that is
// not the present of a stem on -t.
func weakPastShape(w string) bool {
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
	base, ok := strings.CutSuffix(w, "ten")
	if !ok {
		if base, ok = strings.CutSuffix(w, "te"); !ok {
			return false
		}
	}
	return !tStemPresent(w, base)
}

// presentTWords are present forms of verbs on -t that end like the simple
// past of a verb whose name is their own tail ("achten" ends like
// "machten"): read as words, not as endings.
var presentTWords = toWordSet("achten", "achte")

// tStemPresent reports whether w, a form on -te or -ten whose part before
// that ending is base, is the present of a verb whose stem ends in -t
// (issue #506): the simple past of such a verb ends in -tete, so the form is
// none. Its shape shows it where the stem's -t follows a doubled -t
// ("chatten", "splitten", "retten"; "hatte" is the past of "haben") or a
// single vowel ("updaten", "rebooten", "routen", "löten", "bereiten",
// "leiten" — no weak verb puts its past ending right behind a, i, o, u, ä, ö
// or ü, while au, eu and äu do: "bauten", "freuten"), and a few verbs whose
// stem the shape does not show are words of presentTWords. A stem on -ie is
// not read ("bieten" is the present, "kniete" the past of "knien"):
// presentTForms holds "bieten" and "mieten".
func tStemPresent(w, base string) bool {
	if presentTWords[w] {
		return true
	}
	if strings.HasSuffix(base, "t") && !strings.HasPrefix(w, "hat") {
		return true
	}
	r, size := utf8.DecodeLastRuneInString(base)
	if !strings.ContainsRune("aiouäöü", r) {
		return false
	}
	prev, _ := utf8.DecodeLastRuneInString(base[:len(base)-size])
	return r != 'u' || !strings.ContainsRune("aeä", prev)
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
	if clauseOpeners[c.words[s]] || hortatives[c.words[s]] || c.imperativeAt(s, e) {
		return true
	}
	for i := s; i < e; i++ {
		w := c.words[i]
		if subjectWords[w] || copulaWords[w] || pastVerbSet[w] || futureVerbSet[w] || perfektAux[w] || germanFiniteWords[w] ||
			w == "zu" && i+1 < e {
			return true
		}
	}
	return c.participleAt(s, e-1) || c.relativeClause(s, e) && c.verbLastPastAt(e-1, e)
}

// The words of an interrogative without a verb (whEllipsis): the
// conjunctions that may open it, the English interrogatives, the English
// prepositions that may follow the interrogative.
var (
	ellipsisConjunctions  = toWordSet("und", "oder", "aber", "and", "or", "but")
	englishInterrogatives = toWordSet("what", "which", "who", "whom", "whose", "when", "where", "why", "how")
	ellipsisPrepositions  = toWordSet("on", "at", "in", "for", "from", "by", "during", "until", "since", "before", "after",
		"about", "für")
)

// whEllipsis reports whether the clause [s, e) of c, in a sentence of the
// language lang, is an interrogative without a verb of its own (issue #532): "Was war am Montag, was am
// Dienstag?", "What failed on Friday and what on Monday?". A conjunction may
// open it; behind the interrogative (and its particles) stands a preposition,
// a date word or a day keyword, the place a German main clause puts its verb
// and an English one its subject; and no word of it is one the rules read as
// a verb — a finite verb, a subject pronoun, an English form on -ed, "zu" —
// and its last word names a day, a part of the day or, written with its
// capital, a thing, not the verb a clause opened by an interrogative may put
// last ("was am Freitag schiefging", "was am Montag passiert") or the
// separable particle a verb leaves there ("und was am Freitag vor?").
func (c clauseWords) whEllipsis(s, e, lang int) bool {
	if s < e && ellipsisConjunctions[c.words[s]] {
		s++
	}
	if s+1 >= e || !germanInterrogatives[c.words[s]] && !englishInterrogatives[c.words[s]] {
		return false
	}
	v := s + 1
	for v < e && wasParticles[c.words[v]] {
		v++
	}
	if v >= e {
		return false
	}
	if w := c.words[v]; !firstPrepositions[w] && !ellipsisPrepositions[w] && !dateWord(w) && !dayKeywords[w] {
		return false
	}
	if c.verbIn(v+1, e, lang) {
		return false
	}
	// The last word names the day, its part, or a thing (a noun, written
	// with its capital): any other word may be the verb a clause opened by
	// an interrogative puts last ("Ich habe vergessen, was am Montag
	// ansteht").
	last := c.words[e-1]
	_, part := dayPartHours[last]
	return dateWord(last) || dayKeywords[last] || part || c.caps[e-1]
}

// verbIn reports whether a word of [s, e) of c, in a sentence of the
// language lang, is one the rules read as a verb (whEllipsis): a finite verb,
// a subject pronoun, "zu", an English past or form on -ed.
func (c clauseWords) verbIn(s, e, lang int) bool {
	scanned(e - s)
	for i := s; i < e; i++ {
		w := c.words[i]
		if finiteWord(w, lang) || subjectWords[w] && (lang == langMixed || !bothLanguageWords[w]) ||
			lang != langEnglish && w == "zu" || lang != langGerman && (englishPastVerbs[w] || englishParticiple(w)) {
			return true
		}
	}
	return false
}

// hortatives are the imperative of "lassen" that opens a German clause of a
// proposal ("Lass uns am Freitag reden", "Lasst uns …", "Lassen Sie uns
// …"): the clause has a verb of its own, the imperative, which names no past
// (issue #517). Without it the clause took the tense of the whole question —
// in "Lass uns am Freitag über den Bug reden, den wir am Montag hatten" the
// past of the relative clause.
var hortatives = toWordSet("lass", "lasst", "lassen", "let's", "let\u2019s")

// imperatives are the imperatives of the verbs a request to note, check or
// send something opens its clause with ("Erinnere mich am Freitag an den Bug,
// den wir am Montag hatten", "Schau dir am Montag an, was am Freitag
// kaputtging"): like the imperative of "lassen" (hortatives) the clause has a
// verb of its own that names no past (issue #532). A list, not the shape: any
// word before "mal", "bitte" or a pronoun reads adverbs and simple pasts as
// imperatives ("Wieder mal am Freitag, …", "Lief am Montag mal was schief?").
var imperatives = toWordSet("schau", "schaue", "guck", "sieh", "schick", "schicke", "sende", "sag", "sage", "ruf", "rufe",
	"gib", "prüf", "prüfe", "überprüf", "überprüfe", "frag", "frage", "check", "checke", "schreib", "schreibe",
	"erinnere", "erinner", "melde", "meld", "kümmere", "kümmer", "bring", "bringe", "leg", "lege", "lies", "nimm",
	"denk", "denke", "merk", "merke", "teste", "hilf", "zeig", "zeige", "hol", "hole", "such", "suche", "notier",
	"notiere", "informier", "informiere", "kontrollier", "kontrolliere", "stell", "stelle", "setz", "setze", "trag", "trage")

// englishImperatives are the words of imperatives that are the English
// imperative of the same verb as well; imperativeObjects the words that
// make one of them an imperative in an English sentence, its object ("check
// the logs on Monday", "bring them on Friday"), issue #532.
var (
	englishImperatives = toWordSet("bring", "check")
	imperativeObjects  = toWordSet("the", "a", "an", "this", "that", "these", "those", "my", "our", "your", "their",
		"his", "her", "its", "it", "them", "me", "us", "him", "everything", "all")
)

// imperativeAt reports whether word s of c, the first word of the clause [s,
// e), is an imperative of imperatives where it can be one (issue #532): the
// forms are German, and most are an English word as well ("such", "sage",
// "check", "hole", "leg", "lies", "sag") or, written with their capital, a
// German noun ("Suche", "Frage", "Schau", "Stelle", "Trage", "Rufe"). So a
// form counts in a clause that is not English, not written with its
// capital inside a sentence ("…, Suche am Montag auch kaputt?"), not in "sage
// und schreibe", and not where another verb of its clause is the finite one
// ("Check am Montag ergab nichts"); in an English clause only "bring" and
// "check" do, at the start of the question or before their object ("Check
// on Friday whether …", "…, check the logs on Monday").
func (c clauseWords) imperativeAt(s, e int) bool {
	w := c.words[s]
	if !imperatives[w] || s > 0 && !c.sentence[s] && c.written[s] {
		return false
	}
	if w == "sage" && s+2 < e && c.words[s+1] == "und" && c.words[s+2] == "schreibe" {
		return false
	}
	// The language of the clause, or of the question where the clause has
	// no word of one: read per clause, a sentence of many such clauses is
	// read in time linear in its length.
	lang := c.languageOf(s, e)
	if lang == langMixed {
		lang = c.lang
	}
	if lang == langEnglish {
		return englishImperatives[w] && (s == 0 || s+1 < e && imperativeObjects[c.words[s+1]])
	}
	return c.germanImperativeAt(s, e)
}

// imperativeSigns are the words that make a German form of imperatives the
// imperative where its case does not tell it from a noun (at the start of a
// sentence, in a question written in lower case): its object, a pronoun or
// an article ("Suche am Montag die Mail heraus", "Schau dir …"), or "bitte",
// "mal", "doch"; imperativeParticles the separable particles a request
// leaves at the end of its clause ("Trag am Freitag ein, …", "Frag am Montag
// nach, …"), issue #532.
var (
	imperativeSigns = toWordSet("mir", "mich", "dir", "dich", "uns", "euch", "ihm", "ihn", "ihnen", "es", "sie",
		"der", "die", "das", "den", "dem", "des", "ein", "eine", "einen", "einem", "einer", "eines",
		"mein", "meine", "meinen", "meinem", "meiner", "dein", "deine", "deinen", "deinem", "deiner",
		"unser", "unsere", "unseren", "unserem", "unserer", "euer", "eure", "euren", "eurem", "seine", "seinen",
		"seinem", "ihre", "ihren", "ihrem", "bitte", "mal", "doch")
	imperativeParticles = toWordSet("an", "nach", "ein", "aus", "mit", "zurück", "heraus", "raus", "herein", "rein",
		"zusammen", "vor", "fest", "durch", "auf", "ab", "zu", "weg", "hoch", "runter", "sicher")
)

// germanImperativeAt is imperativeAt in a clause that is not English: a
// form there is the imperative where no other verb of the clause is the
// finite one ("Check am Montag ergab nichts"; a word written with its
// capital is a noun, "Bestand", not "bestand") and where something says so —
// a word of imperativeSigns, a particle of imperativeParticles at the end of
// the clause, or a clause behind it in its sentence opened by a conjunction
// or an interrogative ("Prüf am Freitag, ob …"). Without one the form is a
// noun as often ("Am Freitag fiel der Login aus. Suche am Montag
// ebenfalls?"), issue #532.
func (c clauseWords) germanImperativeAt(s, e int) bool {
	scanned(e - s)
	sign := false
	for i := s + 1; i < e; i++ {
		x := c.words[i]
		if (germanFiniteVerb(x) || strongPastWord(x)) && !c.written[i] {
			return false
		}
		sign = sign || imperativeSigns[x]
	}
	if sign || imperativeParticles[c.words[e-1]] {
		return true
	}
	return e < len(c.words) && !c.sentence[e] && (germanSubordinators[c.words[e]] || germanInterrogatives[c.words[e]])
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
		// The loan verbs of a failure, which take "sein" as "scheitern"
		// does ("ist gefailed", "sind geflaked"), issue #506.
		"failed", "failt", "flaked", "flakt", "crashed",
	}
	seinParticiples = toWordSet(
		"passiert", "eskaliert", "explodiert", "kollabiert", "gewesen", "geworden", "gefahren",
		"gereist", "verreist", "abgereist", "gelandet", "gestartet", "gefolgt", "begegnet",
		"entstanden", "aufgestanden", "eingeschlafen", "aufgewacht", "umgezogen", "fehlgeschlagen",
		"gekippt", "abgebrochen", "zusammengebrochen", "eingebrochen", "ausgebrochen", "worden", "abgeschmiert",
		"gewechselt",
		// "brechen" without an object takes "sein": "Der Build ist am
		// Dienstag gebrochen" reports what broke (issue #532).
		"gebrochen",
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
// "geöffnet", which with "haben" asks for opening hours ("Hat das Büro am
// Samstag geöffnet?"), and the nouns of that shape that are the participle of
// no verb (issue #506): written in lower case they lose the only mark of a
// noun the form has ("haben wir am montag verlust"), so which word is a
// participle only the word tells.
var participleExceptions = toWordSet("bereit", "bestimmt", "beliebt", "berühmt", "gesamt", "verrückt", "verwandt", "geöffnet", "gestern", "genau",
	"verlust", "bericht", "gericht", "gewicht", "gesicht", "geschäft", "gebiet", "gerät", "gehalt", "verbot", "gebot",
	"verdienst", "beirat", "geist")

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
	closing := c.closing
	if closing == nil {
		closing = new([]int)
	}
	for cs := s; cs < e; {
		_, ce := c.clauseOf(cs)
		if ce > e {
			ce = e
		}
		if c.clausePerfekt(cs, ce, closing) {
			return true
		}
		cs = ce
	}
	return false
}

// clausePerfekt is hasPerfektForm for the one clause [s, e); an auxiliary
// whose clause does not end with its participle may find it at the end of a
// later clause of the sentence (closingParticiples, a table the first
// clause of the question that needs it fills).
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
			// The exceptions hold for the pattern as for the list: "haben wir
			// am montag gehalt auf dem konto?" has no participle (issue #517).
			if w := c.words[j]; (knownParticiples[w] || perfektParticiplePat.MatchString(w) || ierenParticiple(w)) && !participleExceptions[w] {
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
// other clause with an auxiliary or another finite verb the rules know
// ends the search ("…, und was ist geplant?", "…, oder wird da
// gearbeitet?": the participle is that clause's own, issue #506). The entry of a clause start is the start found, -1 for none.
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
			w := c.words[k]
			aux = aux || perfektAux[w] || germanFiniteWords[w] || germanPastVerbs[w]
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

// germanFiniteWords are the finite verbs of a German clause the rules know
// beside germanPastVerbs and perfektAux: the forms of "sein", "werden" and
// the modals, and the present of the verbs a question asks with.
var germanFiniteWords = toWordSet(
	"bin", "bist", "ist", "sind", "seid", "wird", "wirst", "werde", "werden", "werdet", "wurde", "wurden",
	"muss", "musst", "müssen", "soll", "sollst", "sollen", "kann", "kannst", "können", "darf", "darfst", "dürfen",
	"mag", "will", "willst", "wollen", "möchte", "möchten", "gibt", "gilt", "steht", "kommt", "läuft", "geht",
	"fehlt", "bleibt", "liegt", "braucht", "brauchen", "kriegt", "kriegen", "bekommt",
)

// indefiniteWords are the pronouns that make a clause active: an object
// ("was" for "etwas", "nichts") or a subject ("jemand", "keiner") of the verb
// whose participle ends the clause.
var indefiniteWords = toWordSet("was", "etwas", "irgendwas", "nichts", "nix", "jemand", "irgendwer", "irgendjemand", "niemand", "keiner")

// planParticiples are the participles of the verbs that make a plan: their
// Perfekt names the date of what is planned, which lies ahead ("am Freitag
// was geplant?"), and which of the two a question means only the verb tells,
// not the form of the clause.
var planParticiples = toWordSet("geplant", "eingeplant", "vorgesehen", "angesetzt", "terminiert", "vorgemerkt", "angekündigt", "angedacht",
	"bestellt", "eingeteilt", "reserviert", "gebucht", "eingetragen", "angemeldet")

// ellipticPerfekt reports a German Perfekt without its auxiliary in a clause
// of [s, e) (issue #506): spoken German drops "haben" or "sein" ("am Freitag
// was kaputt gemacht?", "Server am Freitag abgestürzt?"). A clause counts
// when it has no finite verb the rules know, ends with a participle and the
// auxiliary it dropped can only be that of a Perfekt:
//   - the participle is one of a verb that takes "sein" (seinParticiple),
//     whose state no passive names, or
//   - an indefinite pronoun stands before it (indefiniteWords; "was" not as
//     the interrogative that opens the clause): the clause is active, its
//     auxiliary "haben" — and the participle is no plan's, and no date in
//     the clause stands after "für", which names what is arranged for it.
//
// A clause with a subject pronoun right behind its first word has a finite
// verb there the rules do not know ("Kriegen wir am Montag was gefixt?"),
// and a participle right behind an opening interrogative is the present
// ("Was passiert, wenn …?").
// Any other clause ending with a participle ("Montag Deployment geplant",
// "Freitag Release freigegeben?", "Bug am Montag gefixt?") may as well be
// the state passive or a plan with "ist" dropped: the form cannot tell, and
// where it cannot the tense stays neutral. Either wrong reading moves the
// date by a week; the superseded filter is off for every question a rule
// answers, whatever its tense (query.go Step 6d).
func (c clauseWords) ellipticPerfekt(s, e int) bool {
	for cs := s; cs < e; {
		_, ce := c.clauseOf(cs)
		if ce > e {
			ce = e
		}
		if c.clauseEllipsis(cs, ce) {
			return true
		}
		cs = ce
	}
	return false
}

// clauseEllipsis is ellipticPerfekt for the one clause [s, e).
func (c clauseWords) clauseEllipsis(s, e int) bool {
	last := e - 1
	if last <= s || !c.participleAt(s, last) || last > s+1 && subjectWords[c.words[s+1]] {
		return false
	}
	// Right behind an opening interrogative the form is the finite verb, the
	// present of a verb on -ieren or -t ("Was passiert, wenn …?").
	if clauseOpeners[c.words[s]] {
		v := s + 1
		for v < last && wasParticles[c.words[v]] {
			v++
		}
		if v == last {
			return false
		}
	}
	proof := seinParticiple(c.words[last]) || c.pairedEllipsis(s, e)
	for i := s; i < last; i++ {
		w := c.words[i]
		if germanPastVerbs[w] || perfektAux[w] || germanFiniteWords[w] {
			return false
		}
		// A date after "für" is what something is arranged for, not when
		// it happened ("Für Dienstag was reserviert?").
		if (w == "für" || w == "for") && i+1 < last && dateWord(c.words[i+1]) {
			return false
		}
		if indefiniteWords[w] && (i > s || w != "was") && !planParticiples[c.words[last]] {
			proof = true
		}
	}
	return proof
}

// stateParticiples are participles that name an arrangement or a state as
// often as an act (pairedEllipsis): "Montag Büro geschlossen, Dienstag
// Homeoffice angeordnet". The second line holds those a review of the wave
// found missing ("Am Freitag anberaumt, am Montag festgelegt?") and the
// participles of arranging and of closing off a place found beside them
// (issue #532); the list is open: a participle of an arrangement it does not
// hold reads as an act.
var stateParticiples = toWordSet("geschlossen", "geöffnet", "gesperrt", "freigegeben", "angeordnet", "vereinbart",
	"eingeschränkt", "ausgebucht", "besetzt", "ausgelastet", "ausverkauft", "abgesagt", "verschoben", "aufgehoben",
	"eingefroren", "genehmigt", "organisiert", "freigeschaltet", "gedrosselt", "angesagt", "zugesagt", "bestätigt",
	"anberaumt", "festgelegt", "festgesetzt", "verabredet", "avisiert", "anvisiert", "eingetaktet", "fixiert",
	"ausgemacht", "abgesprochen", "abgestimmt", "einberufen", "disponiert", "verplant", "geblockt",
	"ausgeschrieben", "zugeteilt", "abgesperrt", "vermietet", "belegt", "verriegelt")

// reasonWords ask for a reason.
var reasonWords = toWordSet("warum", "wieso", "weshalb", "weswegen", "why")

// pairedEllipsis reports whether the clause [s, e) and a clause beside it in
// its sentence are each a date with a participle at its end and no finite
// verb (participleClause), and the pair asks (pairAsks): a reason ("Am Montag
// gelauncht, am Mittwoch gerollbackt – warum?") or that one of the two ends
// with a question mark ("Am Dienstag deployed, am Donnerstag wieder
// zurückgerollt?"): two reports of what was done, a Perfekt without its
// auxiliary (issue #532). A question mark behind a clause of its own ("…, am
// Montag wieder hochgefahren – passt das?") asks about an arrangement as
// often, and an arrangement or a state at the end (stateParticiples,
// planParticiples) reads as one.
func (c clauseWords) pairedEllipsis(s, e int) bool {
	if !c.participleClause(s, e) {
		return false
	}
	if s > 0 && !c.sentence[s] {
		if ps, pe := c.clauseOf(s - 1); c.participleClause(ps, pe) && c.pairAsks(ps, e) {
			return true
		}
	}
	if e < len(c.words) && !c.sentence[e] {
		if ns, ne := c.clauseOf(e); (c.participleClause(ns, ne) || c.againClause(ns, ne)) && c.pairAsks(s, ne) {
			return true
		}
	}
	return false
}

// againStates are the states a thing is in "wieder" or "erneut" after an
// act ("am Dienstag wieder kaputt"), and againWords those words.
var (
	againStates = toWordSet("kaputt", "offen", "rot", "down", "offline", "defekt", "tot", "weg")
	againWords  = toWordSet("wieder", "erneut")
)

// againClause reports whether the clause [s, e) is a date and a state of
// againStates behind "wieder" or "erneut" at its end and no finite verb: the
// report of what followed a participle clause before it ("Am Montag
// repariert, am Dienstag wieder kaputt?"), which pairs with it as a second
// participle clause does (pairedEllipsis, issue #532). The participle clause
// carries the past; the state clause, without a verb of its own, takes the
// question's.
func (c clauseWords) againClause(s, e int) bool {
	last := e - 1
	if last < s+2 || !againStates[c.words[last]] || !againWords[c.words[last-1]] {
		return false
	}
	date := false
	for i := s; i < last-1; i++ {
		w := c.words[i]
		if germanPastVerbs[w] || perfektAux[w] || germanFiniteWords[w] || subjectWords[w] {
			return false
		}
		date = date || dateWord(w)
	}
	return date
}

// pairAsks reports whether the pair of clauses [s, e) asks (pairedEllipsis):
// it ends its sentence with a question mark, or a word of reasonWords stands
// in it, in the clause before it or in the clause behind it in its sentence.
// It reads those three clauses, so that a question of many such clauses is
// read in time linear in its length.
func (c clauseWords) pairAsks(s, e int) bool {
	if c.asks[e-1] && (e == len(c.words) || c.sentence[e]) {
		return true
	}
	lo, hi := s, e
	if s > 0 && !c.sentence[s] {
		lo, _ = c.clauseOf(s - 1)
	}
	if e < len(c.words) && !c.sentence[e] {
		_, hi = c.clauseOf(e)
	}
	scanned(hi - lo)
	for i := lo; i < hi; i++ {
		if reasonWords[c.words[i]] {
			return true
		}
	}
	return false
}

// participleClause reports whether the clause [s, e) has a date word, ends
// with a participle that names no plan and no state, and has no finite verb
// and no subject pronoun.
func (c clauseWords) participleClause(s, e int) bool {
	last := e - 1
	if last <= s || !c.participleAt(s, last) || planParticiples[c.words[last]] || stateParticiples[c.words[last]] {
		return false
	}
	date := false
	for i := s; i < last; i++ {
		w := c.words[i]
		if germanPastVerbs[w] || perfektAux[w] || germanFiniteWords[w] || subjectWords[w] || englishFiniteWords[w] && w != "am" {
			return false
		}
		date = date || dateWord(w)
	}
	return date
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

// recurrence is hasRecurrence for the words of c, without a weekday plural
// that names no day (notADay): the "Mittwochs" of "Was war am Montag mit dem
// Mittwochs-Job?" asks for no Wednesdays (issue #517).
func (c clauseWords) recurrence() bool {
	for i, tok := range c.words {
		if recurrenceKeywords[tok] || isWeekdayPlural(tok) && !c.notADay(i) {
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
var reSeitBisBefore = regexp.MustCompile(`(?i)\b(?:(?:seit|ab|vor|nach)(?:\s+dem)?|bis(?:\s+zu[mr])?|vo[nm]|since|until|from|to|through|(?:before|after)(?:\s+the)?)\s+$`)

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

// reEdgeBefore matches an edge word of matchOpenEdge (with "dem" or "the")
// right before a day date; its group is the word.
var reEdgeBefore = regexp.MustCompile(`(?i)\b(ab|vor|nach|before|after)(?:\s+(?:dem|the))?\s+$`)

// opposingEdges reports whether query has two day dates, each right behind
// an edge word of matchOpenEdge, one opening a window towards the future
// ("ab", "nach", "after") and one towards the past ("vor", "before"), that
// leave a day between them: the window between them, which matchOpenEdge
// reads as it reads two years ("What happened after 2026-03-15 and before
// 2026-03-20?" is 16 to 19 March, issue #517). Edges that leave no day
// between them ("vor dem 15.03.2026 und nach dem 20.03.2026" asks for the
// time outside) stay the two days. Each date reads only the words right
// before it (wordBefore, and the one before an article).
func opposingEdges(query string, matches []dayDate) bool {
	if len(matches) != 2 {
		return false
	}
	lower := strings.ToLower(query)
	var start, end time.Time
	from, until := 0, 0
	for _, m := range matches {
		before := wordBefore(lower, m.start)
		if w := strings.TrimRight(before, asciiSpace); w == "dem" || w == "the" {
			before = wordBefore(lower, m.start-len(before)) + before
		}
		g := reEdgeBefore.FindStringSubmatch(before)
		if g == nil {
			return false
		}
		switch g[1] {
		case "vor", "before":
			until++
			end = m.day.AddDate(0, 0, -1)
		case "ab":
			from++
			start = m.day
		default:
			from++
			start = m.day.AddDate(0, 0, 1)
		}
	}
	return from == 1 && until == 1 && !end.Before(start)
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
	if deferToSeitBis && (leftToWindow(query, matches) || opposingEdges(query, matches)) {
		return nil
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

	// One date, two without a range word, or more: single entries, each
	// with the direction of its own clause's tense (tenseAt): "We deployed
	// on 2026-11-16, and we will deploy on 2026-11-23" (issue #506).
	dates := make([]TemporalDate, 0, len(matches))
	tr := newTenseReader(query)
	for _, m := range matches {
		dir := "past"
		if tr.at(m.start) == "future" {
			dir = "future"
		}
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
	// "seit" and "since" name a start that has begun, as matchSeit reads
	// it: a bare weekday or month is the last one (issue #506).
	since := strings.HasPrefix(m[0], "seit") || strings.HasPrefix(m[0], "since")
	startEdge := resolvePhraseEdge(m[1], backward || since, now)
	if w := phraseWords(m[1]); len(w) > 0 && nowWords[w[0]] {
		startEdge = phraseEdge{day: truncDate(now)}
	}
	endEdge := resolvePhraseEdge(m[2], backward, now)
	if !rangeEdgesStand(query, m, off, startEdge, endEdge, exact, years) {
		return nil
	}
	startEdge = startInEndYear(m, startEdge, endEdge, now)
	if since && startEdge.day.After(truncDate(now)) {
		return nil // "seit 2027 bis 2028" asked in 2026 has no time since yet
	}
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
	return yearEdgeStands(clauseTokens(lower), off)
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
		e.year && !yearEdgeStands(clauseTokens(lower), idx[0]) {
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
	// A "seit X bis Y" whose start lies ahead names no time (issue #506):
	// its "bis" is no deadline of its own.
	if seitStartsAhead(lower, now) {
		return nil
	}
	idx := reBis.FindStringSubmatchIndex(lower)
	if idx == nil {
		return nil
	}
	m := submatches(lower, idx)
	e := resolvePhraseEdge(m[2], false, now)
	if e.day.IsZero() || m[1] == "until" && bareMonthWord(m[2], e) || e.year && !yearEdgeStands(clauseTokens(lower), idx[0]) {
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

// seitStartsAhead reports whether lower holds "seit X bis Y" ("since X
// until Y") whose start X reads as a day after today, the last such weekday
// or month included: a since-window that has not begun, which no rule
// answers (issue #506).
func seitStartsAhead(lower string, now time.Time) bool {
	idx := reSeitBis.FindStringSubmatchIndex(lower)
	if idx == nil {
		return false
	}
	start := resolvePhraseEdge(lower[idx[2]:idx[3]], true, now)
	return !start.day.IsZero() && start.day.After(truncDate(now))
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
// day counts. An edge towards the future and one towards the past in the same
// question are the window between them (issue #506): "nach 2024 und vor
// 2026" is 2025 — when the second ends after the first begins, and with a
// deferred day date when one of them names it.
func matchOpenEdge(query string, now time.Time, edge string) *TemporalResult {
	lower := strings.ToLower(query)
	var proof *yearProof // the year check of the question's candidates, read once
	stands := func(off int) bool {
		if proof == nil {
			proof = newYearProof(clauseTokens(lower))
		}
		return proof.stands(off)
	}
	var first, from, until *openHit
	// The candidates may overlap: a phrase of three words swallows the next
	// edge word ("nach 2024 und vor 2026"), so the search goes on from the
	// phrase, not from the end of the match (issue #506).
	for pos := 0; pos < len(lower); {
		m := reOpenEdge.FindStringSubmatchIndex(lower[pos:])
		if m == nil {
			break
		}
		for k := range m {
			if m[k] >= 0 {
				m[k] += pos
			}
		}
		pos = m[4]
		h := openEdgeHit(lower, m, now, stands)
		if h == nil {
			continue
		}
		if first == nil && (edge == "" || fmtDate(h.e.day) == edge) {
			first = h
		}
		if h.from() && from == nil {
			from = h
		} else if !h.from() && until == nil {
			until = h
		}
	}
	// An edge of each direction is one window, between them (issue #506):
	// "Was kam nach 2024 und vor 2026?" is 2025.
	if from != nil && until != nil && (edge == "" || fmtDate(from.e.day) == edge || fmtDate(until.e.day) == edge) {
		start, end := from.start(), until.e.day.AddDate(0, 0, -1)
		if !end.Before(start) {
			ref := lower[min(from.start0, until.start0):max(from.end1, until.end1)]
			return yearWeighted(rangeResult(ref, fmtDate(start), fmtDate(end), "range"), from.e, until.e)
		}
	}
	if first == nil {
		return nil
	}
	return first.result(now)
}

// openEdgeHit returns the edge the candidate m of reOpenEdge in lower
// names, or nil (matchOpenEdge): not after a unit or a word of nearness, not
// coordinated with another edge word, a date spelt as such that names its
// day itself, a year only with the proof of stands.
func openEdgeHit(lower string, m []int, now time.Time, stands func(int) bool) *openHit {
	word, phrase := lower[m[2]:m[3]], lower[m[4]:m[5]]
	before := wordBefore(lower, m[0])
	if lookBehind(reTimeUnitBefore, before) || word != "ab" && lookBehind(reNearBefore, before) ||
		coordinatedEdge(lower, m[0]-len(before), before) {
		return nil
	}
	// A year first is an edge here only as the year (yearEdge, issue
	// #487): "2024 Oktober" has no use, and "after 2025 may be deleted"
	// names no month but the year.
	yearFirst := false
	if w := phraseWords(phrase); len(w) > 0 {
		_, yearFirst = isYear(w[0])
	}
	e := resolvePhraseEdge(phrase, false, now)
	if e.day.IsZero() || e.fuzzy || e.rel || yearFirst && !e.year || e.year && !stands(m[0]) {
		return nil
	}
	return &openHit{word: word, ref: word + " " + phrase, e: e, start0: m[0], end1: m[1]}
}

// edgeWords and edgeConjunctions: an edge word behind another one and a
// conjunction shares its date with it ("vor und nach dem 15.03.2026",
// "before and after the release"): the two name the time around the date,
// and the date stays the day it names (matchOpenEdge).
var (
	edgeWords        = toWordSet("ab", "vor", "nach", "before", "after")
	edgeConjunctions = toWordSet("und", "oder", "bzw", "and", "or")
)

// coordinatedEdge reports whether the word before (wordBefore, starting at
// the byte offset at of lower) is a conjunction with an edge word before it.
func coordinatedEdge(lower string, at int, before string) bool {
	return edgeConjunctions[strings.TrimRight(before, asciiSpace)] &&
		edgeWords[strings.TrimRight(wordBefore(lower, at), asciiSpace)]
}

// openHit is a phrase matchOpenEdge takes as an edge: its word, its
// reference, what it names and where its match starts and ends.
type openHit struct {
	word, ref    string
	e            phraseEdge
	start0, end1 int
}

// from reports whether the hit opens a window towards the future ("ab",
// "nach", "after").
func (h *openHit) from() bool {
	return h.word == "ab" || h.word == "nach" || h.word == "after"
}

// start is the first day of the window a hit towards the future opens:
// the day of "ab", the day after what "nach" and "after" name.
func (h *openHit) start() time.Time {
	if h.word == "ab" {
		return h.e.day
	}
	return h.e.last().AddDate(0, 0, 1)
}

// result is the window of the hit alone.
func (h *openHit) result(now time.Time) *TemporalResult {
	if h.from() {
		return yearWeighted(&TemporalResult{Dates: []TemporalDate{{Ref: h.ref, Date: fmtDate(h.start()), Dir: DirFrom}}}, h.e)
	}
	// vor, before: a day to come is a deadline, the window from today to
	// the day before it, as "bis" with a later end is one; a day that has
	// begun is the time up to the day before it.
	if today := truncDate(now); h.e.day.After(today) {
		return yearWeighted(rangeResult(h.ref, fmtDate(today), fmtDate(h.e.day.AddDate(0, 0, -1)), "range"), h.e)
	}
	return yearWeighted(&TemporalResult{Dates: []TemporalDate{{Ref: h.ref, Date: fmtDate(h.e.day.AddDate(0, 0, -1)), Dir: DirUntil}}}, h.e)
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
	// reHoliday finds where a holiday of matchAnnualHoliday stands
	// (matchDayKeywordPart, issue #532).
	reHoliday = regexp.MustCompile(reWeihnachten.String() + "|" + reSilvester.String() + "|" + reNeujahr.String())
)

// matchRelativeOrPartDays is Priority 8 of NormalizeTemporalRules: the
// relative days, then a day keyword with a part of the day.
func matchRelativeOrPartDays(query string, now time.Time) *TemporalResult {
	if r := matchRelativeDays(query, now); r != nil {
		return r
	}
	return matchDayKeywordPart(query, now)
}

// dayKeywordDays are the day keywords of simpleKeywords by word, and
// timeOfDayWords the words matchTimeOfDay's expressions read: a part of the
// day of that kind right behind "heute" or "today" is matchTimeOfDay's
// ("heute früh", "today morning"), issue #532.
var (
	dayKeywordDays = func() map[string]simpleKW {
		m := make(map[string]simpleKW, len(simpleKeywords))
		for _, kw := range simpleKeywords {
			m[kw.word] = kw
		}
		return m
	}()
	timeOfDayWords = toWordSet("morgens", "früh", "fruh", "morning", "early", "vormittags", "mittags", "noon", "midday",
		"lunchtime", "nachmittags", "afternoon", "abends", "evening", "nachts", "night", "spät", "spat", "late")
	// lastNightArticles before "last night" or "letzte Nacht" make it the
	// last night of something ("the last night of the summit", "die letzte
	// Nacht im Hotel"); lastNightOf behind it says of what.
	lastNightArticles = toWordSet("the", "a", "that", "this", "on", "der", "die", "das", "in", "am", "eine", "einer")
	lastNightOf       = toWordSet("of", "des", "der", "vor", "before")
	nightNouns        = toWordSet("night", "night's", "nacht")
)

// matchDayKeywordPart reads a day keyword with a part of the day (issue
// #532): a word of a part of the day right behind a day keyword in its clause
// ("gestern Abend", "heute Morgen", "morgen früh", "yesterday evening",
// "tomorrow morning" — "heute morgen" is this morning: "heute" and "morgen"
// side by side in one clause name one day), "last night" and "letzte Nacht"
// without an article before them or "of" behind them (yesterday's night),
// and "tonight" (today). The answer is the keyword's day, and every weekday of
// the question that names a day as the weekday rules read it, in the order of
// the question: the day keyword answers before the weekday rules and does not
// drop their dates. A part of the day joined by a hyphen ("Abend-Bericht") is
// a word of its own, and one matchTimeOfDay reads right behind "heute" or
// "today" stays its.
//
// The part's hour is set on a day that lies behind (dayKeywordHour): the
// linear gravity of the query (handler applyTemporalGravity) takes the hour as
// the instant of its target and filters a date to come by that instant, so an
// hour on today or a day ahead moved the gravity off that day's content
// without a time of day onto the next day's (gravity-hour probe of issue
// #532); behind the target it keeps the content of that day before the hour,
// which a target at midnight leaves out.
func matchDayKeywordPart(query string, now time.Time) *TemporalResult {
	lower := strings.ToLower(query)
	if !strings.Contains(lower, "night") && !strings.Contains(lower, "nacht") && !strings.Contains(lower, "heute") &&
		!strings.Contains(lower, "today") && !strings.Contains(lower, "gestern") && !strings.Contains(lower, "yesterday") &&
		!strings.Contains(lower, "morgen") && !strings.Contains(lower, "tomorrow") {
		return nil
	}
	c := clauseTokens(query)
	hits, plain := c.dayKeywordHits(now)
	weekdays := c.dayWeekdays()
	// A day keyword without a part of the day answers here beside a weekday
	// or a day keyword with one, which would drop it ("Was ist gestern und am
	// Montag passiert?", "Was lief gestern Abend schief und was steht morgen
	// an?"), issue #532; alone it is matchSimpleKeywords'.
	if len(weekdays) > 0 || len(hits) > 0 {
		hits = append(hits, plain...)
	}
	if len(hits) == 0 {
		return nil
	}
	hits = c.withWeekdays(query, hits, weekdays, now)
	sort.SliceStable(hits, func(a, b int) bool { return hits[a].idx < hits[b].idx })
	var dates []TemporalDate
	seen := map[string]bool{}
	for _, h := range hits {
		if !seen[h.date.Date] {
			seen[h.date.Date] = true
			dates = append(dates, h.date)
		}
	}
	r := multiResult(c.withHolidays(query, lower, hits, dates, seen, now))
	// The weights of the first date's matcher: a weekday's where a weekday
	// comes first.
	if _, ok := weekdayWord(c.words[hits[0].idx]); ok {
		r.DimensionWeights = dwLinearWeekday()
	}
	return r
}

// keywordHit is a date matchDayKeywordPart reads at the word idx.
type keywordHit struct {
	idx  int
	date TemporalDate
}

// dayKeywordHits returns the day keywords of c with a part of the day
// (dayKeywordPartAt) and those without one (dayKeywordWord), each with its
// date.
func (c clauseWords) dayKeywordHits(now time.Time) (hits, plain []keywordHit) {
	scanned(len(c.words))
	for i := 0; i < len(c.words); i++ {
		if d, ok := c.dayKeywordPartAt(i, now); ok {
			hits = append(hits, keywordHit{i, d})
			if c.words[i] != "tonight" && c.words[i] != "tonight's" {
				i++ // the part of the day is no keyword of its own ("heute Morgen")
			}
		} else if kw, ok := dayKeywordDays[c.words[i]]; ok && c.dayKeywordWord(i) {
			kw = dayBeforeOrAfter(c.words, i, kw)
			plain = append(plain, keywordHit{i, TemporalDate{Ref: kw.word, Date: fmtDate(truncDate(kw.resolve(now))), Dir: kw.dir}})
		}
	}
	return hits, plain
}

// withWeekdays appends to hits the dates of the weekdays of the question,
// each by its clause (weekdayDate); a weekday in the clause of a day keyword
// that names the keyword's day is that day ("Heute ist Montag", "Ist morgen
// Freitag?").
func (c clauseWords) withWeekdays(query string, hits []keywordHit, weekdays []struct {
	wd  time.Weekday
	idx int
	ref string
}, now time.Time) []keywordHit {
	named := map[[2]int]bool{}
	for _, h := range hits {
		if d, err := time.Parse("2006-01-02", h.date.Date); err == nil {
			cs, _ := c.clauseOf(h.idx)
			named[[2]int{cs, int(d.Weekday())}] = true
		}
	}
	var tr *tenseReader
	for _, w := range weekdays {
		if cs, _ := c.clauseOf(w.idx); named[[2]int{cs, int(w.wd)}] {
			continue
		}
		if tr == nil {
			tr = newTenseReader(query)
		}
		hits = append(hits, keywordHit{w.idx, c.weekdayDate(tr, w.wd, w.idx, w.ref, now)})
	}
	return hits
}

// withHolidays appends to dates the date of a holiday and of the weekend of
// the question as their rules read it where it stands in a clause of its own
// ("…, und was ist für Weihnachten geplant?"); in the clause of a day keyword
// it says when within that day ("last night during the new year rollover"),
// issue #532.
func (c clauseWords) withHolidays(query, lower string, hits []keywordHit, dates []TemporalDate, seen map[string]bool, now time.Time) []TemporalDate {
	inHit := map[int]bool{}
	for _, h := range hits {
		cs, _ := c.clauseOf(h.idx)
		inHit[cs] = true
	}
	for _, x := range []struct {
		re    *regexp.Regexp
		match func(string, time.Time) *TemporalResult
	}{{reHoliday, matchAnnualHoliday}, {reWochenende, matchWeekend}} {
		loc := x.re.FindStringIndex(lower)
		if loc == nil {
			continue
		}
		if i := c.wordAt(loc[0]); i >= 0 {
			if cs, _ := c.clauseOf(i); inHit[cs] {
				continue
			}
		}
		if extra := x.match(query, now); extra != nil && !seen[extra.Dates[0].Date] {
			seen[extra.Dates[0].Date] = true
			dates = append(dates, extra.Dates[0])
		}
	}
	return dates
}

// dayKeywordPartAt returns the date matchDayKeywordPart reads at word i of c.
func (c clauseWords) dayKeywordPartAt(i int, now time.Time) (TemporalDate, bool) {
	w := c.words[i]
	if w == "tonight" || w == "tonight's" {
		// Behind a determiner it is a word of a name ("The Tonight Show"),
		// and so it is written with its capital beside another such word or
		// behind a word of nameGenus ("Wer spielt in der Band Tonight
		// Alive?"), issue #532.
		if i > 0 && !c.starts[i] && englishDeterminers[c.words[i-1]] || c.nameWordAt(i) {
			return TemporalDate{}, false
		}
		return TemporalDate{Ref: w, Date: fmtDate(now), Dir: "today"}, true
	}
	if i+1 >= len(c.words) || c.starts[i+1] || c.joinedNext[i] || i+1 < len(c.words)-1 && c.joinedNext[i+1] {
		return TemporalDate{}, false
	}
	next := c.words[i+1]
	if (w == "last" && next != "nacht" || w == "letzte" && next == "nacht") && nightNouns[next] {
		return c.lastNightAt(i, now)
	}
	kw, ok := dayKeywordDays[w]
	h, part := dayPartHours[next]
	if !ok || !part || (w == "heute" || w == "today") && timeOfDayWords[next] {
		return TemporalDate{}, false
	}
	d := TemporalDate{Ref: w + " " + next, Date: fmtDate(truncDate(kw.resolve(now))), Dir: kw.dir}
	if kw.dir == "past" {
		d.Hour = &h
	}
	return d, true
}

// nameGenus are the words of a kind of work or group a name follows ("die
// Band Tonight Alive", "the show Tonight Live"), issue #532.
var nameGenus = toWordSet("band", "show", "album", "song", "film", "movie", "series", "serie", "podcast", "sendung",
	"gruppe", "track", "single", "tour", "book", "buch", "roman", "novel", "musical", "titel", "title")

// nameWordAt reports whether word i of c, written with its capital inside a
// sentence, is a word of a name (dayKeywordPartAt): before another word
// written so in its clause ("Tonight Alive"), behind a word of nameGenus
// ("die Band Tonight Alive", "the show Tonight Live"), or behind a word
// written so that follows one ("das Album Midnight Tonight"). A capital
// word before it alone is no sign — English writes a noun with its capital
// as often ("Is the Release Tonight?", "Meeting with Anna Tonight?") — and
// a question that writes all its words with their capital ("Who Is Playing
// Tonight?", titled) gives none.
func (c clauseWords) nameWordAt(i int) bool {
	if c.titled || !c.capInSentence(i) {
		return false
	}
	if i+1 < len(c.words) && !c.starts[i+1] && c.capInSentence(i+1) {
		return true
	}
	return !c.starts[i] && (nameGenus[c.words[i-1]] || i >= 2 && !c.starts[i-1] && c.capInSentence(i-1) && nameGenus[c.words[i-2]])
}

// capInSentence reports whether word i of c is written with its capital and
// does not start a sentence.
func (c clauseWords) capInSentence(i int) bool {
	return i > 0 && i < len(c.words) && !c.sentence[i] && c.written[i]
}

// lastNightAt is dayKeywordPartAt for "last night" or "letzte Nacht" at word
// i of c: yesterday at the hour of the night, unless an article before it or
// "of" behind it makes it the last night of something.
func (c clauseWords) lastNightAt(i int, now time.Time) (TemporalDate, bool) {
	if i > 0 && !c.starts[i] && lastNightArticles[c.words[i-1]] || i+2 < len(c.words) && !c.starts[i+2] && lastNightOf[c.words[i+2]] {
		return TemporalDate{}, false
	}
	h := dayPartHours["night"]
	return TemporalDate{Ref: c.words[i] + " " + c.words[i+1], Date: fmtDate(now.AddDate(0, 0, -1)), Dir: "past", Hour: &h}, true
}

func matchAnnualHoliday(query string, now time.Time) *TemporalResult {
	lower := strings.ToLower(query)
	var ref string
	var month time.Month
	var day int
	var loc []int
	switch {
	case reWeihnachten.MatchString(lower):
		ref, month, day, loc = "Weihnachten", time.December, 25, reWeihnachten.FindStringIndex(lower)
	case reSilvester.MatchString(lower):
		ref, month, day, loc = "Silvester", time.December, 31, reSilvester.FindStringIndex(lower)
	case reNeujahr.MatchString(lower):
		ref, month, day, loc = "Neujahr", time.January, 1, reNeujahr.FindStringIndex(lower)
	default:
		return nil
	}
	// The holiday of the year a question means: the year written right
	// behind it ("Weihnachten 2025"), else the last one when its clause is in
	// the past and the next one otherwise ("Was war an Neujahr los?" asked in
	// November is the past 1 January, "Was ist an Neujahr geplant?" the
	// coming one), issue #532.
	rest := strings.TrimLeft(lower[loc[1]:], asciiSpace)
	if n := strings.IndexAny(rest, asciiSpace); n >= 0 {
		rest = rest[:n]
	}
	if y, ok := isYear(strings.Trim(rest, wordTrim)); ok {
		return singleResult(ref, fmtDate(time.Date(y, month, day, 0, 0, 0, 0, now.Location())), "range")
	}
	d := time.Date(now.Year(), month, day, 0, 0, 0, 0, now.Location())
	today := truncDate(now)
	if tenseAt(query, loc[0]) == "past" {
		if d.After(today) {
			d = d.AddDate(-1, 0, 0)
		}
	} else if d.Before(today) {
		d = d.AddDate(1, 0, 0)
	}
	return singleResult(ref, fmtDate(d), "range")
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
	r := timeOfDayAt(lower, now)
	if r == nil {
		return nil
	}
	// Greetings and parts of the day in a noun phrase name no time ("Guten
	// Morgen", "the night owl"): read again without them (issue #532).
	c := clauseTokens(query)
	if unread := c.withoutUnreadDayParts(query, lower); unread != lower {
		if r = timeOfDayAt(unread, now); r == nil {
			return nil
		}
	}
	return timeOfDayRest(c, r)
}

// timeOfDayAt is today at the hour of the first part of the day of
// matchTimeOfDay's expressions in lower, or nil.
func timeOfDayAt(lower string, now time.Time) *TemporalResult {
	today := now.Format("2006-01-02")
	var r *TemporalResult
	// Reference hour per pattern (middle of each daypart).
	mk := func(ref string, hour int) {
		h := hour
		r = &TemporalResult{
			Dates: []TemporalDate{{Ref: ref, Date: today, Dir: "today", Hour: &h}},
		}
	}
	switch {
	case reMorgens.MatchString(lower):
		mk("morgens", 8) // 07:00-10:00
	case reVormittags.MatchString(lower):
		mk("vormittags", 10) // 10:00-12:00
	case reMittags.MatchString(lower):
		mk("mittags", 12) // 12:00-14:00
	case reNachmittags.MatchString(lower):
		mk("nachmittags", 15) // 14:00-17:00
	case reAbends.MatchString(lower):
		mk("abends", 19) // 17:00-22:00
	case reNachts.MatchString(lower):
		mk("nachts", 23) // 22:00-06:00
	}
	return r
}

// timeOfDayRest is the rest of matchTimeOfDay: a part of the day that
// belongs to a weekday, a weekday or a day keyword of another day beside it.
func timeOfDayRest(c clauseWords, r *TemporalResult) *TemporalResult {
	// A part of the day that belongs to a weekday is a time of that day, not
	// of today: the weekday rules read it with its hour (dayPartAt). And a
	// weekday that names a day answers before a time-of-day word that belongs
	// to none, unless the word is today's ("heute Abend", "this morning"):
	// "early", "late", "früh" and "spät" say how a thing came as often as
	// when ("Was the Monday release late?", "Ist das Meeting am Dienstag zu
	// früh angesetzt?"), issue #517.
	weekdays := c.dayWeekdays()
	for _, w := range weekdays {
		if _, ok := c.dayPartAt(w.idx); ok {
			return nil
		}
	}
	// A day keyword of another day answers before a time-of-day word that
	// belongs to none, as a weekday does ("Was passiert morgen, früh genug
	// für das Review?", "Was the night build green yesterday?"), issue #532.
	if (len(weekdays) > 0 || c.otherDayKeyword()) && !c.todaysDayPart() {
		return nil
	}
	return r
}

// greetingWords open a greeting with a part of the day ("Guten Morgen",
// "Good evening"); greetingParts are those parts, and greetingAddressees the
// words a greeting of a part of the day alone at the start of a question
// turns to ("Morning team", "Evening all"). attributiveOpeners before an
// English part of the day make it a word of a noun ("the morning star", "the
// night owl", "the last night train"), unless a preposition before them makes
// it a time ("in the morning", "during the night"), issue #532.
var (
	greetingWords      = toWordSet("guten", "gute", "good")
	greetingParts      = toWordSet("morgen", "abend", "tag", "nacht", "morning", "evening", "night", "afternoon")
	greetingAddressees = toWordSet("all", "everyone", "everybody", "team", "folks", "zusammen", "allerseits")
	englishDayParts    = toWordSet("morning", "evening", "night", "afternoon")
	attributiveOpeners = toWordSet("the", "a", "an", "our", "my", "your", "their", "his", "her", "its", "that")
	timePrepositions   = toWordSet("in", "during", "at", "through", "throughout", "over", "by", "into")
)

// greetingAt reports whether word i of c, a part of the day, is a greeting:
// behind "guten", "gute" or "good", or an English part of the day that opens
// the question before a comma or an addressee ("Morning, did …").
func (c clauseWords) greetingAt(i int) bool {
	w := c.words[i]
	if !greetingParts[w] {
		return false
	}
	if i > 0 && greetingWords[c.words[i-1]] {
		return true
	}
	return i == 0 && englishDayParts[w] && w != "night" && (len(c.words) == 1 || c.starts[1] || greetingAddressees[c.words[1]])
}

// attributivePartAt reports whether word i of c, an English part of the day,
// is a word of a noun phrase and names no time (attributiveOpeners).
func (c clauseWords) attributivePartAt(i int) bool {
	if !englishDayParts[c.words[i]] || i == 0 || c.starts[i] {
		return false
	}
	prev := c.words[i-1]
	if prev == "last" {
		if i+1 < len(c.words) && !c.starts[i+1] && c.words[i+1] == "of" {
			return true
		}
		if i < 2 || c.starts[i-1] {
			return false
		}
		i, prev = i-1, c.words[i-2]
	}
	return attributiveOpeners[prev] && (i < 2 || c.starts[i-1] || !timePrepositions[c.words[i-2]])
}

// withoutUnreadDayParts returns lower, the lowercased query, with the words
// of a part of the day that are a greeting or a word of a noun blanked, so
// that matchTimeOfDay's expressions do not read them; a question whose
// lowercasing changes its length is returned as it is.
func (c clauseWords) withoutUnreadDayParts(query, lower string) string {
	if len(lower) != len(query) {
		return lower
	}
	scanned(len(c.words))
	var b []byte
	for i, w := range c.words {
		if greetingParts[w] && (c.greetingAt(i) || c.attributivePartAt(i)) {
			if b == nil {
				b = []byte(lower)
			}
			for k := c.offs[i]; k < c.offs[i]+len(w) && k < len(b); k++ {
				b[k] = ' '
			}
		}
	}
	if b == nil {
		return lower
	}
	return string(b)
}

// distanceWords count a distance behind "vor" or "in" (dayKeywordWord).
var distanceWords = toWordSet("einer", "einem", "einen", "zwei", "drei", "vier", "fünf", "sechs", "sieben", "acht",
	"neun", "zehn", "1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11", "12", "14", "30")

// otherDayKeyword reports whether c has a day keyword of a day other than
// today as a word of its own: not a greeting, not in a compound, not
// "morgen" behind a weekday ("Montag morgen").
func (c clauseWords) otherDayKeyword() bool {
	scanned(len(c.words))
	for i, w := range c.words {
		if _, ok := dayKeywordDays[w]; ok && w != "heute" && w != "today" && c.dayKeywordWord(i) {
			return true
		}
	}
	return false
}

// dayKeywordWord reports whether word i of c, a day keyword, names its day:
// it is no greeting ("Guten Morgen"), not joined by a hyphen to a word that
// names no day ("Morgen-Report"), not the start of a distance from it
// ("gestern vor einer Woche", "heute in zwei Wochen"), and "morgen" is no
// part of the day of a weekday right before it in its clause ("Montag
// morgen").
func (c clauseWords) dayKeywordWord(i int) bool {
	if c.greetingAt(i) || c.inCompound(i) {
		return false
	}
	if i+2 < len(c.words) && !c.starts[i+1] && (c.words[i+1] == "vor" || c.words[i+1] == "in") && distanceWords[c.words[i+2]] {
		return false
	}
	if c.words[i] == "morgen" && i > 0 && !c.starts[i] {
		if _, ok := weekdayWord(c.words[i-1]); ok {
			return false
		}
	}
	return true
}

// todayWords make the part of the day right behind them today's ("heute
// Abend", "this morning"), issue #517.
var todayWords = toWordSet("heute", "today", "this")

// todaysDayPart reports whether c has a word of a part of the day right
// behind a word of todayWords.
func (c clauseWords) todaysDayPart() bool {
	for i := 1; i < len(c.words); i++ {
		if _, ok := dayPartHours[c.words[i]]; ok && todayWords[c.words[i-1]] {
			return true
		}
	}
	return false
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
	// The first "Wochenende" or "weekend" not joined by a hyphen to a word
	// that names no day ("Wochenende-Flag", "Weekend-Mode"), issue #506.
	var loc []int
	var c *clauseWords
	for _, l := range reWochenende.FindAllStringIndex(strings.ToLower(query), -1) {
		if c == nil {
			cw := clauseTokens(query)
			c = &cw
		}
		if i := c.wordAt(l[0]); i < 0 || c.offs[i] != l[0] || !c.inCompound(i) {
			loc = l
			break
		}
	}
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

// weekdayMentions counts the weekday names in lower, a lowercased question,
// as substrings (the German names, the English ones): an upper bound of its
// weekday words, read without tokenizing it.
func weekdayMentions(lower string) int {
	n := 0
	for i := range weekdayStemsDE {
		n += strings.Count(lower, weekdayStemsDE[i]) + strings.Count(lower, strings.ToLower(weekdayNameEN[i]))
	}
	return n
}

// afterCapital reports whether word i of c stands right behind a word in its
// clause that is written with a capital and is no acronym, not the word that
// opens the clause. It reads the case as written (written), also where a
// question capitalizes most of its words: German capitalizes its nouns, and a
// short question with a name is mostly capitals ("Am Dienstag hat Anna
// Freitag angerufen"). A question with every word capitalized loses a
// weekday beyond the first here, as the rules lost it before issue #517.
func (c clauseWords) afterCapital(i int) bool {
	return i > 0 && !c.starts[i] && !c.starts[i-1] && c.written[i-1] && !c.upper[i-1]
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
		} else if wd, _, ok := weekdayCompound(tok); ok {
			results = append(results, struct {
				wd  time.Weekday
				idx int
				ref string
			}{wd, i, weekdayNameDE[wd]})
		}
	}
	return results
}

// weekdayDate is the date of the weekday wd at word i of c, named ref, among
// several dates of a question: by the tense of its clause (tr), the hour of a
// part of the day that belongs to it (dayPartAt), and — as for a single
// weekday (matchWeekday) — "vom Freitag" names the Friday that has been
// unless its clause points forward (issue #532).
func (c clauseWords) weekdayDate(tr *tenseReader, wd time.Weekday, i int, ref string, now time.Time) TemporalDate {
	t := tr.at(c.offs[i])
	backward := t == "past" || i > 0 && c.words[i-1] == "vom" && t != "future"
	dir := "future"
	if backward {
		dir = "past"
	}
	td := TemporalDate{Ref: ref, Date: fmtDate(resolveWeekdayDate(wd, backward, now)), Dir: dir}
	if h, ok := c.dayPartAt(i); ok {
		td.Hour = &h
	}
	return td
}

func matchMultiKeyword(query string, now time.Time) *TemporalResult {
	lower := strings.ToLower(query)

	// First: every weekday of the question that names a day (notADay: no
	// compound, no surname) — a disjunction or conjunction ("Montag oder
	// Dienstag", "Monday or Tuesday") and two clauses with a weekday each,
	// with or without a conjunction ("Am Mittwoch hatten wir 3 Incidents, am
	// Freitag werden wir das Postmortem halten", issue #517).
	hasConjunction := strings.Contains(lower, " und ") || strings.Contains(lower, " and ")
	hasDisjunction := strings.Contains(lower, " oder ") || strings.Contains(lower, " or ")
	if !hasConjunction && !hasDisjunction && weekdayMentions(lower) < 2 {
		return nil // no two weekdays, no conjunction: nothing for either branch
	}
	c := clauseTokens(query)
	weekdays := c.dayWeekdays()
	if !hasConjunction && !hasDisjunction {
		// Without a conjunction a weekday beyond the first right behind a
		// word written with a capital inside its clause may be a surname
		// ("Am Dienstag hat Max Montag die Migration gemacht", "Am Mittwoch
		// hat die Frau Montag angerufen"); it counts only elsewhere ("…, am
		// Freitag werden wir …", "Deployed Monday, rolled back Wednesday"),
		// issue #517.
		n := 0
		for k, w := range weekdays {
			if k == 0 || !c.afterCapital(w.idx) {
				weekdays[n] = w
				n++
			}
		}
		weekdays = weekdays[:n]
	}
	if len(weekdays) >= 2 {
		// Each weekday by the tense of its own clause (tenseAt): "We
		// deployed on Monday, and we will deploy on Monday" names two days
		// (issue #506); weekdays of one clause share its tense.
		tr := newTenseReader(query)
		var dates []TemporalDate
		seen := make(map[string]bool)
		for _, w := range weekdays {
			if td := c.weekdayDate(tr, w.wd, w.idx, w.ref, now); !seen[td.Date] {
				seen[td.Date] = true
				dates = append(dates, td)
			}
		}
		if len(dates) >= 2 {
			return multiResult(dates)
		}
	}

	if !hasConjunction && !hasDisjunction {
		return nil
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
	// The first weekday that names a day: not in a compound (issue #506), not
	// a surname (issue #517).
	c := clauseTokens(query)
	var wd time.Weekday
	wdIdx, found := -1, false
	for i := range c.words {
		if w, ok := weekdayWord(c.words[i]); ok && !c.notADay(i) {
			wd, wdIdx, found = w, i, true
			break
		}
	}
	if !found {
		return nil
	}

	// Disambiguate "Morgen" as time-of-day vs "morgen" as tomorrow.
	// If a weekday precedes "morgen", it's morning, not tomorrow.
	// "Mittwoch Morgen" — morgen is morning; no action needed here.

	tense := tenseAt(query, c.offs[wdIdx])
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
	r := singleResult(ref, fmtDate(d), dir)
	// A part of the day that belongs to the weekday gives the hour: the day
	// is the weekday's, the hour the part's (issue #517).
	if h, ok := c.dayPartAt(wdIdx); ok {
		r.Dates[0].Hour = &h
	}
	return r
}

// dayBeforeOrAfter returns the day keyword the English phrase ending at
// tokens[i] names: "the day before yesterday" is "vorgestern", "the day after
// tomorrow" "übermorgen" (issue #532); any other keyword kw is itself.
func dayBeforeOrAfter(tokens []string, i int, kw simpleKW) simpleKW {
	if i < 2 || tokens[i-2] != "day" {
		return kw
	}
	switch {
	case kw.word == "yesterday" && tokens[i-1] == "before":
		return dayKeywordDays["vorgestern"]
	case kw.word == "tomorrow" && tokens[i-1] == "after":
		return dayKeywordDays["übermorgen"]
	}
	return kw
}

func matchSimpleKeywords(query string, now time.Time) *TemporalResult {
	c := clauseTokens(query)
	tokens := c.words
	var dates []TemporalDate
	seen := make(map[string]bool)

	for i, tok := range tokens {
		// A greeting and a compound name no day ("Guten Morgen",
		// "Morgen-Report"), issue #532.
		if c.greetingAt(i) || c.inCompound(i) {
			continue
		}
		for _, kw := range simpleKeywords {
			if fuzzyMatch(tok, kw.word) {
				kw = dayBeforeOrAfter(tokens, i, kw)
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

// inZone returns e with its day as the same calendar day at midnight in the
// zone of now.
func (e phraseEdge) inZone(now time.Time) phraseEdge {
	y, m, d := e.day.Date()
	e.day = time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	return e
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
	"fehlen", "passiert", "existiert", "alle", "jede", "jeden", "jeder", "jedes", "sämtliche", "beide",
	// English
	"and", "or", "but", "to", "through", "till", "on", "at", "for", "from", "of", "by", "with",
	"about", "into", "over", "under", "during", "onwards", "onward", "the", "a", "it", "i", "we",
	"they", "you", "he", "she", "there", "this", "that", "these", "those", "is", "are", "be", "been",
	"has", "have", "will", "would", "can", "could", "should", "must", "may", "might", "shall", "do",
	"does", "not", "ever", "still", "already", "only", "also", "again", "yet", "then", "when", "where",
	"what", "which", "who", "how", "why", "here", "now", "all", "every", "each", "both",
)

// yearFollower reports whether w may follow a bare year at a window's edge
// (yearFollowers). The "ge" pattern of the participles is no follower: German
// nouns take its shape as well ("nach 2048 Generationen", "nach 2025
// Angeboten").
func yearFollower(w string) bool {
	return yearFollowers[w] || pastVerbSet[w] || futureVerbSet[w] || perfektAux[w] || knownParticiples[w] ||
		ierenParticiple(w) || strongPastSet[w]
}

// strongPastSet is strongPastForms as words: a strong simple past behind a
// year is a verb, not the noun the year counts ("Bis 2025 galten andere
// Limits"), issue #506.
var strongPastSet = toWordSet(strongPastForms...)

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
		if !endsClause(phrase) && !dashWord(words[1]) && (!yearFollower(words[1]) ||
			yearConjunctions[words[1]] && len(words) > 2 && quantityWords[words[2]] ||
			rangeWords[words[1]] && len(words) > 2 && strings.Trim(words[2], "0123456789") == "") {
			return phraseEdge{}, false
		}
	}
	return phraseEdge{day: time.Date(y, time.January, 1, 0, 0, 0, 0, now.Location()), year: true}, true
}

// dashWord reports whether w is a dash of its own, which ends a clause as a
// comma does ("Seit 2022 – wie viele Ausfälle hatten wir?"), issue #506.
func dashWord(w string) bool {
	return w != "" && strings.Trim(w, "-\u2013\u2014") == ""
}

// yearInWindow reports whether y is a year a bare number may name as a
// window's edge at now: minYearEdge to maxYearEdge, and no later than
// maxYearsAhead after now's year.
func yearInWindow(y int, now time.Time) bool {
	return y >= minYearEdge && y <= maxYearEdge && y <= now.Year()+maxYearsAhead
}

// endsClause reports whether the first word of phrase (phraseWords), as it
// stands in the phrase, ends with one of the runes of clauseEnd — a closing
// parenthesis only when the parenthesis opened before the word.
func endsClause(phrase string) bool {
	for _, f := range strings.Fields(phrase) {
		if strings.Trim(f, phrasePunct) != "" {
			r, _ := utf8.DecodeLastRuneInString(f)
			// A parenthesis the word opens itself closes the word, not a
			// clause: in "After (2024) failures" the noun follows the year
			// (issue #506).
			return strings.ContainsRune(clauseEnd, r) && (r != ')' || !strings.Contains(f, "("))
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
// offset off of the lowercased question, whose words are c (clauseTokens of
// it), may stand as a window's edge
// (issue #487): a year needs, beside the window of yearInWindow and the word
// behind it (yearEdge), a clause without a word that makes the number a
// count, a position or a name (countWords) and without a phrase that asks for
// a count (countPhrases) — the end of its clause alone is no proof ("Zeig mir
// die Zeilen ab 2024.") —, and no other clause that asks for a count of such
// a noun ("Nach wie vielen Requests, ab 2024?"). A count phrase in another
// clause that counts no such noun governs no number of the year's clause
// (issue #506): "Since 2024, how many outages have we had?" names the year,
// as "since 2024, lines have grown" does. Such a question goes to the LLM
// fallback, as before.
func yearEdgeStands(c clauseWords, off int) bool {
	return newYearProof(c).stands(off)
}

// yearProof answers yearEdgeStands for the year candidates of one question
// (matchOpenEdge asks it per candidate), reading each clause for a count
// word and a count phrase once (issue #506): read anew per candidate, the
// check cost time quadratic in the length of a question with many
// candidates.
type yearProof struct {
	c clauseWords
	// clauses holds, per clause start, what the clause has; read: every
	// clause of the question is in it.
	clauses map[int]clauseCount
	read    bool
	// counting holds the starts of the clauses with a count phrase and a
	// count word, at most two (read once, when read is set).
	counting []int
}

// clauseCount is what yearProof reads of a clause: a count word, a count
// phrase.
type clauseCount struct{ word, phrase bool }

func newYearProof(c clauseWords) *yearProof {
	return &yearProof{c: c, clauses: map[int]clauseCount{}}
}

// clause returns what the clause that starts at s and ends at e has.
func (p *yearProof) clause(s, e int) clauseCount {
	if k, ok := p.clauses[s]; ok {
		return k
	}
	scanned(e - s)
	words := p.c.words[s:e]
	k := clauseCount{word: hasCountWord(words)}
	joined := strings.Join(words, " ")
	for _, phrase := range countPhrases {
		k.phrase = k.phrase || strings.Contains(joined, phrase)
	}
	p.clauses[s] = k
	return k
}

// stands is yearEdgeStands for the year after the word at the byte offset
// off.
func (p *yearProof) stands(off int) bool {
	s, e := 0, len(p.c.words)
	i := p.c.wordAt(off)
	if i >= 0 {
		s, e = p.c.clauseOf(i)
	}
	if p.clause(s, e).word || i >= 0 && p.c.countsAParticipleNoun(i+1, e) {
		return false
	}
	if !p.read {
		p.read = true
		for cs := 0; cs < len(p.c.words); {
			_, ce := p.c.clauseOf(cs)
			if k := p.clause(cs, ce); k.word && k.phrase && len(p.counting) < 2 {
				p.counting = append(p.counting, cs)
			}
			cs = ce
		}
	}
	for _, cs := range p.counting {
		if cs != s {
			return false
		}
	}
	return true
}

// countsAParticipleNoun reports whether the year at word y of c, in the
// clause that ends at e, counts the noun behind it (issue #517): an English
// participle and a plural noun follow it, an attribute and its noun ("After
// 2021 failed logins the account locks"), and the noun phrase ends there —
// with its clause, or before a determiner or a subject pronoun that starts
// what the phrase leads into. Before a verb the noun phrase is the subject
// and the year the window's edge ("After 2025 failed logins lock the
// account", "Before 2025 released versions had no SBOM").
func (c clauseWords) countsAParticipleNoun(y, e int) bool {
	if y+2 >= e || !englishParticiple(c.words[y+1]) || !englishPlural(c.words[y+2]) {
		return false
	}
	return y+3 == e || englishDeterminers[c.words[y+3]] || englishSubjects[c.words[y+3]]
}

// englishPlural reports whether w has the shape of an English plural noun:
// a word of at least four letters on -s that is no English verb form of
// "be", "have" or "do" and does not end in -ss ("logins", "attempts",
// "builds"; not "was", "has", "does", "access").
func englishPlural(w string) bool {
	return utf8.RuneCountInString(w) >= 4 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss") && !englishNotPassive[w] && !englishBeForms[w]
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
	// The day is a calendar day in the zone of now, as the days of the other
	// edges are: ordered by instant, a day date at UTC midnight and a year at
	// local midnight came apart west of UTC (issue #506).
	if t, ok := parseDayDate(strings.ToUpper(tok)); ok {
		return phraseEdge{day: t}.inZone(now)
	}
	if t, err := time.Parse("2006-01-02", tok); err == nil {
		return phraseEdge{day: t}.inZone(now)
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
	cw := clauseTokens(query)
	tokens := cw.words
	recur := cw.recurrence()

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

	// Priority 8: Relative days (vor N Tagen, in N Tagen), then a day
	// keyword with a part of the day ("gestern Abend", "tomorrow morning",
	// "last night": the keyword's day at the part's hour, with every weekday
	// of the question, issue #532) — pure linear
	if r := matchRelativeOrPartDays(query, now); r != nil {
		return withWeights(r, dwLinear)
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
		if len(cw.dayWeekdays()) >= 2 {
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

