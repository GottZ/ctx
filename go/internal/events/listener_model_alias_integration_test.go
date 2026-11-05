//go:build integration

// Integration gate of the W02-03 hot-reload path (design/02 §4.4, §4.1 row
// internal/events/listener.go): a context_model_aliases write — psql
// included — reaches the model-alias registry through the settings channel
// without a restart, and ONLY the registry. The notification is the one the
// 162 trigger really sends (LISTEN on a pool connection), handed to the
// SettingsWriteHandler seam.
//
// NEGATIVE probe: without the entity branch a _global alias write falls
// through to settings.Reload — the config base is rebuilt (new snapshot
// pointer) and the registry keeps its generation; a tenant alias write
// drops that tenant's config generation instead. Both subtests go red.
//
//	go test -tags=integration -p 1 -count=1 -run 'TestListenerModelAlias' ./internal/events/
package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/GottZ/ctx/internal/pgxdb"
	"github.com/GottZ/ctx/internal/testdb"
)

// aliasNotification runs stmt and returns the ctx_settings_write
// notification it fired, read on a LISTENing connection of the pool.
func aliasNotification(ctx context.Context, t *testing.T, pool *pgxpool.Pool, stmt string, args ...any) *pgconn.Notification {
	t.Helper()
	lc, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire LISTEN conn: %v", err)
	}
	defer lc.Release()
	if _, err := lc.Exec(ctx, "LISTEN "+channelSettingsWrite); err != nil {
		t.Fatalf("LISTEN: %v", err)
	}
	defer func() { _, _ = lc.Exec(ctx, "UNLISTEN "+channelSettingsWrite) }()
	if _, err := pool.Exec(ctx, stmt, args...); err != nil {
		t.Fatalf("%s: %v", stmt, err)
	}
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	n, err := lc.Conn().WaitForNotification(wctx)
	if err != nil {
		t.Fatalf("no notification for %s: %v", stmt, err)
	}
	var p struct{ Entity string }
	if err := json.Unmarshal([]byte(n.Payload), &p); err != nil || p.Entity != "context_model_aliases" {
		t.Fatalf("notification %s (%v), want entity context_model_aliases", n.Payload, err)
	}
	return n
}

// failingQuerier is a database that refuses every load, counting them.
type failingQuerier struct{ calls atomic.Int64 }

func (f *failingQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	f.calls.Add(1)
	return nil, errors.New("connection refused")
}

// awaitGeneration waits until reg reached gen (the listener reloads
// asynchronously since Fremd-Review 2 R-1) and reports whether it did.
func awaitGeneration(reg *modelalias.Registry, gen uint64) bool {
	deadline := time.Now().Add(5 * time.Second)
	for reg.Generation() < gen {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(2 * time.Millisecond)
	}
	return true
}

func aliasRole(t *testing.T, reg *modelalias.Registry, name, tenant string) string {
	t.Helper()
	a, ok := modelalias.Lookup(reg.Snapshot(), name, tenant)
	if !ok {
		return "<none>"
	}
	return a.Role
}

