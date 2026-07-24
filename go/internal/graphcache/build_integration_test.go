//go:build integration

// Integration coverage for W05.1: the Build path against a real, migrated,
// seeded Postgres (testcontainers). Exercises the three-stream read, the
// archived-in-universe rule, both edge classes, NodeID binary search, the
// raw-vs-filtered degree, InducedEdges against a hand-computed expectation, the
// determinism (byte-equal Fingerprint) over a real DB, and the dangling-edge
// counter (injected via session_replication_role=replica, the only way past the
// FK that otherwise forbids a dangling row).
//
// Run with:
//
//	GOTMPDIR=/compose/n8n/.gocache GOCACHE=/compose/n8n/.gocache/build \
//	  go test -tags=integration ./internal/graphcache/ -count=1 -v
package graphcache_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/GottZ/ctx/internal/graphcache"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestBuildAgainstSeededDB(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	idA := uuid.New()
	idB := uuid.New()
	idC := uuid.New()
	idD := uuid.New()

	blocks := []struct {
		id       uuid.UUID
		scope    string
		typeName string
		archived bool
	}{
		{idA, "shared", "knowledge", false},
		{idB, "private", "knowledge", false},
		{idC, "shared", "note", true}, // archived — must still land in the universe
		{idD, "shared", "knowledge", false},
	}
	for _, b := range blocks {
		if _, err := pool.Exec(ctx,
			// Title carries the id — uq_context_category_title_scope(category,
			// title, scope) forbids duplicate (test, title, scope) tuples.
			`INSERT INTO context_blocks (id, category, title, content, scope, type_name, is_archived)
			 VALUES ($1::uuid, 'test', $1, 'content', $2, $3, $4)`,
			b.id.String(), b.scope, b.typeName, b.archived,
		); err != nil {
			t.Fatalf("seed block %v: %v", b.id, err)
		}
	}

	// Dream links: A->B (raw 0.3), A->C (raw 0.9), C->A (raw 0.5).
	dream := [][3]any{
		{idA.String(), idB.String(), 0.3},
		{idA.String(), idC.String(), 0.9},
		{idC.String(), idA.String(), 0.5},
	}
	for _, d := range dream {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_dream_links
			   (source_block_id, target_block_id, relationship, confidence, raw_confidence, scope)
			 VALUES ($1::uuid, $2::uuid, 'topical', $3, $3, 'shared')`,
			d[0], d[1], d[2],
		); err != nil {
			t.Fatalf("seed dream link: %v", err)
		}
	}

	// Structural links: A->D (references/system), A->C (references/manual).
	structs := [][4]string{
		{idA.String(), idD.String(), "references", "system"},
		{idA.String(), idC.String(), "references", "manual"},
	}
	for _, s := range structs {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_structural_links
			   (source_block_id, target_block_id, link_class, scope, origin)
			 VALUES ($1::uuid, $2::uuid, $3, 'shared', $4)`,
			s[0], s[1], s[2], s[3],
		); err != nil {
			t.Fatalf("seed struct link: %v", err)
		}
	}

	// Dangling dream edge: A -> <absent uuid>, forced past the FK on ONE
	// session via replica mode (the only way to create a genuine dangling row
	// the Build must skip + count).
	danglingTarget := uuid.New()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, err := conn.Exec(ctx, `SET session_replication_role = replica`); err != nil {
		conn.Release()
		t.Fatalf("set replica role: %v", err)
	}
	_, insErr := conn.Exec(ctx,
		`INSERT INTO context_dream_links
		   (source_block_id, target_block_id, relationship, confidence, raw_confidence, scope)
		 VALUES ($1::uuid, $2::uuid, 'topical', 0.6, 0.6, 'shared')`,
		idA.String(), danglingTarget.String())
	_, _ = conn.Exec(ctx, `SET session_replication_role = DEFAULT`)
	conn.Release()
	if insErr != nil {
		t.Fatalf("inject dangling edge: %v", insErr)
	}

	// --- Build ---
	snap, err := graphcache.Build(ctx, pool)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Universe includes the archived block.
	if snap.NumNodes() != 4 {
		t.Fatalf("NumNodes = %d, want 4 (archived block included)", snap.NumNodes())
	}
	if err := graphcache.CheckOrdering(snap); err != nil {
		t.Fatalf("CheckOrdering on Build snapshot: %v", err)
	}

	nodeOf := func(id uuid.UUID) uint32 {
		n, ok := snap.NodeID([16]byte(id))
		if !ok {
			t.Fatalf("NodeID(%v) not found", id)
		}
		return n
	}
	nA, nB, nC, nD := nodeOf(idA), nodeOf(idB), nodeOf(idC), nodeOf(idD)
	if _, ok := snap.NodeID([16]byte(danglingTarget)); ok {
		t.Error("NodeID found the absent dangling target")
	}

	// Archived bit.
	if !snap.IsArchived(nC) {
		t.Error("IsArchived(C) = false, want true")
	}
	if snap.IsArchived(nA) {
		t.Error("IsArchived(A) = true, want false")
	}

	// Dangling counter + edge counts.
	if snap.Stats.DreamDangling != 1 {
		t.Errorf("DreamDangling = %d, want 1", snap.Stats.DreamDangling)
	}
	if snap.Stats.DreamEdges != 3 {
		t.Errorf("DreamEdges = %d, want 3", snap.Stats.DreamEdges)
	}
	if snap.Stats.StructEdges != 2 {
		t.Errorf("StructEdges = %d, want 2", snap.Stats.StructEdges)
	}

	// Dream fwd adjacency of A: C (0.9) before B (0.3), dangling absent.
	de := snap.DreamNeighbors(nA, graphcache.Forward)
	if len(de.Targets) != 2 || de.Targets[0] != nC || de.Targets[1] != nB {
		t.Errorf("dream fwd A targets = %v, want [C=%d B=%d]", de.Targets, nC, nB)
	}

	// Degree: raw across all four directions = 5 (dream fwd 2 + dream rev 1 +
	// struct fwd 2 + struct rev 0).
	raw, capped := snap.Degree(nA, nil)
	if capped || raw != 5 {
		t.Errorf("raw degree(A) = %d capped=%v, want 5 false", raw, capped)
	}
	// Filtered to shared scope: B (private) drops. A's neighbours: B(fwd dream,
	// private), C(fwd struct + fwd dream + rev dream, shared, archived→excluded
	// by ExcludeArchived), D(fwd struct, shared). C is archived so excluded;
	// only D remains among the distinct... note Degree counts ROWS per direction.
	hints := snap.MakeDegreeHints([]string{"shared"}, nil, 0)
	filtered, _ := snap.Degree(nA, &hints)
	// Rows to shared, non-archived neighbours: struct fwd A->D (D shared) = 1;
	// dream fwd A->C excluded (archived), A->B excluded (private); dream rev
	// C->A excluded (archived); struct fwd A->C excluded (archived). => 1.
	if filtered != 1 {
		t.Errorf("filtered degree(A) = %d, want 1 (only D shared+active)", filtered)
	}

	// InducedEdges over {A,C,D}: dream A->C, C->A (both in set), A->B excluded;
	// struct A->D, A->C (both in set). => 2 dream, 2 struct.
	set := []uint32{nA, nC, nD}
	res := snap.InducedEdges(set)
	if len(res.Dream) != 2 {
		t.Errorf("induced dream = %d, want 2", len(res.Dream))
	}
	if len(res.Struct) != 2 {
		t.Errorf("induced struct = %d, want 2", len(res.Struct))
	}

	// Determinism: a second Build over the unchanged DB is byte-equal.
	snap2, err := graphcache.Build(ctx, pool)
	if err != nil {
		t.Fatalf("second Build: %v", err)
	}
	if snap.Fingerprint() != snap2.Fingerprint() {
		t.Error("two Builds over identical DB produced different Fingerprints")
	}
}
