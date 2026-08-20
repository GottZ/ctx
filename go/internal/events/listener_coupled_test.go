package events

import (
	"testing"

	"github.com/GottZ/ctx/internal/backends"
)

// embedRow builds a minimal pool row for the coupled-set tests: only the fields
// the set derivation reads (id, host, protocol, roles, enabled).
func embedRow(id, host string, proto backends.Protocol, enabled bool, roles ...string) backends.Backend {
	return backends.Backend{
		ID: id, Name: id, Host: host, Protocol: proto,
		Roles: roles, Enabled: enabled, Scope: backends.GlobalScope,
	}
}

func has(t *testing.T, s coupledSet, host string, proto backends.Protocol) {
	t.Helper()
	if _, ok := s[coupledPair{host: host, protocol: string(proto)}]; !ok {
		t.Fatalf("coupled set %v is missing %s/%s", s, host, proto)
	}
}

// TestCoupledSetServingEligible pins the qualification of design/04 §3.2a: the
// set spans the backends that would actually WRITE the cache — enabled AND not
// disabled by an active profile — and nothing else. A row that is merely
// present, or carries no embed role, must not appear; otherwise an unrelated
// chat-row edit would flush every vector.
func TestCoupledSetServingEligible(t *testing.T) {
	p := backends.NewPool(nil, nil)
	p.SeedSnapshotDisabledByForTest([]backends.Backend{
		embedRow("a", "http://embed-a:11434", backends.ProtocolOllama, true, backends.RoleEmbed),
		embedRow("b", "http://embed-b:11434", backends.ProtocolOllama, false, backends.RoleEmbed),
		embedRow("c", "http://embed-c:11434", backends.ProtocolOllama, true, backends.RoleEmbed),
		embedRow("d", "http://chat-d:8000", backends.ProtocolOpenAI, true, backends.RoleChat),
	}, map[string]string{"c": "wartung"})

	got := coupledSetOf(p)
	if len(got) != 1 {
		t.Fatalf("coupled set = %v, want exactly the one serving-eligible embed host", got)
	}
	has(t, got, "http://embed-a:11434", backends.ProtocolOllama)
}

// TestCoupledSetPairsNotModels pins what the set is keyed on: connection
// identity, not model. Two embed rows on the same host/protocol collapse to one
// pair (a model-only edit therefore diffs empty — the cache keys on model and
// addresses different rows by itself), while the dream-embed role counts as a
// cache writer just like embed, and a protocol switch on an unchanged host is a
// distinct pair.
func TestCoupledSetPairsNotModels(t *testing.T) {
	shared := "http://embed:11434"
	rows := []backends.Backend{
		embedRow("a", shared, backends.ProtocolOllama, true, backends.RoleEmbed),
		embedRow("b", shared, backends.ProtocolOllama, true, backends.RoleEmbed),
		embedRow("c", shared, backends.ProtocolOpenAI, true, backends.RoleDreamEmbed),
	}
	rows[0].Model = "qwen3-embedding:8b"
	rows[1].Model = "nomic-embed-text"

	p := backends.NewPool(nil, nil)
	p.SeedSnapshotForTest(rows)

	got := coupledSetOf(p)
	if len(got) != 2 {
		t.Fatalf("coupled set = %v, want 2 pairs (host×protocol, model-blind)", got)
	}
	has(t, got, shared, backends.ProtocolOllama)
	has(t, got, shared, backends.ProtocolOpenAI)
}

// TestCoupledSetNilPool keeps the pre-wire/test construction path inert: a
// handler without a pool derives the empty set instead of panicking.
func TestCoupledSetNilPool(t *testing.T) {
	if got := coupledSetOf(nil); len(got) != 0 {
		t.Fatalf("coupled set of nil pool = %v, want empty", got)
	}
}

// TestCoupledBaselineFromConstructor pins the boot posture of A04-W3: the
// handler takes its baseline from the pool it is built on (boot loads the pool
// before the listener exists, cmd/ctxd/main.go), so a boot without an
// intervening edit diffs empty. Against a zero-valued baseline this test fails
// — and the production consequence would be a full embed-cache flush on the
// first backend write after every restart.
func TestCoupledBaselineFromConstructor(t *testing.T) {
	p := backends.NewPool(nil, nil)
	p.SeedSnapshotForTest([]backends.Backend{
		embedRow("a", "http://embed-a:11434", backends.ProtocolOllama, true, backends.RoleEmbed),
	})

	h := NewSettingsWriteHandler(nil, nil, p, nil)
	if !mapsEqualForTest(h.coupledPrev, coupledSetOf(p)) {
		t.Fatalf("baseline = %v, want the constructing pool's coupled set %v", h.coupledPrev, coupledSetOf(p))
	}
}

func mapsEqualForTest(a, b coupledSet) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}
