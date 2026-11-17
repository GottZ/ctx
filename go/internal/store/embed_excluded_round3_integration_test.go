//go:build integration

// Issue #213, second review of migration 176: a tenant's type write does not
// wait for another tenant's block writes, the advisory locks of a statement
// follow its groups, a 175 registry write in flight during the migration
// loses no entry, a stale vector survives a lift that commits under the
// reconcile or a content change, a '*' entry clears only the groups that
// lift, the left checks catch a block a pass leaves behind, and an assignment
// reaches the names of the tenant's own rows.
//
//	go test -tags=integration ./internal/store/ -run 'TestEmbedExcludedR3' -count=1 -v
package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// TestEmbedExcludedR3TenantRowWaitsForItsGroupOnly: tenant B holds a
// transaction open that wrote a checkpoint block (answer true, so it holds
// the name's and its group's locks shared). Tenant A's excluded overlay of
// checkpoint and tenant C's checkpoint block do not wait for it; a lift of A
// waits for an open block write of A.
func TestEmbedExcludedR3TenantRowWaitsForItsGroupOnly(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	reconcile(t, pool)
	ownerTenant(t, pool, "wg-a", "wg-a")
	ownerTenant(t, pool, "wg-b", "wg-b")
	ownerTenant(t, pool, "wg-c", "wg-c")
	btx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = btx.Rollback(ctx) }()
	if _, err := btx.Exec(ctx, `INSERT INTO context_blocks (category, title, content, type_name, scope, sensitivity, updated_at)
		VALUES ('wg', 'b open', 'x', 'checkpoint', 'wg-b', 'public', now())`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := store.CreateBlockType(ctx, pool, store.BlockTypeWrite{Name: "checkpoint", Scope: "wg-a", DisplayName: "cp",
			Config: []byte(`{"retrieval":{"policy":"excluded"}}`)}, nil, "wg")
		done <- err
	}()
	aWaited := lockWaitSeen(t, pool, func() bool { return len(done) == 0 })
	if aWaited {
		t.Errorf("tenant A's type write waits for tenant B's open block write")
		if err := btx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !aWaited {
		// C's block write of the same name may wait for B's open transaction
		// on a row lock outside this change (a block write updates shared
		// rows of other triggers); it must not wait on an advisory lock.
		cdone := make(chan error, 1)
		go func() {
			_, err := store.UpsertBlock(ctx, pool, "wg", "c checkpoint", "x", nil, nil, "wg-c", true, store.SensitivityWrite{}, "checkpoint")
			cdone <- err
		}()
		if advisoryWaitSeen(t, pool, func() bool { return len(cdone) == 0 }) {
			t.Errorf("tenant C's block write waits on an advisory lock behind tenant B's open block write")
		}
		if err := btx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-cdone; err != nil {
			t.Fatal(err)
		}
	}

	// The own group: A's open block write of checkpoint holds A's group lock
	// shared; A's lifting row waits for it.
	atx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = atx.Rollback(ctx) }()
	if _, err := atx.Exec(ctx, `INSERT INTO context_blocks (category, title, content, type_name, scope, sensitivity, updated_at)
		VALUES ('wg', 'a open', 'x', 'checkpoint', 'wg-a', 'public', now())`); err != nil {
		t.Fatal(err)
	}
	ldone := make(chan error, 1)
	go func() {
		_, err := pool.Exec(ctx, `UPDATE context_block_types SET config = '{"retrieval":{"policy":"full-pass"}}' WHERE name = 'checkpoint' AND scope = 'wg-a'`)
		ldone <- err
	}()
	if !lockWaitSeen(t, pool, func() bool { return len(ldone) == 0 }) {
		t.Errorf("tenant A's lift does not wait for A's own open block write")
	}
	if err := atx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-ldone; err != nil {
		t.Fatal(err)
	}
	reconcile(t, pool)
	if got := markedCount(t, pool, "wg-a", "checkpoint"); got != 0 {
		t.Errorf("%d of A's checkpoint blocks marked after A's lift and the reconcile", got)
	}
	if got := markedCount(t, pool, "wg-b", "checkpoint"); got != 1 {
		t.Errorf("%d/1 of B's checkpoint blocks marked", got)
	}
}

