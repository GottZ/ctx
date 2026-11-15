//go:build integration

package store_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvec "github.com/pgvector/pgvector-go"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// Issue #198, part B: a real title or content change keeps the block's
// previous vector searchable until the backfill writes the new one. Before,
// every such change nulled the vector and took the block out of both semantic
// arms of ctx_rrf for as long as the backfill queue took.
//
// The change marks the vector stale instead (embedding_stale), and the
// embed-backfill pick reads stale blocks next to unembedded ones. What the
// change still clears is what it cleared before besides the serving vector:
// the _next pair of a re-embed migration (a vector of the new space must never
// stand for other content, see store.InvalidateEmbeddingTx) and the regular
// backfill memo. A block that has no vector stays unembedded and unmarked.
//
// Every writer of a content change is covered: by-id update (plain and scope
// move), upsert, and the three issue writers.
//
//	go test -tags=integration ./internal/store/ -run TestEmbeddingStale -count=1 -v

const (
	stale198Model = "s198-model"
	stale198Next  = "s198-model-next"
)

func stale198Vec(v float32) []float32 {
	vec := make([]float32, 1024)
	for i := range vec {
		vec[i] = v
		if i%3 == 0 {
			vec[i] = -v
		}
	}
	return vec
}

// stale198Seed gives block id a serving vector, a _next vector and a regular
// backfill memo, the state a content change has to resolve.
func stale198Seed(t *testing.T, pool *pgxpool.Pool, id string, vec []float32) {
	t.Helper()
	ctx := context.Background()
	if err := store.StoreEmbedding(ctx, pool, id, stale198Model, vec); err != nil {
		t.Fatalf("seed embedding: %v", err)
	}
	if err := store.StoreEmbeddingNext(ctx, pool, id, stale198Next, vec); err != nil {
		t.Fatalf("seed embedding_next: %v", err)
	}
	if err := store.RecordEmbedFailure(ctx, pool, id, store.EmbedFailureOversize, "oversize: seed", 0, 0); err != nil {
		t.Fatalf("seed backfill memo: %v", err)
	}
}

type stale198State struct {
	sameVector, hasNext bool
	embedModel          string
	memos               int
}

