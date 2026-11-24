//go:build integration

package store_test

// Cost gate of issue #237: a browse page and a recent page read the page, not
// the reader's scopes and not other tenants' rows.
//
// Before migration 195 both statements carried `scope = ANY($1) OR id =
// ANY($grants)` and an ORDER BY updated_at that no index delivered: every
// visible row was read and sorted for the LIMIT, so a page of a reader of
// 184 000 rows read 157 848-201 391 buffers at 1 000 000 rows (docs/
// operations.md, "Migration 195"). The gate takes the statement production
// sends (traced from the store functions), on two stores alike but for size
// (the second ten times the first), and EXPLAINs it at random_page_cost 1.1
// and 4, custom and generic plan. Per page it allows each read scope its
// rows plus a descent (listArmBound), a constant for the plan's top and four
// buffers per granted block (the grant arm reads them by primary key), and
// from the small store to the ten times larger one a growth of two buffers
// per scope and one per granted block (an index one level deeper). A small
// reader (300 rows) beside the other tenants' rows is held to the same: its
// cost must not follow their rows — the property an index without the scope
// column would lose (rejected in the issue: its generic plan walked every
// tenant's rows newer than the reader's page).

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// pageCostRows is the size of the small store; the large one has ten times as
// many rows. 1 in 5 rows is in pc-big, 300 in pc-small, the rest spread over
// 60 other scopes; 1 in 13 archived.
const pageCostRows = 12000

// listArmBound is what one arm of the walk may read for a page of n rows:
// its n rows and the descent to them.
func listArmBound(n int) int64 { return int64(n) + 4 }

// pageCostSeed fills a store of rows rows through COPY (the production
// triggers run; the per-row path of the API would take minutes at the large
// size). Timestamps step by one second per 4 rows, so they tie.
func pageCostSeed(rows int) func(ctx context.Context, pool *pgxpool.Pool) error {
	return func(ctx context.Context, pool *pgxpool.Pool) error {
		newest := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
		smallEvery := rows / 300
		const workers = 4
		var wg sync.WaitGroup
		errs := make(chan error, workers)
		for wk := 0; wk < workers; wk++ {
			wg.Add(1)
			go func(wk int) {
				defer wg.Done()
				conn, err := pool.Acquire(ctx)
				if err != nil {
					errs <- err
					return
				}
				defer conn.Release()
				next := wk
				_, err = conn.Conn().CopyFrom(ctx, pgx.Identifier{"context_blocks"},
					[]string{"category", "title", "content", "scope", "type_name", "is_archived", "created_at", "updated_at", "metadata", "tags"},
					pgx.CopyFromFunc(func() ([]any, error) {
						if next >= rows {
							return nil, nil
						}
						i := next
						next += workers
						ts := newest.Add(-time.Duration(i/4) * time.Second)
						scope := fmt.Sprintf("pc-o%02d", (i*7)%60)
						switch {
						case i%smallEvery == 0:
							scope = "pc-small"
						case (i*2654435761)%5 == 0:
							scope = "pc-big"
						}
						typ := "knowledge"
						if i%7 == 3 {
							typ = "checkpoint"
						}
						return []any{fmt.Sprintf("cat-%d", i%20), fmt.Sprintf("pc row %d", i),
							fmt.Sprintf("%s %d", "lorem ipsum dolor sit amet consectetur adipiscing elit sed do eiusmod tempor incididunt ut labore et dolore magna aliqua", i),
							scope, typ, i%13 == 0, ts, ts, map[string]any{}, []string{"note"}}, nil
					}))
				if err != nil {
					errs <- fmt.Errorf("worker %d: %w", wk, err)
				}
			}(wk)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			return err
		}
		_, err := pool.Exec(ctx, `VACUUM (ANALYZE) context_blocks`)
		return err
	}
}

// pageCostCase is one listing a reader sends.
type pageCostCase struct {
	name   string
	scopes []string
	grants bool
	deep   bool
	kind   string // compact, full, recent
}

// pageCostStatement calls the store function of c on tp, a pool on pool's
// database whose statements tr records, and returns the statement it sent and
// the rows it answers per page.
func pageCostStatement(t *testing.T, pool, tp *pgxpool.Pool, tr *listTracer, c pageCostCase) (string, []any, int) {
	t.Helper()
	ctx := context.Background()
	var grants []string
	if c.grants {
		rows, err := pool.Query(ctx, `SELECT id::text FROM context_blocks WHERE scope = 'pc-o07' ORDER BY id LIMIT 20`)
		if err != nil {
			t.Fatal(err)
		}
		grants, err = pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
	}
	var after *store.SearchCursor
	if c.deep {
		after = &store.SearchCursor{}
		if err := pool.QueryRow(ctx, `SELECT updated_at, id::text FROM context_blocks WHERE NOT is_archived AND scope = ANY($1)
			ORDER BY updated_at DESC, id DESC OFFSET (SELECT count(*) * 4 / 5 FROM context_blocks WHERE NOT is_archived AND scope = ANY($1)) LIMIT 1`,
			c.scopes).Scan(&after.UpdatedAt, &after.ID); err != nil {
			t.Fatalf("deep cursor: %v", err)
		}
	}
	var err error
	n := 10
	switch c.kind {
	case "compact":
		_, _, err = store.SearchBlocksPage(ctx, tp, nil, "", c.scopes, "", nil, nil, 10, true, after, grants, nil, nil, nil)
		n = 11 // the probe row
	case "full":
		_, err = store.SearchBlocks(ctx, tp, nil, "", c.scopes, "", nil, 10, false, after, grants, nil, nil, nil)
	case "recent":
		_, err = store.RecentBlocks(ctx, tp, nil, c.scopes, "", 10, nil, nil, grants)
	}
	if err != nil {
		t.Fatalf("%s: %v", c.name, err)
	}
	sql, args := tr.last(t)
	return sql, args, n
}

