package events

import (
	"log/slog"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
)

// TestAbortSettleWritePathAndDerivationAgree pins the two halves of
// limits.abort_settle_ms (A03-W10, design/03 §3.2) against each other, like
// the external keys (TestDispatchExternalLimitsWritePathAndDerivationAgree):
// the backend write path refuses (422) exactly the values the derivation
// ignores with a WARN and a 0, it accepts the values the derivation reads,
// and both sides share the bound — the write path's mirror of
// dispatch.AbortSettleMaxMs is accepted, one above is refused on both.
func TestAbortSettleWritePathAndDerivationAgree(t *testing.T) {
	const origin = "http://gpu:8089"
	row := func(v any) backends.Backend {
		return backends.Backend{Name: "row", Scope: "_global", Protocol: backends.ProtocolOpenAI, Trust: backends.TrustFull, Enabled: true,
			Host: origin, ProviderClass: backends.ProviderLlamaCpp, Locality: backends.LocalityLAN, Roles: []string{"chat"},
			Limits: map[string]any{"slots": float64(4), "abort_settle_ms": v}}
	}
	refused := func(b backends.Backend) bool {
		_, errs := backends.ValidateBackend(&b)
		for _, e := range errs {
			if e.Field == "limits.abort_settle_ms" {
				return true
			}
		}
		return false
	}
	derived := func(b backends.Backend) (time.Duration, time.Duration) {
		tp := dispatch.DerivePolicy(DispatchBackendRows([]backends.Backend{b}), slog.New(slog.DiscardHandler)).Targets[origin]
		return tp.AbortSettleStream, tp.AbortSettleNonstream
	}
	for _, bad := range []any{
		map[string]any{"stream": float64(-1)},
		map[string]any{"stream": float64(dispatch.AbortSettleMaxMs + 1)},
		map[string]any{"nonstream": float64(30001)},
		map[string]any{"stream": "4000"},
		map[string]any{"stream": 1.5},
		map[string]any{"stream": float64(1), "other": float64(1)},
		"4000", float64(4000), []any{float64(1), float64(2)}, nil,
	} {
		b := row(bad)
		if !refused(b) {
			t.Errorf("write path accepts abort_settle_ms=%v", bad)
		}
		if s, n := derived(b); s != 0 || n != 0 {
			t.Errorf("derivation reads abort_settle_ms=%v as %v/%v, which the write path refuses", bad, s, n)
		}
	}
	for _, good := range []struct {
		v    any
		s, n time.Duration
	}{
		{map[string]any{}, 0, 0},
		{map[string]any{"stream": float64(0), "nonstream": float64(0)}, 0, 0},
		{map[string]any{"nonstream": float64(4000)}, 0, 4 * time.Second},
		{map[string]any{"stream": float64(dispatch.AbortSettleMaxMs), "nonstream": float64(1)}, 30 * time.Second, time.Millisecond},
	} {
		b := row(good.v)
		if refused(b) {
			t.Errorf("write path refuses abort_settle_ms=%v", good.v)
		}
		if s, n := derived(b); s != good.s || n != good.n {
			t.Errorf("derivation reads abort_settle_ms=%v as %v/%v, want %v/%v", good.v, s, n, good.s, good.n)
		}
	}
}
