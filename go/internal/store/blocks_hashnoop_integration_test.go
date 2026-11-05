//go:build integration

// T02-12 (design/02 Naht 8, E02-6 = A): the integration gates for
// store.HashNOOPCheck — the dedup chokepoint of THREE write surfaces
// (handler/context_store.go:105, handler/ingest.go:234, handler/mcp.go:544).
// Since issue #64 the REST surface asks store.IdenticalContentMatch, which
// shares the identity predicate (identicalContentWhere) these gates pin.
// Until this file the function had zero integration coverage; its only written
// trace was a t.Skip skeleton naming four cases (blocks_test.go:156-162), which
// this file replaces with the real thing.
//
// Fixtures deliberately go through the PRODUCTION write path (store.UpsertBlock
// — the very call all three callers run right after the check) and the
// production archive verb (store.DeleteBlock). A hand-written INSERT is not an
// option and would not be an honest one either: content_hash is a GENERATED
// ALWAYS column (migrations/113_baseline.sql:71, doc at blocks.go:521), so
// seeding it by hand would plant the exact assumption the subject relies on.
// Gate 5 pins that seam explicitly.
//
// Run with:
//
//	go test -tags=integration -p 1 -count=1 -run 'HashNOOP' ./internal/store/
package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

const (
	t0212Scope    = "private"
	t0212Category = "learnings"
	t0212Title    = "t0212 hash noop subject"
	t0212Content  = "t0212 canonical fixture content for the dedup chokepoint"
)

// t0212Seed writes one block through the production write path and returns its
// id. store.UpsertBlock is what context_store.go:131, ingest.go:263 and
// mcp.go:615 run right after their HashNOOPCheck call, so fixture and subject
// share one and the same notion of block identity — (category, title, scope)
// against the partial unique index uq_context_category_title_scope.
func t0212Seed(t *testing.T, pool *pgxpool.Pool, category, title, content, scope string) string {
	t.Helper()
	b, err := store.UpsertBlock(context.Background(), pool, category, title, content,
		nil, nil, scope, false, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("seed via store.UpsertBlock(%q/%q/%q): %v", category, title, scope, err)
	}
	if b == nil || b.ID == "" {
		t.Fatalf("seed via store.UpsertBlock(%q/%q/%q): got nil/empty block, want a written row", category, title, scope)
	}
	return b.ID
}