func TestListenerModelAliasNotify_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	base := envBaseConfig(t)

	if _, err := pool.Exec(ctx, `INSERT INTO context_model_aliases (scope, name, kind, role) VALUES
	        ('_global', 'chat', 'chat', 'chat'), ('lm-acme:main', 'own', 'chat', 'chat')`); err != nil {
		t.Fatalf("seed aliases: %v", err)
	}
	reg := modelalias.NewRegistry(pool)
	if err := reg.Reload(ctx); err != nil {
		t.Fatalf("boot reload: %v", err)
	}

	var builds atomic.Int64
	cfg := config.NewStore(base)
	cfg.SetOverlay(func(_ context.Context, b *config.Config, _ string) (*config.Config, error) {
		builds.Add(1)
		c := *b
		return &c, nil
	})
	h := NewSettingsWriteHandler(pool, cfg, nil, nil)
	h.modelAliases = reg

	t.Run("global_alias_write_reloads_the_registry_only", func(t *testing.T) {
		n := aliasNotification(ctx, t, pool, `UPDATE context_model_aliases SET role = 'chat-b' WHERE scope = '_global' AND name = 'chat'`)
		gen, snapBefore := reg.Generation(), cfg.Snapshot()
		if err := h.HandleNotification(ctx, n, nil); err != nil {
			t.Fatalf("HandleNotification: %v", err)
		}
		if !awaitGeneration(reg, gen+1) || reg.Generation() != gen+1 {
			t.Errorf("registry generation %d after the NOTIFY, want %d (entity branch missing?)", reg.Generation(), gen+1)
		}
		if got := aliasRole(t, reg, "chat", ""); got != "chat-b" {
			t.Errorf("chat role after the NOTIFY = %s, want chat-b", got)
		}
		if cfg.Snapshot() != snapBefore {
			t.Errorf("the alias write rebuilt the config base (settings.Reload) — it must reach the registry only")
		}
	})

	t.Run("tenant_alias_write_keeps_the_tenant_config", func(t *testing.T) {
		cfg.SnapshotForTenant(ctx, "lm-acme:main")
		before := builds.Load()
		n := aliasNotification(ctx, t, pool, `UPDATE context_model_aliases SET role = 'own-b' WHERE scope = 'lm-acme:main' AND name = 'own'`)
		gen := reg.Generation()
		if err := h.HandleNotification(ctx, n, nil); err != nil {
			t.Fatalf("HandleNotification: %v", err)
		}
		if !awaitGeneration(reg, gen+1) || reg.Generation() != gen+1 {
			t.Errorf("registry generation %d, want %d", reg.Generation(), gen+1)
		}
		if got := aliasRole(t, reg, "own", "lm-acme:main"); got != "own-b" {
			t.Errorf("tenant alias role = %s, want own-b", got)
		}
		cfg.SnapshotForTenant(ctx, "lm-acme:main")
		if got := builds.Load() - before; got != 0 {
			t.Errorf("the tenant alias write dropped the tenant's config generation (%d rebuild(s)) — it is a pool scope, not a config write", got)
		}
	})

	t.Run("delete_reaches_the_registry", func(t *testing.T) {
		n := aliasNotification(ctx, t, pool, `DELETE FROM context_model_aliases WHERE scope = 'lm-acme:main' AND name = 'own'`)
		gen := reg.Generation()
		if err := h.HandleNotification(ctx, n, nil); err != nil {
			t.Fatalf("HandleNotification: %v", err)
		}
		awaitGeneration(reg, gen+1)
		if got := aliasRole(t, reg, "own", "lm-acme:main"); got != "<none>" {
			t.Errorf("deleted alias still offered (role %s)", got)
		}
	})

	t.Run("backlog_reloads_the_registry", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `UPDATE context_model_aliases SET role = 'chat-c' WHERE scope = '_global' AND name = 'chat'`); err != nil {
			t.Fatalf("update: %v", err)
		}
		gen := reg.Generation()
		if err := h.HandleBacklog(ctx, channelSettingsWrite, nil); err != nil {
			t.Fatalf("HandleBacklog: %v", err)
		}
		awaitGeneration(reg, gen+1)
		if got := reg.Generation(); got != gen+1 || aliasRole(t, reg, "chat", "") != "chat-c" {
			t.Errorf("after the backlog: generation %d (want %d), role %s (want chat-c)", got, gen+1, aliasRole(t, reg, "chat", ""))
		}
	})

	t.Run("reload_failure_keeps_the_snapshot", func(t *testing.T) {
		fq := &failingQuerier{}
		dead := modelalias.NewRegistry(fq)
		hd := NewSettingsWriteHandler(pool, cfg, nil, nil)
		hd.modelAliases = dead
		snapBefore := cfg.Snapshot()
		n := aliasNotification(ctx, t, pool, `UPDATE context_model_aliases SET role = 'chat-d' WHERE scope = '_global' AND name = 'chat'`)
		if err := hd.HandleNotification(ctx, n, nil); err != nil {
			t.Fatalf("HandleNotification returned %v — a reload failure must never reach pgxlisten", err)
		}
		for deadline := time.Now().Add(5 * time.Second); fq.calls.Load() == 0 && time.Now().Before(deadline); {
			time.Sleep(2 * time.Millisecond)
		}
		time.Sleep(20 * time.Millisecond)
		if fq.calls.Load() == 0 {
			t.Fatal("the failing registry was never asked to reload")
		}
		if dead.Generation() != 0 || cfg.Snapshot() != snapBefore {
			t.Errorf("failed reload: generation %d, config rebuilt %v — want 0 and no fall-through", dead.Generation(), cfg.Snapshot() != snapBefore)
		}
	})

	t.Run("no_registry_is_inert", func(t *testing.T) {
		hn := NewSettingsWriteHandler(pool, cfg, nil, nil)
		snapBefore := cfg.Snapshot()
		n := aliasNotification(ctx, t, pool, `UPDATE context_model_aliases SET role = 'chat-e' WHERE scope = '_global' AND name = 'chat'`)
		if err := hn.HandleNotification(ctx, n, nil); err != nil {
			t.Fatalf("HandleNotification: %v", err)
		}
		if cfg.Snapshot() != snapBefore {
			t.Errorf("without a registry the alias write fell through to settings.Reload")
		}
	})
}

