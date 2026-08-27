package ctxcheckpoint

import (
	"strconv"
	"strings"
)

// transcriptMarker separates a part's boilerplate head from its body. The
// plugin renders it unconditionally (_render_direct_evidence), and all 5477
// listed parts of the live corpus carry it exactly once with the two trailing
// newlines — measured, not assumed.
const transcriptMarker = "## Direct transcript\n\n"

// headerPrefix and headerSep are the two literal pieces of a message header.
// The separator is U+2014 EM DASH and nothing else: over the whole live corpus
// the strict shape "### Message <digits> — user|assistant" matches all 1543
// header occurrences, so a lenient dash class would only widen the surface
// without covering a single real line.
const (
	headerPrefix = "### Message "
	headerSep    = " — "
)

// roleUser and roleAssistant are the only two roles the renderer emits. They
// are compared against, never parsed out of arbitrary text, so a header whose
// role is anything else is not a header at all and stays part of the body.
const (
	roleUser      = "user"
	roleAssistant = "assistant"
)

// carry is the message attribution that survives part boundaries.
//
// It exists because 93.7 % of the parts (5133 of 5477, measured) carry no
// header at all: the plugin's chunker cuts inside a single message whenever
// that message is longer than a third of its chunk limit, and every piece after
// the first is headerless. The attribution is therefore not IN the part — it is
// reconstructible only along "manifest -> source_block_ids in order -> last
// header seen". A reader that starts fresh per part reports an empty role for
// nine out of ten items.
//
// Carrying is sound because the chunker cuts either at a message boundary or
// inside the SAME message; it can never land in a different message without
// emitting that message's header first.
type carry struct {
	// Ordinal is the last header's message number, 0 before the first header.
	// It is a READABILITY field: the plugin enumerates the filtered message
	// list of one generation, so the same turn carries different ordinals in
	// two generations. Identity is (BlockID, ChunkIndex).
	Ordinal int

	// Role is "user", "assistant", or "" before the first header.
	Role string
}

// chunk is one prompt-ready piece of a part body, with the attribution that
// held at its first line.
type chunk struct {
	Text    string
	Ordinal int
	Role    string
}

// stripBoilerplate returns the part body — everything after the transcript
// marker — and whether the marker was found.
//
// The head averages 498 characters of form: a title, the compaction source
// evidence block and the transcript SHA-256. Dropping it is not cosmetic — it
// is the same form in every part, so it is exactly the text a substring quote
// gate would accept as "grounded" without the model having read a single line
// of actual material.
//
// The body is found by SEARCHING for the marker, never by an offset: the head's
// length is not constant (measured: 9 distinct marker positions over the 5477
// listed parts), so a fixed offset would cut into the transcript in some parts
// and leave form in others.
//
// A part without the marker yields ok=false and no body: guessing an offset
// into foreign text would hand the model a head it cannot attribute.
func stripBoilerplate(content string) (string, bool) {
	i := strings.Index(content, transcriptMarker)
	if i < 0 {
		return "", false
	}
	return content[i+len(transcriptMarker):], true
}

// parseHeader reads one line as a message header. It returns the ordinal, the
// role and whether the line is a header at all.
//
// The match is exact on both literals and on the role set. Anything else — a
// header-looking line inside a fenced code block, a truncated header at a
// plugin chunk edge — is body text, and treating it as a header would move the
// attribution of everything after it.
func parseHeader(line string) (int, string, bool) {
	rest, ok := strings.CutPrefix(line, headerPrefix)
	if !ok {
		return 0, "", false
	}
	num, role, ok := strings.Cut(rest, headerSep)
	if !ok || num == "" {
		return 0, "", false
	}
	if role != roleUser && role != roleAssistant {
		return 0, "", false
	}
	ord, err := strconv.Atoi(num)
	if err != nil || ord < 0 {
		return 0, "", false
	}
	return ord, role, true
}

