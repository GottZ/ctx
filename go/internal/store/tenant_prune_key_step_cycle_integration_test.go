//go:build integration

// Issue #555, point 1 (found by the review of the wave): the key step of a
// tenant-delete against two writers of two keys of the tenant, in one fixed
// interleaving. A block Y of the tenant's scope; writer "late" (a single
// update of Y, UpdateBlockBooked: its key's gate first, then the row Y, its
// key's row last through the booking's foreign key) stands before its
// booking, holding Y; writer "first" (a single store of Y's identity,
// UpsertBlock) holds its key's gate and stands at its next statement. Then:
// the delete's key step asks for both gates and waits, the first writer goes
// on and waits for Y, the late writer goes on and books. In the first form of
// #555 the update took its key's share last, after Y: the key step (holding
// the late key's row, deleted) waited for the first writer's key, the first
// writer for Y, and the late writer for its key — 40P01, and the delete
// stopped part-way. Every request write takes its key's gate first now, and
// the key step takes the gates before any row, so it waits for both writers
// and nothing waits for it.
package store_test

import (
	"context"
	"errors"
	"strings"
	"sync"
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
			cLate, cFirst := requestCtx(ctx, late), requestCtx(ctx, first)
			f.block(t, scopes[0], "kscycle seed")

			hl := newPruneHold(func(sql string) bool { return strings.HasPrefix(sql, "INSERT INTO context_access_log") })
			// The first writer's gate is a batch (not traced as a query): the
			// first statement after its BEGIN is the one after the gate.
			hf := newPruneHold(func(sql string) bool { return !strings.HasPrefix(strings.ToLower(sql), "begin") })
			pl, pf := pruneHoldPool(t, f.dsn, hl), pruneHoldPool(t, f.dsn, hf)
			var rel sync.Map
			release := func(h *pruneHold) {
				if _, done := rel.LoadOrStore(h, true); !done {
					close(h.release)
				}
			}
			t.Cleanup(func() { release(hl); release(hf) }) // runs before the pools close

			// 1. Y, written by a request of the late key.
			const title = "kscycle y"
			y, err := store.UpsertBlock(cLate, f.pool, "learnings", title, "y before", nil, nil, scopes[0], true, store.SensitivityWrite{}, "")
			if err != nil {
				t.Fatalf("Y: %v", err)
			}

			// 2. The late writer holds its gate and Y and stands before its booking.
			lateDone := make(chan error, 1)
			go func() {
				content := "y updated"
				_, _, err := store.UpdateBlockBooked(cLate, pl, y.ID, store.UpdateBlockData{Content: &content}, scopes, &store.WriteBudget{APIKeyID: late.ApiKeyID})
				lateDone <- err
			}()
			waitPruneHeld(t, hl, lateDone)

			// 3. The first writer holds its gate and stands at its next statement.
			firstDone := make(chan error, 1)
			go func() {
				_, err := store.UpsertBlock(cFirst, pf, "learnings", title, "y stored", nil, nil, scopes[0], true, store.SensitivityWrite{}, "")
				firstDone <- err
			}()
			select {
			case <-hf.held:
			case err := <-firstDone:
				t.Fatalf("the first writer ended (%v) before it was held", err)
			case <-time.After(10 * time.Second):
				t.Fatalf("the first writer did not reach the statement after its gate")
			}

			// 4. The delete's key step asks for both gates and waits.
			pruneDone := make(chan error, 1)
			go func() { pruneDone <- store.PruneTenant(ctx, f.pool, tenantID, store.AuditStamp{}) }()
			if !waitLockWaiters(f.pool, 1) {
				t.Fatalf("the key step did not wait")
			}
			// 5. The first writer goes on and waits for Y.
			release(hf)
			if !waitLockWaiters(f.pool, 2) {
				t.Logf("the first writer did not wait for Y (it may have gone through)")
			}
			// 6. The late writer books.
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
			t.Logf("%s: after %s — tenant-delete: %v (40P01 %v); late writer (booked update): %v (40P01 %v); first writer (store): %v (40P01 %v)",
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
