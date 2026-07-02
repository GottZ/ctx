//go:build integration

// WF T8 policy gates against real PG (design/01 §7-T8). RED states were
// proven against the pre-T8 tree on 2026-07-02 with a scratch probe (deleted
// in this wave):
//   - a block of a registered type with dream.linkable=false (≠ system-meta)
//     WAS written as a WriteLinks link target ("linkable=false target got 1
//     link(s) written"), because only is_meta sieved;
//   - a causal link WAS written although the source type's link_classes
//     restricted to ["topical"] (no link-class gate existed).
// These tests pin the GREEN contract on the same fixtures.
package dream_test

import (
	"context"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// registerT8Type inserts a registry row (M072 table) — the same path a T10
// type-create would take, minus the API layer.
func registerT8Type(t *testing.T, pool *pgxpool.Pool, name, config string) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO context_block_types (name, scope, config) VALUES ($1, '_global', $2::jsonb)`,
		name, config)
	if err != nil {
		t.Fatalf("register type %s: %v", name, err)
	}
}

func setBlockType(t *testing.T, pool *pgxpool.Pool, id, typeName string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE context_blocks SET type_name = $2 WHERE id = $1::uuid`, id, typeName); err != nil {
		t.Fatalf("set type %s on %s: %v", typeName, id, err)
	}
}

// bootedSet loads the registry from the test DB (seeds + rows registered by
// the test) — the production resolution path, never a test-local list.
func bootedSet(t *testing.T, pool *pgxpool.Pool) *blocktype.Set {
	t.Helper()
	reg := blocktype.NewRegistry()
	reg.Boot(context.Background(), pool)
	return reg.Snapshot()
}

// TestWriteLinks_LinkableFalseTarget_NeverWritten_BehaviourMatchesContract:
// dream.linkable acts on the TARGET side (§3.3 R1) — a linkable=false type
// never appears as a link target of a WriteLinks batch. RED before T8 (see
// file header), GREEN now.
func TestWriteLinks_LinkableFalseTarget_NeverWritten_BehaviourMatchesContract(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	tEarly := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tLate := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	registerT8Type(t, pool, "wf-nolink", `{"v":1,"dream":{"linkable":false}}`)
	insertBlock(t, pool, icSourceID, "private", "decisions", "src title", tEarly, tEarly)
	insertBlock(t, pool, icTargetID, "private", "decisions", "tgt title", tLate, tLate)
	setBlockType(t, pool, icTargetID, "wf-nolink")

	written, err := dream.WriteLinks(ctx, pool, bootedSet(t, pool), icSourceID, "private", 1.0,
		[]dream.Link{{TargetID: icTargetID, Relationship: "topical", Confidence: 0.9}})
	if err != nil {
		t.Fatalf("WriteLinks: %v", err)
	}
	if written != 0 || countLinks(t, pool, icSourceID) != 0 {
		t.Errorf("linkable=false target got %d link(s) written, want 0", written)
	}
}

// TestWriteLinks_LinkClasses_RealRegistryRow_BehaviourMatchesContract: the
// source type's link_classes travel from a REAL registry row through
// Boot/Reload into the gate — causal rejected, topical written. RED before
// T8 (see file header), GREEN now.
func TestWriteLinks_LinkClasses_RealRegistryRow_BehaviourMatchesContract(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	tEarly := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tLate := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	registerT8Type(t, pool, "wf-topical-only", `{"v":1,"dream":{"linkable":true,"link_classes":["topical"]}}`)
	insertBlock(t, pool, icSourceID, "private", "decisions", "src title", tEarly, tEarly)
	insertBlock(t, pool, icTargetID, "private", "projects", "tgt title", tLate, tLate)
	setBlockType(t, pool, icSourceID, "wf-topical-only")
	set := bootedSet(t, pool)

	// causal: passes V9 (src created before tgt) — only the class gate rejects.
	written, err := dream.WriteLinks(ctx, pool, set, icSourceID, "private", 1.0,
		[]dream.Link{{TargetID: icTargetID, Relationship: "causal", Confidence: 0.9}})
	if err != nil {
		t.Fatalf("WriteLinks causal: %v", err)
	}
	if written != 0 || countLinks(t, pool, icSourceID) != 0 {
		t.Errorf("link_classes=[topical] source wrote a causal link (written=%d, want 0)", written)
	}

	// topical: the allowed class still writes (subset filter, not shut-off).
	written, err = dream.WriteLinks(ctx, pool, set, icSourceID, "private", 1.0,
		[]dream.Link{{TargetID: icTargetID, Relationship: "topical", Confidence: 0.9}})
	if err != nil {
		t.Fatalf("WriteLinks topical: %v", err)
	}
	if written != 1 || countLinks(t, pool, icSourceID) != 1 {
		t.Errorf("allowed class must write (written=%d, want 1)", written)
	}
}
