//go:build integration

// Issue #555, review round: what the request key's gate costs the writers of
// one key — a measurement, not a gate (opt-in):
//
//	CTX_P555_TPUT=1 go test -tags=integration -p 1 ./internal/store/ -run TestRequestKeyGateThroughput -v
//
// N writers of ONE key (CTX_P555_TPUT_WRITERS, default "8,32,64"), each
// iteration a request as production runs it: the key's authentication
// (ctx_auth_by_id, which writes the key row's last_used_at) and then one write
// — a single store, or with CTX_P555_TPUT_BUDGET=1 a one-item batch booked on
// the write budget (its booking inserts into the access log, whose foreign key
// takes the key row's key share). Per window (CTX_P555_TPUT_SECS, default 5)
// it prints the writes per second and the MultiXacts the window created
// (pg_control_checkpoint after a CHECKPOINT). The same file runs on an older
// tree with probeCtx returning the plain context (the base holds nothing) or
// the older WithRequestKey form, so base and head alternate as whole runs.
package store_test

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/store"
)

// probeCtx is the context a request of ar writes with on this tree.
func probeCtx(ctx context.Context, ar *auth.AuthResult) context.Context {
	return requestCtx(ctx, ar)
}

func TestRequestKeyGateThroughput(t *testing.T) {
	if os.Getenv("CTX_P555_TPUT") != "1" {
		t.Skip("measurement: set CTX_P555_TPUT=1")
	}
	writerSets := []int{8, 32, 64}
	if v := os.Getenv("CTX_P555_TPUT_WRITERS"); v != "" {
		writerSets = nil
		for _, s := range strings.Split(v, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(s))
			if err != nil || n < 1 {
				t.Fatalf("CTX_P555_TPUT_WRITERS %q", v)
			}
			writerSets = append(writerSets, n)
		}
	}
	secs := envInt("CTX_P555_TPUT_SECS", 5)
	budgeted := os.Getenv("CTX_P555_TPUT_BUDGET") == "1"
	f := newP547Fixture(t)
	ctx := context.Background()
	_, scopes, raw := pruneWritersTenant(t, f, "tput", 1)
	ar, err := auth.Authenticate(ctx, f.pool, raw)
	if err != nil || !ar.IsValid {
		t.Fatal(err)
	}
	wctx := probeCtx(ctx, ar)
	pool := zzGatePool(t, f.dsn, 72)
	for _, writers := range writerSets {
		before := nextMultiXact(t, f.pool)
		deadline := time.Now().Add(time.Duration(secs) * time.Second)
		var n, failed atomic.Int64
		var first atomic.Value
		var wg sync.WaitGroup
		start := time.Now()
		for w := range writers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; time.Now().Before(deadline); i++ {
					var ok bool
					if err := pool.QueryRow(ctx, `SELECT is_valid FROM ctx_auth_by_id($1::uuid)`, ar.ApiKeyID).Scan(&ok); err != nil || !ok {
						failed.Add(1)
						first.CompareAndSwap(nil, fmt.Sprintf("auth: %v %v", ok, err))
						continue
					}
					title := fmt.Sprintf("tput %d %d %d", writers, w, i)
					if budgeted {
						_, err = store.UpsertBlocks(wctx, pool, []store.BatchUpsert{{Category: "learnings", Title: title, Content: "c " + title, Scope: scopes[0], ScopeExplicit: true}},
							ar.ApiKeyID, f.set, &store.WriteBudget{APIKeyID: ar.ApiKeyID, Limit: 1 << 30})
					} else {
						_, err = store.UpsertBlock(wctx, pool, "learnings", title, "c "+title, nil, nil, scopes[0], true, store.SensitivityWrite{}, "")
					}
					if err != nil {
						failed.Add(1)
						first.CompareAndSwap(nil, err.Error())
						continue
					}
					n.Add(1)
				}
			}()
		}
		wg.Wait()
		el := time.Since(start)
		mx := nextMultiXact(t, f.pool) - before
		if failed.Load() > 0 {
			t.Errorf("%d writers: %d failed writes, first: %v", writers, failed.Load(), first.Load())
		}
		t.Logf("TPUT writers=%d budget=%v secs=%d writes=%d writes_per_s=%.0f multixacts=%d",
			writers, budgeted, secs, n.Load(), float64(n.Load())/el.Seconds(), mx)
	}
}
