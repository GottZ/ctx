//go:build integration

package dream_test

import (
	"context"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/testdb"
)

// IDs scoped to this test file to avoid collision with other integration tests
// in the same package.
const (
	smNewerID = "019d0000-0000-7000-9000-0000000046a1" // newer source — the replacement
	smOlderID = "019d0000-0000-7000-9000-0000000046a2" // outdated target — superseded
	smOther1  = "019d0000-0000-7000-9000-0000000046a3"
	smOther2  = "019d0000-0000-7000-9000-0000000046a4"
)

// TestSupersedesMap_KeyedByOutdatedTarget_Welle46Convention verifies that
// SupersedesMap returns map[outdated_block_id] = []newer_source_ids that
// supersede it. Under the Welle 46 Convention-Switch (2026-05-22), the
// natural-language reading "A supersedes B" → A=source=newer, B=target=
// outdated. The map is queried with a set of candidate IDs and returns
// entries for those that are TARGETS of supersedes-links.
func TestSupersedesMap_KeyedByOutdatedTarget_Welle46Convention(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	tEarly := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tLate := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	// Setup: newer block supersedes older block. Direct INSERT bypassing the
	// in-Go filter chain (we test the read-side, not the write-side).
	insertBlock(t, pool, smNewerID, "private", "decisions", "spec v2", tLate, tLate)
	insertBlock(t, pool, smOlderID, "private", "decisions", "spec v1", tEarly, tEarly)
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_dream_links
			(source_block_id, target_block_id, relationship, confidence, raw_confidence, scope, dream_version)
		VALUES ($1::uuid, $2::uuid, 'supersedes', 0.95, 0.95, 'private', 5)`,
		smNewerID, smOlderID,
	); err != nil {
		t.Fatalf("seed link: %v", err)
	}

	// Query with the OUTDATED block id — must return the newer replacement.
	m, err := dream.SupersedesMap(ctx, pool, []string{smOlderID})
	if err != nil {
		t.Fatalf("SupersedesMap (outdated key): %v", err)
	}
	supersederIDs, ok := m[smOlderID]
	if !ok {
		t.Fatalf("expected key=%s (outdated target), map=%v", smOlderID, m)
	}
	if len(supersederIDs) != 1 || supersederIDs[0] != smNewerID {
		t.Errorf("got supersederIDs=%v, want [%s] (newer source replaces older target)", supersederIDs, smNewerID)
	}

	// Querying with the NEWER block id — must NOT return it (the newer block
	// is not superseded by anything in this fixture).
	m, err = dream.SupersedesMap(ctx, pool, []string{smNewerID})
	if err != nil {
		t.Fatalf("SupersedesMap (newer key): %v", err)
	}
	if _, ok := m[smNewerID]; ok {
		t.Errorf("newer block must not appear as map key (it is not superseded), got %v", m)
	}
}

// TestSupersedesMap_MultipleSupersedersOneTarget verifies that when several
// newer blocks supersede the same outdated block, all are listed under that
// target's map entry.
func TestSupersedesMap_MultipleSupersedersOneTarget(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	tEarly := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tMid := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	tLate := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	insertBlock(t, pool, smOlderID, "private", "decisions", "spec v1", tEarly, tEarly)
	insertBlock(t, pool, smOther1, "private", "decisions", "spec v2", tMid, tMid)
	insertBlock(t, pool, smOther2, "private", "decisions", "spec v3", tLate, tLate)

	// Both newer blocks supersede the older one (real corpus can have this
	// shape when a v1 is replaced first by v2, then again by v3).
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_dream_links
			(source_block_id, target_block_id, relationship, confidence, raw_confidence, scope, dream_version)
		VALUES
			($1::uuid, $3::uuid, 'supersedes', 0.90, 0.90, 'private', 5),
			($2::uuid, $3::uuid, 'supersedes', 0.95, 0.95, 'private', 5)`,
		smOther1, smOther2, smOlderID,
	); err != nil {
		t.Fatalf("seed links: %v", err)
	}

	m, err := dream.SupersedesMap(ctx, pool, []string{smOlderID})
	if err != nil {
		t.Fatalf("SupersedesMap: %v", err)
	}
	if got := m[smOlderID]; len(got) != 2 {
		t.Fatalf("got %d superseders, want 2 (%v)", len(got), got)
	}
}

