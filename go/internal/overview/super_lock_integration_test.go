//go:build integration

// W-F gate 7 — THE LOCK WINDOW, and migration 127's idempotency.
//
// Gate 7 asks for proof that the resolution search does not run inside the
// persist transaction. The design phrases it as a lock-DURATION measurement
// ("grows by at most the INSERT time, not by eight Louvain runs"), which on a
// fast host cannot distinguish a correct implementation from a lucky one: eight
// probes over a test-sized fixture take microseconds, and the assertion would be
// a coin toss. The counter below answers the same question categorically —
// between BEGIN and COMMIT the number of Louvain probes is ZERO, at any fixture
// size, on any machine.
//
// This file lives in package `overview` (not `overview_test`) because persist is
// unexported and calling it directly IS the gate: it proves the function cannot
// probe, rather than that it happened not to.
package overview

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

func superLockFixture(t *testing.T, pool *pgxpool.Pool, clusters int) (clustering, map[string]string, []rawEdge) {
	t.Helper()
	ctx := context.Background()
	var nodes []string
	var edges []rawEdge
	scopes := map[string]string{}
	id := func(c, i int) string { return fmt.Sprintf("019d0000-0000-7000-9a00-%09d%03d", c, i) }
	for c := range clusters {
		for i := range 3 {
			b := id(c, i)
			if _, err := pool.Exec(ctx,
				`INSERT INTO context_blocks (id, category, title, content, scope)
				 VALUES ($1::uuid, 'learnings', $2, 'x', 'private')`, b, fmt.Sprintf("c%d b%d", c, i)); err != nil {
				t.Fatalf("insert block: %v", err)
			}
			nodes = append(nodes, b)
			scopes[b] = "private"
		}
		edges = append(edges,
			rawEdge{src: id(c, 0), dst: id(c, 1), weight: 0.95},
			rawEdge{src: id(c, 1), dst: id(c, 2), weight: 0.95})
		if c > 0 {
			edges = append(edges, rawEdge{src: id(c-1, 0), dst: id(c, 0), weight: 0.02})
		}
	}
	for _, e := range edges {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_dream_links (source_block_id, target_block_id, relationship, confidence, raw_confidence, scope)
			 VALUES ($1::uuid, $2::uuid, 'topical', $3, $3, 'private')`, e.src, e.dst, e.weight); err != nil {
			t.Fatalf("insert link: %v", err)
		}
	}
	return computeClustering(nodes, edges, 1.0), scopes, edges
}

// TestSuperLevel_NoProbeInsidePersist is gate 7.
func TestSuperLevel_NoProbeInsidePersist(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	cl, scopes, edges := superLockFixture(t, pool, 8)

	types := []string{"knowledge"}
	opts := Options{Resolution: 1.0, VisibleTypes: types, OverviewTypes: types}
	p := superParams{Enabled: true, TargetRows: 3, MinResolution: 0.05, Resolution: 1.0}

	// Phase 1: the search. It MUST cost probes — otherwise the counter is dead
	// and the assertion below would be vacuously true (the red half of this
	// gate: a probe counter that never moves proves nothing).
	before := superProbes.Load()
	level := computeSuperLevel(ctx, cl, scopes, edges, p)
	spent := superProbes.Load() - before
	if spent == 0 {
		t.Fatal("the resolution search spent no probes — the counter is not wired, so the assertion below would prove nothing")
	}
	if len(level.Groups) == 0 {
		t.Fatal("the search produced no groups")
	}

	// Phase 2: persist, with the FINISHED level. Not one probe may happen here.
	beforeTx := superProbes.Load()
	stats, err := persist(ctx, pool, cl, opts, scopes, tallyScopes(scopes), level)
	if err != nil {
		t.Fatalf("persist: %v", err)
	}
	if stats.Skipped {
		t.Fatalf("persist skipped (%s)", stats.SkipReason)
	}
	if inTx := superProbes.Load() - beforeTx; inTx != 0 {
		t.Errorf("%d Louvain probes ran inside the persist transaction (search spent %d outside) — the γ search moved under the advisory lock (K5)",
			inTx, spent)
	}
	if stats.SuperN != len(level.Groups) {
		t.Errorf("persist reported %d groups for a level of %d", stats.SuperN, len(level.Groups))
	}
}

// TestMigration127Idempotent replays the file body a second time in a rolled-back
// transaction, exactly as the runner holds it (store/migrations.go). The runner
// skips applied versions, so this is the only way to prove a re-run — a fresh
// database, a restore, or a version-table repair — cannot fail on it.
func TestMigration127Idempotent(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	body, err := migrations.FS.ReadFile("127_cluster_super_level.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, string(body)); err != nil {
		t.Fatalf("second apply of 127 failed — not idempotent: %v", err)
	}

	// The lock deckel is the other half of the file's contract: without it the
	// migration waits behind the longest open statement and stacks every reader
	// and writer behind itself (083 header vocabulary).
	if !strings.Contains(string(body), "SET LOCAL lock_timeout") {
		t.Error("migration 127 has no lock_timeout — it can stall the whole database behind a running persist tx")
	}
}
