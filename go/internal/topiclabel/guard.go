package topiclabel

import (
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/GottZ/ctx/internal/promptguard"
	"github.com/GottZ/ctx/internal/sensitivity"
)

// maxLabelRunes mirrors the gct_label_len CHECK. Runes, not bytes: the
// constraint counts characters, so a 120-umlaut label weighs 240 bytes and
// still has to pass.
const maxLabelRunes = 120

// rejection classifies why a model answer did not become a label. The three
// values are counted separately and reported — a silent filter is the one thing
// the UX condition of decision E4-02 forbids.
type rejection string

const (
	rejectNone      rejection = ""
	rejectStructure rejection = "structure" // not the agreed shape
	rejectScan      rejection = "scan"      // sensitivity.Scan hit (A01-3 stage 1)
	rejectEcho      rejection = "echo"      // n-gram echo of a sensitive title (A01-3 stage 2)
)

// labelEnvelope is the whole agreed answer contract.
type labelEnvelope struct {
	Label string `json:"label"`
}

// parseLabel validates a model answer STRUCTURALLY and returns the accepted
// label. No confidence is read and none is asked for: project empiricism
// (session 24, dream v3) says the model's self-assessment is unusable as a
// gate, so the gate is the shape of the answer plus a hard rune cap.
//
// Accepted iff ALL hold:
//
//  1. valid JSON object with EXACTLY the key "label" — an answer that invents
//     fields did not follow the contract, and a contract that tolerates extra
//     fields cannot tell "the model added a note" from "the model was steered";
//  2. non-empty after promptguard.ClampLine and TrimSpace;
//  3. at most maxLabelRunes runes;
//  4. promptguard.Neutralize breaks NOTHING out of it — a label carrying a
//     control marker is a prompt-injection artefact, not a name.
func parseLabel(raw string) (string, rejection) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &fields); err != nil {
		return "", rejectStructure
	}
	if len(fields) != 1 {
		return "", rejectStructure
	}
	if _, ok := fields["label"]; !ok {
		return "", rejectStructure
	}
	var env labelEnvelope
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &env); err != nil {
		return "", rejectStructure
	}
	label := strings.TrimSpace(promptguard.ClampLine(env.Label))
	if label == "" || utf8.RuneCountInString(label) > maxLabelRunes {
		return "", rejectStructure
	}
	if _, broken := promptguard.Neutralize(label); broken > 0 {
		return "", rejectStructure
	}
	return label, rejectNone
}

// screenLabel applies the two UNCONDITIONAL halves of the label output
// hardening (Amendment A01-3 / decision E4-02) to an already structurally valid
// label. Stage 3 — credentials cores never reach a model at all — is the opt-in
// config knob and lives in the run loop, not here.
//
// The threat this closes is drift across two LLM abstraction levels: block
// content was summarised into a title by one pipeline, and the title is now
// summarised into a public map label by another. Neither step is a secret
// boundary, so a secret that survived into a title would ride into a name that
// the map shows to everyone who can read the scope.
func screenLabel(label string, echo echoIndex) (rejection, string) {
	if m, hit := sensitivity.Scan(label); hit {
		return rejectScan, m.Kind
	}
	if gram, hit := echo.hit(label); hit {
		return rejectEcho, gram
	}
	return rejectNone, ""
}

// echoMinTokenRunes is the length below which a WORD is grammar, not substance.
const echoMinTokenRunes = 3

// echoContentTokenRunes is the length at which a word carries topic substance.
// A substantial two-word window needs at least ONE such word — "auf die" and
// "in the" are shared by every German and English text in the corpus, and a gate
// that treats them as an echo would reject abstract names for the wrong reason
// and quietly disable the feature.
const echoContentTokenRunes = 6

// echoLongTokenRunes is the length at which a SINGLE word is substantial on its
// own — a hostname, an identifier, a key name. Below it a lone word match is
// far more likely to be vocabulary the two texts simply share.
const echoLongTokenRunes = 12

// echoIndex is the deterministic echo gate: the normalised word bigrams and
// long single words of the titles of the CREDENTIALS/PERSONAL core blocks.
//
// Deterministic on purpose. The thing being guarded is the output of a model,
// and guarding a model's output with another model would only move the failure
// one level up — the gate has to be a fact about strings.
type echoIndex struct {
	grams map[string]struct{}
	longs map[string]struct{}
}

// normalizeEcho lowercases and reduces to word tokens: everything that is not a
// letter or a digit is a separator. That is what makes the gate robust against
// the obvious evasions of the SUMMARISER (not of an attacker — the model is not
// adversarial here, it is careless): punctuation, casing and separators differ
// between a title and a label built from it, the words do not.
func normalizeEcho(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// newEchoIndex builds the gate from the sensitive titles of one topic's core.
// An empty title set yields an index that never fires — the correct behaviour
// for a topic whose core carries nothing sensitive.
func newEchoIndex(titles []string) echoIndex {
	idx := echoIndex{grams: map[string]struct{}{}, longs: map[string]struct{}{}}
	for _, title := range titles {
		toks := normalizeEcho(title)
		for i, tok := range toks {
			if utf8.RuneCountInString(tok) >= echoLongTokenRunes {
				idx.longs[tok] = struct{}{}
			}
			if i+1 < len(toks) && substantialPair(tok, toks[i+1]) {
				idx.grams[tok+" "+toks[i+1]] = struct{}{}
			}
		}
	}
	return idx
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

// hit reports the first substantial echo found in the label, and what it was —
// the counter alone would say a filter fired, the fragment says why, and that
// difference is what makes the rejection reviewable instead of mysterious.
func (e echoIndex) hit(label string) (string, bool) {
	toks := normalizeEcho(label)
	for i, tok := range toks {
		if _, ok := e.longs[tok]; ok {
			return tok, true
		}
		if i+1 >= len(toks) {
			continue
		}
		gram := tok + " " + toks[i+1]
		if _, ok := e.grams[gram]; ok {
			return gram, true
		}
	}
	return "", false
}
