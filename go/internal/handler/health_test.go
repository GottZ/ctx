// F1-W7 health-shape invariant: GET /health is unauthenticated and proxied
// to the public internet. Its body must NEVER contain a host string or model
// name from the config — only role names mapped to ok|error. The tests pin
// the invariant against BOTH ping outcomes (the error path is the dangerous
// one: a careless refactor rendering ping errors into the body would embed
// `dial tcp <host>` topology), plus the W7 property itself: the ping targets
// come from the CURRENT snapshot, not a boot copy.
//
// Fixture hygiene: ping targets are loopback httptest servers / closed
// loopback ports (deterministic reachability — RFC-2606 .example hosts would
// make the error path depend on the resolver: NXDOMAIN is fast, a DNS
// blackhole eats the full 5s budget). Loopback URLs carry no real topology;
// every other config string stays synthetic.
package handler

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
)

// swapStore is a minimal ConfigStore whose generation can be swapped without
// config.Store's Validate gate (the synthetic test configs are deliberately
// sparse and would not pass V-class validation).
type swapStore struct {
	p atomic.Pointer[config.Config]
}

func (s *swapStore) Snapshot() *config.Config { return s.p.Load() }
func (s *swapStore) SnapshotForRequest(context.Context) *config.Config { return s.p.Load() }
func (s *swapStore) SnapshotForTenant(context.Context, string) *config.Config { return s.p.Load() }

// healthTestConfig wires all three ping roles to the given hosts and plants
// distinctive synthetic strings in every name-carrying field. The needles
// derived from it must not surface in the public body.
func healthTestConfig(embedHost, chatHost, dreamHost string) *config.Config {
	return &config.Config{
		Embed: config.EmbedConfig{
			Host: embedHost, Model: "needle-embed-model-g14", APIKey: "sk-needle-embed-0123456789abcdef",
		},
		Chat: config.ChatConfig{
			Host: chatHost, Model: "needle-chat-model-g14", APIKey: "sk-needle-chat-0123456789abcdefg",
		},
		Dream: config.DreamConfig{
			Enabled: true,
			Host:    dreamHost, Model: "needle-dream-model-g14", APIKey: "sk-needle-dream-0123456789abcdef",
		},
	}
}

// healthNeedles lists every config-derived string that must stay out of the
// response body: full host URLs, their bare host:port forms, model names and
// API keys.
func healthNeedles(cfg *config.Config) []string {
	needles := []string{
		cfg.Embed.Host, cfg.Chat.Host, cfg.Dream.Host,
		strings.TrimPrefix(cfg.Embed.Host, "http://"),
		strings.TrimPrefix(cfg.Chat.Host, "http://"),
		strings.TrimPrefix(cfg.Dream.Host, "http://"),
		cfg.Embed.Model, cfg.Chat.Model, cfg.Dream.Model,
		cfg.Embed.APIKey, cfg.Chat.APIKey, cfg.Dream.APIKey,
	}
	out := needles[:0]
	for _, n := range needles {
		if n != "" {
			out = append(out, n)
		}
	}
	return out
}

// healthTestPool builds the F3 pool snapshot the health aggregation reads:
// one backend per historical ping role, each with a distinctive name that
// must never surface in the public body (extra needles).
func healthTestPool(embedHost, chatHost, dreamHost string) *backends.Pool {
	bp := backends.NewPool(nil, nil)
	bp.SeedSnapshotForTest([]backends.Backend{
		{ID: "e", Name: "needle-backend-embed", Host: embedHost, Enabled: true,
			Trust: backends.TrustFull, Roles: []string{backends.RoleEmbed}},
		{ID: "c", Name: "needle-backend-chat", Host: chatHost, Enabled: true,
			Trust: backends.TrustFull, Roles: []string{backends.RoleSynthesis}},
		{ID: "d", Name: "needle-backend-dream", Host: dreamHost, Enabled: true,
			Trust: backends.TrustFull, Roles: []string{backends.RoleDream}},
	})
	return bp
}

var poolNameNeedles = []string{"needle-backend-embed", "needle-backend-chat", "needle-backend-dream"}

// closedPortHost reserves a loopback port and closes it again: connecting
// fails with an immediate refusal — the deterministic ping-error fixture.
func closedPortHost(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return "http://" + addr
}

