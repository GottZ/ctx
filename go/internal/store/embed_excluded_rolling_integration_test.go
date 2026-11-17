//go:build integration

package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// TestEmbedExcludedRollingOldBinary: a binary before migration 176
// (v5.49.3) keeps running against the schema of 176 until it is replaced.
// Its statements are the ones of 6d6ddacf: the pick with the one-argument
// type predicate, the /status stale count with it, and the reconcile of 175
// (reconcile175, a verbatim copy). They run without an error; the old pick
// leaves a block the new rule marked; the old reconcile finds its queue empty
// and so cannot consume an entry of the new rule (which it would decide by
// the one-argument answer and delete without marking); the new reconcile
// takes the entries the old binary's registry writes queued. The old
// statements that read idx_blocks_embed_excluded run on its definition of
// 176 and cannot unmark a block the owner rule marks.
func TestEmbedExcludedRollingOldBinary(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	reconcile(t, pool)
	ownerTenant(t, pool, "rl-a", "rl-a")

	oneArg := "ctx_block_type_never_embedded(context_blocks.type_name)"
	twoArg := "ctx_block_type_never_embedded(context_blocks.type_name, context_blocks.scope)"
	newPick := store.PendingPickSQL("id", true, store.EmbedFailureExcludedPredicate)
	if n := strings.Count(newPick, twoArg); n != 4 {
		t.Fatalf("the pick states the two-argument predicate %d times, want 4 (one per branch)", n)
	}
	oldPick := strings.ReplaceAll(newPick, twoArg, oneArg)

	// The old binary's type API write and block writes go through the new
	// triggers (their Go code is not part of this change).
	createType(t, pool, "rl-x", "rl-a", "excluded")
	ids := ownerSeed(t, pool, "rl-a", "rl-x", 3)
	plain := storeTyped(t, pool, "rl-a", "rl plain", "")
	for _, id := range ids {
		if !marker(t, pool, id) {
			t.Errorf("block %s written while the old binary runs is not marked by the new trigger", id)
		}
	}
	var sens string
	if err := pool.QueryRow(ctx, `SELECT sensitivity FROM context_blocks WHERE id = $1`, ids[0]).Scan(&sens); err != nil {
		t.Fatal(err)
	}
	cl := store.NewPendingClasses()
	cl.PairScopes, cl.PairLevels = []string{"rl-a"}, []string{sens}
	var picked string
	if err := pool.QueryRow(ctx, oldPick, append(cl.Args(), []string{})...).Scan(&picked); err != nil {
		t.Fatalf("old pick: %v", err)
	}
	if picked != plain.ID {
		t.Errorf("old pick returned %s, want the plain block %s (the marked ones are out of its indexes)", picked, plain.ID)
	}

	var never bool
	if err := pool.QueryRow(ctx, `SELECT ctx_block_type_never_embedded('rl-x')`).Scan(&never); err != nil {
		t.Fatalf("one-argument function: %v", err)
	}
	if never {
		t.Errorf("the one-argument function changed its answer for a tenant-own type (175 said false)")
	}
	var stale int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_blocks
		WHERE embedding_stale AND NOT is_archived AND embedding IS NOT NULL AND NOT `+oneArg).Scan(&stale); err != nil {
		t.Fatalf("old /status stale count: %v", err)
	}

	// Unmarked blocks of the name and an entry for them: the old reconcile
	// does not see the entry, the new one marks them.
	if _, err := pool.Exec(ctx, `INSERT INTO context_blocks (category, title, content, type_name, scope, sensitivity, updated_at)
		SELECT 'own-cat', 'rl late ' || i, 'x', 'rl-y', 'rl-a', 'public', now() FROM generate_series(1, 5) i`); err != nil {
		t.Fatal(err)
	}
	createType(t, pool, "rl-y", "rl-a", "excluded")
	old, err := reconcile175(ctx, pool)
	if err != nil {
		t.Fatalf("old reconcile on the new schema: %v", err)
	}
	if old.Names != 0 {
		t.Errorf("the old reconcile worked on %d entries, want 0", old.Names)
	}
	var queued int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_embed_excluded_queue WHERE name = 'rl-y'`).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 1 {
		t.Errorf("the entry of rl-y is queued %d times after the old reconcile, want 1", queued)
	}
	reconcile(t, pool)
	if got := markedCount(t, pool, "rl-a", "rl-y"); got != 5 {
		t.Errorf("%d/5 blocks of rl-y marked after the new reconcile", got)
	}

	// The old binary's statements that read idx_blocks_embed_excluded, its
	// left check and its clear (for entries of its old queue, which nothing
	// fills after 176), run on the definition of 176. The old clear decides
	// by the one-argument answer (false for rl-y) and writes the rows; the
	// trigger decides each by the owner rule and keeps it marked.
	if def, _ := m176Index(t, pool); def != m176IndexDef {
		t.Fatalf("idx_blocks_embed_excluded is %s, want the definition of 176", def)
	}
	var left bool
	if err := pool.QueryRow(ctx, v175LeftSQL, "rl-y", false).Scan(&left); err != nil {
		t.Fatalf("old left check on the index of 176: %v", err)
	}
	if !left {
		t.Errorf("the old left check does not see rl-y's marked blocks")
	}
	var wrote, reached int
	var last *string
	if err := pool.QueryRow(ctx, v175ClearSQL, "rl-y", store.EmbedExcludedReconcileBatch, false, v175KeyStart.id).
		Scan(&wrote, &reached, &last); err != nil {
		t.Fatalf("old clear on the index of 176: %v", err)
	}
	if wrote != 5 || reached != 0 {
		t.Errorf("old clear wrote %d rows, %d of them unmarked; want 5 written, 0 unmarked", wrote, reached)
	}
	if got := markedCount(t, pool, "rl-a", "rl-y"); got != 5 {
		t.Errorf("%d/5 blocks of rl-y marked after the old clear", got)
	}
}
