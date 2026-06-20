//go:build integration

// Integration test for MT wave T36b (Achse 04-W4): the tenant-quota-set/get
// management round-trip against PG18 — a server-admin set persists + refreshes
// the live accountant, a tenant-admin get is pinned to its own scope.
//
//	go test -tags=integration ./internal/handler/ -run TestQuotaManage -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestQuotaManageRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// Map two tenant scopes (no keys needed — quota is keyed on scope).
	for _, scope := range []string{"tq-a", "tq-other"} {
		var tid string
		if err := pool.QueryRow(ctx, `INSERT INTO context_tenants (slug, display_name) VALUES ($1,$2) RETURNING id::text`, scope, scope).Scan(&tid); err != nil {
			t.Fatalf("tenant %s: %v", scope, err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO context_tenant_scopes (scope, tenant_id) VALUES ($1,$2::uuid)`, scope, tid); err != nil {
			t.Fatalf("scope %s: %v", scope, err)
		}
	}

	acc := backends.NewQuotaAccountant(pool, time.Minute)
	h := NewManageHandler(pool, nil, nil, nil, nil, nil, acc)

	callSet := func(ar *auth.AuthResult, data map[string]any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(data)
		rec := httptest.NewRecorder()
		h.handleTenantQuotaSet(rec, httptest.NewRequest(http.MethodPost, "/api/manage", nil), ar, manageRequest{Data: raw})
		return rec
	}
	callGet := func(ar *auth.AuthResult, data map[string]any) map[string]any {
		raw, _ := json.Marshal(data)
		rec := httptest.NewRecorder()
		h.handleTenantQuotaGet(rec, httptest.NewRequest(http.MethodPost, "/api/manage", nil), ar, manageRequest{Data: raw})
		var resp struct {
			Quota map[string]any `json:"quota"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode get: %v (body %s)", err, rec.Body.String())
		}
		return resp.Quota
	}

	server := &auth.AuthResult{IsValid: true, IsAdmin: true, HomeScope: "_global"}

	// set persists.
	t.Run("set_persists", func(t *testing.T) {
		rec := callSet(server, map[string]any{"scope": "tq-a", "daily_cost_usd": 5.0, "on_exceed": "block"})
		if rec.Code != http.StatusOK {
			t.Fatalf("set: status %d, body %s", rec.Code, rec.Body.String())
		}
		q, found, err := store.GetTenantQuota(ctx, pool, "tq-a")
		if err != nil || !found {
			t.Fatalf("readback: found=%v err=%v", found, err)
		}
		if q.DailyCostUSD == nil || *q.DailyCostUSD != 5.0 || q.OnExceed != "block" {
			t.Fatalf("persisted policy wrong: %+v", q)
		}
	})

	// set refreshes the live accountant: a daily_calls=0 policy makes the very
	// next call over budget — Gate errors right after the set (no TTL wait).
	t.Run("set_refreshes_accountant", func(t *testing.T) {
		rec := callSet(server, map[string]any{"scope": "tq-a", "daily_calls": 0})
		if rec.Code != http.StatusOK {
			t.Fatalf("set calls=0: status %d", rec.Code)
		}
		chain := []backends.Backend{{ID: "x", Name: "local", Locality: "local"}}
		_, err := acc.Gate("tq-a", chain)
		var qe *backends.ErrQuotaExceeded
		if err == nil || !errors.As(err, &qe) {
			t.Fatalf("set should have refreshed the accountant (calls=0 → over budget), Gate err=%v", err)
		}
	})

	// tenant-admin get is pinned to its OWN scope (payload scope ignored).
	t.Run("tenant_admin_get_pinned", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `INSERT INTO context_tenant_quota (scope, daily_cost_usd, on_exceed) VALUES ('tq-other', 1.0, 'block')`); err != nil {
			t.Fatalf("seed tq-other quota: %v", err)
		}
		ta := &auth.AuthResult{IsValid: true, HomeScope: "tq-a", TenantID: "tid-a", TenantRole: auth.RoleAdmin}
		// Even asking for tq-other, a tenant-admin only ever sees its own scope.
		got := callGet(ta, map[string]any{"scope": "tq-other"})
		if got["scope"] != "tq-a" {
			t.Fatalf("tenant-admin get not pinned to own scope: got scope=%v (want tq-a)", got["scope"])
		}
	})

	// server-admin get can target any scope.
	t.Run("server_admin_get_any_scope", func(t *testing.T) {
		got := callGet(server, map[string]any{"scope": "tq-other"})
		if got["scope"] != "tq-other" {
			t.Fatalf("server-admin get should reach tq-other: got %v", got["scope"])
		}
	})
}
