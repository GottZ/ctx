package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/go-chi/chi/v5"
)

// typesRouterAs mounts the PRODUCTION types chain (MountTypes — the same
// function server.go mounts) behind a middleware that injects ar, and fires one
// request. pool is nil: any request that passes the member gate and reaches the
// store layer panics, which the recover converts into the red proof that no
// gate was in the chain. ar=nil models a request that somehow reached the mount
// without the Auth middleware having injected an AuthResult.
func typesRouterAs(t *testing.T, ar *auth.AuthResult, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := req.Context()
			if ar != nil {
				ctx = context.WithValue(ctx, authResultKey, ar)
			}
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	MountTypes(r, NewTypesHandler(nil, nil))

	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	defer func() {
		if rc := recover(); rc != nil {
			t.Fatalf("handler panicked (reached the store layer — no member gate before it): %v", rc)
		}
	}()
	r.ServeHTTP(rec, req)
	return rec
}

// invalidAR is a key that authenticated to a non-valid result (the auth layer
// sets IsValid=false for a revoked/unknown key). It must not pass RequireMember.
func invalidAR() *auth.AuthResult {
	return &auth.AuthResult{IsValid: false}
}

// SECURITY PROPERTY (design/03 §5.1): the /api/types reads require a VALID key.
// Without the in-mount gate a nil/invalid AuthResult would reach the handler
// and either panic into typeVisibleScopes (nil deref) or hit the nil pool — the
// fail-open trap. The gate returns 401 BEFORE the handler runs, so no store
// touch happens and the recover never fires.
//
// Negative probe (2026-07-03): run against MountTypes with the
// `r.Use(RequireMember)` line removed — both subtests failed (nil-AR request
// panicked into the handler / non-401 status), proving the gate is load-bearing
// in exactly the chain production mounts.
func TestTypesMemberGate_NoAuth401(t *testing.T) {
	cases := []struct{ method, path string }{
		{http.MethodGet, "/api/types"},
		{http.MethodGet, "/api/types/knowledge"},
	}
	for _, c := range cases {
		t.Run("nil_ar_"+c.path, func(t *testing.T) {
			rec := typesRouterAs(t, nil, c.method, c.path)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s %s with no auth: status = %d, want 401 (body %s)",
					c.method, c.path, rec.Code, rec.Body.String())
			}
		})
		t.Run("invalid_ar_"+c.path, func(t *testing.T) {
			rec := typesRouterAs(t, invalidAR(), c.method, c.path)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s %s with invalid key: status = %d, want 401 (body %s)",
					c.method, c.path, rec.Code, rec.Body.String())
			}
		})
	}
}

// SECURITY PROPERTY (design/03 §5.1, §7-W2): the /api/types write routes
// (PUT/DELETE) require the admin-or-tenant-admin tier. A member key passes the
// read gate but MUST be refused 403 by the write gate BEFORE any handler runs —
// with a nil pool, a missing gate would let the request panic into the store
// layer (HandlePut → store.GetBlockType(nil)), which the recover turns into the
// red proof.
//
// Negative probe (2026-07-04): run against MountTypes with the
// `r.Use(RequireAdminOrTenantAdmin)` line removed from the write group — the
// member PUT/DELETE reached the handler and panicked on the nil pool
// (recover fired: "handler panicked (reached the store layer — no member gate
// before it)"), proving the write gate is load-bearing in exactly the chain
// production mounts. With the gate in place both subtests return 403.
func TestTypesWriteGate_Member403(t *testing.T) {
	cases := []struct{ method, path string }{
		{http.MethodPut, "/api/types/knowledge"},
		{http.MethodDelete, "/api/types/knowledge"},
	}
	for _, c := range cases {
		t.Run("member_"+c.method, func(t *testing.T) {
			rec := typesRouterAs(t, memberAR(), c.method, c.path)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s %s as member: status = %d, want 403 (body %s)",
					c.method, c.path, rec.Code, rec.Body.String())
			}
		})
	}
}
