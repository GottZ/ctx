//go:build integration

// Issue #555, point 1: what the key share costs the most frequent write, a
// single store. Opt-in (a measurement, not a gate):
//
//	CTX_P555_COST=1 go test -tags=integration -p 1 ./internal/store/ -run TestRequestKeyHoldCost -v
//
// Eight writers of one key store into one scope for a fixed time, alternating
// with and without the key in their context (the hold is the only
// difference: the same UpsertBlock, the same pool); then the hold statement's
// own buffers and the WAL a write produces with and without it.
package store_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/store"
)

func TestRequestKeyHoldCost(t *testing.T) {
	if os.Getenv("CTX_P555_COST") == "" {
		t.Skip("measurement: set CTX_P555_COST=1")
	}
	f := newP547Fixture(t)
	ctx := context.Background()
	_, scopes, key := pruneWritersTenant(t, f, "cost", 1)
	ar, err := auth.Authenticate(ctx, f.pool, key)
	if err != nil || !ar.IsValid {
		t.Fatal(err)
	}
	keyed := store.WithRequestKey(ctx, ar.ApiKeyID)
	const writers = 8
	run := func(label string, c context.Context, d time.Duration, round int) float64 {
		var n atomic.Int64
		stop := time.Now().Add(d)
		var wg sync.WaitGroup
		for w := range writers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; time.Now().Before(stop); i++ {
					if _, err := store.UpsertBlock(c, f.pool, "learnings", fmt.Sprintf("cost %s %d %d %d", label, round, w, i), "a single store's block", nil, nil,
						scopes[0], true, store.SensitivityWrite{}, ""); err != nil {
						t.Errorf("%s: %v", label, err)
						return
					}
					n.Add(1)
				}
			}()
		}
		wg.Wait()
		return float64(n.Load()) / d.Seconds()
	}
	run("warm", ctx, 2*time.Second, 0)
	var with, without []string
	for round := range 4 {
		without = append(without, fmt.Sprintf("%.0f", run("without", ctx, 5*time.Second, round)))
		with = append(with, fmt.Sprintf("%.0f", run("with", keyed, 5*time.Second, round)))
	}
	t.Logf("8 parallel single stores of one key into one scope, writes per second, alternating 5 s runs: without the key share %s; with it %s",
		strings.Join(without, ", "), strings.Join(with, ", "))

	var plan []string
	rows, err := f.pool.Query(ctx, `EXPLAIN (ANALYZE, BUFFERS, COSTS OFF, TIMING OFF) SELECT 1 FROM context_api_keys WHERE id = $1::uuid FOR KEY SHARE`, ar.ApiKeyID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, l)
	}
	rows.Close()
	t.Logf("the hold statement:\n%s", strings.Join(plan, "\n"))

	wal := func(label string, c context.Context) float64 {
		const n = 400
		var start, end string
		if err := f.pool.QueryRow(ctx, `SELECT pg_current_wal_lsn()::text`).Scan(&start); err != nil {
			t.Fatal(err)
		}
		for i := range n {
			if _, err := store.UpsertBlock(c, f.pool, "learnings", fmt.Sprintf("wal %s %d", label, i), "a single store's block", nil, nil,
				scopes[0], true, store.SensitivityWrite{}, ""); err != nil {
				t.Fatal(err)
			}
		}
		if err := f.pool.QueryRow(ctx, `SELECT pg_current_wal_lsn()::text`).Scan(&end); err != nil {
			t.Fatal(err)
		}
		var bytes float64
		if err := f.pool.QueryRow(ctx, `SELECT pg_wal_lsn_diff($1::pg_lsn, $2::pg_lsn)`, end, start).Scan(&bytes); err != nil {
			t.Fatal(err)
		}
		return bytes / n
	}
	for round := range 2 {
		t.Logf("WAL per single store (one writer, 400 writes), round %d: without the key share %.0f B, with it %.0f B",
			round, wal(fmt.Sprintf("without %d", round), ctx), wal(fmt.Sprintf("with %d", round), keyed))
	}
}
