//go:build integration

// Gates of W01-3 (a): the store primitives of model consumers and model keys
// (design/01 §3.2 I-K2/I-K6/I-K8, §4.2 usage stamp, §4.6 lifecycle; design/05
// §3.2 list form, §3.4 V-01-d/V-01-g).
//
//   - TestConsumerCapRace: two creates at cap − 1 ⇒ exactly one succeeds; red
//     against a tenant lock without FOR UPDATE. Reactivation counts against
//     the cap and waits for the same tenant lock.
//   - TestRotateKeepsAtMostTwoLive: three rotations ⇒ two live keys and the
//     rotated_from chain; a predecessor's earlier expiry survives a longer
//     grace (V-01-d (a), red against a rotation without LEAST); grace 0
//     revokes at once.
//   - TestRotateLocksTheConsumer: the rotation waits for a running
//     deactivation AT the consumer lock (before it touches a key) and then
//     refuses; the migration-161 insert guard maps to ErrModelConsumerInactive.
//   - TestModelKeyTenantJoin (V-01-g): another tenant's key or consumer reads
//     byte-identically to an unknown id and stays untouched — red against a
//     revoke without the join.
//   - TestListSingleQuery: 50 consumers (à 3 grants since W01-9) ⇒ one statement (QueryTracer) — red
//     against an N+1 list.
//   - TestListNeverCarriesSecret: no 64-hex value, no ctxm_ plaintext in the
//     list, the history or any audit row after a create and two rotations.
//   - TestUsageFlushNoLockWait: 200 parallel touches across a minute boundary
//     ⇒ one UPDATE, no lock wait, no audit row — red against an UPDATE per
//     touch; monotonic, retried after a failure, flushed on shutdown; the
//     final flush returns at its window when no attempt can succeed (#124).
//   - TestModelKeyWritersStamp: every writer's audit rows carry via='api', the
//     actor, the request id and metadata.tenant_id — red with stampAudit as a
//     no-op.
//
// Run with:
//
//	go test -tags=integration -p 1 -count=1 -run 'Consumer|Rotate|ModelKey|ListSingle|ListNever|UsageFlush' ./internal/store/ -v
package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/modelkey"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// mkEnv is one test database with an acting server-admin key of the default
// tenant and its stamp.
type mkEnv struct {
	pool  *pgxpool.Pool
	admin store.ApiKey
	stamp store.AuditStamp
}

func mkSetup(t *testing.T) mkEnv {
	t.Helper()
	pool := testdb.SetupTestDB(t)
	admin, _, err := store.CreateApiKey(context.Background(), pool, "mk-admin", "private", nil, "")
	if err != nil {
		t.Fatalf("mint the acting admin key: %v", err)
	}
	return mkEnv{pool: pool, admin: *admin, stamp: store.APIStamp(admin.ID, "mk-req")}
}

// mkTenant creates a tenant with scope <slug>:main and the given consumer cap
// (nil keeps the default 64).
func mkTenant(ctx context.Context, t *testing.T, pool *pgxpool.Pool, slug string, limit *int) string {
	t.Helper()
	id, _ := mcTenant(ctx, t, pool, slug, "active")
	if limit != nil {
		if _, err := pool.Exec(ctx, `UPDATE context_tenants SET max_model_consumers = $2 WHERE id = $1::uuid`, id, *limit); err != nil {
			t.Fatalf("set cap: %v", err)
		}
	}
	return id
}

func mkIntp(n int) *int { return &n }

func mkCreate(ctx context.Context, t *testing.T, e mkEnv, tenant, label string) (store.ModelConsumer, store.ModelKeyInfo, modelkey.Key) {
	t.Helper()
	c, k, key, err := store.CreateConsumerWithKey(ctx, e.pool, store.ModelConsumerSpec{TenantID: tenant, Label: label}, nil, e.stamp)
	if err != nil {
		t.Fatalf("CreateConsumerWithKey %s: %v", label, err)
	}
	return c, k, key
}

func mkRotate(ctx context.Context, t *testing.T, e mkEnv, consumer string, grace time.Duration) (store.ModelKeyInfo, modelkey.Key) {
	t.Helper()
	k, key, err := store.RotateKey(ctx, e.pool, consumer, store.ModelAccess{ServerAdmin: true}, grace, nil, e.stamp)
	if err != nil {
		t.Fatalf("RotateKey %s: %v", consumer, err)
	}
	return k, key
}

// mkLiveIDs lists the live keys of a consumer, oldest first.
func mkLiveIDs(ctx context.Context, t *testing.T, pool *pgxpool.Pool, consumer string) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT id::text FROM context_model_keys
		WHERE consumer_id = $1::uuid AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now())
		ORDER BY created_at, id`, consumer)
	if err != nil {
		t.Fatalf("live keys: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan live key: %v", err)
		}
		out = append(out, id)
	}
	return out
}

// mkAwaitLockWaiters waits until n backends of this test database wait on a
// heavyweight lock and returns their statements.
func mkAwaitLockWaiters(ctx context.Context, t *testing.T, pool *pgxpool.Pool, n int) []string {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var seen []string
	for time.Now().Before(deadline) {
		rows, err := pool.Query(ctx, `SELECT query FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock' AND pid <> pg_backend_pid()`)
		if err != nil {
			t.Fatalf("pg_stat_activity: %v", err)
		}
		seen = seen[:0]
		for rows.Next() {
			var q string
			if err := rows.Scan(&q); err != nil {
				t.Fatalf("scan pg_stat_activity: %v", err)
			}
			seen = append(seen, strings.Join(strings.Fields(q), " "))
		}
		rows.Close()
		if len(seen) >= n {
			return seen
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("waited 15 s for %d lock waiter(s), saw %d: %q", n, len(seen), seen)
	return nil
}

// mkKeyRow reads the lifecycle and actor columns of one key.
type mkKeyRow struct {
	expires, revoked                             *time.Time
	rotatedFrom                                  *string
	createdBy, createdByP, revokedBy, revokedByP *string
}

func mkReadKey(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id string) mkKeyRow {
	t.Helper()
	var r mkKeyRow
	if err := pool.QueryRow(ctx, `SELECT expires_at, revoked_at, rotated_from::text,
		       created_by_key::text, created_by_principal::text, revoked_by_key::text, revoked_by_principal::text
		  FROM context_model_keys WHERE id = $1::uuid`, id).Scan(&r.expires, &r.revoked, &r.rotatedFrom,
		&r.createdBy, &r.createdByP, &r.revokedBy, &r.revokedByP); err != nil {
		t.Fatalf("read key %s: %v", id, err)
	}
	return r
}

