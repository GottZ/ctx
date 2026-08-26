//go:build integration

// Wave V-W3 (Wissens-Ebenen, design/05-eval-validierung.md §2.2 row S3 and §7
// row V-W3, masterplan §6 R4): UpsertBlock must strip the reserved metadata
// key `guard_checked_at` on BOTH upsert paths.
//
// The defect: guardPendingWhere (guard.go:65-70) takes a block out of the
// guard batch as soon as `metadata->>'guard_checked_at'` is non-NULL. Only
// UpdateBlock strips that key (store/blocks.go:715-732); UpsertBlock wrote
// the caller's metadata through unfiltered — INSERT column `metadata` ($5)
// and the conflict clause `metadata = EXCLUDED.metadata`. Any client, and
// every future derived writer (all of them go through UpsertBlock), could
// therefore silently remove its own block from the duplicate check. Fail-open.
//
// In-package test on purpose (same reasoning as guard_explain_integration_test.go):
// the pending assertion is built from the ACTUAL guardPendingWhere fragment
// the production queries embed, so the probe cannot drift away from the
// predicate it is supposed to protect.
//
// The fixture satisfies all four remaining conjuncts of guardPendingWhere —
// embedding set, lifecycle_state='knowledge' (DDL default, 113_baseline.sql:81),
// category='learnings' != 'index', type_name='knowledge' from the live
// GuardCheckTypes allowlist — so the ONLY variable between control and probe
// is the metadata key. Without that, the probe would not be isolating.
//
// RED state before the fix (verbatim, `go test -tags=integration -p 1
// ./internal/guard/ -run TestVW3UpsertStripsGuardCheckedAt -count=1 -v`):
//
//	upsert_metadata_strip_integration_test.go:186: INSERT path: block with
//	  metadata.guard_checked_at is NOT in the guard pending set — a client
//	  silently removed it from the duplicate check (fail-open, S3)
//	upsert_metadata_strip_integration_test.go:191: INSERT path: metadata in
//	  the DB still carries guard_checked_at: {"source": "vw3", ...}
//	upsert_metadata_strip_integration_test.go:236: UPDATE path: block left the
//	  guard pending set after a conflicting upsert carrying
//	  metadata.guard_checked_at (fail-open, S3)
//
// Run:
//
//	go test -tags=integration -p 1 ./internal/guard/ -run TestVW3UpsertStripsGuardCheckedAt -count=1 -v
package guard