// stale198Read reads the vector state without the marker column, so the
// assertion on it means the same on a schema without the column.
func stale198Read(t *testing.T, pool *pgxpool.Pool, id string, vec []float32) stale198State {
	t.Helper()
	var s stale198State
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(embedding = $2::vector, false), COALESCE(embed_model, ''),
		        embedding_next IS NOT NULL,
		        (SELECT count(*) FROM context_embed_failures f
		          WHERE f.block_id = b.id AND f.migration_id IS NULL)
		   FROM context_blocks b WHERE b.id = $1::uuid`, id, pgvec.NewVector(vec)).
		Scan(&s.sameVector, &s.embedModel, &s.hasNext, &s.memos); err != nil {
		t.Fatalf("read block: %v", err)
	}
	return s
}

func stale198Marker(t *testing.T, pool *pgxpool.Pool, id string) bool {
	t.Helper()
	var stale bool
	if err := pool.QueryRow(context.Background(),
		`SELECT embedding_stale FROM context_blocks WHERE id = $1::uuid`, id).Scan(&stale); err != nil {
		t.Fatalf("read embedding_stale: %v", err)
	}
	return stale
}

// stale198SemanticHit reports whether the semantic arm of ctx_rrf (ann mode)
// ranks block id for the query vector vec, with no lexical match possible.
func stale198SemanticHit(t *testing.T, pool *pgxpool.Pool, id string, vec []float32, scopes []string) bool {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT id::text, cosine_sim FROM ctx_rrf($1, 'zzqqxx', 'zzqqxx', $2::text[],
			p_limit => 50, p_types_visible => ARRAY['knowledge'], p_semantic_mode => 'ann')`,
		pgvec.NewHalfVector(vec), scopes)
	if err != nil {
		t.Fatalf("ctx_rrf: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var got string
		var cos *float64
		if err := rows.Scan(&got, &cos); err != nil {
			t.Fatalf("scan ctx_rrf: %v", err)
		}
		if got == id && cos != nil {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("ctx_rrf rows: %v", err)
	}
	return false
}

func TestEmbeddingStale_ContentChangeKeepsTheVector(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const (
		scopeX  = "s198-x"
		scopeY  = "s198-y"
		content = "the content the vector was made from"
	)
	strptr := func(s string) *string { return &s }
	scopes := []string{scopeX, scopeY}

	cases := []struct {
		name   string
		change func(t *testing.T, id, title string)
	}{
		{"update/content", func(t *testing.T, id, title string) {
			if _, changed, err := store.UpdateBlock(ctx, pool, id, store.UpdateBlockData{Content: strptr(content + " v2")}, scopes); err != nil || !changed {
				t.Fatalf("UpdateBlock: changed=%v err=%v", changed, err)
			}
		}},
		{"update/title", func(t *testing.T, id, title string) {
			if _, changed, err := store.UpdateBlock(ctx, pool, id, store.UpdateBlockData{Title: strptr(title + " v2")}, scopes); err != nil || !changed {
				t.Fatalf("UpdateBlock: changed=%v err=%v", changed, err)
			}
		}},
		{"update/scope_move_with_content", func(t *testing.T, id, title string) {
			if _, changed, err := store.UpdateBlock(ctx, pool, id, store.UpdateBlockData{Content: strptr(content + " v2"), Scope: strptr(scopeY)}, scopes); err != nil || !changed {
				t.Fatalf("UpdateBlock: changed=%v err=%v", changed, err)
			}
		}},
		{"upsert/content", func(t *testing.T, id, title string) {
			b, err := store.UpsertBlock(ctx, pool, "test", title, content+" v2", nil, nil, scopeX, true, store.SensitivityWrite{}, "knowledge")
			if err != nil || b.ID != id {
				t.Fatalf("UpsertBlock: block=%v err=%v, want an update of %s", b, err, id)
			}
		}},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			title := "s198 " + c.name
			b, err := store.UpsertBlock(ctx, pool, "test", title, content, nil, nil, scopeX, true, store.SensitivityWrite{}, "knowledge")
			if err != nil {
				t.Fatalf("seed block: %v", err)
			}
			vec := stale198Vec(float32(i + 1))
			stale198Seed(t, pool, b.ID, vec)
			if !stale198SemanticHit(t, pool, b.ID, vec, scopes) {
				t.Fatalf("precondition: the semantic arm does not rank the seeded block")
			}

			c.change(t, b.ID, title)

			got := stale198Read(t, pool, b.ID, vec)
			if !got.sameVector || got.embedModel != stale198Model {
				t.Fatalf("after the change: same_vector=%v embed_model=%q, want the previous vector and its model kept until the backfill writes the new one",
					got.sameVector, got.embedModel)
			}
			if got.hasNext {
				t.Errorf("after the change: embedding_next is still set, want it cleared (the new space must not hold a vector of other content)")
			}
			if got.memos != 0 {
				t.Errorf("after the change: %d regular backfill memos, want 0 (a content change ends a park)", got.memos)
			}
			if !stale198SemanticHit(t, pool, b.ID, vec, scopes) {
				t.Errorf("after the change: the semantic arm no longer ranks the block, want it ranked by the previous vector")
			}
			if !stale198Marker(t, pool, b.ID) {
				t.Errorf("after the change: embedding_stale = false, want true (the backfill has to re-embed the block)")
			}

			// The backfill's write ends the window.
			fresh := stale198Vec(float32(i + 11))
			if err := store.StoreEmbedding(ctx, pool, b.ID, stale198Model, fresh); err != nil {
				t.Fatalf("StoreEmbedding: %v", err)
			}
			if stale198Marker(t, pool, b.ID) {
				t.Errorf("after StoreEmbedding: embedding_stale = true, want false")
			}
		})
	}

	t.Run("no_vector_stays_unmarked", func(t *testing.T) {
		b, err := store.UpsertBlock(ctx, pool, "test", "s198 no vector", content, nil, nil, scopeX, true, store.SensitivityWrite{}, "knowledge")
		if err != nil {
			t.Fatalf("seed block: %v", err)
		}
		if _, changed, err := store.UpdateBlock(ctx, pool, b.ID, store.UpdateBlockData{Content: strptr(content + " v2")}, scopes); err != nil || !changed {
			t.Fatalf("UpdateBlock: changed=%v err=%v", changed, err)
		}
		var hasEmbedding bool
		if err := pool.QueryRow(ctx, `SELECT embedding IS NOT NULL FROM context_blocks WHERE id = $1::uuid`, b.ID).Scan(&hasEmbedding); err != nil {
			t.Fatal(err)
		}
		if hasEmbedding {
			t.Errorf("a block without a vector has one after a content change")
		}
		if stale198Marker(t, pool, b.ID) {
			t.Errorf("a block without a vector is marked stale, want unmarked (it is pending as unembedded)")
		}
	})

	t.Run("unchanged_content_stays_unmarked", func(t *testing.T) {
		title := "s198 unchanged"
		b, err := store.UpsertBlock(ctx, pool, "test", title, content, nil, nil, scopeX, true, store.SensitivityWrite{}, "knowledge")
		if err != nil {
			t.Fatalf("seed block: %v", err)
		}
		vec := stale198Vec(7)
		if err := store.StoreEmbedding(ctx, pool, b.ID, stale198Model, vec); err != nil {
			t.Fatal(err)
		}
		if _, changed, err := store.UpdateBlock(ctx, pool, b.ID, store.UpdateBlockData{Title: strptr(title), Content: strptr(content), Tags: []string{"t"}}, scopes); err != nil || changed {
			t.Fatalf("UpdateBlock: changed=%v err=%v, want false/nil", changed, err)
		}
		if _, err := store.UpsertBlock(ctx, pool, "test", title, content, []string{"u"}, nil, scopeX, true, store.SensitivityWrite{}, "knowledge"); err != nil {
			t.Fatal(err)
		}
		if got := stale198Read(t, pool, b.ID, vec); !got.sameVector {
			t.Errorf("an unchanged title and content lost the vector")
		}
		if stale198Marker(t, pool, b.ID) {
			t.Errorf("an unchanged title and content marked the vector stale")
		}
	})
}

