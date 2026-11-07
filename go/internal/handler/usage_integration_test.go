//go:build integration

// W06-16 gates at the handler (RC-D S1, cluster C2): GET /api/usage and
// GET /api/usage/consumers (K18) and the status sections gateway /
// usage_split_24h / federation.
//
//	go test -tags=integration -p 1 -count=1 -run 'TestUsage|TestStatusGateway' -v ./internal/handler/
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/llmusage"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/internal/usageevents"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// usageStatements records the text of every statement of a pool.
type usageStatements struct {
	mu  sync.Mutex
	sql []string
}

func (s *usageStatements) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	s.mu.Lock()
	s.sql = append(s.sql, d.SQL)
	s.mu.Unlock()
	return ctx
}

func (*usageStatements) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// count returns the statements since mark that mention every needle.
func (s *usageStatements) count(mark int, needles ...string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, q := range s.sql[mark:] {
		hit := true
		for _, needle := range needles {
			hit = hit && strings.Contains(q, needle)
		}
		if hit {
			n++
		}
	}
	return n
}

func (s *usageStatements) mark() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.sql) }

func usageTracedPool(t *testing.T, dsn string) (*pgxpool.Pool, *usageStatements) {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	rec := &usageStatements{}
	cfg.ConnConfig.Tracer = rec
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool, rec
}

// usageFixture: tenants A and B, consumers a1, a2 (A) and b1 (B), rollup
// rows for each in the last hours plus an internal row, a capacity refusal
// of a1 with target and dispatch class, an auth refusal without consumer.
type usageFixture struct {
	tenantA, tenantB string
	a1, a2, b1       string
	hour             time.Time
}

func seedUsageFixture(t *testing.T, pool *pgxpool.Pool) usageFixture {
	t.Helper()
	ctx := context.Background()
	f := usageFixture{tenantA: w0617Tenant(t, pool, "w0616-a"), tenantB: w0617Tenant(t, pool, "w0616-b")}
	f.a1 = w0617Consumer(t, pool, f.tenantA, "a1")
	f.a2 = w0617Consumer(t, pool, f.tenantA, "a2")
	f.b1 = w0617Consumer(t, pool, f.tenantB, "b1")
	f.hour = time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	for _, r := range []struct {
		consumer string
		calls    int
	}{{f.a1, 5}, {f.a2, 6}, {f.b1, 7}} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO context_llm_usage_hourly (bucket_start, consumer_id, pipeline, model_alias, backend_name, dispatch_class, calls, prompt_tokens)
			VALUES ($1, $2::uuid, 'gateway-chat', 'chat', 'spark', 'external_urgent', $3, 100),
			       ($4, $2::uuid, 'gateway-chat', 'chat', 'spark', 'external_urgent', 1, 1)`,
			f.hour, r.consumer, r.calls, f.hour.Add(-30*time.Hour)); err != nil {
			t.Fatalf("rollup: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO context_llm_usage_hourly (bucket_start, api_key_id, pipeline, calls) VALUES ($1, gen_random_uuid(), 'query', 11)`, f.hour); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE _migrations SET applied_at = $1 WHERE version = 167`, f.hour.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_llm_rejections_hourly (bucket_start, consumer_id, op, model_alias, dispatch_class, target, reason, detail, n)
		VALUES ($1, $2::uuid, 'chat', 'chat', 'external_urgent', 'http://spark:30000', 'capacity', 'server_busy', 4),
		       ($1, NULL, '', '', '', '', 'auth', 'invalid_key', 9)`, f.hour, f.a1); err != nil {
		t.Fatal(err)
	}
	return f
}

func serveUsage(t *testing.T, h http.HandlerFunc, path string, ar *auth.AuthResult) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req = req.WithContext(context.WithValue(req.Context(), authResultKey, ar))
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func tenantAdminOf(tenant string) *auth.AuthResult {
	return &auth.AuthResult{IsValid: true, TenantID: tenant, TenantRole: auth.RoleAdmin}
}

