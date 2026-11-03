//go:build integration

// Integration probe for wave W02-02 (masterplan K4, design/02 §4.2 stage 1):
// with gateway.enabled=false every request under the gateway prefix gets the
// same OpenAI 404 BEFORE any credential is looked at — no oracle on whether a
// key is valid, and no database work for anyone.
//
//	go test -tags=integration -p 1 -count=1 -run 'Gateway' ./internal/handler/
package handler

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/oai"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// gatewayQueryCounter counts EVERY query on the traced pool — the gate's
// claim is "no database work at all", so there is no fingerprint to match.
type gatewayQueryCounter struct{ n atomic.Int64 }

func (c *gatewayQueryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.n.Add(1)
	return ctx
}

func (c *gatewayQueryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// gatewayTracedPool opens a second pool on the test DSN with the counter.
func gatewayTracedPool(t *testing.T, dsn string) (*pgxpool.Pool, *gatewayQueryCounter) {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	ctr := &gatewayQueryCounter{}
	cfg.ConnConfig.Tracer = ctr
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("traced pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, ctr
}

type gatewayProbe struct {
	status int
	body   []byte
	header http.Header
	reads  int64
}

// probeGateway sends one GET through h and reports the answer plus the
// number of queries the traced pool saw for it.
func probeGateway(t *testing.T, h http.Handler, ctr *gatewayQueryCounter, bearer string) gatewayProbe {
	t.Helper()
	before := ctr.n.Load()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return gatewayProbe{status: rec.Code, body: rec.Body.Bytes(), header: rec.Header(), reads: ctr.n.Load() - before}
}

// TestGatewayEnabledGateBeforeAuth: flag off ⇒ an anonymous request and one
// carrying a VALID key get byte-identical 404s, and neither costs a query.
// The stack under probe is the order the /v1 group mounts (design/02 §4.2):
// the gate, then the authenticating middleware — here the knowledge Auth,
// standing in for ModelAuth (W01-6), because it is a real credential check
// that queries the database for a well-formed key.
func TestGatewayEnabledGateBeforeAuth(t *testing.T) {
	pool, ctr, rawKey := gatewayGateFixture(t)
	off := oai.EnabledGate(func() oai.Settings { return oai.Settings{Enabled: false} })
	for _, v := range gatewayGateViolations(t, off(Auth(pool)(gatewayInner())), ctr, rawKey) {
		t.Error(v)
	}
}

// TestGatewayEnabledGateCounterproof keeps the red side reproducible: the
// same probe against the inverted stack (Auth first, gate behind it) sees a
// 401 for the anonymous request, a query for the valid key, and two
// different bodies.
func TestGatewayEnabledGateCounterproof(t *testing.T) {
	pool, ctr, rawKey := gatewayGateFixture(t)
	off := oai.EnabledGate(func() oai.Settings { return oai.Settings{Enabled: false} })
	got := gatewayGateViolations(t, Auth(pool)(off(gatewayInner())), ctr, rawKey)
	for _, v := range got {
		t.Log(v)
	}
	if len(got) == 0 {
		t.Error("the Auth-first stack passes the gate probe — the probe has lost its teeth")
	}
}

func gatewayInner() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
}

// gatewayGateFixture: a fresh DB with one real knowledge key, and a traced
// pool on it for the stack under probe.
func gatewayGateFixture(t *testing.T) (*pgxpool.Pool, *gatewayQueryCounter, string) {
	t.Helper()
	seedPool, dsn := testdb.SetupTestDBWithDSN(t)
	_, rawKey, err := store.CreateApiKey(context.Background(), seedPool, "w02-02-gate-probe", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatalf("create probe key: %v", err)
	}
	pool, ctr := gatewayTracedPool(t, dsn)
	return pool, ctr, rawKey
}

// gatewayGateViolations probes stack anonymously and with the valid key and
// returns what deviates from "same 404, zero queries".
func gatewayGateViolations(t *testing.T, stack http.Handler, ctr *gatewayQueryCounter, rawKey string) []string {
	t.Helper()
	var out []string
	anon := probeGateway(t, stack, ctr, "")
	valid := probeGateway(t, stack, ctr, rawKey)
	for _, c := range []struct {
		name string
		p    gatewayProbe
	}{{"anonymous", anon}, {"valid key", valid}} {
		if c.p.status != http.StatusNotFound {
			out = append(out, fmt.Sprintf("%s: status %d, want 404 (flag off answers before auth)", c.name, c.p.status))
		}
		if c.p.reads != 0 {
			out = append(out, fmt.Sprintf("%s: %d database queries, want 0 (the gate runs before any credential lookup)", c.name, c.p.reads))
		}
	}
	if !bytes.Equal(anon.body, valid.body) {
		out = append(out, fmt.Sprintf("bodies differ — the answer is an oracle on the key: anonymous %q, valid key %q", anon.body, valid.body))
	}
	for _, h := range []string{"Content-Type", "x-should-retry", "WWW-Authenticate"} {
		if anon.header.Get(h) != valid.header.Get(h) {
			out = append(out, fmt.Sprintf("header %s differs: anonymous %q, valid key %q", h, anon.header.Get(h), valid.header.Get(h)))
		}
	}
	return out
}
