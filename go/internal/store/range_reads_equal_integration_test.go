//go:build integration

package store_test

// Equality probes of issue #536: each rewritten statement answers the rows of
// the statement it replaces, in the same order, for a set of parameters, under
// random_page_cost 1.1 and 4, custom and generic plan. The replaced statements
// are kept here byte for byte as they were in 89d9e70c (rrOld*): the reference
// of the row set, not a production path.
//
//	go test -tags=integration -p 1 ./internal/store/ -run 'TestRangeReadsSameRows' -count=1 -v

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/visibility"
)

var rrOldFullGraphSeed = fmt.Sprintf(
	`SELECT b.id::text, left(b.title, 120), b.category, b.scope::text, b.created_at
		 FROM context_blocks b
		 WHERE %s
		   AND ($5::text[] IS NULL OR b.category = ANY($5))
		   AND ($6::timestamptz IS NULL OR b.created_at >= $6)
		   AND ($7::timestamptz IS NULL OR b.created_at <  $7)
		 ORDER BY b.created_at DESC, b.id
		 LIMIT $4`,
	visibility.Predicate("b", "$3", "$1", "$2"),
)

const rrOldStarScopes = `SELECT scope::text FROM context_blocks
		 WHERE embedding IS NULL AND NOT is_archived AND NOT embed_excluded AND type_name = $1
		UNION
		SELECT scope::text FROM context_blocks
		 WHERE embedding_stale AND NOT is_archived AND type_name = $1
		ORDER BY 1`

const rrOldSearchUpdated = `
	SELECT b.id::text, b.scope, b.type_name, b.title, b.workflow_status, b.updated_at, b.created_at
	FROM context_blocks b
	WHERE b.scope = $1::text
	  AND b.type_name = $2
	  AND b.type_name NOT IN ('checkpoint','system-meta')
	  AND NOT b.is_archived
	  AND ($3::text = '' OR b.workflow_status = $3)
	  AND (b.ts_de @@ plainto_tsquery('german', $4) OR b.ts_en @@ plainto_tsquery('english', $4))
	  AND (b.updated_at, b.id) < ($5::timestamptz, $6::uuid)
	  AND ($7::text[] IS NULL OR b.metadata->'labels' ?& $7::text[])
	ORDER BY b.updated_at DESC, b.id DESC
	LIMIT $8`

const rrOldSearchCreated = `
	SELECT b.id::text, b.scope, b.type_name, b.title, b.workflow_status, b.updated_at, b.created_at
	FROM context_blocks b
	WHERE b.scope = $1::text
	  AND b.type_name = $2
	  AND b.type_name NOT IN ('checkpoint','system-meta')
	  AND NOT b.is_archived
	  AND ($3::text = '' OR b.workflow_status = $3)
	  AND (b.ts_de @@ plainto_tsquery('german', $4) OR b.ts_en @@ plainto_tsquery('english', $4))
	  AND (b.created_at, b.id) < ($5::timestamptz, $6::uuid)
	  AND ($7::text[] IS NULL OR b.metadata->'labels' ?& $7::text[])
	ORDER BY b.created_at DESC, b.id DESC
	LIMIT $8`

const rrOldGuardList = `SELECT b.id, b.title, b.category, b.scope, b.type_name, b.guard_status,
		b.metadata->>'guard_similarity' AS similarity,
		CASE
			WHEN mb.id IS NULL THEN NULL
			WHEN mb.scope = ANY($%d) THEN b.metadata->>'guard_matched_id'
			ELSE NULL
		END AS matched_id,
		CASE
			WHEN mb.id IS NULL THEN NULL
			WHEN mb.scope = ANY($%d) THEN mb.title
			ELSE '[redacted]'
		END AS matched_title,
		b.metadata->>'guard_checked_at' AS checked_at,
		b.updated_at
	FROM context_blocks b
	LEFT JOIN context_blocks mb ON mb.id::text = b.metadata->>'guard_matched_id'
	WHERE %s
	ORDER BY (SELECT CASE WHEN pg_input_is_valid(s.v, 'numeric') THEN s.v::numeric END
		  FROM (SELECT b.metadata->>'guard_similarity' AS v OFFSET 0) AS s) DESC NULLS LAST
	LIMIT $%d`

