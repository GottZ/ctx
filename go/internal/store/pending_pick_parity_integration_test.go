//go:build integration

package store_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// pendingPickReference is the pick as one statement over context_blocks, in
// the shape both backfill picks had before issue #239: the pending predicate
// (since issue #198 part B unembedded or stale, store.EmbedPendingPredicate),
// the failure-memo and type predicates, the classes as a filter, ORDER BY and
// LIMIT 1. It is the reference store.PendingPickSQL is held against.
func pendingPickReference(order, failurePredicate string) string {
	return `SELECT id FROM context_blocks
		WHERE ` + store.EmbedPendingPredicate +
		failurePredicate + store.RetrievalExcludedTypePredicate + `
		  AND ((sensitivity = ANY($1::text[]) AND scope <> ALL($2::text[]))
		    OR (scope, sensitivity) IN (SELECT * FROM unnest($3::text[], $4::text[])))
		  AND id <> ALL($5::uuid[])
		ORDER BY ` + order + `
		LIMIT 1`
}

// TestPendingPickParity holds store.PendingPickSQL against the reference statement:
// over a fixture of every kind of row the pick has to tell apart, both
// statements drain the same block sequence, for both orders, both
// failure-memo predicates, and bound and unbound class sets.
func TestPendingPickParity(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// Six scopes, the four sensitivities, three blocks per pair; one in
	// three of them shares its created_at with a neighbour (one transaction).
	// On top: archived, embedded, excluded-type, and memoized rows, and stale
	// ones: embedded and stale (pending again), archived and stale (not
	// pending), stale without a vector (in both sets, picked once), stale with
	// a memo.
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_blocks (category, title, content, scope, sensitivity, created_at, updated_at)
		SELECT 'learnings', 'pp ' || s || ' ' || l || ' ' || i, 'c', s, l,
		       timestamptz '2026-01-01' + make_interval(secs => abs(hashtext(s || l || i)) % 60 / 3 * 3), now()
		  FROM unnest(ARRAY['pp-a','pp-b','pp-own','pp-lifted','pp-mild','pp-other']) s,
		       unnest(ARRAY['public','internal','personal','credentials']) l,
		       generate_series(1, 3) i`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	for _, st := range []string{
		`UPDATE context_blocks SET is_archived = true WHERE title LIKE 'pp % 1' AND scope IN ('pp-a', 'pp-own')`,
		`UPDATE context_blocks SET embedding = array_fill(0.1::real, ARRAY[1024])::vector, embed_model = 'test'
		  WHERE title LIKE 'pp % 2' AND scope IN ('pp-b', 'pp-mild')`,
		`UPDATE context_blocks SET type_name = 'checkpoint' WHERE title LIKE 'pp % 3' AND scope = 'pp-a'`,
		// A memo inside its backoff, one whose backoff ran out, a
		// caller_timeout memo, and a parked oversize memo.
		`INSERT INTO context_embed_failures (block_id, migration_id, attempts, last_error, last_class, next_attempt_at)
		 SELECT id, NULL, 1, 'e',
		        (ARRAY['wire','wire','caller_timeout','oversize'])[1 + (row_number() OVER (ORDER BY id))::int % 4],
		        (ARRAY[now() + interval '1 hour', now() - interval '1 hour', now() - interval '1 hour', 'infinity'::timestamptz])[1 + (row_number() OVER (ORDER BY id))::int % 4]
		   FROM context_blocks WHERE title LIKE 'pp % 3' AND scope IN ('pp-b', 'pp-own', 'pp-other')`,
		`UPDATE context_blocks SET embedding_stale = true WHERE title LIKE 'pp % 2' AND scope = 'pp-mild'`,
		`UPDATE context_blocks SET embedding_stale = true WHERE title LIKE 'pp % 1' AND scope = 'pp-own'`,
		`UPDATE context_blocks SET embedding_stale = true WHERE title LIKE 'pp % 2' AND scope = 'pp-a'`,
		`UPDATE context_blocks SET embedding_stale = true WHERE title LIKE 'pp % 3' AND scope = 'pp-b'`,
	} {
		if _, err := pool.Exec(ctx, st); err != nil {
			t.Fatalf("%s: %v", st, err)
		}
	}

	unbound := store.PendingClasses{
		Shared:     []string{"public", "internal", "personal"},
		Dead:       []string{"pp-lifted"},
		PairScopes: []string{"pp-own"},
		PairLevels: []string{"credentials"},
	}
	bound := store.NewPendingClasses()
	for _, s := range []string{"pp-a", "pp-own", "pp-mild"} {
		levels := []string{"public", "internal", "personal"}
		if s == "pp-own" {
			levels = append(levels, "credentials")
		}
		for _, l := range levels {
			bound.PairScopes = append(bound.PairScopes, s)
			bound.PairLevels = append(bound.PairLevels, l)
		}
	}
	queryPath := store.EmbedFailureQueryPathExcludedPredicate
	arm := store.EmbedFailureExcludedPredicate
	for _, v := range []struct {
		name             string
		order, predicate string
		classes          store.PendingClasses
	}{
		{"caller_newest_first", store.PendingNewestFirst, queryPath, bound},
		{"old_end_oldest_first", store.PendingOldestFirst, queryPath, unbound},
		{"arm_owned", store.PendingOldestFirst, arm, bound},
		{"arm_unbounded", store.PendingOldestFirst, arm, unbound},
		{"unbound_newest_first", store.PendingNewestFirst, arm, unbound},
		{"nothing_servable", store.PendingOldestFirst, arm, store.NewPendingClasses()},
	} {
		t.Run(v.name, func(t *testing.T) {
			pick := func(sql string, skip []string) (string, error) {
				var id string
				err := pool.QueryRow(ctx, sql, append(v.classes.Args(), skip)...).Scan(&id)
				return id, err
			}
			newSQL := store.PendingPickSQL("id", v.order == store.PendingNewestFirst, v.predicate)
			refSQL := pendingPickReference(v.order, v.predicate)
			skip := []string{}
			for {
				got, gerr := pick(newSQL, skip)
				want, werr := pick(refSQL, skip)
				if errors.Is(gerr, pgx.ErrNoRows) && errors.Is(werr, pgx.ErrNoRows) {
					break
				}
				if gerr != nil || werr != nil || got != want {
					t.Fatalf("pick %d: store.PendingPickSQL %q (%v), reference %q (%v)", len(skip)+1, got, gerr, want, werr)
				}
				if slices.Contains(skip, got) {
					t.Fatalf("pick %d returned %q twice", len(skip)+1, got)
				}
				skip = append(skip, got)
			}
			if v.name != "nothing_servable" && len(skip) < 10 {
				t.Fatalf("drained %d blocks; the fixture is meant to give this variant more", len(skip))
			}
			t.Logf("%d blocks in the same order", len(skip))
		})
	}
}
