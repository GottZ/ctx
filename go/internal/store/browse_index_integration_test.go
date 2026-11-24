//go:build integration

package store_test

// Issue #237: browse (the empty-query path of the compact and the full
// search) and recent read their page through one LATERAL arm per read scope
// over idx_blocks_scope_updated (migration 195) plus one arm for the granted
// blocks outside the read scopes. These tests hold that the walk answers the
// rows of the statements before 195 in their order, that its cost is the
// page's and not the store's, and that the listings send the statements
// before 195 where the walk could read past its page or the index does not
// stand.
//
//	go test -tags=integration ./internal/store/ -run 'TestListWalk|TestBrowseRecent' -count=1 -v

import (
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// listWalkFixture is a store written through the production write path:
// singles and batches (one transaction, one updated_at for every row of the
// batch, so timestamps tie in mass, within a scope and across scopes), three
// types, archived blocks, and blocks of a scope no reader reads, whose ids
// serve as grants.
type listWalkFixture struct {
	pool *pgxpool.Pool
	// granted: blocks of the foreign scope (archived ones among them) and of
	// the read scope a.
	grantsOutside, grantsInside []string
}

func listWalkSeed(t *testing.T, pool *pgxpool.Pool) listWalkFixture {
	t.Helper()
	ctx := context.Background()
	rng := rand.New(rand.NewPCG(237, 1))
	typ := func(i int) string {
		switch {
		case i%7 == 3:
			return "checkpoint"
		case i%5 == 1:
			return "reference"
		default:
			return "knowledge"
		}
	}
	var archive []string
	single := func(scope string, i int) string {
		b, err := store.UpsertBlock(ctx, pool, fmt.Sprintf("cat-%d", i%4), fmt.Sprintf("%s single %d", scope, i),
			fmt.Sprintf("body %s %d", scope, i), []string{fmt.Sprintf("tag-%d", i%3)}, nil, scope, true, store.SensitivityWrite{}, typ(i))
		if err != nil {
			t.Fatalf("write %s %d: %v", scope, i, err)
		}
		if i%13 == 5 {
			archive = append(archive, b.ID)
		}
		return b.ID
	}
	batch := func(name string, scopes []string, n int) []string {
		items := make([]store.BatchUpsert, n)
		for i := range items {
			items[i] = store.BatchUpsert{Category: fmt.Sprintf("cat-%d", i%4), Title: fmt.Sprintf("%s batch %d", name, i),
				Content: fmt.Sprintf("body %s %d", name, i), Tags: []string{fmt.Sprintf("tag-%d", i%3)},
				Scope: scopes[i%len(scopes)], ScopeExplicit: true, TypeName: typ(i)}
		}
		res, err := store.UpsertBlocks(ctx, pool, items, "", nil, nil)
		if err != nil {
			t.Fatalf("batch %s: %v", name, err)
		}
		ids := make([]string, 0, n)
		for i, r := range res {
			ids = append(ids, r.Block.ID)
			if i%11 == 4 {
				archive = append(archive, r.Block.ID)
			}
		}
		return ids
	}
	var f listWalkFixture
	f.pool = pool
	// Interleaved: singles of every scope, then batches, then singles again,
	// so each scope's range holds ties at several places of its order.
	for i := 0; i < 120; i++ {
		single("lw-a", i)
		if i%2 == 0 {
			single("lw-b", i)
		}
		if i%5 == 0 {
			single("lw-c", i)
		}
		if i%3 == 0 {
			id := single("lw-foreign", i)
			if len(f.grantsOutside) < 12 {
				f.grantsOutside = append(f.grantsOutside, id)
			}
		}
		if i == 40 {
			f.grantsInside = batch("a-only", []string{"lw-a"}, 60)[:6]
		}
		if i == 80 {
			batch("across", []string{"lw-a", "lw-b", "lw-c", "lw-foreign"}, 48)
		}
	}
	for range rng.IntN(3) {
		batch("tail", []string{"lw-b"}, 5)
	}
	for _, id := range archive {
		if _, err := store.DeleteBlock(ctx, pool, id, []string{"lw-a", "lw-b", "lw-c", "lw-foreign"}); err != nil {
			t.Fatalf("archive %s: %v", id, err)
		}
	}
	// A granted block that is archived must stay out (the arm's NOT
	// is_archived): archive two of the outside grants.
	for _, id := range f.grantsOutside[:2] {
		if _, err := store.DeleteBlock(ctx, pool, id, []string{"lw-foreign"}); err != nil {
			t.Fatalf("archive grant %s: %v", id, err)
		}
	}
	if _, err := pool.Exec(ctx, `ANALYZE context_blocks`); err != nil {
		t.Fatal(err)
	}
	return f
}

// listRows runs sql and renders every column of every row.
func listRows(t *testing.T, pool *pgxpool.Pool, sql string, args []any) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), sql, args...)
	if err != nil {
		t.Fatalf("%v\n%s", err, sql)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		v, err := rows.Values()
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%v", v))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%v\n%s", err, sql)
	}
	return out
}