func TestBrowseRecentPageCostIndependentOfTheStore(t *testing.T) {
	small := testdb.SetupSeededTestDB(t, fmt.Sprintf("i237-pagecost-%d", pageCostRows), pageCostSeed(pageCostRows))
	large := testdb.SetupSeededTestDB(t, fmt.Sprintf("i237-pagecost-%d", 10*pageCostRows), pageCostSeed(10*pageCostRows))
	trS, trL := &listTracer{}, &listTracer{}
	tpS, tpL := listTracedPool(t, small, trS), listTracedPool(t, large, trL)
	three := []string{"pc-o01", "pc-o02", "pc-o03"}
	fifty := make([]string, 50)
	for i := range fifty {
		fifty[i] = fmt.Sprintf("pc-o%02d", i)
	}
	var cases []pageCostCase
	for _, kind := range []string{"compact", "full", "recent"} {
		for _, r := range []struct {
			name   string
			scopes []string
			grants bool
		}{
			{"big", []string{"pc-big"}, false},
			{"small", []string{"pc-small"}, false},
			{"three", three, false},
			{"fifty", fifty, false},
			{"big with grants", []string{"pc-big"}, true},
		} {
			cases = append(cases, pageCostCase{name: kind + " " + r.name, scopes: r.scopes, grants: r.grants, kind: kind})
			if kind != "recent" {
				cases = append(cases, pageCostCase{name: kind + " " + r.name + " deep page", scopes: r.scopes, grants: r.grants, kind: kind, deep: true})
			}
		}
	}
	for _, rpc := range testdb.RandomPageCosts {
		sp, lp := testdb.WithRandomPageCost(t, small, rpc), testdb.WithRandomPageCost(t, large, rpc)
		for _, c := range cases {
			for _, plan := range []string{"custom", "generic"} {
				explain := testdb.ExplainCustom
				if plan == "generic" {
					explain = testdb.ExplainGeneric
				}
				sqlS, argsS, n := pageCostStatement(t, small, tpS, trS, c)
				sqlL, argsL, _ := pageCostStatement(t, large, tpL, trL, c)
				s, l := explain(t, sp, sqlS, argsS...), explain(t, lp, sqlL, argsL...)
				bound := int64(len(c.scopes))*listArmBound(n) + 16
				if c.grants {
					bound += 20 * 4 // the grant arm: 20 granted blocks by primary key
				}
				grow := s.Buffers + 2*int64(len(c.scopes)) + 2
				if c.grants {
					grow += 20 // a primary key one level deeper per granted block
				}
				t.Logf("%s (rpc %s, %s plan): %d buffers at %d rows, %d at %d rows; bound %d, growth bound %d",
					c.name, rpc, plan, s.Buffers, pageCostRows, l.Buffers, 10*pageCostRows, bound, grow)
				if s.Buffers > bound || l.Buffers > bound {
					t.Errorf("%s (rpc %s, %s plan): a page reads %d / %d buffers (%d / %d rows), more than %d (%d scopes × %d + 16): it reads more than its page",
						c.name, rpc, plan, s.Buffers, l.Buffers, pageCostRows, 10*pageCostRows, bound, len(c.scopes), listArmBound(n))
				}
				if l.Buffers > grow {
					t.Errorf("%s (rpc %s, %s plan): %d buffers at %d rows against %d at %d rows: the page's cost follows the store",
						c.name, rpc, plan, l.Buffers, 10*pageCostRows, s.Buffers, pageCostRows)
				}
			}
		}
	}
}

// listTracer records the statements a pool sends.
type listTracer struct {
	mu   sync.Mutex
	sql  []string
	args [][]any
}

func (l *listTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	l.mu.Lock()
	l.sql = append(l.sql, d.SQL)
	l.args = append(l.args, d.Args)
	l.mu.Unlock()
	return ctx
}

func (l *listTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// last returns the last statement that reads context_blocks.
func (l *listTracer) last(t *testing.T) (string, []any) {
	t.Helper()
	return l.lastOn(t, "FROM context_blocks")
}

// lastOn returns the last statement whose text holds from (the catalog
// reading of the listings aside).
func (l *listTracer) lastOn(t *testing.T, from string) (string, []any) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := len(l.sql) - 1; i >= 0; i-- {
		if strings.Contains(l.sql[i], from) && !strings.Contains(l.sql[i], "pg_index") {
			return l.sql[i], l.args[i]
		}
	}
	t.Fatalf("no statement %q traced", from)
	return "", nil
}

// listTracedPool is a pool on pool's database whose statements tr records.
func listTracedPool(t *testing.T, pool *pgxpool.Pool, tr *listTracer) *pgxpool.Pool {
	t.Helper()
	cfg := pool.Config()
	cfg.ConnConfig.Tracer = tr
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}
