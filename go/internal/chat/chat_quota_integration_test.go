//go:build integration

// GHSA 2026-10-31 §4 path b): the web-chat chain is gated by the tenant quota
// exactly like the synthesis path. Driven through the PRODUCTION PoolProvider
// over a seeded backends.Pool and a real QuotaAccountant against PG18, so the
// probe covers the wiring, not a fake provider. The quota row sits on the
// tenant's first scope while the session belongs to a second scope of the
// same tenant — the tenant-wide policy lookup and the chat gate together.
//
//	go test -tags=integration ./internal/chat/ -run TestChatQuota -count=1 -v
package chat_test

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/chat"
	"github.com/GottZ/ctx/internal/testdb"
)

// seedQuotaTenant maps scopes[0..] to one new tenant and puts the given quota
// row on scopes[0]. dailyCalls/dailyCost nil = unlimited on that axis.
func seedQuotaTenant(t *testing.T, pool *pgxpool.Pool, slug string, scopes []string, dailyCalls *int, dailyCost *float64, onExceed string) string {
	t.Helper()
	ctx := context.Background()
	var tenantID string
	if err := pool.QueryRow(ctx, `INSERT INTO context_tenants (slug, display_name) VALUES ($1,$2) RETURNING id::text`, slug, slug).Scan(&tenantID); err != nil {
		t.Fatalf("tenant %s: %v", slug, err)
	}
	for _, s := range scopes {
		if _, err := pool.Exec(ctx, `INSERT INTO context_tenant_scopes (scope, tenant_id) VALUES ($1,$2::uuid)`, s, tenantID); err != nil {
			t.Fatalf("scope %s: %v", s, err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO context_tenant_quota (scope, daily_calls, daily_cost_usd, on_exceed) VALUES ($1,$2,$3,$4)`,
		scopes[0], dailyCalls, dailyCost, onExceed); err != nil {
		t.Fatalf("quota %s: %v", scopes[0], err)
	}
	return tenantID
}

// logExternalSpend writes an attributed external-cost row for a fresh key of
// the tenant, so the accountant sees the tenant over its cost budget.
func logExternalSpend(t *testing.T, pool *pgxpool.Pool, tenantID, homeScope string, cost float64) {
	t.Helper()
	ctx := context.Background()
	var keyID string
	if err := pool.QueryRow(ctx,
		`WITH p AS (INSERT INTO context_principals (display_name) VALUES ($2) RETURNING id)
		 INSERT INTO context_api_keys (key_hash, label, home_scope, tenant_id, principal_id)
		 SELECT $1,$2,$3,$4::uuid, p.id FROM p RETURNING id::text`, "cq-"+homeScope, "cq-"+homeScope, homeScope, tenantID).Scan(&keyID); err != nil {
		t.Fatalf("key: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_llm_log (pipeline, model, host, duration_ms, api_key_id, cost_usd, backend_locality)
		 VALUES ('web-chat','m','h',10,$1::uuid,$2,'external')`, keyID, cost); err != nil {
		t.Fatalf("spend: %v", err)
	}
}

func TestChatQuota(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	run := func(t *testing.T, b backends.Backend, sessScope string, acc *backends.QuotaAccountant) (*collector, *fakeLLM) {
		t.Helper()
		llmSrv := &fakeLLM{responses: []fakeResp{{sse: sseContent("served")}}}
		srv := httptest.NewServer(llmSrv.handler())
		t.Cleanup(srv.Close)
		b.Host = srv.URL
		bpool := backends.NewPool(nil, nil)
		bpool.SeedSnapshotForTest([]backends.Backend{b})
		exec := chat.NewExecutor(pool, &fakeQuery{}, 8000)
		eng := chat.NewEngine(pool, chat.NewPoolProvider(bpool, acc), exec, chat.Config{}, testAdmitter(t))
		sess := newSession(t, pool, sessScope, []string{sessScope})
		col := &collector{}
		if err := eng.RunTurn(ctx, &auth.AuthResult{HomeScope: sessScope, ReadScopes: []string{sessScope}}, sess, "hello", backends.SensInternal, false, col); err != nil {
			t.Fatalf("RunTurn: %v", err)
		}
		return col, llmSrv
	}
	refreshed := func(t *testing.T) *backends.QuotaAccountant {
		t.Helper()
		acc := backends.NewQuotaAccountant(pool, time.Hour)
		if err := acc.RefreshNow(ctx); err != nil {
			t.Fatalf("refresh: %v", err)
		}
		return acc
	}
	zero := 0

	// daily_calls=0 on the tenant's first scope; the session runs in its
	// SECOND scope. The turn ends with quota_exceeded and the backend is never
	// reached (before the fix: Chain without Gate → streamed answer).
	t.Run("CallBudgetBlocksSecondScope", func(t *testing.T) {
		seedQuotaTenant(t, pool, "cq-calls", []string{"cq-calls", "cq-calls:side"}, &zero, nil, backends.QuotaExceedExternalOff)
		col, llmSrv := run(t, fullTrustBackend("local", ""), "cq-calls:side", refreshed(t))
		if llmSrv.hitCount() != 0 {
			t.Fatalf("llm hits = %d; want 0 — the call budget must stop the chat before the wire", llmSrv.hitCount())
		}
		if e := col.last("error"); e == nil || e["code"] != "quota_exceeded" {
			t.Fatalf("error event = %v; want quota_exceeded", e)
		}
		if col.count("done") != 0 {
			t.Fatal("a quota-blocked turn must not emit done")
		}
	})

	// Cost budget over with on_exceed=block: quota_exceeded, no wire call.
	t.Run("CostBudgetBlock", func(t *testing.T) {
		tid := seedQuotaTenant(t, pool, "cq-blk", []string{"cq-blk"}, nil, ptrF(0.001), backends.QuotaExceedBlock)
		logExternalSpend(t, pool, tid, "cq-blk", 0.5)
		ext := fullTrustBackend("cloud", "")
		ext.Locality = backends.LocalityExternal
		col, llmSrv := run(t, ext, "cq-blk", refreshed(t))
		if llmSrv.hitCount() != 0 {
			t.Fatalf("llm hits = %d; want 0", llmSrv.hitCount())
		}
		if e := col.last("error"); e == nil || e["code"] != "quota_exceeded" {
			t.Fatalf("error event = %v; want quota_exceeded", e)
		}
	})

	// Cost budget over with external_off and only an external backend: the
	// degrade empties the chain → no_eligible_backend, no wire call.
	t.Run("CostBudgetExternalOffDropsExternal", func(t *testing.T) {
		tid := seedQuotaTenant(t, pool, "cq-eo", []string{"cq-eo"}, nil, ptrF(0.001), backends.QuotaExceedExternalOff)
		logExternalSpend(t, pool, tid, "cq-eo", 0.5)
		ext := fullTrustBackend("cloud", "")
		ext.Locality = backends.LocalityExternal
		col, llmSrv := run(t, ext, "cq-eo", refreshed(t))
		if llmSrv.hitCount() != 0 {
			t.Fatalf("llm hits = %d; want 0 — external_off must drop the external backend", llmSrv.hitCount())
		}
		if e := col.last("error"); e == nil || e["code"] != "no_eligible_backend" {
			t.Fatalf("error event = %v; want no_eligible_backend", e)
		}
	})

	// Control: a scope whose tenant has no quota row streams normally.
	t.Run("NoQuotaStreams", func(t *testing.T) {
		col, llmSrv := run(t, fullTrustBackend("local", ""), "cq-free", refreshed(t))
		if llmSrv.hitCount() != 1 {
			t.Fatalf("llm hits = %d; want 1", llmSrv.hitCount())
		}
		if done := col.last("done"); done == nil || done["finish_reason"] != "stop" {
			t.Fatalf("done = %v; want finish_reason stop (events %v)", done, col.last("error"))
		}
	})
}

func ptrF(v float64) *float64 { return &v }
