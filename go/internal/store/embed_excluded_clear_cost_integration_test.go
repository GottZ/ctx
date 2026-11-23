//go:build integration

// Issue #213, second review, F1: the cost of a tenant's queue entry in both
// directions of the reconcile, marking and clearing, against the marked
// blocks of the same name in another tenant.
//
//	go test -tags=integration ./internal/store/ -run 'TestEmbedExcludedTenantEntryCostBothDirections' -count=1 -v
package store_test

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// embedExcludedEntryBound is the bound of the cost pin below on the buffers
// of all statements of a tenant's entry: a few pages per statement on the
// ranges of the entry's own scope. A clear that reaches the scope's marked
// blocks through another index (the scope's whole range, or the name's
// marked blocks in every tenant on the definition of migration 175) reads
// thousands.
const embedExcludedEntryBound = 200

var indexNames = regexp.MustCompile(`"Index Name": "[^"]+"`)

// entryReplay reads the statements of a traced reconcile pass again, with
// the arguments they ran with, under every value of testdb.RandomPageCosts,
// on their custom and on their generic plans, and under the SET LOCAL
// settings of the traced transaction each ran in (the batches turn sorts
// off). A statement that changed rows in the pass finds none of them in its
// range any more, so the replay reads the range it scans.
type entryReplay struct {
	t     *testing.T
	pool  *pgxpool.Pool
	pools map[string]*pgxpool.Pool
}

func newEntryReplay(t *testing.T, pool *pgxpool.Pool) *entryReplay {
	return &entryReplay{t: t, pool: pool, pools: map[string]*pgxpool.Pool{}}
}

// planner returns the pool that plans with random_page_cost rpc and the given
// SET LOCAL settings, one per combination: a fresh connection reads the
// catalog on its first statements, which a replay would count.
func (r *entryReplay) planner(rpc string, local map[string]string) *pgxpool.Pool {
	settings := map[string]string{"random_page_cost": rpc}
	for k, v := range local {
		settings[k] = v
	}
	keys := make([]string, 0, len(settings))
	for k, v := range settings {
		keys = append(keys, k+"="+v)
	}
	sort.Strings(keys)
	key := strings.Join(keys, ",")
	if r.pools[key] == nil {
		r.pools[key] = testdb.WithSessionSettings(r.t, r.pool, settings)
	}
	return r.pools[key]
}

// buffers replays every traced statement on context_blocks that keep admits
// and returns the buffers per plan ("custom random_page_cost=4", …).
func (r *entryReplay) buffers(label string, traced []reviewStmt, keep func(sql string) bool) map[string]int64 {
	t, ctx := r.t, context.Background()
	t.Helper()
	sums := map[string]int64{}
	var local map[string]string
	for _, st := range traced {
		switch {
		case st.sql == "begin" || st.sql == "commit" || st.sql == "rollback":
			local = nil
			continue
		case strings.HasPrefix(st.sql, "SET LOCAL "):
			local = setLocals(t, st.sql)
			continue
		case !strings.Contains(st.sql, "context_blocks") || !keep(st.sql):
			continue
		}
		head := strings.Join(strings.Fields(st.sql), " ")
		if len(head) > 60 {
			head = head[:60]
		}
		for _, rpc := range testdb.RandomPageCosts {
			p := r.planner(rpc, local)
			// One unmeasured execution first, as the stats pin does: the rows
			// the pass changed have left the statement's range, so it writes
			// nothing.
			if _, err := p.Exec(ctx, st.sql, st.args...); err != nil {
				t.Fatalf("%s: warm-up: %v", label, err)
			}
			plans := map[string]testdb.PlanCost{"custom": testdb.ExplainCustom(t, p, st.sql, st.args...)}
			// The pass without a created_at is constant false since migration
			// 186 (NOT NULL): its plan names no parameter, so it has no
			// generic plan to read.
			if !strings.Contains(st.sql, "created_at IS NULL") {
				plans["generic"] = testdb.ExplainGeneric(t, p, st.sql, st.args...)
			}
			for form, c := range plans {
				plan := form + " random_page_cost=" + rpc
				sums[plan] += c.Buffers
				t.Logf("   %-60s %-26s %5d buffers on %v", head, plan, c.Buffers, indexNames.FindAllString(c.Plan, -1))
			}
		}
	}
	t.Logf("%s: buffers over the entry's statements %v", label, sums)
	return sums
}

