//go:build integration

package events

// The limits registry on the real notify path (wave W06-15, Fremd-Review 1
// L-3): a psql INSERT into context_gateway_limits fires the 168 trigger,
// its payload reaches the registry through HandleNotification without a
// settings reload; a write while disconnected reaches it through
// HandleBacklog.
//
//	go test -tags=integration -p 1 -run TestGatewayLimitsListener ./internal/events/ -count=1 -v

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/gatewaylimits"
	"github.com/GottZ/ctx/internal/keyquota"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestGatewayLimitsListener(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	var tenant, consumer string
	if err := pool.QueryRow(ctx, `INSERT INTO context_tenants (slug, display_name) VALUES ('gl-ev', 'gl-ev') RETURNING id::text`).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO context_model_consumers (tenant_id, label) VALUES ($1::uuid, 'gl-ev-app') RETURNING id::text`, tenant).Scan(&consumer); err != nil {
		t.Fatal(err)
	}
	reg := gatewaylimits.NewRegistry(func(ctx context.Context) ([]gatewaylimits.Row, error) { return store.LoadGatewayLimits(ctx, pool) })
	if err := reg.Load(ctx); err != nil {
		t.Fatalf("boot load: %v", err)
	}
	var builds atomic.Int64
	cfg := config.NewStore(envBaseConfig(t))
	cfg.SetOverlay(func(_ context.Context, b *config.Config, _ string) (*config.Config, error) {
		builds.Add(1)
		c := *b
		return &c, nil
	})
	h := NewSettingsWriteHandler(pool, cfg, nil, nil)
	h.gatewayLimits = reg
	floor := gatewaylimits.Defaults{RPM: 30, TPM: 1, Concurrency: 1}
	await := func(want int64) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for reg.Snapshot().Resolve(consumer, tenant, keyquota.OpChat, floor).RPM != want {
			if time.Now().After(deadline) {
				t.Fatalf("registry rpm %d, want %d", reg.Snapshot().Resolve(consumer, tenant, keyquota.OpChat, floor).RPM, want)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	t.Run("psql_insert_reaches_the_registry", func(t *testing.T) {
		before := builds.Load()
		lc, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer lc.Release()
		if _, err := lc.Exec(ctx, "LISTEN "+channelSettingsWrite); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO context_gateway_limits (subject_kind, consumer_id, tier, op, rpm)
			VALUES ('consumer', $1::uuid, 'server', 'chat', 7)`, consumer); err != nil {
			t.Fatal(err)
		}
		wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		n, err := lc.Conn().WaitForNotification(wctx)
		if err != nil {
			t.Fatalf("no notification for the limits insert: %v", err)
		}
		if err := h.HandleNotification(ctx, n, nil); err != nil {
			t.Fatalf("HandleNotification: %v", err)
		}
		await(7)
		if got := builds.Load() - before; got != 0 {
			t.Errorf("a limits write rebuilt the settings %d time(s), want 0 (only the registry reloads)", got)
		}
	})

	t.Run("backlog_reloads_after_a_disconnect", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `UPDATE context_gateway_limits SET rpm = 9 WHERE consumer_id = $1::uuid`, consumer); err != nil {
			t.Fatal(err)
		}
		if err := h.HandleBacklog(ctx, channelSettingsWrite, nil); err != nil {
			t.Fatalf("HandleBacklog: %v", err)
		}
		await(9)
	})
}
