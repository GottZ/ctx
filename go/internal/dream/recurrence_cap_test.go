package dream

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
)

// TestConfirmRecurrenceKeepsItsCap pins the recurrence confirm's budget: the
// dream role's model_map max_tokens is sized for the link eval (thousands of
// tokens on a reasoning model) and must not inflate the per-pair confirm,
// whose whole answer is one short JSON verdict. The assertion is on the
// options the chain walk RESOLVED — what the backend is actually told.
func TestConfirmRecurrenceKeepsItsCap(t *testing.T) {
	p := backends.NewPool(nil, nil)
	p.SeedSnapshotForTest([]backends.Backend{{
		ID: "b", Name: "b", Host: "h", Model: "m",
		Trust: backends.TrustFull, Locality: "lan",
		Roles: []string{backends.RoleDream},
		ModelMap: map[string]backends.ModelSpec{
			backends.RoleDream: {Model: "m", Params: map[string]any{"max_tokens": float64(8000)}},
		},
		Priority: 100, Enabled: true,
	}})
	r := &Router{Pool: p, Admit: testAdmit()}

	opts := DreamOptions() // 600, the cycle's options
	seen := scriptedChat(t, scriptedAnswer{`{"verdict":"none","pattern":"","confidence":0.1}`, 20, "stop"})

	if _, err := confirmRecurrence(context.Background(), nil, r, opts, srcBlock(uuidA), recCand(uuidB)); err != nil {
		t.Fatalf("confirmRecurrence: %v", err)
	}
	if len(*seen) != 1 {
		t.Fatalf("want 1 wire call, got %d", len(*seen))
	}
	if got := (*seen)[0].NumPredict; got != opts.NumPredict {
		t.Errorf("confirm sent num_predict %d, want its own %d (model_map 8000 must not apply)", got, opts.NumPredict)
	}
	if opts.CapLocked {
		t.Error("the caller's options were mutated — CapLocked must stay local to the confirm call")
	}
}
