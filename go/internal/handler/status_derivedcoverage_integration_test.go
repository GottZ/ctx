//go:build integration

// Integration test for Achse-01 W01-6 (design/01 §4.7.4): the derived_coverage
// section of /api/status. The abort rule demands the coverage gap as an
// OPERATING FIGURE — visible on the status surface, not a state an operator has
// to reconstruct from four columns after the fact (the label arm's 42 %
// dead-end, §4.7.4, became visible only when someone went looking).
//
//	go test -tags=integration ./internal/handler/ -run TestStatusDerivedCoverage -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/derived"
	"github.com/GottZ/ctx/internal/events"
	"github.com/GottZ/ctx/internal/testdb"
)

// fakeCoverageDreams satisfies dreamModeSource only — no recallRunSource, no
// graphCacheSource. The coverage section must appear WITHOUT any scheduler
// slice being wired: it reads the corpus, not an arm.
type fakeCoverageDreams struct{}

func (fakeCoverageDreams) GetDreamMode() (int32, time.Duration) { return events.DreamModeOff, 0 }

func TestStatusDerivedCoverageSection(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	newCollector := func() *StatusCollector {
		return NewStatusCollector(pool, backends.NewPool(nil, nil), fakeCoverageDreams{},
			config.NewStore(&config.Config{}), nil, nil)
	}

	// -----------------------------------------------------------------------
	// Gate 2: an EMPTY corpus. Both numbers 0 per derived type, section present.
	// -----------------------------------------------------------------------
	t.Run("gate2_empty_corpus_is_zero_zero_not_an_absent_section", func(t *testing.T) {
		snap := newCollector().Snapshot(ctx)
		if snap.DerivedCoverage == nil {
			t.Fatal("derived_coverage absent on the server-admin path — the gap must be a figure, not an absence")
		}
		rows := *snap.DerivedCoverage
		if len(rows) != len(derived.DerivedTypeNames()) {
			t.Fatalf("derived_coverage rows = %d, want %d (one per derived type)",
				len(rows), len(derived.DerivedTypeNames()))
		}
		for _, r := range rows {
			if r.Blocks != 0 || r.AnchorMissed != 0 {
				t.Errorf("%s = blocks %d / anchor_missed %d on an empty corpus, want 0 / 0",
					r.Type, r.Blocks, r.AnchorMissed)
			}
		}
	})

	t.Run("wire_shape_carries_both_numbers_under_stable_keys", func(t *testing.T) {
		snap := newCollector().Snapshot(ctx)
		b, err := json.Marshal(snap)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var frame map[string]json.RawMessage
		if err := json.Unmarshal(b, &frame); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		raw, ok := frame["derived_coverage"]
		if !ok {
			t.Fatal(`the status frame has no "derived_coverage" key`)
		}
		var rows []map[string]any
		if err := json.Unmarshal(raw, &rows); err != nil {
			t.Fatalf("derived_coverage is not a list of objects: %v", err)
		}
		for _, r := range rows {
			for _, k := range []string{"type", "blocks", "anchor_missed"} {
				if _, ok := r[k]; !ok {
					t.Errorf("derived_coverage row %v has no %q key", r, k)
				}
			}
		}
	})

	// -----------------------------------------------------------------------
	// Gate 1: a corpus WITH a drifting anchor must report a non-zero gap through
	// the whole chain — corpus → store.DerivedCoverage → status frame.
	// -----------------------------------------------------------------------
	t.Run("gate1_a_drifting_anchor_reaches_the_status_frame", func(t *testing.T) {
		var topicID string
		if err := pool.QueryRow(ctx,
			`INSERT INTO graph_cluster_topic (scope) VALUES ('private') RETURNING topic_id::text`).Scan(&topicID); err != nil {
			t.Fatalf("seed topic: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO graph_cluster_node (cluster_id, scope, size, repr_block_id, repr_title, topic_id, core_hash)
			 VALUES (gen_random_uuid(), 'private', 5, gen_random_uuid(), 'repr', $1::uuid, 'core-CURRENT')`,
			topicID); err != nil {
			t.Fatalf("seed node: %v", err)
		}
		mk := func(title, coreHash string) {
			if _, err := pool.Exec(ctx,
				`INSERT INTO context_blocks (category, title, content, scope, type_name, metadata)
				 VALUES ('learnings', $1, 'body', 'private', $2, $3::jsonb)`,
				title, derived.TypeCatalog,
				`{"provenance":{"v":1,"stratum":1,"arm":"w016","anchor":{"kind":"cluster_topic","topic_id":"`+
					topicID+`","core_hash":"`+coreHash+`"}}}`); err != nil {
				t.Fatalf("seed block %q: %v", title, err)
			}
		}
		mk("Katalog covered", "core-CURRENT")
		mk("Katalog stale", "core-STALE")

		snap := newCollector().Snapshot(ctx)
		if snap.DerivedCoverage == nil {
			t.Fatal("derived_coverage absent")
		}
		var cat *derivedCoverageRow
		for i := range *snap.DerivedCoverage {
			if (*snap.DerivedCoverage)[i].Type == derived.TypeCatalog {
				cat = &(*snap.DerivedCoverage)[i]
			}
		}
		if cat == nil {
			t.Fatalf("no catalog row in derived_coverage: %+v", *snap.DerivedCoverage)
		}
		if cat.Blocks != 2 {
			t.Errorf("catalog blocks = %d, want 2", cat.Blocks)
		}
		if cat.AnchorMissed != 1 {
			t.Errorf("catalog anchor_missed = %d, want 1 (one block still points at the old core)", cat.AnchorMissed)
		}
	})

	// -----------------------------------------------------------------------
	// §5.3 / the Recall+ClusterMap posture: per-scope corpus sizes are
	// server-global observability. A tenant snapshot must not carry them.
	// -----------------------------------------------------------------------
	t.Run("the_tenant_path_never_carries_the_section", func(t *testing.T) {
		snap := newCollector().SnapshotForTenant(ctx, "private", []string{"private"})
		if snap.DerivedCoverage != nil {
			t.Errorf("derived_coverage present on the per-tenant path: %+v — corpus sizes go tenants nothing",
				*snap.DerivedCoverage)
		}
	})
}
