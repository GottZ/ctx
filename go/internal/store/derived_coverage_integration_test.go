//go:build integration

// Integration test for Achse-01 W01-6 (design/01 §4.7.4): the coverage counter
// behind the /api/status operating figure. Per derived type name it must report
// how many active blocks of that type exist and how many of those MISS their
// drift anchor — the gap the abort rule (§4.7.4) demands as a NUMBER instead of
// a state one has to reconstruct from four columns.
//
//	go test -tags=integration ./internal/store/ -run TestDerivedCoverage -count=1 -v
package store_test

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/derived"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// w016CovByType indexes a coverage report by type name so a subtest can assert on
// one row without depending on the slice order.
func w016CovByType(t *testing.T, rows []store.DerivedCoverageRow) map[string]store.DerivedCoverageRow {
	t.Helper()
	m := make(map[string]store.DerivedCoverageRow, len(rows))
	for _, r := range rows {
		if _, dup := m[r.TypeName]; dup {
			t.Fatalf("duplicate row for type %q — the join fans out and the counts are wrong", r.TypeName)
		}
		m[r.TypeName] = r
	}
	return m
}

// w016SeedTopic creates a cluster topic plus its node row carrying coreHash — the
// CURRENT core of that topic (§4.7.3: the drift anchor is compared against
// graph_cluster_node.core_hash, never against label_stale, which the label arm
// owns and clears).
func w016SeedTopic(t *testing.T, ctx context.Context, pool *pgxpool.Pool, scope, coreHash string) string {
	t.Helper()
	var topicID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO graph_cluster_topic (scope) VALUES ($1) RETURNING topic_id::text`,
		scope).Scan(&topicID); err != nil {
		t.Fatalf("seed topic: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO graph_cluster_node
		   (cluster_id, scope, size, repr_block_id, repr_title, topic_id, core_hash)
		 VALUES (gen_random_uuid(), $1, 5, gen_random_uuid(), 'repr', $2::uuid, $3)`,
		scope, topicID, coreHash); err != nil {
		t.Fatalf("seed node: %v", err)
	}
	return topicID
}