// listKeys is the visible listing of scopes and grants in the browse order,
// as (updated_at, id): the cursors of the raster are taken from it.
func listKeys(t *testing.T, pool *pgxpool.Pool, scopes, grants []string) []store.SearchCursor {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT updated_at, id::text FROM context_blocks
		WHERE NOT is_archived AND (scope = ANY($1::text[]) OR id = ANY($2::uuid[])) ORDER BY updated_at DESC, id DESC`, scopes, grants)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (store.SearchCursor, error) {
		var c store.SearchCursor
		return c, r.Scan(&c.UpdatedAt, &c.ID)
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestListWalkSameRowsAsTheStatementsBefore195 compares, page by page, the
// walk (the statement production sends while idx_blocks_scope_updated
// stands) with the statement before 195 over a raster of read scopes (one,
// three, a scope without rows, a scope named twice, an empty list), grants
// (none, outside the read scopes with archived ones among them, inside the
// read scopes), type exclusions, limits, the probe row and cursors (every
// page of a full paging, random rows of the listing, a tie, a position
// between rows), for the compact and the full browse and for recent — with
// the index and again after the index is dropped.
func TestListWalkSameRowsAsTheStatementsBefore195(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	f := listWalkSeed(t, pool)
	var ties int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM (SELECT updated_at FROM context_blocks
		WHERE NOT is_archived AND scope LIKE 'lw-%' GROUP BY scope, updated_at HAVING count(*) > 1) x`).Scan(&ties); err != nil || ties < 3 {
		t.Fatalf("fixture: %d (scope, updated_at) ties, want at least 3 (err %v)", ties, err)
	}

	scopeSets := [][]string{
		{"lw-a"}, {"lw-b"}, {"lw-c"}, {"lw-none"},
		{"lw-a", "lw-b", "lw-c"}, {"lw-c", "lw-a"}, {"lw-a", "lw-a", "lw-b"}, {"lw-b", "lw-none"}, {},
	}
	grantSets := [][]string{nil, f.grantsOutside, f.grantsInside, append(slices.Clone(f.grantsOutside), f.grantsInside...)}
	excludes := [][]string{nil, {"checkpoint"}, {"knowledge", "reference"}, {"knowledge", "reference", "checkpoint"}}
	kinds := []string{"compact", "full", "recent"}
	categories := []string{"", "", "cat-1", "cat-none"}

	type kase struct {
		kind     string
		category string
		scopes   []string
		grants   []string
		excl     []string
		limit    int
		probe    bool
		after    *store.SearchCursor
	}
	stmts := func(c kase) (base, walk string, baseArgs, walkArgs []any) {
		if c.kind == "recent" {
			base, baseArgs = store.RecentStatementForTest(c.scopes, c.category, c.limit, nil, c.excl, nonNil(c.grants), false, false)
			walk, walkArgs = store.RecentStatementForTest(c.scopes, c.category, c.limit, nil, c.excl, nonNil(c.grants), true, true)
			return
		}
		p := store.SearchBlocksParams{ReadScopes: c.scopes, Category: c.category, Limit: c.limit, Compact: c.kind == "compact",
			After: c.after, GrantedBlockIDs: c.grants, TypesExclude: c.excl, ProbeNext: c.probe}
		base, baseArgs = store.SearchStatementForTest(p, false, false)
		walk, walkArgs = store.SearchStatementForTest(p, true, true)
		return
	}
	pages := 0
	check := func(t *testing.T, c kase) []string {
		t.Helper()
		base, walk, ba, wa := stmts(c)
		if base == walk {
			t.Fatalf("%+v: the walk is the statement before 195", c)
		}
		want := listRows(t, pool, base, ba)
		got := listRows(t, pool, walk, wa)
		pages++
		if !slices.Equal(got, want) {
			t.Errorf("%s scopes %v category %q grants %d excl %v limit %d probe %v after %+v:\n walk   %d rows %v\n before %d rows %v",
				c.kind, c.scopes, c.category, len(c.grants), c.excl, c.limit, c.probe, c.after, len(got), got, len(want), want)
		}
		return want
	}

	run := func(t *testing.T) {
		pages = 0
		rng := rand.New(rand.NewPCG(237, 2))
		// Full pagings: every page of every listing at a small page size.
		for _, scopes := range scopeSets {
			for _, grants := range grantSets[:2] {
				for _, kind := range []string{"compact", "full"} {
					keys := listKeys(t, pool, scopes, nonNil(grants))
					var after *store.SearchCursor
					for page := 0; page <= len(keys)/7+1; page++ {
						c := kase{kind: kind, scopes: scopes, grants: grants, limit: 7, probe: true, after: after}
						rows := check(t, c)
						if len(rows) < 8 {
							break
						}
						k := keys[(page+1)*7-1]
						after = &k
					}
				}
			}
		}
		// Random pages.
		for range 400 {
			scopes := scopeSets[rng.IntN(len(scopeSets))]
			grants := grantSets[rng.IntN(len(grantSets))]
			c := kase{kind: kinds[rng.IntN(len(kinds))], category: categories[rng.IntN(len(categories))], scopes: scopes, grants: grants,
				excl: excludes[rng.IntN(len(excludes))], limit: []int{1, 2, 3, 10, 50}[rng.IntN(5)], probe: rng.IntN(2) == 0}
			if c.kind != "recent" {
				keys := listKeys(t, pool, scopes, nonNil(grants))
				switch n := rng.IntN(4); {
				case n == 1 && len(keys) > 0:
					k := keys[rng.IntN(len(keys))]
					c.after = &k
				case n == 2 && len(keys) > 0:
					// A position between rows: the timestamp of a row, an id
					// no row has.
					k := keys[rng.IntN(len(keys))]
					c.after = &store.SearchCursor{UpdatedAt: k.UpdatedAt, ID: "7fffffff-ffff-7fff-bfff-ffffffffffff"}
				case n == 3:
					c.after = &store.SearchCursor{UpdatedAt: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), ID: "00000000-0000-0000-0000-000000000000"}
				}
			}
			check(t, c)
		}
		t.Logf("%d pages compared", pages)
		if pages < 200 {
			t.Errorf("%d pages compared, want at least 200", pages)
		}
	}
	t.Run("with_index", run)
	for _, ix := range []string{"idx_blocks_scope_updated", "idx_blocks_scope_category_updated"} {
		if _, err := pool.Exec(context.Background(), `DROP INDEX `+ix); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("without_index", run)
}