// TestEmbeddingStale_IssueWritersKeepTheVector: the issue surface and the
// forge pull write a block's title and content outside UpdateBlock. They
// share the invalidation of a content change with it, so they keep the vector
// and clear the _next pair and the memo the same way (before, each of them
// nulled the serving vector alone and left embed_model, embedding_next and the
// memo behind).
func TestEmbeddingStale_IssueWritersKeepTheVector(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const scope = "s198-issues"
	strptr := func(s string) *string { return &s }

	inTx := func(t *testing.T, fn func(tx pgx.Tx) error) {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if err := fn(tx); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}

	writers := []struct {
		name  string
		write func(tx pgx.Tx, id string) error
	}{
		{"update_issue", func(tx pgx.Tx, id string) error {
			_, err := store.UpdateIssueBlock(ctx, tx, id, store.IssueUpdate{Content: strptr("issue body v2")}, nil, []string{scope})
			return err
		}},
		{"pull_update_issue", func(tx pgx.Tx, id string) error {
			_, err := store.PullUpdateIssueBlock(ctx, tx, id, store.ForgeIssueContent{Number: 198, Title: "pulled title v2", Body: "pulled body v2"})
			return err
		}},
		{"pull_update_comment", func(tx pgx.Tx, id string) error {
			_, err := store.PullUpdateCommentBlock(ctx, tx, id, "pulled comment v2")
			return err
		}},
	}
	for i, w := range writers {
		t.Run(w.name, func(t *testing.T) {
			var id string
			inTx(t, func(tx pgx.Tx) error {
				b, err := store.PullCreateIssueBlock(ctx, tx, scope, store.ForgeIssueContent{Number: int64(1000 + i), Title: "issue " + w.name, Body: "issue body"})
				if err != nil {
					return err
				}
				id = b.ID
				return nil
			})
			vec := stale198Vec(float32(i + 21))
			stale198Seed(t, pool, id, vec)

			inTx(t, func(tx pgx.Tx) error { return w.write(tx, id) })

			got := stale198Read(t, pool, id, vec)
			if !got.sameVector || got.embedModel != stale198Model {
				t.Fatalf("after %s: same_vector=%v embed_model=%q, want the previous vector and its model kept", w.name, got.sameVector, got.embedModel)
			}
			if got.hasNext || got.memos != 0 {
				t.Errorf("after %s: has_next=%v memos=%d, want the _next pair and the memo cleared", w.name, got.hasNext, got.memos)
			}
			if !stale198Marker(t, pool, id) {
				t.Errorf("after %s: embedding_stale = false, want true", w.name)
			}
		})
	}
}