func TestConsumerCapRace(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	e := mkSetup(t)

	t.Run("two_creates_at_cap_minus_one", func(t *testing.T) {
		tenant := mkTenant(ctx, t, e.pool, "mk-cap-race", mkIntp(2))
		mkCreate(ctx, t, e, tenant, "first")
		// Hold the tenant row: with the FOR UPDATE lock both creates queue in
		// front of their count; without it both count 1 < 2 and then queue at
		// the consumer INSERT (its foreign key takes FOR KEY SHARE on the row).
		hold, err := e.pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = hold.Rollback(ctx) }()
		if _, err := hold.Exec(ctx, `SELECT id FROM context_tenants WHERE id = $1::uuid FOR UPDATE`, tenant); err != nil {
			t.Fatalf("hold the tenant row: %v", err)
		}
		results := make(chan error, 2)
		for _, label := range []string{"racer-a", "racer-b"} {
			go func() {
				_, _, _, err := store.CreateConsumerWithKey(ctx, e.pool, store.ModelConsumerSpec{TenantID: tenant, Label: label}, nil, e.stamp)
				results <- err
			}()
		}
		t.Logf("waiting: %q", mkAwaitLockWaiters(ctx, t, e.pool, 2))
		if err := hold.Commit(ctx); err != nil {
			t.Fatalf("release: %v", err)
		}
		var ok, capped int
		for range 2 {
			switch err := <-results; {
			case err == nil:
				ok++
			case errors.Is(err, store.ErrModelConsumerCapExceeded):
				capped++
			default:
				t.Errorf("racer: unexpected error %v", err)
			}
		}
		active := mkCount(ctx, t, e.pool, `SELECT count(*) FROM context_model_consumers WHERE tenant_id = $1::uuid AND active`, tenant)
		if ok != 1 || capped != 1 || active != 2 {
			t.Errorf("two creates at cap − 1: %d succeeded, %d capped, %d active — want 1, 1, 2", ok, capped, active)
		}
	})

	t.Run("inactive_holds_no_slot_and_reactivation_is_capped", func(t *testing.T) {
		tenant := mkTenant(ctx, t, e.pool, "mk-cap-react", mkIntp(1))
		a, _, _ := mkCreate(ctx, t, e, tenant, "a")
		if _, _, _, err := store.CreateConsumerWithKey(ctx, e.pool, store.ModelConsumerSpec{TenantID: tenant, Label: "b"}, nil, e.stamp); !errors.Is(err, store.ErrModelConsumerCapExceeded) {
			t.Fatalf("create at the cap: %v, want ErrModelConsumerCapExceeded", err)
		}
		if err := store.DeleteConsumer(ctx, e.pool, a.ID, store.ModelAccess{TenantID: tenant}, e.stamp); err != nil {
			t.Fatalf("DeleteConsumer: %v", err)
		}
		mkCreate(ctx, t, e, tenant, "b") // a's slot is free again
		yes := true
		_, err := store.UpdateConsumer(ctx, e.pool, a.ID, store.ModelAccess{TenantID: tenant}, store.ModelConsumerPatch{Active: &yes}, e.stamp)
		if !errors.Is(err, store.ErrModelConsumerCapExceeded) {
			t.Fatalf("reactivation at the cap: %v, want ErrModelConsumerCapExceeded", err)
		}
		if n := mkCount(ctx, t, e.pool, `SELECT count(*) FROM context_model_consumers WHERE id = $1::uuid AND active`, a.ID); n != 0 {
			t.Errorf("refused reactivation left the consumer active")
		}
	})

	t.Run("reactivation_waits_for_the_tenant_lock", func(t *testing.T) {
		tenant := mkTenant(ctx, t, e.pool, "mk-cap-wait", mkIntp(2))
		a, _, _ := mkCreate(ctx, t, e, tenant, "a")
		if err := store.DeleteConsumer(ctx, e.pool, a.ID, store.ModelAccess{TenantID: tenant}, e.stamp); err != nil {
			t.Fatalf("DeleteConsumer: %v", err)
		}
		hold, err := e.pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = hold.Rollback(ctx) }()
		if _, err := hold.Exec(ctx, `SELECT id FROM context_tenants WHERE id = $1::uuid FOR UPDATE`, tenant); err != nil {
			t.Fatalf("hold the tenant row: %v", err)
		}
		done := make(chan error, 1)
		go func() {
			yes := true
			_, err := store.UpdateConsumer(ctx, e.pool, a.ID, store.ModelAccess{TenantID: tenant}, store.ModelConsumerPatch{Active: &yes}, e.stamp)
			done <- err
		}()
		waiting := mkAwaitLockWaiters(ctx, t, e.pool, 1)
		if !strings.Contains(waiting[0], "FROM context_tenants") {
			t.Errorf("reactivation waits at %q, want the tenant lock", waiting[0])
		}
		if err := hold.Commit(ctx); err != nil {
			t.Fatalf("release: %v", err)
		}
		if err := <-done; err != nil {
			t.Fatalf("reactivation: %v", err)
		}
	})
}

