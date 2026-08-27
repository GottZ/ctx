package ctxcheckpoint

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// liveBoilerplate reproduces the shape of a part's head as the plugin renders
// it. Its exact length does not matter — what matters is that it carries both
// marker strings the gate checks for and that it sits in front of the
// transcript marker.
const liveBoilerplate = "# Compaction checkpoint 20260712_205012_837f2c part 3\n\n" +
	"## Compaction source evidence\n\n" +
	"- Transcript SHA-256: 6f1c2d3e4a5b60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f9\n" +
	"- Source blocks: 12\n\n"

func part(body string) string { return liveBoilerplate + transcriptMarker + body }

// TestStripBoilerplateRemovesTheHead is the boilerplate gate: the head is
// identical in every part, so it is exactly the text a substring quote gate
// would accept as grounded without the model having read any material.
func TestStripBoilerplateRemovesTheHead(t *testing.T) {
	body := "### Message 1 — user\n\nreal material\n"
	got, ok := stripBoilerplate(part(body))
	if !ok {
		t.Fatal("stripBoilerplate: marker not found in a well-formed part")
	}
	if got != body {
		t.Errorf("body mismatch:\n got %q\nwant %q", got, body)
	}
	for _, marker := range []string{"Compaction source evidence", "Transcript SHA-256"} {
		if strings.Contains(got, marker) {
			t.Errorf("stripped body still contains boilerplate marker %q", marker)
		}
	}
}

// TestStripBoilerplateRejectsPartWithoutMarker: a part that does not carry the
// marker is not the agreed shape, and guessing an offset into foreign text
// would hand the model a head it cannot attribute.
func TestStripBoilerplateRejectsPartWithoutMarker(t *testing.T) {
	if body, ok := stripBoilerplate("no marker anywhere"); ok || body != "" {
		t.Errorf("want ok=false body=\"\", got ok=%v body=%q", ok, body)
	}
}

// TestParseHeaderStrictShape pins the header grammar. The separator is U+2014
// EM DASH and the role set is closed: over the whole live corpus the strict
// shape matches all 1543 header occurrences, so anything looser only widens the
// surface on which body text can be mistaken for an attribution change.
func TestParseHeaderStrictShape(t *testing.T) {
	tests := []struct {
		line string
		ord  int
		role string
		ok   bool
	}{
		{"### Message 1 — user", 1, "user", true},
		{"### Message 42 — assistant", 42, "assistant", true},
		{"### Message 7 - user", 0, "", false},       // hyphen, not em dash
		{"### Message 7 – user", 0, "", false},       // en dash
		{"### Message 7 — tool", 0, "", false},       // role outside the set
		{"### Message x — user", 0, "", false},       // non-numeric ordinal
		{"### Message  — user", 0, "", false},        // missing ordinal
		{"#### Message 7 — user", 0, "", false},      // wrong heading level
		{"prefix ### Message 7 — user", 0, "", false}, // not at line start
	}
	for _, tc := range tests {
		ord, role, ok := parseHeader(tc.line)
		if ok != tc.ok || ord != tc.ord || role != tc.role {
			t.Errorf("parseHeader(%q) = (%d, %q, %v), want (%d, %q, %v)",
				tc.line, ord, role, ok, tc.ord, tc.role, tc.ok)
		}
	}
}

// TestRoleCarryAcrossPartBoundary is the carry gate. 5133 of 5477 live parts
// carry no header at all, so a reader that starts fresh per part reports an
// empty role for the overwhelming majority of the corpus.
func TestRoleCarryAcrossPartBoundary(t *testing.T) {
	first := "### Message 12 — assistant\n" + strings.Repeat("a", 100)
	second := strings.Repeat("b", 100) // headerless continuation
	if strings.Contains(second, headerPrefix) {
		t.Fatal("fixture second part must be headerless")
	}

	var c carry
	got1, c := chunkBody(first, 4000, c)
	got2, c := chunkBody(second, 4000, c)

	if len(got1) != 1 || got1[0].Role != "assistant" || got1[0].Ordinal != 12 {
		t.Fatalf("part 1: got %d chunks, role=%q ordinal=%d; want 1 chunk assistant/12",
			len(got1), got1[0].Role, got1[0].Ordinal)
	}
	if len(got2) != 1 {
		t.Fatalf("part 2: got %d chunks, want 1", len(got2))
	}
	if got2[0].Role != "assistant" || got2[0].Ordinal != 12 {
		t.Errorf("carry lost across the part boundary: role=%q ordinal=%d, want assistant/12",
			got2[0].Role, got2[0].Ordinal)
	}
	if c.Role != "assistant" || c.Ordinal != 12 {
		t.Errorf("returned carry = %+v, want {12 assistant}", c)
	}
}

// TestRoleCarryUpdatesOnNewHeader: the carry holds until a header replaces it,
// and a chunk that STARTS with a header adopts it immediately.
func TestRoleCarryUpdatesOnNewHeader(t *testing.T) {
	body := "### Message 1 — user\nfrom user\n### Message 2 — assistant\nfrom assistant\n"
	got, c := chunkBody(body, 4000, carry{})
	if len(got) != 1 {
		t.Fatalf("got %d chunks, want 1", len(got))
	}
	// One chunk holds both messages, so it is attributed to the state at its
	// first line — the identity of an item is (BlockID, ChunkIndex), not the
	// ordinal.
	if got[0].Ordinal != 1 || got[0].Role != "user" {
		t.Errorf("chunk attribution = %d/%q, want 1/user", got[0].Ordinal, got[0].Role)
	}
	if c.Ordinal != 2 || c.Role != "assistant" {
		t.Errorf("carry after body = %+v, want {2 assistant}", c)
	}
}