// TestEmbeddingStale_ParkEndsTheWindow: a block whose new text the backfill
// parks for good (oversize, an infinity memo) never gets the new vector, so
// the park drops the previous one, and the block is unembedded as before part
// B. A transient failure (wire) keeps it; a park of a block that is not stale
// keeps its vector.
func TestEmbeddingStale_ParkEndsTheWindow(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const scope = "s198-park"
	strptr := func(s string) *string { return &s }

	seed := func(t *testing.T, title string, stale bool) (string, []float32) {
		t.Helper()
		b, err := store.UpsertBlock(ctx, pool, "test", title, "text", nil, nil, scope, true, store.SensitivityWrite{}, "knowledge")
		if err != nil {
			t.Fatal(err)
		}
		vec := stale198Vec(3)
		if err := store.StoreEmbedding(ctx, pool, b.ID, stale198Model, vec); err != nil {
			t.Fatal(err)
		}
		if stale {
			if _, _, err := store.UpdateBlock(ctx, pool, b.ID, store.UpdateBlockData{Content: strptr("text v2")}, []string{scope}); err != nil {
				t.Fatal(err)
			}
		}
		return b.ID, vec
	}
	cases := []struct {
		name     string
		stale    bool
		class    store.EmbedFailureClass
		wantKept bool
	}{
		{"stale_oversize_drops", true, store.EmbedFailureOversize, false},
		{"stale_wire_keeps", true, store.EmbedFailureWire, true},
		{"fresh_oversize_keeps", false, store.EmbedFailureOversize, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id, vec := seed(t, "s198 park "+c.name, c.stale)
			if err := store.RecordEmbedFailure(ctx, pool, id, c.class, string(c.class)+": test", time.Minute, time.Hour); err != nil {
				t.Fatalf("RecordEmbedFailure: %v", err)
			}
			got := stale198Read(t, pool, id, vec)
			if got.sameVector != c.wantKept || (c.wantKept && got.embedModel != stale198Model) || (!c.wantKept && got.embedModel != "") {
				t.Errorf("after a %s memo: same_vector=%v embed_model=%q, want kept=%v", c.class, got.sameVector, got.embedModel, c.wantKept)
			}
			if got.memos != 1 {
				t.Errorf("memos = %d, want 1", got.memos)
			}
			if stale := stale198Marker(t, pool, id); stale != (c.stale && c.wantKept) {
				t.Errorf("embedding_stale = %v, want %v", stale, c.stale && c.wantKept)
			}
		})
	}
}

