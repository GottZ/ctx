//go:build integration

// Issue #426: ClassifyBlockAfterUpsert writes a verdict only while the block
// still holds the inputs it was computed from — the title and the part of the
// metadata Set.Classify reads (the source as a string, the flags of the set as
// true). Every other part of the row may differ. The handler tests
// (handler/update_reclassify_stale_integration_test.go) drive the interleaving
// through the production write paths; this file pins the comparison itself.
//
// Run with:
//
//	go test -tags=integration ./internal/store/ -run TestClassifyVerdictNeedsItsInputs -count=1 -v
package store_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// i426OverlaySet is the builtin knowledge, audit-trail and system-meta types,
// with system-meta's classify rules overlaid the way a tenant may overlay them
// (classify.* is freely overlayable): one more metadata flag, i426_overlay.
func i426OverlaySet(t *testing.T) *blocktype.Set {
	t.Helper()
	var policies []blocktype.Policy
	for _, name := range []string{"knowledge", "audit-trail", "system-meta"} {
		p, ok := blocktype.BuiltinPolicy(name)
		if !ok {
			t.Fatalf("builtin policy %q missing", name)
		}
		if name == "system-meta" {
			p.Classify.MetadataFlags = append(slices.Clone(p.Classify.MetadataFlags), "i426_overlay")
		}
		policies = append(policies, p)
	}
	set, err := blocktype.NewSet(policies)
	if err != nil {
		t.Fatalf("overlay set: %v", err)
	}
	return set
}

