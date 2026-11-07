package backends

import (
	"slices"
	"testing"
)

// TestPoolStatusHintSkipsCtxRows is gate G6, second half (F-W5, F-W4
// handoff): the status hint answers_as_ctx_instance tells an admin to
// register a row as ctx — a row that already is one never carries it, while
// a generic row answering with Ctx-Instance still does.
func TestPoolStatusHintSkipsCtxRows(t *testing.T) {
	p := NewPool(nil, nil)
	p.SeedSnapshotForTest([]Backend{
		{ID: "peer", Name: "peer-a", ProviderClass: ProviderCtx, Enabled: true},
		{ID: "gen", Name: "behind-litellm", ProviderClass: ProviderGeneric, Enabled: true},
	})
	const instance = "0c1d2e3f-4a5b-4c6d-8e7f-9a0b1c2d3e4f"
	p.NotePeerInstance("peer", instance)
	p.NotePeerInstance("gen", instance)
	if h := hintsOf(p, "peer"); h != nil {
		t.Errorf("ctx row hints = %v, want none", h)
	}
	if h := hintsOf(p, "gen"); !slices.Equal(h, []string{HintAnswersAsCtxInstance}) {
		t.Errorf("generic row hints = %v, want [%s]", h, HintAnswersAsCtxInstance)
	}
}
