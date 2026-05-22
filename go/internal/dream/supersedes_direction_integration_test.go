//go:build integration

package dream_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/testdb"
)

// IDs scoped to this test file so we don't collide with writelinks_integration_test.go
// fixtures running in the same package within the same container build.
const (
	sdSrcCorrectID = "019d0000-0000-7000-9000-0000000043a1" // newer source, older target — keep
	sdTgtCorrectID = "019d0000-0000-7000-9000-0000000043a2"
	sdSrcInvertID  = "019d0000-0000-7000-9000-0000000043b1" // older source, newer target — must swap
	sdTgtInvertID  = "019d0000-0000-7000-9000-0000000043b2"
)

// TestMigration043_SupersedesDirectionSwap_RealDBBehaviour verifies that the
// SQL migration 043_supersedes_direction_swap.sql actually swaps invertierte
// supersedes-Links (src.created_at < tgt.created_at) while leaving correctly-
// oriented links untouched. Operates against the live container's migration
// chain — SetupTestDB runs all migrations including 043, so we manually
// insert a fresh inverted link AFTER setup and re-run the swap step inline.
func TestMigration043_SupersedesDirectionSwap_RealDBBehaviour(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	tOlder := time.Date(2026, 3, 25, 0, 0, 0, 0, time.UTC)
	tNewer := time.Date(2026, 3, 26, 0, 0, 0, 0, time.UTC)

	// Correct-direction pair: source newer than target. Migration must leave alone.
	insertBlock(t, pool, sdSrcCorrectID, "private", "decisions", "Spec Alpha v2", tNewer, tNewer)
	insertBlock(t, pool, sdTgtCorrectID, "private", "decisions", "Spec Alpha v1", tOlder, tOlder)

	// Inverted pair: source older than target. Migration must swap.
	insertBlock(t, pool, sdSrcInvertID, "private", "decisions", "Spec Beta v1", tOlder, tOlder)
	insertBlock(t, pool, sdTgtInvertID, "private", "decisions", "Spec Beta v2", tNewer, tNewer)

	// Inject both supersedes links directly (bypassing the in-Go filter chain,
	// which would now also reject the inverted one — we're testing the data-
	// migration behaviour against legacy persisted records).
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_dream_links
			(source_block_id, target_block_id, relationship, confidence, raw_confidence, scope, dream_version)
		VALUES
			($1::uuid, $2::uuid, 'supersedes', 0.95, 0.95, 'private', 5),
			($3::uuid, $4::uuid, 'supersedes', 0.90, 0.90, 'private', 5)`,
		sdSrcCorrectID, sdTgtCorrectID, sdSrcInvertID, sdTgtInvertID,
	); err != nil {
		t.Fatalf("seed supersedes links: %v", err)
	}

	// Inline application of the migration's swap algorithm. SetupTestDB ran
	// migration 043 once at startup against an empty links table; we seeded
	// AFTER that, so we replay the swap explicitly here.
	if err := applyMigration043Swap(ctx, pool); err != nil {
		t.Fatalf("apply swap: %v", err)
	}

	// Verify correct-direction pair is untouched.
	var correctCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM context_dream_links
		WHERE source_block_id = $1::uuid AND target_block_id = $2::uuid
		  AND relationship = 'supersedes'`,
		sdSrcCorrectID, sdTgtCorrectID,
	).Scan(&correctCount); err != nil {
		t.Fatalf("query correct-direction: %v", err)
	}
	if correctCount != 1 {
		t.Errorf("correct-direction link missing or duplicated: got %d, want 1", correctCount)
	}

	// Verify inverted pair was swapped: original (src, tgt) gone, (tgt, src) present.
	var invertedRemnant int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM context_dream_links
		WHERE source_block_id = $1::uuid AND target_block_id = $2::uuid
		  AND relationship = 'supersedes'`,
		sdSrcInvertID, sdTgtInvertID,
	).Scan(&invertedRemnant); err != nil {
		t.Fatalf("query inverted remnant: %v", err)
	}
	if invertedRemnant != 0 {
		t.Errorf("inverted link still present (not swapped): got %d, want 0", invertedRemnant)
	}

	var swappedPresent int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM context_dream_links
		WHERE source_block_id = $1::uuid AND target_block_id = $2::uuid
		  AND relationship = 'supersedes'`,
		sdTgtInvertID, sdSrcInvertID,
	).Scan(&swappedPresent); err != nil {
		t.Fatalf("query swapped: %v", err)
	}
	if swappedPresent != 1 {
		t.Errorf("swapped link missing: got %d, want 1 (source/target should be swapped)", swappedPresent)
	}

	// Sanity: post-swap there are no invertierte supersedes-Links left across
	// the full table (defense against accidental cross-test pollution).
	var invertedRemaining int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM context_dream_links dl
		JOIN context_blocks src ON src.id = dl.source_block_id
		JOIN context_blocks tgt ON tgt.id = dl.target_block_id
		WHERE dl.relationship = 'supersedes'
		  AND src.created_at < tgt.created_at`,
	).Scan(&invertedRemaining); err != nil {
		t.Fatalf("query invariant: %v", err)
	}
	if invertedRemaining != 0 {
		t.Errorf("post-swap invariant violated: %d invertierte supersedes-Links remain", invertedRemaining)
	}

	// Idempotency: re-applying the swap on the already-corrected state must
	// affect 0 rows.
	if err := applyMigration043Swap(ctx, pool); err != nil {
		t.Fatalf("re-apply swap: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM context_dream_links
		WHERE source_block_id = $1::uuid AND target_block_id = $2::uuid
		  AND relationship = 'supersedes'`,
		sdTgtInvertID, sdSrcInvertID,
	).Scan(&swappedPresent); err != nil {
		t.Fatalf("post-idempotency query: %v", err)
	}
	if swappedPresent != 1 {
		t.Errorf("idempotency broke: got %d, want 1", swappedPresent)
	}
}

