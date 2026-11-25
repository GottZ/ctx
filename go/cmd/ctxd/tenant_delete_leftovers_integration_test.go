//go:build integration

// Issue #555, review round: what a tenant-delete that answered success left
// for the next owner of the tenant's scope names — through the PRODUCTION
// router, every step a request (tenant-create, the tenant owner's
// backend-create, the server-admin's api-key-create, tenant-delete, and the
// next tenant created with the same slug).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GottZ/ctx/internal/store"
)

// leftoversAdminKey is the server-admin key the fixture bootstraps.
const leftoversAdminKey = "aa1120261125000000000000000000000a0d17a0d17a0d17a0d17a0d17a0555"

type leftoversFixture struct{ f *inflightFixture }

func newLeftoversFixture(t *testing.T) leftoversFixture {
	t.Helper()
	f := newInflightFixture(t)
	if created, _, err := store.BootstrapAdminKey(context.Background(), f.pool, leftoversAdminKey, "555 leftovers admin"); err != nil || !created {
		t.Fatalf("bootstrap admin key: created=%v err=%v", created, err)
	}
	return leftoversFixture{f: f}
}

// call sends one JSON request through the router with key.
func (lf leftoversFixture) call(t *testing.T, key, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	req.RemoteAddr = "192.0.2.56:4000"
	req.Header.Set("Authorization", "Bearer "+key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	lf.f.router.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// manage runs one /api/manage action and wants 200.
func (lf leftoversFixture) manage(t *testing.T, key, action string, fields map[string]any) map[string]any {
	t.Helper()
	body := map[string]any{"action": action}
	for k, v := range fields {
		body[k] = v
	}
	st, out := lf.call(t, key, http.MethodPost, "/api/manage", body)
	if st != http.StatusOK {
		t.Fatalf("%s: HTTP %d %v", action, st, out)
	}
	return out
}

// tenant creates a tenant through tenant-create and returns its id, its
// '<slug>:main' scope and its owner key.
func (lf leftoversFixture) tenant(t *testing.T, slug string) (id, scope, owner string) {
	t.Helper()
	out := lf.manage(t, leftoversAdminKey, "tenant-create", map[string]any{"data": map[string]any{"slug": slug, "display_name": slug}})
	tn, _ := out["tenant"].(map[string]any)
	id, _ = tn["id"].(string)
	scope, _ = out["scope"].(string)
	owner, _ = out["owner_key"].(string)
	if id == "" || scope == "" || owner == "" {
		t.Fatalf("tenant-create %s: %v", slug, out)
	}
	return id, scope, owner
}

// TestTenantDeleteLeavesNoBackendForTheNextOwner (B3): a tenant owner
// creates a backend (its scope is the owner's home scope); after the
// tenant-delete, the owner of the next tenant created with the same slug
// lists no backend of the deleted one — before, it listed it, active, and its
// model calls could go to the endpoint the deleted tenant chose.
func TestTenantDeleteLeavesNoBackendForTheNextOwner(t *testing.T) {
	lf := newLeftoversFixture(t)
	id, scope, owner := lf.tenant(t, "p555bk")
	lf.manage(t, owner, "backend-create", map[string]any{"data": map[string]any{
		"name": "p555-chosen", "base_url": "https://llm.example.net", "protocol": "openai",
		"roles": []string{"chat"}, "model_map": map[string]any{"default": map[string]any{"model": "m"}},
		"priority": 100, "scope": scope,
	}})
	lf.manage(t, leftoversAdminKey, "tenant-delete", map[string]any{"id": id})
	_, nextScope, nextOwner := lf.tenant(t, "p555bk")
	if nextScope != scope {
		t.Fatalf("the next tenant's scope %q, want %q", nextScope, scope)
	}
	var n int
	if err := lf.f.pool.QueryRow(context.Background(), `SELECT count(*)::int FROM context_backends WHERE scope = $1`, scope).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d backends left in %s (%v)", n, scope, err)
	}
	// The next owner configures a backend of its own; the pool reloads from
	// the table (in a running instance the delete's NOTIFY reloads it at once,
	// the listener this fixture does not start), and the list is what the
	// pool now serves to the next owner's calls.
	lf.manage(t, nextOwner, "backend-create", map[string]any{"data": map[string]any{
		"name": "p555-own", "base_url": "https://llm2.example.net", "protocol": "openai",
		"roles": []string{"chat"}, "model_map": map[string]any{"default": map[string]any{"model": "m"}},
		"priority": 100, "scope": scope,
	}})
	out := lf.manage(t, nextOwner, "backend-list", nil)
	rows, _ := out["backends"].([]any)
	for _, r := range rows {
		b, _ := r.(map[string]any)
		if b["name"] == "p555-chosen" {
			t.Fatalf("the next owner of %s lists the deleted tenant's backend: %v", scope, b)
		}
	}
}

// TestTenantDeleteTakesItsScopesFromOtherKeysOverTheRouter (B2): a
// server-admin mints two keys of its own tenant that name the tenant's scope
// — one with its home there (api-key-create without tenant_id: no gate), one
// with it in its allowed and write scopes. Both write there before the
// delete. After a tenant-delete that answered success no row is left in the
// scope, the first key is gone (its next request: 401), and neither writes
// into the scope of the next tenant created with the same slug — before, the
// first wrote across the delete and both wrote into the next owner's scope.
func TestTenantDeleteTakesItsScopesFromOtherKeysOverTheRouter(t *testing.T) {
	lf := newLeftoversFixture(t)
	id, scope, _ := lf.tenant(t, "p555fk")
	homeOut := lf.manage(t, leftoversAdminKey, "api-key-create", map[string]any{"data": map[string]any{"label": "bridge home", "home_scope": scope}})
	homeKey, _ := homeOut["api_key"].(string)
	listOut := lf.manage(t, leftoversAdminKey, "api-key-create", map[string]any{"data": map[string]any{
		"label": "bridge list", "home_scope": "private", "allowed_scopes": []string{scope}, "write_scopes": []string{scope}}})
	listKey, _ := listOut["api_key"].(string)
	if homeKey == "" || listKey == "" {
		t.Fatalf("api-key-create: %v / %v", homeOut, listOut)
	}
	for _, k := range []string{homeKey, listKey} {
		if st, out := lf.call(t, k, http.MethodPost, "/api/store", map[string]any{"category": "learnings", "title": "bridge " + k[:6], "content": "written by a key of another tenant", "scope": scope}); st != http.StatusOK {
			t.Fatalf("store before the delete: %d %v", st, out)
		}
	}
	lf.manage(t, leftoversAdminKey, "tenant-delete", map[string]any{"id": id})
	left := rowsIn(t, lf.f.pool, scope)
	st, out := lf.call(t, homeKey, http.MethodPost, "/api/store", map[string]any{"category": "learnings", "title": "bridge home after", "content": "after the delete"})
	if st != http.StatusUnauthorized {
		t.Errorf("the key whose home was the deleted tenant's scope, after the delete: %d %v, want 401", st, out)
	}
	_, nextScope, _ := lf.tenant(t, "p555fk")
	if nextScope != scope {
		t.Fatalf("the next tenant's scope %q, want %q", nextScope, scope)
	}
	st2, out2 := lf.call(t, listKey, http.MethodPost, "/api/store", map[string]any{"category": "learnings", "title": "bridge list after", "content": "into the next owner's scope", "scope": scope})
	if len(left) != 0 {
		t.Errorf("rows left in %s after a delete that answered success: %v", scope, left)
	}
	if st2 == http.StatusOK {
		t.Errorf("a key of another tenant wrote into the next owner's %s: %v", scope, out2)
	}
	if st, out := lf.call(t, listKey, http.MethodPost, "/api/store", map[string]any{"category": "learnings", "title": "bridge own", "content": "its own home"}); st != http.StatusOK {
		t.Errorf("the listing key in its own home after the delete: %d %v", st, out)
	}
}