// TestUsageCrossTenantProbe is W06-16's cross-tenant probe (B-06-3) and
// Finding 24: tenant-admin A sees its own consumer's /v1 rows under
// ?consumer= (red against a filter over TenantAPIKeyIDs only), and a
// foreign or unknown consumer answers 200 with the same empty body (no
// oracle — red without the membership check).
func TestUsageCrossTenantProbe(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	f := seedUsageFixture(t, pool)
	h := NewUsageHandler(pool, nil, nil)
	window := "&from=" + f.hour.Add(-time.Hour).Format(time.RFC3339) + "&to=" + f.hour.Add(time.Hour).Format(time.RFC3339)
	own := serveUsage(t, h.HandleUsage, "/api/usage?bucket=hour&group=consumer&consumer="+f.a1+window, tenantAdminOf(f.tenantA))
	if own.Code != http.StatusOK || !strings.Contains(own.Body.String(), `"calls":5`) {
		t.Errorf("W06-16 gate (Finding 24): tenant-admin A, own consumer: %d %s — want 200 with its 5 calls", own.Code, own.Body.Bytes())
	}
	foreign := serveUsage(t, h.HandleUsage, "/api/usage?bucket=hour&group=consumer&consumer="+f.b1+window, tenantAdminOf(f.tenantA))
	unknown := serveUsage(t, h.HandleUsage, "/api/usage?bucket=hour&group=consumer&consumer=0195a6f0-0000-7000-8000-00000000dead"+window, tenantAdminOf(f.tenantA))
	if foreign.Code != http.StatusOK || strings.Contains(foreign.Body.String(), `"calls":7`) || strings.Contains(foreign.Body.String(), f.b1) {
		t.Errorf("W06-16 gate (cross-tenant): tenant-admin A, B's consumer: %d %s — want 200 without B's rows", foreign.Code, foreign.Body.Bytes())
	}
	if foreign.Code != unknown.Code || foreign.Body.String() != unknown.Body.String() {
		t.Errorf("W06-16 gate (no oracle): foreign and unknown consumer answer differently:\nforeign %d %s\nunknown %d %s",
			foreign.Code, foreign.Body.Bytes(), unknown.Code, unknown.Body.Bytes())
	}
	all := serveUsage(t, h.HandleUsage, "/api/usage?bucket=hour&group=consumer"+window, tenantAdminOf(f.tenantA))
	body := all.Body.String()
	if all.Code != http.StatusOK || !strings.Contains(body, f.a1) || !strings.Contains(body, f.a2) || strings.Contains(body, f.b1) || strings.Contains(body, `"calls":11`) {
		t.Errorf("W06-16 gate (tenant filter): tenant-admin A without ?consumer: %d %s — want a1 and a2, never b1 nor internal rows", all.Code, body)
	}
}