// TestChunkingIsLossless is the chunking gate: N items, each at most maxRunes
// RUNES, and their concatenation byte-identical to the body. A head cap would
// cover 11 % of a live body and — because the dedup ledger hashes what was
// shown — mark the rest as seen for good.
func TestChunkingIsLossless(t *testing.T) {
	// A body in the live shape: ~36000 characters of paragraphs, one header at
	// the top, no header after it (the 93.7 % case).
	var sb strings.Builder
	sb.WriteString("### Message 3 — user\n\n")
	for sb.Len() < 36000 {
		sb.WriteString(strings.Repeat("x", 79))
		sb.WriteString("\n\n")
	}
	body := sb.String()

	chunks, _ := chunkBody(body, 4000, carry{})
	if len(chunks) < 2 {
		t.Fatalf("got %d chunks for a %d-rune body, want several", len(chunks), utf8.RuneCountInString(body))
	}

	var joined strings.Builder
	for i, ch := range chunks {
		if n := utf8.RuneCountInString(ch.Text); n > 4000 {
			t.Errorf("chunk %d holds %d runes, cap is 4000", i+1, n)
		}
		if ch.Text == "" {
			t.Errorf("chunk %d is empty", i+1)
		}
		joined.WriteString(ch.Text)
	}
	if joined.String() != body {
		t.Errorf("concatenation is not byte-identical to the body: got %d bytes, want %d",
			joined.Len(), len(body))
	}
	t.Logf("body %d runes -> %d chunks", utf8.RuneCountInString(body), len(chunks))
}

// TestChunkingCutsAtHeaderBoundaryFirst pins the cut preference. The header
// must begin the following chunk, which is also what keeps the concatenation
// exact.
func TestChunkingCutsAtHeaderBoundaryFirst(t *testing.T) {
	body := strings.Repeat("a", 3000) + "\n### Message 9 — assistant\n" + strings.Repeat("b", 3000)
	chunks, _ := chunkBody(body, 4000, carry{})
	if len(chunks) != 2 {
		t.Fatalf("got %d chunks, want 2", len(chunks))
	}
	if !strings.HasPrefix(chunks[1].Text, headerPrefix) {
		t.Errorf("second chunk does not start at the header: %.40q", chunks[1].Text)
	}
	if chunks[1].Ordinal != 9 || chunks[1].Role != "assistant" {
		t.Errorf("second chunk attribution = %d/%q, want 9/assistant", chunks[1].Ordinal, chunks[1].Role)
	}
	if chunks[0].Text+chunks[1].Text != body {
		t.Error("concatenation is not byte-identical to the body")
	}
}

// TestChunkingCountsRunesNotBytes: a byte cut would split a multi-byte
// character and hand the model half of one, and the quote gate would then
// verify against text that no longer decodes the way the source read it.
func TestChunkingCountsRunesNotBytes(t *testing.T) {
	// Every rune here is 3 bytes, so a byte-based cap would produce roughly
	// three times as many chunks and would land mid-character.
	body := strings.Repeat("あ", 5000)
	chunks, _ := chunkBody(body, 4000, carry{})
	if len(chunks) != 2 {
		t.Fatalf("got %d chunks, want 2 (5000 runes at a 4000-rune cap)", len(chunks))
	}
	if n := utf8.RuneCountInString(chunks[0].Text); n != 4000 {
		t.Errorf("first chunk holds %d runes, want exactly 4000", n)
	}
	for i, ch := range chunks {
		if !utf8.ValidString(ch.Text) {
			t.Errorf("chunk %d is not valid UTF-8 — a cut landed inside a rune", i+1)
		}
	}
	if chunks[0].Text+chunks[1].Text != body {
		t.Error("concatenation is not byte-identical to the body")
	}
}

// TestChunkingHardCutsWhenBoundaryIsTooEarly: a boundary closer than a third of
// the cap is rejected, so one convenient newline near the start cannot turn a
// 4000-rune budget into a stream of near-empty chunks.
func TestChunkingHardCutsWhenBoundaryIsTooEarly(t *testing.T) {
	body := strings.Repeat("a", 40) + "\n" + strings.Repeat("b", 8000)
	chunks, _ := chunkBody(body, 4000, carry{})
	if n := utf8.RuneCountInString(chunks[0].Text); n != 4000 {
		t.Errorf("first chunk holds %d runes, want the hard cut at 4000", n)
	}
	var joined strings.Builder
	for _, ch := range chunks {
		joined.WriteString(ch.Text)
	}
	if joined.String() != body {
		t.Error("concatenation is not byte-identical to the body")
	}
}

// TestChunkingEdgeCases keeps the degenerate inputs from producing items.
func TestChunkingEdgeCases(t *testing.T) {
	if got, _ := chunkBody("", 4000, carry{}); got != nil {
		t.Errorf("empty body produced %d chunks", len(got))
	}
	if got, _ := chunkBody("text", 0, carry{}); got != nil {
		t.Errorf("non-positive cap produced %d chunks", len(got))
	}
	if got, _ := chunkBody("short", 4000, carry{}); len(got) != 1 || got[0].Text != "short" {
		t.Errorf("short body = %+v, want one chunk holding it whole", got)
	}
}
