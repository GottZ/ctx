//go:build integration

// Gates of the W01-3 hardening after the second external review
// (reports/review/W01-3-fremdreview-2.md):
//
//   - TestUsageFlushNeverWaits (F-1): the usage flush never waits on a row
//     lock, so it cannot close a lock cycle with the consumer writers (prune
//     step 7b, rotation with grace 0, the deactivation trigger), which lock a
//     consumer's keys in heap/index order. Red against a flush that updates
//     the keys in array order: 40P01 (deadlock), whichever side loses.
//   - TestModelKeyExpiryInPast (F-4): a mint or rotation with an expiry that
//     is already over is refused — it would mint a dead key (and a rotation
//     would fork the chain).
//   - TestKeyCreatedAtIsInsertTime, TestListAndIDsCoverInactive (F-5): pins
//     for clock_timestamp() as a key's created_at, the active filter of the
//     list, and inactive consumers in TenantConsumerIDs.
//
// Run with:
//
//	go test -tags=integration -p 1 -count=1 -run 'TestUsageFlushNeverWaits|TestModelKeyExpiryInPast|TestKeyCreatedAtIsInsertTime|TestListAndIDsCoverInactive' ./internal/store/ -v
package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/modelkey"
	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// mkLockWaiterQueries lists the statements of this test database's backends