// chunkBody splits a part body into chunks of at most maxRunes RUNES and
// returns them together with the attribution state at the end of the body, so
// the next part of the same manifest continues where this one stopped.
//
// Two properties are load-bearing and are asserted by the tests rather than
// described here:
//
//  1. The concatenation of all chunks is BYTE-IDENTICAL to body. Nothing is
//     trimmed at a cut, nothing is dropped, nothing overlaps. The predecessor
//     design head-capped a part at 4000 runes, which covered 11 % of a typical
//     36000-character body — and because the dedup ledger hashes the text that
//     was shown, the other 89 % would have been marked "seen" for good. A
//     silent, permanent loss disguised as a cost parameter.
//  2. Chunks do not overlap. The quote gate verifies against exactly the chunk
//     a model saw, and an overlap would enter the same text into the dedup
//     ledger twice.
//
// Runes, not bytes: a byte cut splits a multi-byte character and hands the
// model half of one, and the gate would then verify against text that no longer
// decodes the way the source read it.
func chunkBody(body string, maxRunes int, c carry) ([]chunk, carry) {
	if body == "" || maxRunes <= 0 {
		return nil, c
	}
	var out []chunk
	for rest := body; rest != ""; {
		// The attribution of this chunk is the state at its first line: a
		// chunk that STARTS with a header adopts it immediately, otherwise the
		// carried state holds.
		start := c
		if ord, role, ok := parseHeader(firstLine(rest)); ok {
			start = carry{Ordinal: ord, Role: role}
		}
		cut := cutPoint(rest, maxRunes)
		out = append(out, chunk{Text: rest[:cut], Ordinal: start.Ordinal, Role: start.Role})
		// Advance the carry over everything this chunk consumed, so the next
		// chunk (and the next part) sees the last header of the whole prefix.
		c = advanceCarry(rest[:cut], c)
		rest = rest[cut:]
	}
	return out, c
}

// firstLine returns s up to the first newline, without it.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// advanceCarry folds every header in s into c and returns the resulting state.
func advanceCarry(s string, c carry) carry {
	for rest := s; rest != ""; {
		line, tail, _ := strings.Cut(rest, "\n")
		if ord, role, ok := parseHeader(line); ok {
			c = carry{Ordinal: ord, Role: role}
		}
		rest = tail
	}
	return c
}

// cutPoint returns the byte offset at which s is split so the first piece holds
// at most maxRunes runes.
//
// The boundary is searched backwards from the rune cap, in this order: a
// message header, a paragraph break, a line break, and finally the hard rune
// cap. A candidate closer than a third of the cap is rejected and the hard cut
// is taken instead — the same floor the producing chunker uses, and the reason
// is the same: a boundary at position 40 of a 4000-rune budget buys readability
// for one chunk and pays for it with a thousand near-empty ones.
//
// Every returned offset lies AFTER the separator, never before it, which is what
// keeps the concatenation byte-identical.
func cutPoint(s string, maxRunes int) int {
	hard, whole := runeOffset(s, maxRunes)
	if whole {
		return hard
	}
	floor, _ := runeOffset(s, maxRunes/3)
	head := s[:hard]

	// A header must begin a line, so the search is for the newline in front of
	// it and the cut lands on the header's first byte.
	if i := strings.LastIndex(head, "\n"+headerPrefix); i >= floor {
		return i + 1
	}
	if i := strings.LastIndex(head, "\n\n"); i >= floor {
		return i + 2
	}
	if i := strings.LastIndexByte(head, '\n'); i >= floor {
		return i + 1
	}
	return hard
}

// runeOffset returns the byte offset of rune n in s and whether s has fewer
// than n runes (in which case the offset is len(s)).
func runeOffset(s string, n int) (int, bool) {
	if n <= 0 {
		return 0, false
	}
	count := 0
	for i := range s {
		if count == n {
			return i, false
		}
		count++
	}
	return len(s), true
}
