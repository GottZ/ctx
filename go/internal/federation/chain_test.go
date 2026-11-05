package federation

// F-W9 G4/G5, pure half (design/04 §4.5 "Kettenfilter auf A", R-02-13): the
// chain filters in their fixed order — ExcludeHopChain, then ExcludeForEgressLocal
// (Ctx-Egress: local) or TransitCap (any) — and the topology-free
// ErrNoEligibleBackend of an empty result.
//
//	go test -short -count=1 -run 'TestFilterChain' -v ./internal/federation/

import (
	"errors"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/fedhop"
)

func row(id, class, locality string) backends.Backend {
	return backends.Backend{ID: id, Name: id, ProviderClass: class, Locality: locality}
}

func ids(chain []backends.Backend) string {
	var out []string
	for _, b := range chain {
		out = append(out, b.ID)
	}
	return strings.Join(out, ",")
}

// chainFixture: two ctx rows (C1, C2), a LAN generic row whose instance A
// learned (G — a ctx instance registered as generic), a local engine (E), a
// LAN generic row without learned instance (L) and an external row (X).
func chainFixture() ([]backends.Backend, map[string]string) {
	chain := []backends.Backend{
		row("C1", backends.ProviderCtx, backends.LocalityLAN),
		row("G", backends.ProviderGeneric, backends.LocalityLAN),
		row("E", backends.ProviderLlamaCpp, backends.LocalityLocal),
		row("C2", backends.ProviderCtx, backends.LocalityLAN),
		row("L", backends.ProviderGeneric, backends.LocalityLAN),
		row("X", backends.ProviderOpenRouter, backends.LocalityExternal),
	}
	learned := map[string]string{"G": "00000000-0000-4000-8000-00000000000c", "C2": "00000000-0000-4000-8000-0000000000c2"}
	return chain, learned
}

// TestFilterChainEgressLocal is G4 pure: Ctx-Egress: local keeps A's own
// engines only — no ctx row, no row with a learned instance, no external
// row (Finding 3; "DropExternal" alone would keep C1, G and C2). The LAN
// generic row without learned instance stays: the named residual risk of
// §4.5 until F-W12 lets A learn it.
func TestFilterChainEgressLocal(t *testing.T) {
	chain, learned := chainFixture()
	got, err := FilterChain(chain, fedhop.Inbound{}, Policy{Required: backends.SensCredentials, EgressLocal: true}, learned)
	if err != nil {
		t.Fatalf("FilterChain: %v", err)
	}
	if ids(got) != "E,L" {
		t.Fatalf("egress local: %s, want E,L", ids(got))
	}
}

// TestFilterChainTransit is G5 pure: without Ctx-Egress: local at most the
// first instance-hop row (ctx or learned) of the order stays; local and
// external rows stay (Finding 7.b — linear fan-out instead of multiplicative).
func TestFilterChainTransit(t *testing.T) {
	chain, learned := chainFixture()
	got, err := FilterChain(chain, fedhop.Inbound{}, Policy{Required: backends.SensCredentials}, learned)
	if err != nil {
		t.Fatalf("FilterChain: %v", err)
	}
	if ids(got) != "C1,E,L,X" {
		t.Fatalf("transit: %s, want C1,E,L,X (three instance-hop rows C1, G, C2 → only C1)", ids(got))
	}
}

// TestFilterChainHopChainFirst: ExcludeHopChain runs before the cap, so a row
// whose learned instance is already on Ctx-Path frees the cap for the next
// instance row instead of taking it (fixed order, §4.5).
func TestFilterChainHopChainFirst(t *testing.T) {
	chain := []backends.Backend{
		row("G", backends.ProviderGeneric, backends.LocalityLAN),
		row("C2", backends.ProviderCtx, backends.LocalityLAN),
		row("E", backends.ProviderLlamaCpp, backends.LocalityLocal),
	}
	learned := map[string]string{"G": "00000000-0000-4000-8000-00000000000c"}
	in := fedhop.Inbound{Path: []string{"00000000-0000-4000-8000-00000000000c"}}
	got, err := FilterChain(chain, in, Policy{Required: backends.SensCredentials}, learned)
	if err != nil {
		t.Fatalf("FilterChain: %v", err)
	}
	if ids(got) != "C2,E" {
		t.Fatalf("hop chain then cap: %s, want C2,E (G is on the path, the cap goes to C2)", ids(got))
	}
}

// TestFilterChainEmptyIsNoEligibleBackend: an empty result is the
// topology-free ErrNoEligibleBackend — it carries the requirement and one
// reason per filtered row for the admin log, never a peer's identity.
func TestFilterChainEmptyIsNoEligibleBackend(t *testing.T) {
	chain := []backends.Backend{row("C1", backends.ProviderCtx, backends.LocalityLAN), row("X", backends.ProviderGeneric, backends.LocalityExternal)}
	got, err := FilterChain(chain, fedhop.Inbound{}, Policy{Required: backends.SensPersonal, EgressLocal: true}, nil)
	var nb *backends.ErrNoEligibleBackend
	if len(got) != 0 || !errors.As(err, &nb) {
		t.Fatalf("FilterChain = %s, %v — want an empty chain and *ErrNoEligibleBackend", ids(got), err)
	}
	if nb.Required != backends.SensPersonal || len(nb.Excluded) != 2 {
		t.Errorf("error %+v, want Required personal and two exclusion reasons", nb)
	}
	for _, e := range nb.Excluded {
		if strings.Contains(e.Reason, "00000000-") {
			t.Errorf("reason %q names an instance id", e.Reason)
		}
	}
	if _, err := FilterChain(nil, fedhop.Inbound{}, Policy{Required: backends.SensCredentials}, nil); !errors.As(err, &nb) {
		t.Errorf("empty input: %v, want *ErrNoEligibleBackend", err)
	}
}

// TestFilterChainDoesNotTouchInput: the filters are pure — the caller's chain
// keeps its rows and order.
func TestFilterChainDoesNotTouchInput(t *testing.T) {
	chain, learned := chainFixture()
	before := ids(chain)
	_, _ = FilterChain(chain, fedhop.Inbound{}, Policy{Required: backends.SensCredentials, EgressLocal: true}, learned)
	_, _ = FilterChain(chain, fedhop.Inbound{}, Policy{Required: backends.SensCredentials}, learned)
	if ids(chain) != before {
		t.Fatalf("input chain changed: %s, was %s", ids(chain), before)
	}
}
