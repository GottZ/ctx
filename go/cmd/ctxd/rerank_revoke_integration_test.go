//go:build integration

// W01-8 landing (review 2 M1): the rerank routes under the door's in-flight
// registry, over the real router (buildRouter without a route wrap) — real
// ctxm_ key, real door, real registry, real POST /v1/rerank and POST
// /v2/rerank, the engine holding the block. The manage action
// model-key-revoke ends the running rerank request: the action answers
// cancelled_inflight 1, the request 401 invalid_api_key with
// WWW-Authenticate before any byte of an answer, the engine call aborted,
// the lease free, the registry empty. Red: the rerank verdict without the
// auth.ErrCredentialRevoked branch answers an empty 200.
//
//	go test -tags=integration -p 1 -count=1 -run 'RevokedKeyEndsRerankOverTheRealRouter' -v ./cmd/ctxd/
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestRevokedKeyEndsRerankOverTheRealRouter(t *testing.T) {
	var hits atomic.Int64
	entered := make(chan struct{}, 4)
	aborted := make(chan struct{}, 4)
	release := make(chan struct{})
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.Copy(io.Discard, r.Body) // a read body lets the server see the client go away (r.Context)
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			aborted <- struct{}{}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"engine","usage":{"prompt_tokens":6,"total_tokens":6},"results":[{"index":0,"relevance_score":1},{"index":1,"relevance_score":0},{"index":2,"relevance_score":2}]}`))
	}))
	t.Cleanup(engine.Close)
	t.Cleanup(func() { close(release) }) // runs before engine.Close (cleanups run last-in first-out)

	seed, dsn := testdb.SetupTestDBWithDSN(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	applyEnv(t, map[string]string{"CTX_GATEWAY_INFLIGHT_SWEEP_INTERVAL": "3600"})
	cc, issues := loadFromEnv()
	if config.HasErrors(issues) {
		t.Fatalf("env fixture invalid: %v", issues)
	}
	cfgStore := config.NewStore(cc)
	insertAlias(t, seed, "_global", "rr", "rerank", `{}`, true)
	created, adminID, err := store.BootstrapAdminKey(ctx, seed, v1AdminKey, "w01-8-rerank")
	if err != nil || !created {
		t.Fatalf("bootstrap admin key: created=%v err=%v", created, err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	dispatch.SetPrincipalHook(handler.RequestPrincipal) // as cmd/ctxd/main.go
	t.Cleanup(func() { dispatch.SetPrincipalHook(nil) })
	row := backends.Backend{
		ID: "id-rerank", Name: "rerank-row", Host: engine.URL, Protocol: backends.ProtocolOpenAI, Model: "engine-model",
		Roles: []string{"rerank"}, Trust: backends.TrustFull, Locality: "local", Scope: backends.GlobalScope, Enabled: true,
		Priority: 1, Timeouts: map[string]int{"rerank": 30},
	}
	backendPool := backends.NewPool(pool, nil)
	backendPool.SeedSnapshotForTest([]backends.Backend{row})
	scheduler := events.NewScheduler(pool, cfgStore, backendPool, events.StartupConfig{})
	scheduler.SetModelAliasRegistry(bootModelAliases(ctx, pool, modelAliasCeilings(cfgStore)))
	projectHub := events.NewProjectHub(ctx, pool, cfgStore)
	disp := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(disp.Close)
	disp.UpdatePolicy(dispatch.Policy{Targets: map[string]dispatch.TargetPolicy{row.Host: {Slots: 4, InternalReserved: 1}}})
	scheduler.SetDispatcher(disp)
	sctx := fedhop.WithIdentity(ctx, fedhop.NewIdentity("00000000-0000-4000-8000-0000000018f9", bytes.Repeat([]byte{0x18}, 32)))
	router, _, gw := buildRouter(sctx, pool, cfgStore, scheduler, backendPool, blocktype.NewRegistry(), projectHub, disp, nil)
	next := *cfgStore.Snapshot()
	next.Gateway.Enabled = true
	if err := cfgStore.Replace(&next); err != nil {
		t.Fatalf("set gateway.enabled=true: %v", err)
	}
	held := func() int {
		for _, ts := range disp.Snapshot().Targets {
			if ts.Origin == row.Host {
				return ts.Held
			}
		}
		return 0
	}

	for i, path := range []string{"/v1/rerank", "/v2/rerank"} {
		_, k, key, err := store.CreateConsumerWithKey(ctx, seed,
			store.ModelConsumerSpec{TenantID: store.DefaultTenantID, Label: "w01-8-rerank-" + strings.TrimPrefix(path, "/v")[:1], AllModels: true, AdmissionLane: handler.LaneUrgent},
			nil, store.APIStamp(adminID, "w01-8-rerank"))
		if err != nil {
			t.Fatalf("consumer %d: %v", i, err)
		}
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			req := httptest.NewRequestWithContext(ctx, http.MethodPost, path, strings.NewReader(`{"model":"rr","query":"q","documents":["alpha","beta","gamma"]}`))
			req.RemoteAddr = "192.0.2.1:40000"
			req.TLS = &tls.ConnectionState{}
			req.Header.Set("Authorization", "Bearer "+key.Plaintext())
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			done <- rec
		}()
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			t.Fatalf("POST %s: the rerank block did not reach the engine", path)
		}

		body, _ := json.Marshal(map[string]any{"action": "model-key-revoke", "data": map[string]any{"key_id": k.ID}})
		mreq := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/manage", strings.NewReader(string(body)))
		mreq.Header.Set("Authorization", "Bearer "+v1AdminKey)
		mreq.Header.Set("Content-Type", "application/json")
		at := time.Now()
		mrec := httptest.NewRecorder()
		router.ServeHTTP(mrec, mreq)
		var resp map[string]any
		_ = json.Unmarshal(mrec.Body.Bytes(), &resp)
		if v, ok := resp["cancelled_inflight"].(float64); mrec.Code != http.StatusOK || !ok || v != 1 {
			t.Errorf("POST %s: model-key-revoke %d %s, want 200 with cancelled_inflight 1", path, mrec.Code, mrec.Body.String())
		}

		var rec *httptest.ResponseRecorder
		select {
		case rec = <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("POST %s: the rerank request still runs 5 s after the revocation", path)
		}
		t.Logf("W018 rerank POST %s: model-key-revoke %s → %d after %v, %d body bytes: %s (engine hits %d)", path,
			strings.TrimSpace(mrec.Body.String()), rec.Code, time.Since(at).Round(time.Millisecond), rec.Body.Len(), strings.TrimSpace(rec.Body.String()), hits.Load())
		if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") == "" || !strings.Contains(rec.Body.String(), `"code":"invalid_api_key"`) {
			t.Errorf("POST %s: answer %d %v %q, want 401 invalid_api_key with WWW-Authenticate", path, rec.Code, rec.Header(), rec.Body.String())
		}
		select {
		case <-aborted:
		case <-time.After(2 * time.Second):
			t.Errorf("POST %s: the engine call was not aborted", path)
		}
		deadline := time.Now().Add(2 * time.Second)
		for held() != 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if n := held(); n != 0 {
			t.Errorf("POST %s: %d leases still held", path, n)
		}
		if s := gw.door.Stats(); s.Inflight != 0 || s.InflightCancelled != uint64(i+1) {
			t.Errorf("POST %s: registry running %d, ended %d — want 0 running and %d ended", path, s.Inflight, s.InflightCancelled, i+1)
		}
	}
}
