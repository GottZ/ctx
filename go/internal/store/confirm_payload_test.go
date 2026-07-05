package store

import (
	"encoding/json"
	"strings"
	"testing"
)

func baseWrite() CanonicalWrite {
	return CanonicalWrite{
		Op:       "store",
		Scope:    "private",
		Category: "learnings",
		Title:    "Ein Titel",
		Content:  "Inhalt mit Umlauten: äöüß",
		Tags:     []string{"zulu", "alpha", "mike"},
		Metadata: map[string]any{
			"source": "claude-code-2026-07-05",
			"nested": map[string]any{"b": 2, "a": 1},
		},
		Sensitivity: "internal",
	}
}

// The hash must be stable under everything JSON key/element order can vary:
// tag order and metadata construction order (Go maps are unordered; the
// canonical form sorts tags and encoding/json sorts map keys).
func TestPayloadHashStableUnderReordering(t *testing.T) {
	h1, c1, err := baseWrite().PayloadHash()
	if err != nil {
		t.Fatal(err)
	}

	reordered := baseWrite()
	reordered.Tags = []string{"mike", "zulu", "alpha"}
	// Rebuild the metadata map in a different insertion order.
	reordered.Metadata = map[string]any{
		"nested": map[string]any{"a": 1, "b": 2},
		"source": "claude-code-2026-07-05",
	}
	h2, _, err := reordered.PayloadHash()
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatalf("hash not order-stable: %s != %s", h1, h2)
	}
	if len(h1) != 64 || strings.ToLower(h1) != h1 {
		t.Fatalf("hash is not lowercase sha256 hex: %q", h1)
	}
	if !json.Valid(c1) {
		t.Fatalf("canonical bytes are not valid JSON: %q", c1)
	}
}

// The input Tags slice must not be mutated by canonicalization (the caller's
// request object stays untouched).
func TestCanonicalDoesNotMutateInput(t *testing.T) {
	w := baseWrite()
	if _, err := w.Canonical(); err != nil {
		t.Fatal(err)
	}
	if w.Tags[0] != "zulu" || w.Tags[1] != "alpha" || w.Tags[2] != "mike" {
		t.Fatalf("input tags mutated: %v", w.Tags)
	}
}

// Any single-byte payload difference must change the hash.
func TestPayloadHashOneByteDelta(t *testing.T) {
	h1, _, _ := baseWrite().PayloadHash()

	oneByte := baseWrite()
	oneByte.Content += "!"
	h2, _, _ := oneByte.PayloadHash()
	if h1 == h2 {
		t.Fatal("one-byte content delta did not change the hash")
	}
}

// The hash binds the FULL resolved sensitivity intent (value, manual flag,
// detector flag) — a confirm can never execute under a different
// classification than the card promised.
func TestPayloadHashBindsSensitivity(t *testing.T) {
	h1, _, _ := baseWrite().PayloadHash()

	sensValue := baseWrite()
	sensValue.Sensitivity = "credentials"
	h2, _, _ := sensValue.PayloadHash()
	if h1 == h2 {
		t.Fatal("sensitivity value delta did not change the hash")
	}

	sensManual := baseWrite()
	sensManual.SensitivityManual = true
	h3, _, _ := sensManual.PayloadHash()
	if h1 == h3 {
		t.Fatal("sensitivity manual-flag delta did not change the hash")
	}

	sensDetect := baseWrite()
	sensDetect.SensitivityDetect = true
	h4, _, _ := sensDetect.PayloadHash()
	if h1 == h4 {
		t.Fatal("sensitivity detector-flag delta did not change the hash")
	}
}

// The canonical bytes round-trip into the same struct — the staged payload is
// executable as-is. NOTE (JSONB lesson, D-W1): the hash is only ever formed
// over these client-side canonical bytes; rehashing bytes read back from the
// JSONB column would NOT reproduce it (JSONB normalizes key order/whitespace).
func TestCanonicalRoundTrip(t *testing.T) {
	w := baseWrite()
	_, canonical, err := w.PayloadHash()
	if err != nil {
		t.Fatal(err)
	}
	var back CanonicalWrite
	if err := json.Unmarshal(canonical, &back); err != nil {
		t.Fatalf("canonical bytes do not unmarshal: %v", err)
	}
	if back.Op != w.Op || back.Scope != w.Scope || back.Category != w.Category ||
		back.Title != w.Title || back.Content != w.Content || back.Type != w.Type {
		t.Fatalf("round-trip drift: %+v vs %+v", back, w)
	}
	// Tags come back SORTED (canonical form).
	if len(back.Tags) != 3 || back.Tags[0] != "alpha" || back.Tags[1] != "mike" || back.Tags[2] != "zulu" {
		t.Fatalf("round-trip tags not canonical-sorted: %v", back.Tags)
	}
}
