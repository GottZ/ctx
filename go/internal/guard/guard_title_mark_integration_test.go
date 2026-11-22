//go:build integration

// Issue #452 point 5, the guard's check mark (metadata.guard_checked_at): the
// duplicate guard decides on the vector of the block's title and content, and
// guardPendingWhere takes a block out of its queue for as long as the mark
// stands. A write that changes the title or the content therefore drops the
// mark, so the guard checks the block again once the vector of the new text
// is written (store.InvalidateEmbeddingTx, the step every such write runs).
// Before, UpdateBlock dropped it for a content write only, and the issue
// writers (UpdateIssueBlock, the forge pull update, the forge push rename)
// never did: a renamed block kept the verdict about its earlier text for good.
//
// The guard's own verdict write is metadata on the block row and runs no
// invalidation, so a checked block does not swing back into the queue: the
// batches after the re-check process nothing.
//
//	go test -tags=integration ./internal/guard/ -run TestGuardCheckFollowsTheText -count=1 -v
package guard_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/guard"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func i452Tx(t *testing.T, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestGuardCheckFollowsTheText(t *testing.T) {
	const scope = "private"
	writable := []string{scope}
	str := func(s string) *string { return &s }
	newIssue := func(t *testing.T, pool *pgxpool.Pool, title string) string {
		t.Helper()
		var id string
		i452Tx(t, pool, func(tx pgx.Tx) error {
			b, err := store.InsertIssueBlock(context.Background(), tx, store.IssueFields{Scope: scope, Title: title, Content: "issue body"}, writable)
			if b != nil {
				id = b.ID
			}
			return err
		})
		return id
	}

	for _, tc := range []struct {
		name string
		// create returns the block; write changes it.
		create  func(t *testing.T, pool *pgxpool.Pool) string
		write   func(t *testing.T, pool *pgxpool.Pool, id string)
		recheck bool
	}{
		{"title update", func(t *testing.T, pool *pgxpool.Pool) string {
			b, err := store.UpsertBlock(context.Background(), pool, "learnings", "i452 harbour", "harbour body", nil, nil, scope, true, store.SensitivityWrite{}, "knowledge")
			if err != nil {
				t.Fatal(err)
			}
			return b.ID
		}, func(t *testing.T, pool *pgxpool.Pool, id string) {
			b, changed, err := store.UpdateBlock(context.Background(), pool, id, store.UpdateBlockData{Title: str("i452 harbour, renamed")}, writable)
			if err != nil || !changed {
				t.Fatalf("update: changed=%v err=%v", changed, err)
			}
			if _, ok := b.Metadata["guard_checked_at"]; ok {
				t.Errorf("the update's answer carries the check mark the write removed")
			}
		}, true},
		// A person's keep is a verdict on the old vector too: the rename puts
		// the block up for a check again (as a content change always did) and
		// leaves the trace of the decision.
		{"title update of a kept block", func(t *testing.T, pool *pgxpool.Pool) string {
			b, err := store.UpsertBlock(context.Background(), pool, "learnings", "i452 kept", "kept body", nil, nil, scope, true, store.SensitivityWrite{}, "knowledge")
			if err != nil {
				t.Fatal(err)
			}
			return b.ID
		}, func(t *testing.T, pool *pgxpool.Pool, id string) {
			ctx := context.Background()
			if _, err := store.GuardResolve(ctx, pool, id, "keep", writable); err != nil {
				t.Fatalf("guard resolve: %v", err)
			}
			if _, changed, err := store.UpdateBlock(ctx, pool, id, store.UpdateBlockData{Title: str("i452 kept, renamed")}, writable); err != nil || !changed {
				t.Fatalf("update: changed=%v err=%v", changed, err)
			}
			var resolution string
			if err := pool.QueryRow(ctx, `SELECT COALESCE(metadata->>'guard_resolution', '') FROM context_blocks WHERE id = $1::uuid`, id).Scan(&resolution); err != nil {
				t.Fatal(err)
			}
			if resolution != "keep" {
				t.Errorf("the rename dropped the trace of the person's decision: guard_resolution=%q", resolution)
			}
		}, true},
		{"issue title", func(t *testing.T, pool *pgxpool.Pool) string { return newIssue(t, pool, "i452 crane") },
			func(t *testing.T, pool *pgxpool.Pool, id string) {
				i452Tx(t, pool, func(tx pgx.Tx) error {
					b, err := store.UpdateIssueBlock(context.Background(), tx, id, store.IssueUpdate{Title: str("i452 crane, renamed")}, nil, writable)
					if b != nil {
						if _, ok := b.Metadata["guard_checked_at"]; ok {
							t.Errorf("the issue update's answer carries the check mark the write removed")
						}
					}
					return err
				})
			}, true},
		{"issue body", func(t *testing.T, pool *pgxpool.Pool) string { return newIssue(t, pool, "i452 ferry") },
			func(t *testing.T, pool *pgxpool.Pool, id string) {
				i452Tx(t, pool, func(tx pgx.Tx) error {
					_, err := store.UpdateIssueBlock(context.Background(), tx, id, store.IssueUpdate{Content: str("issue body, rewritten")}, nil, writable)
					return err
				})
			}, true},
		{"forge pull update", func(t *testing.T, pool *pgxpool.Pool) string {
			var id string
			i452Tx(t, pool, func(tx pgx.Tx) error {
				b, err := store.PullCreateIssueBlock(context.Background(), tx, scope, store.ForgeIssueContent{Number: 41, Title: "pulled", Body: "forge body"})
				if b != nil {
					id = b.ID
				}
				return err
			})
			return id
		}, func(t *testing.T, pool *pgxpool.Pool, id string) {
			i452Tx(t, pool, func(tx pgx.Tx) error {
				_, err := store.PullUpdateIssueBlock(context.Background(), tx, id, store.ForgeIssueContent{Number: 41, Title: "pulled, renamed on the forge", Body: "forge body"})
				return err
			})
		}, true},
		{"forge push rename", func(t *testing.T, pool *pgxpool.Pool) string { return newIssue(t, pool, "i452 pushed") },
			func(t *testing.T, pool *pgxpool.Pool, id string) {
				if err := store.FinalizePushCreateIssue(context.Background(), pool, id, 77, "h", []byte(`{}`), nil); err != nil {
					t.Fatalf("finalize push create: %v", err)
				}
			}, true},
		{"tags only", func(t *testing.T, pool *pgxpool.Pool) string { return newIssue(t, pool, "i452 tagged") },
			func(t *testing.T, pool *pgxpool.Pool, id string) {
				i452Tx(t, pool, func(tx pgx.Tx) error {
					_, err := store.UpdateIssueBlock(context.Background(), tx, id, store.IssueUpdate{Tags: []string{"x"}}, nil, writable)
					return err
				})
			}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := testdb.SetupTestDB(t)
			ctx := context.Background()
			batch := func(t *testing.T, what string, want int) {
				t.Helper()
				n, err := guard.RunGuardBatch(ctx, pool, gitSet(), 50, guard.SearchIndexForTest)
				if err != nil || n != want {
					t.Errorf("%s: guard batch processed %d (err %v), want %d", what, n, err, want)
				}
			}
			checked := func(t *testing.T, id string) (mark, stale bool) {
				t.Helper()
				if err := pool.QueryRow(ctx,
					`SELECT metadata ? 'guard_checked_at', embedding_stale FROM context_blocks WHERE id = $1::uuid`, id).
					Scan(&mark, &stale); err != nil {
					t.Fatalf("read %s: %v", id, err)
				}
				return mark, stale
			}

			id := tc.create(t, pool)
			if err := store.StoreEmbedding(ctx, pool, id, "test-embed", unitVec1024(3, 1)); err != nil {
				t.Fatal(err)
			}
			batch(t, "first check", 1)
			if mark, _ := checked(t, id); !mark {
				t.Fatalf("precondition: the first batch left no check mark")
			}

			tc.write(t, pool, id)
			if mark, _ := checked(t, id); tc.recheck == mark {
				t.Errorf("after the write: check mark present=%v, want %v", mark, !tc.recheck)
			}
			// The backfill writes the vector of the block's text.
			if err := store.StoreEmbedding(ctx, pool, id, "test-embed", unitVec1024(4, 1)); err != nil {
				t.Fatal(err)
			}
			want := 0
			if tc.recheck {
				want = 1
			}
			batch(t, "after the write and the new vector", want)

			// No swing: the verdict write runs no invalidation, so the next
			// batches find nothing and the vector stays current.
			batch(t, "second batch after the re-check", 0)
			batch(t, "third batch after the re-check", 0)
			if mark, stale := checked(t, id); !mark || stale {
				t.Errorf("after the re-check: check mark present=%v, vector stale=%v; want a checked block with a current vector", mark, stale)
			}
		})
	}
}
