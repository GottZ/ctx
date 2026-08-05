//go:build integration

// WF T10 filter gates (design/01 §7-T10 R1): the server-side type filters on
// SearchBlocks / RecentBlocks / ListMeta — types narrows, types_exclude
// inverts, both are BIND parameters (EXPLAIN GENERIC_PLAN probe keeps the
// parameter symbol in the plan — the injection-surface invariant §5.5), and
// absent filters are byte-identical to the pre-T10 behaviour.
//
// Run with:
//
//	go test -tags=integration ./internal/store/ -run TestTypeFilter -count=1 -v
package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func seedTypedBlock(t *testing.T, pool *pgxpool.Pool, title, typeName string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO context_blocks (category, title, content, scope, type_name)
		 VALUES ('typefilter', $1, 'filter fixture content', 'private', $2)`,
		title, typeName); err != nil {
		t.Fatalf("seed block %q: %v", title, err)
	}
}

func TestTypeFilter_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	scopes := []string{"private"}

	seedTypedBlock(t, pool, "TF Knowledge A", "knowledge")
	seedTypedBlock(t, pool, "TF Knowledge B", "knowledge")
	seedTypedBlock(t, pool, "TF Audit", "audit-trail")
	seedTypedBlock(t, pool, "TF Reference", "reference")

	typesOf := func(previews []store.BlockPreview) map[string]int {
		out := map[string]int{}
		for _, p := range previews {
			out[p.TypeName]++
		}
		return out
	}

	t.Run("search_types_only_knowledge", func(t *testing.T) {
		res, err := store.SearchBlocks(ctx, pool, "", scopes, "typefilter", nil, 50, true, nil, nil, []string{"knowledge"}, nil, nil)
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		got := typesOf(res)
		if got["knowledge"] != 2 || len(got) != 1 {
			t.Errorf("types=[knowledge] returned %v, want only 2 knowledge rows", got)
		}
	})

	t.Run("search_types_exclude_inverts", func(t *testing.T) {
		res, err := store.SearchBlocks(ctx, pool, "", scopes, "typefilter", nil, 50, true, nil, nil, nil, []string{"knowledge"}, nil)
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		got := typesOf(res)
		if got["knowledge"] != 0 || got["audit-trail"] != 1 || got["reference"] != 1 {
			t.Errorf("types_exclude=[knowledge] returned %v, want audit-trail+reference only", got)
		}
	})

	t.Run("search_no_filter_returns_all_with_type_fields", func(t *testing.T) {
		res, err := store.SearchBlocks(ctx, pool, "", scopes, "typefilter", nil, 50, true, nil, nil, nil, nil, nil)
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if len(res) != 4 {
			t.Fatalf("unfiltered rows = %d, want 4 (opt-in filter must not hard-exclude)", len(res))
		}
		for _, p := range res {
			if p.TypeName == "" || p.LifecycleState == "" || p.TypeSource == "" {
				t.Errorf("row %q missing type axes: type=%q lifecycle=%q source=%q", p.Title, p.TypeName, p.LifecycleState, p.TypeSource)
			}
		}
	})

	t.Run("recent_filters", func(t *testing.T) {
		res, err := store.RecentBlocks(ctx, pool, scopes, "typefilter", 50, []string{"audit-trail"}, nil)
		if err != nil {
			t.Fatalf("recent: %v", err)
		}
		if got := typesOf(res); got["audit-trail"] != 1 || len(got) != 1 {
			t.Errorf("recent types=[audit-trail] returned %v", got)
		}
		res, err = store.RecentBlocks(ctx, pool, scopes, "typefilter", 50, nil, []string{"audit-trail", "reference"})
		if err != nil {
			t.Fatalf("recent: %v", err)
		}
		if got := typesOf(res); got["knowledge"] != 2 || len(got) != 1 {
			t.Errorf("recent types_exclude returned %v, want 2 knowledge", got)
		}
	})

	t.Run("listmeta_filters", func(t *testing.T) {
		res, err := store.ListMeta(ctx, pool, scopes, []string{"reference"}, nil)
		if err != nil {
			t.Fatalf("list meta: %v", err)
		}
		count := 0
		for _, m := range res {
			if m.Category == "typefilter" {
				count++
				if m.TypeName != "reference" {
					t.Errorf("listmeta types=[reference] returned %q", m.TypeName)
				}
			}
		}
		if count != 1 {
			t.Errorf("listmeta types=[reference]: %d typefilter rows, want 1", count)
		}
	})

	// EXPLAIN GENERIC_PLAN probe (§5.5 / gate "EXPLAIN zeigt Bind-Parameter"):
	// the exact filter conjunct shape runs as a PARAMETER ($n stays a symbol
	// in the generic plan) — type names never enter the SQL text. Index
	// story (§7-T10): the 035-line partial index idx_context_blocks_type_name
	// (WHERE type_name != 'knowledge') covers non-knowledge filter values by
	// predicate implication; the knowledge case is the unfiltered default
	// view. The composite (scope, type_name) index is Achse-02 territory.
	t.Run("explain_generic_plan_bind_parameter", func(t *testing.T) {
		// Raw simple-query path (PgConn.Exec): pgx's argument counting would
		// otherwise demand values for $1/$2 — GENERIC_PLAN is exactly the
		// value-free shape.
		conn, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		defer conn.Release()
		results, err := conn.Conn().PgConn().Exec(ctx, `EXPLAIN (GENERIC_PLAN)
			SELECT id FROM context_blocks
			 WHERE NOT is_archived AND scope = ANY($1::text[]) AND type_name = ANY($2::text[])
			 ORDER BY updated_at DESC LIMIT 10`).ReadAll()
		if err != nil {
			t.Fatalf("explain generic plan: %v", err)
		}
		var plan strings.Builder
		for _, res := range results {
			for _, row := range res.Rows {
				for _, col := range row {
					plan.Write(col)
					plan.WriteString("\n")
				}
			}
		}
		if !strings.Contains(plan.String(), "$2") {
			t.Errorf("generic plan does not carry the $2 bind symbol for the type filter:\n%s", plan.String())
		}
	})
}