// assertHealthBody enforces the full shape contract on a raw body: exactly
// the two fields {status, services}, status from the fixed vocabulary,
// service keys from the fixed role set, service values only ok|error — and
// no config-derived needle anywhere in the raw bytes. Because every field is
// vocabulary-checked AND the raw body is needle-scanned, the invariant holds
// for each response field, present and future.
func assertHealthBody(t *testing.T, body []byte, needles []string) {
	t.Helper()

	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	var resp struct {
		Status            string            `json:"status"`
		Services          map[string]string `json:"services"`
		BlocktypeRegistry string            `json:"blocktype_registry"`
		SchemaContract    string            `json:"schema_contract"`
	}
	if err := dec.Decode(&resp); err != nil {
		t.Fatalf("health body has unknown fields or bad shape: %v\nbody: %s", err, body)
	}

	switch resp.Status {
	case "ok", "degraded", "unhealthy":
	default:
		t.Errorf("status %q outside the fixed vocabulary", resp.Status)
	}
	// WF T3: the blocktype_registry field carries process-internal
	// degradation state, name-free by construction ("ok"|"builtin-fallback").
	switch resp.BlocktypeRegistry {
	case "ok", "builtin-fallback":
	default:
		t.Errorf("blocktype_registry %q outside the fixed vocabulary", resp.BlocktypeRegistry)
	}
	// Evokoa design/03 §4.6/E-03-4 (D03): public schema_contract collapses
	// drift|unchecked into ONE value "attention" (Timing-Orakel-Argument) —
	// ok|attention|off is the full public vocabulary, name-free, never an
	// object name or hash.
	switch resp.SchemaContract {
	case "ok", "attention", "off":
	default:
		t.Errorf("schema_contract %q outside the fixed vocabulary", resp.SchemaContract)
	}
	roles := map[string]bool{"database": true, "embed": true, "chat": true, "dream": true}
	for k, v := range resp.Services {
		if !roles[k] {
			t.Errorf("service key %q outside the fixed role set", k)
		}
		if v != "ok" && v != "error" {
			t.Errorf("service %s value %q is not ok|error", k, v)
		}
	}

	raw := string(body)
	for _, n := range needles {
		if strings.Contains(raw, n) {
			t.Errorf("health body leaks config string %q:\n%s", n, raw)
		}
	}
}

// TestHealthShapeInvariant pins the public-surface property: whatever the
// ping outcome, the body stays free of every host/model/key string from the
// config snapshot. The DB ping runs against a fast-failing pool (no DB in
// unit tests), so overall status is "unhealthy"/503 in both cases — the
// per-role results still split the backend ping success and error paths.
func TestHealthShapeInvariant(t *testing.T) {
	pool := brokenPool(t)

	t.Run("backends up", func(t *testing.T) {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(backend.Close)

		cfg := healthTestConfig(backend.URL, backend.URL, backend.URL)
		st := &swapStore{}
		st.p.Store(cfg)

		rec := httptest.NewRecorder()
		NewHealthHandler(pool, st, healthTestPool(backend.URL, backend.URL, backend.URL), nil).Health(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503 (DB down in unit test)", rec.Code)
		}
		assertHealthBody(t, rec.Body.Bytes(), append(healthNeedles(cfg), poolNameNeedles...))

		var resp struct {
			Services map[string]string `json:"services"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		for _, role := range []string{"embed", "chat", "dream"} {
			if resp.Services[role] != "ok" {
				t.Errorf("service %s = %q, want ok", role, resp.Services[role])
			}
		}
	})

	t.Run("backends down", func(t *testing.T) {
		down := closedPortHost(t)
		cfg := healthTestConfig(down, down, down)
		st := &swapStore{}
		st.p.Store(cfg)

		rec := httptest.NewRecorder()
		NewHealthHandler(pool, st, healthTestPool(down, down, down), nil).Health(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", rec.Code)
		}
		assertHealthBody(t, rec.Body.Bytes(), append(healthNeedles(cfg), poolNameNeedles...))

		var resp struct {
			Status   string            `json:"status"`
			Services map[string]string `json:"services"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		if resp.Status != "unhealthy" {
			t.Errorf("status = %q, want unhealthy", resp.Status)
		}
		for _, role := range []string{"database", "embed", "chat", "dream"} {
			if resp.Services[role] != "error" {
				t.Errorf("service %s = %q, want error", role, resp.Services[role])
			}
		}
	})
}

// TestHealthPingsSnapshotTargets pins the per-request snapshot property in
// its F3-P2 form: the ping targets come from the POOL snapshot taken per
// request. After a pool reload (here: a seeded swap — exactly what Reload
// publishes), the next health check probes the new backends; a boot copy
// would keep hitting generation A and fail this test.
func TestHealthPingsSnapshotTargets(t *testing.T) {
	pool := brokenPool(t)

	var hitsA, hitsB atomic.Int32
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hitsA.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srvA.Close)
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hitsB.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srvB.Close)

	st := &swapStore{}
	st.p.Store(healthTestConfig(srvA.URL, srvA.URL, srvA.URL))
	bp := healthTestPool(srvA.URL, srvA.URL, srvA.URL)
	h := NewHealthHandler(pool, st, bp, nil)

	h.Health(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/health", nil))
	if got := hitsA.Load(); got != 3 {
		t.Errorf("generation A: %d pings, want 3 (embed+chat+dream)", got)
	}
	if got := hitsB.Load(); got != 0 {
		t.Errorf("generation B pinged before the swap: %d", got)
	}

	bp.SeedSnapshotForTest(healthTestPool(srvB.URL, srvB.URL, srvB.URL).Snapshot())

	h.Health(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/health", nil))
	if got := hitsB.Load(); got != 3 {
		t.Errorf("generation B: %d pings after swap, want 3", got)
	}
	if got := hitsA.Load(); got != 3 {
		t.Errorf("generation A pinged after the swap: %d (boot copy?)", got-3)
	}
}