// TestHashNOOPCheckIntegration runs the four cases the retired skeleton named,
// plus a fifth that keeps case 1 from being vacuous.
func TestHashNOOPCheckIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	subjectID := t0212Seed(t, pool, t0212Category, t0212Title, t0212Content, t0212Scope)

	t.Run("gate1_identical_content_returns_existing_id", func(t *testing.T) {
		got, err := store.HashNOOPCheck(ctx, pool, t0212Content, t0212Scope, t0212Category, t0212Title)
		if err != nil {
			t.Fatalf("HashNOOPCheck: %v", err)
		}
		if got != subjectID {
			t.Errorf("HashNOOPCheck(identical content) = %q, want %q — the chokepoint missed a byte-identical rewrite, so every one of the three write surfaces would store a duplicate instead of answering NOOP", got, subjectID)
		}
	})

	t.Run("gate2_different_content_returns_empty", func(t *testing.T) {
		got, err := store.HashNOOPCheck(ctx, pool, t0212Content+" plus one trailing edit", t0212Scope, t0212Category, t0212Title)
		if err != nil {
			t.Fatalf("HashNOOPCheck: %v", err)
		}
		if got != "" {
			t.Errorf("HashNOOPCheck(changed content, same identity) = %q, want \"\" — a real edit was swallowed as a NOOP and would never reach the store", got)
		}
	})

	t.Run("gate3_archived_block_is_not_returned", func(t *testing.T) {
		const (
			title   = "t0212 archived subject"
			content = "t0212 fixture content that is archived after a positive control"
		)
		id := t0212Seed(t, pool, t0212Category, title, content, t0212Scope)

		// Positive control first: while the row is live the identical call has
		// to find it. Without it the post-archive assertion below could pass
		// for the wrong reason (nothing ever matched).
		ctrl, err := store.HashNOOPCheck(ctx, pool, content, t0212Scope, t0212Category, title)
		if err != nil {
			t.Fatalf("HashNOOPCheck (pre-archive control): %v", err)
		}
		if ctrl != id {
			t.Fatalf("control broken: HashNOOPCheck before archiving = %q, want %q", ctrl, id)
		}

		// Archive through the production verb, not a hand UPDATE: the archive
		// semantics under test are the ones the write surfaces produce.
		// DeleteBlock answers a miss with (nil, nil), so nil has to be caught.
		archived, err := store.DeleteBlock(ctx, pool, id, []string{t0212Scope})
		if err != nil {
			t.Fatalf("store.DeleteBlock: %v", err)
		}
		if archived == nil {
			t.Fatalf("store.DeleteBlock(%s) returned (nil, nil) — the row was not archived, so the assertion below would not be about archiving at all", id)
		}

		got, err := store.HashNOOPCheck(ctx, pool, content, t0212Scope, t0212Category, title)
		if err != nil {
			t.Fatalf("HashNOOPCheck (post-archive): %v", err)
		}
		if got != "" {
			t.Errorf("HashNOOPCheck(archived block) = %q, want \"\" — the ON CONFLICT target is partial on NOT is_archived, so an archived row that still answers the check would make its own re-creation impossible: the caller reports \"identical content already exists\" for a row it can no longer reach", got)
		}
	})

	t.Run("gate4_wrong_identity_returns_empty", func(t *testing.T) {
		cases := []struct {
			name     string
			scope    string
			category string
			title    string
		}{
			{"wrong_scope", "shared", t0212Category, t0212Title},
			{"wrong_category", t0212Scope, "decisions", t0212Title},
			{"wrong_title", t0212Scope, t0212Category, t0212Title + " (a different block)"},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				got, err := store.HashNOOPCheck(ctx, pool, t0212Content, c.scope, c.category, c.title)
				if err != nil {
					t.Fatalf("HashNOOPCheck: %v", err)
				}
				if got != "" {
					t.Errorf("HashNOOPCheck(%s, identical content) = %q, want \"\" — identical CONTENT under a different identity is a different block; collapsing the two drops a legitimate write and, across scopes, leaks the existence of a foreign row", c.name, got)
				}
			})
		}
	})

	t.Run("gate5_content_hash_comes_from_the_write_path", func(t *testing.T) {
		// Nothing in this file writes content_hash. It is GENERATED ALWAYS AS
		// (encode(digest(content,'sha256'),'hex')) STORED
		// (migrations/113_baseline.sql:71) and store.UpsertBlock's column list
		// (blocks.go:583) does not carry it. Gate 1 would therefore be
		// trivially green if the column stayed empty on both sides of the
		// comparison — pin the value against an INDEPENDENT hash: pgcrypto
		// digest() on the stored side, crypto/sha256 on this one.
		var stored string
		if err := pool.QueryRow(ctx,
			`SELECT content_hash FROM context_blocks WHERE id = $1::uuid`, subjectID,
		).Scan(&stored); err != nil {
			t.Fatalf("read content_hash of %s: %v", subjectID, err)
		}
		if stored == "" {
			t.Fatalf("content_hash of the seeded row is empty — the write path produced no hash, which would make gate 1 vacuous")
		}
		sum := sha256.Sum256([]byte(t0212Content))
		if want := hex.EncodeToString(sum[:]); stored != want {
			t.Errorf("content_hash = %q, want %q (sha256 over the content, computed in Go) — the column the chokepoint matches on is not the sha256 of what the production path stored", stored, want)
		}
	})
}