// batchSettingsHook records, for every statement that updates embed_excluded,
// the sort settings it ran with — what the replay assumes from the traced
// SET LOCAL, observed where it takes effect.
func batchSettingsHook(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for _, st := range []string{
		`CREATE TABLE pin_batch_settings (sort text NOT NULL, incremental_sort text NOT NULL)`,
		`CREATE FUNCTION pin_batch_settings() RETURNS trigger LANGUAGE plpgsql AS $$
		 BEGIN
		     INSERT INTO pin_batch_settings VALUES (current_setting('enable_sort'), current_setting('enable_incremental_sort'));
		     RETURN NULL;
		 END $$`,
		`CREATE TRIGGER zz_pin_batch_settings AFTER UPDATE OF embed_excluded ON context_blocks
		 FOR EACH STATEMENT EXECUTE FUNCTION pin_batch_settings()`,
	} {
		if _, err := pool.Exec(context.Background(), st); err != nil {
			t.Fatalf("%s: %v", st, err)
		}
	}
}

// assertBatchesRanWithoutSorts checks that the reconcile pass since the last
// call ran batches and each of them with sorts off, and empties the record.
func assertBatchesRanWithoutSorts(t *testing.T, pool *pgxpool.Pool, label string) {
	t.Helper()
	var n, off int
	if err := pool.QueryRow(context.Background(),
		`WITH d AS (DELETE FROM pin_batch_settings RETURNING sort, incremental_sort)
		 SELECT count(*), count(*) FILTER (WHERE sort = 'off' AND incremental_sort = 'off') FROM d`).Scan(&n, &off); err != nil {
		t.Fatal(err)
	}
	if n == 0 || off != n {
		t.Errorf("%s: %d of %d batch statements ran with sorts off", label, off, n)
	}
}

