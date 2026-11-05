package backends

// F-W4 (design/04 §4.3 Ziel-Ableitung, §4.4): HopTarget derives the hop tier
// from the provider class alone — None for openrouter only, Loop for every
// other class, unknown and empty included, and never through the zero value
// — and the Pool is the fedhop.Observer that turns a row answering as a ctx
// instance into a status hint.
//
//	go test -count=1 -run 'TestHopTarget|TestPoolObserver' ./internal/backends/

import (
	"slices"
	"testing"

	"github.com/GottZ/ctx/internal/fedhop"
)

func TestHopTargetTierByProviderClass(t *testing.T) {
	cases := []struct {
		class string
		want  fedhop.Tier
	}{
		{ProviderOpenRouter, fedhop.TierNone},
		{ProviderGeneric, fedhop.TierLoop},
		{ProviderLlamaCpp, fedhop.TierLoop},
		// ctx rows get Tier F with F-W5/F-W6; until then a peer is a tier-L target.
		{ProviderCtx, fedhop.TierLoop},
		{"", fedhop.TierLoop},
		{"OpenRouter", fedhop.TierLoop}, // the class is matched exactly, as at the ZDR force
		{"unregistered-class", fedhop.TierLoop},
	}
	for _, c := range cases {
		for _, kind := range []fedhop.WireKind{fedhop.WireChat, fedhop.WireEmbed, fedhop.WireRerank, fedhop.WireProbe} {
			b := Backend{ID: "row-1", ProviderClass: c.class, Protocol: ProtocolAnthropic}
			got := b.HopTarget(kind)
			want := fedhop.Target{BackendID: "row-1", Tier: c.want}
			if got != want {
				t.Errorf("class %q kind %d: HopTarget = %+v, want %+v", c.class, kind, got, want)
			}
		}
	}
}

// TestHopTargetSetsTierExplicitly pins the F-W3 handover: the zero Tier is
// TierLoop, so an openrouter target must carry TierNone explicitly — a
// HopTarget that left the field unset would send openrouter a token.
func TestHopTargetSetsTierExplicitly(t *testing.T) {
	var zero fedhop.Target
	if zero.Tier != fedhop.TierLoop {
		t.Fatalf("fedhop's zero Tier is %d, the F-W3 contract says TierLoop — HopTarget's mapping must be revisited", zero.Tier)
	}
	b := Backend{ProviderClass: ProviderOpenRouter}
	if got := b.HopTarget(fedhop.WireChat).Tier; got != fedhop.TierNone {
		t.Fatalf("openrouter HopTarget tier = %d, want TierNone (%d)", got, fedhop.TierNone)
	}
}

func hintsOf(p *Pool, id string) []string {
	for _, s := range p.Status() {
		if s.ID == id {
			return s.Hints
		}
	}
	return nil
}

func TestPoolObserverNotePeerInstanceHint(t *testing.T) {
	p := NewPool(nil, nil)
	p.SeedSnapshotForTest([]Backend{
		{ID: "llama", Name: "llama-cpu", ProviderClass: ProviderLlamaCpp, Enabled: true},
		{ID: "quiet", Name: "spark-chat", ProviderClass: ProviderGeneric, Enabled: true},
	})
	var obs fedhop.Observer = p
	if h := hintsOf(p, "llama"); h != nil {
		t.Fatalf("hint before any answer: %v", h)
	}
	obs.NotePeerInstance("llama", "0c1d2e3f-4a5b-4c6d-8e7f-9a0b1c2d3e4f")
	obs.NotePeerInstance("llama", "0c1d2e3f-4a5b-4c6d-8e7f-9a0b1c2d3e4f")
	if h := hintsOf(p, "llama"); !slices.Equal(h, []string{HintAnswersAsCtxInstance}) {
		t.Fatalf("llama-cpu hints = %v, want [%s] once", h, HintAnswersAsCtxInstance)
	}
	if h := hintsOf(p, "quiet"); h != nil {
		t.Fatalf("a row that never answered as a ctx instance carries hints %v", h)
	}
	// MarkSelfLoop has no effect before F-W11 (learning and exclusion).
	obs.MarkSelfLoop("quiet")
	if h := hintsOf(p, "quiet"); h != nil {
		t.Fatalf("MarkSelfLoop changed the status before F-W11: %v", h)
	}
	// A row without an id (not from the pool) is not tracked.
	obs.NotePeerInstance("", "0c1d2e3f-4a5b-4c6d-8e7f-9a0b1c2d3e4f")
	p.healthM.Lock()
	_, tracked := p.health[""]
	p.healthM.Unlock()
	if tracked {
		t.Fatal("NotePeerInstance tracked an empty backend id")
	}
}
