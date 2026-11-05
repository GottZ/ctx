//go:build integration

// W06-8b gate through the PRODUCTION wiring: NewRouter must leave the
// foreign-load detector probing on its own cadence with no status reader at
// all — no /api/status poll, no SSE stream, not one request served. A marked
// '_global' row on a fake SGLang /metrics reporting 3 running requests, the
// real dispatcher (enabled, the origin never acquired ⇒ held 0) and
// CTX_STATUS_FOREIGN_LOAD_INTERVAL=1 through the env loader: within a few
// intervals the fake sees ≥ 2 GETs and the detector logs its one "suspected"
// WARN. Then the server context ends and no further GET arrives. Red against
// a tree whose rounds only follow status reads: 0 GETs, no WARN.

package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/events"
	"github.com/GottZ/ctx/internal/testdb"
)

// flTickLog counts slog records by message.
type flTickLog struct {
	mu   sync.Mutex
	msgs []string
}

func (l *flTickLog) Enabled(context.Context, slog.Level) bool { return true }
func (l *flTickLog) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	l.msgs = append(l.msgs, r.Level.String()+" "+r.Message)
	l.mu.Unlock()
	return nil
}
func (l *flTickLog) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *flTickLog) WithGroup(string) slog.Handler      { return l }

func (l *flTickLog) count(msg string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, m := range l.msgs {
		if m == msg {
			n++
		}
	}
	return n
}

func TestForeignLoadTickerHeadless_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)

	var hits atomic.Int32
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		hits.Add(1)
		fmt.Fprint(w, strings.Join([]string{
			`sglang:num_running_reqs{engine_type="unified",model_name="qwen38-27b",tp_rank="0"} 3.0`,
			`sglang:http_requests_active{endpoint="/v1/chat/completions",method="POST"} 3.0`,
			`sglang:http_responses_total{endpoint="/v1/chat/completions",method="POST",status_code="200"} 100.0`,
		}, "\n")+"\n")
	}))
	defer engine.Close()

	logs := &flTickLog{}
	prev := slog.Default()
	slog.SetDefault(slog.New(logs))
	t.Cleanup(func() { slog.SetDefault(prev) })

	applyEnv(t, map[string]string{"CTX_STATUS_FOREIGN_LOAD_INTERVAL": "1"})
	cc, issues := loadFromEnv()
	if config.HasErrors(issues) {
		t.Fatalf("env fixture invalid: %v", issues)
	}
	cfgStore := config.NewStore(cc)
	backendPool := backends.NewPool(pool, nil)
	backendPool.SeedSnapshotForTest([]backends.Backend{{
		ID: "fl-ticker", Name: "spark-chat", Host: engine.URL, Scope: backends.GlobalScope, Enabled: true,
		Roles: []string{backends.RoleChat}, Metadata: map[string]any{"foreign_load_probe": "sglang"},
	}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	scheduler := events.NewScheduler(pool, cfgStore, backendPool, events.StartupConfig{})
	projectHub := events.NewProjectHub(ctx, pool, cfgStore)
	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	scheduler.SetDispatcher(d)
	_ = NewRouter(ctx, pool, cfgStore, scheduler, backendPool, blocktype.NewRegistry(), projectHub, d)
	start := time.Now()

	const suspected = "WARN status: foreign load suspected"
	deadline := start.Add(6 * time.Second)
	for (hits.Load() < 2 || logs.count(suspected) < 1) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	got, warns := hits.Load(), logs.count(suspected)
	t.Logf("no status read: %d /metrics GETs and %d suspected WARN after %v at a 1s interval", got, warns, time.Since(start).Round(time.Millisecond))
	if got < 2 || warns != 1 {
		t.Fatalf("%d /metrics GETs and %d suspected WARNs within 6s without a status reader, want >= 2 and 1 — the detector only measures while someone reads the status", got, warns)
	}

	cancel()
	time.Sleep(200 * time.Millisecond) // a round in flight at the cancel is cut off, not finished
	atStop := hits.Load()
	time.Sleep(2500 * time.Millisecond) // more than two intervals
	if n := hits.Load(); n != atStop {
		t.Fatalf("%d /metrics GETs after the server context ended (%d at the stop, %d now) — rounds outlive the shutdown", n-atStop, atStop, n)
	}
	if n := logs.count(suspected); n != 1 {
		t.Fatalf("%d suspected WARNs, want 1 (per state change)", n)
	}
}
