//go:build integration

package store_test

// The measurement behind issue #237 (not a gate): browse (the empty-query
// path of the compact and the full search) and recent, on a store of
// CTX_I237_ROWS rows (default 50000) grown by generate_series — rows of about
// 1.3 kB of heap, 500 scopes interleaved, 1 in 13 archived, 1 in 7 a
// checkpoint part, timestamps shared by 50 rows each. Per phase
// (CTX_I237_PHASES):
//
//   - noidx: neither index of migration 195; the statement before 195 (base).
//   - idx2: both indexes; the statement before 195 (base) and the statement
//     production sends with both standing (new), whose rows must equal the
//     base's.
//   - glob: the variant the issue rejected, (updated_at DESC, id DESC) WHERE
//     NOT is_archived without the scope; the statement before 195 (base).
//
// Every case runs at random_page_cost 1.1 and 4, custom and generic plan, and
// logs buffers, rows read by the scans and time. Set CTX_I237_MEASURE=1.
//
//	CTX_I237_MEASURE=1 CTX_I237_ROWS=1000000 go test -tags=integration ./internal/store/ -run TestBrowseIndexMeasure -count=1 -v -timeout 4h

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// The indexes of the phases: the two of migration 195 and the rejected
// variant without the scope.
const (
	i237IndexDDL         = `CREATE INDEX idx_blocks_scope_updated ON context_blocks (scope, updated_at DESC, id DESC) WHERE NOT is_archived`
	i237CategoryIndexDDL = `CREATE INDEX idx_blocks_scope_category_updated ON context_blocks (scope, category, updated_at DESC, id DESC) WHERE NOT is_archived`
	i237GlobalIndexDDL   = `CREATE INDEX i237_global_updated ON context_blocks (updated_at DESC, id DESC) WHERE NOT is_archived`
)

// i237Seed grows context_blocks from have to rows rows (see the file header).
// The triggers are off for the mass insert (none of them shapes what browse
// and recent read).
func i237Seed(t *testing.T, pool *pgxpool.Pool, have, rows int) {
	t.Helper()
	ctx := context.Background()
	m182Exec(t, pool, `ALTER TABLE context_blocks DISABLE TRIGGER USER`)
	defer m182Exec(t, pool, `ALTER TABLE context_blocks ENABLE TRIGGER USER`)
	smallEvery := rows / 1000
	const batch = 250000
	for lo := have; lo < rows; lo += batch {
		hi := min(lo+batch, rows)
		t0 := time.Now()
		m182Exec(t, pool, `INSERT INTO context_blocks (category, title, content, scope, type_name, is_archived, metadata, tags, created_at, updated_at)
			SELECT CASE WHEN g % 7 = 3 THEN 'compaction-checkpoints'
			            WHEN scope LIKE 'i237-o%' AND g % 20 = 7 THEN 'i237-skewcat'
			            ELSE 'cat-' || lpad(((g / 3) % 20)::text, 2, '0') END,
			       'i237 row ' || g,
			       repeat('lorem ipsum dolor sit amet ', 36) || g,
			       scope,
			       CASE WHEN g % 7 = 3 THEN 'checkpoint' WHEN g % 10 = 1 THEN 'reference' ELSE 'knowledge' END,
			       g % 13 = 0,
			       jsonb_build_object('pad', repeat('m', 200)),
			       CASE WHEN g % 7 = 3 THEN ARRAY['hermes', 'compaction'] ELSE ARRAY['note', 'tag-' || lpad((g % 50)::text, 2, '0')] END,
			       ts - ((g % 1000) * interval '1 minute'),
			       ts
			  FROM (SELECT g,
			               CASE WHEN g % $3::int = 0 THEN 'i237-small'
			                    WHEN b < 200 THEN 'i237-big'
			                    WHEN b < 220 THEN 'i237-t1'
			                    WHEN b < 240 THEN 'i237-t2'
			                    WHEN b < 260 THEN 'i237-t3'
			                    ELSE 'i237-o' || lpad(((b * 7 + g) % 497)::text, 3, '0') END AS scope,
			               timestamptz '2026-11-01 00:00:00+00' - ((g / 50) * interval '1 second') AS ts
			          FROM (SELECT g, (g::bigint * 2654435761) % 1000 AS b FROM generate_series($1::int + 1, $2::int) g) gb) s`,
			lo, hi, smallEvery)
		t.Logf("seed %d..%d in %s", lo+1, hi, time.Since(t0).Round(time.Second))
	}
	// Ten rows of the big scope, spread over its whole order, carry the rare
	// category, the rare type and the rare tag; ten more the skewed category,
	// which 5 % of the other scopes' rows carry.
	m182Exec(t, pool, `WITH big AS (
			SELECT id, row_number() OVER (ORDER BY updated_at DESC, id DESC) AS r, count(*) OVER () AS n
			  FROM context_blocks WHERE scope = 'i237-big' AND NOT is_archived AND type_name = 'knowledge')
		UPDATE context_blocks c SET category = 'i237-rarecat', type_name = 'i237-raretype', tags = c.tags || ARRAY['i237-raretag']
		  FROM big WHERE big.id = c.id AND big.r % (big.n / 10) = 1 AND big.r < big.n - big.n / 20`)
	m182Exec(t, pool, `WITH big AS (
			SELECT id, row_number() OVER (ORDER BY updated_at DESC, id DESC) AS r, count(*) OVER () AS n
			  FROM context_blocks WHERE scope = 'i237-big' AND NOT is_archived AND type_name = 'knowledge')
		UPDATE context_blocks c SET category = 'i237-skewcat'
		  FROM big WHERE big.id = c.id AND big.r % (big.n / 10) = 7 AND big.r < big.n - big.n / 20`)
	t0 := time.Now()
	m182Exec(t, pool, `VACUUM ANALYZE context_blocks`)
	var heap int64
	if err := pool.QueryRow(ctx, `SELECT pg_relation_size('context_blocks')`).Scan(&heap); err != nil {
		t.Fatal(err)
	}
	t.Logf("vacuum analyze in %s; heap %d MB", time.Since(t0).Round(time.Second), heap>>20)
}

