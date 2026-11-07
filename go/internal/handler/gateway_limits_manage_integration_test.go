//go:build integration

package handler

// Gates of wave W06-15 on the manage actions gateway-limits-get|set|delete
// (design/06 §4.3, §5 B-06-5, B-06-19, B-06-20, §7 W06-15 (a)–(f);
// masterplan §3.1 "Tenant-Zeile weitet ⇒ 400"; dispatch C1):
//
//	go test -tags=integration -p 1 -run 'TestGatewayLimitsManage' ./internal/handler/ -count=1 -v

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/gatewaylimits"
	"github.com/GottZ/ctx/internal/keyquota"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// glFloor is the settings floor of every op in these tests.
var glFloor = gatewaylimits.Defaults{RPM: 30, TPM: 60000, Concurrency: 2, DailyTokens: 2000000, MonthlyCostUSD: 1,
	TenantEnvelopeFactor: 5, InstanceEnvelopeFactor: 10, ConcurrencyCap: 8}

// glFixture: a handler with gateway and self-service on and the limits
// registry over the pool, a server-admin, tenants A and B with an owner key
// each, a consumer in each.
type glFixture struct {
	pool   *pgxpool.Pool
	h      *ManageHandler
	reg    *gatewaylimits.Registry
	admin  *auth.AuthResult
	a, b   auTenant
	cA, cB mcConsumer
}

func newGLFixture(t *testing.T) glFixture {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	h := mcHandler(t, pool, true, true)
	reg := gatewaylimits.NewRegistry(func(ctx context.Context) ([]gatewaylimits.Row, error) {
		return store.LoadGatewayLimits(ctx, pool)
	})
	h.SetGatewayLimits(reg, func(keyquota.Op) gatewaylimits.Defaults { return glFloor })
	admin, _ := kaServerAdmin(ctx, t, pool)
	a := auMakeTenant(ctx, t, pool, h, admin, "gl-a")
	b := auMakeTenant(ctx, t, pool, h, admin, "gl-b")
	return glFixture{pool: pool, h: h, reg: reg, admin: admin, a: a, b: b,
		cA: mcCreate(t, h, admin, map[string]any{"label": "gl-a-app", "tenant_id": a.id}),
		cB: mcCreate(t, h, admin, map[string]any{"label": "gl-b-app", "tenant_id": b.id})}
}

// set drives gateway-limits-set and returns status, body and param.
func (f glFixture) set(t *testing.T, ar *auth.AuthResult, data map[string]any) (int, string, string) {
	t.Helper()
	status, body, resp := mcCall(t, f.h, ar, "gateway-limits-set", data)
	param, _ := resp["param"].(string)
	return status, string(body), param
}

func glData(kind, idField, id, op string, dims map[string]any) map[string]any {
	d := map[string]any{"subject_kind": kind, "op": op}
	if idField != "" {
		d[idField] = id
	}
	for k, v := range dims {
		d[k] = v
	}
	return d
}

