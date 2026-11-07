//go:build integration

// W06-14 against a real PG18 + TimescaleDB: the asymmetry of the tenant
// quota (masterplan K17, design/06 §4.5, B-06-18). The consumer's /v1 calls
// never count into the tenant's internal daily_calls — the knowledge paths
// (/api/query, the web chat: Gate(scope)) stay open (masterplan R5, a
// regression pin: red only against the spend-union overlay, report); the
// tenant's own internal calls do stop /v1 (GateTenant), and the refusal
// carries the earliest relief: the oldest counted row + 24 h.
//
//	go test -tags=integration -p 1 ./internal/backends/ -run 'Asymmetry' -count=1 -v
package backends_test

import (
	"context"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestGateTenantAsymmetryAndRelief(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const n = 3
	scope, key := seedQuotaTenant(t, pool, "w0614-asym", "w0614-asym")
	setQuota(t, pool, scope, nil, ipv(n), "block")
	var tenant, consumer string
	if err := pool.QueryRow(ctx, `SELECT tenant_id::text FROM context_tenant_scopes WHERE scope = $1`, scope).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO context_model_consumers (tenant_id, label) VALUES ($1::uuid, 'w0614-app') RETURNING id::text`, tenant).Scan(&consumer); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	// R5: n /v1 calls of the tenant's consumer.
	for i := range n {
		if _, err := pool.Exec(ctx, `INSERT INTO context_llm_log (created_at, pipeline, model, host, duration_ms, consumer_id, backend_locality)
			 VALUES ($1, 'gateway-chat', 'm', 'h', 10, $2::uuid, 'local')`, now.Add(-time.Duration(i+1)*time.Minute), consumer); err != nil {
			t.Fatal(err)
		}
	}
	a := backends.NewQuotaAccountant(pool, time.Minute)
	a.SetClockForTest(func() time.Time { return now })
	if err := a.RefreshNow(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := a.Gate(scope, mixedChain()); err != nil || len(got) != 2 {
		t.Fatalf("R5: after %d /v1 calls the knowledge path (Gate(scope)) got %d rows, err %v — want the chain (200 on /api/query and the web chat)", n, len(got), err)
	}
	if _, err := a.GateTenant(tenant, mixedChain()); err != nil {
		t.Fatalf("after %d /v1 calls /v1 itself (GateTenant) is refused: %v — the consumer's calls never count", n, err)
	}

	// The back direction: n internal calls, the oldest 23 h ago.
	oldest := now.Add(-23 * time.Hour).Add(7 * time.Second)
	for _, at := range []time.Time{oldest, now.Add(-2 * time.Hour), now.Add(-time.Hour)} {
		if _, err := pool.Exec(ctx, `INSERT INTO context_llm_log (created_at, pipeline, model, host, duration_ms, api_key_id, backend_locality)
			 VALUES ($1, 'query-synthesize', 'm', 'h', 10, $2::uuid, 'local')`, at, key); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.RefreshNow(ctx); err != nil {
		t.Fatal(err)
	}
	_, err := a.GateTenant(tenant, mixedChain())
	var qe *backends.ErrQuotaExceeded
	if !errorsAs(err, &qe) || qe.Reason != "daily_calls" {
		t.Fatalf("after %d internal calls: /v1 GateTenant %v, want daily_calls", n, err)
	}
	if want := oldest.Add(24 * time.Hour); !qe.ReliefAt.Equal(want) {
		t.Errorf("ReliefAt %v, want the oldest counted row + 24 h = %v", qe.ReliefAt, want)
	}
	if _, err := a.Gate(scope, mixedChain()); !errorsAs(err, &qe) {
		t.Errorf("the internal path itself is not gated after %d internal calls: %v", n, err)
	}
}
