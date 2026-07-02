//go:build integration

// Integration tests for the G40 credentials pattern re-audit store layer
// against a real PG18 testcontainer (migration 055 columns).
//
// The store functions do NOT scan — the scheduler/handler call sensitivity.Scan
// and hand a verdict here. These tests pin the SQL invariants that carry the
// opsec guarantees:
//   - PickClassifyCandidates excludes already-credentials AND manual rows,
//     stays home-scope only, and keyset-paginates by id
//   - ApplyPatternVerdict is UPGRADE-ONLY (never touches an already-credentials
//     row), manual-untouchable BY THE PREDICATE (red without the source guard),
//     scope-bound, and records metadata.sensitivity_detector
//   - UpsertBlock with Detector=true stamps source='pattern' and re-stamps on a
//     STRICT elevation only (an already-credentials manual row stays manual)
//
// Run with:
//
//	go test -tags=integration ./internal/store/ -run TestCredentialsClassify -count=1 -v
package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// seedClassifyBlock inserts a block with explicit sensitivity + source. Content
// is irrelevant to the store layer (scanning happens upstream).
func seedClassifyBlock(t *testing.T, pool *pgxpool.Pool, scope, title, sens, source string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var id string
	err := pool.QueryRow(ctx,
		`INSERT INTO context_blocks (category, title, content, scope, sensitivity, sensitivity_source)
		 VALUES ('learnings', $1, 'content of ' || $1, $2, $3, $4) RETURNING id`,
		title, scope, sens, source).Scan(&id)
	if err != nil {
		t.Fatalf("seed block %s: %v", title, err)
	}
	return id
}

func detectorKind(t *testing.T, pool *pgxpool.Pool, id string) (kind string, present bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var k *string
	if err := pool.QueryRow(ctx,
		`SELECT metadata->'sensitivity_detector'->>'kind' FROM context_blocks WHERE id = $1`,
		id).Scan(&k); err != nil {
		t.Fatalf("read detector metadata: %v", err)
	}
	if k == nil {
		return "", false
	}
	return *k, true
}

func classifyPickIDs(t *testing.T, pool *pgxpool.Pool, scope, afterID string, limit int) []string {
	t.Helper()
	blocks, err := store.PickClassifyCandidates(context.Background(), pool, scope, afterID, limit)
	if err != nil {
		t.Fatalf("pick classify candidates: %v", err)
	}
	ids := make([]string, len(blocks))
	for i, b := range blocks {
		ids[i] = b.ID
	}
	return ids
}

func contains(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

func TestCredentialsClassify_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	internalID := seedClassifyBlock(t, pool, "private", "g40-internal", "internal", "llm-audit")
	personalID := seedClassifyBlock(t, pool, "private", "g40-personal", "personal", "llm-audit")
	credID := seedClassifyBlock(t, pool, "private", "g40-already-cred", "credentials", "default")
	manualLowID := seedClassifyBlock(t, pool, "private", "g40-manual-public", "public", "manual")
	foreignID := seedClassifyBlock(t, pool, "work", "g40-foreign", "internal", "llm-audit")

	t.Run("pick excludes credentials, manual, and foreign scope", func(t *testing.T) {
		picked := classifyPickIDs(t, pool, "private", "", 100)
		if !contains(picked, internalID) || !contains(picked, personalID) {
			t.Error("a lower-sensitivity non-manual block is missing from the candidate set")
		}
		if contains(picked, credID) {
			t.Error("already-credentials block in candidate set — the detector only RAISES")
		}
		if contains(picked, manualLowID) {
			t.Error("manual block in candidate set — manual is untantastbar (design 03 §2.3d)")
		}
		if contains(picked, foreignID) {
			t.Error("foreign-scope block in candidate set — classification follows ownership")
		}
	})

	t.Run("keyset pagination is disjoint and drains", func(t *testing.T) {
		first := classifyPickIDs(t, pool, "private", "", 1)
		if len(first) != 1 {
			t.Fatalf("limit 1 returned %d rows", len(first))
		}
		rest := classifyPickIDs(t, pool, "private", first[0], 100)
		if contains(rest, first[0]) {
			t.Error("keyset re-returned the cursor row")
		}
		// first ∪ rest must cover both candidates exactly once.
		all := append([]string{}, first...)
		all = append(all, rest...)
		if !contains(all, internalID) || !contains(all, personalID) {
			t.Error("keyset pages do not cover the full candidate set")
		}
	})

	t.Run("verdict raises to credentials+pattern and records the reason", func(t *testing.T) {
		applied, err := store.ApplyPatternVerdict(ctx, pool, internalID, "private", "aws-key", "AWS access key id pattern")
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		if !applied {
			t.Fatal("verdict not applied to an internal/llm-audit candidate")
		}
		sens, source, auditedAt := blockSensState(t, pool, internalID)
		if sens != "credentials" || source != "pattern" || auditedAt == nil {
			t.Fatalf("post-verdict state wrong: sens=%s source=%s audited=%v", sens, source, auditedAt)
		}
		if kind, ok := detectorKind(t, pool, internalID); !ok || kind != "aws-key" {
			t.Fatalf("metadata.sensitivity_detector.kind = %q present=%v, want aws-key", kind, ok)
		}
		// Out of the candidate set now, and a second verdict is discarded
		// (already credentials — the upgrade-only predicate).
		if contains(classifyPickIDs(t, pool, "private", "", 100), internalID) {
			t.Error("raised block still a candidate")
		}
		applied, err = store.ApplyPatternVerdict(ctx, pool, internalID, "private", "aws-key", "x")
		if err != nil {
			t.Fatalf("second apply: %v", err)
		}
		if applied {
			t.Error("second verdict applied to an already-credentials row")
		}
	})

	t.Run("manual is untouchable by the source predicate", func(t *testing.T) {
		applied, err := store.ApplyPatternVerdict(ctx, pool, manualLowID, "private", "aws-key", "x")
		if err != nil {
			t.Fatalf("apply against manual: %v", err)
		}
		if applied {
			t.Fatal("verdict applied to a manual row — the source guard is gone")
		}
		sens, source, _ := blockSensState(t, pool, manualLowID)
		if sens != "public" || source != "manual" {
			t.Fatalf("manual row changed: sens=%s source=%s", sens, source)
		}
		// Negative probe: the same UPDATE WITHOUT the source guard hits — the
		// predicate carries the guarantee, not luck.
		probeID := seedClassifyBlock(t, pool, "private", "g40-manual-probe", "public", "manual")
		tag, err := pool.Exec(ctx,
			`UPDATE context_blocks SET sensitivity = 'credentials', sensitivity_source = 'pattern' WHERE id = $1`, probeID)
		if err != nil {
			t.Fatalf("unguarded probe: %v", err)
		}
		if tag.RowsAffected() != 1 {
			t.Fatal("unguarded probe did not hit — the negative probe proves nothing")
		}
	})

	t.Run("verdict is scope-bound", func(t *testing.T) {
		// personalID lives in private; applying it under home_scope 'work' must
		// match zero rows (no cross-tenant reclassification, design 03 §2.3d).
		applied, err := store.ApplyPatternVerdict(ctx, pool, personalID, "work", "aws-key", "x")
		if err != nil {
			t.Fatalf("cross-scope apply: %v", err)
		}
		if applied {
			t.Fatal("verdict crossed scope boundaries")
		}
		sens, source, _ := blockSensState(t, pool, personalID)
		if sens != "personal" || source != "llm-audit" {
			t.Fatalf("cross-scope apply mutated the block: sens=%s source=%s", sens, source)
		}
	})
}

