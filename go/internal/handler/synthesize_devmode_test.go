package handler

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/llmlog"
)

// TestDevmodeIsFailClosed pins the C6-C posture at the seams where a tenant may
// be UNRESOLVABLE. The flag is a per-tenant answer; wherever no tenant can be
// named the answer is "no", and the seal holds.
//
// Three seams, all of them real:
//
//   - the daily-synthesis handler without config wiring (h.cfg nil — the boot
//     order and every test harness that skips it),
//   - a request with no AuthResult in its context, whose tenant scope resolves
//     to "" and therefore to the server-global base generation, not to some
//     other tenant's,
//   - the zero values of the two structs that carry the flag into the domain
//     packages: a bench harness or test that builds llm.SynthesisSettings{} or
//     dream.Router{} by hand gets sealing without having to know the flag
//     exists.
func TestDevmodeIsFailClosed(t *testing.T) {
	entry := llmlog.Entry{
		RequiredSensitivity: "credentials",
		RequestSystem:       "sys", RequestUser: "user", ResponseContent: "resp",
	}
	sealed := func(t *testing.T, what string, devmode bool) {
		t.Helper()
		got := entry.Slimmed(devmode)
		if got.RequestSystem != "" || got.RequestUser != "" || got.ResponseContent != "" {
			t.Errorf("%s: devmode=%v kept bodies %+v, want a sealed row", what, devmode, got)
		}
	}

	t.Run("handler without config wiring", func(t *testing.T) {
		h := &SynthesizeHandler{}
		if got := h.devmode(context.Background()); got {
			t.Errorf("devmode with nil cfg = %v, want false", got)
		}
		sealed(t, "nil cfg", h.devmode(context.Background()))
	})

	t.Run("request without an AuthResult resolves to the base generation", func(t *testing.T) {
		// _global itself is off, so an unresolvable request seals. The point of
		// the probe is WHICH generation it lands on: the operator's own base,
		// never a tenant's.
		base := &config.Config{}
		h := &SynthesizeHandler{cfg: config.NewStore(base)}
		if got := h.devmode(context.Background()); got != base.Tenant.Devmode {
			t.Errorf("devmode without auth = %v, want the base generation's %v", got, base.Tenant.Devmode)
		}
		sealed(t, "no auth", h.devmode(context.Background()))
	})

	t.Run("zero-value carriers seal", func(t *testing.T) {
		sealed(t, "llm.SynthesisSettings{}", llm.SynthesisSettings{}.Devmode)
	})
}