// TestGatewayLimitsManageNarrowing is gate (a) and the dispatch's
// "Verengung": a tenant row that widens the instance default ⇒ 400 with the
// field; a tenant-admin consumer row above the tenant default ⇒ 400; a
// missing server row means the setting's value (05 V-06-e), never unlimited;
// at or under the ceiling ⇒ 200; a server-admin is free; a tenant-tier
// permission (B-06-20) and a 0 value ⇒ 400 with the field.
func TestGatewayLimitsManageNarrowing(t *testing.T) {
	f := newGLFixture(t)
	steps := []struct {
		name      string
		ar        *auth.AuthResult
		data      map[string]any
		wantCode  int
		wantParam string
	}{
		{"server-admin instance rpm 20", f.admin, glData("instance", "", "", "chat", map[string]any{"rpm": 20}), http.StatusOK, ""},
		{"tenant row widens the instance default", f.a.ar, glData("tenant", "tenant_id", f.a.id, "chat", map[string]any{"rpm": 21}), http.StatusBadRequest, "rpm"},
		{"tenant row at the instance default", f.a.ar, glData("tenant", "tenant_id", f.a.id, "chat", map[string]any{"rpm": 20}), http.StatusOK, ""},
		{"consumer row above the tenant default", f.a.ar, glData("consumer", "consumer_id", f.cA.id, "chat", map[string]any{"rpm": 21}), http.StatusBadRequest, "rpm"},
		{"consumer row above the setting (no server row)", f.a.ar, glData("consumer", "consumer_id", f.cA.id, "chat", map[string]any{"daily_tokens": 2000001}), http.StatusBadRequest, "daily_tokens"},
		{"consumer row narrows", f.a.ar, glData("consumer", "consumer_id", f.cA.id, "chat", map[string]any{"rpm": 10, "daily_tokens": 1000}), http.StatusOK, ""},
		{"tenant-tier allow_external true", f.a.ar, glData("consumer", "consumer_id", f.cA.id, "embed", map[string]any{"allow_external": true}), http.StatusBadRequest, "allow_external"},
		{"tenant-tier allow_unmetered_external", f.a.ar, glData("consumer", "consumer_id", f.cA.id, "embed", map[string]any{"allow_unmetered_external": false}), http.StatusBadRequest, "allow_unmetered_external"},
		{"tenant-tier withdraws allow_external", f.a.ar, glData("consumer", "consumer_id", f.cA.id, "embed", map[string]any{"allow_external": false}), http.StatusOK, ""},
		{"zero rpm (no 0 sentinel)", f.admin, glData("consumer", "consumer_id", f.cA.id, "rerank", map[string]any{"rpm": 0}), http.StatusBadRequest, "rpm"},
		{"zero cost (B-06-20)", f.admin, glData("consumer", "consumer_id", f.cA.id, "rerank", map[string]any{"monthly_cost_usd": 0}), http.StatusBadRequest, "monthly_cost_usd"},
		{"concurrency over the V48 cap", f.admin, glData("consumer", "consumer_id", f.cA.id, "rerank", map[string]any{"max_concurrency": 9}), http.StatusBadRequest, "max_concurrency"},
		{"server-admin free above everything", f.admin, glData("consumer", "consumer_id", f.cA.id, "chat", map[string]any{"rpm": 500, "allow_external": true}), http.StatusOK, ""},
		{"server-admin tenant row widens the instance default", f.admin, glData("tenant", "tenant_id", f.b.id, "chat", map[string]any{"rpm": 50}), http.StatusOK, ""},
		{"envelope with rpm", f.admin, glData("tenant_envelope", "tenant_id", f.a.id, "chat", map[string]any{"rpm": 1}), http.StatusBadRequest, "rpm"},
		{"unknown op", f.admin, glData("consumer", "consumer_id", f.cA.id, "images", nil), http.StatusBadRequest, "op"},
		{"unknown field", f.admin, glData("consumer", "consumer_id", f.cA.id, "chat", map[string]any{"tier": "server"}), http.StatusBadRequest, ""},
	}
	for _, s := range steps {
		code, body, param := f.set(t, s.ar, s.data)
		if code != s.wantCode || (s.wantParam != "" && param != s.wantParam) {
			t.Errorf("%s: %d param %q body %s, want %d param %q", s.name, code, param, body, s.wantCode, s.wantParam)
		}
	}
}

// TestGatewayLimitsManageTiers: the table tier (a member key ⇒ 403) and the
// field tiers (a tenant-admin on instance or tenant_envelope ⇒ 403, read
// and write).
func TestGatewayLimitsManageTiers(t *testing.T) {
	f := newGLFixture(t)
	member := mcMember(context.Background(), t, f.pool, f.h, f.a)
	cases := []struct {
		name   string
		ar     *auth.AuthResult
		action string
		data   map[string]any
	}{
		{"member get", member, "gateway-limits-get", glData("consumer", "consumer_id", f.cA.id, "chat", nil)},
		{"member set", member, "gateway-limits-set", glData("consumer", "consumer_id", f.cA.id, "chat", map[string]any{"rpm": 1})},
		{"tenant-admin set instance", f.a.ar, "gateway-limits-set", glData("instance", "", "", "chat", map[string]any{"rpm": 1})},
		{"tenant-admin get instance", f.a.ar, "gateway-limits-get", glData("instance", "", "", "chat", nil)},
		{"tenant-admin delete instance", f.a.ar, "gateway-limits-delete", glData("instance", "", "", "chat", nil)},
		{"tenant-admin set own envelope", f.a.ar, "gateway-limits-set", glData("tenant_envelope", "tenant_id", f.a.id, "chat", map[string]any{"daily_tokens": 1})},
	}
	for _, c := range cases {
		status, body, _ := mcCall(t, f.h, c.ar, c.action, c.data)
		if status != http.StatusForbidden {
			t.Errorf("%s: %d %s, want 403", c.name, status, body)
		}
	}
}

