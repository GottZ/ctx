//go:build integration

package main

// TestRouterWiresTheInflightRegistry pins the production wiring of W01-8's
// in-flight registry (cmd/ctxd server.go, gateway.go) through the router
// main builds (buildRouter, with one blocking probe route behind the
// model-key door instead of the gateway routes): (1) the manage action
// model-key-revoke ends the running request and answers cancelled_inflight
// 1 — manageH.SetInflight; (2) a psql revocation of another key reaches
// the registry through a listener built from the router's scheduler and is
// checked by the gateway's background loops — scheduler.SetModelAccessSink
// and the worker in modelGateway.run (startModelGateway). Red: either
// wiring removed (the action answers null and the request runs on; the
// psql revocation ends nothing).

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/events"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

const wiringAdminKey = "f8f82026110700000000000000000000deadbeefdeadbeefdeadbeefdeadbe01"

func TestRouterWiresTheInflightRegistry(t *testing.T) {
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	applyEnv(t, map[string]string{"CTX_GATEWAY_ENABLED": "true", "CTX_GATEWAY_INFLIGHT_SWEEP_INTERVAL": "3600"})
	cc, issues := loadFromEnv()
	if config.HasErrors(issues) {
		t.Fatalf("env fixture invalid: %v", issues)
	}
	cfgStore := config.NewStore(cc)
	backendPool := backends.NewPool(pool, nil)
	scheduler := events.NewScheduler(pool, cfgStore, backendPool, events.StartupConfig{})
	projectHub := events.NewProjectHub(ctx, pool, cfgStore)
	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	scheduler.SetDispatcher(d)
	created, adminID, err := store.BootstrapAdminKey(ctx, pool, wiringAdminKey, "w01-8-wiring")
	if err != nil || !created {
		t.Fatalf("bootstrap admin key: created=%v err=%v", created, err)
	}
	stamp := store.APIStamp(adminID, "w01-8-wiring")
	_, k1, key1, err := store.CreateConsumerWithKey(ctx, pool, store.ModelConsumerSpec{TenantID: store.DefaultTenantID, Label: "w018-wire-1"}, nil, stamp)
	if err != nil {
		t.Fatalf("consumer 1: %v", err)
	}
	_, k2, key2, err := store.CreateConsumerWithKey(ctx, pool, store.ModelConsumerSpec{TenantID: store.DefaultTenantID, Label: "w018-wire-2"}, nil, stamp)
	if err != nil {
		t.Fatalf("consumer 2: %v", err)
	}

	ends := make(chan error, 4)
	started := make(chan struct{}, 4)
	probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-r.Context().Done()
		ends <- context.Cause(r.Context())
	})
	ident := fedhop.NewIdentity("00000000-0000-4000-8000-0000000018f8", []byte(strings.Repeat("\x18", 32)))
	router, _, gw := buildRouter(fedhop.WithIdentity(ctx, ident), pool, cfgStore, scheduler, backendPool, blocktype.NewRegistry(), projectHub, d,
		func([]modelRoute) []modelRoute { return []modelRoute{{method: http.MethodPost, pattern: "/probe", handler: probe}} })
	stop := startModelGateway(gw)
	t.Cleanup(stop)
	l := events.NewPgxlistenListener(dsn, 100*time.Millisecond, scheduler, pool, cfgStore, backendPool, nil, nil)
	lctx, lcancel := context.WithCancel(ctx)
	ldone := make(chan struct{})
	go func() { defer close(ldone); _ = l.Listen(lctx) }()
	t.Cleanup(func() { lcancel(); <-ldone })
	listening := func() bool {
		var n int
		_ = pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND query ILIKE 'listen %'`).Scan(&n)
		return n > 0
	}
	for deadline := time.Now().Add(10 * time.Second); !listening(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the listener did not connect")
		}
	}

	open := func(plain string) {
		t.Helper()
		go func() {
			req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/probe", strings.NewReader("{}"))
			req.RemoteAddr = "192.0.2.7:40000"
			req.TLS = &tls.ConnectionState{}
			req.Header.Set("Authorization", "Bearer "+plain)
			router.ServeHTTP(httptest.NewRecorder(), req)
		}()
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("the probe request did not pass the door")
		}
	}
	ended := func(what string) {
		t.Helper()
		select {
		case cause := <-ends:
			if !errors.Is(cause, auth.ErrCredentialRevoked) {
				t.Errorf("%s: cause %v, want auth.ErrCredentialRevoked", what, cause)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: the running request did not end", what)
		}
	}

	// (1) The manage action through the router.
	open(key1.Plaintext())
	body, _ := json.Marshal(map[string]any{"action": "model-key-revoke", "data": map[string]any{"key_id": k1.ID}})
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/manage", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+wiringAdminKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if v, ok := resp["cancelled_inflight"].(float64); rec.Code != http.StatusOK || !ok || v != 1 {
		t.Errorf("model-key-revoke: %d %s, want 200 with cancelled_inflight 1 (manageH.SetInflight)", rec.Code, rec.Body.String())
	}
	ended("model-key-revoke through the router")

	// (2) psql through the router's scheduler and the gateway's loops.
	open(key2.Plaintext())
	if _, err := pool.Exec(ctx, `UPDATE context_model_keys SET revoked_at = now() WHERE id = $1::uuid`, k2.ID); err != nil {
		t.Fatalf("psql revoke: %v", err)
	}
	ended("psql revocation (scheduler sink, gateway worker)")
	if s := gw.door.Stats(); s.InflightCancelled != 2 {
		t.Errorf("the door's registry ended %d requests, want 2", s.InflightCancelled)
	}
}
