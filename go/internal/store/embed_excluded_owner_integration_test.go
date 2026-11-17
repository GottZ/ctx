//go:build integration

// Issue #213 (migration 176): the never-embedded answer of a block is read
// from the registry of the tenant that owns the block's scope, plus _global.
// A type that exists only in a tenant's scopes and is excluded there is
// marked and never picked; the registry change of one tenant never writes a
// block of another; a scope assignment or a scope move re-decides.
//
//	go test -tags=integration ./internal/store/ -run 'TestEmbedExcludedOwner' -count=1 -v
package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// ownerTenants creates tenants named after the scopes and assigns each its
// scopes: ownerTenants(t, pool, "oa", "oa2") gives one tenant with two scopes.
func ownerTenant(t *testing.T, pool *pgxpool.Pool, slug string, scopes ...string) string {
	t.Helper()
	ctx := context.Background()
	tn, err := store.CreateTenant(ctx, pool, slug, slug)
	if err != nil {
		t.Fatalf("create tenant %s: %v", slug, err)
	}
	for _, s := range scopes {
		if _, err := store.AssignTenantScope(ctx, pool, tn.ID, s, nil); err != nil {
			t.Fatalf("assign %s to %s: %v", s, slug, err)
		}
	}
	return tn.ID
}

// ownerSeed writes n blocks of typeName in scope through the store path and
// returns their ids.
func ownerSeed(t *testing.T, pool *pgxpool.Pool, scope, typeName string, n int) []string {
	t.Helper()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		b := storeTyped(t, pool, scope, "own "+scope+" "+typeName+" "+strings.Repeat("i", i+1), typeName)
		ids = append(ids, b.ID)
	}
	return ids
}

// scopeState is a fingerprint of every block of scope: id, row version and
// marker. Any rewrite of a row changes it.
func scopeState(t *testing.T, pool *pgxpool.Pool, scope string) string {
	t.Helper()
	var s *string
	if err := pool.QueryRow(context.Background(),
		`SELECT string_agg(id::text || ':' || xmin::text || ':' || embed_excluded::text, ',' ORDER BY id)
		   FROM context_blocks WHERE scope = $1`, scope).Scan(&s); err != nil {
		t.Fatal(err)
	}
	if s == nil {
		return ""
	}
	return *s
}