const rrOldResolve = `SELECT id, title, category, tags, scope, updated_at
		 FROM context_blocks
		 WHERE id::text LIKE $1 || '%' AND NOT is_archived AND (scope = ANY($2::text[]) OR id = ANY($4::uuid[]))
		 ORDER BY updated_at DESC
		 LIMIT $3`

// rrRows runs st under one plan and returns its rows, one string each, in
// result order.
func (r *rrPlanners) rows(db *pgxpool.Pool, rpc string, generic bool, st rrStmt) []string {
	t := r.t
	t.Helper()
	ctx := context.Background()
	conn, err := r.pool(db, rpc, st.local).Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	mode := "force_custom_plan"
	if generic {
		mode = "force_generic_plan"
	}
	for _, s := range []string{"SET plan_cache_mode = " + mode, "PREPARE rr_rows AS " + st.sql} {
		if _, err := conn.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v\n%s", strings.SplitN(s, " ", 2)[0], err, st.sql)
		}
	}
	defer func() {
		_, _ = conn.Exec(ctx, "DEALLOCATE rr_rows")
		_, _ = conn.Exec(ctx, "RESET plan_cache_mode")
	}()
	lits := make([]string, len(st.args))
	for i, a := range st.args {
		lits[i] = rrLiteral(t, a)
	}
	exec := "EXECUTE rr_rows"
	if len(lits) > 0 {
		exec += "(" + strings.Join(lits, ", ") + ")"
	}
	rows, err := conn.Query(ctx, exec, pgx.QueryExecModeSimpleProtocol)
	if err != nil {
		t.Fatalf("rows: %v\n%s", err, st.sql)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			t.Fatal(err)
		}
		parts := make([]string, len(vals))
		for i, v := range vals {
			switch x := v.(type) {
			case time.Time:
				parts[i] = x.UTC().Format(time.RFC3339Nano)
			case [16]byte:
				parts[i] = fmt.Sprintf("%x-%x-%x-%x-%x", x[0:4], x[4:6], x[6:8], x[8:10], x[10:16])
			default:
				parts[i] = fmt.Sprint(v)
			}
		}
		out = append(out, strings.Join(parts, "|"))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// rrSame compares the rows of the new statement with the old one's under
// every plan and counts the parameter sets.
type rrSame struct {
	p        *rrPlanners
	db       *pgxpool.Pool
	sets     int
	nonEmpty int
}

// group returns the check at the end of a group of parameter sets: at least
// one of them has rows, so the group does not compare empty answers only.
func (s *rrSame) group(t *testing.T) func() {
	n := s.nonEmpty
	return func() {
		if s.nonEmpty == n {
			t.Errorf("no parameter set of the group has rows")
		}
	}
}

func (s *rrSame) check(label string, oldSt, newSt rrStmt) {
	t := s.p.t
	t.Helper()
	s.sets++
	ref := s.p.rows(s.db, "4", false, oldSt)
	if len(ref) > 0 {
		s.nonEmpty++
	}
	t.Logf("%s: %d rows", label, len(ref))
	for _, p := range rrPlans {
		for form, st := range map[string]rrStmt{"old": oldSt, "new": newSt} {
			got := s.p.rows(s.db, p.rpc, p.generic, st)
			if strings.Join(got, "\n") != strings.Join(ref, "\n") {
				t.Errorf("%s (%s, %s statement): %d rows differ from the old statement's %d\n got %v\nwant %v",
					label, rrPlanName(p.rpc, p.generic), form, len(got), len(ref), got, ref)
			}
		}
	}
}