func TestRotateKeepsAtMostTwoLive(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	e := mkSetup(t)
	tenant := mkTenant(ctx, t, e.pool, "mk-rotate", nil)

	t.Run("three_rotations", func(t *testing.T) {
		c, k1, _ := mkCreate(ctx, t, e, tenant, "three")
		k2, _ := mkRotate(ctx, t, e, c.ID, 24*time.Hour)
		k3, _ := mkRotate(ctx, t, e, c.ID, 24*time.Hour)
		k4, _ := mkRotate(ctx, t, e, c.ID, 24*time.Hour)
		if live := mkLiveIDs(ctx, t, e.pool, c.ID); strings.Join(live, ",") != k3.ID+","+k4.ID {
			t.Errorf("live after three rotations: %v, want [%s %s]", live, k3.ID, k4.ID)
		}
		chain := map[string]*string{k1.ID: nil, k2.ID: &k1.ID, k3.ID: &k2.ID, k4.ID: &k3.ID}
		for id, want := range chain {
			got := mkReadKey(ctx, t, e.pool, id).rotatedFrom
			if mcStr(got) != mcStr(want) {
				t.Errorf("key %s rotated_from %s, want %s", id, mcStr(got), mcStr(want))
			}
		}
		if mcStr(k4.RotatedFrom) != k3.ID {
			t.Errorf("returned key rotated_from %s, want %s", mcStr(k4.RotatedFrom), k3.ID)
		}
		list, err := store.ListConsumers(ctx, e.pool, store.ModelAccess{TenantID: tenant}, store.ListConsumersOptions{})
		if err != nil {
			t.Fatalf("ListConsumers: %v", err)
		}
		for _, lc := range list {
			if lc.ID != c.ID {
				continue
			}
			if len(lc.Keys) != 2 || lc.Keys[0].ID != k4.ID || lc.Keys[1].ID != k3.ID || lc.KeysTotal != 4 {
				t.Errorf("list: keys %+v total %d, want [k4 k3] of 4", lc.Keys, lc.KeysTotal)
			}
		}
	})

	t.Run("predecessor_expiry_is_never_extended", func(t *testing.T) {
		in1h := time.Now().Add(time.Hour)
		c, k1, _, err := store.CreateConsumerWithKey(ctx, e.pool, store.ModelConsumerSpec{TenantID: tenant, Label: "v01d"}, &in1h, e.stamp)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		before := mkReadKey(ctx, t, e.pool, k1.ID).expires
		mkRotate(ctx, t, e, c.ID, 24*time.Hour)
		after := mkReadKey(ctx, t, e.pool, k1.ID).expires
		if before == nil || after == nil || !after.Equal(*before) {
			t.Errorf("predecessor expires_at %v → %v under a 24 h grace, want unchanged", before, after)
		}
		mkRotate(ctx, t, e, c.ID, time.Minute)
		// The second rotation revokes k1: it is no longer the youngest.
		if r := mkReadKey(ctx, t, e.pool, k1.ID); r.revoked == nil {
			t.Errorf("k1 not revoked by the second rotation")
		}
	})

	t.Run("grace_zero_revokes_at_once", func(t *testing.T) {
		c, k1, _ := mkCreate(ctx, t, e, tenant, "leak")
		k2, _ := mkRotate(ctx, t, e, c.ID, 0)
		if r := mkReadKey(ctx, t, e.pool, k1.ID); r.revoked == nil {
			t.Errorf("grace 0: predecessor not revoked")
		}
		if live := mkLiveIDs(ctx, t, e.pool, c.ID); len(live) != 1 || live[0] != k2.ID {
			t.Errorf("grace 0: live %v, want only %s", live, k2.ID)
		}
		if mcStr(k2.RotatedFrom) != k1.ID {
			t.Errorf("grace 0: rotated_from %s, want %s", mcStr(k2.RotatedFrom), k1.ID)
		}
	})

	t.Run("no_live_key_starts_a_new_chain", func(t *testing.T) {
		c, k1, _ := mkCreate(ctx, t, e, tenant, "fresh")
		if err := store.RevokeKey(ctx, e.pool, k1.ID, store.ModelAccess{TenantID: tenant}, e.stamp); err != nil {
			t.Fatalf("RevokeKey: %v", err)
		}
		k2, _ := mkRotate(ctx, t, e, c.ID, 24*time.Hour)
		if k2.RotatedFrom != nil {
			t.Errorf("first key after a revoke: rotated_from %s, want NULL", *k2.RotatedFrom)
		}
	})

	t.Run("grace_bounds", func(t *testing.T) {
		c, _, _ := mkCreate(ctx, t, e, tenant, "bounds")
		for _, g := range []time.Duration{-time.Nanosecond, store.MaxRotationGrace + time.Microsecond} {
			if _, _, err := store.RotateKey(ctx, e.pool, c.ID, store.ModelAccess{TenantID: tenant}, g, nil, e.stamp); !errors.Is(err, store.ErrRotationGraceOutOfRange) {
				t.Errorf("grace %v: %v, want ErrRotationGraceOutOfRange", g, err)
			}
		}
		mkRotate(ctx, t, e, c.ID, store.MaxRotationGrace)
	})

	t.Run("inactive_consumer_is_refused", func(t *testing.T) {
		c, _, _ := mkCreate(ctx, t, e, tenant, "inactive")
		if err := store.DeleteConsumer(ctx, e.pool, c.ID, store.ModelAccess{TenantID: tenant}, e.stamp); err != nil {
			t.Fatalf("DeleteConsumer: %v", err)
		}
		if _, _, err := store.RotateKey(ctx, e.pool, c.ID, store.ModelAccess{TenantID: tenant}, time.Hour, nil, e.stamp); !errors.Is(err, store.ErrModelConsumerInactive) {
			t.Errorf("rotate an inactive consumer: %v, want ErrModelConsumerInactive", err)
		}
		if n := mkCount(ctx, t, e.pool, `SELECT count(*) FROM context_model_keys WHERE consumer_id = $1::uuid`, c.ID); n != 1 {
			t.Errorf("inactive consumer has %d key(s), want 1", n)
		}
	})
}

func TestRotateLocksTheConsumer(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	e := mkSetup(t)
	tenant := mkTenant(ctx, t, e.pool, "mk-lock", nil)

	t.Run("deactivation_first", func(t *testing.T) {
		c, k1, _ := mkCreate(ctx, t, e, tenant, "lock")
		hold, err := e.pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = hold.Rollback(ctx) }()
		if _, err := hold.Exec(ctx, `UPDATE context_model_consumers SET active = false, updated_at = now() WHERE id = $1::uuid`, c.ID); err != nil {
			t.Fatalf("deactivate: %v", err)
		}
		done := make(chan error, 1)
		go func() {
			_, _, err := store.RotateKey(ctx, e.pool, c.ID, store.ModelAccess{TenantID: tenant}, 24*time.Hour, nil, e.stamp)
			done <- err
		}()
		waiting := mkAwaitLockWaiters(ctx, t, e.pool, 1)
		if !strings.Contains(waiting[0], "FROM context_model_consumers c") || !strings.Contains(waiting[0], "FOR UPDATE") {
			t.Errorf("rotation waits at %q, want the consumer lock (SELECT … FROM context_model_consumers c … FOR UPDATE)", waiting[0])
		}
		if err := hold.Commit(ctx); err != nil {
			t.Fatalf("commit the deactivation: %v", err)
		}
		if err := <-done; !errors.Is(err, store.ErrModelConsumerInactive) {
			t.Errorf("rotation behind a deactivation: %v, want ErrModelConsumerInactive", err)
		}
		if n := mkCount(ctx, t, e.pool, `SELECT count(*) FROM context_model_keys WHERE consumer_id = $1::uuid`, c.ID); n != 1 {
			t.Errorf("%d key(s) after the refused rotation, want 1", n)
		}
		var actions []string
		for _, r := range kaRows(ctx, t, e.pool, "model_key", k1.ID) {
			actions = append(actions, r.Action)
		}
		if strings.Join(actions, ",") != "create,revoke" {
			t.Errorf("k1 audit actions %v, want [create revoke] (the rotation touched no key)", actions)
		}
	})

	t.Run("insert_guard_maps_to_inactive", func(t *testing.T) {
		c, _, _ := mkCreate(ctx, t, e, tenant, "guard")
		if err := store.DeleteConsumer(ctx, e.pool, c.ID, store.ModelAccess{TenantID: tenant}, e.stamp); err != nil {
			t.Fatalf("DeleteConsumer: %v", err)
		}
		key, err := modelkey.Generate()
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		tx, err := e.pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		_, err = store.InsertModelKeyForTest(ctx, tx, c.ID, key, nil, nil)
		if !errors.Is(err, store.ErrModelConsumerInactive) {
			t.Errorf("key insert into an inactive consumer: %v (SQLSTATE %q), want ErrModelConsumerInactive", err, mcSQLState(err))
		}
	})
}

