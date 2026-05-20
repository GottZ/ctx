//go:build integration

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// insertResolveTestBlock writes a minimal block — only id, category, title,
// scope, created_at, updated_at. No embedding (resolve is text-only).
func insertResolveTestBlock(t *testing.T, pool *pgxpool.Pool, id, title, scope string, ts time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := pool.Exec(ctx,
		`INSERT INTO context_blocks
			(id, category, title, content, scope, created_at, updated_at)
		 VALUES ($1::uuid, $2, $3, $4, $5, $6, $6)`,
		id, "resolve_test", title, "content", scope, ts,
	)
	if err != nil {
		t.Fatalf("insert resolve-test block %s: %v", id, err)
	}
}

func TestResolveBlockID_UniquePrefix(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)

	insertResolveTestBlock(t, pool, "019e4444-0000-7000-9000-000000000001", "alpha", "private", now)
	insertResolveTestBlock(t, pool, "019e5555-0000-7000-9000-000000000001", "beta", "private", now)

	id, matches, err := store.ResolveBlockID(context.Background(), pool, "019e4444", []string{"private"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "019e4444-0000-7000-9000-000000000001" {
		t.Errorf("got id %q, want first block", id)
	}
	if len(matches) != 1 {
		t.Errorf("got %d matches, want 1", len(matches))
	}
}

func TestResolveBlockID_AmbiguousPrefix(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)

	insertResolveTestBlock(t, pool, "019e6666-0000-7000-9000-00000000000a", "alpha", "private", now)
	insertResolveTestBlock(t, pool, "019e6666-0000-7000-9000-00000000000b", "beta", "private", now.Add(time.Second))

	id, matches, err := store.ResolveBlockID(context.Background(), pool, "019e6666", []string{"private"})
	if !errors.Is(err, store.ErrAmbiguousID) {
		t.Fatalf("want ErrAmbiguousID, got %v", err)
	}
	if id != "" {
		t.Errorf("ambiguous resolve must return empty id, got %q", id)
	}
	if len(matches) != 2 {
		t.Fatalf("want 2 matches, got %d", len(matches))
	}
	// Order: newer first (ORDER BY updated_at DESC).
	if matches[0].Title != "beta" || matches[1].Title != "alpha" {
		t.Errorf("unexpected order: %+v", matches)
	}
}

func TestResolveBlockID_NoMatch(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	id, matches, err := store.ResolveBlockID(context.Background(), pool, "deadbeef", []string{"private"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "" || matches != nil {
		t.Errorf("want empty result, got id=%q matches=%+v", id, matches)
	}
}

func TestResolveBlockID_ScopeFiltered(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)

	insertResolveTestBlock(t, pool, "019e7777-0000-7000-9000-000000000001", "shared-only", "shared", now)

	// Caller has only 'private' scope → block invisible.
	id, _, err := store.ResolveBlockID(context.Background(), pool, "019e7777", []string{"private"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "" {
		t.Errorf("scope-filtered lookup must miss, got %q", id)
	}

	// Caller with 'shared' scope sees it.
	id, _, err = store.ResolveBlockID(context.Background(), pool, "019e7777", []string{"private", "shared"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "019e7777-0000-7000-9000-000000000001" {
		t.Errorf("want resolved id with shared scope, got %q", id)
	}
}

func TestResolveBlockID_ArchivedExcluded(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	const fullID = "019e8888-0000-7000-9000-000000000001"

	insertResolveTestBlock(t, pool, fullID, "archived-block", "private", now)

	// Archive it.
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE context_blocks SET is_archived = true WHERE id = $1::uuid`, fullID); err != nil {
		t.Fatalf("archive block: %v", err)
	}

	id, _, err := store.ResolveBlockID(ctx, pool, "019e8888", []string{"private"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "" {
		t.Errorf("archived block must be invisible to resolve, got %q", id)
	}
}