// TestSupersedesMap_RawConfidenceGate verifies the raw_confidence>=0.7 gate
// excludes weak supersedes-claims from the map.
func TestSupersedesMap_RawConfidenceGate(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	tEarly := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tLate := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	insertBlock(t, pool, smNewerID, "private", "decisions", "spec v2", tLate, tLate)
	insertBlock(t, pool, smOlderID, "private", "decisions", "spec v1", tEarly, tEarly)
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_dream_links
			(source_block_id, target_block_id, relationship, confidence, raw_confidence, scope, dream_version)
		VALUES ($1::uuid, $2::uuid, 'supersedes', 0.5, 0.5, 'private', 5)`,
		smNewerID, smOlderID,
	); err != nil {
		t.Fatalf("seed link: %v", err)
	}

	m, err := dream.SupersedesMap(ctx, pool, []string{smOlderID})
	if err != nil {
		t.Fatalf("SupersedesMap: %v", err)
	}
	if _, ok := m[smOlderID]; ok {
		t.Errorf("weak (raw<0.7) supersedes leaked into map: %v", m)
	}
}

// TestPromoteToCanonical_RejectsBlocksWithInboundSupersedes verifies the
// Welle 46 Convention-Switch (2026-05-22): under "A supersedes B" → A=source=
// newer, B=target=outdated, a block that appears as a TARGET of a supersedes-
// link must NOT be promoted to canonical (something newer has replaced it).
// The pre-Welle-46 check tested source_block_id; this test verifies the
// post-Welle-46 target_block_id semantics.
func TestPromoteToCanonical_RejectsBlocksWithInboundSupersedes(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	tEarly := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tLate := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	// older block is high-quality and would otherwise qualify, but has been
	// superseded → must NOT be promoted.
	insertBlock(t, pool, smOlderID, "private", "decisions", "spec v1", tEarly, tEarly)
	insertBlock(t, pool, smNewerID, "private", "decisions", "spec v2", tLate, tLate)
	if _, err := pool.Exec(ctx,
		`UPDATE context_blocks SET quality_score = 0.9, block_type = 'knowledge'
		WHERE id IN ($1::uuid, $2::uuid)`,
		smOlderID, smNewerID,
	); err != nil {
		t.Fatalf("set quality: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_dream_links
			(source_block_id, target_block_id, relationship, confidence, raw_confidence, scope, dream_version)
		VALUES ($1::uuid, $2::uuid, 'supersedes', 0.95, 0.95, 'private', 5)`,
		smNewerID, smOlderID,
	); err != nil {
		t.Fatalf("seed link: %v", err)
	}

	// Outdated target — must NOT be promoted.
	promoted, err := dream.PromoteToCanonical(ctx, pool, smOlderID)
	if err != nil {
		t.Fatalf("PromoteToCanonical(outdated): %v", err)
	}
	if promoted {
		t.Errorf("outdated target was promoted to canonical — must be rejected (inbound supersedes)")
	}

	// Newer source — should be eligible (no inbound supersedes).
	promoted, err = dream.PromoteToCanonical(ctx, pool, smNewerID)
	if err != nil {
		t.Fatalf("PromoteToCanonical(newer): %v", err)
	}
	if !promoted {
		t.Errorf("newer source was not promoted — should be canonical (no inbound supersedes, quality 0.9)")
	}

	// Verify block_type was actually updated.
	var bt string
	if err := pool.QueryRow(ctx,
		`SELECT block_type FROM context_blocks WHERE id = $1::uuid`, smNewerID,
	).Scan(&bt); err != nil {
		t.Fatalf("read block_type: %v", err)
	}
	if bt != "canonical" {
		t.Errorf("post-promote block_type=%q, want canonical", bt)
	}
}