// TestEmbedExcludedR3LocksFollowGroups: one statement that writes blocks of
// a never-embedded type in three tenants holds one advisory lock for the name
// and one per group, however many rows it writes.
func TestEmbedExcludedR3LocksFollowGroups(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	reconcile(t, pool)
	for _, s := range []string{"lg-a", "lg-b", "lg-c"} {
		ownerTenant(t, pool, s, s)
	}
	for _, rows := range []int{3, 300} {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO context_blocks (category, title, content, type_name, scope, sensitivity, updated_at)
			SELECT 'lg', 'lg ' || $1::int || ' ' || i, 'x', 'checkpoint', (ARRAY['lg-a','lg-b','lg-c'])[1 + i % 3], 'public', now()
			  FROM generate_series(1, $1::int) i`, rows); err != nil {
			t.Fatal(err)
		}
		var adv int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND pid = pg_backend_pid()`).Scan(&adv); err != nil {
			t.Fatal(err)
		}
		_ = tx.Rollback(ctx)
		if adv != 4 {
			t.Errorf("an insert of %d checkpoint blocks in three tenants holds %d advisory locks, want 4 (the name and three groups)", rows, adv)
		}
	}
}

// TestEmbedExcludedR3MigrationStraggler: a registry write under the 175
// trigger is in flight while migration 176 runs (a binary before 176 lifts
// checkpoint). The migration waits for it at the trigger swap and moves the
// entry it queued in the old table afterwards, so the lift reaches the
// blocks.
func TestEmbedExcludedR3MigrationStraggler(t *testing.T) {
	pool := testdb.SetupTestDBUpTo(t, 175)
	ctx := context.Background()
	if _, err := reconcile175(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_blocks (category, title, content, type_name, scope, sensitivity, updated_at)
		 SELECT 'st', 'st ' || i, 'x', 'checkpoint', 'private', 'public', now() FROM generate_series(1, 5) i`); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcile175(ctx, pool); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE context_block_types SET config = jsonb_set(config, '{retrieval,policy}', '"full-pass"') WHERE name = 'checkpoint' AND scope = '_global'`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- store.RunMigrations(ctx, pool) }()
	if !lockWaitSeen(t, pool, func() bool { return len(done) == 0 }) {
		t.Fatalf("the migration did not wait for the in-flight registry write")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("migration: %v", err)
	}
	var old, queued int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM context_embed_excluded_reconcile), (SELECT count(*) FROM context_embed_excluded_queue WHERE name = 'checkpoint')`).Scan(&old, &queued); err != nil {
		t.Fatal(err)
	}
	if old != 0 || queued != 1 {
		t.Errorf("after the migration: old queue %d (want 0), new queue %d entries of checkpoint (want 1)", old, queued)
	}
	reconcile(t, pool)
	if got := markedCount(t, pool, "private", "checkpoint"); got != 0 {
		t.Errorf("%d/5 checkpoint blocks still marked after the lift and the reconcile", got)
	}
}

// staleFixture writes a block of typ in scope with a vector, then changes its
// content so the vector is kept stale (the type is embeddable then).
// advisoryWaitSeen reports whether a session waits on an advisory lock
// before the statement behind pending ends (or 2 s pass without either).
func advisoryWaitSeen(t *testing.T, pool *pgxpool.Pool, pending func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_stat_activity
			  WHERE datname = current_database() AND pid <> pg_backend_pid() AND wait_event_type = 'Lock' AND wait_event = 'advisory'`).Scan(&waiting); err != nil {
			t.Fatalf("advisory wait probe: %v", err)
		}
		if waiting > 0 {
			return true
		}
		if !pending() {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func staleFixture(t *testing.T, pool *pgxpool.Pool, scope, title, typ string) *store.Block {
	t.Helper()
	ctx := context.Background()
	b := storeTyped(t, pool, scope, title, typ)
	if _, err := pool.Exec(ctx, `UPDATE context_blocks SET embedding = array_fill(0.1::real, ARRAY[1024])::vector, embed_model = 'test' WHERE id = $1`, b.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertBlock(ctx, pool, "ee-cat", title, "changed body", nil, nil, scope, true, store.SensitivityWrite{}, typ); err != nil {
		t.Fatal(err)
	}
	return b
}

func vecState(t *testing.T, pool *pgxpool.Pool, id string) (vec, stale, marked bool) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `SELECT embedding IS NOT NULL, embedding_stale, embed_excluded FROM context_blocks WHERE id = $1`, id).
		Scan(&vec, &stale, &marked); err != nil {
		t.Fatal(err)
	}
	return
}

