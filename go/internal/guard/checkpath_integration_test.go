//go:build integration

package guard

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"testing"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// TestPrepareChecksWindowReachesTheFunction (#381): within one transaction,
// the same check through ctx_guard_check reads more of the HNSW index after
// prepareChecks set ef_search 800 than after it set 40 — the function
// searches with the window the batch sets (it carries no SET clause of its
// own for it). Same block, same graph, only the window differs.
func TestPrepareChecksWindowReachesTheFunction(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	rng := rand.New(rand.NewSource(3817))
	set := blocktype.NewRegistry().Snapshot()
	vec := func() []float32 {
		v := make([]float32, 1024)
		var n float64
		for i := range v {
			x := rng.NormFloat64()
			v[i] = float32(x)
			n += x * x
		}
		for i := range v {
			v[i] = float32(float64(v[i]) / math.Sqrt(n))
		}
		return v
	}
	write := func(typeName, title string) string {
		b, err := store.UpsertBlock(ctx, pool, "cp381", title, "body of "+title, nil, nil,
			"private", true, store.SensitivityWrite{}, typeName)
		if err != nil {
			t.Fatalf("upsert %q: %v", title, err)
		}
		if err := store.StoreEmbedding(ctx, pool, b.ID, "cp381-model", vec()); err != nil {
			t.Fatalf("embed %q: %v", title, err)
		}
		return b.ID
	}
	for i := 0; i < 900; i++ {
		write("knowledge", fmt.Sprintf("bulk-%d", i))
	}
	id := write("knowledge", "probe")
	if _, err := pool.Exec(ctx, `ANALYZE context_blocks`); err != nil {
		t.Fatal(err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	blocks := []pendingBlock{{id: id, typeName: "knowledge"}}
	read := map[int]int64{}
	for _, ef := range []int{40, 800} {
		if err := prepareChecks(ctx, tx, blocks, set, Search{EfSearch: ef, ExactMaxCandidates: 0}); err != nil {
			t.Fatal(err)
		}
		var h0, h1 int64
		if err := tx.QueryRow(ctx, `SELECT pg_stat_get_xact_blocks_fetched('idx_embedding_hnsw'::regclass)`).Scan(&h0); err != nil {
			t.Fatal(err)
		}
		dup, rev := set.GuardThresholds("knowledge")
		if _, err := checkBlock(ctx, tx, id, dup, rev, set.GuardCandidateTypes(), false, false); err != nil {
			t.Fatalf("check (ef %d): %v", ef, err)
		}
		if err := tx.QueryRow(ctx, `SELECT pg_stat_get_xact_blocks_fetched('idx_embedding_hnsw'::regclass)`).Scan(&h1); err != nil {
			t.Fatal(err)
		}
		read[ef] = h1 - h0
	}
	t.Logf("HNSW blocks of one check: ef 40 %d, ef 800 %d", read[40], read[800])
	if read[40] < 2 {
		t.Fatalf("ef 40: the check read %d HNSW blocks, want the index searched", read[40])
	}
	if read[800] <= read[40] {
		t.Errorf("the check read %d HNSW blocks at ef 800 and %d at 40 — the function does not search with the batch's window", read[800], read[40])
	}
}