// TestEmbedExcludedTenantEntryCostBothDirections: tenant B uses the name fc-x
// as its own excluded type and holds 10 000, then 100 000 marked blocks of
// it. Tenant A holds 20 000 blocks of a visible type with a vector and three
// blocks of fc-x, and writes its own row of the name: `excluded` (A's scopes
// answer true, the reconcile marks A's blocks), then `full-pass` (they answer
// false, the reconcile clears them). The buffers of every statement of A's
// entry, read again on their custom and on their generic plans, each under
// random_page_cost 1.1 and 4 (testdb.RandomPageCosts; at 4, a deployment's
// value, the batches walked the primary key and the whole scope before issue
// #513), stay flat in both directions and within embedExcludedEntryBound:
// A's entry reads the ranges of A's marked and pending blocks of the name and
// none of B's blocks. B's rows are not rewritten. Every batch statement of
// the pass ran with sorts off.
func TestEmbedExcludedTenantEntryCostBothDirections(t *testing.T) {
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	traced, tracer := r3Traced(t, dsn)
	ctx := context.Background()
	reconcile(t, pool)
	ownerTenant(t, pool, "fc-a", "fc-a")
	ownerTenant(t, pool, "fc-b", "fc-b")
	createType(t, pool, "fc-x", "fc-b", "excluded")
	ownerSeed(t, pool, "fc-a", "fc-x", 3)
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_blocks (category, title, content, type_name, scope, sensitivity, updated_at, embedding, embed_model)
		 SELECT 'fc', 'fc a ' || i, 'x', 'knowledge', 'fc-a', 'public', now(), array_fill(0.1::real, ARRAY[1024])::vector, 'test'
		   FROM generate_series(1, 20000) i`); err != nil {
		t.Fatal(err)
	}
	reconcile(t, pool)
	batchSettingsHook(t, pool)
	replay := newEntryReplay(t, pool)
	// entryCost returns the entry's buffers per plan (random_page_cost and
	// plan form).
	entryCost := func(label string) map[string]int64 {
		t.Helper()
		var grp string
		if err := pool.QueryRow(ctx, `SELECT string_agg(grp, ',') FROM context_embed_excluded_queue WHERE name = 'fc-x'`).Scan(&grp); err != nil {
			t.Fatalf("%s: A's entry: %v", label, err)
		}
		if want := "t:"; !strings.HasPrefix(grp, want) || strings.Contains(grp, ",") {
			t.Fatalf("%s: queued groups %q, want A's group alone", label, grp)
		}
		// The statements are read again after the pass, with the arguments
		// they ran with: the rows they changed have left their range, so the
		// replay reads the range each statement scans and none of A's three
		// rows it wrote. That range is what this pin is about: the writes of
		// A's three rows cost the same in every state, and what grows is the
		// other tenant's marked blocks a statement walks on its way. On
		// 7adaa9b0 the replayed clear read 2 775 -> 15 341 buffers, and the
		// mutants that take the scope out of the clear's index access or
		// leave the index of 175 in place are red here.
		tracer.take()
		if _, err := pool.Exec(ctx, `DELETE FROM pin_batch_settings`); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ReconcileEmbedExcluded(ctx, traced); err != nil {
			t.Fatalf("%s: reconcile: %v", label, err)
		}
		assertBatchesRanWithoutSorts(t, pool, label)
		return replay.buffers(label, tracer.take(), func(string) bool { return true })
	}
	var row *store.BlockTypeRow
	var marks, clears []map[string]int64
	from := 0
	for _, n := range []int{10_000, 100_000} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blocks (category, title, content, type_name, scope, sensitivity, updated_at)
			 SELECT 'fc', 'fc ' || i, 'x', 'fc-x', 'fc-b', 'public', now() FROM generate_series($1::int + 1, $2::int) i`, from, n); err != nil {
			t.Fatal(err)
		}
		from = n
		if _, err := pool.Exec(ctx, `ANALYZE context_blocks`); err != nil {
			t.Fatal(err)
		}
		reconcile(t, pool)
		if got := markedCount(t, pool, "fc-b", "fc-x"); got != n {
			t.Fatalf("fixture: %d/%d of B's fc-x marked", got, n)
		}
		stB := scopeState(t, pool, "fc-b")

		if row == nil {
			row = createType(t, pool, "fc-x", "fc-a", "excluded")
		} else {
			updateType(t, pool, row.ID, "fc-a", "excluded")
		}
		marks = append(marks, entryCost(fmt.Sprintf("mark: A's fc-x excluded, B holds %d marked", n)))
		if got := markedCount(t, pool, "fc-a", "fc-x"); got != 3 {
			t.Errorf("%d/3 of A's fc-x marked after A's excluded row", got)
		}

		updateType(t, pool, row.ID, "fc-a", "full-pass")
		clears = append(clears, entryCost(fmt.Sprintf("clear: A's fc-x full-pass, B holds %d marked", n)))
		if got := markedCount(t, pool, "fc-a", "fc-x"); got != 0 {
			t.Errorf("%d/3 of A's fc-x still marked after A's full-pass row", got)
		}
		if scopeState(t, pool, "fc-b") != stB {
			t.Errorf("A's entries rewrote B's rows")
		}
	}
	for _, d := range []struct {
		dir  string
		sums []map[string]int64
	}{{"mark", marks}, {"clear", clears}} {
		for plan := range d.sums[0] {
			small, large := d.sums[0][plan], d.sums[1][plan]
			if large > small+small/2+50 {
				t.Errorf("%s (%s): A's entry reads %d buffers with 10 000 marked blocks of B and %d with 100 000: it walks B's rows",
					d.dir, plan, small, large)
			}
			for _, sum := range []int64{small, large} {
				if sum > embedExcludedEntryBound {
					t.Errorf("%s (%s): A's entry reads %d buffers, above %d: it does not read its scope's range of the name",
						d.dir, plan, sum, embedExcludedEntryBound)
				}
			}
		}
	}
}