func TestModelKeyTenantJoin(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	e := mkSetup(t)
	a := mkTenant(ctx, t, e.pool, "mk-join-a", nil)
	b := mkTenant(ctx, t, e.pool, "mk-join-b", nil)
	asA := store.ModelAccess{TenantID: a}
	cB, kB, _ := mkCreate(ctx, t, e, b, "victim")
	mkCreate(ctx, t, e, a, "own")
	unknown := uuid.Must(uuid.NewV7()).String()

	t.Run("revoke_foreign_key_reads_as_unknown", func(t *testing.T) {
		foreign := store.RevokeKey(ctx, e.pool, kB.ID, asA, e.stamp)
		missing := store.RevokeKey(ctx, e.pool, unknown, asA, e.stamp)
		malformed := store.RevokeKey(ctx, e.pool, "not-a-uuid", asA, e.stamp)
		nobody := store.RevokeKey(ctx, e.pool, kB.ID, store.ModelAccess{}, e.stamp)
		for name, err := range map[string]error{"foreign": foreign, "unknown": missing, "malformed": malformed, "no tenant": nobody} {
			if !errors.Is(err, store.ErrModelKeyNotFound) {
				t.Errorf("%s: %v, want ErrModelKeyNotFound", name, err)
			}
		}
		if foreign == nil || missing == nil || foreign.Error() != missing.Error() || malformed == nil || malformed.Error() != missing.Error() {
			t.Errorf("errors differ: foreign %q, unknown %q, malformed %q", foreign, missing, malformed)
		}
		if r := mkReadKey(ctx, t, e.pool, kB.ID); r.revoked != nil {
			t.Errorf("tenant A revoked tenant B's key")
		}
	})

	t.Run("consumer_primitives_foreign_reads_as_unknown", func(t *testing.T) {
		label := "renamed"
		no := false
		calls := map[string]func(id string) error{
			"rotate": func(id string) error {
				_, _, err := store.RotateKey(ctx, e.pool, id, asA, time.Hour, nil, e.stamp)
				return err
			},
			"update": func(id string) error {
				_, err := store.UpdateConsumer(ctx, e.pool, id, asA, store.ModelConsumerPatch{Label: &label}, e.stamp)
				return err
			},
			"reactivate": func(id string) error {
				yes := true
				_, err := store.UpdateConsumer(ctx, e.pool, id, asA, store.ModelConsumerPatch{Active: &yes}, e.stamp)
				return err
			},
			"deactivate": func(id string) error {
				_, err := store.UpdateConsumer(ctx, e.pool, id, asA, store.ModelConsumerPatch{Active: &no}, e.stamp)
				return err
			},
			"delete": func(id string) error { return store.DeleteConsumer(ctx, e.pool, id, asA, e.stamp) },
			"history": func(id string) error {
				_, err := store.KeyHistory(ctx, e.pool, id, asA, nil, 10)
				return err
			},
		}
		for name, call := range calls {
			foreign, missing := call(cB.ID), call(unknown)
			if !errors.Is(foreign, store.ErrModelConsumerNotFound) || !errors.Is(missing, store.ErrModelConsumerNotFound) ||
				foreign.Error() != missing.Error() {
				t.Errorf("%s: foreign %v, unknown %v — want the same ErrModelConsumerNotFound", name, foreign, missing)
			}
		}
		var label2 string
		var active bool
		if err := e.pool.QueryRow(ctx, `SELECT label, active FROM context_model_consumers WHERE id = $1::uuid`, cB.ID).Scan(&label2, &active); err != nil {
			t.Fatalf("read victim: %v", err)
		}
		if label2 != "victim" || !active || len(mkLiveIDs(ctx, t, e.pool, cB.ID)) != 1 {
			t.Errorf("tenant A changed tenant B's consumer: label %q active %v", label2, active)
		}
	})

	t.Run("list_and_ids_stay_in_the_tenant", func(t *testing.T) {
		list, err := store.ListConsumers(ctx, e.pool, asA, store.ListConsumersOptions{IncludeInactive: true})
		if err != nil {
			t.Fatalf("ListConsumers: %v", err)
		}
		for _, c := range list {
			if c.TenantID != a {
				t.Errorf("tenant A's list carries consumer %s of tenant %s", c.ID, c.TenantID)
			}
		}
		if len(list) != 1 {
			t.Errorf("tenant A lists %d consumer(s), want 1", len(list))
		}
		if none, err := store.ListConsumers(ctx, e.pool, store.ModelAccess{}, store.ListConsumersOptions{IncludeInactive: true}); err != nil || len(none) != 0 {
			t.Errorf("no tenant, no server-admin: %d consumer(s), err %v — want none", len(none), err)
		}
		ids, err := store.TenantConsumerIDs(ctx, e.pool, b)
		if err != nil || len(ids) != 1 || ids[0] != cB.ID {
			t.Errorf("TenantConsumerIDs(B) = %v, %v — want [%s]", ids, err, cB.ID)
		}
		if ids, err := store.TenantConsumerIDs(ctx, e.pool, ""); err != nil || ids == nil || len(ids) != 0 {
			t.Errorf("TenantConsumerIDs(\"\") = %#v, %v — want an empty slice", ids, err)
		}
	})

	t.Run("owner_and_server_admin_reach_it", func(t *testing.T) {
		if err := store.RevokeKey(ctx, e.pool, kB.ID, store.ModelAccess{TenantID: b}, e.stamp); err != nil {
			t.Fatalf("tenant B revokes its key: %v", err)
		}
		if err := store.RevokeKey(ctx, e.pool, kB.ID, store.ModelAccess{TenantID: b}, e.stamp); err != nil {
			t.Errorf("second revoke of the same key: %v, want a no-op success", err)
		}
		if err := store.RevokeKey(ctx, e.pool, kB.ID, store.ModelAccess{ServerAdmin: true}, e.stamp); err != nil {
			t.Errorf("server-admin revoke of a revoked key: %v", err)
		}
		if r := mkReadKey(ctx, t, e.pool, kB.ID); r.revoked == nil {
			t.Errorf("key still live after its own tenant revoked it")
		}
	})
}