// i237Reader is one caller: its read scopes and its resolved grants.
type i237Reader struct {
	name   string
	scopes []string
	grants []string
}

// i237Filter is one filter combination a caller sets.
type i237Filter struct {
	name                      string
	category                  string
	tags, types, typesExclude []string
	browseOnly                bool
}

var i237Filters = []i237Filter{
	{name: "none"},
	{name: "cat", category: "cat-03"},
	{name: "rarecat", category: "i237-rarecat"},
	{name: "skewcat", category: "i237-skewcat"},
	{name: "types", types: []string{"reference"}},
	{name: "raretype", types: []string{"i237-raretype"}},
	{name: "excl", typesExclude: []string{"checkpoint", "system-meta"}},
	{name: "exclrare", typesExclude: []string{"knowledge", "checkpoint", "reference"}},
	{name: "tags", tags: []string{"tag-07"}, browseOnly: true},
	{name: "raretag", tags: []string{"i237-raretag"}, browseOnly: true},
}

// i237ScanRows sums the rows the scan nodes of a JSON plan read (returned
// plus removed by a filter or a recheck, times loops).
func i237ScanRows(t *testing.T, plan string) int64 {
	t.Helper()
	var doc []struct {
		Plan map[string]any `json:"Plan"`
	}
	if err := json.Unmarshal([]byte(plan), &doc); err != nil || len(doc) != 1 {
		t.Fatalf("plan json: %v", err)
	}
	var walk func(n map[string]any) int64
	walk = func(n map[string]any) int64 {
		var sum int64
		typ, _ := n["Node Type"].(string)
		if strings.HasSuffix(typ, "Scan") && typ != "Function Scan" && typ != "Subquery Scan" && !strings.HasPrefix(typ, "Bitmap Index") {
			f := func(k string) float64 { v, _ := n[k].(float64); return v }
			sum += int64((f("Actual Rows") + f("Rows Removed by Filter") + f("Rows Removed by Index Recheck")) * f("Actual Loops"))
		}
		if kids, ok := n["Plans"].([]any); ok {
			for _, k := range kids {
				if m, ok := k.(map[string]any); ok {
					sum += walk(m)
				}
			}
		}
		return sum
	}
	return walk(doc[0].Plan)
}

func i237Env(name string, def []string) []string {
	if v := os.Getenv(name); v != "" {
		return strings.Split(v, ",")
	}
	return def
}