// TestUsageExposureTenantAdmin: refusal target/dispatch_class absent for a
// tenant-admin (keys, not just values), present for a server-admin; the
// auth refusal never reaches a tenant; group=instance ⇒ 403 for a
// tenant-admin; to − from > 92 d ⇒ 400 for both — red against a shared
// serialization.
func TestUsageExposureTenantAdmin(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	f := seedUsageFixture(t, pool)
	h := NewUsageHandler(pool, nil, nil)
	window := "&from=" + f.hour.Add(-time.Hour).Format(time.RFC3339) + "&to=" + f.hour.Add(time.Hour).Format(time.RFC3339)
	ten := serveUsage(t, h.HandleUsage, "/api/usage?bucket=hour&group=consumer"+window, tenantAdminOf(f.tenantA))
	if ten.Code != http.StatusOK || !strings.Contains(ten.Body.String(), `"server_busy"`) {
		t.Fatalf("W06-16 gate (exposure): tenant-admin series: %d %s — want 200 with a1's capacity refusal", ten.Code, ten.Body.Bytes())
	}
	for _, leak := range []string{`"target"`, `"dispatch_class"`, "spark:30000", "invalid_key"} {
		if strings.Contains(ten.Body.String(), leak) {
			t.Errorf("W06-16 gate (exposure): tenant-admin answer carries %s: %s", leak, ten.Body.Bytes())
		}
	}
	srv := serveUsage(t, h.HandleUsage, "/api/usage?bucket=hour&group=consumer"+window, &auth.AuthResult{IsValid: true, IsAdmin: true})
	if srv.Code != http.StatusOK || !strings.Contains(srv.Body.String(), "spark:30000") || !strings.Contains(srv.Body.String(), "external_urgent") {
		t.Errorf("W06-16 gate (exposure): server-admin answer lacks target/dispatch_class: %d %s", srv.Code, srv.Body.Bytes())
	}
	if rec := serveUsage(t, h.HandleUsage, "/api/usage?bucket=hour&group=instance"+window, tenantAdminOf(f.tenantA)); rec.Code != http.StatusForbidden {
		t.Errorf("W06-16 gate (group=instance server-admin only): tenant-admin got %d %s, want 403", rec.Code, rec.Body.Bytes())
	}
	long := "&from=2026-01-01T00:00:00Z&to=2026-04-03T01:00:00Z"
	if rec := serveUsage(t, h.HandleUsage, "/api/usage?bucket=day&group=consumer"+long, &auth.AuthResult{IsValid: true, IsAdmin: true}); rec.Code != http.StatusBadRequest {
		t.Errorf("W06-16 gate (92 d): to − from = 92 d 1 h gives %d %s, want 400", rec.Code, rec.Body.Bytes())
	}
}

