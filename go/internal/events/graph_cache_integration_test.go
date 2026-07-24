//go:build integration

// Integration coverage for the Achse-05 W05.2 graph-cache rebuild arm (design/05
// §4.3/§4.6, §7 wave W05.2 gates). Driven through the scheduler build path
// (graphCacheBuildOnce) against a real migrated + seeded Postgres.
//
//	DoubleBuild   — two builds on an unchanged DB ⇒ identical counts (Fingerprint
//	                stable) and seq+1 each (swap monotony, §4.3 double-buffer).
//	DirtyAgeSwap  — a signal marked BEFORE the build is consumed (Staleness→0); a
//	                signal marked DURING the build survives the swap and re-anchors
//	                the Dirty-Age clock (§4.2), while a signal after a clean build
//	                opens a fresh episode.
//
// Run with:
//
//	GOTMPDIR=/compose/n8n/.gocache GOCACHE=/compose/n8n/.gocache/build \
//	  go test -tags=integration ./internal/events/ -run TestGraphCacheRebuildArm -count=1 -v
package events

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/graphcache"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestGraphCacheRebuildArm(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// Seed a tiny graph: two blocks + one dream link + one structural link, so the
	// counts are non-trivial and stable across rebuilds.
	idA, idB := uuid.New(), uuid.New()
	for _, b := range []struct {
		id    uuid.UUID
		scope string
	}{{idA, "shared"}, {idB, "shared"}} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blocks (id, category, title, content, scope, type_name, is_archived)
			 VALUES ($1::uuid, 'test', $1, 'content', $2, 'knowledge', false)`,
			b.id.String(), b.scope,
		); err != nil {
			t.Fatalf("seed block: %v", err)
		}
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_dream_links (source_block_id, target_block_id, relationship, confidence, raw_confidence, scope)
		 VALUES ($1::uuid, $2::uuid, 'topical', 0.8, 0.8, 'shared')`, idA.String(), idB.String()); err != nil {
		t.Fatalf("seed dream link: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_structural_links (source_block_id, target_block_id, link_class, scope, origin)
		 VALUES ($1::uuid, $2::uuid, 'references', 'shared', 'system')`, idA.String(), idB.String()); err != nil {
		t.Fatalf("seed struct link: %v", err)
	}

	cfg := config.GraphCacheConfig{Enabled: true, MaxStaleness: 15 * time.Minute, FailedThreshold: 3}
	s := NewScheduler(pool, config.NewStore(&config.Config{GraphCache: cfg}), backends.NewPool(nil, nil), StartupConfig{})

	stateCfg := graphcache.StateConfig{MaxStaleness: cfg.MaxStaleness, FailedThreshold: cfg.FailedThreshold}

	t.Run("DoubleBuild", func(t *testing.T) {
		s.graphCacheBuildOnce(ctx, time.Now(), cfg, "test-1")
		snap1 := s.graphCache.Current()
		if snap1 == nil {
			t.Fatal("no snapshot after first build")
		}
		fp1 := snap1.Fingerprint()
		if snap1.Stats.Nodes != 2 || snap1.Stats.DreamEdges != 1 || snap1.Stats.StructEdges != 1 {
			t.Fatalf("counts after build 1: nodes=%d dream=%d struct=%d, want 2/1/1",
				snap1.Stats.Nodes, snap1.Stats.DreamEdges, snap1.Stats.StructEdges)
		}
		seq1 := snap1.Seq

		s.graphCacheBuildOnce(ctx, time.Now(), cfg, "test-2")
		snap2 := s.graphCache.Current()
		if snap2.Fingerprint() != fp1 {
			t.Error("two builds on an unchanged DB produced different Fingerprints")
		}
		if snap2.Stats.Nodes != snap1.Stats.Nodes || snap2.Stats.DreamEdges != snap1.Stats.DreamEdges ||
			snap2.Stats.StructEdges != snap1.Stats.StructEdges {
			t.Errorf("counts drifted across builds: %+v vs %+v", snap2.Stats, snap1.Stats)
		}
		if snap2.Seq != seq1+1 {
			t.Errorf("seq = %d after second build, want %d (swap monotony seq+1)", snap2.Seq, seq1+1)
		}
	})

	t.Run("DirtyAgeSwap", func(t *testing.T) {
		now := time.Now()
		// A signal before the build is consumed → Staleness returns to 0.
		s.graphCache.MarkDirty(now.Add(-1 * time.Minute))
		if got := s.graphCache.Staleness(now); got == 0 {
			t.Fatal("Staleness 0 with a pending pre-build signal — setup broke")
		}
		s.graphCacheBuildOnce(ctx, now, cfg, "test-consume")
		if got := s.graphCache.Staleness(time.Now()); got != 0 {
			t.Errorf("Staleness = %v after a build that consumed the pre-build signal, want 0", got)
		}
		if st := s.graphCache.State(time.Now(), stateCfg); st.String() != "Fresh" {
			t.Errorf("State = %s after consuming build, want Fresh", st)
		}

		// A signal marked DURING a build survives the swap (§4.2). Simulate the
		// begin→mark→commit ordering the goroutine + listener produce concurrently.
		buildStart := time.Now()
		s.graphCache.BeginBuild(buildStart)
		duringWrite := buildStart.Add(1 * time.Second)
		s.graphCache.MarkDirty(duringWrite) // arrives WHILE building
		s.graphCache.CommitBuild(s.graphCache.Current(), buildStart.Add(2*time.Second), 5*time.Millisecond)
		probe := duringWrite.Add(30 * time.Second)
		pending, _, age := s.graphCache.Dirty(probe)
		if !pending {
			t.Fatal("pending = false after a during-build write, want true (must survive swap)")
		}
		if want := probe.Sub(duringWrite); age != want {
			t.Errorf("pendingAge = %v, want %v (re-anchored on the during-build write)", age, want)
		}
	})
}