// TestListWalkRouting holds which listings walk: browse without a query and
// recent, with no filter but a category and the type exclusion, while the
// index their arms read stands (idx_blocks_scope_category_updated with a
// category, idx_blocks_scope_updated without); every other listing sends the
// statement before 195, byte for byte.
func TestListWalkRouting(t *testing.T) {
	cluster := "019a0000-0000-7000-8000-000000000237"
	empty := ""
	scopes := []string{"a", "b"}
	type ix struct{ scope, category bool }
	all := []ix{{false, false}, {true, false}, {false, true}, {true, true}}
	for _, c := range []struct {
		name  string
		p     store.SearchBlocksParams
		walks func(ix) bool
	}{
		{"browse", store.SearchBlocksParams{ReadScopes: scopes, Compact: true}, func(i ix) bool { return i.scope }},
		{"browse full", store.SearchBlocksParams{ReadScopes: scopes}, func(i ix) bool { return i.scope }},
		{"browse probe cursor", store.SearchBlocksParams{ReadScopes: scopes, Compact: true, ProbeNext: true, After: &store.SearchCursor{UpdatedAt: time.Now(), ID: cluster}}, func(i ix) bool { return i.scope }},
		{"browse types_exclude", store.SearchBlocksParams{ReadScopes: scopes, Compact: true, TypesExclude: []string{"checkpoint"}}, func(i ix) bool { return i.scope }},
		{"browse empty cluster", store.SearchBlocksParams{ReadScopes: scopes, Compact: true, Cluster: &empty}, func(i ix) bool { return i.scope }},
		{"category", store.SearchBlocksParams{ReadScopes: scopes, Compact: true, Category: "c"}, func(i ix) bool { return i.category }},
		{"category types_exclude", store.SearchBlocksParams{ReadScopes: scopes, Category: "c", TypesExclude: []string{"checkpoint"}}, func(i ix) bool { return i.category }},
		{"full text", store.SearchBlocksParams{ReadScopes: scopes, Compact: true, Query: "x"}, func(ix) bool { return false }},
		{"tags", store.SearchBlocksParams{ReadScopes: scopes, Compact: true, Tags: []string{"t"}}, func(ix) bool { return false }},
		{"tags_all", store.SearchBlocksParams{ReadScopes: scopes, Compact: true, TagsAll: []string{"t"}}, func(ix) bool { return false }},
		{"types", store.SearchBlocksParams{ReadScopes: scopes, Compact: true, Types: []string{"knowledge"}}, func(ix) bool { return false }},
		{"category types", store.SearchBlocksParams{ReadScopes: scopes, Compact: true, Category: "c", Types: []string{"knowledge"}}, func(ix) bool { return false }},
		{"cluster", store.SearchBlocksParams{ReadScopes: scopes, Compact: true, Cluster: &cluster}, func(ix) bool { return false }},
	} {
		before, _ := store.SearchStatementForTest(c.p, false, false)
		if strings.Contains(before, "CROSS JOIN LATERAL") {
			t.Errorf("%s: without the indexes the statement walks", c.name)
		}
		for _, i := range all {
			got, _ := store.SearchStatementForTest(c.p, i.scope, i.category)
			if walks := strings.Contains(got, "CROSS JOIN LATERAL"); walks != c.walks(i) {
				t.Errorf("%s (indexes %+v): walks %v, want %v", c.name, i, walks, c.walks(i))
			}
			if !c.walks(i) && got != before {
				t.Errorf("%s (indexes %+v): the statement is not the one before 195", c.name, i)
			}
		}
	}
	for _, c := range []struct {
		name     string
		category string
		types    []string
		excl     []string
		walks    func(ix) bool
	}{
		{"recent", "", nil, nil, func(i ix) bool { return i.scope }},
		{"recent types_exclude", "", nil, []string{"checkpoint"}, func(i ix) bool { return i.scope }},
		{"recent category", "c", nil, nil, func(i ix) bool { return i.category }},
		{"recent types", "", []string{"knowledge"}, nil, func(ix) bool { return false }},
	} {
		before, _ := store.RecentStatementForTest(scopes, c.category, 10, c.types, c.excl, nil, false, false)
		for _, i := range all {
			got, _ := store.RecentStatementForTest(scopes, c.category, 10, c.types, c.excl, nil, i.scope, i.category)
			if walks := strings.Contains(got, "CROSS JOIN LATERAL"); walks != c.walks(i) {
				t.Errorf("%s (indexes %+v): walks %v, want %v", c.name, i, walks, c.walks(i))
			}
			if !c.walks(i) && got != before {
				t.Errorf("%s (indexes %+v): the statement is not the one before 195", c.name, i)
			}
		}
	}
}
