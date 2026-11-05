//go:build integration

package main

// Integration pin of Bau-Abweichung 3 (Härtung W02-07 H-5, taken over from
// the Fremd-Review 2 probe): the production router as buildRouter mounts it
// — WITHOUT the test's limiter wrap — has no Limiter (W06-10 not built), and
// the chat route refuses fail-closed: 503 service_unavailable with the
// largest Retry-After step of dispatch.external_retry_after_steps and
// x-should-retry: true, before any engine contact and without a gateway-chat
// row; GET /v1/models is not affected.
//
// W06-10 turns this pin around on purpose: with the meter wired the same
// request is served — that wave replaces the assertions, it does not delete
// the fail-closed case (a Limiter that is nil again must still refuse).

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/events"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestV1ChatLimiterMissingOnTheRealRouter(t *testing.T) {
	seed, dsn := testdb.SetupTestDBWithDSN(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	applyEnv(t, map[string]string{})
	cc, issues := loadFromEnv()
	if config.HasErrors(issues) {
		t.Fatalf("env: %v", issues)
	}
	cfgStore := config.NewStore(cc)
	insertAlias(t, seed, "_global", v1ChatAlias, "chat", `{}`, true)
	admin, _, err := store.CreateApiKey(ctx, seed, "limiter-nil", "private", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	_, _, key, err := store.CreateConsumerWithKey(ctx, seed,
		store.ModelConsumerSpec{TenantID: store.DefaultTenantID, Label: "limiter-nil", AllModels: true, AdmissionLane: handler.LaneUrgent},
		nil, store.APIStamp(admin.ID, "limiter-nil"))
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	dispatch.SetPrincipalHook(handler.RequestPrincipal)
	t.Cleanup(func() { dispatch.SetPrincipalHook(nil) })
	e := newV1ChatEngine(t, http.StatusOK)
	rows := []backends.Backend{e.row("gpu", 10)}
	bp := backends.NewPool(pool, nil)
	bp.SeedSnapshotForTest(rows)
	sched := events.NewScheduler(pool, cfgStore, bp, events.StartupConfig{})
	sched.SetModelAliasRegistry(bootModelAliases(ctx, pool, modelAliasCeilings(cfgStore)))
	hub := events.NewProjectHub(ctx, pool, cfgStore)
	disp := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(disp.Close)
	pol := dispatch.Policy{Targets: map[string]dispatch.TargetPolicy{}}
	for _, b := range rows {
		pol.Targets[b.Host] = dispatch.TargetPolicy{Slots: 4, InternalReserved: 1}
	}
	disp.UpdatePolicy(pol)
	sched.SetDispatcher(disp)
	ident := fedhop.NewIdentity("00000000-0000-4000-8000-0000000f0207", bytes.Repeat([]byte{0x29}, 32))
	router, _, _ := buildRouter(fedhop.WithIdentity(ctx, ident), pool, cfgStore, sched, bp, blocktype.NewRegistry(), hub, disp, nil)
	next := *cfgStore.Snapshot()
	next.Gateway.Enabled = true
	if err := cfgStore.Replace(&next); err != nil {
		t.Fatal(err)
	}
	do := func(method, path, body string) (int, http.Header, []byte) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.RemoteAddr = "192.0.2.1:40000"
		req.TLS = &tls.ConnectionState{}
		req.Header.Set("Authorization", "Bearer "+key.Plaintext())
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code, rec.Header(), rec.Body.Bytes()
	}

	steps := disp.ExternalRetryAfterSteps()
	if len(steps) == 0 {
		t.Fatal("the dispatcher has no external Retry-After steps")
	}
	wantRA := strconv.Itoa(int(math.Ceil(steps[len(steps)-1].Seconds())))
	for i := range 3 {
		st, h, b := do(http.MethodPost, "/v1/chat/completions", `{"model":"`+v1ChatAlias+`","messages":[{"role":"user","content":"hi"}]}`)
		var env struct {
			Error struct {
				Code *string `json:"code"`
				Type string  `json:"type"`
			} `json:"error"`
		}
		if err := json.Unmarshal(b, &env); err != nil || env.Error.Code == nil {
			t.Fatalf("request %d: %d %s, want the 503 service_unavailable envelope", i, st, b)
		}
		if st != http.StatusServiceUnavailable || *env.Error.Code != "service_unavailable" || env.Error.Type != "service_unavailable_error" {
			t.Fatalf("request %d: %d %s, want 503 service_unavailable (no Limiter wired)", i, st, b)
		}
		if got := h.Get("Retry-After"); got != wantRA {
			t.Fatalf("request %d: Retry-After = %q, want the largest step %s of %v", i, got, wantRA, steps)
		}
		if got := h.Get("x-should-retry"); got != "true" {
			t.Fatalf("request %d: x-should-retry = %q, want true", i, got)
		}
	}
	if st, _, b := do(http.MethodGet, "/v1/models", ""); st != http.StatusOK || !bytes.Contains(b, []byte(`"`+v1ChatAlias+`"`)) {
		t.Fatalf("GET /v1/models without a Limiter: %d %s, want 200 listing %s", st, b, v1ChatAlias)
	}
	if n := e.hits.Load(); n != 0 {
		t.Fatalf("engine contacts %d, want 0 (refused before the planner)", n)
	}
	// The row writer is asynchronous: settle it before counting (as
	// v1ChatFixture.rows does).
	sctx, scancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer scancel()
	if err := llmlog.Settle(sctx, pool); err != nil {
		t.Fatalf("llmlog settle: %v", err)
	}
	var n int
	if err := seed.QueryRow(ctx, `SELECT count(*) FROM context_llm_log WHERE pipeline = $1`, llmlog.PipelineGatewayChat).Scan(&n); err != nil {
		t.Fatalf("count gateway-chat rows: %v", err)
	}
	if n != 0 {
		t.Fatalf("gateway-chat rows %d, want 0", n)
	}
}