// TestUpsertBlockDetector_Integration pins the write-path source='pattern'
// behaviour: a Detector hit stamps source='pattern' on insert, and on conflict
// re-stamps only on a STRICT elevation so an already-credentials block — manual
// included — is left intact.
func TestUpsertBlockDetector_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	t.Run("detector insert stamps credentials+pattern", func(t *testing.T) {
		b, err := store.UpsertBlock(ctx, pool, "learnings", "g40-write-new", "body", nil, nil, "private", false,
			store.SensitivityWrite{Value: backends.SensCredentials, Detector: true}, "")
		if err != nil {
			t.Fatalf("upsert: %v", err)
		}
		if b.Sensitivity != "credentials" || b.SensitivitySource != "pattern" {
			t.Fatalf("new block: sens=%s source=%s, want credentials/pattern", b.Sensitivity, b.SensitivitySource)
		}
	})

	t.Run("detector does not override an existing manual credentials block", func(t *testing.T) {
		// Seed a manual credentials block, then re-save the same key with a
		// detector hit. credentials > credentials is false (strict >), so the
		// source stays 'manual'.
		manualID := seedClassifyBlock(t, pool, "private", "g40-write-manual", "credentials", "manual")
		_, err := store.UpsertBlock(ctx, pool, "learnings", "g40-write-manual", "new body with a key", nil, nil, "private", false,
			store.SensitivityWrite{Value: backends.SensCredentials, Detector: true}, "")
		if err != nil {
			t.Fatalf("upsert conflict: %v", err)
		}
		sens, source, _ := blockSensState(t, pool, manualID)
		if sens != "credentials" || source != "manual" {
			t.Fatalf("manual credentials block flipped: sens=%s source=%s, want credentials/manual", sens, source)
		}
	})

	t.Run("detector raises a lower manual block on strict elevation", func(t *testing.T) {
		// internal (manual) + detector hit → credentials. internal < credentials,
		// so the strict-> elevation fires and re-stamps source='pattern' (the
		// safety net overrides a too-low classification; that is a RAISE).
		lowID := seedClassifyBlock(t, pool, "private", "g40-write-low-manual", "internal", "manual")
		_, err := store.UpsertBlock(ctx, pool, "learnings", "g40-write-low-manual", "now carries a key", nil, nil, "private", false,
			store.SensitivityWrite{Value: backends.SensCredentials, Detector: true}, "")
		if err != nil {
			t.Fatalf("upsert conflict: %v", err)
		}
		sens, source, _ := blockSensState(t, pool, lowID)
		if sens != "credentials" || source != "pattern" {
			t.Fatalf("low manual block not raised: sens=%s source=%s, want credentials/pattern", sens, source)
		}
	})
}