func TestBrowseIndexMeasure(t *testing.T) {
	if os.Getenv("CTX_I237_MEASURE") == "" {
		t.Skip("measurement; set CTX_I237_MEASURE=1")
	}
	ctx := context.Background()
	rows := 50000
	if v := os.Getenv("CTX_I237_ROWS"); v != "" {
		if _, err := fmt.Sscan(v, &rows); err != nil {
			t.Fatal(err)
		}
	}
	image := os.Getenv("CTX_TEST_PG_IMAGE")
	if image == "" {
		image = "ci"
	}
	pool := testdb.SetupTestDB(t)
	i237Seed(t, pool, 0, rows)

	ids := func(sql string, args ...any) []string {
		t.Helper()
		r, err := pool.Query(ctx, sql, args...)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		var out []string
		for r.Next() {
			var s string
			if err := r.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
		return out
	}
	// Grants: 20 blocks of other scopes (archived ones among them) and 5 of
	// the big scope.
	grants := ids(`SELECT id::text FROM context_blocks WHERE scope = 'i237-o013' ORDER BY id LIMIT 20`)
	grants = append(grants, ids(`SELECT id::text FROM context_blocks WHERE scope = 'i237-big' AND NOT is_archived ORDER BY id LIMIT 5`)...)
	others := ids(`SELECT DISTINCT scope FROM context_blocks WHERE scope LIKE 'i237-o%' ORDER BY 1`)
	readers := map[string]i237Reader{
		"big":     {"big", []string{"i237-big"}, nil},
		"small":   {"small", []string{"i237-small"}, nil},
		"three":   {"three", []string{"i237-t1", "i237-t2", "i237-t3"}, nil},
		"big+g":   {"big+g", []string{"i237-big"}, grants},
		"small+g": {"small+g", []string{"i237-small"}, grants},
		"three+g": {"three+g", []string{"i237-t1", "i237-t2", "i237-t3"}, grants},
		"many50":  {"many50", others[:min(50, len(others))], nil},
		"many500": {"many500", append([]string{"i237-t1", "i237-t2", "i237-t3"}, others...), nil},
		"empty":   {"empty", []string{"i237-none"}, nil},
	}
	readerNames := i237Env("CTX_I237_READERS", []string{"big", "small", "three", "big+g", "small+g", "three+g", "many50", "many500", "empty"})
	filterNames := i237Env("CTX_I237_FILTERS", nil)
	filterReaders := i237Env("CTX_I237_FILTER_READERS", []string{"big", "small", "three"})
	phases := i237Env("CTX_I237_PHASES", []string{"noidx", "idx2", "glob"})
	stmts := i237Env("CTX_I237_STMTS", []string{"browse-compact", "browse-full", "recent"})
	var load string
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		load = strings.Join(strings.Fields(string(b))[:3], " ")
	}
	t.Logf("rows=%d image=%s load=%s", rows, image, load)

	type stmt struct {
		sql  string
		args []any
	}
	type kase struct {
		reader i237Reader
		filter i237Filter
		kind   string
		page   string
		base   stmt
		next   stmt
	}
	var cases []kase
	deep := map[string]*store.SearchCursor{}
	deepOf := func(r i237Reader) *store.SearchCursor {
		if c, ok := deep[r.name]; ok {
			return c
		}
		var n int64
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_blocks WHERE NOT is_archived AND scope = ANY($1)`, r.scopes).Scan(&n); err != nil {
			t.Fatal(err)
		}
		c := &store.SearchCursor{}
		if n > 0 {
			if err := pool.QueryRow(ctx, `SELECT updated_at, id::text FROM context_blocks WHERE NOT is_archived AND scope = ANY($1)
				ORDER BY updated_at DESC, id DESC OFFSET $2 LIMIT 1`, r.scopes, n*8/10).Scan(&c.UpdatedAt, &c.ID); err != nil {
				t.Fatal(err)
			}
		} else {
			c = &store.SearchCursor{UpdatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), ID: "00000000-0000-7000-8000-000000000000"}
		}
		deep[r.name] = c
		return c
	}
	for _, rn := range readerNames {
		r, ok := readers[rn]
		if !ok {
			t.Fatalf("reader %q", rn)
		}
		for _, f := range i237Filters {
			if f.name != "none" && !slices.Contains(filterReaders, rn) {
				continue
			}
			if filterNames != nil && !slices.Contains(filterNames, f.name) {
				continue
			}
			for _, kind := range stmts {
				if kind == "recent" {
					if f.browseOnly {
						continue
					}
					bs, ba := store.RecentBlocksSQLForTest(r.scopes, f.category, 10, f.types, f.typesExclude, nonNil(r.grants))
					ns, na := store.RecentStatementForTest(r.scopes, f.category, 10, f.types, f.typesExclude, nonNil(r.grants), true, true)
					cases = append(cases, kase{r, f, kind, "first", stmt{bs, ba}, stmt{ns, na}})
					continue
				}
				for _, page := range []string{"first", "deep"} {
					p := store.SearchBlocksParams{
						ReadScopes: r.scopes, Category: f.category, Tags: f.tags, Types: f.types, TypesExclude: f.typesExclude,
						Limit: store.SearchDefaultLimit, Compact: kind == "browse-compact", GrantedBlockIDs: nonNil(r.grants), ProbeNext: true,
					}
					if page == "deep" {
						p.After = deepOf(r)
					}
					bs, ba := store.SearchBlocksSQLForTest(p)
					ns, na := store.SearchStatementForTest(p, true, true)
					cases = append(cases, kase{r, f, kind, page, stmt{bs, ba}, stmt{ns, na}})
				}
			}
		}
	}
	t.Logf("%d cases", len(cases))

	pools := map[string]*pgxpool.Pool{}
	for _, rpc := range testdb.RandomPageCosts {
		pools[rpc] = testdb.WithRandomPageCost(t, pool, rpc)
	}
	for _, phase := range phases {
		m182Exec(t, pool, `DROP INDEX IF EXISTS idx_blocks_scope_updated`)
		m182Exec(t, pool, `DROP INDEX IF EXISTS idx_blocks_scope_category_updated`)
		m182Exec(t, pool, `DROP INDEX IF EXISTS i237_global_updated`)
		variants := []string{"base"}
		switch phase {
		case "noidx":
		case "idx2":
			variants = append(variants, "new")
			for _, ix := range []struct{ name, ddl string }{
				{"idx_blocks_scope_updated", i237IndexDDL}, {"idx_blocks_scope_category_updated", i237CategoryIndexDDL},
			} {
				t0 := time.Now()
				m182Exec(t, pool, ix.ddl)
				var size int64
				if err := pool.QueryRow(ctx, `SELECT pg_relation_size($1::regclass)`, ix.name).Scan(&size); err != nil {
					t.Fatal(err)
				}
				t.Logf("MEASURE-INDEX|rows=%d|image=%s|index=%s|build=%s|size_mb=%.1f", rows, image, ix.name, time.Since(t0).Round(time.Millisecond), float64(size)/(1<<20))
			}
		case "glob":
			m182Exec(t, pool, i237GlobalIndexDDL)
		default:
			t.Fatalf("phase %q", phase)
		}
		m182Exec(t, pool, `ANALYZE context_blocks`)
		for _, c := range cases {
			// The two statements answer the same rows in the same order.
			got := map[string][]string{}
			stmtOf := map[string]stmt{"base": c.base, "new": c.next}
			for _, v := range variants {
				s := stmtOf[v]
				r, err := pool.Query(ctx, s.sql, s.args...)
				if err != nil {
					t.Fatalf("%s %s: %v\n%s", v, c.kind, err, s.sql)
				}
				for r.Next() {
					vals, err := r.Values()
					if err != nil {
						t.Fatal(err)
					}
					got[v] = append(got[v], fmt.Sprintf("%v|%v", vals[0], vals[len(vals)-2]))
				}
				r.Close()
			}
			if len(variants) > 1 && !slices.Equal(got["base"], got["new"]) {
				t.Errorf("DIFF %s %s %s %s: base %v new %v", c.reader.name, c.filter.name, c.kind, c.page, got["base"], got["new"])
			}
			for _, rpc := range testdb.RandomPageCosts {
				for _, plan := range []string{"custom", "generic"} {
					explain := testdb.ExplainCustom
					if plan == "generic" {
						explain = testdb.ExplainGeneric
					}
					for _, v := range variants {
						s := stmtOf[v]
						pc := explain(t, pools[rpc], s.sql, s.args...)
						t.Logf("MEASURE|rows=%d|image=%s|phase=%s|reader=%s|filter=%s|stmt=%s|page=%s|rpc=%s|plan=%s|variant=%s|buffers=%d|scanned=%d|ms=%.2f|out=%d",
							rows, image, phase, c.reader.name, c.filter.name, c.kind, c.page, rpc, plan, v, pc.Buffers, i237ScanRows(t, pc.Plan), pc.ExecMs, len(got[v]))
						if os.Getenv("CTX_I237_PLANS") != "" {
							t.Logf("PLAN|%s|%s|%s|%s|%s|%s|%s|%s\n%s", phase, c.reader.name, c.filter.name, c.kind, c.page, rpc, plan, v, i237PlanShape(pc.Plan))
						}
					}
				}
			}
		}
	}
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// i237PlanShape renders the node types and index names of a JSON plan, one
// node per line, indented by depth.
func i237PlanShape(plan string) string {
	var doc []struct {
		Plan map[string]any `json:"Plan"`
	}
	if err := json.Unmarshal([]byte(plan), &doc); err != nil || len(doc) != 1 {
		return "?"
	}
	var b strings.Builder
	var walk func(n map[string]any, d int)
	walk = func(n map[string]any, d int) {
		typ, _ := n["Node Type"].(string)
		ix, _ := n["Index Name"].(string)
		loops, _ := n["Actual Loops"].(float64)
		rows, _ := n["Actual Rows"].(float64)
		hit, _ := n["Shared Hit Blocks"].(float64)
		read, _ := n["Shared Read Blocks"].(float64)
		fmt.Fprintf(&b, "%s%s %s rows=%.0f loops=%.0f buf=%.0f\n", strings.Repeat("  ", d), typ, ix, rows, loops, hit+read)
		if kids, ok := n["Plans"].([]any); ok {
			for _, k := range kids {
				if m, ok := k.(map[string]any); ok {
					walk(m, d+1)
				}
			}
		}
	}
	walk(doc[0].Plan, 0)
	return b.String()
}