// applyMigration043Swap runs the swap+conflict-resolution+insert sequence
// from migration 043 in a fresh TX. The migration itself uses a TEMP TABLE
// ON COMMIT DROP; here we follow the same pattern.
func applyMigration043Swap(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err := tx.Exec(ctx,
		`CREATE TEMP TABLE supersedes_to_swap ON COMMIT DROP AS
		SELECT
		  dl.source_block_id, dl.target_block_id, dl.relationship,
		  dl.confidence, dl.raw_confidence, dl.scope, dl.metadata, dl.dream_version
		FROM context_dream_links dl
		JOIN context_blocks src ON src.id = dl.source_block_id
		JOIN context_blocks tgt ON tgt.id = dl.target_block_id
		WHERE dl.relationship = 'supersedes'
		  AND src.created_at < tgt.created_at`,
	); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx,
		`DELETE FROM context_dream_links dl
		WHERE EXISTS (
		  SELECT 1 FROM supersedes_to_swap s
		  WHERE s.source_block_id = dl.target_block_id
		    AND s.target_block_id = dl.source_block_id
		)`,
	); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx,
		`DELETE FROM context_dream_links dl
		WHERE EXISTS (
		  SELECT 1 FROM supersedes_to_swap s
		  WHERE s.source_block_id = dl.source_block_id
		    AND s.target_block_id = dl.target_block_id
		)`,
	); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO context_dream_links (
		  source_block_id, target_block_id, relationship,
		  confidence, raw_confidence, scope, metadata, dream_version, created_at
		)
		SELECT
		  target_block_id, source_block_id, relationship,
		  confidence, raw_confidence, scope,
		  COALESCE(metadata, '{}'::jsonb) || '{"direction_swap_w46": true, "audit_source": "sub-agent-2-2026-05-22"}'::jsonb,
		  dream_version, now()
		FROM supersedes_to_swap
		ON CONFLICT (source_block_id, target_block_id) DO NOTHING`,
	); err != nil {
		return err
	}

	return tx.Commit(ctx)
}