// TestEmbedExcludedR3StaleSurvivesLift: a lift of the type commits while the
// reconcile's stale batch of the name waits for it (the type was excluded and
// queued before): the block's stale vector stays, unmarked — the trigger
// decides the drop under its lock.
func TestEmbedExcludedR3StaleSurvivesLift(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	reconcile(t, pool)
	ownerTenant(t, pool, "sl-a", "sl-a")
	row := createType(t, pool, "sl-x", "sl-a", "full-pass")
	reconcile(t, pool)
	b := staleFixture(t, pool, "sl-a", "sl stale", "sl-x")
	if v, s, _ := vecState(t, pool, b.ID); !v || !s {
		t.Fatalf("fixture: vector %v stale %v", v, s)
	}
	updateType(t, pool, row.ID, "sl-a", "excluded")
	lift, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lift.Rollback(ctx) }()
	if _, err := lift.Exec(ctx, `UPDATE context_block_types SET config = '{"retrieval":{"policy":"full-pass"}}' WHERE id = $1`, row.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := store.ReconcileEmbedExcluded(ctx, pool)
		done <- err
	}()
	if !lockWaitSeen(t, pool, func() bool { return len(done) == 0 }) {
		t.Errorf("the reconcile's stale batch did not wait for the open lift")
	}
	if err := lift.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	reconcile(t, pool)
	if v, s, m := vecState(t, pool, b.ID); !v || !s || m {
		t.Errorf("after the lift committed under the stale batch: vector %v, stale %v, marker %v; want true, true, false", v, s, m)
	}
}

// TestEmbedExcludedR3ContentChangeDuringLift: a content change of a block of
// an excluded type (holding a fresh vector) while a lift of its type is in
// flight, through the upsert and through the by-id update: the change waits
// for the lift, keeps the vector stale and leaves the block unmarked.
func TestEmbedExcludedR3ContentChangeDuringLift(t *testing.T) {
	for _, viaUpdate := range []bool{false, true} {
		name := "upsert"
		if viaUpdate {
			name = "update"
		}
		t.Run(name, func(t *testing.T) {
			pool := testdb.SetupTestDB(t)
			ctx := context.Background()
			reconcile(t, pool)
			ownerTenant(t, pool, "cl-a", "cl-a")
			row := createType(t, pool, "cl-x", "cl-a", "full-pass")
			b := storeTyped(t, pool, "cl-a", "cl block", "cl-x")
			if _, err := pool.Exec(ctx, `UPDATE context_blocks SET embedding = array_fill(0.1::real, ARRAY[1024])::vector, embed_model = 'test' WHERE id = $1`, b.ID); err != nil {
				t.Fatal(err)
			}
			updateType(t, pool, row.ID, "cl-a", "excluded")
			reconcile(t, pool)
			lift, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = lift.Rollback(ctx) }()
			if _, err := lift.Exec(ctx, `UPDATE context_block_types SET config = '{"retrieval":{"policy":"full-pass"}}' WHERE id = $1`, row.ID); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				if viaUpdate {
					c := "changed body"
					_, _, err := store.UpdateBlock(ctx, pool, b.ID, store.UpdateBlockData{Content: &c}, []string{"cl-a"})
					done <- err
					return
				}
				_, err := store.UpsertBlock(ctx, pool, "ee-cat", "cl block", "changed body", nil, nil, "cl-a", true, store.SensitivityWrite{}, "cl-x")
				done <- err
			}()
			if !lockWaitSeen(t, pool, func() bool { return len(done) == 0 }) {
				t.Errorf("the content change did not wait for the open lift")
			}
			if err := lift.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			reconcile(t, pool)
			if v, s, m := vecState(t, pool, b.ID); !v || !s || m {
				t.Errorf("after the lift: vector %v, stale %v, marker %v; want true, true, false", v, s, m)
			}
		})
	}
}

