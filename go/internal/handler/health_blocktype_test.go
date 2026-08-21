// WF T3 (design 01 §4.3): /health carries the blocktype_registry degradation
// field. A degraded registry (builtin-fallback after a failed DB load) must
// surface LOUDLY: field value "builtin-fallback" AND overall status folded to
// "degraded" — never a silent "ok" (the builtin fallback would revert
// operator visibility narrowings unnoticed otherwise).
package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeBlocktypeHealth fakes the registry degradation state.
type fakeBlocktypeHealth struct{ v string }

func (f fakeBlocktypeHealth) Health() string { return f.v }

// TestAggregateHealthStatusBlocktypeFold pins the status fold: with all
// services healthy, a builtin-fallback registry degrades the overall status
// (negative probe: a silent pass-through would report "ok" and go red here);
// an unhealthy DB stays "unhealthy" regardless.
func TestAggregateHealthStatusBlocktypeFold(t *testing.T) {
	healthy := map[string]string{"database": "ok", "embed": "ok", "chat": "ok"}
	if got := aggregateHealthStatus(healthy, "ok"); got != "ok" {
		t.Errorf("healthy + registry ok: status = %q, want ok", got)
	}
	if got := aggregateHealthStatus(healthy, "builtin-fallback"); got != "degraded" {
		t.Errorf("healthy + builtin-fallback: status = %q, want degraded (silent pass-through?)", got)
	}
	dbDown := map[string]string{"database": "error", "embed": "ok", "chat": "ok"}
	if got := aggregateHealthStatus(dbDown, "builtin-fallback"); got != "unhealthy" {
		t.Errorf("db down + builtin-fallback: status = %q, want unhealthy", got)
	}
}

// TestHealthBlocktypeRegistryField pins the wire field end-to-end through the
// handler: degraded fake ⇒ "builtin-fallback" in the body; nil wiring (tests
// only — the binary always wires the registry) ⇒ "ok".
func TestHealthBlocktypeRegistryField(t *testing.T) {
	pool := brokenPool(t)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backend.Close)
	cfg := healthTestConfig(backend.URL, backend.URL)
	st := &swapStore{}
	st.p.Store(cfg)

	for _, tc := range []struct {
		name string
		bt   BlocktypeHealth
		want string
	}{
		{"degraded registry", fakeBlocktypeHealth{v: "builtin-fallback"}, "builtin-fallback"},
		{"healthy registry", fakeBlocktypeHealth{v: "ok"}, "ok"},
		{"nil wiring", nil, "ok"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h := NewHealthHandler(pool, st, healthTestPool(backend.URL, backend.URL, backend.URL), tc.bt)
			h.Health(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

			var resp struct {
				BlocktypeRegistry string `json:"blocktype_registry"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if resp.BlocktypeRegistry != tc.want {
				t.Errorf("blocktype_registry = %q, want %q", resp.BlocktypeRegistry, tc.want)
			}
			assertHealthBody(t, rec.Body.Bytes(), healthNeedles(cfg))
		})
	}
}