// TestGatewayLimitsManageNoOracle is gate (d): another tenant's consumer, an
// unknown and a malformed id answer byte-identically (404), for set, get
// and delete; the same for another tenant's and an unknown tenant.
func TestGatewayLimitsManageNoOracle(t *testing.T) {
	f := newGLFixture(t)
	for _, action := range []string{"gateway-limits-get", "gateway-limits-set", "gateway-limits-delete"} {
		var bodies [][]byte
		for _, id := range []string{f.cB.id, "00000000-0000-7000-8000-000000000000", "not-a-uuid"} {
			status, body, _ := mcCall(t, f.h, f.a.ar, action, glData("consumer", "consumer_id", id, "chat", map[string]any{"rpm": 1}))
			if status != http.StatusNotFound {
				t.Errorf("%s consumer %s: %d %s, want 404", action, id, status, body)
			}
			bodies = append(bodies, body)
		}
		if !bytes.Equal(bodies[0], bodies[1]) || !bytes.Equal(bodies[1], bodies[2]) {
			t.Errorf("%s: foreign/unknown/malformed consumer bodies differ: %q %q %q", action, bodies[0], bodies[1], bodies[2])
		}
		s1, b1, _ := mcCall(t, f.h, f.a.ar, action, glData("tenant", "tenant_id", f.b.id, "chat", map[string]any{"rpm": 1}))
		s2, b2, _ := mcCall(t, f.h, f.a.ar, action, glData("tenant", "tenant_id", "00000000-0000-7000-8000-000000000001", "chat", map[string]any{"rpm": 1}))
		if s1 != http.StatusNotFound || s2 != http.StatusNotFound || !bytes.Equal(b1, b2) {
			t.Errorf("%s foreign/unknown tenant: %d %s / %d %s, want two identical 404", action, s1, b1, s2, b2)
		}
	}
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM context_gateway_limits`).Scan(&n); err != nil || n != 0 {
		t.Errorf("rows after the refused writes: %d (%v), want 0", n, err)
	}
}

// TestGatewayLimitsManageTenantDefaultInherited is gate (c) (B-06-19): the
// server-admin throttles tenant B (tenant row rpm 1); a consumer B creates
// through self-service afterwards resolves to rpm 1, and B's tenant-admin
// cannot lift the throttle with a tenant-tier row.
func TestGatewayLimitsManageTenantDefaultInherited(t *testing.T) {
	f := newGLFixture(t)
	if code, body, _ := f.set(t, f.admin, glData("tenant", "tenant_id", f.b.id, "chat", map[string]any{"rpm": 1})); code != http.StatusOK {
		t.Fatalf("server-admin throttle: %d %s", code, body)
	}
	fresh := mcCreate(t, f.h, f.b.ar, map[string]any{"label": "gl-b-selfservice"})
	if got := f.reg.Snapshot().Resolve(fresh.id, f.b.id, keyquota.OpChat, glFloor).RPM; got != 1 {
		t.Errorf("self-service consumer of the throttled tenant: effective rpm %d, want the tenant default 1", got)
	}
	status, body, resp := mcCall(t, f.h, f.b.ar, "gateway-limits-get", glData("consumer", "consumer_id", fresh.id, "chat", nil))
	if status != http.StatusOK {
		t.Fatalf("get: %d %s", status, body)
	}
	limits, _ := resp["limits"].(map[string]any)
	ops, _ := limits["ops"].(map[string]any)
	chat, _ := ops["chat"].(map[string]any)
	eff, _ := chat["effective"].(map[string]any)
	if rpm, _ := eff["rpm"].(float64); rpm != 1 {
		t.Errorf("gateway-limits-get effective rpm %v, want 1 (body %s)", eff["rpm"], body)
	}
	if code, body, param := f.set(t, f.b.ar, glData("tenant", "tenant_id", f.b.id, "chat", map[string]any{"rpm": 30})); code != http.StatusBadRequest || param != "rpm" {
		t.Errorf("tenant-admin lifts the server throttle: %d param %q %s, want 400 rpm", code, param, body)
	}
}

// TestGatewayLimitsManageAuditAndReload is gate (e): a server-admin write
// applies without a restart (the registry reloads before the answer) and
// lands one audit row keyed by the consumer id; an unchanged write lands
// none; a read lands none; a delete lands one and the dimension inherits
// again.
func TestGatewayLimitsManageAuditAndReload(t *testing.T) {
	f := newGLFixture(t)
	ctx := context.Background()
	audit := func() int {
		var n int
		if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM context_settings_audit WHERE entity_type = 'gateway_limits' AND entity_key = $1`, f.cA.id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	write := glData("consumer", "consumer_id", f.cA.id, "chat", map[string]any{"rpm": 7})
	if code, body, _ := f.set(t, f.admin, write); code != http.StatusOK {
		t.Fatalf("set: %d %s", code, body)
	}
	if got := f.reg.Snapshot().Resolve(f.cA.id, f.a.id, keyquota.OpChat, glFloor).RPM; got != 7 {
		t.Errorf("right after the answer: registry rpm %d, want 7 (synchronous reload)", got)
	}
	if n := audit(); n != 1 {
		t.Errorf("audit rows after the set: %d, want 1", n)
	}
	if code, body, _ := f.set(t, f.admin, write); code != http.StatusOK {
		t.Fatalf("unchanged set: %d %s", code, body)
	}
	if n := audit(); n != 1 {
		t.Errorf("audit rows after an unchanged set: %d, want still 1", n)
	}
	if status, body, _ := mcCall(t, f.h, f.admin, "gateway-limits-get", glData("consumer", "consumer_id", f.cA.id, "", nil)); status != http.StatusOK {
		t.Errorf("get: %d %s", status, body)
	}
	if n := audit(); n != 1 {
		t.Errorf("audit rows after a get: %d, want still 1", n)
	}
	status, body, resp := mcCall(t, f.h, f.admin, "gateway-limits-delete", glData("consumer", "consumer_id", f.cA.id, "chat", nil))
	if deleted, _ := resp["deleted"].(bool); status != http.StatusOK || !deleted {
		t.Fatalf("delete: %d %s, want deleted", status, body)
	}
	if n := audit(); n != 2 {
		t.Errorf("audit rows after the delete: %d, want 2", n)
	}
	if got := f.reg.Snapshot().Resolve(f.cA.id, f.a.id, keyquota.OpChat, glFloor).RPM; got != glFloor.RPM {
		t.Errorf("after the delete: registry rpm %d, want the floor %d", got, glFloor.RPM)
	}
}

