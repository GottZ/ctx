//go:build integration

// Drift-census gold-id contract (design 05 §6.5, wave M-W3b): the census has
// to survive the gold set the planned campaign builds — 300 + 200 + 150
// single-label cases plus the multi-gold slices G-SESS/G-MH/G-GLOB, ~2 130
// ids (design 05 §4.5). The old cap of 2 000 turned that campaign into an
// error at its own instrument.
//
// What is pinned here:
//   - 2 130 ids go through and each addressed block comes back exactly once;
//   - the cap stays a HARD error, never a silent truncation — 1 000 000 ids
//     are refused before a single statement is sent (a variant that drops the
//     cap instead of chunking either runs for minutes or answers, both red
//     against the 30s context below);
//   - the chunk boundary is invisible from outside: 999/1000/1001/2000/2001
//     ids count correctly;
//   - the answer is ordered by id regardless of the request order, so merging
//     chunks reproduces the single statement's ORDER BY byte for byte. The
//     request in the ordering probe is DESCENDING on purpose: a plain
//     concatenation of per-chunk results would fail it.
//
// Run with:
//
//	go test -tags=integration ./internal/store/ -run Drift -count=1 -v
package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// driftGoldFixtureN is the size of the seeded gold corpus. It is the planned
// campaign's id count (design 05 §6.5) and therefore the number the old cap
// rejected.
const driftGoldFixtureN = 2130

// driftGoldID is the deterministic id of seeded fixture block i (1-based).
// The variant nibble 8 keeps it a well-formed uuid and the zero-padded hex
// suffix makes id order equal to i order, so an expectation can be written
// down instead of read back from the answer.
func driftGoldID(i int) string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012x", i)
}

// driftAbsentID is a well-formed id that is never seeded. The variant nibble
// 9 sorts every absent id AFTER every seeded one, so "absent" is a controlled
// position in the request, not an accident of ordering.
func driftAbsentID(i int) string {
	return fmt.Sprintf("00000000-0000-4000-9000-%012x", i)
}

// driftGoldStamp is fixture block i's created_at/updated_at. Fixed stamps make
// the marshalled answer byte-stable across runs, which is what the ordering
// probe's digest compares.
func driftGoldStamp(i int) time.Time {
	return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Second)
}

// seedDriftGoldCorpus inserts driftGoldFixtureN visible blocks in one
// statement, plus one archived and one out-of-scope block whose ids the
// caller may address to prove the census keeps filtering.
func seedDriftGoldCorpus(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO context_blocks (id, category, title, content, scope, type_name, created_at, updated_at)
		 SELECT ('00000000-0000-4000-8000-' || lpad(to_hex(g), 12, '0'))::uuid,
		        'driftgold', 'DG ' || g, 'drift gold fixture ' || g, 'private', 'knowledge',
		        timestamptz '2026-01-01 00:00:00+00' + (g || ' seconds')::interval,
		        timestamptz '2026-01-01 00:00:00+00' + (g || ' seconds')::interval
		   FROM generate_series(1, $1) g`, driftGoldFixtureN); err != nil {
		t.Fatalf("seed gold corpus: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO context_blocks (id, category, title, content, scope, type_name, is_archived)
		 VALUES ('00000000-0000-4000-a000-000000000001', 'driftgold', 'DG archived', 'archived', 'private', 'knowledge', true),
		        ('00000000-0000-4000-a000-000000000002', 'driftgold', 'DG foreign', 'foreign scope', 'work', 'knowledge', false)`); err != nil {
		t.Fatalf("seed filtered blocks: %v", err)
	}
}