func TestClassifyVerdictNeedsItsInputs_Integration(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	base := dbRegistry(t, ctx, pool).Snapshot()
	overlay := i426OverlaySet(t)

	const audit = "Session i426 Handover"
	cases := []struct {
		name      string
		set       *blocktype.Set
		rowTitle  string
		rowMeta   map[string]any // nil = SQL NULL
		inTitle   string
		inMeta    map[string]any
		wantType  string // "" = nothing written, the row stays knowledge
		wantFinal string
	}{
		{"inputs held", base, audit, map[string]any{}, audit, map[string]any{}, "audit-trail", "audit-trail"},
		{"title replaced", base, "i426 plain notes", map[string]any{}, audit, map[string]any{}, "", "knowledge"},
		{"source replaced", base, "i426 notes", map[string]any{"source": "claude-code-i426"},
			"i426 notes", map[string]any{"source": "dream-i426"}, "", "knowledge"},
		{"flag added", base, audit, map[string]any{"is_meta": true}, audit, map[string]any{}, "", "knowledge"},
		// The overlay's flag is an input of the overlay's verdict only.
		{"overlay flag added, overlay set", overlay, "i426 notes", map[string]any{"source": "dream-i426", "i426_overlay": true},
			"i426 notes", map[string]any{"source": "dream-i426"}, "", "knowledge"},
		{"overlay flag added, base set", base, "i426 notes", map[string]any{"source": "dream-i426", "i426_overlay": true},
			"i426 notes", map[string]any{"source": "dream-i426"}, "audit-trail", "audit-trail"},
		// Keys no rule reads, a flag that is false on one side and absent on
		// the other, a non-string source and a string "true" are no inputs.
		{"unread keys differ", base, "i426 notes",
			map[string]any{"source": "dream-i426", "note": "row", "pages": 3, "is_meta": false, "guard_matched_id": "x"},
			"i426 notes", map[string]any{"source": "dream-i426", "note": "write"}, "audit-trail", "audit-trail"},
		{"non-string source", base, audit, map[string]any{"source": 7}, audit, map[string]any{}, "audit-trail", "audit-trail"},
		{"flag not true", base, audit, map[string]any{"is_meta": "true"}, audit, map[string]any{"is_meta": "true"}, "audit-trail", "audit-trail"},
		{"null metadata", base, audit, nil, audit, nil, "audit-trail", "audit-trail"},
		// The comparison sees the characters, not an encoding of them.
		{"multi-byte and escaped", base, "Übergabe \"i426\" <a&b>", map[string]any{"source": "dream-ü \"q\" <&>\n"},
			"Übergabe \"i426\" <a&b>", map[string]any{"source": "dream-ü \"q\" <&>\n"}, "audit-trail", "audit-trail"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A category of its own per case: the titles repeat across cases.
			var id string
			if err := pool.QueryRow(ctx,
				`INSERT INTO context_blocks (category, title, content, scope, metadata)
				 VALUES ('test_classify_i426_' || $3::int, $1, 'i426', 'private', $2) RETURNING id::text`,
				tc.rowTitle, tc.rowMeta, i).Scan(&id); err != nil {
				t.Fatalf("insert: %v", err)
			}
			if want, matched := tc.set.Classify(tc.inTitle, tc.inMeta); !matched || want == "knowledge" {
				t.Fatalf("fixture: the inputs classify to (%q, %v) — the case needs a verdict to write", want, matched)
			}
			got, err := store.ClassifyBlockAfterUpsert(ctx, pool, tc.set, id, tc.inTitle, tc.inMeta)
			if err != nil {
				t.Fatalf("classify: %v", err)
			}
			if got != tc.wantType {
				t.Errorf("applied %q, want %q", got, tc.wantType)
			}
			var name string
			if err := pool.QueryRow(ctx, `SELECT type_name FROM context_blocks WHERE id = $1::uuid`, id).Scan(&name); err != nil {
				t.Fatalf("read type: %v", err)
			}
			if name != tc.wantFinal {
				t.Errorf("type_name = %q, want %q", name, tc.wantFinal)
			}
		})
	}

	// A manual block holding the inputs stays as it is.
	t.Run("manual", func(t *testing.T) {
		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO context_blocks (category, title, content, scope, type_name, type_source)
			 VALUES ('test_classify_i426', $1, 'i426', 'private', 'reference', 'manual') RETURNING id::text`,
			audit+" manual").Scan(&id); err != nil {
			t.Fatalf("insert: %v", err)
		}
		if got, err := store.ClassifyBlockAfterUpsert(ctx, pool, base, id, audit+" manual", nil); err != nil || got != "" {
			t.Errorf("classify of a manual block = (%q, %v), want (\"\", nil)", got, err)
		}
	})
}

// TestClassifyUpdatePlanUsesPrimaryKey_Integration pins that the guarded
// UPDATE reaches the row by its primary key alone. A bare `title = $n` beside
// `id = $2` is a condition the trigram indexes on title support; with mostly
// distinct titles (the shape of a real corpus, here 2000 rows written in one
// statement and analysed) the generic plan of that form scans
// idx_trgm_title_gist with the id as a filter.
func TestClassifyUpdatePlanUsesPrimaryKey_Integration(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_blocks (category, title, content, scope, metadata)
		 SELECT 'test_classify_plan', 'Notes on topic ' || md5(g::text), 'i426 ' || g, 'private', '{"source": "cli"}'
		   FROM generate_series(1, 2000) g`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := pool.Exec(ctx, `ANALYZE context_blocks`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	// The simple protocol sends the statement with its placeholders unbound.
	res, err := conn.Conn().PgConn().Exec(ctx, `EXPLAIN (GENERIC_PLAN) `+store.ClassifyUpdateSQLForTest).ReadAll()
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	var plan []string
	for _, r := range res[0].Rows {
		plan = append(plan, string(r[0]))
	}
	text := strings.Join(plan, "\n")
	var conds []string
	for _, l := range plan {
		if l = strings.TrimSpace(l); strings.HasPrefix(l, "Index Cond:") {
			conds = append(conds, l)
		}
	}
	if !strings.Contains(text, "Index Scan using context_blocks_pkey on context_blocks") ||
		!slices.Equal(conds, []string{"Index Cond: (id = $2)"}) {
		t.Errorf("the guarded classify UPDATE must reach the row by its primary key only, plan:\n%s", text)
	}
}