// TestGatewayLimitsManageGetShape: the get answer per op carries the stored
// rows, the ceiling, the effective values with their sources; a
// tenant-admin sees its tenant envelope but never the instance envelope; a
// get without op answers all three ops.
func TestGatewayLimitsManageGetShape(t *testing.T) {
	f := newGLFixture(t)
	if code, body, _ := f.set(t, f.admin, glData("consumer", "consumer_id", f.cA.id, "chat", map[string]any{"rpm": 12})); code != http.StatusOK {
		t.Fatalf("set: %d %s", code, body)
	}
	status, body, resp := mcCall(t, f.h, f.a.ar, "gateway-limits-get", glData("consumer", "consumer_id", f.cA.id, "", nil))
	if status != http.StatusOK {
		t.Fatalf("get: %d %s", status, body)
	}
	limits, _ := resp["limits"].(map[string]any)
	ops, _ := limits["ops"].(map[string]any)
	if len(ops) != 3 {
		t.Fatalf("ops %v, want chat, embed, rerank (body %s)", ops, body)
	}
	chat, _ := ops["chat"].(map[string]any)
	server, _ := chat["server"].(map[string]any)
	if rpm, _ := server["rpm"].(float64); rpm != 12 || chat["tenant"] != nil {
		t.Errorf("chat server %v tenant %v, want the stored rpm 12 and no tenant row", chat["server"], chat["tenant"])
	}
	eff, _ := chat["effective"].(map[string]any)
	if eff["rpm"] != float64(12) || eff["instance_envelope"] != nil || eff["tenant_envelope"] == nil {
		t.Errorf("tenant-admin effective %v, want rpm 12, a tenant envelope, no instance envelope", eff)
	}
	sources, _ := chat["sources"].(map[string]any)
	if sources["rpm"] != string(gatewaylimits.SourceConsumerServer) || sources["tpm"] != string(gatewaylimits.SourceSettings) {
		t.Errorf("sources %v, want rpm consumer_server, tpm settings", sources)
	}
	if _, ok := chat["ceiling"].(map[string]any); !ok {
		t.Errorf("chat without a ceiling: %v", chat)
	}
}