// TestListenerWiresModelAliasRegistry_Integration (Fremd-Review F-2): the
// production listener built by NewPgxlistenListener carries the scheduler's
// registry — its backlog on connect reloads it, a psql write reaches it. Red
// without the hand-over in NewPgxlistenListener (the registry then stays at
// its boot load).
func TestListenerWiresModelAliasRegistry_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	reg := modelalias.NewRegistry(pool)
	s := &Scheduler{modelAliases: reg}
	cfg := config.NewStore(envBaseConfig(t))
	l := NewPgxlistenListener(dsn, 100*time.Millisecond, s, pool, cfg, nil, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = l.Listen(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	waitFor := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s (registry generation %d)", what, reg.Generation())
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	waitFor("the backlog reload on connect", func() bool { return reg.Generation() >= 1 })
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO context_model_aliases (name, kind, role) VALUES ('wired', 'chat', 'chat')`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	waitFor("the NOTIFY reload", func() bool {
		_, ok := modelalias.Lookup(reg.Snapshot(), "wired", "")
		return ok
	})
}

// recordingQuerier counts the loads through it; while hold is armed, the
// load with number holdAt (1-based) blocks until hold is closed.
type recordingQuerier struct {
	q      pgxdb.Querier
	calls  atomic.Int64
	holdAt int64
	hold   chan struct{}
}

func (r *recordingQuerier) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	n := r.calls.Add(1)
	if r.hold != nil && n == r.holdAt {
		select {
		case <-r.hold:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return r.q.Query(ctx, sql, args...)
}

// TestListenerModelAliasStorm_Integration (Fremd-Review 2 R-1): a burst of
// alias writes costs a bounded number of registry reloads, and the listener
// loop is not held by a running one. 100 single-row UPDATEs (100 NOTIFYs)
// arrive while the first triggered reload is held inside its load; then a
// context_backends notification follows on the same channel. Coalesced: the
// backend reload runs while the alias reload is still held (the listener is
// free), and the 100 notifications cost the held reload plus ONE rerun. Red
// against a reload on the listener loop: the backend reload waits for the
// held alias reload, and every notification is a full reload (100).
func TestListenerModelAliasStorm_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO context_model_aliases (name, kind, role) VALUES ('storm', 'chat', 'chat')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Load 1 = the backlog reload on connect; load 2 = the first one the
	// storm triggers — held until the backend reload was seen (or 3 s).
	aliasQ := &recordingQuerier{q: pool, holdAt: 2, hold: make(chan struct{})}
	reg := modelalias.NewRegistry(aliasQ)
	backendQ := &recordingQuerier{q: pool}
	s := &Scheduler{modelAliases: reg}
	l := NewPgxlistenListener(dsn, 100*time.Millisecond, s, pool, config.NewStore(envBaseConfig(t)), backends.NewPool(backendQ, nil), nil, nil)
	lctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); _ = l.Listen(lctx) }()
	released := false
	release := func() {
		if !released {
			released = true
			close(aliasQ.hold)
		}
	}
	t.Cleanup(func() { release(); cancel(); <-done })

	waitFor := func(what string, d time.Duration, ok func() bool) bool {
		deadline := time.Now().Add(d)
		for !ok() {
			if time.Now().After(deadline) {
				t.Logf("gave up waiting for %s after %v", what, d)
				return false
			}
			time.Sleep(5 * time.Millisecond)
		}
		return true
	}
	if !waitFor("the backlog reload", 10*time.Second, func() bool { return reg.Generation() >= 1 }) {
		t.Fatal("no backlog reload")
	}
	backendsAtStart := backendQ.calls.Load()

	stormStart := time.Now()
	for i := 0; i < 100; i++ {
		if _, err := pool.Exec(ctx, `UPDATE context_model_aliases SET role = $1 WHERE name = 'storm'`, fmt.Sprintf("r-%d", i)); err != nil {
			t.Fatalf("update %d: %v", i, err)
		}
	}
	stormTook := time.Since(stormStart)
	if !waitFor("the held alias reload", 5*time.Second, func() bool { return aliasQ.calls.Load() >= 2 }) {
		t.Fatal("the storm triggered no reload")
	}
	notified := time.Now()
	if _, err := pool.Exec(ctx, `SELECT pg_notify('ctx_settings_write', '{"entity":"context_backends","key":"x","scope":"_global","op":"UPDATE"}')`); err != nil {
		t.Fatalf("notify backends: %v", err)
	}
	backendSeenWhileHeld := waitFor("the backend reload while the alias reload is held", 3*time.Second,
		func() bool { return backendQ.calls.Load() > backendsAtStart })
	release()
	if !backendSeenWhileHeld {
		waitFor("the backend reload after the release", 60*time.Second, func() bool { return backendQ.calls.Load() > backendsAtStart })
	}
	backendLatency := time.Since(notified)
	// Quiescence: no further alias load for 500 ms, the last write visible.
	last, stable := aliasQ.calls.Load(), time.Now()
	waitFor("quiescence", 120*time.Second, func() bool {
		if n := aliasQ.calls.Load(); n != last {
			last, stable = n, time.Now()
		}
		return time.Since(stable) > 500*time.Millisecond
	})
	reloads := aliasQ.calls.Load() - 1 // minus the backlog load
	a, ok := modelalias.Lookup(reg.Snapshot(), "storm", "")
	t.Logf("storm: 100 writes in %v; alias reloads %d; backend reload seen while the alias reload was held: %v (latency after notify %v); final role %q, generation %d",
		stormTook.Round(time.Millisecond), reloads, backendSeenWhileHeld, backendLatency.Round(time.Millisecond), a.Role, reg.Generation())
	if !ok || a.Role != "r-99" {
		t.Errorf("after the storm the registry offers role %q (found %v), want the last write r-99", a.Role, ok)
	}
	if !backendSeenWhileHeld {
		t.Errorf("the context_backends notification waited for the running alias reload — the listener loop is held by it")
	}
	if reloads > 3 {
		t.Errorf("100 alias writes cost %d registry reloads, want ≤ 3 (coalesced)", reloads)
	}
}

// TestListenerSettingsWriteReclampsAliases_Integration (W02-04, design/02
// §3.3): the registry clamps tenant aliases to the global-only
// gateway.tenant_alias_ceiling.* at reload, so a _global settings write that
// moves the ceiling must reload the registry — without it the snapshot kept
// the old clamp until the next alias write. NEGATIVE probe: without the
// Trigger in the _global settings branch the generation stays and max_n
// stays clamped at 1.
func TestListenerSettingsWriteReclampsAliases_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	cfg := config.NewStore(envBaseConfig(t))
	if _, err := pool.Exec(ctx, `INSERT INTO context_model_aliases (scope, name, kind, role, policy)
	        VALUES ('lm-acme:main', 'chat', 'chat', 'chat', '{"max_n": 4}')`); err != nil {
		t.Fatalf("seed alias: %v", err)
	}
	reg := modelalias.NewRegistry(pool)
	reg.SetCeilings(func() modelalias.Ceilings {
		g := cfg.Snapshot().Gateway //nolint:forbidigo // test: the global-only ceiling, as cmd/ctxd reads it
		return modelalias.Ceilings{Tenant: modelalias.TenantCeiling{
			MaxN: g.TenantAliasCeiling.MaxN, TimeoutS: 600, MaxOutputTokens: 8192,
			EmbeddingMaxInputs: 32, RerankMaxDocuments: 128, ToolsMaxCount: 64}}
	})
	if err := reg.Reload(ctx); err != nil {
		t.Fatalf("boot reload: %v", err)
	}
	maxN := func() int {
		a, ok := modelalias.Lookup(reg.Snapshot(), "chat", "lm-acme:main")
		if !ok {
			return -1
		}
		return a.Policy.MaxN.V
	}
	if got := maxN(); got != 1 {
		t.Fatalf("boot: tenant max_n %d, want 1 (clamped to the default ceiling)", got)
	}
	h := NewSettingsWriteHandler(pool, cfg, nil, nil)
	h.modelAliases = reg

	lc, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire LISTEN conn: %v", err)
	}
	defer lc.Release()
	if _, err := lc.Exec(ctx, "LISTEN "+channelSettingsWrite); err != nil {
		t.Fatalf("LISTEN: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO context_settings (key, scope, value) VALUES ($1, '_global', $2)`,
		"gateway.tenant_alias_ceiling.max_n", json.RawMessage(`"4"`)); err != nil {
		t.Fatalf("insert setting: %v", err)
	}
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	n, err := lc.Conn().WaitForNotification(wctx)
	if err != nil {
		t.Fatalf("WaitForNotification: %v", err)
	}
	gen := reg.Generation()
	if err := h.HandleNotification(ctx, n, nil); err != nil {
		t.Fatalf("HandleNotification: %v", err)
	}
	if got := cfg.Snapshot().Gateway.TenantAliasCeiling.MaxN; got != 4 { //nolint:forbidigo // test: global-only key
		t.Fatalf("config after the settings write: ceiling max_n %d, want 4", got)
	}
	if !awaitGeneration(reg, gen+1) {
		t.Fatalf("registry generation stayed %d after a ceiling write — the _global settings branch does not reload the registry", gen)
	}
	if got := maxN(); got != 4 {
		t.Errorf("after raising the ceiling to 4: tenant max_n %d, want 4", got)
	}
}
