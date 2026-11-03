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
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/modelalias"
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

// failingQuerier is a database that refuses every load.
type failingQuerier struct{}

func (failingQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("connection refused")
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
		if got := reg.Generation(); got != gen+1 {
			t.Errorf("registry generation %d after the NOTIFY, want %d (entity branch missing?)", got, gen+1)
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
		if got := reg.Generation(); got != gen+1 {
			t.Errorf("registry generation %d, want %d", got, gen+1)
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
		if err := h.HandleNotification(ctx, n, nil); err != nil {
			t.Fatalf("HandleNotification: %v", err)
		}
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
		if got := reg.Generation(); got != gen+1 || aliasRole(t, reg, "chat", "") != "chat-c" {
			t.Errorf("after the backlog: generation %d (want %d), role %s (want chat-c)", got, gen+1, aliasRole(t, reg, "chat", ""))
		}
	})

	t.Run("reload_failure_keeps_the_snapshot", func(t *testing.T) {
		dead := modelalias.NewRegistry(failingQuerier{})
		hd := NewSettingsWriteHandler(pool, cfg, nil, nil)
		hd.modelAliases = dead
		snapBefore := cfg.Snapshot()
		n := aliasNotification(ctx, t, pool, `UPDATE context_model_aliases SET role = 'chat-d' WHERE scope = '_global' AND name = 'chat'`)
		if err := hd.HandleNotification(ctx, n, nil); err != nil {
			t.Fatalf("HandleNotification returned %v — a reload failure must never reach pgxlisten", err)
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
