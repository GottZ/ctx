package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/config"
)

func ctxWithAuthResult(ar *auth.AuthResult) context.Context {
	return context.WithValue(context.Background(), authResultKey, ar)
}

// TestRequestTenantScope: the cycle-free wrapper main wires into config (MT
// 06-C5) maps a request context to the caller's tenant scope = the
// AuthResult.HomeScope namespace, NOT the tenant UUID (§11.1). Absent auth or an
// empty home scope yields "" so SnapshotForRequest fails safe to the base
// generation rather than minting a bogus tenant lookup.
func TestRequestTenantScope(t *testing.T) {
	cases := []struct {
		name string
		ctx  context.Context
		want string
	}{
		{"no auth result", context.Background(), ""},
		{"valid home scope", ctxWithAuthResult(&auth.AuthResult{HomeScope: "work", IsValid: true}), "work"},
		{"empty home scope", ctxWithAuthResult(&auth.AuthResult{HomeScope: "", IsValid: true}), ""},
		// A tenant UUID is present but must NOT be used as the scope key.
		{"home scope wins over tenant id", ctxWithAuthResult(&auth.AuthResult{HomeScope: "work", TenantID: "00000000-0000-0000-0000-0000000d3fa0", IsValid: true}), "work"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RequestTenantScope(tc.ctx); got != tc.want {
				t.Errorf("RequestTenantScope = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSnapshotForRequest_IgnoresBodyScope is the C5 cross-tenant-leak scan
// (§8.4): a request whose BODY claims a foreign scope, but whose authenticated
// identity is tenant-a, must resolve tenant-a's config generation. The
// guarantee is by construction — SnapshotForRequest takes no scope argument; it
// derives the tenant from the auth result via the wired hook (RequestTenantScope),
// so a body field can never redirect the policy (§5.1).
//
// Red-proof: a resolution driven by the body "tenant-b" hits the overlay's
// decline path → base (not tenantA); an unwired hook → "" → base too. Both
// diverge from tenantA, so this fails unless the authenticated HomeScope drives
// the lookup.
func TestSnapshotForRequest_IgnoresBodyScope(t *testing.T) {
	base := &config.Config{}
	tenantA := &config.Config{}
	st := config.NewStore(base)
	st.SetOverlay(func(_ context.Context, _ *config.Config, scope string) (*config.Config, error) {
		if scope == "tenant-a" {
			return tenantA, nil
		}
		return nil, nil // any other scope inherits the base generation
	})

	config.SetRequestScopeHook(RequestTenantScope)
	defer config.SetRequestScopeHook(nil)

	// Authenticated as tenant-a (the way the auth middleware populates ctx);
	// the body claims a foreign scope "tenant-b" that the handler layer never
	// forwards to the config resolution.
	req := httptest.NewRequest(http.MethodPost, "/api/store",
		strings.NewReader(`{"scope":"tenant-b","category":"c","title":"t","content":"x"}`))
	req = req.WithContext(context.WithValue(req.Context(), authResultKey,
		&auth.AuthResult{HomeScope: "tenant-a", IsValid: true}))

	if got := st.SnapshotForRequest(req.Context()); got != tenantA {
		t.Fatalf("SnapshotForRequest must resolve the authenticated tenant (tenant-a), not the body scope; got %p (base=%p tenantA=%p)", got, base, tenantA)
	}
}