// w016SeedBlock inserts one block with a raw metadata JSON document. Raw JSON on
// purpose: the counter has to survive metadata this build did NOT write (a
// foreign contract version, a missing provenance, a broken anchor kind), and
// those shapes are not constructible through the typed writer.
func w016SeedBlock(t *testing.T, ctx context.Context, pool *pgxpool.Pool, typeName, title, metaJSON string, archived bool) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_blocks (category, title, content, scope, type_name, metadata, is_archived)
		 VALUES ('learnings', $1, 'body', 'private', $2, $3::jsonb, $4)`,
		title, typeName, metaJSON, archived); err != nil {
		t.Fatalf("seed block %q: %v", title, err)
	}
}

func TestDerivedCoverage_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// ---------------------------------------------------------------------
	// Gate 2 of the briefing, first half: an EMPTY corpus reports a row per
	// derived type with both numbers 0 — not an empty slice and not an error.
	// A missing row is indistinguishable from "the arm has full coverage".
	// ---------------------------------------------------------------------
	t.Run("empty_corpus_reports_zero_zero_per_derived_type", func(t *testing.T) {
		rows, err := store.DerivedCoverage(ctx, pool)
		if err != nil {
			t.Fatalf("DerivedCoverage on an empty corpus must not fail: %v", err)
		}
		got := w016CovByType(t, rows)
		for _, name := range derived.DerivedTypeNames() {
			r, ok := got[name]
			if !ok {
				t.Fatalf("no row for derived type %q — a missing row reads as full coverage", name)
			}
			if r.Blocks != 0 || r.AnchorMissed != 0 {
				t.Errorf("%s on an empty corpus = blocks %d / missed %d, want 0 / 0", name, r.Blocks, r.AnchorMissed)
			}
		}
		if len(rows) != len(derived.DerivedTypeNames()) {
			t.Errorf("rows = %d, want exactly one per derived type (%d) — no originals in the report",
				len(rows), len(derived.DerivedTypeNames()))
		}
	})

	// ---------------------------------------------------------------------
	// The fixture corpus. Every block below is deliberate; the comment on each
	// says which half of the counter it holds down.
	// ---------------------------------------------------------------------
	fresh := w016SeedTopic(t, ctx, pool, "private", "core-aaa")
	drifted := w016SeedTopic(t, ctx, pool, "private", "core-NEW") // node moved on

	prov := func(kind, body string) string {
		return `{"provenance":{"v":1,"stratum":1,"arm":"w016-fixture","anchor":{"kind":"` + kind + `"` + body + `}}}`
	}

	// (1) catalog, anchor matches the topic's CURRENT core → covered.
	w016SeedBlock(t, ctx, pool, derived.TypeCatalog, "Katalog fresh",
		prov("cluster_topic", `,"topic_id":"`+fresh+`","core_hash":"core-aaa"`), false)
	// (2) catalog, stored core_hash is the OLD one → drift → missed.
	w016SeedBlock(t, ctx, pool, derived.TypeCatalog, "Katalog drifted",
		prov("cluster_topic", `,"topic_id":"`+drifted+`","core_hash":"core-OLD"`), false)
	// (3) catalog anchored on a topic that no longer has a live node (retired /
	//     torn down, §4.7.5) → the anchor cannot be re-found → missed.
	w016SeedBlock(t, ctx, pool, derived.TypeCatalog, "Katalog orphan",
		prov("cluster_topic", `,"topic_id":"11111111-1111-4111-8111-111111111111","core_hash":"core-aaa"`), false)
	// (4) catalog whose metadata carries NO provenance at all → its freshness is
	//     not provable, so it counts as missed (fail-closed, §4.5.3 posture).
	w016SeedBlock(t, ctx, pool, derived.TypeCatalog, "Katalog bare", `{"note":"no provenance"}`, false)
	// (5) catalog with a FOREIGN contract version → same posture: this build
	//     cannot read the anchor, so it must not claim the block is covered.
	w016SeedBlock(t, ctx, pool, derived.TypeCatalog, "Katalog v2",
		`{"provenance":{"v":2,"anchor":{"kind":"cluster_topic","topic_id":"`+fresh+`","core_hash":"core-aaa"}}}`, false)
	// (6) catalog with an anchor kind outside the V7 vocabulary → missed.
	w016SeedBlock(t, ctx, pool, derived.TypeCatalog, "Katalog junk",
		prov("moonbeam", `,"topic_id":"`+fresh+`"`), false)
	// (7) ARCHIVED and drifting → out of the population entirely (§4.7.5: the
	//     arm archives, and an archived block is not an uncovered block).
	w016SeedBlock(t, ctx, pool, derived.TypeCatalog, "Katalog archived",
		prov("cluster_topic", `,"topic_id":"`+drifted+`","core_hash":"core-OLD"`), true)
	// (8) insight on a root_session anchor. Its identity anchor is root session ×
	//     watermark — strictly monotone, never dies (§4.1 table) — so it has no
	//     core drift to miss. It counts in blocks, never in missed.
	w016SeedBlock(t, ctx, pool, derived.TypeInsight, "Session insights A",
		prov("root_session", `,"root_session_id":"sess-1","watermark_from":7`), false)
	// (9) an ORIGINAL block must not appear in the report at all.
	w016SeedBlock(t, ctx, pool, "knowledge", "an original", `{}`, false)

	t.Run("gate1_a_drifting_anchor_is_counted_a_fresh_one_is_not", func(t *testing.T) {
		rows, err := store.DerivedCoverage(ctx, pool)
		if err != nil {
			t.Fatalf("DerivedCoverage: %v", err)
		}
		got := w016CovByType(t, rows)

		cat := got[derived.TypeCatalog]
		// 6 active catalog blocks (the archived one is out).
		if cat.Blocks != 6 {
			t.Errorf("catalog blocks = %d, want 6 (7 seeded, 1 archived is out of the population)", cat.Blocks)
		}
		// 5 of them miss: drifted, orphan, bare, v2, junk. Only "fresh" is covered.
		if cat.AnchorMissed != 5 {
			t.Errorf("catalog anchor_missed = %d, want 5 (drifted+orphan+bare+v2+junk; only the matching core is covered)",
				cat.AnchorMissed)
		}
		if cat.AnchorMissed == 0 {
			t.Error("the whole point of §4.7.4: a corpus with a drifting anchor must NOT report a zero gap")
		}

		ins := got[derived.TypeInsight]
		if ins.Blocks != 1 {
			t.Errorf("insight blocks = %d, want 1", ins.Blocks)
		}
		if ins.AnchorMissed != 0 {
			t.Errorf("insight anchor_missed = %d, want 0 — a root_session anchor is strictly monotone and cannot drift (§4.1)",
				ins.AnchorMissed)
		}
	})

	t.Run("originals_never_enter_the_report", func(t *testing.T) {
		rows, err := store.DerivedCoverage(ctx, pool)
		if err != nil {
			t.Fatalf("DerivedCoverage: %v", err)
		}
		for _, r := range rows {
			if derived.StratumOf(r.TypeName) == derived.StratumSource {
				t.Errorf("type %q is an original (stratum 0) and must not be in the coverage report", r.TypeName)
			}
		}
	})

	// ---------------------------------------------------------------------
	// Gate 1, second half: once the topic's core is moved BACK onto the stored
	// value, the same block is covered again — the counter reads the live core,
	// it does not latch.
	// ---------------------------------------------------------------------
	t.Run("regenerating_the_core_closes_the_gap", func(t *testing.T) {
		if _, err := pool.Exec(ctx,
			`UPDATE graph_cluster_node SET core_hash = 'core-OLD' WHERE topic_id = $1::uuid`, drifted); err != nil {
			t.Fatalf("move core back: %v", err)
		}
		t.Cleanup(func() {
			_, _ = pool.Exec(ctx,
				`UPDATE graph_cluster_node SET core_hash = 'core-NEW' WHERE topic_id = $1::uuid`, drifted)
		})
		rows, err := store.DerivedCoverage(ctx, pool)
		if err != nil {
			t.Fatalf("DerivedCoverage: %v", err)
		}
		cat := w016CovByType(t, rows)[derived.TypeCatalog]
		if cat.AnchorMissed != 4 {
			t.Errorf("catalog anchor_missed after the core matches again = %d, want 4 (the drifted one is covered now)",
				cat.AnchorMissed)
		}
	})
}

// TestDerivedTypeNames_Integration pins the ONE source of the derived type set
// against StratumOf — so the counter cannot silently stop reporting a type that
// the derivation order already treats as derived.
func TestDerivedTypeNames_Integration(t *testing.T) {
	names := derived.DerivedTypeNames()
	if len(names) == 0 {
		t.Fatal("DerivedTypeNames is empty — the coverage report would have nothing to say")
	}
	for _, n := range names {
		if derived.StratumOf(n) == derived.StratumSource {
			t.Errorf("DerivedTypeNames carries %q, but StratumOf says it is an original", n)
		}
	}
	for _, n := range []string{derived.TypeInsight, derived.TypeCatalog} {
		found := false
		for _, m := range names {
			if m == n {
				found = true
			}
		}
		if !found {
			t.Errorf("StratumOf(%q) > 0 but DerivedTypeNames omits it — the type would vanish from /api/status", n)
		}
	}
	// The returned slice must not alias package state: a caller that sorts or
	// truncates it must not be able to shrink the status surface.
	names[0] = "mutated"
	if derived.DerivedTypeNames()[0] == "mutated" {
		t.Error("DerivedTypeNames returns the package slice itself — a caller can edit the status surface")
	}
}