func markedCount(t *testing.T, pool *pgxpool.Pool, scope, typeName string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM context_blocks WHERE scope = $1 AND type_name = $2 AND embed_excluded AND NOT is_archived`,
		scope, typeName).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func updateType(t *testing.T, pool *pgxpool.Pool, id, scope, policy string) {
	t.Helper()
	cfg, _ := json.Marshal(map[string]any{"retrieval": map[string]any{"policy": policy}})
	if _, err := store.UpdateBlockType(context.Background(), pool, id, []string{scope}, store.BlockTypeUpdate{Config: cfg}, nil, "own-test"); err != nil {
		t.Fatalf("update type %s/%s -> %s: %v", scope, id, policy, err)
	}
}

func deleteType(t *testing.T, pool *pgxpool.Pool, id, scope string) {
	t.Helper()
	if found, err := store.DeleteBlockType(context.Background(), pool, id, []string{scope}, nil, "own-test"); err != nil || !found {
		t.Fatalf("delete type %s/%s: found=%v err=%v", scope, id, found, err)
	}
}

// TestEmbedExcludedOwnerTypePick: a block of a type whose only registry row
// is tenant A's, excluded, is marked and not picked — in A's scope and in A's
// second scope. A block of the same name in tenant B, which has no row of the
// name, stays embeddable (no row in its R).
func TestEmbedExcludedOwnerTypePick(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	reconcile(t, pool)
	ownerTenant(t, pool, "own-a", "own-a", "own-a2")
	ownerTenant(t, pool, "own-b", "own-b")
	createType(t, pool, "own-x", "own-a", "excluded")
	reconcile(t, pool)

	for _, scope := range []string{"own-a", "own-a2"} {
		b := storeTyped(t, pool, scope, "own x in "+scope, "own-x")
		if !marker(t, pool, b.ID) {
			t.Errorf("block of A's excluded own type in %s is not marked", scope)
		}
		if got := eePick(t, pool, scope, b.Sensitivity); got != "" {
			t.Errorf("block of A's excluded own type in %s picked: %s", scope, got)
		}
	}
	b := storeTyped(t, pool, "own-b", "own x in own-b", "own-x")
	if marker(t, pool, b.ID) {
		t.Errorf("block of the name in tenant B, which has no row of it, is marked by A's row")
	}
	if got := eePick(t, pool, "own-b", b.Sensitivity); got != b.ID {
		t.Errorf("block of the name in tenant B not picked: %q", got)
	}
}

// TestEmbedExcludedOwnerBoundary: no registry write of tenant A changes a row
// of tenant B, and no write of B a row of A (id, xmin and marker of every
// block of the other tenant stay as they were), for a tenant-own name, for a
// _global-excluded name with a lifting overlay, and for a tenant row that
// predates its _global base (F2 of #310). Each tenant's own blocks follow its
// own registry.
func TestEmbedExcludedOwnerBoundary(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	reconcile(t, pool)
	ownerTenant(t, pool, "bnd-a", "bnd-a")
	ownerTenant(t, pool, "bnd-b", "bnd-b")
	for _, s := range []string{"bnd-a", "bnd-b"} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blocks (category, title, content, type_name, scope, sensitivity, updated_at)
			 SELECT 'own-cat', 'bnd ' || n || ' ' || i, 'x', n, $1, 'public', now()
			   FROM unnest(ARRAY['bnd-x', 'checkpoint', 'bnd-y']) n, generate_series(1, 40) i`, s); err != nil {
			t.Fatalf("seed %s: %v", s, err)
		}
	}
	reconcile(t, pool)

	step := func(label, writer, other string, f func(), check func()) {
		t.Helper()
		before := scopeState(t, pool, other)
		f()
		reconcile(t, pool)
		if after := scopeState(t, pool, other); after != before {
			t.Errorf("%s: a registry write of %s changed rows of %s", label, writer, other)
		}
		check()
	}
	want := func(label, scope, typ string, n int) {
		t.Helper()
		if got := markedCount(t, pool, scope, typ); got != n {
			t.Errorf("%s: %s blocks of %s marked %d/40, want %d", label, scope, typ, got, n)
		}
	}

	for _, dir := range []struct{ w, o string }{{"bnd-a", "bnd-b"}, {"bnd-b", "bnd-a"}} {
		w, o := dir.w, dir.o
		var row *store.BlockTypeRow
		step(w+" creates own bnd-x excluded", w, o, func() { row = createType(t, pool, "bnd-x", w, "excluded") },
			func() { want(w+" own excluded", w, "bnd-x", 40) })
		step(w+" lifts own bnd-x", w, o, func() { updateType(t, pool, row.ID, w, "full-pass") },
			func() { want(w+" own lifted", w, "bnd-x", 0) })
		step(w+" excludes own bnd-x again", w, o, func() { updateType(t, pool, row.ID, w, "excluded") },
			func() { want(w+" own excluded again", w, "bnd-x", 40) })
		var ov *store.BlockTypeRow
		step(w+" lifts checkpoint by an overlay", w, o, func() { ov = createType(t, pool, "checkpoint", w, "full-pass") },
			func() {
				want(w+" checkpoint lifted for itself", w, "checkpoint", 0)
				want(w+" checkpoint of the other tenant", o, "checkpoint", 40)
			})
		// A type with blocks cannot be deleted (BlockTypeInUseError); the
		// overlay goes back to excluded instead.
		step(w+" narrows the overlay", w, o, func() { updateType(t, pool, ov.ID, w, "excluded") },
			func() { want(w+" checkpoint excluded again", w, "checkpoint", 40) })
		// Leave bnd-x lifted so the other direction starts from the same
		// state for the other tenant.
		updateType(t, pool, row.ID, w, "full-pass")
		reconcile(t, pool)
	}

	// F2 of #310: B's row of bnd-y predates its _global base. B's row
	// keeps B's blocks embeddable; the operator's excluded base marks A's,
	// and B narrowing its row marks B's and leaves A's rows as they are.
	by := createType(t, pool, "bnd-y", "bnd-b", "full-pass")
	reconcile(t, pool)
	createType(t, pool, "bnd-y", "_global", "excluded")
	reconcile(t, pool)
	want("F2 base", "bnd-a", "bnd-y", 40)
	want("F2 base", "bnd-b", "bnd-y", 0)
	step("B narrows its predating row", "bnd-b", "bnd-a", func() { updateType(t, pool, by.ID, "bnd-b", "excluded") },
		func() { want("F2 narrowed", "bnd-b", "bnd-y", 40) })
	step("B lifts it again", "bnd-b", "bnd-a", func() { updateType(t, pool, by.ID, "bnd-b", "full-pass") },
		func() { want("F2 lifted again", "bnd-b", "bnd-y", 0) })

	// An entry of A never writes B's rows, also rows of B that are out of
	// step with B's own answer (written while B's type became excluded in a
	// transaction that stayed open: the documented limit, they keep false
	// until B's next entry). B's rows of bnd-z carry false although B's own
	// row excludes the name; A's registry write of the name queues A's group,
	// and its reconcile leaves B's rows as they are.
	createType(t, pool, "bnd-z", "bnd-b", "excluded")
	reconcile(t, pool)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO context_blocks (category, title, content, type_name, scope, sensitivity, updated_at, embed_excluded)
		 SELECT 'own-cat', 'bnd z ' || i, 'x', 'bnd-z', 'bnd-b', 'public', now(), false FROM generate_series(1, 10) i`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	step("A creates bnd-z while B's rows of it are out of step", "bnd-a", "bnd-b",
		func() { createType(t, pool, "bnd-z", "bnd-a", "excluded") },
		func() { want("B out of step", "bnd-b", "bnd-z", 0) })

	// The other direction: B's rows of bnd-w carry the marker although B's
	// own row lifts the name; A's lifting row of the name makes A's entry
	// clear, and that clear leaves B's rows as they are.
	createType(t, pool, "bnd-w", "bnd-b", "full-pass")
	reconcile(t, pool)
	tx2, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx2.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx2.Exec(ctx,
		`INSERT INTO context_blocks (category, title, content, type_name, scope, sensitivity, updated_at, embed_excluded)
		 SELECT 'own-cat', 'bnd w ' || i, 'x', 'bnd-w', 'bnd-b', 'public', now(), true FROM generate_series(1, 10) i`); err != nil {
		t.Fatal(err)
	}
	if err := tx2.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	step("A lifts bnd-w while B's marked rows of it are out of step", "bnd-a", "bnd-b",
		func() { createType(t, pool, "bnd-w", "bnd-a", "full-pass") },
		func() { want("B out of step, marked", "bnd-b", "bnd-w", 10) })
}

// TestEmbedExcludedOwnerTransitions walks the transitions of a tenant-own
// type: created excluded over existing blocks (pending marked, a block with a
// vector keeps it), delete refused while blocks exist, lifted, a _global row
// of the name arriving later, re-type, a scope move across the tenant
// boundary, the assignment of a tenantless scope with blocks to a tenant, and
// a grant (no change).
func TestEmbedExcludedOwnerTransitions(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	reconcile(t, pool)
	ta := ownerTenant(t, pool, "tr-a", "tr-a")
	tb := ownerTenant(t, pool, "tr-b", "tr-b")
	pending := ownerSeed(t, pool, "tr-a", "tr-x", 3)
	vectored := storeTyped(t, pool, "tr-a", "tr x with vector", "tr-x")
	if _, err := pool.Exec(ctx, `UPDATE context_blocks SET embedding = array_fill(0.1::real, ARRAY[1024])::vector, embed_model = 'test' WHERE id = $1`, vectored.ID); err != nil {
		t.Fatal(err)
	}

	var sens string
	if err := pool.QueryRow(ctx, `SELECT sensitivity FROM context_blocks WHERE id = $1`, pending[0]).Scan(&sens); err != nil {
		t.Fatal(err)
	}
	x := createType(t, pool, "tr-x", "tr-a", "excluded")
	// Before the reconcile the predicate filters the unmarked blocks.
	if got := eePick(t, pool, "tr-a", sens); got != "" {
		t.Errorf("before the reconcile the pick returns a block of the excluded type: %s", got)
	}
	reconcile(t, pool)
	for _, id := range pending {
		if !marker(t, pool, id) {
			t.Errorf("pending block %s not marked after the create and the reconcile", id)
		}
	}
	var hasVec bool
	if err := pool.QueryRow(ctx, `SELECT embedding IS NOT NULL FROM context_blocks WHERE id = $1`, vectored.ID).Scan(&hasVec); err != nil {
		t.Fatal(err)
	}
	if marker(t, pool, vectored.ID) || !hasVec {
		t.Errorf("block with a vector: marker %v, vector kept %v; want false, true", marker(t, pool, vectored.ID), hasVec)
	}

	if _, err := store.DeleteBlockType(ctx, pool, x.ID, []string{"tr-a"}, nil, "own-test"); !errors.As(err, new(*store.BlockTypeInUseError)) {
		t.Errorf("delete of a type with blocks: %v, want BlockTypeInUseError", err)
	}

	updateType(t, pool, x.ID, "tr-a", "full-pass")
	reconcile(t, pool)
	if marker(t, pool, pending[0]) {
		t.Errorf("block still marked after the lift")
	}
	updateType(t, pool, x.ID, "tr-a", "excluded")
	reconcile(t, pool)

	// A _global row of the name arrives (a migration, an operator).
	g := createType(t, pool, "tr-x", "_global", "full-pass")
	reconcile(t, pool)
	if marker(t, pool, pending[0]) {
		t.Errorf("a visible _global row of the name did not lift A's blocks")
	}
	updateType(t, pool, g.ID, "_global", "excluded")
	reconcile(t, pool)
	if !marker(t, pool, pending[0]) {
		t.Errorf("_global and A's row excluded: A's block not marked")
	}

	// Re-type out and back.
	for _, c := range []struct {
		typ  string
		want bool
	}{{"knowledge", false}, {"tr-x", true}} {
		if _, err := pool.Exec(ctx, `UPDATE context_blocks SET type_name = $2 WHERE id = $1`, pending[1], c.typ); err != nil {
			t.Fatal(err)
		}
		if got := marker(t, pool, pending[1]); got != c.want {
			t.Errorf("re-type to %s: marker %v, want %v", c.typ, got, c.want)
		}
	}

	// Scope move across the tenant boundary, through the store's update
	// path: in B the name has only the _global row (excluded) -> still
	// marked; with a lifting row of B -> cleared at the move.
	bx := createType(t, pool, "tr-x", "tr-b", "full-pass")
	reconcile(t, pool)
	moveTo := "tr-b"
	if b, _, err := store.UpdateBlock(ctx, pool, pending[2], store.UpdateBlockData{Scope: &moveTo}, []string{"tr-a", "tr-b"}); err != nil || b == nil {
		t.Fatalf("move: block=%v err=%v", b, err)
	}
	if marker(t, pool, pending[2]) {
		t.Errorf("block moved into B, where B's row lifts the name, keeps its marker")
	}
	moveBack := "tr-a"
	if b, _, err := store.UpdateBlock(ctx, pool, pending[2], store.UpdateBlockData{Scope: &moveBack}, []string{"tr-a", "tr-b"}); err != nil || b == nil {
		t.Fatalf("move back: block=%v err=%v", b, err)
	}
	if !marker(t, pool, pending[2]) {
		t.Errorf("block moved back into A is not marked")
	}
	deleteType(t, pool, bx.ID, "tr-b")
	reconcile(t, pool)

	// A tenantless scope with blocks and its own rows is assigned to A.
	// tr-legacy's own row of tr-y is excluded (its blocks are marked); A's
	// row of tr-y is full-pass, so after the assignment R holds both and the
	// blocks are cleared. A's blocks of tr-z have no row in A's R until
	// tr-legacy (which excludes tr-z) joins A.
	createType(t, pool, "tr-y", "tr-legacy", "excluded")
	createType(t, pool, "tr-y", "tr-a", "full-pass")
	createType(t, pool, "tr-z", "tr-legacy", "excluded")
	legacy := ownerSeed(t, pool, "tr-legacy", "tr-y", 2)
	az := ownerSeed(t, pool, "tr-a", "tr-z", 2)
	reconcile(t, pool)
	if !marker(t, pool, legacy[0]) || marker(t, pool, az[0]) {
		t.Fatalf("fixture: legacy tr-y marked %v (want true), A's tr-z marked %v (want false)", marker(t, pool, legacy[0]), marker(t, pool, az[0]))
	}
	if _, err := store.AssignTenantScope(ctx, pool, ta, "tr-legacy", nil); err != nil {
		t.Fatalf("assign legacy: %v", err)
	}
	reconcile(t, pool)
	if marker(t, pool, legacy[0]) {
		t.Errorf("after the assignment A's lifting row of tr-y does not reach the legacy scope's blocks")
	}
	if !marker(t, pool, az[0]) {
		t.Errorf("after the assignment the legacy scope's excluded row of tr-z does not reach A's blocks")
	}

	// A grant changes no answer and queues nothing.
	before := scopeState(t, pool, "tr-a")
	if _, err := store.CreateTenantGrant(ctx, pool, tb, "tr-a", ""); err != nil {
		t.Fatalf("grant: %v", err)
	}
	var queued int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_embed_excluded_queue`).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	reconcile(t, pool)
	if queued != 0 || scopeState(t, pool, "tr-a") != before {
		t.Errorf("a grant queued %d entries or changed A's rows", queued)
	}
}

