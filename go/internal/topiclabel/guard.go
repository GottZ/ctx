package topiclabel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

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
//
// A markdown code fence around the object is stripped BEFORE the structural
// gate (stripCodeFence): several model families (gemma-4 measured at 13/23
// fenced answers, 2026-08-16) wrap otherwise contract-clean JSON in ```json
// fences. The fence is presentation, not structure — every check above still
// runs on the unwrapped bytes, and the rerank parser has always been
// equivalently tolerant (regex extraction from surrounding text).
func parseLabel(raw string) (string, rejection) {
	raw = stripCodeFence(strings.TrimSpace(raw))
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
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

// stripCodeFence removes ONE markdown code fence that wraps the ENTIRE input
// (```-line, body, closing ```). Anything else — text beside the fence, a
// missing closing fence, no fence at all — returns the input unchanged, so a
// fenced-plus-commentary answer still fails the structural gate. The opening
// line is dropped wholesale: whether it reads ``` or ```json, it is fence
// syntax, and the unwrapped body still has to survive every structural check.
func stripCodeFence(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	rest := s[3:]
	i := strings.IndexByte(rest, '\n')
	if i < 0 {
		return s
	}
	body := strings.TrimSpace(rest[i+1:])
	if !strings.HasSuffix(body, "```") {
		return s
	}
	return strings.TrimSpace(strings.TrimSuffix(body, "```"))
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
// own — a hostname, an identifier, a key name, a product name.
//
// Lowered from 12 to 7 (K2-1). Twelve was chosen to avoid false positives on
// shared vocabulary, but it left the whole middle of the identifier range
// uncovered: "vaultkey", "pgbouncer", "grafana1" are all seven to nine runes and
// all far more likely to be a name copied out of a title than a word two texts
// happen to share. Seven is the shortest length at which that stays true for the
// languages this corpus is written in; below it German and English common nouns
// dominate.
const echoLongTokenRunes = 7

// echoCJKMinRunes is the containment threshold for scripts WITHOUT word
// separators. Three Han characters carry roughly the information of an English
// content word, so the bar sits where echoContentTokenRunes sits for Latin.
const echoCJKMinRunes = 3

// echoIndex is the deterministic echo gate: the normalised word bigrams, long
// single words and separator-less (CJK) tokens of the titles of the
// CREDENTIALS/PERSONAL core blocks.
//
// Deterministic on purpose. The thing being guarded is the output of a model,
// and guarding a model's output with another model would only move the failure
// one level up — the gate has to be a fact about strings.
type echoIndex struct {
	grams map[string]struct{}
	longs map[string]struct{}
	cjk   []string
}

// normalizeEcho reduces a string to comparable word tokens.
//
// NFKC FIRST, then case folding, then tokenisation (K2-1). The order matters
// and so does the form: a title and the label a model builds from it routinely
// differ in Unicode normalisation alone — "Straße" vs "Strasse" stays
// distinct (that is a real spelling difference), but "é" vs "é",
// a full-width "ｋｅｙ" vs "key" and a ligature "ﬁ" vs "fi" are the SAME text
// rendered differently, and a byte comparison would call them different. NFKC
// collapses exactly those. Without it the gate is trivially evaded by the
// SUMMARISER — not by an attacker; the model is not adversarial here, it is
// careless — simply by echoing a title in a different normalisation form.
//
// Everything that is not a letter or a digit separates tokens: punctuation,
// casing and separators differ between a title and a label built from it, the
// words do not.
func normalizeEcho(s string) []string {
	s = strings.ToLower(norm.NFKC.String(s))
	return strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// hasNoWordBoundaries reports whether a token is written in a script that does
// not separate words — Han, Hiragana, Katakana, Hangul. Such a title tokenises
// into ONE token, so neither the bigram nor the long-token rule can ever fire on
// it, and a full echo would pass unseen. Deliberately narrow: substring
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

// gramKey is the ORDER-FREE key of a two-word window (K2-1): the pair sorted,
// not the pair as written. A summariser that reorders "Hetzner Storagebox" into
// "Storagebox Hetzner" has echoed the same substance, and an ordered key would
// have called that a different string — which is the cheapest possible evasion
// and the one a careless model performs by accident.
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

// hit reports whether the label echoes indexed substance, and returns the
// offending fragment for the caller to account for — never to print (K2-2).
func (e echoIndex) hit(label string) (string, bool) {
	toks := normalizeEcho(label)
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

// echoFingerprint renders a rejected fragment for the log WITHOUT the text
// (K2-2): its rune length plus a short sha256 prefix.
//
// A fragment suspected of echoing a credentials title is exactly the string not
// to write into a log file that a wider audience reads than the block itself.
// The fingerprint still supports the two questions an operator actually has —
// "is this the same rejection over and over" and "how much substance was it" —
// without reproducing the substance.
func echoFingerprint(fragment string) string {
	if fragment == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(fragment))
	return fmt.Sprintf("len=%d sha256=%s", utf8.RuneCountInString(fragment), hex.EncodeToString(sum[:])[:12])
}
