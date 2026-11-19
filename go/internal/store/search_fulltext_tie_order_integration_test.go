//go:build integration

// Issue #368: the full-text branch of the block search ordered by rank alone.
// Rows of the same rank came back in the order the scan read them — the
// physical placement of their row versions — and the cut at the limit among
// them was arbitrary: after an unrelated update the same query could return a
// different subset.
//
// The fixture writes rows of one rank through the store's write path and then
// updates some of them, so their newest row versions sit behind the others on
// the heap: physical order and id order disagree. Both full-text forms (the
// compact page subquery and the full statement) must answer the rank order,
// and among rows of one rank the id order, newest id first — the tie-break the
// browse order (updated_at DESC, id DESC) already has. The limited answer must
// be the prefix of the unlimited one.
//
//	go test -tags=integration -p 1 ./internal/store/ -run TestSearchFullTextTieOrder -count=1 -v
package store_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestSearchFullTextTieOrder(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	scopes := []string{"private"}

	// Two ranks: the term twice (3 rows) and once (12 rows). The titles carry
	// no query word, so the rank is the content's alone.
	var high, tied []string
	write := func(title, content string) string {
		t.Helper()
		b, err := store.UpsertBlock(ctx, pool, "tie-order", title, content, nil, nil, "private", true, store.SensitivityWrite{}, "knowledge")
		if err != nil {
			t.Fatalf("upsert %s: %v", title, err)
		}
		return b.ID
	}
	for i := 0; i < 12; i++ {
		tied = append(tied, write(fmt.Sprintf("Row %02d", i), "The sextant lies on the shelf."))
		if i%4 == 0 {
			high = append(high, write(fmt.Sprintf("Row %02d double", i), "The sextant lies beside a second sextant."))
		}
	}
	// New row versions for every other tied row: they land behind the rest
	// on the heap, so a scan no longer reads the tied rows in id order.
	note := "moved"
	for i, id := range tied {
		if i%2 == 0 {
			if _, _, err := store.UpdateBlock(ctx, pool, id, store.UpdateBlockData{Metadata: map[string]any{"note": note}}, scopes); err != nil {
				t.Fatalf("update %s: %v", id, err)
			}
		}
	}

	byIDDesc := func(ids []string) []string {
		out := slices.Clone(ids)
		slices.Sort(out)
		slices.Reverse(out)
		return out
	}
	want := append(byIDDesc(high), byIDDesc(tied)...)

	for _, compact := range []bool{true, false} {
		for _, limit := range []int{50, 7, 4} {
			t.Run(fmt.Sprintf("compact=%t/limit=%d", compact, limit), func(t *testing.T) {
				rows, err := store.SearchBlocks(ctx, pool, nil, "sextant", scopes, "", nil, limit, compact, nil, nil, nil, nil, nil)
				if err != nil {
					t.Fatalf("search: %v", err)
				}
				got := make([]string, len(rows))
				for i, r := range rows {
					got[i] = r.ID
				}
				w := want[:min(limit, len(want))]
				if !slices.Equal(got, w) {
					t.Errorf("answer order differs from (rank DESC, id DESC):\n got  %s\n want %s",
						strings.Join(got, " "), strings.Join(w, " "))
				}
			})
		}
	}
}
