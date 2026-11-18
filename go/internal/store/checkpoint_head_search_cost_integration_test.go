//go:build integration

// Head search of the Hermes checkpoint chain with and without a type filter
// (issue #342). The ctx_checkpoint plugin finds the head of a chain by full
// text on its title, and ctx_handover by full text on the title alone; until
// #342 both sent no type filter over MCP, and both send types=["checkpoint"]
// since. The type is on the FTS deny-list of the partial GIN indexes
// (impliesHardFTSDeny is false for it), so no index premise is declared and the
// type filter can only change the access path the planner picks for the other
// quals. Measured (TestCheckpointHeadSearchMeasure, 100 000 rows): the
// ctx_checkpoint form reads 809 buffers without the filter and 809 with it under
// a generic plan, 871 under a custom plan (the planner adds the type index as a
// third bitmap arm); the ctx_handover form as it was before #342 (title words
// only) reads a third less with it, and with the head tag it sends since #342
// it reads what the ctx_checkpoint form reads. The gate
// holds the filter within 10 % (+16 buffers) of the search without it, so a
// change of indexes or statement that turns the typed search into a walk over
// the checkpoint rows shows here.
//
// The fixture is the reference instance's shape (unrankedSeed: 57 % checkpoint
// parts of 35 000 characters, interleaved in time with ranked blocks) plus one
// head per root session that has parts, written as the plugin writes it
// (category compaction-checkpoints, type checkpoint, tag checkpoint-head).
//
// TestCheckpointHeadSearchCost is the gate (small store, runs in CI);
// TestCheckpointHeadSearchMeasure measures the three search forms on larger
// stores, custom and generic plan, one warm run and five measured runs each:
//
//	CTX_HEAD_MEASURE=10000,100000 go test -tags=integration -p 1 ./internal/store/ -run TestCheckpointHeadSearchMeasure -count=1 -v -timeout 3h
package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// headPartsPerRoot is the number of parts unrankedSeed files under one root
// session (its titles read "part NNN of 034", root = position / 34).
const headPartsPerRoot = 34

// headPreferredRoot is the root whose head the searches look for when the
// fixture has one; smaller fixtures take their middle root instead.
const headPreferredRoot = 123

// headShape is the measured store shape at n rows: 57 % checkpoint parts of
// transcript width, interleaved with ranked blocks.
func headShape(n int) unrankedShape {
	unranked := n * 57 / 100
	return unrankedShape{ranked: n - unranked, unranked: unranked, unrankedWidth: 35000, interleaved: true}
}

// headRoots lists, ascending, the root sessions that hold at least one part.
func headRoots(shape unrankedShape) []int {
	if shape.omitUnranked {
		return nil
	}
	seen := map[int]bool{}
	for i := 0; i < shape.ranked+shape.unranked; i++ {
		if !shape.isRanked(i) {
			seen[i/headPartsPerRoot] = true
		}
	}
	roots := make([]int, 0, len(seen))
	for r := range seen {
		roots = append(roots, r)
	}
	sort.Ints(roots)
	return roots
}

// headTitle is the title the plugin gives the head of a root session.
func headTitle(root int) string {
	return fmt.Sprintf("Compaction checkpoint head r%06d", root)
}

// headSearchRoot is the root whose head the searches look for.
func headSearchRoot(roots []int) int {
	for _, r := range roots {
		if r == headPreferredRoot {
			return r
		}
	}
	return roots[len(roots)/2]
}

// headSeed seeds shape through unrankedSeed and then, through COPY, exactly one
// head per root session that has parts: small content, the root and its latest
// manifest in metadata, the plugin's head tags.
func headSeed(shape unrankedShape) func(ctx context.Context, pool *pgxpool.Pool) error {
	parts := unrankedSeed(shape)
	return func(ctx context.Context, pool *pgxpool.Pool) error {
		if err := parts(ctx, pool); err != nil {
			return err
		}
		roots := headRoots(shape)
		newest := time.Date(2026, 11, 1, 0, 0, 1, 0, time.UTC)
		next := 0
		_, err := pool.CopyFrom(ctx, pgx.Identifier{"context_blocks"},
			[]string{"category", "title", "content", "scope", "type_name", "created_at", "updated_at", "metadata", "tags"},
			pgx.CopyFromFunc(func() ([]any, error) {
				if next >= len(roots) {
					return nil, nil
				}
				r := roots[next]
				next++
				ts := newest.Add(time.Duration(r) * time.Millisecond)
				manifest := fmt.Sprintf("019a0000-0000-7000-8000-%012d", r)
				content := fmt.Sprintf("ctx checkpoint head: root session r%06d, latest manifest %s, %d parts in the chain. "+
					"The head names the newest manifest of this root's checkpoint chain; a restore reads that manifest "+
					"and then its parts in order. Written by the compaction hook after every stored checkpoint.",
					r, manifest, headPartsPerRoot)
				return []any{"compaction-checkpoints", headTitle(r), content, "private", "checkpoint", ts, ts,
					map[string]any{"root_session_id": fmt.Sprintf("r%06d", r), "latest_manifest_id": manifest},
					[]string{"hermes", "compaction", "checkpoint-head", "ctx", "control"}}, nil
			}))
		if err != nil {
			return fmt.Errorf("heads: %w", err)
		}
		_, err = pool.Exec(ctx, `VACUUM (ANALYZE) context_blocks`)
		return err
	}
}

