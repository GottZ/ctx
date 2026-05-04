//go:build integration

package dream_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/testdb"
)

const (
	icSourceID = "019d0000-0000-7000-9000-000000000001"
	icTargetID = "019d0000-0000-7000-9000-000000000002"
	icOtherID  = "019d0000-0000-7000-9000-000000000003"
)

// insertBlock writes a minimal context_blocks row with the supplied id and
// metadata. created_at is set explicitly so V8/V9 temporal checks fire
// deterministically.
func insertBlock(t *testing.T, pool *pgxpool.Pool, id, scope, category, title string, createdAt, updatedAt time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := pool.Exec(ctx,
		`INSERT INTO context_blocks (id, category, title, content, scope, created_at, updated_at)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7)`,
		id, category, title, "test content for "+title, scope, createdAt, updatedAt,
	)
	if err != nil {
		t.Fatalf("insert block %s: %v", id, err)
	}
}

// countLinks returns the number of context_dream_links rows for the given
// source.
func countLinks(t *testing.T, pool *pgxpool.Pool, sourceID string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var n int
	err := pool.QueryRow(ctx,
		`SELECT count(*) FROM context_dream_links WHERE source_block_id = $1::uuid`,
		sourceID,
	).Scan(&n)
	if err != nil {
		t.Fatalf("count links: %v", err)
	}
	return n
}

// TestWriteLinks_TxAbort_BehaviourMatchesContract verifies that when a per-link
// INSERT fails inside the transaction, the deferred Rollback (or the failing
// Commit) actually leaves the DB clean — i.e. no partial writes survive. The
// pgxmock equivalent (TestWriteLinks_LoopBreaks_ZeroWritten) only checks the
// return value, not the persisted state, because pgxmock cannot model an
// aborted-transaction Commit (W10-gap).
//
// Trigger: a relationship value that passes the per-link Go-side filter chain
// but fails the schema CHECK constraint
// (relationship IN ('topical','factual','causal','supersedes')).
func TestWriteLinks_TxAbort_BehaviourMatchesContract(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	tEarly := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tLate := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	insertBlock(t, pool, icSourceID, "private", "decisions", "src title", tEarly, tEarly)
	insertBlock(t, pool, icTargetID, "private", "decisions", "tgt title", tLate, tLate)

	// 'invalid_relationship' clears the V5/V6/V8/V9/V10 chain (none of those
	// gates apply to unknown relationships) and reaches the INSERT, where the
	// CHECK constraint rejects it. Real PG marks the TX aborted, so Commit
	// must fail or the deferred Rollback must reclaim cleanly.
	written, err := dream.WriteLinks(ctx, pool, icSourceID, "private", 1.0,
		[]dream.Link{{TargetID: icTargetID, Relationship: "invalid_relationship", Confidence: 0.9}})

	// Either: Commit returns the aborted-TX error wrapped by WriteLinks, OR
	// the deferred Rollback fires first and Commit never runs (also fine).
	// The contract that matters is: written=0 AND no row persisted.
	if written != 0 {
		t.Errorf("got written=%d, want 0", written)
	}
	if err != nil {
		// Acceptable: error path with aborted-TX wrapping.
		msg := err.Error()
		if !(strings.Contains(msg, "commit") || strings.Contains(msg, "transaction") || strings.Contains(msg, "abort")) {
			t.Errorf("error not recognisable as TX-abort: %v", err)
		}
	}

	if got := countLinks(t, pool, icSourceID); got != 0 {
		t.Errorf("persisted %d links after CHECK-rejected INSERT, want 0 (TX must roll back)", got)
	}
}
