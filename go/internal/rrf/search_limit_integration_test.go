//go:build integration

// Issue #40 Bug 1: the output limit of ctx_rrf must be CAPPED at the ceiling,
// never reset to the default. The query pipeline's aggregate over-fetch widens
// the internal limit to 400 (handler/query.go internalLimit=200 x
// query_fold.go overFetchFactor=2), which the former `> 200 => 5` reset turned
// into a five-row retrieval for every query in a scope holding one comment.
//
// The limits in the row-count gate are deliberately LITERALS, not the package
// constants: that test has to stay executable against a build that predates
// them, so its red state is an assertion failure and not a compile error.
//
//	go test -tags=integration ./internal/rrf/ -run TestSearchLimit -count=1 -v
package rrf_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/rrf"
	"github.com/GottZ/ctx/internal/testdb"
)

// TestSearchLimitOverFetchWindow is the A-W1 gate: a search asking for the
// over-fetch window must deliver the whole matching corpus, not the default
// five rows. The corpus is deliberately larger than the default so the collapse
// is visible as a row count, not as a ranking difference.
func TestSearchLimitOverFetchWindow(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	emb := t40bEmbedding()
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

	const (
		scope        = "aw1-limit"
		seeded       = 12
		overFetch    = 400 // the window handler/query.go hands to rrf.Search
		aboveCeiling = 501 // one past the ceiling: cap, never reset
		defaultLimit = 5
	)
	for i := 1; i <= seeded; i++ {
		t40bInsertBlock(t, pool, fmt.Sprintf("019f4a01-0000-7000-9000-0000000000%02d", i),
			scope, "knowledge", false, emb, now)
	}

	search := func(limit int) []rrf.SearchResult {
		t.Helper()
		res, _, err := rrf.Search(ctx, pool, emb, "zzqqxx", "zzqqxx",
			[]string{scope}, nil, nil, limit, "", "", testVisibleTypes, nil, nil, nil, nil, nil,
			rrf.SelectorPolicy{})
		if err != nil {
			t.Fatalf("rrf.Search(limit=%d): %v", limit, err)
		}
		return res
	}

	// Control inside the historic window: the corpus is fully retrievable, so a
	// short result at limit=400 below can only come from the clamp.
	if got := len(search(50)); got != seeded {
		t.Fatalf("limit=50 returned %d rows, want %d — corpus not fully retrievable, gate inconclusive", got, seeded)
	}

	// The gate: the over-fetch window the pipeline actually produces.
	got := search(overFetch)
	if len(got) <= defaultLimit {
		t.Fatalf("limit=%d returned %d rows, want > %d (limit collapsed to the default instead of being capped)",
			overFetch, len(got), defaultLimit)
	}
	if len(got) != seeded {
		t.Errorf("limit=%d returned %d rows, want the full corpus of %d", overFetch, len(got), seeded)
	}

	// Above the ceiling the request is capped, not reset: the corpus still
	// arrives complete.
	if n := len(search(aboveCeiling)); n != seeded {
		t.Errorf("limit=%d returned %d rows, want the full corpus of %d (cap, not reset)",
			aboveCeiling, n, seeded)
	}
}

// TestSearchLimitDecisionReportsSQLLimit is the log-truth half of the fix: the
// decision reports the limit ctx_rrf actually ran with, so a search log line
// explains its own result_count instead of showing the pre-clamp value.
func TestSearchLimitDecisionReportsSQLLimit(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	emb := t40bEmbedding()

	cases := []struct{ ask, want int }{
		{0, rrf.DefaultSearchLimit},
		{50, 50},
		{400, 400},
		{rrf.MaxSearchLimit + 1, rrf.MaxSearchLimit},
	}
	for _, c := range cases {
		_, dec, err := rrf.Search(ctx, pool, emb, "zzqqxx", "zzqqxx",
			[]string{"aw1-sqllimit"}, nil, nil, c.ask, "", "", testVisibleTypes, nil, nil, nil, nil, nil,
			rrf.SelectorPolicy{})
		if err != nil {
			t.Fatalf("rrf.Search(limit=%d): %v", c.ask, err)
		}
		if dec.SQLLimit != c.want {
			t.Errorf("Search(limit=%d): decision.SQLLimit = %d, want %d", c.ask, dec.SQLLimit, c.want)
		}
	}
}