// TestUsageConsumersGeneration is V-06-f + the consumers list contract: two
// polls of tenant-admin A within 30 s read the rollup once (statement
// recorder); the list holds a1 and a2 per op with usage_24h and binding,
// never b1; the meter is absent ⇒ live null, not 0.
func TestUsageConsumersGeneration(t *testing.T) {
	seed, dsn := testdb.SetupTestDBWithDSN(t)
	f := seedUsageFixture(t, seed)
	pool, rec := usageTracedPool(t, dsn)
	h := NewUsageHandler(pool, nil, nil)
	mark := rec.mark()
	first := serveUsage(t, h.HandleUsageConsumers, "/api/usage/consumers", tenantAdminOf(f.tenantA))
	second := serveUsage(t, h.HandleUsageConsumers, "/api/usage/consumers", tenantAdminOf(f.tenantA))
	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("W06-16 gate (consumers list): %d / %d %s", first.Code, second.Code, first.Body.Bytes())
	}
	if n := rec.count(mark, "context_llm_usage_hourly"); n != 1 {
		t.Errorf("W06-16 gate (V-06-f generation): two polls within %v read the rollup %d times, want 1", usageGenerationTTL, n)
	}
	var out struct {
		Consumers []map[string]json.RawMessage `json:"consumers"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v — %s", err, first.Body.Bytes())
	}
	seen := map[string]bool{}
	for _, c := range out.Consumers {
		var id string
		_ = json.Unmarshal(c["consumer_id"], &id)
		seen[id] = true
		for _, k := range []string{"op", "limits", "day", "month", "binding", "live", "usage_24h", "rejections_24h", "last_bucket"} {
			if _, ok := c[k]; !ok {
				t.Errorf("W06-16 gate (consumers shape): entry of %s lacks %q", id, k)
			}
		}
		if string(c["live"]) != "null" {
			t.Errorf("W06-16 gate (null ≠ 0): live = %s without a meter, want null", c["live"])
		}
	}
	if !seen[f.a1] || !seen[f.a2] || seen[f.b1] {
		t.Errorf("W06-16 gate (consumers list tenant filter): consumers %v, want a1 and a2 only", seen)
	}
}

// TestStatusGatewaySections is the status half of W06-16 (V-06-d, hub
// load): with a feed wired, the server-admin status carries gateway (with
// auth and the ring's top consumers), usage_split_24h and federation; two
// snapshots within 30 s read the consumers' counts once and never the raw
// log beyond its edge hour; the tenant-admin status carries gateway with its
// own consumers only and none of auth, instance_envelope, events,
// usage_split_24h, federation.
func TestStatusGatewaySections(t *testing.T) {
	seed, dsn := testdb.SetupTestDBWithDSN(t)
	f := seedUsageFixture(t, seed)
	pool, rec := usageTracedPool(t, dsn)
	col := NewStatusCollector(pool, backends.NewPool(nil, nil), fakeDreamMode{}, config.NewStore(&config.Config{}), nil, nil)
	ring := NewConsumerRing(nil)
	ring.Add(f.a1, f.tenantA, 100)
	ring.Add(f.b1, f.tenantB, 50)
	col.SetGatewayFeed(&GatewayFeed{
		Enabled:   func() bool { return true },
		DoorStats: func() ModelAuthStats { return ModelAuthStats{Accepted: 3} },
		Ring:      ring,
		Events:    func() usageevents.Stats { return usageevents.Stats{} },
		Split: func(now time.Time) ([]llmusage.SplitRow, error) {
			return llmusage.ReadSplit24h(context.Background(), pool, now)
		},
	})
	h := NewStatusHandler(col)
	mark := rec.mark()
	srv1 := serveUsage(t, h.HandleStatus, "/api/status", &auth.AuthResult{IsValid: true, IsAdmin: true})
	_ = serveUsage(t, h.HandleStatus, "/api/status", &auth.AuthResult{IsValid: true, IsAdmin: true})
	var s map[string]json.RawMessage
	if err := json.Unmarshal(srv1.Body.Bytes(), &s); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, k := range []string{"gateway", "usage_split_24h", "federation"} {
		if _, ok := s[k]; !ok {
			t.Errorf("W06-16 gate (status sections): server-admin status lacks %q", k)
		}
	}
	if g := string(s["gateway"]); !strings.Contains(g, `"auth"`) || !strings.Contains(g, f.a1) || !strings.Contains(g, `"consumers":3`) {
		t.Errorf("W06-16 gate (status gateway): server-admin gateway = %s, want auth, a1 in top_consumers_1h, consumers 3", g)
	}
	if n := rec.count(mark, "context_model_consumers"); n > 1 {
		t.Errorf("W06-16 gate (hub load, V-06-d): two snapshots within %v read context_model_consumers %d times, want ≤ 1", gatewayGenTTL, n)
	}
	ten := serveUsage(t, h.HandleStatus, "/api/status", &auth.AuthResult{IsValid: true, TenantID: f.tenantA, TenantRole: auth.RoleAdmin, HomeScope: "w0616-a"})
	var ts map[string]json.RawMessage
	_ = json.Unmarshal(ten.Body.Bytes(), &ts)
	g, ok := ts["gateway"]
	if !ok {
		t.Errorf("W06-16 gate (status gateway, tenant): tenant-admin status lacks gateway (own consumers)")
	}
	for _, leak := range []string{`"auth"`, `"instance_envelope"`, `"events"`, f.b1} {
		if strings.Contains(string(g), leak) {
			t.Errorf("W06-16 gate (tenant shape): tenant gateway carries %s: %s", leak, g)
		}
	}
	if ok && !strings.Contains(string(g), `"consumers":2`) {
		t.Errorf("W06-16 gate (tenant shape): tenant gateway = %s, want consumers 2 (a1, a2)", g)
	}
	for _, k := range []string{"usage_split_24h", "federation"} {
		if _, ok := ts[k]; ok {
			t.Errorf("W06-16 gate (exposure): tenant-admin status carries %q", k)
		}
	}
}