func TestListSingleQuery(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	e := mkSetup(t)
	tenant := mkTenant(ctx, t, e.pool, "mk-list", nil)
	// Three model aliases, granted to every consumer (W01-9: the grants ride
	// in the same statement, one LATERAL aggregate).
	var aliases []string
	for i := range 3 {
		aliases = append(aliases, maAlias(ctx, t, e.pool, "_global", fmt.Sprintf("mk-list-a%d", i), nil))
	}
	sort.Strings(aliases)
	rotated := map[string]bool{}
	for i := range 50 {
		c, _, _ := mkCreate(ctx, t, e, tenant, fmt.Sprintf("list-%02d", i))
		if i%5 == 0 {
			mkRotate(ctx, t, e, c.ID, 24*time.Hour)
			rotated[c.ID] = true
		}
		for _, a := range aliases {
			mgGrant(ctx, t, e.pool, c.ID, a)
		}
	}
	tr := &mkTracer{}
	tp := mkTracedPool(t, e.pool, tr)
	if err := tp.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	tr.reset()
	start := time.Now()
	list, err := store.ListConsumers(ctx, tp, store.ModelAccess{TenantID: tenant}, store.ListConsumersOptions{})
	took := time.Since(start)
	if err != nil {
		t.Fatalf("ListConsumers: %v", err)
	}
	sqls, _ := tr.snapshot()
	t.Logf("measured: ListConsumers over 50 consumers / 60 keys / 150 grants: %d statement(s), %v", len(sqls), took)
	if len(sqls) != 1 {
		t.Errorf("ListConsumers sent %d statements, want exactly 1:\n%s", len(sqls), strings.Join(sqls, "\n---\n"))
	}
	if len(list) != 50 {
		t.Fatalf("listed %d consumers, want 50", len(list))
	}
	for i, c := range list {
		want := 1
		if rotated[c.ID] {
			want = 2
		}
		if len(c.Keys) != want || c.KeysTotal != want {
			t.Errorf("consumer %s: %d live / %d total, want %d / %d", c.Label, len(c.Keys), c.KeysTotal, want, want)
		}
		if strings.Join(c.Models, ",") != strings.Join(aliases, ",") {
			t.Errorf("consumer %s: models %v, want %v", c.Label, c.Models, aliases)
		}
		if i > 0 && c.CreatedAt.After(list[i-1].CreatedAt) {
			t.Errorf("list not newest first at %d", i)
		}
		for _, k := range c.Keys {
			if k.ConsumerID != c.ID || k.Hint == "" || !strings.HasPrefix(k.Hint, modelkey.Prefix) {
				t.Errorf("consumer %s key %+v: wrong consumer or hint", c.Label, k)
			}
		}
	}
}

// mkSecretRE matches a hex SHA-256 — the shape of key_hash.
var mkSecretRE = regexp.MustCompile(`[0-9a-f]{64}`)

func mkAssertNoSecret(t *testing.T, where, text string, keys []modelkey.Key) {
	t.Helper()
	if m := mkSecretRE.FindString(text); m != "" {
		t.Errorf("%s carries a 64-hex value %s…", where, m[:12])
	}
	for _, k := range keys {
		p := k.Plaintext()
		if strings.Contains(text, p) || strings.Contains(text, p[len(modelkey.Prefix):]) {
			t.Errorf("%s carries the plaintext of %s", where, k.Hint())
		}
		if strings.Contains(text, k.Hash()) {
			t.Errorf("%s carries the hash of %s", where, k.Hint())
		}
	}
}

func TestListNeverCarriesSecret(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	e := mkSetup(t)
	tenant := mkTenant(ctx, t, e.pool, "mk-secret", nil)
	c, k1, key1 := mkCreate(ctx, t, e, tenant, "secret")
	k2, key2 := mkRotate(ctx, t, e, c.ID, 24*time.Hour)
	k3, key3 := mkRotate(ctx, t, e, c.ID, 24*time.Hour)
	keys := []modelkey.Key{key1, key2, key3}

	// The check is only meaningful if the stored hash has the shape it looks for.
	var stored string
	if err := e.pool.QueryRow(ctx, `SELECT key_hash FROM context_model_keys WHERE id = $1::uuid`, k3.ID).Scan(&stored); err != nil {
		t.Fatalf("read hash: %v", err)
	}
	if stored != key3.Hash() || !mkSecretRE.MatchString(stored) {
		t.Fatalf("stored hash %q is not key3.Hash() in 64-hex form", stored)
	}

	list, err := store.ListConsumers(ctx, e.pool, store.ModelAccess{TenantID: tenant}, store.ListConsumersOptions{IncludeInactive: true})
	if err != nil {
		t.Fatalf("ListConsumers: %v", err)
	}
	hist, err := store.KeyHistory(ctx, e.pool, c.ID, store.ModelAccess{TenantID: tenant}, nil, 100)
	if err != nil {
		t.Fatalf("KeyHistory: %v", err)
	}
	if len(hist) != 3 || hist[0].ID != k3.ID || hist[1].ID != k2.ID || hist[2].ID != k1.ID {
		t.Errorf("history %+v, want k3 k2 k1", hist)
	}
	page, err := store.KeyHistory(ctx, e.pool, c.ID, store.ModelAccess{TenantID: tenant},
		&store.KeyCursor{CreatedAt: hist[0].CreatedAt, ID: hist[0].ID}, 1)
	if err != nil || len(page) != 1 || page[0].ID != k2.ID {
		t.Errorf("history after k3, limit 1: %+v, %v — want [k2]", page, err)
	}
	for name, v := range map[string]any{"list": list, "history": hist, "created": []any{c, k1}, "rotated": []any{k2, k3}} {
		js, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal %s: %v", name, err)
		}
		mkAssertNoSecret(t, name+" (json)", string(js), keys)
		mkAssertNoSecret(t, name+" (%#v)", fmt.Sprintf("%#v %+v", v, v), keys)
	}

	rows, err := e.pool.Query(ctx, `SELECT entity_type, action, COALESCE(old_value::text, ''), COALESCE(new_value::text, ''), metadata::text
		  FROM context_settings_audit
		 WHERE entity_type IN ('model_consumer', 'model_key') AND entity_key = ANY($1::text[])`,
		[]string{c.ID, k1.ID, k2.ID, k3.ID})
	if err != nil {
		t.Fatalf("audit rows: %v", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var et, action, oldV, newV, meta string
		if err := rows.Scan(&et, &action, &oldV, &newV, &meta); err != nil {
			t.Fatalf("scan audit row: %v", err)
		}
		n++
		mkAssertNoSecret(t, fmt.Sprintf("audit %s/%s", et, action), oldV+" "+newV+" "+meta, keys)
	}
	// consumer create; k1 create + update (grace) + revoke; k2 create + update; k3 create.
	if n != 7 {
		t.Errorf("%d audit rows, want 7", n)
	}
}

