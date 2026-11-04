package backends

// F-W9 (design/04 §4.5): the chain filters of A's /v1 chain and the snapshot
// reader of the learned instances.
//
//	go test -short -count=1 -run 'TestLearnedInstances|TestExcludeHopChain|TestExcludeForEgressLocal|TestTransitCap' -v ./internal/backends/

import (
	"strings"
	"sync"
	"testing"
)

func hopRow(id, class, locality string) Backend {
	return Backend{ID: id, Name: id, ProviderClass: class, Locality: locality}
}

func hopIDs(chain []Backend) string {
	var out []string
	for _, b := range chain {
		out = append(out, b.ID)
	}
	return strings.Join(out, ",")
}

// TestLearnedInstances: the snapshot holds every row with a learned instance
// and nothing else; a later NotePeerInstance does not change a snapshot
// already taken (a copy, not a view), and concurrent notes and reads are
// race-free.
func TestLearnedInstances(t *testing.T) {
	p := NewPool(nil, nil)
	if got := p.LearnedInstances(); len(got) != 0 {
		t.Fatalf("fresh pool: %v, want none", got)
	}
	p.NotePeerInstance("b1", "00000000-0000-4000-8000-0000000000b1")
	p.ReportFailure("b2", ClassTransport, 0) // health without an instance
	snap := p.LearnedInstances()
	if len(snap) != 1 || snap["b1"] != "00000000-0000-4000-8000-0000000000b1" {
		t.Fatalf("snapshot %v, want only b1", snap)
	}
	p.NotePeerInstance("b3", "00000000-0000-4000-8000-0000000000b3")
	if len(snap) != 1 {
		t.Errorf("a taken snapshot changed: %v", snap)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(2)
		go func() { defer wg.Done(); p.NotePeerInstance("b4", "00000000-0000-4000-8000-0000000000b4") }()
		go func() { defer wg.Done(); _ = p.LearnedInstances() }()
	}
	wg.Wait()
	if got := p.LearnedInstances(); len(got) != 3 {
		t.Errorf("after the notes: %v, want b1, b3, b4", got)
	}
}

// TestExcludeHopChain: a row whose learned instance is on Ctx-Path leaves the
// chain; rows without learned instance and rows learned elsewhere stay.
func TestExcludeHopChain(t *testing.T) {
	chain := []Backend{hopRow("a", ProviderCtx, LocalityLAN), hopRow("b", ProviderGeneric, LocalityLAN), hopRow("c", ProviderCtx, LocalityLAN)}
	learned := map[string]string{"a": "i-a", "b": "i-b"}
	if got := hopIDs(ExcludeHopChain(chain, []string{"i-b", "i-x"}, learned)); got != "a,c" {
		t.Fatalf("ExcludeHopChain = %s, want a,c", got)
	}
	if got := hopIDs(ExcludeHopChain(chain, nil, learned)); got != "a,b,c" {
		t.Fatalf("no path: %s, want every row", got)
	}
}

// TestExcludeForEgressLocal: (a) provider class ctx, (b) a learned instance,
// (c) an external (or unknown) locality — each leaves the chain; A's own
// local and LAN engines stay.
func TestExcludeForEgressLocal(t *testing.T) {
	chain := []Backend{
		hopRow("ctx", ProviderCtx, LocalityLAN),
		hopRow("learned", ProviderGeneric, LocalityLAN),
		hopRow("ext", ProviderGeneric, LocalityExternal),
		hopRow("unknown", ProviderGeneric, ""),
		hopRow("local", ProviderLlamaCpp, LocalityLocal),
		hopRow("lan", ProviderGeneric, LocalityLAN),
	}
	if got := hopIDs(ExcludeForEgressLocal(chain, map[string]string{"learned": "i-l"})); got != "local,lan" {
		t.Fatalf("ExcludeForEgressLocal = %s, want local,lan", got)
	}
}

// TestTransitCap: the first instance-hop row (ctx or learned) of the order
// stays, every later one goes; local and external rows stay.
func TestTransitCap(t *testing.T) {
	chain := []Backend{
		hopRow("local", ProviderLlamaCpp, LocalityLocal),
		hopRow("learned", ProviderGeneric, LocalityLAN),
		hopRow("ctx1", ProviderCtx, LocalityLAN),
		hopRow("ext", ProviderOpenRouter, LocalityExternal),
		hopRow("ctx2", ProviderCtx, LocalityLAN),
	}
	if got := hopIDs(TransitCap(chain, map[string]string{"learned": "i-l"})); got != "local,learned,ext" {
		t.Fatalf("TransitCap = %s, want local,learned,ext", got)
	}
	if got := hopIDs(TransitCap(chain, nil)); got != "local,learned,ctx1,ext" {
		t.Fatalf("nothing learned: %s, want local,learned,ctx1,ext", got)
	}
}
