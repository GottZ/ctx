//go:build integration

// MW2 integration probe (design/01 §4.5 + §4.6 N8a signal side): the
// PRODUCTION router mounts /api/synthesize/daily wrapped in WithScheduler —
// the third herald entry. The probe observes InteractiveDemand() == 1 WHILE
// the daily request is mid-handler (held deterministically on a table lock
// its first query needs), and 0 after the response: demand counts from HTTP
// ingress, not from any wire call, and every entry has its exit.
//
// Red probe (2026-07-06, documented per wave contract): with the
// WithScheduler wrap removed from the /api/synthesize/daily mount in
// server.go, this test failed with "herald never saw the daily request" —
// it observes the PRODUCTION mount, not a re-built fixture chain.

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/events"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestDailyMountIsThirdHeraldEntry_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	applyEnv(t, map[string]string{})
	cc, issues := loadFromEnv()
	if config.HasErrors(issues) {
		t.Fatalf("env fixture invalid: %v", issues)
	}
	cfgStore := config.NewStore(cc)

	backendPool := backends.NewPool(pool, nil)
	blocktypeReg := blocktype.NewRegistry()
	scheduler := events.NewScheduler(pool, cfgStore, backendPool, events.StartupConfig{})
	projectHub := events.NewProjectHub(ctx, pool, cfgStore)

	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	scheduler.SetDispatcher(d)

	// Hex-only: auth.SanitizeKey strips every non-hex character before the
	// hash comparison, so the fixture key must survive sanitization as-is.
	const plaintext = "aa112026070600000000000000000000deadbeefdeadbeefdeadbeefdeadbeef"
	created, _, err := store.BootstrapAdminKey(ctx, pool, plaintext, "mw2-herald-probe")
	if err != nil || !created {
		t.Fatalf("bootstrap admin key: created=%v err=%v", created, err)
	}

	router := NewRouter(ctx, pool, cfgStore, scheduler, backendPool, blocktypeReg, projectHub, d)

	// Deterministic mid-handler hold: HandleDaily's first query reads
	// context_write_log (dream.fetchDailyDecisions); an ACCESS EXCLUSIVE
	// lock blocks it until rollback — the request sits INSIDE the wrapped
	// handler, after the herald entry, before any response.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin lock tx: %v", err)
	}
	if _, err := tx.Exec(ctx, "LOCK TABLE context_write_log IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatalf("lock context_write_log: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/synthesize/daily", nil)
	req.Header.Set("Authorization", "Bearer "+plaintext)
	done := make(chan struct{})
	go func() {
		defer close(done)
		router.ServeHTTP(rec, req)
	}()

	// Demand must become visible while the handler hangs on the lock.
	deadline := time.Now().Add(15 * time.Second)
	for d.InteractiveDemand() == 0 {
		if time.Now().After(deadline) {
			_ = tx.Rollback(ctx)
			t.Fatal("herald never saw the daily request — /api/synthesize/daily mount not WithScheduler-wrapped?")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := d.InteractiveDemand(); got != 1 {
		t.Errorf("mid-flight demand = %d, want 1", got)
	}

	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("release lock: %v", err)
	}
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("daily request did not finish after lock release")
	}

	if got := d.InteractiveDemand(); got != 0 {
		t.Fatalf("demand after response = %d, want 0 (herald leak)", got)
	}
	// Empty DB ⇒ deterministic no_activity 200 (no LLM call happens).
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}