import (
	"context"
	"slices"
	"testing"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

const (
	vw3Scope    = "private"
	vw3Category = "learnings"
	vw3Type     = "knowledge"
	vw3Stamp    = "2026-08-26T00:00:00Z"
)

func TestVW3UpsertStripsGuardCheckedAt(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// The allowlist comes from the registry (DB rows merged over the
	// builtins), not from a test literal — the same source RunGuardBatch
	// consumes (guard.go:108).
	reg := blocktype.NewRegistry()
	if err := reg.Reload(ctx, pool); err != nil {
		t.Fatalf("registry reload: %v", err)
	}
	checkTypes := reg.Snapshot().GuardCheckTypes()
	if !slices.Contains(checkTypes, vw3Type) {
		t.Fatalf("precondition: type %q is not in GuardCheckTypes %v — fixture would not be isolating", vw3Type, checkTypes)
	}

	vec := func(fill float32) []float32 {
		v := make([]float32, 1024)
		for i := range v {
			v[i] = fill
		}
		return v
	}

	// upsert goes through the PRODUCTION write path under test.
	upsert := func(t *testing.T, title, content string, meta map[string]any) *store.Block {
		t.Helper()
		b, err := store.UpsertBlock(ctx, pool, vw3Category, title, content, nil, meta,
			vw3Scope, false, store.SensitivityWrite{}, vw3Type)
		if err != nil {
			t.Fatalf("upsert %q: %v", title, err)
		}
		return b
	}

	embed := func(t *testing.T, id string) {
		t.Helper()
		if err := store.StoreEmbedding(ctx, pool, id, "vw3-model", vec(0.3)); err != nil {
			t.Fatalf("StoreEmbedding %s: %v", id, err)
		}
	}

	// pending answers the ONE question the whole wave is about, using the
	// real predicate fragment.
	pending := func(t *testing.T, id string) bool {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_blocks WHERE id = $2::uuid AND `+guardPendingWhere("$1"),
			checkTypes, id,
		).Scan(&n); err != nil {
			t.Fatalf("pending probe %s: %v", id, err)
		}
		return n == 1
	}

	// dbMeta returns the persisted metadata as canonical jsonb text plus the
	// key-existence flag — the SELECT proof the briefing asks for.
	dbMeta := func(t *testing.T, id string) (string, bool) {
		t.Helper()
		var raw string
		var hasKey bool
		if err := pool.QueryRow(ctx,
			`SELECT COALESCE(metadata, '{}'::jsonb)::text,
			        jsonb_exists(COALESCE(metadata, '{}'::jsonb), 'guard_checked_at')
			 FROM context_blocks WHERE id = $1::uuid`, id,
		).Scan(&raw, &hasKey); err != nil {
			t.Fatalf("read metadata %s: %v", id, err)
		}
		return raw, hasKey
	}

	// Control probe (green BEFORE and AFTER the fix): the fixture shape
	// itself lands in the pending set. This is what makes the two red probes
	// below attributable to the metadata key and to nothing else.
	t.Run("control_without_key_is_pending", func(t *testing.T) {
		b := upsert(t, "vw3-control", "control content", map[string]any{"source": "vw3"})
		embed(t, b.ID)
		if !pending(t, b.ID) {
			t.Fatalf("control: fixture block is NOT in the guard pending set — the probe is not isolating (checkTypes=%v)", checkTypes)
		}
		if _, hasKey := dbMeta(t, b.ID); hasKey {
			t.Errorf("control: metadata unexpectedly carries guard_checked_at")
		}
	})

	// RED #1 — INSERT path. The block does not exist yet, so the upsert takes
	// the INSERT branch: a strip that only sat on the ON CONFLICT clause would
	// leave exactly this probe red.
	t.Run("insert_path_key_is_stripped", func(t *testing.T) {
		b := upsert(t, "vw3-insert", "insert content", map[string]any{
			"source":           "vw3",
			"guard_checked_at": vw3Stamp,
			"nested":           map[string]any{"a": []any{1, 2}},
		})
		embed(t, b.ID)

		if _, ok := b.Metadata["guard_checked_at"]; ok {
			t.Errorf("INSERT path: RETURNING metadata still carries guard_checked_at = %v", b.Metadata["guard_checked_at"])
		}
		if !pending(t, b.ID) {
			t.Errorf("INSERT path: block with metadata.guard_checked_at is NOT in the guard pending set — a client silently removed it from the duplicate check (fail-open, S3)")
		}
		raw, hasKey := dbMeta(t, b.ID)
		if hasKey {
			t.Errorf("INSERT path: metadata in the DB still carries guard_checked_at: %s", raw)
		}
		// The strip must remove exactly one key, not sanitize the document.
		var source string
		var nested string
		if err := pool.QueryRow(ctx,
			`SELECT COALESCE(metadata->>'source', ''), COALESCE(metadata->'nested'->>'a', '')
			 FROM context_blocks WHERE id = $1::uuid`, b.ID,
		).Scan(&source, &nested); err != nil {
			t.Fatalf("read sibling keys: %v", err)
		}
		if source != "vw3" || nested != "[1, 2]" {
			t.Errorf("INSERT path: sibling keys damaged by the strip: source=%q nested.a=%q (want \"vw3\" / \"[1, 2]\")", source, nested)
		}
	})

	// RED #2 — UPDATE path (ON CONFLICT DO UPDATE). Content stays IDENTICAL on
	// the second write on purpose: a content change would clear the embedding
	// (blocks.go W04-8 ClearEmbeddingTx) and the block would drop out of the
	// pending set for the wrong reason, contaminating the probe.
	t.Run("update_path_key_is_stripped", func(t *testing.T) {
		const content = "stable update content"
		b := upsert(t, "vw3-update", content, map[string]any{"source": "vw3"})
		embed(t, b.ID)
		if !pending(t, b.ID) {
			t.Fatalf("UPDATE path precondition: block is not pending before the conflicting upsert")
		}

		b2 := upsert(t, "vw3-update", content, map[string]any{
			"source":           "vw3",
			"guard_checked_at": vw3Stamp,
		})
		if b2.ID != b.ID {
			t.Fatalf("UPDATE path: second upsert created a new row (%s != %s) — conflict branch not taken", b2.ID, b.ID)
		}
		if _, ok := b2.Metadata["guard_checked_at"]; ok {
			t.Errorf("UPDATE path: RETURNING metadata still carries guard_checked_at = %v", b2.Metadata["guard_checked_at"])
		}
		if !pending(t, b.ID) {
			t.Errorf("UPDATE path: block left the guard pending set after a conflicting upsert carrying metadata.guard_checked_at (fail-open, S3)")
		}
		raw, hasKey := dbMeta(t, b.ID)
		if hasKey {
			t.Errorf("UPDATE path: metadata in the DB still carries guard_checked_at: %s", raw)
		}
	})

	// Non-regression: an upsert WITHOUT the reserved key leaves the persisted
	// metadata byte-identical, on both paths (insert then idempotent conflict).
	// The strip expression must not reorder, re-type or drop anything else.
	t.Run("without_key_metadata_is_byte_identical", func(t *testing.T) {
		const content = "byte-identical content"
		meta := map[string]any{
			"source":            "claude-code-2026-08-26",
			"sensitivity_audit": map[string]any{"level": "internal", "by": "vw3"},
			"list":              []any{"a", "b"},
			"n":                 float64(7),
		}
		b := upsert(t, "vw3-identical", content, meta)
		embed(t, b.ID)
		before, hasKey := dbMeta(t, b.ID)
		if hasKey {
			t.Fatalf("precondition: fixture metadata carries guard_checked_at")
		}

		b2 := upsert(t, "vw3-identical", content, meta)
		if b2.ID != b.ID {
			t.Fatalf("second upsert created a new row (%s != %s)", b2.ID, b.ID)
		}
		after, hasKeyAfter := dbMeta(t, b.ID)
		if hasKeyAfter {
			t.Errorf("idempotent upsert introduced guard_checked_at: %s", after)
		}
		if before != after {
			t.Errorf("metadata not byte-identical across an idempotent upsert:\nbefore = %s\nafter  = %s", before, after)
		}
		if !pending(t, b.ID) {
			t.Errorf("idempotent upsert took the block out of the guard pending set")
		}
	})
}