// ownerOpenWrite starts a transaction that writes one block of typeName in
// scope and returns it open with the block's id.
func ownerOpenWrite(t *testing.T, pool *pgxpool.Pool, scope, typeName string) (pgx.Tx, string) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var id string
	if err := tx.QueryRow(ctx,
		`INSERT INTO context_blocks (category, title, content, type_name, scope, sensitivity, updated_at)
		 VALUES ('own-cat', 'open write', 'x', $1, $2, 'public', now()) RETURNING id`, typeName, scope).Scan(&id); err != nil {
		t.Fatalf("open write: %v", err)
	}
	return tx, id
}

// TestEmbedExcludedOwnerInterleavings: a block written in a transaction that
// stays open, against each change that can lift its answer — a tenant
// registry change, a scope assignment and a _global change — in both orders,
// and a registry row written in a tenantless scope against the assignment of
// that scope. The later of the two waits for the earlier one's commit
// (observed as a lock wait in pg_stat_activity), and after a reconcile the
// block carries its answer.
func TestEmbedExcludedOwnerInterleavings(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		setup func(t *testing.T, pool *pgxpool.Pool) (scope, typeName string, lift func(tx pgx.Tx) error)
	}{
		{"tenant_registry_lift", func(t *testing.T, pool *pgxpool.Pool) (string, string, func(pgx.Tx) error) {
			ownerTenant(t, pool, "il-a", "il-a")
			row := createType(t, pool, "il-x", "il-a", "excluded")
			reconcile(t, pool)
			return "il-a", "il-x", func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `UPDATE context_block_types SET config = '{"retrieval":{"policy":"full-pass"}}' WHERE id = $1`, row.ID)
				return err
			}
		}},
		{"scope_assignment_lift", func(t *testing.T, pool *pgxpool.Pool) (string, string, func(pgx.Tx) error) {
			ta := ownerTenant(t, pool, "il-a", "il-a")
			createType(t, pool, "il-y", "il-free", "excluded")
			createType(t, pool, "il-y", "il-a", "full-pass")
			reconcile(t, pool)
			return "il-free", "il-y", func(tx pgx.Tx) error {
				_, err := store.AssignTenantScopeTx(ctx, tx, ta, "il-free", nil)
				return err
			}
		}},
		{"global_lift", func(t *testing.T, pool *pgxpool.Pool) (string, string, func(pgx.Tx) error) {
			ownerTenant(t, pool, "il-a", "il-a")
			reconcile(t, pool)
			return "il-a", "checkpoint", func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `UPDATE context_block_types SET config = jsonb_set(config, '{retrieval,policy}', '"full-pass"') WHERE name = 'checkpoint' AND scope = '_global'`)
				return err
			}
		}},
	}
	for _, c := range cases {
		for _, order := range []string{"block_first", "change_first"} {
			t.Run(c.name+"/"+order, func(t *testing.T) {
				pool := testdb.SetupTestDB(t)
				reconcile(t, pool)
				scope, typ, lift := c.setup(t, pool)
				var probe bool
				if err := pool.QueryRow(ctx, `SELECT ctx_block_type_never_embedded($1, $2)`, typ, scope).Scan(&probe); err != nil || !probe {
					t.Fatalf("fixture: answer before the change = %v (%v), want true", probe, err)
				}
				var id string
				if order == "block_first" {
					tx, bid := ownerOpenWrite(t, pool, scope, typ)
					defer func() { _ = tx.Rollback(ctx) }()
					id = bid
					done := make(chan error, 1)
					go func() {
						ctx := context.Background()
						tx2, err := pool.Begin(ctx)
						if err != nil {
							done <- err
							return
						}
						if err := lift(tx2); err != nil {
							_ = tx2.Rollback(ctx)
							done <- err
							return
						}
						done <- tx2.Commit(ctx)
					}()
					if !lockWaitSeen(t, pool, func() bool { return len(done) == 0 }) {
						t.Errorf("the change did not wait for the open block write")
					}
					if err := tx.Commit(ctx); err != nil {
						t.Fatal(err)
					}
					if err := <-done; err != nil {
						t.Fatalf("change: %v", err)
					}
				} else {
					tx2, err := pool.Begin(ctx)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = tx2.Rollback(ctx) }()
					if err := lift(tx2); err != nil {
						t.Fatalf("open change: %v", err)
					}
					type written struct {
						id  string
						err error
					}
					done := make(chan written, 1)
					go func() {
						var w written
						w.err = pool.QueryRow(context.Background(),
							`INSERT INTO context_blocks (category, title, content, type_name, scope, sensitivity, updated_at)
							 VALUES ('own-cat', 'during change', 'x', $1, $2, 'public', now()) RETURNING id`, typ, scope).Scan(&w.id)
						done <- w
					}()
					if !lockWaitSeen(t, pool, func() bool { return len(done) == 0 }) {
						t.Errorf("the block write did not wait for the open change")
					}
					if err := tx2.Commit(ctx); err != nil {
						t.Fatal(err)
					}
					w := <-done
					if w.err != nil {
						t.Fatalf("block write: %v", w.err)
					}
					id = w.id
					if marker(t, pool, id) {
						t.Errorf("block written during the change decided on the state before it")
					}
				}
				reconcile(t, pool)
				if marker(t, pool, id) {
					t.Errorf("block keeps embed_excluded after the lift and the reconcile")
				}
			})
		}
	}

	// A lifting registry row written in a tenantless scope while the scope is
	// assigned to a tenant whose blocks of the name are marked — by its own
	// excluded row (own_row), or by the _global row alone (global_only: the
	// tenant holds no row of the name, so the assignment finds the name only
	// through the new row): one of the two queues the name for the tenant's
	// group, so A's blocks are cleared.
	for _, c := range []struct{ kind, order string }{
		{"own_row", "row_first"}, {"own_row", "assignment_first"},
		{"global_only", "row_first"}, {"global_only", "assignment_first"},
	} {
		order := c.order
		t.Run("registry_row_vs_assignment/"+c.kind+"/"+order, func(t *testing.T) {
			pool := testdb.SetupTestDB(t)
			reconcile(t, pool)
			ta := ownerTenant(t, pool, "ra-a", "ra-a")
			name := "ra-x"
			if c.kind == "global_only" {
				name = "ra-g"
				createType(t, pool, name, "_global", "excluded")
			} else {
				createType(t, pool, name, "ra-a", "excluded")
			}
			ids := ownerSeed(t, pool, "ra-a", name, 3)
			reconcile(t, pool)
			if !marker(t, pool, ids[0]) {
				t.Fatalf("fixture: A's block not marked")
			}
			insertRow := func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `INSERT INTO context_block_types (name, scope, display_name, builtin, is_default, config)
					VALUES ($1::text, 'ra-free', $1::text, false, false, '{"retrieval":{"policy":"full-pass"}}')`, name)
				return err
			}
			assign := func(tx pgx.Tx) error {
				_, err := store.AssignTenantScopeTx(ctx, tx, ta, "ra-free", nil)
				return err
			}
			first, second := insertRow, assign
			if order == "assignment_first" {
				first, second = assign, insertRow
			}
			tx1, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx1.Rollback(ctx) }()
			if err := first(tx1); err != nil {
				t.Fatalf("first: %v", err)
			}
			done := make(chan error, 1)
			go func() {
				tx2, err := pool.Begin(context.Background())
				if err != nil {
					done <- err
					return
				}
				if err := second(tx2); err != nil {
					_ = tx2.Rollback(context.Background())
					done <- err
					return
				}
				done <- tx2.Commit(context.Background())
			}()
			if !lockWaitSeen(t, pool, func() bool { return len(done) == 0 }) {
				t.Errorf("the second write did not wait for the first")
			}
			// A reconcile between the two commits must not settle anything
			// for good.
			if err := tx1.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatalf("second: %v", err)
			}
			reconcile(t, pool)
			for _, id := range ids {
				if marker(t, pool, id) {
					t.Errorf("A's block %s stays marked although ra-free's lifting row is in A's registry now", id)
				}
			}
		})
	}
}