// headForm is one search form as a consumer sends it, without a type filter.
type headForm struct {
	name string
	p    store.SearchBlocksParams
}

// headForms are the searches: the ctx_checkpoint head search, the ctx_handover
// head search (with the head tag since #342) and, as it was before, without it,
// and the agent's search for a passage.
func headForms(title string) []headForm {
	scopes := []string{"private"}
	return []headForm{
		{"checkpoint head", store.SearchBlocksParams{Query: title, ReadScopes: scopes, Category: "compaction-checkpoints",
			Tags: []string{"checkpoint-head"}, Limit: 10, Compact: true, ProbeNext: true}},
		{"handover head", store.SearchBlocksParams{Query: title, ReadScopes: scopes, Tags: []string{"checkpoint-head"},
			Limit: 5, Compact: true, ProbeNext: true}},
		{"handover head, title only", store.SearchBlocksParams{Query: title, ReadScopes: scopes, Limit: 5, Compact: true, ProbeNext: true}},
		{"agent passage", store.SearchBlocksParams{Query: unrankedWords[2000], ReadScopes: scopes, Category: "compaction-checkpoints",
			Limit: 10, Compact: true, ProbeNext: true}},
	}
}

// typed returns p with the checkpoint type filter.
func (f headForm) typed(on bool) store.SearchBlocksParams {
	p := f.p
	if on {
		p.Types = []string{"checkpoint"}
	}
	return p
}

// headPlanNode is the part of an EXPLAIN (FORMAT JSON) node the chain shows.
type headPlanNode struct {
	NodeType  string         `json:"Node Type"`
	IndexName string         `json:"Index Name"`
	Plans     []headPlanNode `json:"Plans"`
}

func (n headPlanNode) render() string {
	s := n.NodeType
	if n.IndexName != "" {
		s += ":" + n.IndexName
	}
	switch len(n.Plans) {
	case 0:
		return s
	case 1:
		return s + " > " + n.Plans[0].render()
	}
	kids := make([]string, len(n.Plans))
	for i, k := range n.Plans {
		kids[i] = k.render()
	}
	return s + " > {" + strings.Join(kids, ", ") + "}"
}

// headPlanChain renders a plan as its node types in pre-order, the index a
// node scans after a colon, siblings in braces.
func headPlanChain(t *testing.T, plan string) string {
	t.Helper()
	var doc []struct {
		Plan headPlanNode `json:"Plan"`
	}
	if err := json.Unmarshal([]byte(plan), &doc); err != nil || len(doc) != 1 {
		t.Fatalf("plan json: %v (%d documents)", err, len(doc))
	}
	return doc[0].Plan.render()
}

// headTitles executes the statement and returns the titles of its rows.
func headTitles(t *testing.T, pool *pgxpool.Pool, sql string, args []any) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), sql, args...)
	if err != nil {
		t.Fatalf("search: %v\n%s", err, sql)
	}
	defer rows.Close()
	col := -1
	for i, fd := range rows.FieldDescriptions() {
		if fd.Name == "title" {
			col = i
		}
	}
	if col < 0 {
		t.Fatalf("search: no title column\n%s", sql)
	}
	var titles []string
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			t.Fatalf("search row: %v", err)
		}
		titles = append(titles, fmt.Sprint(vals[col]))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("search rows: %v", err)
	}
	return titles
}

// headExplain picks the explain function of a plan kind.
func headExplain(plan string) func(*testing.T, *pgxpool.Pool, string, ...any) testdb.PlanCost {
	if plan == "generic" {
		return testdb.ExplainGeneric
	}
	return testdb.ExplainCustom
}

// headTypesLabel names the type filter of a measurement.
func headTypesLabel(on bool) string {
	if on {
		return "with types"
	}
	return "without types"
}

