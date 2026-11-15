//go:build integration

// Issue #198, part B, the topic-map centroids: a title or content change keeps
// the member's previous vector (marked stale) until the backfill writes the
// new one. Before, the change nulled the vector, so the member's tag in the
// diff carrier went from '+' to '-' and back, and both transitions made the
// partition dirty. With the vector kept, a '+'/'-' tag stays '+' through the
// whole window, the partition hashes as it did before the change, and its
// centroid would keep the member's previous vector for good. The stale tag
// ('~', memberSetExpr) makes both ends of the window a recompute.
//
// ROT-PROBE: drop the `WHEN b.embedding_stale THEN '~'` arm from
// memberSetExpr ⇒ the pass after the new vector reports 0 recomputed and the
// centroid still points at the previous vector.
//
//	go test -tags=integration ./internal/overview/ -run TestCentroidDiffSeesStaleVectorReplacement -count=1 -v
package overview

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestCentroidDiffSeesStaleVectorReplacement(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	oneHot := func(dim int) []float32 {
		v := make([]float32, 1024)
		v[dim-1] = 1
		return v
	}
	// cosineTo reads the centroid's cosine similarity to the one-hot of dim.
	cosineTo := func(t *testing.T, dim int) float64 {
		t.Helper()
		var c float64
		if err := pool.QueryRow(ctx,
			`SELECT 1 - (centroid <=> $2::vector::halfvec(1024)) FROM graph_cluster_centroid WHERE topic_id = $1::uuid`,
			c8TopicID(31), pgvecText(oneHot(dim))).Scan(&c); err != nil {
			t.Fatalf("read centroid: %v", err)
		}
		return c
	}

	c8Block(t, pool, c8BlockID(31), "private", 31, true)
	c8Block(t, pool, c8BlockID(32), "private", 32, true)
	c8Partition(t, pool, c8TopicID(31), c8ClusterID(31), "private", []string{c8BlockID(31), c8BlockID(32)})
	if _, err := BuildCentroids(ctx, pool, []string{"private"}, c8Opts()); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if c := cosineTo(t, 32); c < 0.5 {
		t.Fatalf("precondition: centroid cosine to member 32's vector = %.3f, want it in the average", c)
	}

	// Member 32 is rewritten: its vector is kept, marked stale.
	content := "c8 fixture rewritten"
	if _, changed, err := store.UpdateBlock(ctx, pool, c8BlockID(32), store.UpdateBlockData{Content: &content}, []string{"private"}); err != nil || !changed {
		t.Fatalf("UpdateBlock: changed=%v err=%v", changed, err)
	}
	if _, err := BuildCentroids(ctx, pool, []string{"private"}, c8Opts()); err != nil {
		t.Fatalf("pass in the window: %v", err)
	}

	// The backfill writes the vector of the new text.
	if err := store.StoreEmbedding(ctx, pool, c8BlockID(32), "test-embed", oneHot(33)); err != nil {
		t.Fatal(err)
	}
	st, err := BuildCentroids(ctx, pool, []string{"private"}, c8Opts())
	if err != nil {
		t.Fatalf("pass after the new vector: %v", err)
	}
	if st.Recomputed != 1 {
		t.Errorf("pass after the new vector = %+v, want 1 recomputed", st)
	}
	if c := cosineTo(t, 32); c > 0.01 {
		t.Errorf("centroid cosine to member 32's previous vector = %.3f, want 0 (the previous vector left the average)", c)
	}
	if c := cosineTo(t, 33); c < 0.5 {
		t.Errorf("centroid cosine to member 32's new vector = %.3f, want it in the average", c)
	}
}

// pgvecText renders a vector of zeros and ones (the one-hots of this file) as a
// pgvector text literal.
func pgvecText(v []float32) string {
	b := make([]byte, 0, len(v)*2+2)
	b = append(b, '[')
	for i, f := range v {
		if i > 0 {
			b = append(b, ',')
		}
		if f == 0 {
			b = append(b, '0')
		} else {
			b = append(b, '1')
		}
	}
	return string(append(b, ']'))
}
