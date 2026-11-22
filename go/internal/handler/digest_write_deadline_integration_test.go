//go:build integration

package handler

// Issue #505: POST /api/digest answers a digest that runs longer than the
// server's WriteTimeout — the CLI waits for it now (digestCallBudget), and
// without the rolled write deadline the answer of such a run was cut off
// (the client read EOF while the map was written). The test stands a 1 s
// WriteTimeout and a 200 ms heartbeat for ctxd's 120 s and 25 s, and holds
// the digest's read on a lock for 2.5 s.
//
//	go test -tags=integration ./internal/handler/ -run 'TestDigestAnswersPastTheWriteTimeout' -count=1 -v

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestDigestAnswersPastTheWriteTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	admin, _ := kaServerAdmin(ctx, t, pool)
	for _, title := range []string{"i505 digest a", "i505 digest b"} {
		if _, err := store.UpsertBlock(ctx, pool, "learnings", title, "body", nil, nil, admin.HomeScope, true, store.SensitivityWrite{}, ""); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	reg := blocktype.NewRegistry()
	bctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	reg.Boot(bctx, pool)
	h := NewDigestHandler(pool, reg, staticConfigStore{cfg: config.Defaults()})

	prevInterval, prevWindow := heartbeatInterval, heartbeatWriteWindow
	heartbeatInterval, heartbeatWriteWindow = 200*time.Millisecond, time.Second
	t.Cleanup(func() { heartbeatInterval, heartbeatWriteWindow = prevInterval, prevWindow })

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.HandleDigest(w, r.WithContext(context.WithValue(r.Context(), authResultKey, admin)))
	}))
	srv.Config.WriteTimeout = time.Second // stands for ctxd's 120 s
	srv.Start()
	t.Cleanup(srv.Close)

	// The digest's read of the blocks waits on this lock for 2.5 s.
	release := lockedTx(t, pool, `LOCK TABLE context_blocks IN ACCESS EXCLUSIVE MODE`)
	go func() {
		time.Sleep(2500 * time.Millisecond)
		release()
	}()
	start := time.Now()
	resp, err := (&http.Client{}).Post(srv.URL+"/api/digest", "application/json", strings.NewReader(`{"trigger":"manual"}`))
	if err != nil {
		t.Fatalf("the digest's answer did not arrive (%v after %s): the write deadline cut it", err, time.Since(start).Round(time.Millisecond))
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	took := time.Since(start)
	var out map[string]any
	if err != nil || resp.StatusCode != http.StatusOK || json.Unmarshal(body, &out) != nil || out["success"] != true {
		t.Fatalf("digest answered %d %q (%v) after %s, want 200 and its success body", resp.StatusCode, body, err, took)
	}
	if took < 2*time.Second {
		t.Fatalf("the digest answered after %s: the lock did not hold it past the WriteTimeout — the test proves nothing", took)
	}
	t.Logf("digest answered %d after %s past a 1 s WriteTimeout: %s", resp.StatusCode, took.Round(time.Millisecond), body)
}
