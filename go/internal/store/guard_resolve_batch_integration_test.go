//go:build integration

// GuardResolveBatch contract (needs_review pipeline W1):
//   - one transaction, every requested id accounted for (resolved XOR skipped+reason)
//   - batch touches ONLY flagged states (needs_review/near_duplicate/possible_duplicate);
//     unflagged ids skip as not_flagged instead of being mass-archived (blast radius)
//   - cross-scope and nonexistent ids collapse to not_found (no existence oracle)
//   - empty write-scope set fails closed with ErrNoScopes (T07 line)
//
//	go test -tags=integration ./internal/store/ -run TestGuardResolveBatch -count=1 -v
package store_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func seedGuardBlock(t *testing.T, pool *pgxpool.Pool, title, scope, guardStatus, typeName string, archived bool) string {
	t.Helper()
	var id string
	err := pool.QueryRow(context.Background(),
		`INSERT INTO context_blocks (category, title, content, scope, guard_status, type_name, is_archived)
		 VALUES ('learnings', $1, 'content of '||$1, $2, $3, $4, $5)
		 RETURNING id::text`,
		title, scope, guardStatus, typeName, archived,
	).Scan(&id)
	if err != nil {
		t.Fatalf("seed %s: %v", title, err)
	}
	return id
}

func skipReasons(skipped []store.GuardSkip) map[string]string {
	m := make(map[string]string, len(skipped))
	for _, s := range skipped {
		m[s.ID] = s.Reason
	}
	return m
}

func TestGuardResolveBatch_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	writeScopes := []string{"private"}

	flaggedA := seedGuardBlock(t, pool, "batch-flagged-a", "private", "needs_review", "knowledge", false)
	flaggedB := seedGuardBlock(t, pool, "batch-flagged-b", "private", "near_duplicate", "knowledge", false)
	clean := seedGuardBlock(t, pool, "batch-clean", "private", "clean", "knowledge", false)
	archived := seedGuardBlock(t, pool, "batch-archived", "private", "needs_review", "knowledge", true)
	foreign := seedGuardBlock(t, pool, "batch-foreign", "hth", "needs_review", "knowledge", false)
	const missing = "00000000-0000-7000-8000-00000000dead"

	t.Run("keep resolves flagged and accounts for every skip reason", func(t *testing.T) {
		ids := []string{flaggedA, flaggedB, clean, archived, foreign, missing, "not-a-uuid", flaggedA}
		resolved, skipped, err := store.GuardResolveBatch(ctx, pool, ids, "keep", writeScopes)
		if err != nil {
			t.Fatalf("batch keep: %v", err)
		}
		if len(resolved) != 2 {
			t.Fatalf("resolved = %d, want 2 (%v)", len(resolved), resolved)
		}
		reasons := skipReasons(skipped)
		want := map[string]string{
			clean:      "not_flagged",
			archived:   "already_archived",
			foreign:    "not_found", // cross-scope must be indistinguishable from missing
			missing:    "not_found",
			"not-a-uuid": "invalid_id",
		}
		for id, reason := range want {
			if reasons[id] != reason {
				t.Errorf("skip[%s] = %q, want %q", id, reasons[id], reason)
			}
		}
		if len(skipped) != len(want) {
			t.Errorf("skipped = %d entries (%v), want %d (duplicate id must be deduped)", len(skipped), reasons, len(want))
		}

		// State assertions against the database, not the return value.
		var status string
		var resolution *string
		if err := pool.QueryRow(ctx,
			`SELECT guard_status, metadata->>'guard_resolution' FROM context_blocks WHERE id = $1`,
			flaggedA).Scan(&status, &resolution); err != nil {
			t.Fatalf("read back: %v", err)
		}
		if status != "active" || resolution == nil || *resolution != "keep" {
			t.Errorf("flaggedA after keep: status=%q resolution=%v", status, resolution)
		}

		// The foreign block must be untouched.
		if err := pool.QueryRow(ctx,
			`SELECT guard_status FROM context_blocks WHERE id = $1`, foreign).Scan(&status); err != nil {
			t.Fatalf("read foreign: %v", err)
		}
		if status != "needs_review" {
			t.Errorf("foreign block was touched: status=%q", status)
		}
	})

	t.Run("archive sets archived_dup and is_archived", func(t *testing.T) {
		id := seedGuardBlock(t, pool, "batch-archive-me", "private", "possible_duplicate", "knowledge", false)
		resolved, skipped, err := store.GuardResolveBatch(ctx, pool, []string{id}, "archive", writeScopes)
		if err != nil {
			t.Fatalf("batch archive: %v", err)
		}
		if len(resolved) != 1 || len(skipped) != 0 {
			t.Fatalf("resolved=%d skipped=%d, want 1/0", len(resolved), len(skipped))
		}
		var status string
		var isArchived bool
		if err := pool.QueryRow(ctx,
			`SELECT guard_status, is_archived FROM context_blocks WHERE id = $1`, id).Scan(&status, &isArchived); err != nil {
			t.Fatalf("read back: %v", err)
		}
		if status != "archived_dup" || !isArchived {
			t.Errorf("after archive: status=%q is_archived=%v", status, isArchived)
		}
	})

	t.Run("fails closed on empty write scopes", func(t *testing.T) {
		_, _, err := store.GuardResolveBatch(ctx, pool, []string{flaggedA}, "keep", []string{})
		if !errors.Is(err, store.ErrNoScopes) {
			t.Fatalf("err = %v, want ErrNoScopes", err)
		}
	})

	t.Run("rejects invalid resolution and oversized batch", func(t *testing.T) {
		if _, _, err := store.GuardResolveBatch(ctx, pool, []string{flaggedA}, "purge", writeScopes); err == nil {
			t.Error("invalid resolution accepted")
		}
		big := make([]string, 501)
		for i := range big {
			big[i] = fmt.Sprintf("00000000-0000-7000-8000-%012d", i)
		}
		_, _, err := store.GuardResolveBatch(ctx, pool, big, "keep", writeScopes)
		if err == nil || !strings.Contains(err.Error(), "cap") {
			t.Errorf("oversized batch: err = %v, want cap error", err)
		}
	})
}

func TestGuardList_TypeFilter_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	seedGuardBlock(t, pool, "list-checkpoint", "private", "needs_review", "checkpoint", false)
	seedGuardBlock(t, pool, "list-knowledge", "private", "needs_review", "knowledge", false)

	items, err := store.GuardList(ctx, pool, []string{"private"}, "", "needs_review", []string{"checkpoint"}, 0)
	if err != nil {
		t.Fatalf("guard list: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1 (%v)", len(items), items)
	}
	if items[0].Title != "list-checkpoint" || items[0].Type != "checkpoint" {
		t.Errorf("item = %+v, want title list-checkpoint type checkpoint", items[0])
	}

	// No filter → both rows visible.
	items, err = store.GuardList(ctx, pool, []string{"private"}, "", "needs_review", nil, 0)
	if err != nil {
		t.Fatalf("guard list unfiltered: %v", err)
	}
	if len(items) != 2 {
		t.Errorf("unfiltered items = %d, want 2", len(items))
	}
}
