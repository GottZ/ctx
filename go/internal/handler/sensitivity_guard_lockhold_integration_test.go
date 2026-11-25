//go:build integration

// Issue #547, review round 1 (F6): the sensitivity downgrade guard decides on
// the row its write holds — the lock is what TestSensitivityGuardDecides-
// UnderTheRowLock did not pin: it holds A before the store's read, so a read
// without the lock passed it as well. Here a raise (B) starts right after
// A's locking read has returned, while A holds the row: B must wait for A's
// commit and raise the block after it, on the single update and on a batch
// update item, with and without the confirm. Without the row lock B commits
// first and A's write lowers the block with no or a wrong audit entry (the
// review's probe: 10 of 10 rounds).
package handler

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/store"
)

// sgEndHook runs hook once at the end of the first statement match accepts.
type sgEndHook struct {
	match func(sql string) bool
	hook  atomic.Pointer[func()]
}

type sgSQLKey struct{}

func (h *sgEndHook) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, sgSQLKey{}, d.SQL)
}

func (h *sgEndHook) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	sql, _ := ctx.Value(sgSQLKey{}).(string)
	if h.match(sql) {
		if fn := h.hook.Swap(nil); fn != nil {
			(*fn)()
		}
	}
}

func TestSensitivityGuardHoldsTheRowItDecidesOn(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newSBFixture(t)
	k := f.key(t, false)
	tr := &sgEndHook{match: func(sql string) bool {
		// The store's locking read (single update: lockSensitivitySQL; batch:
		// the lock pass of an item with a sensitivity), not the handler's.
		return strings.Contains(sql, "sensitivity FROM context_blocks") && strings.Contains(sql, "metadata ?")
	}}
	poolA := wcPool(t, f.pool, tr)
	cfgB, err := pgxpool.ParseConfig(f.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	poolB, err := pgxpool.NewWithConfig(context.Background(), cfgB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(poolB.Close)
	h := NewManageHandler(poolA, f.cfg, nil, backends.NewPool(nil, nil), nil, nil, nil, f.reg)
	for _, surface := range []string{"manage update", "batch item"} {
		for _, confirm := range []bool{false, true} {
			name := surface + map[bool]string{false: " without confirm", true: " with confirm"}[confirm]
			t.Run(name, func(t *testing.T) {
				id := f.sgSeed(t, "sg-hold "+name, backends.SensInternal)
				data := map[string]any{"sensitivity": "internal"}
				if confirm {
					data = map[string]any{"sensitivity": "public", "confirm_sensitivity_downgrade": true}
				}
				raised := make(chan error, 1)
				var waited atomic.Bool
				fn := func() {
					go func() {
						up := "credentials"
						_, _, err := store.UpdateBlock(context.Background(), poolB, id, store.UpdateBlockData{Sensitivity: &up}, []string{"private"})
						raised <- err
					}()
					select {
					case err := <-raised:
						raised <- err // B went through beside A's held row
					case <-time.After(300 * time.Millisecond):
						waited.Store(true) // B waits for A's row lock
					}
				}
				tr.hook.Store(&fn)
				var status int
				if surface == "manage update" {
					status, _ = wcManage(t, h, k, map[string]any{"action": "update", "id": id, "data": data})
				} else {
					item := map[string]any{"id": id}
					for kk, v := range data {
						item[kk] = v
					}
					raw := sbItems(item)
					status, _ = func() (int, map[string]any) {
						return f.restOn(t, poolA, k, raw)
					}()
				}
				if tr.hook.Load() != nil {
					t.Fatalf("the store's locking read was not reached")
				}
				if err := <-raised; err != nil {
					t.Fatalf("raise: %v", err)
				}
				sens, audit := f.sgState(t, id)
				if !waited.Load() || status != http.StatusOK || sens != "credentials" {
					t.Errorf("B waited for A's row: %v; A answered %d; the block ends at %s (audit %s) — want B to wait and raise after A",
						waited.Load(), status, sens, audit)
				}
			})
		}
	}
}

// restOn posts one /api/store/batch body through a handler on pool.
func (f *sbFixture) restOn(t *testing.T, pool *pgxpool.Pool, k sbKey, body any) (int, map[string]any) {
	t.Helper()
	g := *f
	g.pool = pool
	return g.rest(t, k, body)
}
