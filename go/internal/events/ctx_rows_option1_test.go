package events

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
)

// TestCtxSlotsRolesMatchDispatchBackgroundRoles pins the two halves of the
// slots duty of a ctx row against each other (F-W5, design/04 §4.12), in the
// pattern of TestDispatchExternalLimitsWritePathAndDerivationAgree: for every
// role, the backend write path demands limits.slots on a ctx row exactly when
// the dispatcher's coverage term would keep Enforcing() off for that row
// without slots (dispatch.backgroundRoles) — backends keeps its own role list
// (it does not import dispatch), and this pin is what keeps it one truth.
func TestCtxSlotsRolesMatchDispatchBackgroundRoles(t *testing.T) {
	local := backends.Backend{
		Name: "gpu-dream", Scope: dispatch.GlobalScope, Host: "http://gpu:8089",
		ProviderClass: backends.ProviderLlamaCpp, Locality: backends.LocalityLocal,
		Roles: []string{"dream"}, Limits: map[string]any{"slots": float64(1)}, Enabled: true,
	}
	roles := append([]string{"vision"}, backends.CoreRoles...)
	demanded := 0
	for _, role := range roles {
		row := backends.Backend{
			Name: "peer-" + role, Scope: dispatch.GlobalScope, Host: "http://10.0.0.5:8077",
			Protocol: backends.ProtocolOpenAI, ProviderClass: backends.ProviderCtx, APIKeyRef: "peer-key",
			Locality: backends.LocalityLAN, Trust: backends.TrustPublic, Roles: []string{role}, Enabled: true,
			ModelMap: map[string]backends.ModelSpec{"default": {Model: "peer-model"}},
		}
		_, errs := backends.ValidateBackend(&row)
		writeDemands := false
		for _, e := range errs {
			if e.Field == "limits.slots" {
				writeDemands = true
			}
		}
		d := dispatch.New(nil, dispatch.DefaultSettings())
		d.UpdatePolicy(dispatch.DerivePolicy(DispatchBackendRows([]backends.Backend{local, row}), slog.New(slog.DiscardHandler)))
		coverageDemands := !d.Enforcing()
		d.Close()
		if writeDemands != coverageDemands {
			t.Errorf("role %q: write path demands limits.slots = %v, coverage term demands it = %v (errs %v)",
				role, writeDemands, coverageDemands, errs)
		}
		if writeDemands {
			demanded++
		}
	}
	// The five background roles of dispatch (dream, dream-embed, embed,
	// digest, classify) — a vacuous pass (no role demanding) is red too.
	if demanded != 5 {
		t.Errorf("%d roles demand limits.slots on a ctx row, want the 5 background roles", demanded)
	}
}

// TestCtxRowsOption1 is the Option-1 gate of F-W5 (F-W7 Review-1 finding 1,
// lead decision (4) of 2026-11-06): a ctx row whose limits do not count for
// its origin under the K2 authority rule — a tenant row on an origin a
// _global row carries, or one of several tenants' rows on an origin without a
// _global row — counts as External for the coverage term: a tenant row never
// switches the instance-wide mode. An authoritative ctx row stays
// coverage-bound (design/04 §4.14). The K2 WARN for the ignored slots stays;
// no coverage WARN names the origin. Red against the F-W7 base as soon as the
// class ctx is writable (Enforcing()=false in a1, a1', a2).
func TestCtxRowsOption1(t *testing.T) {
	local := backends.Backend{
		Name: "gpu-dream", Scope: dispatch.GlobalScope, Host: "http://gpu:8089",
		ProviderClass: backends.ProviderLlamaCpp, Locality: backends.LocalityLocal,
		Roles: []string{"dream"}, Limits: map[string]any{"slots": float64(1)}, Enabled: true,
	}
	globalOR := backends.Backend{
		Name: "or", Scope: dispatch.GlobalScope, Host: "https://openrouter.ai/api/v1",
		ProviderClass: backends.ProviderOpenRouter, Locality: backends.LocalityExternal,
		Roles: []string{"chat"}, Enabled: true,
	}
	ctxRow := func(scope, host string, limits map[string]any) backends.Backend {
		return backends.Backend{
			Name: "ctx-" + scope, Scope: scope, Host: host,
			ProviderClass: backends.ProviderCtx, Locality: backends.LocalityExternal,
			Roles: []string{"dream"}, Limits: limits, Enabled: true,
		}
	}
	slots4 := map[string]any{"slots": float64(4)}
	const peer = "https://peer.example.com"
	for _, tc := range []struct {
		name          string
		rows          []backends.Backend
		wantEnforcing bool
		wantK2        int
		wantCoverage  int
	}{
		{"a1 tenant ctx row (slots 4) on the origin of a _global openrouter row",
			[]backends.Backend{local, globalOR, ctxRow("acme", "https://openrouter.ai/x", slots4)}, true, 1, 0},
		{"a1' the same rows, tenant row first",
			[]backends.Backend{ctxRow("acme", "https://openrouter.ai/x", slots4), local, globalOR}, true, 1, 0},
		{"a2 two tenants' ctx rows (slots 4) on one peer origin, no _global row",
			[]backends.Backend{local, ctxRow("acme", peer, slots4), ctxRow("beta", peer, slots4)}, true, 2, 0},
		{"a2 without slots", []backends.Backend{local, ctxRow("acme", peer, nil), ctxRow("beta", peer, nil)}, true, 0, 0},
		{"control: tenant-exclusive ctx row with slots 4", []backends.Backend{local, ctxRow("acme", peer, slots4)}, true, 0, 0},
		// Authoritative ctx rows stay coverage-bound (design/04 §4.14).
		{"tenant-exclusive ctx row without slots", []backends.Backend{local, ctxRow("acme", peer, nil)}, false, 0, 1},
		{"_global ctx row without slots", []backends.Backend{local, ctxRow(dispatch.GlobalScope, peer, nil)}, false, 0, 1},
		{"_global ctx row without slots next to a tenant ctx row with slots",
			[]backends.Backend{local, ctxRow(dispatch.GlobalScope, peer, nil), ctxRow("acme", peer, slots4)}, false, 1, 1},
		{"_global ctx row with slots next to a tenant ctx row without",
			[]backends.Backend{local, ctxRow(dispatch.GlobalScope, peer, slots4), ctxRow("acme", peer, nil)}, true, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &warnRecorder{}
			logger := slog.New(h)
			d := dispatch.New(logger, dispatch.DefaultSettings())
			t.Cleanup(d.Close)
			d.UpdatePolicy(dispatch.DerivePolicy(DispatchBackendRows(tc.rows), logger))
			k2, coverage := 0, 0
			for _, w := range h.records() {
				switch {
				case strings.Contains(w.msg, "K2 origin authority"):
					k2++
				case strings.Contains(w.msg, "background-reachable local target without slots cap"):
					coverage++
				default:
					t.Errorf("unexpected WARN %s %v", w.msg, w.attrs)
				}
				t.Logf("WARN %s %v", w.msg, w.attrs)
			}
			if got := d.Enforcing(); got != tc.wantEnforcing || k2 != tc.wantK2 || coverage != tc.wantCoverage {
				t.Errorf("Enforcing()=%v K2 WARNs=%d coverage WARNs=%d, want %v/%d/%d",
					got, k2, coverage, tc.wantEnforcing, tc.wantK2, tc.wantCoverage)
			}
		})
	}
}
