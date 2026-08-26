package derived

import (
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/GottZ/ctx/internal/promptguard"
	"github.com/GottZ/ctx/internal/sensitivity"
)

// Claim is one model-written line of a derived block: an assertion about
// exactly ONE source block, carried by a verbatim quote from that block.
//
// The class "computed" (aggregates, counts, time spans, absence statements —
// §4.4.0) is deliberately NOT expressible here: it has no quote, it is
// produced by Go from the source set, and it enters the block through
// RenderBlock, never through a model.
type Claim struct {
	Claim    string `json:"claim"`
	Quote    string `json:"quote"`
	SourceID string `json:"source_id"`
	Kind     string `json:"kind"`
}

// Source is one resolved source block as the gate needs it: the ORIGINAL text
// (never a map-stage output, §4.4.2 rule 1), the title, and the sensitivity
// that decides whether the title feeds the echo index.
//
// The design sketch in §4.5.1 writes CiteGate(claims, sources map[string]string).
// A bare id-to-text map cannot carry G7 (which needs the TITLES of the
// credentials/personal sources) and cannot carry G0 (which needs the block's
// declared id list, a different set from the resolved one). §7 W01-1 makes
// both of those mandatory gates, so the surface follows the gates: the map
// carries Source, and the declared ids are a third argument.
type Source struct {
	Title       string
	Content     string
	Sensitivity string // credentials | personal | internal | public
}

// Verdict is the outcome of one CiteGate run over ALL claims of one block.
type Verdict struct {
	// Kept are the surviving claims in input order.
	Kept []Claim

	// Rejects counts discarded lines per gate. It always carries exactly the
	// eight keys g0…g7, zeros included — it is written verbatim into
	// provenance.coverage.rejects (§3.2), where a missing key and a zero must
	// not be distinguishable.
	Rejects map[string]int
}

// GateKeys are the reject buckets, in gate order. Exactly these, no others.
var GateKeys = []string{"g0", "g1", "g2", "g3", "g4", "g5", "g6", "g7"}

// newRejects returns the zeroed reject map.
func newRejects() map[string]int {
	m := make(map[string]int, len(GateKeys))
	for _, k := range GateKeys {
		m[k] = 0
	}
	return m
}

// SourcesCovered is the number of distinct source ids among the surviving
// claims — provenance.coverage.sources_covered (§3.2).
func (v Verdict) SourcesCovered() int { return distinctSources(v.Kept) }

// distinctSources counts the distinct source ids of a claim set. Shared with
// Validate, which re-derives sources_covered from the kept claims rather than
// trusting the number the writer reports (V9).
func distinctSources(claims []Claim) int {
	seen := make(map[string]struct{}, len(claims))
	for _, c := range claims {
		seen[c.SourceID] = struct{}{}
	}
	return len(seen)
}

// redactionMarkers is the G4 negative list: strings that the pipeline itself
// writes INTO source text, and that a quote may therefore contain without the
// quote proving anything.
//
// [REDACTED] comes from the checkpoint redaction, which REPLACES rather than
// deletes (_GENERIC_SECRET_RE and _BEARER_RE in the ctx_checkpoint plugin), so
// it is a genuine substring of the source text and passes G3 cleanly.
// "[... truncated]" is promptguard.Assemble's truncation marker
// (promptguard/assemble.go:95). A quote whose substance is half redaction
// marks is a citation about nothing.
var redactionMarkers = []string{"[redacted", "[... truncated]"}

// CiteGate runs G0–G7 over every claim of ONE block, in the order of the table
// in §4.4.1 (cheap before expensive), and returns the survivors plus the
// per-gate reject counts.
//
// It runs exactly ONCE, immediately before the write, over ALL claims of the
// block, and it always verifies against the ORIGINAL source content from
// store.ResolveSources — never against a map-stage output (§4.4.2). Map stages
// may gate additionally as a cheap prefilter, but the binding verdict is this
// one.
//
// declaredIDs is provenance.source_block_ids of the block being written; it is
// what G0 checks against. sources is the resolved set whose ORIGINAL text G1,
// G3 and G7 use. The two sets differ on purpose: a source that is declared but
// gone from the own scope (SourceSet.MissingInScope, §4.5.4) is in declaredIDs
// and not in sources, and a source id that a reduce step dragged in from a
// wider map run is in sources and not in declaredIDs.
//
// CALLER CONTRACT, and G7 depends on it: sources must carry EVERY declared
// source that resolves, not just those a claim happens to cite. The echo index
// is built from the titles of the credentials/personal entries of this map, so
// a sensitive source the caller leaves out contributes no index entry and
// claims about OTHER sources can echo its title unchecked. The Validate side
// states the same requirement as a clause (checkFactsCoverDeclared); here it
// cannot be verified, because "resolvable" is knowledge only the DB layer has.
//
// A rejected line is normal. ALL lines of a call rejected although the call
// delivered some is a breaker fault of the producing arm — that decision
// belongs to the arm, not here.
//
// The rejected TEXTS are deliberately not returned: a line may have failed G6
// precisely because it carries a secret. Callers log the class, never the text.
func CiteGate(claims []Claim, sources map[string]Source, declaredIDs []string) Verdict {
	v := Verdict{Rejects: newRejects()}
	declared := make(map[string]struct{}, len(declaredIDs))
	for _, id := range declaredIDs {
		declared[id] = struct{}{}
	}
	echo := newEchoIndex(sensitiveTitles(sources))

	for _, c := range claims {
		if key, bad := screenClaim(c, sources, declared, echo); bad {
			v.Rejects[key]++
			continue
		}
		v.Kept = append(v.Kept, c)
	}
	return v
}

