//go:build integration

// GHSA 2026-10-31 §4: the query path gates its embed chain by the tenant
// quota, before the embed cache and before synthesis. The quota row sits on
// the tenant's FIRST scope; the querying key is homed in a second scope of
// the same tenant (the scope-create bypass). Embed and synthesis backends
// count requests, so the proof is "hits == 0", not the response body alone.
//
//	go test -tags=integration ./internal/handler/ -run TestQueryQuota -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
)

func TestQueryQuotaEmbedGate(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := sfSetup(t, true)
	ctx := context.Background()

	drive := func(t *testing.T, acc *backends.QuotaAccountant, query string) (int, map[string]any, int32, int32) {
		t.Helper()
		embedSrv, embedHits := fakeEmbedServer(t)
		synthSrv, synthHits := sfSynthServer(t)
		h := NewQueryHandler(pool, config.NewStore(sfConfigWith(0)), sfPool(embedSrv.URL, synthSrv.URL),
			acc, blocktype.NewRegistry(), snapshotTestAdmitter(t))
		req := httptest.NewRequest(http.MethodPost, "/api/query", strings.NewReader(`{"query":"`+query+`","limit":5}`))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(context.WithValue(req.Context(), authResultKey, &auth.AuthResult{
			ApiKeyID: sfKeyID, HomeScope: sfScope, ReadScopes: []string{sfScope}, IsValid: true,
		}))
		rec := httptest.NewRecorder()
		h.HandleQuery(rec, req)
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body, embedHits.Load(), synthHits.Load()
	}
	refreshed := func(t *testing.T) *backends.QuotaAccountant {
		t.Helper()
		acc := backends.NewQuotaAccountant(pool, time.Hour)
		if err := acc.RefreshNow(ctx); err != nil {
			t.Fatalf("refresh: %v", err)
		}
		return acc
	}

	// Control first (no quota row anywhere): the same request embeds and
	// synthesizes, so a green gate run below cannot be a broken fixture.
	t.Run("no quota: embeds and synthesizes", func(t *testing.T) {
		code, body, eh, sh := drive(t, refreshed(t), "zzqqxx yyppww vvnnmm")
		if code != http.StatusOK || eh != 1 || sh != 1 {
			t.Fatalf("control: status %d embed hits %d synth hits %d (body %v); want 200/1/1", code, eh, sh, body)
		}
	})

	t.Run("tenant call budget exhausted: 429 before the embed wire", func(t *testing.T) {
		var tid string
		if err := pool.QueryRow(ctx, `INSERT INTO context_tenants (slug, display_name) VALUES ('qq-acme','qq-acme') RETURNING id::text`).Scan(&tid); err != nil {
			t.Fatalf("tenant: %v", err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO context_tenant_scopes (scope, tenant_id) VALUES ('qq-acme',$1::uuid), ($2,$1::uuid)`, tid, sfScope); err != nil {
			t.Fatalf("scopes: %v", err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO context_tenant_quota (scope, daily_calls) VALUES ('qq-acme', 0)`); err != nil {
			t.Fatalf("quota: %v", err)
		}
		// A query text the control did not embed: a cache hit must not be
		// what keeps the embed counter at 0.
		code, body, eh, sh := drive(t, refreshed(t), "uuttss rrqqpp oonnmm")
		if code != http.StatusTooManyRequests || body["error_code"] != "quota_exceeded" {
			t.Fatalf("status %d body %v; want 429 quota_exceeded", code, body)
		}
		if eh != 0 || sh != 0 {
			t.Fatalf("embed hits %d synth hits %d; want 0/0 — the gate must stop the query before any wire call", eh, sh)
		}
	})
}