// TestRangeReadsSameRows: the rewritten statements of issue #536 answer the
// rows of the statements they replace, for every parameter set below, on
// every plan.
func TestRangeReadsSameRows(t *testing.T) {
	small, _ := rrStores(t)
	ctx := context.Background()
	p := &rrPlanners{t: t}
	same := &rrSame{p: p, db: small}
	traced, tr := rrTraced(t, small)
	one := func(sql string, args ...any) string {
		t.Helper()
		var s string
		if err := small.QueryRow(ctx, sql, args...).Scan(&s); err != nil {
			t.Fatalf("%v\n%s", err, sql)
		}
		return s
	}
	list := func(sql string, args ...any) []string {
		t.Helper()
		rows, err := small.Query(ctx, sql, args...)
		if err != nil {
			t.Fatal(err)
		}
		out, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	t.Run("full graph seed", func(t *testing.T) {
		defer same.group(t)()
		grants := list(`(SELECT id::text FROM context_blocks WHERE scope = $1 AND type_name = 'knowledge' ORDER BY created_at DESC LIMIT 3)
			UNION ALL (SELECT id::text FROM context_blocks WHERE scope = $1 AND type_name = 'knowledge' ORDER BY id OFFSET 500 LIMIT 3)
			UNION ALL (SELECT id::text FROM context_blocks WHERE scope = $1 AND type_name = 'issue' ORDER BY id LIMIT 1)
			UNION ALL (SELECT id::text FROM context_blocks WHERE scope = $2 ORDER BY created_at LIMIT 2)`, rrOther, rrRead)
		archived := one(`SELECT id::text FROM context_blocks WHERE scope = $1 AND type_name = 'knowledge' ORDER BY id OFFSET 900 LIMIT 1`, rrOther)
		if _, err := store.DeleteBlock(ctx, small, archived, []string{rrOther}); err != nil {
			t.Fatalf("archive: %v", err)
		}
		grants = append(grants, archived)
		mid, err := time.Parse("2006-01-02 15:04:05.999999-07", one(`SELECT created_at::text FROM context_blocks WHERE scope = $1 ORDER BY created_at OFFSET 150 LIMIT 1`, rrRead))
		if err != nil {
			t.Fatal(err)
		}
		// 300 blocks of one created_at (one statement writes them): a page
		// that ends inside them takes the smallest ids of the tie. Random
		// (version 4) ids, as a database may hold from before uuidv7, so the
		// order the index returns the tie in is not the order of the ids.
		if _, err := small.Exec(ctx, `INSERT INTO context_blocks (id, category, title, content, scope, type_name)
			SELECT gen_random_uuid(), 'learnings', 'rr same ' || i, 'same body ' || i, 'rr-same', 'knowledge' FROM generate_series(1, 300) i`); err != nil {
			t.Fatal(err)
		}
		know := []string{"knowledge"}
		for _, c := range []struct {
			name           string
			scopes, grants []string
			types, cats    []string
			after, before  *time.Time
			limit          int
		}{
			{"one read scope", []string{rrRead}, nil, know, nil, nil, nil, 200},
			{"one read scope, grants", []string{rrRead}, grants, know, nil, nil, nil, 200},
			{"three read scopes, one named twice, grants, a category", []string{rrRead, rrProj, rrRead}, grants, know, []string{"decisions"}, nil, nil, 200},
			{"one read scope, grants, created before", []string{rrRead}, grants, know, nil, nil, &mid, 200},
			{"the large scope, grants", []string{rrOther}, grants, know, nil, nil, nil, 200},
			{"one read scope, grants, two types", []string{rrRead}, grants, []string{"knowledge", "issue"}, nil, nil, nil, 200},
			{"one read scope, grants, limit 4", []string{rrRead}, grants, know, nil, nil, nil, 4},
			{"two read scopes, grants, created after", []string{rrRead, rrOther}, grants, know, nil, &mid, nil, 200},
			{"the project, issues", []string{rrProj}, nil, []string{"issue"}, nil, nil, nil, 50},
			{"a scope of one created_at, a page inside it", []string{"rr-same"}, nil, know, nil, nil, nil, 100},
			{"a scope of one created_at beside the read scope, grants", []string{"rr-same", rrRead}, grants, know, nil, nil, nil, 120},
		} {
			tr.take()
			if _, err := store.FullGraph(ctx, traced, store.EgoParams{Limit: c.limit, EdgeLimit: 4000, Categories: c.cats, CreatedAfter: c.after, CreatedBefore: c.before}, c.scopes, c.grants, c.types); err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			st := rrFind(t, tr.take(), func(sql string) bool { return strings.Contains(sql, "left(") && strings.Contains(sql, "context_blocks") })
			same.check("full graph seed: "+c.name, rrStmt{sql: rrOldFullGraphSeed, args: st.args}, st)
		}
	})

	t.Run("scopes of a star entry", func(t *testing.T) {
		defer same.group(t)()
		// A stale block of the name in rr-guard: the content of one of the
		// guard's blocks changes, its vector stays and is marked stale.
		var id string
		if err := small.QueryRow(ctx, `SELECT id::text FROM context_blocks WHERE scope = $1 ORDER BY id LIMIT 1`, rrGuard).Scan(&id); err != nil {
			t.Fatal(err)
		}
		var title string
		if err := small.QueryRow(ctx, `SELECT title FROM context_blocks WHERE id = $1::uuid`, id).Scan(&title); err != nil {
			t.Fatal(err)
		}
		if _, err := store.UpsertBlock(ctx, small, "learnings", title, "changed guard list body", nil, nil, rrGuard, true, store.SensitivityWrite{}, "knowledge"); err != nil {
			t.Fatal(err)
		}
		if one(`SELECT embedding_stale::text FROM context_blocks WHERE id = $1::uuid`, id) != "true" {
			t.Fatal("fixture: the changed block is not stale")
		}
		for _, name := range []string{rrPtx, "knowledge", "issue", "no-such-type"} {
			newSt := rrStmt{sql: store.EmbedExcludedStarScopesSQLForTest, args: []any{name},
				local: map[string]string{"enable_sort": "off", "enable_incremental_sort": "off"}}
			same.check("scopes of a '*' entry of "+name, rrStmt{sql: rrOldStarScopes, args: []any{name}}, newSt)
		}
	})

	t.Run("issue search", func(t *testing.T) {
		defer same.group(t)()
		var deepAt time.Time
		var deepID string
		if err := small.QueryRow(ctx, `SELECT updated_at, id::text FROM context_blocks WHERE scope = $1 AND type_name = 'issue'
			ORDER BY updated_at DESC, id DESC OFFSET 700 LIMIT 1`, rrProj).Scan(&deepAt, &deepID); err != nil {
			t.Fatal(err)
		}
		for _, c := range []struct {
			name string
			q    store.IssueReadQuery
		}{
			{"common word", store.IssueReadQuery{Scope: rrProj, Q: rrCommon, Limit: 50}},
			{"common word, by creation", store.IssueReadQuery{Scope: rrProj, Q: rrCommon, Sort: store.SortCreated, Limit: 50}},
			{"rare word", store.IssueReadQuery{Scope: rrProj, Q: rrRare, Limit: 50}},
			{"rare word, by creation", store.IssueReadQuery{Scope: rrProj, Q: rrRare, Sort: store.SortCreated, Limit: 50}},
			{"common word, one status", store.IssueReadQuery{Scope: rrProj, Q: rrCommon, Status: "done", Limit: 20}},
			{"common word, a label", store.IssueReadQuery{Scope: rrProj, Q: rrCommon, Labels: []string{"bug"}, Limit: 20}},
			{"common word, deep page", store.IssueReadQuery{Scope: rrProj, Q: rrCommon, Limit: 50, Cursor: &store.WorkflowCursor{UpdatedAt: deepAt, ID: deepID}}},
			{"common word, deep page by creation", store.IssueReadQuery{Scope: rrProj, Q: rrCommon, Sort: store.SortCreated, Limit: 50, Cursor: &store.WorkflowCursor{CreatedAt: deepAt, ID: deepID}}},
			{"a word no block carries", store.IssueReadQuery{Scope: rrProj, Q: "kein-solches-wort", Limit: 50}},
			{"the large scope", store.IssueReadQuery{Scope: rrOther, Q: rrCommon, Limit: 50}},
		} {
			tr.take()
			if _, _, err := store.SearchIssues(ctx, traced, c.q); err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			st := rrFind(t, tr.take(), func(sql string) bool { return strings.Contains(sql, "plainto_tsquery") })
			old := rrOldSearchUpdated
			if c.q.Sort == store.SortCreated {
				old = rrOldSearchCreated
			}
			same.check("issue search: "+c.name, rrStmt{sql: old, args: st.args}, st)
		}
	})

	t.Run("issue lists", func(t *testing.T) {
		defer same.group(t)()
		var deepAt time.Time
		var deepID string
		if err := small.QueryRow(ctx, `SELECT updated_at, id::text FROM context_blocks WHERE scope = $1 AND type_name = 'issue' AND workflow_status = 'backlog'
			ORDER BY updated_at DESC, id DESC OFFSET 300 LIMIT 1`, rrProj).Scan(&deepAt, &deepID); err != nil {
			t.Fatal(err)
		}
		for _, c := range []struct {
			name string
			call func() error
			ord  string
		}{
			{"status list", func() error {
				_, _, err := store.ListIssues(ctx, traced, store.IssueListQuery{Scopes: []string{rrProj}, Status: "backlog", Limit: 50})
				return err
			}, "updated_at DESC"},
			{"status list, deep page", func() error {
				_, _, err := store.ListIssues(ctx, traced, store.IssueListQuery{Scopes: []string{rrProj}, Status: "backlog", Limit: 50,
					Cursor: &store.WorkflowCursor{Status: "backlog", UpdatedAt: deepAt, ID: deepID}})
				return err
			}, "updated_at DESC"},
			{"status list, a label", func() error {
				_, _, err := store.ListIssues(ctx, traced, store.IssueListQuery{Scopes: []string{rrProj}, Status: "done", Labels: []string{"bug"}, Limit: 50})
				return err
			}, "updated_at DESC"},
			{"board column", func() error {
				_, err := store.BoardColumns(ctx, traced, rrProj, []string{"in-progress"}, nil, 20)
				return err
			}, "updated_at DESC"},
			{"created list", func() error {
				_, _, err := store.ListIssuesByCreated(ctx, traced, store.IssueReadQuery{Scope: rrProj, Limit: 50})
				return err
			}, "created_at DESC"},
			{"created list, one status", func() error {
				_, _, err := store.ListIssuesByCreated(ctx, traced, store.IssueReadQuery{Scope: rrProj, Status: "done", Limit: 50})
				return err
			}, "created_at DESC"},
			{"created list, deep page", func() error {
				_, _, err := store.ListIssuesByCreated(ctx, traced, store.IssueReadQuery{Scope: rrProj, Limit: 50,
					Cursor: &store.WorkflowCursor{CreatedAt: deepAt, ID: deepID}})
				return err
			}, "created_at DESC"},
			{"created list, the large scope", func() error {
				_, _, err := store.ListIssuesByCreated(ctx, traced, store.IssueReadQuery{Scope: rrOther, Limit: 50})
				return err
			}, "created_at DESC"},
		} {
			tr.take()
			if err := c.call(); err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			st := rrFind(t, tr.take(), func(sql string) bool { return strings.Contains(sql, "ORDER BY b."+c.ord) && !strings.Contains(sql, "plainto_tsquery") })
			same.check("issue list: "+c.name, rrStmt{sql: st.sql, args: st.args}, st)
		}
	})

	t.Run("guard list", func(t *testing.T) {
		defer same.group(t)()
		// The guard gave both blocks of a pair one similarity: a distinct one
		// each, so the order is the statements' and not a tie's.
		if _, err := small.Exec(ctx, `WITH r AS (SELECT id, row_number() OVER (ORDER BY id) AS n FROM context_blocks WHERE scope = $1)
			UPDATE context_blocks b SET metadata = jsonb_set(b.metadata, '{guard_similarity}', to_jsonb(0.5 + r.n / 1000.0))
			  FROM r WHERE b.id = r.id`, rrGuard); err != nil {
			t.Fatal(err)
		}
		for _, c := range []struct {
			name           string
			scopes         []string
			cat, status    string
			types          []string
			limit          int
		}{
			{"the flagged scope", []string{rrGuard}, "", "", nil, 50},
			{"the flagged scope, limit 5", []string{rrGuard}, "", "", nil, 5},
			{"the flagged scope beside others", []string{rrRead, rrGuard, rrOther}, "", "", nil, 50},
			{"a status", []string{rrGuard}, "", "needs_review", nil, 50},
			{"a category", []string{rrGuard}, "learnings", "", nil, 50},
			{"a type", []string{rrGuard}, "", "", []string{"knowledge"}, 50},
			{"active blocks", []string{rrRead}, "", "active", nil, 20},
		} {
			newSQL, args := store.GuardListQueryForTest(c.scopes, c.cat, c.status, c.types, c.limit)
			n := len(args)
			where := []string{"NOT b.is_archived", "b.scope = ANY($1::text[])"}
			i := 2
			if c.status != "" {
				where = append(where, fmt.Sprintf("b.guard_status = $%d", i))
				i++
			} else {
				where = append(where, "b.guard_status NOT IN ('active', 'clean')")
			}
			if c.cat != "" {
				where = append(where, fmt.Sprintf("b.category = $%d", i))
				i++
			}
			if len(c.types) > 0 {
				where = append(where, fmt.Sprintf("b.type_name = ANY($%d::text[])", i))
			}
			old := fmt.Sprintf(rrOldGuardList, n, n, strings.Join(where, " AND "), n-1)
			same.check("guard list: "+c.name, rrStmt{sql: old, args: args}, rrStmt{sql: newSQL, args: args})
		}
	})

	t.Run("block id prefix", func(t *testing.T) {
		defer same.group(t)()
		// Ids on the bounds of a prefix's range and just outside it.
		for _, id := range []string{"0f0f0f0f-0000-0000-0000-000000000000", "0f0f0f0f-ffff-ffff-ffff-ffffffffffff",
			"0f0f0f0e-ffff-ffff-ffff-ffffffffffff", "0f0f0f10-0000-0000-0000-000000000000"} {
			if _, err := small.Exec(ctx, `INSERT INTO context_blocks (id, category, title, content, scope, type_name)
				VALUES ($1::uuid, 'learnings', 'rr bound ' || $1, 'bound body', $2, 'knowledge')`, id, rrRead); err != nil {
				t.Fatal(err)
			}
		}
		ids := list(`(SELECT id::text FROM context_blocks WHERE scope = $1 ORDER BY id LIMIT 2)
			UNION ALL (SELECT id::text FROM context_blocks WHERE scope = $2 ORDER BY id OFFSET 4000 LIMIT 2)
			UNION ALL (SELECT id::text FROM context_blocks WHERE scope = $3 ORDER BY id DESC LIMIT 1)`, rrRead, rrOther, rrProj)
		var grant string
		if err := small.QueryRow(ctx, `SELECT id::text FROM context_blocks WHERE scope = $1 ORDER BY id OFFSET 1234 LIMIT 1`, rrOther).Scan(&grant); err != nil {
			t.Fatal(err)
		}
		readers := []struct {
			scopes, grants []string
		}{
			{[]string{rrRead}, nil},
			{[]string{rrOther, rrRead}, nil},
			{[]string{rrProj}, []string{grant}},
		}
		var prefixes []string
		for _, id := range ids {
			for _, n := range []int{8, 9, 12, 13, 14, 17, 18, 19, 22, 23, 24, 30, 35} {
				prefixes = append(prefixes, id[:n])
			}
			// Spellings no id's text starts with: a hyphen moved, one left out.
			prefixes = append(prefixes, id[:8]+id[9:13], id[:7]+"-"+id[8:12], id[:13]+id[14:18])
		}
		prefixes = append(prefixes, grant[:20], "0f0f0f0f", "0f0f0f0f-", "0f0f0f0f-ffff-ffff-ffff-fffffffffff", "0f0f0f0f-0000-0000-0000-00000000000")
		for _, prefix := range prefixes {
			lo, hi, ok := store.IDPrefixRangeForTest(prefix)
			for _, rd := range readers {
				g := rd.grants
				if g == nil {
					g = []string{}
				}
				// Every match of the old text match, without the cap: the new
				// statement must answer as many, the newest first, from them.
				all := list(`SELECT id::text || '|' || updated_at::text FROM context_blocks
					WHERE id::text LIKE $1 || '%' AND NOT is_archived AND (scope = ANY($2::text[]) OR id = ANY($3::uuid[]))
					ORDER BY updated_at DESC`, prefix, rd.scopes, g)
				if !ok {
					same.sets++
					if len(all) != 0 {
						t.Errorf("prefix %q: no range, but the old text match finds %d blocks", prefix, len(all))
					}
					continue
				}
				newSt := rrStmt{sql: rrNewResolveSQL(t, traced, tr, prefix, rd.scopes, rd.grants), args: []any{lo, rd.scopes, 11, g, hi}}
				oldSt := rrStmt{sql: rrOldResolve, args: []any{prefix, rd.scopes, 11, g}}
				same.sets++
				if len(all) > 0 {
					same.nonEmpty++
				}
				for _, pl := range rrPlans {
					got := p.rows(small, pl.rpc, pl.generic, newSt)
					ref := p.rows(small, pl.rpc, pl.generic, oldSt)
					if !rrSameUpToTies(got, ref, all) {
						t.Errorf("prefix %q, reader %v (%s): %d rows against the old statement's %d (%d matches)\n got %v\nwant %v",
							prefix, rd.scopes, rrPlanName(pl.rpc, pl.generic), len(got), len(ref), len(all), got, ref)
					}
				}
			}
		}
	})
	t.Logf("%d parameter sets (%d with rows), each on four plans, 0 differences unless reported above", same.sets, same.nonEmpty)
}

// rrNewResolveSQL traces the statement ResolveBlockID sends for prefix.
func rrNewResolveSQL(t *testing.T, traced *pgxpool.Pool, tr *reviewTracer, prefix string, scopes, grants []string) string {
	t.Helper()
	tr.take()
	if _, _, err := store.ResolveBlockID(context.Background(), traced, prefix, scopes, grants); err != nil && !errors.Is(err, store.ErrAmbiguousID) {
		t.Fatalf("resolve %q: %v", prefix, err)
	}
	return rrFind(t, tr.take(), func(sql string) bool { return strings.Contains(sql, "SELECT id, title, category, tags, scope, updated_at") }).sql
}

// rrSameUpToTies: the two answers of ResolveBlockID's statements hold the
// same number of rows, their updated_at in the same order, each row one of
// the matches. The order holds no tiebreak, so among blocks of one
// updated_at either statement may answer any of them.
func rrSameUpToTies(got, ref, all []string) bool {
	if len(got) != len(ref) {
		return false
	}
	match := map[string]string{} // id -> updated_at of the match
	for _, a := range all {
		parts := strings.SplitN(a, "|", 2)
		match[parts[0]] = parts[1]
	}
	at := func(row string) (string, string) {
		f := strings.Split(row, "|")
		return f[0], f[len(f)-1]
	}
	for i := range got {
		gid, gat := at(got[i])
		_, rat := at(ref[i])
		if gat != rat {
			return false
		}
		if _, ok := match[gid]; !ok {
			return false
		}
	}
	gs, rs := append([]string(nil), got...), append([]string(nil), ref...)
	sort.Strings(gs)
	sort.Strings(rs)
	return len(all) > len(got) || strings.Join(gs, "\n") == strings.Join(rs, "\n")
}