// TestEmbedExcludedOwnerCrash: the process dies between the queue entry and
// the reconcile (nothing ran), and in the middle of a reconcile batch (the
// batch statement is cancelled while it waits for a row lock, its
// transaction rolls back). Meanwhile the pick's predicate keeps the unmarked
// blocks out; the next pass of another process finishes the entry.
func TestEmbedExcludedOwnerCrash(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	reconcile(t, pool)
	ownerTenant(t, pool, "cr-a", "cr-a")
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_blocks (category, title, content, type_name, scope, sensitivity, updated_at)
		 SELECT 'own-cat', 'cr ' || i, 'x', 'cr-x', 'cr-a', 'public', now() FROM generate_series(1, 2500) i`); err != nil {
		t.Fatal(err)
	}
	createType(t, pool, "cr-x", "cr-a", "excluded")
	// Died before the reconcile: the entry is queued, nothing is marked, and
	// the pick does not return the blocks.
	var queued int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_embed_excluded_queue WHERE name = 'cr-x'`).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 1 || markedCount(t, pool, "cr-a", "cr-x") != 0 {
		t.Fatalf("fixture: queued %d, marked %d", queued, markedCount(t, pool, "cr-a", "cr-x"))
	}
	if got := eePick(t, pool, "cr-a", "public"); got != "" {
		t.Errorf("before any reconcile the pick returns an unmarked block of the excluded type: %s", got)
	}

	// Died in the middle of a batch: one block is held, the reconcile's batch
	// waits for it and the process goes away (the context is cancelled).
	hold, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hold.Rollback(ctx) }()
	if _, err := hold.Exec(ctx, `SELECT 1 FROM context_blocks WHERE title = 'cr 1500' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	rctx, cancel := context.WithCancel(ctx)
	rdone := make(chan error, 1)
	go func() {
		_, err := store.ReconcileEmbedExcluded(rctx, pool)
		rdone <- err
	}()
	if !lockWaitSeen(t, pool, func() bool { return len(rdone) == 0 }) {
		t.Fatalf("the reconcile batch did not wait for the held block")
	}
	cancel()
	if err := <-rdone; err == nil {
		t.Errorf("the cancelled reconcile reported no error")
	}
	if err := hold.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	mid := markedCount(t, pool, "cr-a", "cr-x")
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_embed_excluded_queue WHERE name = 'cr-x'`).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 1 {
		t.Errorf("after the death mid-batch the entry is not queued any more (%d)", queued)
	}
	if mid == 2500 {
		t.Errorf("fixture: the cancelled pass marked everything (%d)", mid)
	}
	res := reconcile(t, pool)
	if got := markedCount(t, pool, "cr-a", "cr-x"); got != 2500 {
		t.Errorf("after the next pass %d/2500 blocks are marked (%d before it, %+v)", got, mid, res)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_embed_excluded_queue`).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 0 {
		t.Errorf("%d entries left after the next pass", queued)
	}
}
