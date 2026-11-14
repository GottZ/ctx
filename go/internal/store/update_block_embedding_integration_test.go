//go:build integration

package store_test

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// Issue #198, part A, at the store boundary: UpdateBlock itself decides whether
// the block's vector survives, and it does so inside the update's transaction.
//
//   - A title or content that differs from the stored row clears the live and
//     the _next vector pair and the regular backfill memo by the time
//     UpdateBlock returns. No caller afterwork is involved: a clear that ran as
//     a second step could get lost after the update had committed, and since
//     the decision is a difference, sending the same update again would not
//     repair it.
//   - A title or content equal to the stored row keeps all of them and reports
//     false.
//
// Both execution shapes are covered: the plain update and the scope move, which
// scans the same RETURNING column on its own code path.
//
// Run with:
//
//	go test -tags=integration ./internal/store/ -run TestUpdateBlock_EmbeddingFollowsTitleAndContent -count=1 -v
func TestUpdateBlock_EmbeddingFollowsTitleAndContent(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	const (
		scopeX  = "u198-x"
		scopeY  = "u198-y"
		content = "base content"
		model   = "u198-model"
		next    = "u198-model-next"
	)
	vec := make([]float32, 1024)
	for i := range vec {
		vec[i] = 0.5
	}
	strptr := func(s string) *string { return &s }

	// mkBlock seeds a block in scopeX with both vector pairs and a regular
	// backfill memo row, the three things ClearEmbeddingTx removes.
	mkBlock := func(t *testing.T, title string) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO context_blocks (category, title, content, scope)
			 VALUES ('test', $1, $2, $3) RETURNING id::text`,
			title, content, scopeX).Scan(&id); err != nil {
			t.Fatalf("seed block: %v", err)
		}
		if err := store.StoreEmbedding(ctx, pool, id, model, vec); err != nil {
			t.Fatalf("seed embedding: %v", err)
		}
		if err := store.StoreEmbeddingNext(ctx, pool, id, next, vec); err != nil {
			t.Fatalf("seed embedding_next: %v", err)
		}
		if err := store.RecordEmbedFailure(ctx, pool, id, store.EmbedFailureOversize, "oversize: seed", 0, 0); err != nil {
			t.Fatalf("seed backfill memo: %v", err)
		}
		return id
	}

	type state struct {
		hasEmbedding, hasNext bool
		embedModel, nextModel string
		memos                 int
	}
	read := func(t *testing.T, id string) state {
		t.Helper()
		var s state
		if err := pool.QueryRow(ctx,
			`SELECT embedding IS NOT NULL, COALESCE(embed_model, ''),
			        embedding_next IS NOT NULL, COALESCE(embed_model_next, ''),
			        (SELECT count(*) FROM context_embed_failures f
			          WHERE f.block_id = b.id AND f.migration_id IS NULL)
			 FROM context_blocks b WHERE b.id = $1::uuid`, id).
			Scan(&s.hasEmbedding, &s.embedModel, &s.hasNext, &s.nextModel, &s.memos); err != nil {
			t.Fatalf("read block: %v", err)
		}
		return s
	}
	kept := state{hasEmbedding: true, hasNext: true, embedModel: model, nextModel: next, memos: 1}
	cleared := state{}

	cases := []struct {
		name string
		data func(title string) store.UpdateBlockData
		want bool // needsReEmbed, and the vector state that goes with it
	}{
		{"plain/unchanged", func(title string) store.UpdateBlockData {
			return store.UpdateBlockData{Title: strptr(title), Content: strptr(content), Tags: []string{"new"}}
		}, false},
		{"plain/tags_only", func(string) store.UpdateBlockData {
			return store.UpdateBlockData{Tags: []string{"new"}}
		}, false},
		{"plain/content_changed", func(title string) store.UpdateBlockData {
			return store.UpdateBlockData{Title: strptr(title), Content: strptr(content + " v2")}
		}, true},
		{"plain/title_changed", func(title string) store.UpdateBlockData {
			return store.UpdateBlockData{Title: strptr(title + " v2"), Content: strptr(content)}
		}, true},
		{"scope_move/unchanged", func(title string) store.UpdateBlockData {
			return store.UpdateBlockData{Title: strptr(title), Content: strptr(content), Scope: strptr(scopeY)}
		}, false},
		{"scope_move/scope_only", func(string) store.UpdateBlockData {
			return store.UpdateBlockData{Scope: strptr(scopeY)}
		}, false},
		{"scope_move/content_changed", func(string) store.UpdateBlockData {
			return store.UpdateBlockData{Content: strptr(content + " v2"), Scope: strptr(scopeY)}
		}, true},
		{"scope_move/title_changed", func(title string) store.UpdateBlockData {
			return store.UpdateBlockData{Title: strptr(title + " v2"), Scope: strptr(scopeY)}
		}, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			title := "u198 store " + c.name
			id := mkBlock(t, title)
			if pre := read(t, id); pre != kept {
				t.Fatalf("precondition: seeded state = %+v, want %+v", pre, kept)
			}

			blk, needsReEmbed, err := store.UpdateBlock(ctx, pool, id, c.data(title), []string{scopeX, scopeY})
			if err != nil || blk == nil {
				t.Fatalf("UpdateBlock: blk=%v err=%v", blk, err)
			}

			if needsReEmbed != c.want {
				t.Errorf("needsReEmbed = %v, want %v", needsReEmbed, c.want)
			}
			// Read straight after UpdateBlock, with no handler afterwork in
			// between: the state must already be final.
			want := kept
			if c.want {
				want = cleared
			}
			if got := read(t, id); got != want {
				t.Errorf("after UpdateBlock: %+v, want %+v", got, want)
			}
		})
	}
}