func TestUsageFlushNoLockWait(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	e := mkSetup(t)
	tenant := mkTenant(ctx, t, e.pool, "mk-usage", nil)
	c, k, _ := mkCreate(ctx, t, e, tenant, "usage")
	auditBefore := mkCount(ctx, t, e.pool, `SELECT count(*) FROM context_settings_audit WHERE entity_key = ANY($1::text[])`, []string{c.ID, k.ID})
	lastUsed := func() *time.Time {
		t.Helper()
		var ts *time.Time
		if err := e.pool.QueryRow(ctx, `SELECT last_used_at FROM context_model_keys WHERE id = $1::uuid`, k.ID).Scan(&ts); err != nil {
			t.Fatalf("read last_used_at: %v", err)
		}
		return ts
	}

	t.Run("200_touches_across_a_minute_edge", func(t *testing.T) {
		tr := &mkTracer{}
		tp := mkTracedPool(t, e.pool, tr)
		u := store.NewModelKeyUsage(tp)
		edge := time.Date(2026, 11, 3, 23, 0, 0, 0, time.UTC)
		var tick atomic.Int64
		store.SetModelKeyUsageClockForTest(u, func() time.Time {
			return edge.Add(-100*time.Millisecond + time.Duration(tick.Add(1))*time.Millisecond)
		})
		if err := tp.Ping(ctx); err != nil {
			t.Fatalf("ping: %v", err)
		}
		tr.reset()

		var maxWaiters atomic.Int64
		stop := make(chan struct{})
		ready := make(chan struct{})
		sampled := make(chan int, 1)
		var readyOnce sync.Once
		go func() {
			samples := 0
			for {
				select {
				case <-stop:
					sampled <- samples
					return
				default:
				}
				var n int64
				if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
					WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n); err == nil {
					samples++
					readyOnce.Do(func() { close(ready) })
					if n > maxWaiters.Load() {
						maxWaiters.Store(n)
					}
				}
			}
		}()
		<-ready
		var wg sync.WaitGroup
		for range 200 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				u.Touch(k.ID)
			}()
		}
		wg.Wait()
		if err := u.Flush(ctx); err != nil {
			t.Fatalf("Flush: %v", err)
		}
		close(stop)
		samples := <-sampled

		sqls, durs := tr.snapshot()
		updates := 0
		for _, s := range sqls {
			if strings.Contains(s, "UPDATE context_model_keys") {
				updates++
			}
		}
		var flushDur time.Duration
		for _, d := range durs {
			flushDur += d
		}
		t.Logf("measured: %d statement(s), %d UPDATE(s), %v statement time, %d pg_stat_activity samples, max lock waiters %d",
			len(sqls), updates, flushDur, samples, maxWaiters.Load())
		if updates != 1 || len(sqls) != 1 {
			t.Errorf("200 touches + flush sent %d statement(s), %d UPDATE(s) — want exactly 1", len(sqls), updates)
		}
		if maxWaiters.Load() != 0 {
			t.Errorf("max lock waiters %d, want 0", maxWaiters.Load())
		}
		want := edge.Add(100 * time.Millisecond)
		if got := lastUsed(); got == nil || !got.Equal(want) {
			t.Errorf("last_used_at %v, want the latest touch %v", got, want)
		}
		if n := mkCount(ctx, t, e.pool, `SELECT count(*) FROM context_settings_audit WHERE entity_key = ANY($1::text[])`, []string{c.ID, k.ID}); n != auditBefore {
			t.Errorf("audit rows %d → %d, want unchanged (the flush is outside the OF list)", auditBefore, n)
		}
	})

	t.Run("monotonic", func(t *testing.T) {
		later := time.Date(2026, 11, 4, 1, 0, 0, 0, time.UTC)
		if _, err := e.pool.Exec(ctx, `UPDATE context_model_keys SET last_used_at = $2 WHERE id = $1::uuid`, k.ID, later); err != nil {
			t.Fatalf("seed last_used_at: %v", err)
		}
		u := store.NewModelKeyUsage(e.pool)
		at := later.Add(-time.Hour)
		store.SetModelKeyUsageClockForTest(u, func() time.Time { return at })
		u.Touch(k.ID)
		if err := u.Flush(ctx); err != nil {
			t.Fatalf("Flush: %v", err)
		}
		if got := lastUsed(); got == nil || !got.Equal(later) {
			t.Errorf("an older stamp moved last_used_at back to %v, want %v", got, later)
		}
		at = later.Add(time.Hour)
		u.Touch(k.ID)
		if err := u.Flush(ctx); err != nil {
			t.Fatalf("Flush: %v", err)
		}
		if got := lastUsed(); got == nil || !got.Equal(at) {
			t.Errorf("a newer stamp: last_used_at %v, want %v", got, at)
		}
	})

	t.Run("failed_flush_is_retried", func(t *testing.T) {
		u := store.NewModelKeyUsage(e.pool)
		at := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
		store.SetModelKeyUsageClockForTest(u, func() time.Time { return at })
		u.Touch(k.ID)
		u.Touch("not-a-uuid")
		dead, cancel := context.WithCancel(ctx)
		cancel()
		if err := u.Flush(dead); err == nil {
			t.Fatalf("Flush on a cancelled context succeeded")
		}
		if err := u.Flush(ctx); err != nil {
			t.Fatalf("retry Flush: %v", err)
		}
		if got := lastUsed(); got == nil || !got.Equal(at) {
			t.Errorf("after the retry: last_used_at %v, want %v", got, at)
		}
	})

	// A touch that lands while a flush runs must survive the flush's
	// merge-back whether the flush fails (its context is cancelled the moment
	// the statement starts) or skips the key (held by another transaction,
	// next subtest): the later time wins.
	t.Run("touch_during_a_failed_flush_survives", func(t *testing.T) {
		older := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
		newer := older.Add(time.Hour)
		var clock atomic.Value
		clock.Store(older)
		var u *store.ModelKeyUsage
		flushCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		hook := &mkHookTracer{onStart: func(sql string) {
			if strings.Contains(sql, "UPDATE context_model_keys k SET last_used_at") {
				clock.Store(newer)
				u.Touch(k.ID)
				cancel()
			}
		}}
		u = store.NewModelKeyUsage(mkTracedPool(t, e.pool, hook))
		store.SetModelKeyUsageClockForTest(u, func() time.Time { return clock.Load().(time.Time) })
		u.Touch(k.ID)
		if err := u.Flush(flushCtx); err == nil {
			t.Fatalf("flush on a context cancelled at its statement succeeded")
		}
		hook.onStart = nil
		if err := u.Flush(ctx); err != nil {
			t.Fatalf("retry Flush: %v", err)
		}
		if got := lastUsed(); got == nil || !got.Equal(newer) {
			t.Errorf("after the retry: last_used_at %v, want the touch that landed during the failed flush (%v)", got, newer)
		}
	})

	t.Run("touch_during_a_skipping_flush_survives", func(t *testing.T) {
		hold, err := e.pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = hold.Rollback(ctx) }()
		if _, err := hold.Exec(ctx, `SELECT 1 FROM context_model_keys WHERE id = $1::uuid FOR UPDATE`, k.ID); err != nil {
			t.Fatalf("hold the key row: %v", err)
		}
		released := make(chan struct{})
		rolledBack := make(chan struct{})
		go func() {
			// Release the row once the skipping flush is done (the retry
			// inside newerTouchSurvives must reach it).
			<-released
			_ = hold.Rollback(ctx)
			close(rolledBack)
		}()
		// A pgx.Tx is not safe for concurrent use: the retry loop below sees
		// the stamp as soon as the server drops the row lock, while this
		// goroutine may still be reading the ROLLBACK reply — wait for it
		// before the deferred Rollback above runs on the same connection.
		defer func() { <-rolledBack }()
		older := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
		newer := older.Add(time.Hour)
		var clock atomic.Value
		clock.Store(older)
		var u *store.ModelKeyUsage
		hook := &mkHookTracer{onStart: func(sql string) {
			if strings.Contains(sql, "UPDATE context_model_keys k SET last_used_at") {
				clock.Store(newer)
				u.Touch(k.ID)
			}
		}}
		u = store.NewModelKeyUsage(mkTracedPool(t, e.pool, hook))
		store.SetModelKeyUsageClockForTest(u, func() time.Time { return clock.Load().(time.Time) })
		u.Touch(k.ID)
		if err := u.Flush(ctx); err != nil {
			t.Fatalf("flush skipping a held key: %v", err)
		}
		if got := lastUsed(); got != nil && !got.Before(older) {
			t.Fatalf("the held key was written (%v) — the flush did not skip it", got)
		}
		close(released)
		hook.onStart = nil
		deadline := time.Now().Add(10 * time.Second)
		for {
			if err := u.Flush(ctx); err != nil {
				t.Fatalf("retry Flush: %v", err)
			}
			if got := lastUsed(); got != nil && !got.Before(older) {
				if !got.Equal(newer) {
					t.Errorf("after the retry: last_used_at %v, want the touch that landed during the flush (%v)", got, newer)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the skipped stamp never reached the key")
			}
			time.Sleep(10 * time.Millisecond)
		}
	})

	t.Run("run_flushes_on_shutdown", func(t *testing.T) {
		u := store.NewModelKeyUsage(e.pool)
		at := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
		store.SetModelKeyUsageClockForTest(u, func() time.Time { return at })
		runCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			u.Run(runCtx)
			close(done)
		}()
		u.Touch(k.ID)
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("Run did not return after its context ended")
		}
		if got := lastUsed(); got == nil || !got.Equal(at) {
			t.Errorf("after shutdown: last_used_at %v, want %v", got, at)
		}
	})

	// #124: the final flush retries until its window ends — and ends then,
	// even when no attempt can succeed. The table lock makes the first
	// statement wait until the flush context's deadline cuts it; every
	// attempt after that runs on an expired context. Run must return within
	// the window (plus slack for a loaded host), keep the stamp pending, log
	// no WARN of its own for the cut attempt, and the stamp must still be
	// writable once the lock is gone.
	t.Run("run_returns_at_its_window_when_no_flush_can_succeed", func(t *testing.T) {
		u := store.NewModelKeyUsage(e.pool)
		at := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
		store.SetModelKeyUsageClockForTest(u, func() time.Time { return at })
		lock, err := e.pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = lock.Rollback(ctx) }()
		if _, err := lock.Exec(ctx, `LOCK TABLE context_model_keys IN ACCESS EXCLUSIVE MODE`); err != nil {
			t.Fatalf("lock: %v", err)
		}
		logs := &mkLogBuf{}
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
		t.Cleanup(func() { slog.SetDefault(prev) })
		runCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			u.Run(runCtx)
			close(done)
		}()
		u.Touch(k.ID)
		begin := time.Now()
		cancel()
		bound := store.ModelKeyUsageFinalFlushTimeout + 2*time.Second
		select {
		case <-done:
		case <-time.After(bound):
			t.Fatalf("Run did not return within %s of its context ending (window %s) — the final flush outlives its deadline", bound, store.ModelKeyUsageFinalFlushTimeout)
		}
		took := time.Since(begin)
		t.Logf("Run returned %s after its context ended (window %s, bound %s)", took.Round(time.Millisecond), store.ModelKeyUsageFinalFlushTimeout, bound)
		if n := u.Pending(); n != 1 {
			t.Errorf("Pending after Run: %d, want 1 (the stamp it could not write)", n)
		}
		// An attempt the window's deadline cut short is no failure to report:
		// the gateway's stop step counts what is left (one WARN, not two).
		if out := logs.String(); strings.Contains(out, "level=WARN") {
			t.Errorf("Run logged a WARN for the attempt its deadline cut short: %s", out)
		}
		if err := lock.Rollback(ctx); err != nil {
			t.Fatalf("release: %v", err)
		}
		if err := u.Flush(ctx); err != nil {
			t.Fatalf("Flush after the lock: %v", err)
		}
		if got := lastUsed(); got == nil || !got.Equal(at) {
			t.Errorf("after the lock: last_used_at %v, want the retained stamp %v", got, at)
		}
	})
}

