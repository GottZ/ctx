//go:build integration

// Integration test for block-workbench W7: keyset pagination over the
// empty-query browse path of SearchBlocks (ORDER BY updated_at DESC, id DESC).
//
// The corpus target is 1M+; a LIMIT-only ceiling (today ClampLimit 10..50)
// cannot page past the first window. W7 adds an opaque/structured cursor
// (updated_at, id) so the /blocks "Load more" button can fetch the NEXT page.
// updated_at is NOT unique, so id is the mandatory tiebreak — the keyset WHERE
// is the row-value comparison (updated_at, id) < (afterUpdated, afterId).
//
// This test SEEDS a corpus that includes a deliberate updated_at TIE pair
// (identical timestamp, different ids) straddling a page boundary, then walks
// the pages via the cursor and asserts the union is the full, gap-free,
// duplicate-free (updated_at DESC, id DESC) order — INCLUDING the correct tie
// resolution across the boundary.
//
// RED: SearchBlocks currently ignores its `after` cursor (W7 stub), so page 2
// comes back IDENTICAL to page 1 → duplicates + missing tail → this test fails
// until GREEN wires the keyset WHERE on the empty-query path.
//
// Run with:
//
//	go test -tags=integration ./internal/store/ -run TestSearchBlocksKeyset -count=1 -v
package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// seedKeysetBlock inserts one block with an EXPLICIT id and updated_at so the
// (updated_at DESC, id DESC) order — and the tie tiebreak — is fully
// deterministic. uuidv7 ids would correlate with insertion order; pinning the
// id lets the test choose exactly which row of a tie pair sorts first.
func seedKeysetBlock(t *testing.T, pool *pgxpool.Pool, id, scope, title string, updatedAt time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := pool.Exec(ctx,
		`INSERT INTO context_blocks (id, category, title, content, scope, created_at, updated_at)
		 VALUES ($1, 'learnings', $2, 'content of ' || $2, $3, $4, $4)`,
		id, title, scope, updatedAt); err != nil {
		t.Fatalf("seed block %s: %v", title, err)
	}
}

// keysetCursorOf returns the cursor pointing strictly AFTER bp — the
// {updated_at, id} of the last row of a page. (GREEN returns this on the wire
// as next_after; here the test constructs it from the last result to drive the
// next page, exactly as the client will.)
func keysetCursorOf(bp store.BlockPreview) *store.SearchCursor {
	return &store.SearchCursor{UpdatedAt: bp.UpdatedAt, ID: bp.ID}
}

func TestSearchBlocksKeyset_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	const scope = "private"
	base := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)

	// Six blocks, all in one scope. Times are distinct EXCEPT b-tie-a and
	// b-tie-b, which share tieTime and differ only by id — a real updated_at
	// tie. With pageSize 3 the tie straddles the page-1/page-2 boundary, so the
	// keyset WHERE must use id (not just updated_at) to avoid skipping or
	// repeating the second tie row.
	//
	// Expected full order is updated_at DESC, then id DESC within the tie. We
	// give b-tie-b the LEXICALLY-larger id, so b-tie-b precedes b-tie-a.
	tieTime := base.Add(-2 * time.Hour)
	seedKeysetBlock(t, pool, "00000000-0000-7000-8000-000000000001", scope, "newest-0", base)
	seedKeysetBlock(t, pool, "00000000-0000-7000-8000-000000000002", scope, "second-1", base.Add(-1*time.Hour))
	seedKeysetBlock(t, pool, "00000000-0000-7000-8000-0000000000bb", scope, "tie-b", tieTime)     // larger id → first of tie
	seedKeysetBlock(t, pool, "00000000-0000-7000-8000-0000000000aa", scope, "tie-a", tieTime)     // smaller id → second of tie
	seedKeysetBlock(t, pool, "00000000-0000-7000-8000-000000000005", scope, "fifth-4", base.Add(-3*time.Hour))
	seedKeysetBlock(t, pool, "00000000-0000-7000-8000-000000000006", scope, "sixth-5", base.Add(-4*time.Hour))

	// The full, authoritative order (updated_at DESC, id DESC) — what walking
	// the pages must reconstruct exactly.
	wantOrder := []string{
		"00000000-0000-7000-8000-000000000001", // newest-0
		"00000000-0000-7000-8000-000000000002", // second-1
		"00000000-0000-7000-8000-0000000000bb", // tie-b (larger id, tieTime)
		"00000000-0000-7000-8000-0000000000aa", // tie-a (smaller id, tieTime)
		"00000000-0000-7000-8000-000000000005", // fifth-4
		"00000000-0000-7000-8000-000000000006", // sixth-5
	}

	const pageSize = 3

	// --- Page 1: no cursor. Empty query → updated_at DESC, id DESC. ----------
	page1, err := store.SearchBlocks(ctx, pool, "", []string{scope}, "", nil, pageSize, true, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if len(page1) != pageSize {
		t.Fatalf("page 1 size: got %d, want %d", len(page1), pageSize)
	}

	// --- Page 2: cursor = last row of page 1. -------------------------------
	cursor := keysetCursorOf(page1[len(page1)-1])
	page2, err := store.SearchBlocks(ctx, pool, "", []string{scope}, "", nil, pageSize, true, cursor, nil, nil, nil)
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}

	// Walk the union and assert: gap-free, duplicate-free, and equal to the
	// full (updated_at DESC, id DESC) order — the core keyset guarantee.
	var got []string
	seen := map[string]bool{}
	for _, p := range append(append([]store.BlockPreview{}, page1...), page2...) {
		if seen[p.ID] {
			t.Fatalf("keyset returned a DUPLICATE id across pages: %s (page2 == page1 ⇒ cursor not applied)", p.ID)
		}
		seen[p.ID] = true
		got = append(got, p.ID)
	}

	if len(got) != len(wantOrder) {
		t.Fatalf("union size: got %d, want %d (a short union means the keyset SKIPPED rows / a long one means duplicates)\n got=%v\nwant=%v",
			len(got), len(wantOrder), got, wantOrder)
	}
	for i := range wantOrder {
		if got[i] != wantOrder[i] {
			t.Fatalf("order mismatch at index %d: got %s, want %s\n got=%v\nwant=%v\n(the tie pair tie-b before tie-a proves the id DESC tiebreak in the keyset WHERE)",
				i, got[i], wantOrder[i], got, wantOrder)
		}
	}

	// The page-1/page-2 boundary must fall INSIDE the tie pair: page 1 ends on
	// tie-b, page 2 begins on tie-a. This is the assertion that a updated_at-
	// only keyset (no id tiebreak) cannot satisfy — it would either re-emit or
	// drop tie-a at the boundary.
	if page1[len(page1)-1].ID != "00000000-0000-7000-8000-0000000000bb" {
		t.Fatalf("page 1 should end on the first tie row (tie-b); got last id %s", page1[len(page1)-1].ID)
	}
	if len(page2) == 0 || page2[0].ID != "00000000-0000-7000-8000-0000000000aa" {
		t.Fatalf("page 2 should begin on the second tie row (tie-a) — the id-tiebreak case; got %v", page2)
	}
}