// headFixture checks the heads of a seeded store and returns the title the
// searches look for.
func headFixture(t *testing.T, pool *pgxpool.Pool, shape unrankedShape) string {
	t.Helper()
	roots := headRoots(shape)
	var heads int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM context_blocks WHERE type_name = 'checkpoint' AND category = 'compaction-checkpoints' AND tags @> '{checkpoint-head}'`).Scan(&heads); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if heads != len(roots) || heads == 0 {
		t.Fatalf("fixture: %d heads, want one per root with parts (%d)", heads, len(roots))
	}
	return headTitle(headSearchRoot(roots))
}

func TestCheckpointHeadSearchCost(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	shape := headShape(1400)
	pool := testdb.SetupSeededTestDB(t, "i342-head-cost-1400", headSeed(shape))
	title := headFixture(t, pool, shape)
	t.Logf("fixture: %d ranked, %d parts, %d heads; searching %q", shape.ranked, shape.unranked, len(headRoots(shape)), title)

	for _, f := range headForms(title)[:3] {
		// The head is found, and only the head, with and without the filter:
		// both statements answer the same question.
		for _, on := range []bool{false, true} {
			sql, args := store.SearchBlocksSQLForTest(f.typed(on))
			got := headTitles(t, pool, sql, args)
			if len(got) != 1 || got[0] != title {
				t.Errorf("%s %s: rows %q, want exactly the head %q", f.name, headTypesLabel(on), got, title)
			}
		}
		for _, plan := range []string{"custom", "generic"} {
			explain := headExplain(plan)
			sqlOff, argsOff := store.SearchBlocksSQLForTest(f.typed(false))
			sqlOn, argsOn := store.SearchBlocksSQLForTest(f.typed(true))
			off := explain(t, pool, sqlOff, argsOff...)
			on := explain(t, pool, sqlOn, argsOn...)
			bound := int64(float64(off.Buffers)*1.1) + 16
			t.Logf("%s (%s plan): %d buffers with types, %d without, bound %d; trgm title index: %t/%t\n  without: %s\n  with:    %s",
				f.name, plan, on.Buffers, off.Buffers, bound,
				strings.Contains(off.Plan, "idx_trgm_title"), strings.Contains(on.Plan, "idx_trgm_title"),
				headPlanChain(t, off.Plan), headPlanChain(t, on.Plan))
			if on.Buffers > bound {
				t.Errorf("%s (%s plan): %d buffers with types=[checkpoint], more than %d (%d without × 1.1 + 16): the type filter makes the head search more expensive",
					f.name, plan, on.Buffers, bound, off.Buffers)
			}
		}
	}
}

func TestCheckpointHeadSearchMeasure(t *testing.T) {
	sizes := os.Getenv("CTX_HEAD_MEASURE")
	if testing.Short() || sizes == "" {
		t.Skip("measurement; set CTX_HEAD_MEASURE to the store sizes, e.g. 10000,100000")
	}
	for _, f := range strings.Split(sizes, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil || n < 100 {
			t.Fatalf("size %q", f)
		}
		shape := headShape(n)
		start := time.Now()
		pool := testdb.SetupSeededTestDB(t, fmt.Sprintf("i342-head-measure-%d", n), headSeed(shape))
		title := headFixture(t, pool, shape)
		t.Logf("n=%d: %d ranked, %d parts, %d heads, heap %d pages, seeded in %s; searching %q",
			n, shape.ranked, shape.unranked, len(headRoots(shape)), unrankedHeapPages(t, pool), time.Since(start).Round(time.Second), title)
		trgm := 0
		for _, form := range headForms(title) {
			for _, on := range []bool{false, true} {
				sql, args := store.SearchBlocksSQLForTest(form.typed(on))
				t.Logf("n=%d %s %s: %d rows", n, form.name, headTypesLabel(on), len(headTitles(t, pool, sql, args)))
				for _, plan := range []string{"generic", "custom"} {
					explain := headExplain(plan)
					explain(t, pool, sql, args...) // warm
					ms := make([]float64, 0, 5)
					var last testdb.PlanCost
					for r := 0; r < 5; r++ {
						last = explain(t, pool, sql, args...)
						ms = append(ms, last.ExecMs)
					}
					sort.Float64s(ms)
					usesTrgm := strings.Contains(last.Plan, "idx_trgm_title")
					if usesTrgm {
						trgm++
					}
					t.Logf("HEAD n=%d | %s | %s | %s | median %.2f ms (min %.2f, max %.2f) | %d buffers | trgm %t | %s",
						n, form.name, headTypesLabel(on), plan, ms[2], ms[0], ms[4], last.Buffers, usesTrgm, headPlanChain(t, last.Plan))
				}
			}
		}
		t.Logf("n=%d: %d of %d plans use idx_trgm_title", n, trgm, len(headForms(title))*4)
	}
}
