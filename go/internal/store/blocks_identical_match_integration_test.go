//go:build integration

// T02-12 (design/02 Naht 8, E02-6 = A): the integration gates for
// store.IdenticalContentMatch — the dedup chokepoint of the REST, MCP and
// ingest write surfaces. These are the five gates formerly applied to
// store.HashNOOPCheck, restored against its replacement so the shared
// identicalContentWhere predicate does not lose its direct store coverage.
//
// Fixtures deliberately go through the PRODUCTION write path (store.UpsertBlock
// — the call block-mode writers run right after the check) and the production
// archive verb (store.DeleteBlock). A hand-written INSERT is not an option and
// would not be an honest one either: content_hash is a GENERATED ALWAYS column
// (migrations/113_baseline.sql), so seeding it by hand would plant the exact
// assumption the subject relies on. Gate 5 pins that seam explicitly.
//
// Run with:
//
//	go test -tags=integration -p 1 -count=1 -run 'IdenticalContentMatch' ./internal/store/
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
	identicalMatchScope    = "private"
	identicalMatchCategory = "learnings"
	identicalMatchTitle    = "t0212 identical content match subject"
	identicalMatchContent  = "t0212 canonical fixture content for the dedup chokepoint"
)

// identicalMatchSeed writes one block through the production write path and
// returns its id, so fixture and subject share the same block identity:
// (category, title, scope) against uq_context_category_title_scope.
func identicalMatchSeed(t *testing.T, pool *pgxpool.Pool, category, title, content, scope string) string {
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

func matchIdenticalContent(t *testing.T, ctx context.Context, pool *pgxpool.Pool, content, scope, category, title string) store.IdenticalContent {
	t.Helper()
	match, err := store.IdenticalContentMatch(ctx, pool, content, scope, category, title,
		nil, nil, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("IdenticalContentMatch: %v", err)
	}
	return match
}

// TestIdenticalContentMatchIntegration runs the four identity and lifecycle
// cases the retired HashNOOPCheck skeleton named, plus a fifth that keeps the
// identical-content case from being vacuous.
func TestIdenticalContentMatchIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	subjectID := identicalMatchSeed(t, pool, identicalMatchCategory, identicalMatchTitle, identicalMatchContent, identicalMatchScope)

	t.Run("gate1_identical_content_matches_existing_id", func(t *testing.T) {
		got := matchIdenticalContent(t, ctx, pool, identicalMatchContent, identicalMatchScope, identicalMatchCategory, identicalMatchTitle)
		if got.ID != subjectID || !got.Unchanged() {
			t.Errorf("IdenticalContentMatch(identical content) = %+v, want id %q and Unchanged — the chokepoint missed a byte-identical rewrite", got, subjectID)
		}
	})

	t.Run("gate2_different_content_does_not_match", func(t *testing.T) {
		got := matchIdenticalContent(t, ctx, pool, identicalMatchContent+" plus one trailing edit", identicalMatchScope, identicalMatchCategory, identicalMatchTitle)
		if got.ID != "" {
			t.Errorf("IdenticalContentMatch(changed content, same identity) = %+v, want no match — a real edit would be swallowed as a NOOP", got)
		}
	})

	t.Run("gate3_archived_block_does_not_match", func(t *testing.T) {
		const (
			title   = "t0212 archived identical match subject"
			content = "t0212 fixture content that is archived after a positive control"
		)
		id := identicalMatchSeed(t, pool, identicalMatchCategory, title, content, identicalMatchScope)

		// Positive control first: without it the post-archive assertion could
		// pass for the wrong reason (nothing ever matched).
		ctrl := matchIdenticalContent(t, ctx, pool, content, identicalMatchScope, identicalMatchCategory, title)
		if ctrl.ID != id || !ctrl.Unchanged() {
			t.Fatalf("control broken: IdenticalContentMatch before archiving = %+v, want id %q and Unchanged", ctrl, id)
		}

		// Archive through the production verb, not a hand UPDATE. DeleteBlock
		// answers a miss with (nil, nil), so nil has to be caught.
		archived, err := store.DeleteBlock(ctx, pool, id, []string{identicalMatchScope})
		if err != nil {
			t.Fatalf("store.DeleteBlock: %v", err)
		}
		if archived == nil {
			t.Fatalf("store.DeleteBlock(%s) returned (nil, nil) — the row was not archived", id)
		}

		got := matchIdenticalContent(t, ctx, pool, content, identicalMatchScope, identicalMatchCategory, title)
		if got.ID != "" {
			t.Errorf("IdenticalContentMatch(archived block) = %+v, want no match — an archived row must not block its own re-creation", got)
		}
	})

	t.Run("gate4_wrong_identity_does_not_match", func(t *testing.T) {
		cases := []struct {
			name     string
			scope    string
			category string
			title    string
		}{
			{"wrong_scope", "shared", identicalMatchCategory, identicalMatchTitle},
			{"wrong_category", identicalMatchScope, "decisions", identicalMatchTitle},
			{"wrong_title", identicalMatchScope, identicalMatchCategory, identicalMatchTitle + " (a different block)"},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				got := matchIdenticalContent(t, ctx, pool, identicalMatchContent, c.scope, c.category, c.title)
				if got.ID != "" {
					t.Errorf("IdenticalContentMatch(%s, identical content) = %+v, want no match — identical content under another identity is a different block", c.name, got)
				}
			})
		}
	})

	t.Run("gate5_content_hash_comes_from_the_write_path", func(t *testing.T) {
		// Nothing in this file writes content_hash. It is GENERATED ALWAYS AS
		// (encode(digest(content,'sha256'),'hex')) STORED, and UpsertBlock's
		// column list does not carry it. Gate 1 would therefore be trivially
		// green if the column stayed empty on both sides of the comparison.
		// Pin it against an INDEPENDENT hash computed in Go.
		var stored string
		if err := pool.QueryRow(ctx,
			`SELECT content_hash FROM context_blocks WHERE id = $1::uuid`, subjectID,
		).Scan(&stored); err != nil {
			t.Fatalf("read content_hash of %s: %v", subjectID, err)
		}
		if stored == "" {
			t.Fatalf("content_hash of the seeded row is empty — the write path produced no hash, which would make gate 1 vacuous")
		}
		sum := sha256.Sum256([]byte(identicalMatchContent))
		if want := hex.EncodeToString(sum[:]); stored != want {
			t.Errorf("content_hash = %q, want %q (sha256 over the content, computed in Go)", stored, want)
		}
	})
}
