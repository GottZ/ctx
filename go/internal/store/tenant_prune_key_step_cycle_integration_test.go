//go:build integration

// Issue #555, point 1 (found by the review of the wave): the key step of a
// tenant-delete against two writers of two keys of the tenant, in one fixed
// interleaving. The delete has passed the tenant's scope and stands before
// its key step; a block Y was written into the scope after the pass (a
// request in flight). Writer "late" (a single update of Y, UpdateBlockBooked:
// the row first, its key's share last, through the booking's foreign key)
// stands before its booking, holding Y. Writer "first" (a single store of
// Y's identity, UpsertBlock: its key's share first) holds its key and stands
// at its next statement. Then: the key step runs and waits, the first writer
// goes on and waits for Y, the late writer goes on and books. With the
// update's key share taken last, through its booking's foreign key, the key
// step (holding the late key's row, deleted) waited for the first writer's
// key, the first writer for Y, and the late writer for its key: 40P01, and
// the delete stopped part-way. The booked update holds its key first now.
package store_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/store"
)

func keyStepIs40P01(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "40P01"
}

// waitLockWaiters waits until at least n backends of the test database wait
// for a lock; false after 10 s.
func waitLockWaiters(pool *pgxpool.Pool, n int) bool {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var got int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*)::int FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&got); err == nil && got >= n {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

func TestTenantDeleteKeyStepBesideABookedUpdate(t *testing.T) {
	f := newP547Fixture(t)
	ctx := context.Background()
	for _, lateIsFirstKey := range []bool{true, false} {
		name := "late writer = first key"
		slug := "kscycle1"
		if !lateIsFirstKey {
			name, slug = "late writer = second key", "kscycle2"
		}
		t.Run(name, func(t *testing.T) {
			tenantID, scopes, key1 := pruneWritersTenant(t, f, slug, 1)
			_, key2, err := store.CreateApiKey(ctx, f.pool, "kscycle second", scopes[0], scopes, tenantID)
			if err != nil {
				t.Fatal(err)
			}
			ar1, _ := auth.Authenticate(ctx, f.pool, key1)
			ar2, _ := auth.Authenticate(ctx, f.pool, key2)
			late, first := ar1, ar2
			if !lateIsFirstKey {
				late, first = ar2, ar1
			}
			cLate, cFirst := store.WithRequestKey(ctx, late.ApiKeyID), store.WithRequestKey(ctx, first.ApiKeyID)
			f.block(t, scopes[0], "kscycle seed")

			hp := newPruneHold(func(sql string) bool { return strings.HasPrefix(sql, "DELETE FROM context_api_keys") })
			hl := newPruneHold(func(sql string) bool { return strings.HasPrefix(sql, "INSERT INTO context_access_log") })
			var sawHold atomic.Bool
			hf := newPruneHold(func(sql string) bool {
				if strings.Contains(sql, "FROM context_api_keys WHERE id = $1::uuid FOR KEY SHARE") {
					sawHold.Store(true)
					return false
				}
				return sawHold.Load()
			})
			pp, pl, pf := pruneHoldPool(t, f.dsn, hp), pruneHoldPool(t, f.dsn, hl), pruneHoldPool(t, f.dsn, hf)
			var rel sync.Map
			release := func(h *pruneHold) {
				if _, done := rel.LoadOrStore(h, true); !done {
					close(h.release)
				}
			}
			t.Cleanup(func() { release(hp); release(hl); release(hf) }) // runs before the pools close

			// 1. The delete stands before its key step.
			pruneDone := make(chan error, 1)
			go func() { pruneDone <- store.PruneTenant(ctx, pp, tenantID, store.AuditStamp{}) }()
			waitPruneHeld(t, hp, pruneDone)

			// 2. Y, written after the pass by a request of the late key.
			const title = "kscycle y"
			y, err := store.UpsertBlock(cLate, f.pool, "learnings", title, "y before", nil, nil, scopes[0], true, store.SensitivityWrite{}, "")
			if err != nil {
				t.Fatalf("Y: %v", err)
			}

			// 3. The late writer holds Y and stands before its booking.
			lateDone := make(chan error, 1)
			go func() {
				content := "y updated"
				_, _, err := store.UpdateBlockBooked(cLate, pl, y.ID, store.UpdateBlockData{Content: &content}, scopes, &store.WriteBudget{APIKeyID: late.ApiKeyID})
				lateDone <- err
			}()
			waitPruneHeld(t, hl, lateDone)

			// 4. The first writer holds its key and stands at its next statement.
			firstDone := make(chan error, 1)
			go func() {
				_, err := store.UpsertBlock(cFirst, pf, "learnings", title, "y stored", nil, nil, scopes[0], true, store.SensitivityWrite{}, "")
				firstDone <- err
			}()
			select {
			case <-hf.held:
			case err := <-firstDone:
				t.Fatalf("the first writer ended (%v) before it was held: it holds no key share first", err)
			case <-time.After(10 * time.Second):
				t.Fatalf("the first writer did not reach the statement after its key share")
			}

			// 5. The key step runs and waits.
			release(hp)
			if !waitLockWaiters(f.pool, 1) {
				t.Fatalf("the key step did not wait")
			}
			// 6. The first writer goes on and waits for Y.
			release(hf)
			if !waitLockWaiters(f.pool, 2) {
				t.Logf("the first writer did not wait for Y (it may have gone through)")
			}
			// 7. The late writer books.
			began := time.Now()
			release(hl)
			var perr, lerr, ferr error
			for i, ch := range []chan error{pruneDone, lateDone, firstDone} {
				select {
				case err := <-ch:
					switch i {
					case 0:
						perr = err
					case 1:
						lerr = err
					default:
						ferr = err
					}
				case <-time.After(30 * time.Second):
					t.Fatalf("party %d did not end within 30 s", i)
				}
			}
			took := time.Since(began)
			t.Logf("%s: after %s — tenant-delete: %v (40P01 %v); late writer (update, key share last): %v (40P01 %v); first writer (store, key share first): %v (40P01 %v)",
				name, took.Round(time.Millisecond), perr, keyStepIs40P01(perr), lerr, keyStepIs40P01(lerr), ferr, keyStepIs40P01(ferr))
			if perr != nil {
				if err := store.PruneTenant(ctx, f.pool, tenantID, store.AuditStamp{}); err != nil {
					t.Logf("re-run of the delete: %v", err)
				}
			}
			blocks, links, other, tenant := pruneLeftovers(t, f.pool, tenantID, scopes)
			t.Logf("%s: left after the delete(s): %d blocks, %d links, %d other, tenant row %d", name, blocks, links, other, tenant)
			for party, err := range map[string]error{"tenant-delete": perr, "booked update": lerr, "single store": ferr} {
				if keyStepIs40P01(err) {
					t.Errorf("%s: %s ended in a lock cycle: %v", name, party, err)
				}
			}
			if perr != nil {
				t.Errorf("%s: the delete beside the two writers: %v", name, perr)
			}
			if blocks+links+other+tenant != 0 {
				t.Errorf("%s: rows left after the delete", name)
			}
		})
	}
}