// mkLogBuf is a log sink safe for the goroutine that writes it and the test
// that reads it.
type mkLogBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *mkLogBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *mkLogBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// mkHookTracer calls onStart with every statement a traced pool begins.
type mkHookTracer struct{ onStart func(sql string) }

func (h *mkHookTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if h.onStart != nil {
		h.onStart(data.SQL)
	}
	return ctx
}
func (h *mkHookTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// TestRevokeOrderAndIdempotence: every revoke path sets revoked_at =
// GREATEST(now(), created_at), so a key committed after the revoking
// transaction began (created_at > its now()) never trips
// ck_model_keys_revoke_order; and a second revoke changes nothing — not
// revoked_at, not the audit trail.
func TestRevokeOrderAndIdempotence(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	e := mkSetup(t)
	tenant := mkTenant(ctx, t, e.pool, "mk-revoke", nil)
	asT := store.ModelAccess{TenantID: tenant}

	t.Run("key_newer_than_the_revoking_transaction", func(t *testing.T) {
		c, _, _ := mkCreate(ctx, t, e, tenant, "future")
		fut, _ := mcKey(ctx, t, e.pool, c.ID, "mk-future", map[string]string{"created_at": "now() + interval '1 hour'"})
		if err := store.RevokeKey(ctx, e.pool, fut, asT, e.stamp); err != nil {
			t.Errorf("RevokeKey of a key created after now(): %v (SQLSTATE %s)", err, mcSQLState(err))
		}
		c2, _, _ := mkCreate(ctx, t, e, tenant, "future-rotate")
		mcKey(ctx, t, e.pool, c2.ID, "mk-future-2", map[string]string{"created_at": "now() + interval '1 hour'"})
		if _, _, err := store.RotateKey(ctx, e.pool, c2.ID, asT, 0, nil, e.stamp); err != nil {
			t.Errorf("RotateKey (grace 0) over a key created after now(): %v (SQLSTATE %s)", err, mcSQLState(err))
		}
	})

	t.Run("second_revoke_changes_nothing", func(t *testing.T) {
		_, k, _ := mkCreate(ctx, t, e, tenant, "twice")
		if err := store.RevokeKey(ctx, e.pool, k.ID, asT, e.stamp); err != nil {
			t.Fatalf("first revoke: %v", err)
		}
		first := mkReadKey(ctx, t, e.pool, k.ID).revoked
		if err := store.RevokeKey(ctx, e.pool, k.ID, asT, e.stamp); err != nil {
			t.Fatalf("second revoke: %v", err)
		}
		second := mkReadKey(ctx, t, e.pool, k.ID).revoked
		if first == nil || second == nil || !first.Equal(*second) {
			t.Errorf("second revoke moved revoked_at %v → %v", first, second)
		}
		var actions []string
		for _, r := range kaRows(ctx, t, e.pool, "model_key", k.ID) {
			actions = append(actions, r.Action)
		}
		if strings.Join(actions, ",") != "create,revoke" {
			t.Errorf("audit actions %v, want [create revoke]", actions)
		}
	})
}

// Review (CodeRabbit on PR #95): allowed_cidrs binds a []string to cidr[].
// pgx has no binary inet plan for a string, so the claim was that create and
// patch fail at parameter encoding; pgx falls back to the text format for the
// whole array and encodes each string as text. Pinned end to end — create,
// patch, clear — so the binding stays under test when W01-5 starts to pass
// the field.
func TestConsumerAllowedCIDRsRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	e := mkSetup(t)
	tenant := mkTenant(ctx, t, e.pool, "mk-cidr", nil)
	asT := store.ModelAccess{TenantID: tenant}
	c, _, _, err := store.CreateConsumerWithKey(ctx, e.pool, store.ModelConsumerSpec{
		TenantID: tenant, Label: "cidr", AllowedCIDRs: []string{"10.0.0.0/8", "192.168.1.0/24"},
	}, nil, e.stamp)
	if err != nil {
		t.Fatalf("create with allowed_cidrs: %v", err)
	}
	read := func(want []string) {
		t.Helper()
		list, err := store.ListConsumers(ctx, e.pool, asT, store.ListConsumersOptions{})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		for _, got := range list {
			if got.ID != c.ID {
				continue
			}
			if fmt.Sprint(got.AllowedCIDRs) != fmt.Sprint(want) {
				t.Fatalf("allowed_cidrs read back = %v, want %v", got.AllowedCIDRs, want)
			}
			return
		}
		t.Fatalf("consumer %s not listed", c.ID)
	}
	read([]string{"10.0.0.0/8", "192.168.1.0/24"})
	if _, err := store.UpdateConsumer(ctx, e.pool, c.ID, asT, store.ModelConsumerPatch{AllowedCIDRs: []string{"fd00::/8"}}, e.stamp); err != nil {
		t.Fatalf("patch allowed_cidrs: %v", err)
	}
	read([]string{"fd00::/8"})
	if _, err := store.UpdateConsumer(ctx, e.pool, c.ID, asT, store.ModelConsumerPatch{ClearAllowedCIDRs: true}, e.stamp); err != nil {
		t.Fatalf("clear allowed_cidrs: %v", err)
	}
	read(nil)
}

