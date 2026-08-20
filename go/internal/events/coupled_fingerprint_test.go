package events

import (
	"testing"

	"github.com/GottZ/ctx/internal/backends"
)

// setOf is the fingerprint's input under test: the coupled set derived from a
// seeded pool, without touching a database.
func setOf(t *testing.T, rows ...backends.Backend) coupledSet {
	t.Helper()
	p := backends.NewPool(nil, nil)
	p.SeedSnapshotForTest(rows)
	return coupledSetOf(p)
}

// TestFingerprintOrderIndependent is the core property of A04-W4: the digest
// describes a SET, not an iteration. Go randomizes map iteration order, so a
// digest taken in iteration order would differ between two boots of an
// unchanged topology — and every one of those boots would flush the whole embed
// cache. Repeating the derivation on independently built, identically populated
// sets is what exposes that.
func TestFingerprintOrderIndependent(t *testing.T) {
	rows := []backends.Backend{
		embedRow("a", "http://embed-a:11434", backends.ProtocolOllama, true, backends.RoleEmbed),
		embedRow("b", "http://embed-b:11434", backends.ProtocolOpenAI, true, backends.RoleDreamEmbed),
		embedRow("c", "http://embed-c:11434", backends.ProtocolOllama, true, backends.RoleEmbed),
	}
	want := setOf(t, rows...).fingerprint()

	reversed := []backends.Backend{rows[2], rows[1], rows[0]}
	if got := setOf(t, reversed...).fingerprint(); got != want {
		t.Fatalf("fingerprint of the same set built in another order = %s, want %s", got, want)
	}
	for i := range 20 {
		if got := setOf(t, rows...).fingerprint(); got != want {
			t.Fatalf("fingerprint not stable across derivations (run %d): %s != %s", i, got, want)
		}
	}
}

// TestFingerprintDistinguishesTopologies pins the other direction: every change
// the coupled diff exists for must move the digest. A host swap under an
// unchanged model name is exactly the silent cross-space case (design/04 §5.1
// R5); a protocol switch on an unchanged host is the same class.
func TestFingerprintDistinguishesTopologies(t *testing.T) {
	base := setOf(t, embedRow("a", "http://embed-a:11434", backends.ProtocolOllama, true, backends.RoleEmbed)).fingerprint()

	cases := map[string]coupledSet{
		"host swap": setOf(t, embedRow("a", "http://embed-b:11434", backends.ProtocolOllama, true, backends.RoleEmbed)),
		"protocol switch": setOf(t,
			embedRow("a", "http://embed-a:11434", backends.ProtocolOpenAI, true, backends.RoleEmbed)),
		"second host added": setOf(t,
			embedRow("a", "http://embed-a:11434", backends.ProtocolOllama, true, backends.RoleEmbed),
			embedRow("b", "http://embed-b:11434", backends.ProtocolOllama, true, backends.RoleEmbed)),
		"embed role removed": setOf(t,
			embedRow("a", "http://embed-a:11434", backends.ProtocolOllama, true, backends.RoleChat)),
	}
	for name, s := range cases {
		if got := s.fingerprint(); got == base {
			t.Errorf("%s: fingerprint unchanged (%s) — the boot check would miss this edit", name, got)
		}
	}
}

// TestFingerprintFieldSeparation guards the encoding, not the hash: two
// different sets whose concatenated fields happen to spell the same string must
// not collide. Without an unambiguous separator, host "a" + protocol "bc" and
// host "ab" + protocol "c" would hash identically and one of the two edits
// between them would be invisible to the boot check.
func TestFingerprintFieldSeparation(t *testing.T) {
	left := setOf(t, embedRow("a", "a", backends.Protocol("bc"), true, backends.RoleEmbed)).fingerprint()
	right := setOf(t, embedRow("a", "ab", backends.Protocol("c"), true, backends.RoleEmbed)).fingerprint()
	if left == right {
		t.Fatalf("field-boundary collision: both sets hash to %s", left)
	}
}

// TestFingerprintEmptySetIsAValue keeps "no serving-eligible embed backend"
// distinguishable from "never stamped". The empty set has a digest like any
// other set; the never-stamped state is NULL in the table (migration 132) and
// is what the E12 / E-A04-4 (b) seed path answers — conflating the two would
// re-seed silently on every boot of an installation whose embed pool is empty.
func TestFingerprintEmptySetIsAValue(t *testing.T) {
	empty := setOf(t).fingerprint()
	if empty == "" {
		t.Fatal("empty coupled set has no fingerprint — it must have one, distinct from never-stamped")
	}
	if populated := setOf(t, embedRow("a", "http://embed-a:11434", backends.ProtocolOllama, true, backends.RoleEmbed)).fingerprint(); empty == populated {
		t.Fatal("empty and populated coupled sets share a fingerprint")
	}
}
