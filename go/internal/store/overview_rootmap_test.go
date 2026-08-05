package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
)

// TestRootMapReads_RequireScopes is W-B gate 2 (design/02 §7 W-B): every root-map
// read fails CLOSED on an empty scope set — an error, never "all scopes" via
// PostgreSQL's `= ANY('{}')` artefact. It runs WITHOUT a database on purpose:
// RequireScopes must be the FIRST statement of each function, before any pool
// use, so a nil pool proves the guard cannot be reached through a query path.
//
// RED PROBE: drop the RequireScopes call from any of the three functions ⇒ that
// subtest panics on the nil pool instead of returning ErrNoScopes.
func TestRootMapReads_RequireScopes(t *testing.T) {
	ctx := context.Background()

	t.Run("OverviewTotals", func(t *testing.T) {
		if _, err := store.OverviewTotals(ctx, nil, nil, 2); !errors.Is(err, store.ErrNoScopes) {
			t.Fatalf("OverviewTotals(nil scopes) = %v, want ErrNoScopes", err)
		}
	})
	t.Run("OverviewMeta", func(t *testing.T) {
		if _, err := store.OverviewMeta(ctx, nil, []string{}); !errors.Is(err, store.ErrNoScopes) {
			t.Fatalf("OverviewMeta(empty scopes) = %v, want ErrNoScopes", err)
		}
	})
	t.Run("ActiveBlockCount", func(t *testing.T) {
		n, known, err := store.ActiveBlockCount(ctx, nil, nil, time.Second)
		if !errors.Is(err, store.ErrNoScopes) {
			t.Fatalf("ActiveBlockCount(nil scopes) err = %v, want ErrNoScopes", err)
		}
		if n != 0 || known {
			t.Fatalf("ActiveBlockCount(nil scopes) = (%d, %v), want (0, false)", n, known)
		}
	})
}
