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
	"github.com/GottZ/ctx/internal/llmlog"
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

	// §C1: the chat books its OWN spend. A turn on an openrouter-class backend
	// whose final usage chunk reports a cost writes cost_usd on its web-chat
	// row; after the next refresh the tenant is over its cost budget and the
	// following turn is blocked before the wire. Without the stream-side
	// booking the row carries NULL and the second turn streams again.
	t.Run("ChatSpendAccrues", func(t *testing.T) {
		tid := seedQuotaTenant(t, pool, "cq-spend", []string{"cq-spend"}, nil, ptrF(0.01), backends.QuotaExceedBlock)
		keyID := seedTenantKey(t, pool, tid, "cq-spend")
		acc := refreshed(t)

		turn := func(sse string) (*collector, *fakeLLM) {
			llmSrv := &fakeLLM{responses: []fakeResp{{sse: sse}}}
			srv := httptest.NewServer(llmSrv.handler())
			t.Cleanup(srv.Close)
			b := fullTrustBackend("or", srv.URL)
			b.Locality = backends.LocalityExternal
			b.ProviderClass = backends.ProviderOpenRouter
			bpool := backends.NewPool(nil, nil)
			bpool.SeedSnapshotForTest([]backends.Backend{b})
			eng := chat.NewEngine(pool, chat.NewPoolProvider(bpool, acc), chat.NewExecutor(pool, &fakeQuery{}, 8000), chat.Config{}, testAdmitter(t))
			sess := newSession(t, pool, "cq-spend", []string{"cq-spend"})
			col := &collector{}
			ar := &auth.AuthResult{ApiKeyID: keyID, HomeScope: "cq-spend", ReadScopes: []string{"cq-spend"}, IsValid: true}
			if err := eng.RunTurn(ctx, ar, sess, "hello", backends.SensInternal, false, col); err != nil {
				t.Fatalf("RunTurn: %v", err)
			}
			return col, llmSrv
		}
		costly := frame(`{"id":"gen-1","model":"m","choices":[{"index":0,"delta":{"content":"paid"},"finish_reason":null}]}`) +
			frame(`{"id":"gen-1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`) +
			frame(`{"id":"gen-1","model":"m","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"cost":0.05}}`) +
			"data: [DONE]\n\n"

		col, llmSrv := turn(costly)
		if llmSrv.hitCount() != 1 || col.last("done") == nil {
			t.Fatalf("first turn: hits %d done %v error %v; want one streamed answer", llmSrv.hitCount(), col.last("done"), col.last("error"))
		}
		if err := llmlog.Settle(ctx, pool); err != nil {
			t.Fatalf("settle: %v", err)
		}
		var booked *float64
		if err := pool.QueryRow(ctx, `SELECT cost_usd FROM context_llm_log WHERE pipeline='web-chat' AND api_key_id=$1::uuid`, keyID).Scan(&booked); err != nil {
			t.Fatalf("web-chat row: %v", err)
		}
		if booked == nil || *booked != 0.05 {
			t.Fatalf("web-chat cost_usd = %v; want 0.05 — the chat must book the provider-reported cost", booked)
		}
		if err := acc.RefreshNow(ctx); err != nil {
			t.Fatalf("refresh: %v", err)
		}
		col, llmSrv = turn(sseContent("again"))
		if llmSrv.hitCount() != 0 {
			t.Fatalf("second turn: llm hits = %d; want 0 — the chat's own spend must exhaust the cost budget", llmSrv.hitCount())
		}
		if e := col.last("error"); e == nil || e["code"] != "quota_exceeded" {
			t.Fatalf("second turn: error event = %v; want quota_exceeded", e)
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

// seedTenantKey mints a key of the tenant homed in homeScope and returns its
// id — the attribution the accountant's spend query joins on.
func seedTenantKey(t *testing.T, pool *pgxpool.Pool, tenantID, homeScope string) string {
	t.Helper()
	var keyID string
	if err := pool.QueryRow(context.Background(),
		`WITH p AS (INSERT INTO context_principals (display_name) VALUES ($2) RETURNING id)
		 INSERT INTO context_api_keys (key_hash, label, home_scope, tenant_id, principal_id)
		 SELECT $1,$2,$3,$4::uuid, p.id FROM p RETURNING id::text`, "ck-"+homeScope, "ck-"+homeScope, homeScope, tenantID).Scan(&keyID); err != nil {
		t.Fatalf("key: %v", err)
	}
	return keyID
}
