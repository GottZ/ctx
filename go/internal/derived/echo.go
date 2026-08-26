package derived

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// This file is an independent implementation of the echo gate that
// internal/topiclabel already runs on model-written cluster labels
// (topiclabel/guard.go:156-281 — echoIndex, normalizeEcho, hasNoWordBoundaries,
// newEchoIndex, gramKey, substantialPair, hit). It is a LIFT, not an import:
// topiclabel pulls in llm/embed/dispatch/llmlog, and derived is a leaf package
// (see the package doc). That topiclabel should one day consume derived
// instead of carrying its own copy is a noted merge point, not this wave.
//
// The thresholds below are the ones the label arm runs with; they are repeated
// here with their reasoning so this copy can be compared against the original
// line by line.

// echoMinTokenRunes is the length below which a WORD is grammar, not substance.
const echoMinTokenRunes = 3

// echoContentTokenRunes is the length at which a word carries topic substance.
// A substantial two-word window needs at least ONE such word — "auf die" and
// "in the" are shared by every German and English text in the corpus.
const echoContentTokenRunes = 6

// echoLongTokenRunes is the length at which a SINGLE word is substantial on
// its own — a hostname, an identifier, a key name, a product name.
const echoLongTokenRunes = 7

// echoCJKMinRunes is the containment threshold for scripts WITHOUT word
// separators. Three Han characters carry roughly the information of an English
// content word.
const echoCJKMinRunes = 3

// echoIndex is the deterministic echo gate: the normalised word bigrams, long
// single words and separator-less (CJK) tokens of the titles of the
// credentials/personal sources of one derived block.
//
// Deterministic on purpose. The thing being guarded is the output of a model,
// and guarding a model's output with another model would only move the failure
// one level up — the gate has to be a fact about strings.
type echoIndex struct {
	grams map[string]struct{}
	longs map[string]struct{}
	cjk   []string
}

// normalizeEcho reduces a string to comparable word tokens: NFKC first, then
// case folding, then tokenisation. Same order and same form as Normalize; the
// difference is only that this one returns tokens instead of a string, because
// the echo rules are about words and windows, not about substrings.
func normalizeEcho(s string) []string {
	s = strings.ToLower(norm.NFKC.String(s))
	return strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// hasNoWordBoundaries reports whether a token is written in a script that does
// not separate words — Han, Hiragana, Katakana, Hangul. Such a title tokenises
// into ONE token, so neither the bigram nor the long-token rule can ever fire
// on it and a full echo would pass unseen. Deliberately narrow: substring
// containment on Latin would reject "backup" for "backups" and turn the gate
// into a vocabulary ban.
func hasNoWordBoundaries(tok string) bool {
	for _, r := range tok {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
			unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
			return true
		}
	}
	return false
}

// newEchoIndex builds the gate from the sensitive titles of one block's source
// set. An empty title set yields an index that never fires — the correct
// behaviour for a source set that carries nothing sensitive.
func newEchoIndex(titles []string) echoIndex {
	idx := echoIndex{grams: map[string]struct{}{}, longs: map[string]struct{}{}}
	for _, title := range titles {
		toks := normalizeEcho(title)
		for i, tok := range toks {
			if utf8.RuneCountInString(tok) >= echoLongTokenRunes {
				idx.longs[tok] = struct{}{}
			}
			if hasNoWordBoundaries(tok) && utf8.RuneCountInString(tok) >= echoCJKMinRunes {
				idx.cjk = append(idx.cjk, tok)
			}
			if i+1 < len(toks) && substantialPair(tok, toks[i+1]) {
				idx.grams[gramKey(tok, toks[i+1])] = struct{}{}
			}
		}
	}
	return idx
}

// gramKey is the ORDER-FREE key of a two-word window: the pair sorted, not the
// pair as written. A model that reorders "Hetzner Storagebox" into "Storagebox
// Hetzner" has echoed the same substance, and an ordered key would have called
// that a different string — the cheapest possible evasion and the one a
// careless model performs by accident.
func gramKey(a, b string) string {
	if a > b {
		a, b = b, a
	}
	return a + " " + b
}

// substantialPair reports whether a two-word window carries substance: both
// words past the grammar floor, at least one of them a content word.
func substantialPair(a, b string) bool {
	na, nb := utf8.RuneCountInString(a), utf8.RuneCountInString(b)
	if na < echoMinTokenRunes || nb < echoMinTokenRunes {
		return false
	}
	return na >= echoContentTokenRunes || nb >= echoContentTokenRunes
}

// empty reports whether the index can never fire.
func (e echoIndex) empty() bool {
	return len(e.grams) == 0 && len(e.longs) == 0 && len(e.cjk) == 0
}

// hit reports whether text echoes indexed substance, and returns the offending
// fragment for the caller to account for — never to print.
func (e echoIndex) hit(text string) (string, bool) {
	toks := normalizeEcho(text)
	for i, tok := range toks {
		if _, ok := e.longs[tok]; ok {
			return tok, true
		}
		for _, s := range e.cjk {
			if strings.Contains(tok, s) || (utf8.RuneCountInString(tok) >= echoCJKMinRunes && strings.Contains(s, tok)) {
				return tok, true
			}
		}
		if i+1 >= len(toks) {
			continue
		}
		if _, ok := e.grams[gramKey(tok, toks[i+1])]; ok {
			return gramKey(tok, toks[i+1]), true
		}
	}
	return "", false
}