// TestEmbedExcludedStaleCostAgainstForeignStale: tenant B holds 2 000, then
// 20 000 blocks of the name sc-x whose vectors are stale (B has no row of the
// name, so they stay stale), tenant A three stale blocks of it beside 20 000
// blocks of a visible type, and A writes an `excluded` row of the name (the
// first mark drops A's stale vectors; for the second size the row is lifted
// and excluded again). The stale statements of A's entry (the stale pass and
// the mark's left check),
// read again under random_page_cost 1.1 and 4, custom and generic plan, stay
// flat and within embedExcludedEntryBound: they read A's scope's range of
// idx_embedding_stale_scope_sens, not B's stale blocks of the name (before,
// 488 -> 6 177 buffers at 10 000 -> 100 000 of B's). B's rows are not
// rewritten; every batch ran with sorts off.
func TestEmbedExcludedStaleCostAgainstForeignStale(t *testing.T) {
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	traced, tracer := r3Traced(t, dsn)
	ctx := context.Background()
	reconcile(t, pool)
	ownerTenant(t, pool, "sc-a", "sc-a")
	ownerTenant(t, pool, "sc-b", "sc-b")
	for i := 0; i < 3; i++ {
		staleFixture(t, pool, "sc-a", fmt.Sprintf("sc stale %d", i), "sc-x")
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_blocks (category, title, content, type_name, scope, sensitivity, updated_at, embedding, embed_model)
		 SELECT 'sc', 'sc a ' || i, 'x', 'knowledge', 'sc-a', 'public', now(), array_fill(0.1::real, ARRAY[1024])::vector, 'test'
		   FROM generate_series(1, 20000) i`); err != nil {
		t.Fatal(err)
	}
	reconcile(t, pool)
	batchSettingsHook(t, pool)
	replay := newEntryReplay(t, pool)
	stale := func(sql string) bool { return strings.Contains(sql, "embedding_stale AND NOT is_archived") }
	var row *store.BlockTypeRow
	var sums []map[string]int64
	from := 0
	for _, n := range []int{2_000, 20_000} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blocks (category, title, content, type_name, scope, sensitivity, updated_at, embedding, embed_model, embedding_stale)
			 SELECT 'sc', 'sc b ' || i, 'x', 'sc-x', 'sc-b', 'public', now(), array_fill(0.1::real, ARRAY[1024])::vector, 'test', true
			   FROM generate_series($1::int + 1, $2::int) i`, from, n); err != nil {
			t.Fatal(err)
		}
		from = n
		if _, err := pool.Exec(ctx, `ANALYZE context_blocks`); err != nil {
			t.Fatal(err)
		}
		var staleB, staleA int
		if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE scope = 'sc-b'), count(*) FILTER (WHERE scope = 'sc-a')
			FROM context_blocks WHERE embedding_stale AND NOT is_archived AND type_name = 'sc-x'`).Scan(&staleB, &staleA); err != nil {
			t.Fatal(err)
		}
		if staleB != n || (row == nil && staleA != 3) {
			t.Fatalf("fixture: %d/%d of B's and %d/3 of A's sc-x blocks stale", staleB, n, staleA)
		}
		stB := scopeState(t, pool, "sc-b")
		if row == nil {
			row = createType(t, pool, "sc-x", "sc-a", "excluded")
		} else {
			// A's three blocks lost their stale vectors to the first mark;
			// lifted and excluded again they are pending, and the stale pass
			// reads A's (now empty) stale range of the name
			updateType(t, pool, row.ID, "sc-a", "full-pass")
			reconcile(t, pool)
			if _, err := pool.Exec(ctx, `DELETE FROM pin_batch_settings`); err != nil {
				t.Fatal(err)
			}
			updateType(t, pool, row.ID, "sc-a", "excluded")
		}
		label := fmt.Sprintf("mark: A's sc-x excluded, B holds %d stale", n)
		tracer.take()
		if _, err := store.ReconcileEmbedExcluded(ctx, traced); err != nil {
			t.Fatalf("%s: reconcile: %v", label, err)
		}
		assertBatchesRanWithoutSorts(t, pool, label)
		sums = append(sums, replay.buffers(label, tracer.take(), stale))
		if got := markedCount(t, pool, "sc-a", "sc-x"); got != 3 {
			t.Errorf("%s: %d/3 of A's sc-x blocks marked", label, got)
		}
		if scopeState(t, pool, "sc-b") != stB {
			t.Errorf("%s: A's entry rewrote B's rows", label)
		}
	}
	for plan := range sums[0] {
		small, large := sums[0][plan], sums[1][plan]
		if large > small+small/2+50 {
			t.Errorf("stale (%s): A's entry reads %d buffers with 2 000 stale blocks of B and %d with 20 000: it walks B's rows", plan, small, large)
		}
		for _, sum := range []int64{small, large} {
			if sum > embedExcludedEntryBound {
				t.Errorf("stale (%s): A's entry reads %d buffers, above %d: it does not read its scope's range of the name", plan, sum, embedExcludedEntryBound)
			}
		}
	}
}

// setLocals parses a traced "SET LOCAL name = value; SET LOCAL ..." into its
// settings.
func setLocals(t *testing.T, sql string) map[string]string {
	t.Helper()
	m := map[string]string{}
	for _, part := range strings.Split(sql, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kv := strings.SplitN(strings.TrimPrefix(part, "SET LOCAL "), "=", 2)
		if len(kv) != 2 || !strings.HasPrefix(part, "SET LOCAL ") {
			t.Fatalf("traced setting %q: not SET LOCAL name = value", part)
		}
		m[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
	}
	return m
}
