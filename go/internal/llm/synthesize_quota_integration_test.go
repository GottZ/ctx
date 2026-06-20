//go:build integration

// Integration test for MT wave T36 (Achse 04-W4): the quota gate is wired INTO
// the synthesis path — Synthesize consults the QuotaAccountant after resolving
// the chain and before the wire walk. A block-policy tenant over its cost budget
// gets *ErrQuotaExceeded instead of an external call (proving the gate is reached,
// not just unit-correct in isolation). The companion gate-logic + refresh tests
// live in internal/backends/quota*_test.go.
//
//	go test -tags=integration ./internal/llm/ -run TestSynthesize_Quota -count=1 -v
package llm_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestSynthesize_QuotaBlocksOverBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// Tenant scope with a key, a block-policy at a near-zero budget, and an
	// external spend row that blows past it.
	const scope = "t36-synth"
	var tenantID, keyID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_tenants (slug, display_name) VALUES ($1,$2) RETURNING id::text`, scope, scope).Scan(&tenantID); err != nil {
		t.Fatalf("tenant: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_tenant_scopes (scope, tenant_id) VALUES ($1,$2::uuid)`, scope, tenantID); err != nil {
		t.Fatalf("scope map: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_api_keys (key_hash, label, home_scope, tenant_id)
		 VALUES ($1,$2,$3,$4::uuid) RETURNING id::text`, "t36-synth-k", "t36-synth-k", scope, tenantID).Scan(&keyID); err != nil {
		t.Fatalf("key: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_tenant_quota (scope, daily_cost_usd, on_exceed) VALUES ($1,$2,'block')`, scope, 0.001); err != nil {
		t.Fatalf("quota: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_llm_log (pipeline, model, host, duration_ms, api_key_id, cost_usd, backend_locality)
		 VALUES ('query-synthesize','m','h',10,$1::uuid,0.5,'external')`, keyID); err != nil {
		t.Fatalf("log: %v", err)
	}

	// A backend that WOULD answer — the gate must stop the call before it.
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"x"}}],"usage":{"completion_tokens":1,"prompt_tokens":1}}`))
	}))
	defer srv.Close()

	bpool := backends.NewPool(nil, nil)
	bpool.SeedSnapshotForTest([]backends.Backend{{
		ID: "ext", Name: "cloud", Host: srv.URL, Protocol: backends.ProtocolOpenAI, Model: "m",
		Trust: backends.TrustFull, Enabled: true, Locality: backends.LocalityExternal,
		Roles: []string{backends.RoleSynthesis}, Scope: backends.GlobalScope,
	}})

	acc := backends.NewQuotaAccountant(pool, time.Minute)
	if err := acc.RefreshNow(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	settings := llm.SynthesisSettings{ScoreThreshold: 0.001, ConfidentThreshold: 0.008, PromptVersion: llm.PromptVersionV52}
	sources := []llm.Source{{ID: "00000000-0000-0000-0000-000000000001", Title: "t", Category: "c", Content: "body", Score: 0.5, AgeDays: 1}}

	_, err := llm.Synthesize(ctx, pool, bpool, acc, backends.GamingState{},
		settings, backends.SensPersonal, "q", sources, nil, keyID, scope)

	var qe *backends.ErrQuotaExceeded
	if err == nil || !errors.As(err, &qe) {
		t.Fatalf("over-budget block tenant should get ErrQuotaExceeded from the synthesis path, got %v", err)
	}
	if qe.Reason != "cost_budget" {
		t.Errorf("reason = %q, want cost_budget", qe.Reason)
	}
	if called {
		t.Error("the external backend was called despite the block budget — the gate did not stop the walk")
	}

	// Control: a scope with no quota row synthesizes normally (gate fail-open).
	_, err = llm.Synthesize(ctx, pool, bpool, acc, backends.GamingState{},
		settings, backends.SensPersonal, "q", sources, nil, "", "no-quota-scope")
	if err != nil {
		t.Fatalf("no-quota scope should pass the gate and synthesize: %v", err)
	}
	if !called {
		t.Error("control: the backend should have been called for the un-quota'd scope")
	}
}
