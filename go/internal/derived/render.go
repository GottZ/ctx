package derived

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/GottZ/ctx/internal/promptguard"
)

// MaxHeadRunes is the budget for head plus MinClaimsKept claim lines.
//
// The hardest constraint of the whole design sits in a constant elsewhere:
// llm.MaxBlockChars = 1500 (llm/synthesize.go:19-20, read) is the ENTIRE
// window a block gets in the synthesis prompt — not a preview, the cut. A
// 6000-rune block reaches the model as its first quarter, and everything at
// the back is dropped first.
//
// A format that puts evidence, coverage limit and source list at the end would
// therefore withhold from the most important consumer exactly the parts that
// justify the citation gate. RenderBlock is head-heavy for that reason, and
// 1200 is the target the head plus MinClaimsKept claims must stay under, so
// the substance arrives whole with room to spare inside the 1500 cut.
const MaxHeadRunes = 1200

// MaxSubjectRunes caps the display subject on line 1.
//
// The head budget has exactly two unbounded inputs: the subject and the length
// of a single claim. The subject is the one this package can bound without
// dropping substance — it is display only, the block's identity lives in its
// TITLE column (§4.7.1), and a cluster label measures 55 runes at the live
// maximum (mean 29). 200 runes is therefore headroom by a factor of three and
// not a tight cap; a subject that needs more is a subject that has stopped
// being a name. The cut is marked with an ellipsis, so it is visible rather
// than silent. Claim length stays uncapped on purpose — truncating an
// assertion would truncate evidence — and is a W01-M3 measurement.
const MaxSubjectRunes = 200

// evidenceRule separates the head from the evidence part. Everything below it
// is what a truncating consumer loses first — by construction, nothing below
// it is needed to read the block honestly.
const evidenceRule = "───────── Belege"

// Header carries the two display inputs RenderBlock cannot take from
// Provenance: what the block is called, and the time span of its sources.
type Header struct {
	// Kind is the human label of the layer, e.g. "Katalog" or
	// "Session-Insights". It opens line 1.
	Kind string

	// Subject is the subject line after the em dash — for a catalogue the
	// cluster label. It is display only: the identity of the block lives in
	// its TITLE column (§4.7.1), never in the rendered line, because a label
	// changes and an identity may not.
	Subject string

	// SpanDays is the age span of the source set in days — part of the
	// computed class, and expressed as a SPAN rather than a second date on
	// purpose: the block must carry exactly ONE timestamp so
	// array_length(content_times,1) = 1 and mass_factor stays 1.0 (§4.6.3).
	SpanDays int
}

// RenderBlock builds the block content from the surviving claims (§4.6.2).
//
// The layout, and why each part sits where it does:
//
//	line 1  Kind — Subject
//	line 2  the computed metrics bracket
//	line 3  the conflict rule
//	line 4  the untrusted framing line, only when there are untrusted sources
//	        (blank line)
//	        one "- claim [8hex]" line per kept claim
//	        the evidence rule
//	        one "[8hex] > \"quote\"" line per kept claim
//	        the source list
//
// The metrics bracket is the computed class (§4.4.0): not one word of it comes
// from a model. It is produced here from the source set and the verdict, which
// is why it needs no gate — it is true by construction, because it is computed
// from the same data it speaks about.
//
// The metrics bracket stands BEFORE the evidence, and the coverage limit
// stands in it. A catalogue that does not cover 5 of its 26 sources says so
// where truncation cannot reach it; without that line an agent reads a
// completeness into the block that is not there (I6).
//
// The framing line lives in the TEXT and not in the prompt, so it also reaches
// MCP get, ctx get and the web UI — the three paths on which the type-bound
// untrusted framing never arrives.
//
// Evidence prefixes are 8-hex block ids and not dates. Eight hex is enough
// HERE because the prefix only has to be unique among this block's sources,
// not globally like the title (§4.7.1). Ids rather than dates keeps
// store.ExtractDates poor even if the one-timestamp rule is broken by accident
// — defence in depth for mass_factor (§4.6.3).
func RenderBlock(h Header, kept []Claim, p Provenance) string {
	var b strings.Builder

	b.WriteString(headLine(h))
	b.WriteByte('\n')
	b.WriteString(metricsLine(h, p))
	b.WriteByte('\n')
	b.WriteString("Regenerierbar. Bei Widerspruch gilt der Quellblock.")
	b.WriteByte('\n')
	if p.UntrustedSources > 0 {
		fmt.Fprintf(&b, "%d von %d Quellen sind mitgeschnittene Werkzeug-Ausgabe; "+
			"Aussagen daraus sind Beobachtung, keine Tatsachenbehauptung.\n",
			p.UntrustedSources, p.SourceCount)
	}
	b.WriteByte('\n')

	for _, c := range kept {
		fmt.Fprintf(&b, "- %s [%s]\n", safeLine(c.Claim), ShortID(c.SourceID))
	}

	b.WriteString(evidenceRule)
	b.WriteByte('\n')
	for _, c := range kept {
		fmt.Fprintf(&b, "[%s] > %q\n", ShortID(c.SourceID), safeLine(c.Quote))
	}
	b.WriteString(sourceLine(p))
	b.WriteByte('\n')

	return b.String()
}