// TestEmbeddingStale_PickGate: the stale branches of store.PendingPickSQL read
// their own indexes (migration 173) and run only while the index is ready
// with the definition the migration builds. Without it a stale branch would
// be a walk over the table on every pick (a large corpus before the
// out-of-band build, or a same-named index of another definition, which the
// migration leaves alone); with the gate the pick costs what it cost before,
// a stale block left from before waits and keeps its vector, and a new
// content change nulls the vector as before 173, so the unembedded branches
// take the block. Once both indexes are back, a change keeps the vector again
// and the next pick takes the stale block.
func TestEmbeddingStale_PickGate(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const scope = "s198-gate"
	strptr := func(s string) *string { return &s }

	// Ballast the stale branches would walk without their index: 20 000 live,
	// embedded, fresh rows of the class the picks read (not pending, so no
	// pending index holds them, and a partial index over live rows does not
	// skip them). The HNSW index is dropped first: it plays no part in the
	// pick, and 20 000 inserts into it would only cost time.
	if _, err := pool.Exec(ctx, `DROP INDEX IF EXISTS idx_embedding_hnsw`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_blocks (category, title, content, scope, sensitivity, embedding, embed_model, created_at, updated_at)
		 SELECT 'learnings', 'gate ballast ' || i, 'ballast ' || i, $1, 'public',
		        array_fill(0.1::real, ARRAY[1024])::vector, 'ballast',
		        timestamptz '2026-01-01' + make_interval(secs => i), now()
		   FROM generate_series(1, 20000) i`, scope); err != nil {
		t.Fatalf("seed ballast: %v", err)
	}
	stale, err := store.UpsertBlock(ctx, pool, "test", "s198 gate stale", "text", nil, nil, scope, true, store.SensitivityWrite{Value: "public"}, "knowledge")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.StoreEmbedding(ctx, pool, stale.ID, stale198Model, stale198Vec(4)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.UpdateBlock(ctx, pool, stale.ID, store.UpdateBlockData{Content: strptr("text v2")}, []string{scope}); err != nil {
		t.Fatal(err)
	}
	fresh, err := store.UpsertBlock(ctx, pool, "test", "s198 gate unembedded", "text", nil, nil, scope, true, store.SensitivityWrite{Value: "public"}, "knowledge")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `ANALYZE context_blocks`); err != nil {
		t.Fatal(err)
	}

	unbound := store.PendingClasses{Shared: []string{"public"}, Dead: []string{}, PairScopes: []string{}, PairLevels: []string{}}
	bound := store.PendingClasses{Shared: []string{}, Dead: []string{}, PairScopes: []string{scope}, PairLevels: []string{"public"}}
	pickSQL := store.PendingPickSQL("id", false, store.EmbedFailureExcludedPredicate)
	drain := func(t *testing.T, cl store.PendingClasses) []string {
		t.Helper()
		skip := []string{}
		for {
			var id string
			err := pool.QueryRow(ctx, pickSQL, append(cl.Args(), skip)...).Scan(&id)
			if errors.Is(err, pgx.ErrNoRows) {
				return skip
			}
			if err != nil {
				t.Fatalf("pick: %v", err)
			}
			skip = append(skip, id)
		}
	}
	both := fmt.Sprint([]string{stale.ID, fresh.ID})
	for name, cl := range map[string]store.PendingClasses{"unbound": unbound, "bound": bound} {
		if got := fmt.Sprint(drain(t, cl)); got != both {
			t.Fatalf("%s with the indexes: picked %s, want the stale block, then the unembedded one %s", name, got, both)
		}
	}

	// Without the scope index, and with a same-named sens index of another
	// definition: both gates are closed.
	if _, err := pool.Exec(ctx, `DROP INDEX idx_embedding_stale_scope_sens, idx_embedding_stale_sens`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `CREATE INDEX idx_embedding_stale_sens ON context_blocks (sensitivity)`); err != nil {
		t.Fatal(err)
	}
	for name, cl := range map[string]store.PendingClasses{"unbound": unbound, "bound": bound} {
		if got := fmt.Sprint(drain(t, cl)); got != fmt.Sprint([]string{fresh.ID}) {
			t.Errorf("%s without the indexes: picked %s, want only the unembedded block", name, got)
		}
		args := append(cl.Args(), []string{})
		for plan, c := range map[string]testdb.PlanCost{
			"generic": testdb.ExplainGeneric(t, pool, pickSQL, args...),
			"custom":  testdb.ExplainCustom(t, pool, pickSQL, args...),
		} {
			t.Logf("%s, %s plan without the stale indexes: %v", name, plan, c)
			if c.Buffers > 200 {
				t.Errorf("%s, %s plan without the stale indexes: %d buffers, more than 200 — a stale branch ran without its index", name, plan, c.Buffers)
			}
		}
	}
	if got := stale198Read(t, pool, stale.ID, stale198Vec(4)); !got.sameVector {
		t.Errorf("the waiting stale block lost its vector")
	}
	// A content change without the indexes nulls the vector (pre-173).
	later, err := store.UpsertBlock(ctx, pool, "test", "s198 gate later", "text", nil, nil, scope, true, store.SensitivityWrite{Value: "public"}, "knowledge")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.StoreEmbedding(ctx, pool, later.ID, stale198Model, stale198Vec(5)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.UpdateBlock(ctx, pool, later.ID, store.UpdateBlockData{Content: strptr("text v2")}, []string{scope}); err != nil {
		t.Fatal(err)
	}
	if got := stale198Read(t, pool, later.ID, stale198Vec(5)); got.sameVector || got.embedModel != "" || stale198Marker(t, pool, later.ID) {
		t.Errorf("a change without the stale indexes: same_vector=%v embed_model=%q stale=%v, want the vector nulled as before migration 173",
			got.sameVector, got.embedModel, stale198Marker(t, pool, later.ID))
	}
	if got := fmt.Sprint(drain(t, unbound)); got != fmt.Sprint([]string{fresh.ID, later.ID}) {
		t.Errorf("unbound without the indexes: picked %s, want the two unembedded blocks", got)
	}

	// Both indexes back: the stale block is picked again, a change keeps the
	// vector again.
	if _, err := pool.Exec(ctx, `DROP INDEX idx_embedding_stale_sens`); err != nil {
		t.Fatal(err)
	}
	for _, st := range []string{
		`CREATE INDEX idx_embedding_stale_sens ON context_blocks (sensitivity, created_at, id) WHERE embedding_stale AND NOT is_archived`,
		`CREATE INDEX idx_embedding_stale_scope_sens ON context_blocks (scope, sensitivity, created_at, id) WHERE embedding_stale AND NOT is_archived`,
	} {
		if _, err := pool.Exec(ctx, st); err != nil {
			t.Fatal(err)
		}
	}
	if got := fmt.Sprint(drain(t, bound)); got != fmt.Sprint([]string{stale.ID, fresh.ID, later.ID}) {
		t.Errorf("bound after the indexes are back: picked %s, want the stale block first", got)
	}
	if err := store.StoreEmbedding(ctx, pool, later.ID, stale198Model, stale198Vec(5)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.UpdateBlock(ctx, pool, later.ID, store.UpdateBlockData{Content: strptr("text v3")}, []string{scope}); err != nil {
		t.Fatal(err)
	}
	if got := stale198Read(t, pool, later.ID, stale198Vec(5)); !got.sameVector || !stale198Marker(t, pool, later.ID) {
		t.Errorf("a change with both indexes back: same_vector=%v, want the vector kept and marked stale", got.sameVector)
	}
}
