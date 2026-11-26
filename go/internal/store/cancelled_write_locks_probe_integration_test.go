//go:build integration

// Issue #555, point 9 (a measurement): a write whose request is cancelled
// while one of its statements waits for a row lock. With pgx's default
// context watcher (DeadlineContextWatcherHandler, what store.NewPool uses)
// the call returns at once, the pool drops the connection — and the backend
// keeps waiting with every lock it took, the scope's supersedes lock among
// them, until the row it waits for is free. Three pool variants, 20 rounds
// per operation each:
//
//	deadline: the default (the state of the base);
//	cancel:   pgconn.CancelRequestContextWatcherHandler (a cancel request on a
//	          second connection, the deadline after DeadlineDelay);
//	check:    the default watcher, and client_connection_check_interval on the
//	          connection, so a waiting backend checks its socket;
//	+full:    the same with every connection slot of the server taken
//	          (CTX_P555_CANCEL_VARIANTS=deadline+full,cancel+full).
//
//	CTX_P555_CANCEL_PROBE=1 go test -tags=integration -p 1 ./internal/store/ -run TestCancelledWriteLocksProbe -v
package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgconn/ctxwatch"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
)

func cancelProbePool(t *testing.T, dsn, variant string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	switch strings.TrimSuffix(variant, "+full") {
	case "cancel":
		cfg.ConnConfig.BuildContextWatcherHandler = func(c *pgconn.PgConn) ctxwatch.Handler {
			return &pgconn.CancelRequestContextWatcherHandler{Conn: c, DeadlineDelay: time.Second}
		}
	case "check":
		cfg.ConnConfig.RuntimeParams["client_connection_check_interval"] = "200"
	}
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

// othersLocks counts the lock entries (advisory, tuple, transactionid) of
// every backend but the probe's own and the holder's.
func othersLocks(t *testing.T, f *p547Fixture, holderPID uint32) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*)::int FROM pg_locks WHERE pid <> pg_backend_pid() AND pid <> $1 AND locktype IN ('advisory','tuple','transactionid')`,
		int(holderPID)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCancelledWriteLocksProbe(t *testing.T) {
	if os.Getenv("CTX_P555_CANCEL_PROBE") == "" {
		t.Skip("measurement: set CTX_P555_CANCEL_PROBE=1")
	}
	f := newP547Fixture(t)
	ctx := context.Background()
	const rounds = 20
	type op struct {
		name string
		run  func(ctx context.Context, p *pgxpool.Pool, scope, id, tenant string) error
	}
	ops := []op{
		{"UpsertBlocks", func(ctx context.Context, p *pgxpool.Pool, scope, _, _ string) error {
			_, err := store.UpsertBlocks(ctx, p, []store.BatchUpsert{{Category: "learnings", Title: "held", Content: "new", Scope: scope, ScopeExplicit: true}}, "", f.set, nil)
			return err
		}},
		{"DeleteBlock", func(ctx context.Context, p *pgxpool.Pool, scope, id, _ string) error {
			_, err := store.DeleteBlock(ctx, p, id, []string{scope})
			return err
		}},
		{"PurgeScope", func(ctx context.Context, p *pgxpool.Pool, scope, _, _ string) error {
			one := int64(1)
			_, err := store.PurgeScope(ctx, p, store.PurgeQuery{Scope: scope, AllTenants: true, ConfirmBlocks: &one})
			return err
		}},
		{"PruneTenant", func(ctx context.Context, p *pgxpool.Pool, _, _, tenant string) error {
			return store.PruneTenant(ctx, p, tenant, store.AuditStamp{})
		}},
	}
	variants := []string{"deadline", "cancel", "check"}
	if v := os.Getenv("CTX_P555_CANCEL_VARIANTS"); v != "" {
		variants = strings.Split(v, ",")
	}
	for _, variant := range variants {
		p := cancelProbePool(t, f.dsn, variant)
		for _, o := range ops {
			var atReturn, after1s, returned []string
			var maxReturn time.Duration
			for r := range rounds {
				slug := fmt.Sprintf("cp-%s-%s-%d", variant, strings.ToLower(o.name), r)
				tenant := f.tenant(t, slug, slug, "")
				id := f.block(t, slug, "held")
				holder, err := f.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := holder.Exec(ctx, `SELECT 1 FROM context_blocks WHERE id = $1::uuid FOR UPDATE`, id); err != nil {
					t.Fatal(err)
				}
				holderPID := holder.Conn().PgConn().PID()
				var fillers []*pgx.Conn
				if strings.HasSuffix(variant, "+full") {
					// Every connection slot taken (53300 for the next one):
					// the cancel request needs no slot of its own. The
					// operation's pool and the probe's keep one connection
					// each from before.
					if _, err := p.Exec(ctx, `SELECT 1`); err != nil {
						t.Fatal(err)
					}
					if _, err := f.pool.Exec(ctx, `SELECT 1`); err != nil {
						t.Fatal(err)
					}
					for {
						c, err := pgx.Connect(ctx, f.dsn)
						if err != nil {
							break
						}
						fillers = append(fillers, c)
					}
					if r == 0 {
						var maxConn, used int
						_ = f.pool.QueryRow(ctx, `SELECT current_setting('max_connections')::int, (SELECT count(*)::int FROM pg_stat_activity WHERE backend_type = 'client backend')`).Scan(&maxConn, &used)
						t.Logf("%s %s: %d filler connections, %d client backends of max_connections %d", variant, o.name, len(fillers), used, maxConn)
					}
				}
				cctx, cancel := context.WithCancel(ctx)
				done := make(chan error, 1)
				go func() { done <- o.run(cctx, p, slug, id, tenant) }()
				// Cancel once the operation waits for the held row.
				deadline := time.Now().Add(10 * time.Second)
				for {
					var waiting int
					if err := f.pool.QueryRow(ctx, `SELECT count(*)::int FROM pg_locks WHERE NOT granted AND pid <> $1`, int(holderPID)).Scan(&waiting); err != nil {
						t.Fatal(err)
					}
					if waiting > 0 || time.Now().After(deadline) {
						break
					}
					time.Sleep(5 * time.Millisecond)
				}
				start := time.Now()
				cancel()
				opErr := <-done
				took := time.Since(start)
				maxReturn = max(maxReturn, took)
				n0 := othersLocks(t, f, holderPID)
				// How long the backend keeps them while the holder still holds
				// its row (5 s at most).
				gone := time.Now()
				for othersLocks(t, f, holderPID) > 0 && time.Since(gone) < 5*time.Second {
					time.Sleep(2 * time.Millisecond)
				}
				atReturn = append(atReturn, fmt.Sprint(n0))
				after1s = append(after1s, time.Since(gone).Round(time.Millisecond).String())
				if r == 0 {
					var pg *pgconn.PgError
					kind := "other"
					switch {
					case errors.Is(opErr, context.Canceled):
						kind = "context.Canceled"
					case errors.As(opErr, &pg):
						kind = "PgError " + pg.Code
					}
					returned = append(returned, kind)
				}
				for _, c := range fillers {
					_ = c.Close(ctx)
				}
				_ = holder.Rollback(ctx)
				time.Sleep(50 * time.Millisecond)
			}
			t.Logf("%-8s %-12s lock entries of other backends at the return: [%s]; gone after: [%s] (the holder still holds the row); slowest return after the cancel %s; error %v",
				variant, o.name, strings.Join(atReturn, " "), strings.Join(after1s, " "), maxReturn.Round(time.Millisecond), returned)
		}
	}
}