// that wait on a heavyweight lock.
func mkLockWaiterQueries(ctx context.Context, t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT query FROM pg_stat_activity
		WHERE datname = current_database() AND wait_event_type = 'Lock' AND pid <> pg_backend_pid()`)
	if err != nil {
		t.Fatalf("pg_stat_activity: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var q string
		if err := rows.Scan(&q); err != nil {
			t.Fatalf("scan pg_stat_activity: %v", err)
		}
		out = append(out, strings.Join(strings.Fields(q), " "))
	}
	return out
}

func TestUsageFlushNeverWaits(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	e := mkSetup(t)
	srv := store.ModelAccess{ServerAdmin: true}
	no := false
	for _, w := range []struct {
		name       string
		other      func(ctx context.Context, tenant, consumer string) error
		keysRemain bool
	}{
		{"prune", func(ctx context.Context, tenant, _ string) error {
			return store.PruneTenant(ctx, e.pool, tenant, e.stamp)
		}, false},
		{"rotate_grace0", func(ctx context.Context, _, consumer string) error {
			_, _, err := store.RotateKey(ctx, e.pool, consumer, srv, 0, nil, e.stamp)
			return err
		}, true},
		{"deactivate", func(ctx context.Context, _, consumer string) error {
			_, err := store.UpdateConsumer(ctx, e.pool, consumer, srv, store.ModelConsumerPatch{Active: &no}, e.stamp)
			return err
		}, true},
	} {
		t.Run(w.name, func(t *testing.T) {
			tenant := mkTenant(ctx, t, e.pool, "mk-fw-"+strings.ReplaceAll(w.name, "_", "-"), nil)
			c, k1, _ := mkCreate(ctx, t, e, tenant, "fw")
			k2, _ := mkRotate(ctx, t, e, c.ID, time.Hour)
			// A non-HOT update of k1 (created_at is indexed) moves its tuple and
			// its index entries behind k2's: the writers now meet the keys as
			// k2, k1 (heap order, index tie-break by TID), while their ids —
			// the order of a flush over the sorted id array — stay k1, k2.
			if _, err := e.pool.Exec(ctx, `UPDATE context_model_keys SET created_at = created_at - interval '1 microsecond' WHERE id = $1::uuid`, k1.ID); err != nil {
				t.Fatalf("move k1: %v", err)
			}
			hold, err := e.pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer func() { _ = hold.Rollback(ctx) }()
			if _, err := hold.Exec(ctx, `SELECT 1 FROM context_model_keys WHERE id = ANY($1::uuid[]) FOR UPDATE`, []string{k1.ID, k2.ID}); err != nil {
				t.Fatalf("hold both keys: %v", err)
			}
			stamp := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
			u := store.NewModelKeyUsage(e.pool)
			store.SetModelKeyUsageClockForTest(u, func() time.Time { return stamp })
			u.Touch(k1.ID)
			u.Touch(k2.ID)

			opCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			flushed := make(chan error, 1)
			go func() { flushed <- u.Flush(opCtx) }()
			var flushErr error
			flushDone, flushWaited := false, false
			for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline) && !flushDone && !flushWaited; {
				select {
				case flushErr = <-flushed:
					flushDone = true
				default:
					flushWaited = len(mkLockWaiterQueries(ctx, t, e.pool)) > 0
					time.Sleep(5 * time.Millisecond)
				}
			}
			if !flushDone && !flushWaited {
				t.Fatalf("flush neither returned nor waited within 3 s")
			}
			if flushWaited {
				t.Errorf("the flush waits on a row lock another transaction holds: %q", mkLockWaiterQueries(ctx, t, e.pool))
			}
			others := make(chan error, 1)
			go func() { others <- w.other(opCtx, tenant, c.ID) }()
			want := 1
			if flushWaited {
				want = 2
			}
			mkAwaitLockWaiters(ctx, t, e.pool, want)
			if err := hold.Commit(ctx); err != nil {
				t.Fatalf("release: %v", err)
			}
			if !flushDone {
				flushErr = <-flushed
			}
			otherErr := <-others
			t.Logf("measured: flush waited=%v flush=%v (SQLSTATE %q) %s=%v (SQLSTATE %q)",
				flushWaited, flushErr, mcSQLState(flushErr), w.name, otherErr, mcSQLState(otherErr))
			if flushErr != nil || otherErr != nil {
				t.Errorf("flush ‖ %s: flush %v, %s %v — want both to succeed", w.name, flushErr, w.name, otherErr)
			}
			if !w.keysRemain {
				return
			}
			// The flush skipped both locked keys and kept their stamps.
			if err := u.Flush(ctx); err != nil {
				t.Fatalf("second Flush: %v", err)
			}
			for _, id := range []string{k1.ID, k2.ID} {
				var got *time.Time
				if err := e.pool.QueryRow(ctx, `SELECT last_used_at FROM context_model_keys WHERE id = $1::uuid`, id).Scan(&got); err != nil {
					t.Fatalf("read last_used_at: %v", err)
				}
				if got == nil || !got.Equal(stamp) {
					t.Errorf("key %s last_used_at %v after the second flush, want the kept stamp %v", id, got, stamp)
				}
			}
		})
	}
}

func TestModelKeyExpiryInPast(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	e := mkSetup(t)
	tenant := mkTenant(ctx, t, e.pool, "mk-expiry", nil)
	asT := store.ModelAccess{TenantID: tenant}
	past := time.Now().Add(-time.Hour)

	t.Run("create_refuses_a_past_expiry", func(t *testing.T) {
		_, _, _, err := store.CreateConsumerWithKey(ctx, e.pool, store.ModelConsumerSpec{TenantID: tenant, Label: "past"}, &past, e.stamp)
		if err == nil {
			t.Errorf("CreateConsumerWithKey with expires_at an hour ago succeeded, want a refusal")
		}
		if n := mkCount(ctx, t, e.pool, `SELECT count(*) FROM context_model_consumers WHERE tenant_id = $1::uuid AND label = 'past'`, tenant); n != 0 {
			t.Errorf("refused create left %d consumer(s)", n)
		}
		if err != nil && !errors.Is(err, store.ErrModelKeyExpiryInPast) {
			t.Errorf("refusal %v, want ErrModelKeyExpiryInPast", err)
		}
	})

	t.Run("rotate_refuses_a_past_expiry", func(t *testing.T) {
		c, k1, _ := mkCreate(ctx, t, e, tenant, "past-rotate")
		_, _, err := store.RotateKey(ctx, e.pool, c.ID, asT, 24*time.Hour, &past, e.stamp)
		if err == nil {
			t.Errorf("RotateKey with expires_at an hour ago succeeded, want a refusal")
		}
		if err != nil && !errors.Is(err, store.ErrModelKeyExpiryInPast) {
			t.Errorf("refusal %v, want ErrModelKeyExpiryInPast", err)
		}
		live := mkLiveIDs(ctx, t, e.pool, c.ID)
		if len(live) != 1 || live[0] != k1.ID || mkReadKey(ctx, t, e.pool, k1.ID).expires != nil {
			t.Errorf("refused rotation touched the chain: live %v, k1 expiry %v", live, mkReadKey(ctx, t, e.pool, k1.ID).expires)
		}
	})

	t.Run("a_second_ahead_is_allowed", func(t *testing.T) {
		soon := time.Now().Add(time.Second)
		c, _, _, err := store.CreateConsumerWithKey(ctx, e.pool, store.ModelConsumerSpec{TenantID: tenant, Label: "soon"}, &soon, e.stamp)
		if err != nil {
			t.Fatalf("create with expires_at now+1s: %v", err)
		}
		soon = time.Now().Add(time.Second)
		if _, _, err := store.RotateKey(ctx, e.pool, c.ID, asT, time.Hour, &soon, e.stamp); err != nil {
			t.Errorf("rotate with expires_at now+1s: %v", err)
		}
	})
}

func TestKeyCreatedAtIsInsertTime(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	e := mkSetup(t)
	tenant := mkTenant(ctx, t, e.pool, "mk-clock", nil)

	t.Run("two_inserts_in_one_transaction", func(t *testing.T) {
		c, _, _ := mkCreate(ctx, t, e, tenant, "clock-tx")
		tx, err := e.pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		var at []time.Time
		for range 2 {
			key, err := modelkey.Generate()
			if err != nil {
				t.Fatalf("generate: %v", err)
			}
			k, err := store.InsertModelKeyForTest(ctx, tx, c.ID, key, nil, nil)
			if err != nil {
				t.Fatalf("insert: %v", err)
			}
			at = append(at, k.CreatedAt)
		}
		if !at[1].After(at[0]) {
			t.Errorf("two keys inserted in one transaction: created_at %v then %v, want the second later (insert time, not transaction start)", at[0], at[1])
		}
	})

	t.Run("rotation_behind_a_competing_key", func(t *testing.T) {
		c, _, _ := mkCreate(ctx, t, e, tenant, "clock-rotate")
		hold, err := e.pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = hold.Rollback(ctx) }()
		if _, err := hold.Exec(ctx, `SELECT 1 FROM context_model_consumers WHERE id = $1::uuid FOR UPDATE`, c.ID); err != nil {
			t.Fatalf("hold the consumer: %v", err)
		}
		type rotated struct {
			k   store.ModelKeyInfo
			err error
		}
		done := make(chan rotated, 1)
		go func() {
			k, _, err := store.RotateKey(ctx, e.pool, c.ID, store.ModelAccess{ServerAdmin: true}, time.Hour, nil, e.stamp)
			done <- rotated{k, err}
		}()
		mkAwaitLockWaiters(ctx, t, e.pool, 1) // the rotation's transaction has begun
		competing, _ := mcKey(ctx, t, hold, c.ID, "mk-clock-competing", map[string]string{"created_at": "clock_timestamp()"})
		if err := hold.Commit(ctx); err != nil {
			t.Fatalf("commit the competing key: %v", err)
		}
		r := <-done
		if r.err != nil {
			t.Fatalf("RotateKey: %v", r.err)
		}
		if mcStr(r.k.RotatedFrom) != competing {
			t.Errorf("rotation chained from %s, want the competing key %s", mcStr(r.k.RotatedFrom), competing)
		}
		next, _ := mkRotate(ctx, t, e, c.ID, time.Hour)
		if mcStr(next.RotatedFrom) != r.k.ID {
			t.Errorf("next rotation chained from %s, want the previous rotation's key %s (its created_at sorted it before the competing key)", mcStr(next.RotatedFrom), r.k.ID)
		}
	})
}

func TestListAndIDsCoverInactive(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	e := mkSetup(t)
	tenant := mkTenant(ctx, t, e.pool, "mk-inactive", nil)
	asT := store.ModelAccess{TenantID: tenant}
	live, _, _ := mkCreate(ctx, t, e, tenant, "live")
	gone, _, _ := mkCreate(ctx, t, e, tenant, "gone")
	if err := store.DeleteConsumer(ctx, e.pool, gone.ID, asT, e.stamp); err != nil {
		t.Fatalf("DeleteConsumer: %v", err)
	}

	list, err := store.ListConsumers(ctx, e.pool, asT, store.ListConsumersOptions{})
	if err != nil {
		t.Fatalf("ListConsumers: %v", err)
	}
	if len(list) != 1 || list[0].ID != live.ID {
		t.Errorf("default list %d consumer(s), want only the active one %s", len(list), live.ID)
	}
	all, err := store.ListConsumers(ctx, e.pool, asT, store.ListConsumersOptions{IncludeInactive: true})
	if err != nil {
		t.Fatalf("ListConsumers(IncludeInactive): %v", err)
	}
	seen := map[string]bool{}
	for _, c := range all {
		seen[c.ID] = c.Active
	}
	if len(all) != 2 || !seen[live.ID] || seen[gone.ID] {
		t.Errorf("IncludeInactive list %v, want %s active and %s inactive", seen, live.ID, gone.ID)
	}
	ids, err := store.TenantConsumerIDs(ctx, e.pool, tenant)
	if err != nil {
		t.Fatalf("TenantConsumerIDs: %v", err)
	}
	if strings.Join(ids, ",") != live.ID+","+gone.ID && strings.Join(ids, ",") != gone.ID+","+live.ID {
		t.Errorf("TenantConsumerIDs %v, want the active and the inactive consumer", ids)
	}
}