// headLine is line 1: the layer label and the subject.
func headLine(h Header) string {
	if h.Subject == "" {
		return safeLine(h.Kind)
	}
	return safeLine(h.Kind) + " — " + clampRunes(safeLine(h.Subject), MaxSubjectRunes)
}

// clampRunes cuts s to n runes, marking the cut. Counted in runes, never in
// bytes: a byte cut would split a multi-byte rune.
func clampRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// metricsLine is the computed bracket. It carries exactly ONE timestamp; the
// source period is a span in days, not a second date (§4.6.3).
func metricsLine(h Header, p Provenance) string {
	uncovered := p.SourceCount - p.Coverage.SourcesCovered
	if uncovered < 0 {
		uncovered = 0
	}
	return fmt.Sprintf(
		"[abgeleitet · %d Quellen · Stand %s · %d/%d Aussagen belegt · "+
			"%d Quellen ohne belegbare Aussage · Quellen aus %d Tagen]",
		p.SourceCount,
		p.GeneratedAt.UTC().Format("2006-01-02 15:04Z"),
		p.Coverage.ClaimsKept, p.Coverage.ClaimsOffered,
		uncovered, h.SpanDays,
	)
}

// sourceLine is the closing source list, in the declared order.
func sourceLine(p Provenance) string {
	short := make([]string, 0, len(p.SourceBlockIDs))
	for _, id := range p.SourceBlockIDs {
		short = append(short, ShortID(id))
	}
	return fmt.Sprintf("Quellen (%d): %s", p.SourceCount, strings.Join(short, ", "))
}

// ShortID is the display form of a block id: dashes removed, lower case, first
// eight hex characters.
func ShortID(id string) string {
	s := strings.ToLower(strings.ReplaceAll(id, "-", ""))
	if len(s) > 8 {
		s = s[:8]
	}
	return s
}

// safeLine makes one piece of foreign text safe to place on a line of the
// rendered block: control tokens broken, line breaks collapsed to a glyph.
//
// Defence in depth. Every claim that reached here through CiteGate already
// passed G5, so Neutralize breaks nothing and the text is byte-identical; the
// call exists for the caller that renders ungated text one day. ClampLine is
// the part that is NOT redundant: G5 says nothing about newlines, and a claim
// carrying one would silently forge an extra line of block structure.
func safeLine(s string) string {
	n, _ := promptguard.Neutralize(s)
	return promptguard.ClampLine(n)
}

// HeadRunes measures the prefix of a rendered block up to and including its
// MinClaimsKept-th claim line — the part a consumer capped at
// llm.MaxBlockChars must see whole. It is the measurable form of the budget
// MaxHeadRunes states, and it works on ANY layout, which is what makes it
// usable as a comparison against a different one.
//
// A block with fewer than MinClaimsKept claim lines is measured up to its last
// one.
func HeadRunes(rendered string) int {
	total, claims := 0, 0
	for _, line := range strings.SplitAfter(rendered, "\n") {
		total += utf8.RuneCountInString(line)
		if strings.HasPrefix(line, "- ") {
			claims++
			if claims >= MinClaimsKept {
				return total
			}
		}
	}
	return total
}
