//go:build integration

// C6-C: the read half of the tenant devmode unseal against a real PG18
// testcontainer. The WRITE half (who gets bodies stored at all) is pinned in
// internal/llmlog; here the question is what GET /api/llmlog/{id} does with a
// credentials-class row that HAS bodies, and who may see it.
//
//	go test -tags=integration ./internal/handler/ -run TestLLMLogDevmode -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/testdb"
)

// TestLLMLogDevmodeUnsealedRowIsTenantBound is the security invariant of C6-C:
// a tenant that switches devmode on unseals its OWN credentials-class rows and
// NOTHING else. Three claims in one probe:
//
//  1. Override path — the unsealed row renders body_state=present WITH bodies
//     for its own tenant-admin and for the server-admin.
//  2. Isolation — a FOREIGN tenant-admin gets the uniform 404 and never sees a
//     byte of it. devmode is not a key to the neighbour's log: the api_key_id
//     predicate is untouched by this wave, and turning the flag on cannot widen
//     it, because the flag is nowhere in the read query.
//  3. No retroactive unsealing — a row slimmed at write time (bodies stored as
//     '', the pre-devmode state) still renders sealed with null bodies. Flipping
//     the flag creates no bodies for calls that already happened.
func TestLLMLogDevmodeUnsealedRowIsTenantBound(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	insertTenant := func(slug string) string {
		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO context_tenants (slug, display_name) VALUES ($1,$2) RETURNING id::text`, slug, slug).Scan(&id); err != nil {
			t.Fatalf("insert tenant %s: %v", slug, err)
		}
		return id
	}
	insertKey := func(hash, tenantID string) string {
		var id string
		if err := pool.QueryRow(ctx,
			`WITH p AS (
			     INSERT INTO context_principals (display_name) VALUES ($2) RETURNING id
			 )
			 INSERT INTO context_api_keys (key_hash, label, home_scope, tenant_id, principal_id)
			 SELECT $1,$2,'private',$3::uuid, p.id FROM p RETURNING id::text`, hash, hash, tenantID).Scan(&id); err != nil {
			t.Fatalf("insert key %s: %v", hash, err)
		}
		return id
	}
	// insertCredRow writes a credentials-class row exactly the way the write
	// path would: devmode ON stores the real bodies, devmode OFF stores the
	// three empty strings Slimmed() leaves behind (NOT NULL — the insert path
	// binds Go strings, which is what separates a slim from an eviction).
	insertCredRow := func(apiKeyID *string, sys, user, resp string) string {
		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO context_llm_log
			     (pipeline, model, host, duration_ms, api_key_id,
			      required_sensitivity, request_system, request_user, response_content)
			 VALUES ('query-synthesize','m','h',10,$1,'credentials',$2,$3,$4)
			 RETURNING id::text`, apiKeyID, sys, user, resp).Scan(&id); err != nil {
			t.Fatalf("insert credentials row: %v", err)
		}
		return id
	}

	tenantA := insertTenant("c6c-devmode-a")
	tenantB := insertTenant("c6c-devmode-b")
	keyA := insertKey("c6c-key-a", tenantA)
	keyB := insertKey("c6c-key-b", tenantB)

	unsealed := insertCredRow(&keyA, "SYS-DEVMODE-A", "USER-DEVMODE-A", "RESP-DEVMODE-A")
	slimmed := insertCredRow(&keyA, "", "", "")
	foreign := insertCredRow(&keyB, "SYS-DEVMODE-B", "USER-DEVMODE-B", "RESP-DEVMODE-B")

	h := NewLLMLogHandler(pool, config.NewStore(&config.Config{}))
	fetch := func(id string, ar *auth.AuthResult) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/llmlog/"+id, nil)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", id)
		reqCtx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
		req = req.WithContext(context.WithValue(reqCtx, authResultKey, ar))
		rec := httptest.NewRecorder()
		h.HandleLLMLogDetail(rec, req)
		return rec
	}
	detailOf := func(t *testing.T, rec *httptest.ResponseRecorder) llmlogDetail {
		t.Helper()
		var got struct {
			Success bool         `json:"success"`
			Detail  llmlogDetail `json:"detail"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode detail: %v (body %s)", err, rec.Body.String())
		}
		return got.Detail
	}

	admA := &auth.AuthResult{IsValid: true, TenantID: tenantA, TenantRole: auth.RoleAdmin}
	admB := &auth.AuthResult{IsValid: true, TenantID: tenantB, TenantRole: auth.RoleAdmin}
	serverAdmin := &auth.AuthResult{IsValid: true, IsAdmin: true}

	// (1) Override path: own tenant-admin AND server-admin see the bodies.
	for name, ar := range map[string]*auth.AuthResult{"tenant-admin-A": admA, "server-admin": serverAdmin} {
		rec := fetch(unsealed, ar)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s unsealed row: status %d body %s, want 200", name, rec.Code, rec.Body.String())
		}
		d := detailOf(t, rec)
		if d.BodyState != bodyPresent {
			t.Errorf("%s unsealed row: body_state = %q, want %q", name, d.BodyState, bodyPresent)
		}
		if d.RequiredSensitivity != "credentials" {
			t.Errorf("%s unsealed row: sensitivity = %q, want credentials (the class must stay visible)", name, d.RequiredSensitivity)
		}
		if d.RequestSystem == nil || *d.RequestSystem != "SYS-DEVMODE-A" ||
			d.RequestUser == nil || *d.RequestUser != "USER-DEVMODE-A" ||
			d.ResponseContent == nil || *d.ResponseContent != "RESP-DEVMODE-A" {
			t.Errorf("%s unsealed row: bodies not handed out: %+v", name, d)
		}
	}

	// (2) Isolation: tenant B is refused A's unsealed row, uniformly and
	// without a byte of it in the answer. Counter-probe: B's OWN unsealed row
	// is servable to B, so the 404 is the gate and not a broken fetch.
	rec := fetch(unsealed, admB)
	if rec.Code != http.StatusNotFound {
		t.Errorf("tenant-B on A's unsealed row: status %d, want uniform 404", rec.Code)
	}
	for _, marker := range []string{"SYS-DEVMODE-A", "USER-DEVMODE-A", "RESP-DEVMODE-A"} {
		if strings.Contains(rec.Body.String(), marker) {
			t.Errorf("tenant-B on A's unsealed row: %s leaked: %s", marker, rec.Body.String())
		}
	}
	if rec := fetch(foreign, admB); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "SYS-DEVMODE-B") {
		t.Errorf("tenant-B own unsealed row: status %d body %s, want 200 with its own body", rec.Code, rec.Body.String())
	}

	// (3) No retroactive unsealing: the slimmed row keeps reading sealed with
	// null bodies for everyone, devmode or not — there is nothing to unseal.
	for name, ar := range map[string]*auth.AuthResult{"tenant-admin-A": admA, "server-admin": serverAdmin} {
		rec := fetch(slimmed, ar)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s slimmed row: status %d, want 200", name, rec.Code)
		}
		d := detailOf(t, rec)
		if d.BodyState != bodySealed {
			t.Errorf("%s slimmed row: body_state = %q, want %q (no retroactive unsealing)", name, d.BodyState, bodySealed)
		}
		if d.RequestSystem != nil || d.RequestUser != nil || d.ResponseContent != nil {
			t.Errorf("%s slimmed row must render null bodies, got %+v", name, d)
		}
	}
}