func TestModelKeyWritersStamp(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	e := mkSetup(t)
	tenant := mkTenant(ctx, t, e.pool, "mk-stamp", nil)
	asT := store.ModelAccess{TenantID: tenant}
	var principal string
	if err := e.pool.QueryRow(ctx, `SELECT principal_id::text FROM context_api_keys WHERE id = $1::uuid`, e.admin.ID).Scan(&principal); err != nil {
		t.Fatalf("read the admin's principal: %v", err)
	}

	c, k1, _ := mkCreate(ctx, t, e, tenant, "stamp")
	k2, _, err := store.RotateKey(ctx, e.pool, c.ID, asT, 24*time.Hour, nil, e.stamp)
	if err != nil {
		t.Fatalf("RotateKey: %v", err)
	}
	if err := store.RevokeKey(ctx, e.pool, k1.ID, asT, e.stamp); err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}
	label := "stamp-renamed"
	if _, err := store.UpdateConsumer(ctx, e.pool, c.ID, asT, store.ModelConsumerPatch{Label: &label}, e.stamp); err != nil {
		t.Fatalf("UpdateConsumer: %v", err)
	}
	if err := store.DeleteConsumer(ctx, e.pool, c.ID, asT, e.stamp); err != nil {
		t.Fatalf("DeleteConsumer: %v", err)
	}

	for _, w := range []struct{ entity, key, actions string }{
		{"model_consumer", c.ID, "create,update,deactivate"},
		{"model_key", k1.ID, "create,update,revoke"},
		{"model_key", k2.ID, "create,revoke"},
	} {
		var actions []string
		for _, r := range kaRows(ctx, t, e.pool, w.entity, w.key) {
			actions = append(actions, r.Action)
			if r.Meta["via"] != "api" || r.APIKeyID == nil || *r.APIKeyID != e.admin.ID ||
				r.Meta["tenant_id"] != tenant || r.Meta["request_id"] != "mk-req" {
				t.Errorf("%s %s %s: via=%v actor=%s tenant=%v request=%v — want api, %s, %s, mk-req",
					w.entity, w.key, r.Action, r.Meta["via"], mcStr(r.APIKeyID), r.Meta["tenant_id"], r.Meta["request_id"], e.admin.ID, tenant)
			}
		}
		if strings.Join(actions, ",") != w.actions {
			t.Errorf("%s %s actions %v, want %s", w.entity, w.key, actions, w.actions)
		}
	}
	var cBy, cByP *string
	if err := e.pool.QueryRow(ctx, `SELECT created_by_key::text, created_by_principal::text FROM context_model_consumers WHERE id = $1::uuid`, c.ID).Scan(&cBy, &cByP); err != nil {
		t.Fatalf("read consumer actor: %v", err)
	}
	if mcStr(cBy) != e.admin.ID || mcStr(cByP) != principal {
		t.Errorf("consumer created_by %s / %s, want %s / %s", mcStr(cBy), mcStr(cByP), e.admin.ID, principal)
	}
	for _, id := range []string{k1.ID, k2.ID} {
		r := mkReadKey(ctx, t, e.pool, id)
		if mcStr(r.createdBy) != e.admin.ID || mcStr(r.createdByP) != principal ||
			mcStr(r.revokedBy) != e.admin.ID || mcStr(r.revokedByP) != principal {
			t.Errorf("key %s actors created %s/%s revoked %s/%s, want %s/%s twice", id,
				mcStr(r.createdBy), mcStr(r.createdByP), mcStr(r.revokedBy), mcStr(r.revokedByP), e.admin.ID, principal)
		}
	}
}