// screenClaim runs the eight checks in order and returns the first failing
// gate key. Split out of CiteGate so the loop stays flat and each gate stays
// one readable function.
func screenClaim(c Claim, sources map[string]Source, declared map[string]struct{}, echo echoIndex) (string, bool) {
	src, resolved := sources[c.SourceID]
	checks := []struct {
		key  string
		fail func() bool
	}{
		{"g0", func() bool { return failG0(c, declared) }},
		{"g1", func() bool { return failG1(resolved) }},
		{"g2", func() bool { return failG2(c) }},
		{"g3", func() bool { return failG3(c, src) }},
		{"g4", func() bool { return failG4(c) }},
		{"g5", func() bool { return failG5(c) }},
		{"g6", func() bool { return failG6(c) }},
		{"g7", func() bool { return failG7(c, echo) }},
	}
	for _, ch := range checks {
		if ch.fail() {
			return ch.key, true
		}
	}
	return "", false
}

// failG0 — the reduce guard (§4.4.2 rule 3): a claim whose source_id is not in
// provenance.source_block_ids of THIS block is discarded. This is V14 on the
// line level: without it a reduce step could carry a citation about a block
// the written provenance never names, and the block would assert something no
// reader can trace back.
func failG0(c Claim, declared map[string]struct{}) bool {
	_, ok := declared[c.SourceID]
	return !ok
}

// failG1 — the source must be resolvable to ORIGINAL text. Unresolvable means
// there is nothing to verify against, and "cannot verify" is a reject, never a
// pass (§4.4.2 rule 1).
func failG1(resolved bool) bool { return !resolved }

// failG2 — length floor, MinQuoteRunes runes, counted in runes and not bytes.
func failG2(c Claim) bool {
	return utf8.RuneCountInString(c.Quote) < MinQuoteRunes
}

// failG3 — containment: the normalised quote must be a substring of the
// normalised ORIGINAL source content. This is the check that makes the line a
// citation instead of an assertion.
func failG3(c Claim, src Source) bool {
	return !strings.Contains(Normalize(src.Content), Normalize(c.Quote))
}

// failG4 — redaction marks are not evidence (§4.4.1 change 2). Checked on the
// normalised quote so a full-width or differently-cased spelling of the marker
// does not slip past; the list itself is stored lower-cased.
func failG4(c Claim) bool {
	q := Normalize(c.Quote)
	for _, m := range redactionMarkers {
		if strings.Contains(q, m) {
			return true
		}
	}
	return false
}

// failG5 — prompt-structure integrity: neither claim nor quote may carry a
// control token of the promptguard marker table. Neutralize reports how many
// it had to break; anything above zero is a line that would have tried to
// speak as prompt structure.
func failG5(c Claim) bool {
	if _, broken := promptguard.Neutralize(c.Claim); broken > 0 {
		return true
	}
	_, broken := promptguard.Neutralize(c.Quote)
	return broken > 0
}

// failG6 — structured credentials, nothing else (§5.3). sensitivity.Scan is
// the tree's detector; it fires on AWS key ids, PEM private-key headers, JWTs,
// vendor token prefixes, high-entropy secret assignments and high-entropy
// base64/hex blobs, and on nothing that is merely ABOUT a secret.
func failG6(c Claim) bool {
	_, hit := sensitivity.Scan(c.Claim + " " + c.Quote)
	return hit
}

// failG7 — the echo index over the titles of the credentials/personal sources
// (§4.4.1 G7). It closes the hole G6 leaves open: a secret that was already
// summarised INTO a title is no longer a structured credential and Scan will
// not see it, but a derived line that repeats that title has still carried the
// substance one abstraction level further.
//
// Screened is claim + quote, the same pair G5 and G6 screen: a verbatim quote
// out of a credentials block echoes its title just as effectively as a
// paraphrase does.
func failG7(c Claim, echo echoIndex) bool {
	if echo.empty() {
		return false
	}
	_, hit := echo.hit(c.Claim + " " + c.Quote)
	return hit
}

// sensitiveTitles collects the titles of the credentials/personal sources, in
// a deterministic order so the index is reproducible.
func sensitiveTitles(sources map[string]Source) []string {
	ids := make([]string, 0, len(sources))
	for id, s := range sources {
		if s.Sensitivity == SensitivityCredentials || s.Sensitivity == SensitivityPersonal {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	titles := make([]string, 0, len(ids))
	for _, id := range ids {
		titles = append(titles, sources[id].Title)
	}
	return titles
}