// TestEmbedExcludedR3StarClearsLiftingGroupsOnly: a '*' entry of checkpoint
// while its _global row is excluded clears only the groups that hold a
// lifting row of it: with no such group no clear statement runs; with one,
// the clear runs for that group and its blocks are cleared.
func TestEmbedExcludedR3StarClearsLiftingGroupsOnly(t *testing.T) {
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	traced, tracer := r3Traced(t, dsn)
	reconcile(t, pool)
	ownerTenant(t, pool, "sc-a", "sc-a")
	ownerTenant(t, pool, "sc-b", "sc-b")
	for _, s := range []string{"sc-a", "sc-b"} {
		ownerSeed(t, pool, s, "checkpoint", 3)
	}
	clears := func() int {
		n := 0
		for _, st := range tracer.take() {
			if strings.Contains(st.sql, "SET embed_excluded = false") {
				n++
			}
		}
		return n
	}
	if _, err := pool.Exec(ctx, `INSERT INTO context_embed_excluded_queue (name, grp) VALUES ('checkpoint', '*')`); err != nil {
		t.Fatal(err)
	}
	tracer.take()
	if _, err := store.ReconcileEmbedExcluded(ctx, traced); err != nil {
		t.Fatal(err)
	}
	if n := clears(); n != 0 {
		t.Errorf("a '*' entry of checkpoint with no lifting row ran %d clear statements, want 0", n)
	}
	// B lifts checkpoint (a row past the gate); the '*' entry clears B only.
	if _, err := pool.Exec(ctx, `INSERT INTO context_block_types (name, scope, display_name, builtin, is_default, config)
		VALUES ('checkpoint', 'sc-b', 'cp', false, false, '{"retrieval":{"policy":"full-pass"}}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM context_embed_excluded_queue`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO context_embed_excluded_queue (name, grp) VALUES ('checkpoint', '*')`); err != nil {
		t.Fatal(err)
	}
	stA := scopeState(t, pool, "sc-a")
	tracer.take()
	if _, err := store.ReconcileEmbedExcluded(ctx, traced); err != nil {
		t.Fatal(err)
	}
	if n := clears(); n == 0 {
		t.Errorf("a '*' entry with B's lifting row ran no clear statement")
	}
	if got := markedCount(t, pool, "sc-b", "checkpoint"); got != 0 {
		t.Errorf("%d/3 of B's checkpoint blocks marked after the '*' entry", got)
	}
	if scopeState(t, pool, "sc-a") != stA {
		t.Errorf("the '*' entry rewrote A's rows")
	}
}

func r3Traced(t *testing.T, dsn string) (*pgxpool.Pool, *reviewTracer) {
	t.Helper()
	tracer := &reviewTracer{}
	pcfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	pcfg.ConnConfig.Tracer = tracer
	traced, err := pgxpool.NewWithConfig(context.Background(), pcfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(traced.Close)
	return traced, tracer
}

// r3Hook installs a statement trigger that runs body once, inside the first
// reconcile batch that updates embed_excluded (a flag table makes it once).
func r3Hook(t *testing.T, pool *pgxpool.Pool, body string) {
	t.Helper()
	for _, st := range []string{
		`CREATE TABLE r3_hook_once (done boolean NOT NULL)`,
		`INSERT INTO r3_hook_once VALUES (false)`,
		`CREATE FUNCTION r3_hook() RETURNS trigger LANGUAGE plpgsql AS $$
		 BEGIN
		     IF (SELECT NOT done FROM r3_hook_once) THEN
		         UPDATE r3_hook_once SET done = true;
		         ` + body + `
		     END IF;
		     RETURN NULL;
		 END $$`,
		`CREATE TRIGGER zz_r3_hook AFTER UPDATE OF embed_excluded ON context_blocks FOR EACH STATEMENT EXECUTE FUNCTION r3_hook()`,
	} {
		if _, err := pool.Exec(context.Background(), st); err != nil {
			t.Fatalf("%s: %v", st, err)
		}
	}
}

// TestEmbedExcludedR3MarkLeftCatchesABlockBehindTheCursor: while the first
// mark batch runs, a block of the name not yet reached changes its
// sensitivity so that it sorts before the cursor. The cursor pass leaves it;
// the left check sees it and the second round marks it.
func TestEmbedExcludedR3MarkLeftCatchesABlockBehindTheCursor(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	reconcile(t, pool)
	ownerTenant(t, pool, "ml-a", "ml-a")
	if _, err := pool.Exec(ctx, `INSERT INTO context_blocks (category, title, content, type_name, scope, sensitivity, updated_at)
		SELECT 'ml', 'ml ' || s || ' ' || i, 'x', 'ml-x', 'ml-a', s, now()
		  FROM unnest(ARRAY['internal', 'public']) s, generate_series(1, 1000) i`); err != nil {
		t.Fatal(err)
	}
	createType(t, pool, "ml-x", "ml-a", "excluded")
	r3Hook(t, pool, `UPDATE context_blocks SET sensitivity = 'credentials' WHERE title = 'ml public 1';`)
	reconcile(t, pool)
	var moved bool
	if err := pool.QueryRow(ctx, `SELECT embed_excluded FROM context_blocks WHERE title = 'ml public 1'`).Scan(&moved); err != nil {
		t.Fatal(err)
	}
	if !moved {
		t.Errorf("the block moved behind the cursor during the pass is not marked")
	}
	if got := markedCount(t, pool, "ml-a", "ml-x"); got != 2000 {
		t.Errorf("%d/2000 marked", got)
	}
}

// TestEmbedExcludedR3ClearLeftCatchesABlockBehindTheCursor: while the first
// clear batch runs, a block of the name the cursor has passed is marked
// again behind the trigger (a row out of step). The left check sees it and
// the second round clears it.
func TestEmbedExcludedR3ClearLeftCatchesABlockBehindTheCursor(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	reconcile(t, pool)
	ownerTenant(t, pool, "cr-a", "cr-a")
	row := createType(t, pool, "cr-x", "cr-a", "excluded")
	if _, err := pool.Exec(ctx, `INSERT INTO context_blocks (category, title, content, type_name, scope, sensitivity, updated_at)
		SELECT 'cr', 'cr ' || i, 'x', 'cr-x', 'cr-a', 'public', now() FROM generate_series(1, 1500) i`); err != nil {
		t.Fatal(err)
	}
	reconcile(t, pool)
	if got := markedCount(t, pool, "cr-a", "cr-x"); got != 1500 {
		t.Fatalf("fixture: %d/1500 marked", got)
	}
	updateType(t, pool, row.ID, "cr-a", "full-pass")
	r3Hook(t, pool, `SET LOCAL session_replication_role = replica;
		         UPDATE context_blocks SET embed_excluded = true
		          WHERE id = (SELECT id FROM context_blocks WHERE type_name = 'cr-x' ORDER BY id LIMIT 1);
		         SET LOCAL session_replication_role = origin;`)
	reconcile(t, pool)
	if got := markedCount(t, pool, "cr-a", "cr-x"); got != 0 {
		t.Errorf("%d/1500 still marked after the lift and the reconcile", got)
	}
}

// TestEmbedExcludedR3AssignReachesTheTenantsNames: a tenantless scope with
// blocks of a name that has no row in that scope is assigned to a tenant
// whose own row excludes the name: the assignment queues the name (found
// through the tenant's scopes), and the blocks are marked.
func TestEmbedExcludedR3AssignReachesTheTenantsNames(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	reconcile(t, pool)
	ta := ownerTenant(t, pool, "za-a", "za-a")
	createType(t, pool, "za-x", "za-a", "excluded")
	reconcile(t, pool)
	ids := ownerSeed(t, pool, "za-legacy", "za-x", 3)
	reconcile(t, pool)
	if marker(t, pool, ids[0]) {
		t.Fatalf("fixture: block in the tenantless scope is marked before the assignment")
	}
	if _, err := store.AssignTenantScope(ctx, pool, ta, "za-legacy", nil); err != nil {
		t.Fatal(err)
	}
	reconcile(t, pool)
	for _, id := range ids {
		if !marker(t, pool, id) {
			t.Errorf("block %s of za-x in the newly assigned scope is not marked", id)
		}
	}
}