func TestDriftCensusGoldIDCap_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	scopes := []string{"private"}
	types := []string{"knowledge"}

	seedDriftGoldCorpus(t, pool)

	t.Run("planned_campaign_size_passes", func(t *testing.T) {
		ids := make([]string, 0, driftGoldFixtureN)
		for i := 1; i <= driftGoldFixtureN; i++ {
			ids = append(ids, driftGoldID(i))
		}
		c, err := store.GetDriftCensus(ctx, pool, scopes, types, ids)
		if err != nil {
			t.Fatalf("census over %d ids: %v", len(ids), err)
		}
		if len(c.GoldIDs) != driftGoldFixtureN {
			t.Fatalf("census returned %d lifecycles, want %d", len(c.GoldIDs), driftGoldFixtureN)
		}
		seen := map[string]int{}
		for _, b := range c.GoldIDs {
			seen[b.ID]++
		}
		for i := 1; i <= driftGoldFixtureN; i++ {
			if seen[driftGoldID(i)] != 1 {
				t.Fatalf("id %s appeared %d times, want exactly 1", driftGoldID(i), seen[driftGoldID(i)])
			}
		}
	})

	t.Run("absent_ids_are_missing_not_invented", func(t *testing.T) {
		// Still driftGoldFixtureN ids in the request, but 130 of them name
		// nothing: the answer must be the 2 000 that exist, and the gap is
		// the finding (BlockLifecycle doc, drift.go:48-50).
		const absent = 130
		ids := make([]string, 0, driftGoldFixtureN)
		for i := 1; i <= driftGoldFixtureN-absent; i++ {
			ids = append(ids, driftGoldID(i))
		}
		for i := 1; i <= absent; i++ {
			ids = append(ids, driftAbsentID(i))
		}
		c, err := store.GetDriftCensus(ctx, pool, scopes, types, ids)
		if err != nil {
			t.Fatalf("census with absent ids: %v", err)
		}
		if len(c.GoldIDs) != driftGoldFixtureN-absent {
			t.Fatalf("census returned %d lifecycles, want %d", len(c.GoldIDs), driftGoldFixtureN-absent)
		}
		for _, b := range c.GoldIDs {
			if b.ID == driftAbsentID(1) {
				t.Fatalf("census invented absent id %s", b.ID)
			}
		}
	})

	t.Run("archived_and_foreign_scope_stay_filtered", func(t *testing.T) {
		ids := []string{
			"00000000-0000-4000-a000-000000000001", // archived
			"00000000-0000-4000-a000-000000000002", // scope=work
			driftGoldID(1),
		}
		c, err := store.GetDriftCensus(ctx, pool, scopes, types, ids)
		if err != nil {
			t.Fatalf("census over filtered ids: %v", err)
		}
		if len(c.GoldIDs) != 1 || c.GoldIDs[0].ID != driftGoldID(1) {
			t.Fatalf("census returned %+v, want only %s", c.GoldIDs, driftGoldID(1))
		}
	})

	t.Run("order_is_by_id_regardless_of_request_order", func(t *testing.T) {
		// 2 000 ids = the OLD cap, so this case runs on the pre-change tree
		// too and is the vorher/nachher comparison of gate 6. Requested
		// DESCENDING: concatenating per-chunk results without a merge sort
		// would answer [1001..2000, 1..1000] and fail here.
		const n = 2000
		ids := make([]string, 0, n)
		for i := n; i >= 1; i-- {
			ids = append(ids, driftGoldID(i))
		}
		c, err := store.GetDriftCensus(ctx, pool, scopes, types, ids)
		if err != nil {
			t.Fatalf("census over %d descending ids: %v", len(ids), err)
		}
		if len(c.GoldIDs) != n {
			t.Fatalf("census returned %d lifecycles, want %d", len(c.GoldIDs), n)
		}
		for i, b := range c.GoldIDs {
			want := driftGoldID(i + 1)
			if b.ID != want {
				t.Fatalf("position %d is %s, want %s (answer must be ordered by id)", i, b.ID, want)
			}
			if !b.CreatedAt.Equal(driftGoldStamp(i+1)) || !b.UpdatedAt.Equal(driftGoldStamp(i+1)) {
				t.Fatalf("id %s carries stamps %s/%s, want %s", b.ID, b.CreatedAt, b.UpdatedAt, driftGoldStamp(i+1))
			}
		}
		raw, err := json.Marshal(c.GoldIDs)
		if err != nil {
			t.Fatalf("marshal gold ids: %v", err)
		}
		sum := sha256.Sum256(raw)
		t.Logf("gold_ids digest over %d ids: sha256=%s bytes=%d", n, hex.EncodeToString(sum[:]), len(raw))
	})

	t.Run("chunk_boundaries", func(t *testing.T) {
		for _, n := range []int{999, 1000, 1001, 2000, 2001} {
			t.Run(fmt.Sprintf("n_%d", n), func(t *testing.T) {
				ids := make([]string, 0, n)
				for i := 1; i <= n; i++ {
					ids = append(ids, driftGoldID(i))
				}
				c, err := store.GetDriftCensus(ctx, pool, scopes, types, ids)
				if err != nil {
					t.Fatalf("census over %d ids: %v", n, err)
				}
				if len(c.GoldIDs) != n {
					t.Fatalf("census over %d ids returned %d lifecycles, want %d", n, len(c.GoldIDs), n)
				}
				if c.GoldIDs[0].ID != driftGoldID(1) || c.GoldIDs[n-1].ID != driftGoldID(n) {
					t.Fatalf("census over %d ids spans %s..%s, want %s..%s",
						n, c.GoldIDs[0].ID, c.GoldIDs[n-1].ID, driftGoldID(1), driftGoldID(n))
				}
			})
		}
	})

	t.Run("repeated_id_stays_one_row", func(t *testing.T) {
		// `id = ANY(array)` deduped a repeated id; chunking must not turn the
		// repetition into a second lifecycle row.
		ids := []string{driftGoldID(7), driftGoldID(7), driftGoldID(7)}
		c, err := store.GetDriftCensus(ctx, pool, scopes, types, ids)
		if err != nil {
			t.Fatalf("census over repeated id: %v", err)
		}
		if len(c.GoldIDs) != 1 {
			t.Fatalf("census returned %d lifecycles for a thrice-named id, want 1", len(c.GoldIDs))
		}
	})

	t.Run("cap_refuses_a_million_ids_without_touching_the_db", func(t *testing.T) {
		// Negative probe 2 (briefing gate 4): a variant that DROPS the cap
		// instead of chunking sends a million-element array downstream — it
		// either exceeds this deadline or answers, and both fail the
		// assertion below.
		tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		const n = 1000000
		ids := make([]string, 0, n)
		for i := 1; i <= n; i++ {
			ids = append(ids, driftAbsentID(i))
		}
		start := time.Now()
		_, err := store.GetDriftCensus(tctx, pool, scopes, types, ids)
		if err == nil {
			t.Fatalf("census over %d ids returned no error — the cap is gone", n)
		}
		want := fmt.Sprintf("%d ids requested, cap is 10000", n)
		if got := err.Error(); got != "store: drift census: "+want {
			t.Fatalf("census over %d ids failed with %q, want %q", n, got, "store: drift census: "+want)
		}
		if el := time.Since(start); el > 5*time.Second {
			t.Fatalf("cap refusal took %s — it is not refusing before the statement", el)
		}
	})

	t.Run("cap_still_refuses_above_10000", func(t *testing.T) {
		ids := make([]string, 0, 10001)
		for i := 1; i <= 10001; i++ {
			ids = append(ids, driftAbsentID(i))
		}
		_, err := store.GetDriftCensus(ctx, pool, scopes, types, ids)
		if err == nil {
			t.Fatal("census over 10001 ids returned no error, want the cap error")
		}
		if got, want := err.Error(), "store: drift census: 10001 ids requested, cap is 10000"; got != want {
			t.Fatalf("census over 10001 ids failed with %q, want %q", got, want)
		}
	})
}
