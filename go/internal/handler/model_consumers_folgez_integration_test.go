//go:build integration

package handler

// FOLGE-Z Z3 gates: model-consumer-create takes optional starting limits per
// op, writes them as the consumer's row of the CALLER's tier in the create's
// own transaction (a refused limit leaves no consumer and no key behind),
// and both create and get answer with effective_limits per op — the values
// the meter applies, each dimension with its source — so no default stays
// hidden. admission_lane stays where it is (consumer.admission_lane).

import (
	"context"
	"net/http"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/gatewaylimits"
)

// fzCount counts rows of a query with one argument.
func fzCount(t *testing.T, f glFixture, sql string, arg any) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), sql, arg).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", sql, err)
	}
	return n
}

// fzEffective returns effective_limits[op] of an answer.
func fzEffective(t *testing.T, resp map[string]any, op string) map[string]any {
	t.Helper()
	all, ok := resp["effective_limits"].(map[string]any)
	if !ok {
		t.Fatalf("Z3: answer without effective_limits: %v", resp)
	}
	e, ok := all[op].(map[string]any)
	if !ok {
		t.Fatalf("Z3: effective_limits without op %s: %v", op, all)
	}
	return e
}

func fzSource(e map[string]any, dim string) string {
	s, _ := e["sources"].(map[string]any)
	v, _ := s[dim].(string)
	return v
}

// TestConsumerCreateLimitsServerAdmin: a server-admin's limits become the
// consumer's server-tier row; the answer's effective_limits carry them with
// source consumer_server and the settings floor for the rest; get answers
// the same effective_limits.
func TestConsumerCreateLimitsServerAdmin(t *testing.T) {
	f := newGLFixture(t)
	status, body, resp := mcCall(t, f.h, f.admin, "model-consumer-create", map[string]any{
		"label": "fz-app", "tenant_id": f.a.id,
		"limits": map[string]any{"chat": map[string]any{"max_concurrency": 8, "rpm": 120}},
	})
	if status != http.StatusOK {
		t.Fatalf("Z3 create with limits: %d %s", status, body)
	}
	c, _ := resp["consumer"].(map[string]any)
	id, _ := c["id"].(string)
	if lane, _ := c["admission_lane"].(string); lane != "bulk" {
		t.Errorf("Z3: consumer.admission_lane %q, want the visible default bulk", lane)
	}
	if n := fzCount(t, f, `SELECT count(*) FROM context_gateway_limits WHERE consumer_id = $1::uuid AND tier = 'server' AND op = 'chat' AND max_concurrency = 8 AND rpm = 120`, id); n != 1 {
		t.Errorf("Z3: %d server-tier chat rows with the given limits, want 1", n)
	}
	chat := fzEffective(t, resp, "chat")
	if chat["max_concurrency"] != float64(8) || chat["rpm"] != float64(120) || chat["tpm"] != float64(glFloor.TPM) {
		t.Errorf("Z3: effective chat %v, want max_concurrency 8, rpm 120, tpm %d (settings)", chat, glFloor.TPM)
	}
	if fzSource(chat, "max_concurrency") != string(gatewaylimits.SourceConsumerServer) || fzSource(chat, "tpm") != string(gatewaylimits.SourceSettings) {
		t.Errorf("Z3: effective chat sources %v, want max_concurrency consumer_server, tpm settings", chat["sources"])
	}
	embed := fzEffective(t, resp, "embed")
	if embed["max_concurrency"] != float64(glFloor.Concurrency) || fzSource(embed, "max_concurrency") != string(gatewaylimits.SourceSettings) {
		t.Errorf("Z3: effective embed %v, want the settings floor %d with source settings", embed, glFloor.Concurrency)
	}
	_ = fzEffective(t, resp, "rerank")
	// The meter applies the row from the next request on: the registry was
	// reloaded before the answer, not only the answer's overlay.
	if eff := f.reg.Snapshot().Resolve(id, f.a.id, "chat", glFloor); eff.Concurrency != 8 || eff.RPM != 120 {
		t.Errorf("Z3: the limits registry after the create resolves concurrency %d rpm %d, want 8/120 (reloaded before the answer)", eff.Concurrency, eff.RPM)
	}

	status, body, got := mcCall(t, f.h, f.admin, "model-consumer-get", map[string]any{"id": id})
	if status != http.StatusOK {
		t.Fatalf("Z3 get: %d %s", status, body)
	}
	gchat := fzEffective(t, got, "chat")
	if gchat["max_concurrency"] != float64(8) || fzSource(gchat, "rpm") != string(gatewaylimits.SourceConsumerServer) {
		t.Errorf("Z3: get effective chat %v, want the create's limits", gchat)
	}
	// §3 amendment: the bulk lane's concurrency, the meter's share of 8 at
	// the default gateway.xu_reserve_ratio 0.25 — in create and get alike.
	if chat["bulk_max_concurrency"] != float64(6) || gchat["bulk_max_concurrency"] != float64(6) {
		t.Errorf("Z3: bulk_max_concurrency create %v get %v, want 6 (⌊8·0.75⌋)", chat["bulk_max_concurrency"], gchat["bulk_max_concurrency"])
	}
	if embed["bulk_max_concurrency"] != float64(1) {
		t.Errorf("Z3: embed bulk_max_concurrency %v, want 1 (settings floor 2 at r 0.25)", embed["bulk_max_concurrency"])
	}
}

// TestConsumerCreateLimitsTenantAdmin: a tenant-admin's limits become the
// tenant-tier row; a value above its ceiling is refused like
// gateway-limits-set (400, limit widens, param = limits.<op>.<field>) and
// the refusal leaves neither consumer nor key.
func TestConsumerCreateLimitsTenantAdmin(t *testing.T) {
	f := newGLFixture(t)
	status, body, resp := mcCall(t, f.h, f.a.ar, "model-consumer-create", map[string]any{
		"label": "fz-tenant", "limits": map[string]any{"chat": map[string]any{"max_concurrency": 1}},
	})
	if status != http.StatusOK {
		t.Fatalf("Z3 tenant-admin create with limits: %d %s", status, body)
	}
	c, _ := resp["consumer"].(map[string]any)
	id, _ := c["id"].(string)
	if n := fzCount(t, f, `SELECT count(*) FROM context_gateway_limits WHERE consumer_id = $1::uuid AND tier = 'tenant' AND op = 'chat' AND max_concurrency = 1`, id); n != 1 {
		t.Errorf("Z3: %d tenant-tier chat rows, want 1", n)
	}
	if chat := fzEffective(t, resp, "chat"); chat["max_concurrency"] != float64(1) || fzSource(chat, "max_concurrency") != string(gatewaylimits.SourceConsumerTenant) {
		t.Errorf("Z3: tenant-admin effective chat %v, want max_concurrency 1 from consumer_tenant", chat)
	}

	for _, tc := range []struct {
		name   string
		limits map[string]any
		param  string
	}{
		{"rpm above the settings floor", map[string]any{"chat": map[string]any{"rpm": glFloor.RPM + 1}}, "limits.chat.rpm"},
		{"allow_external true", map[string]any{"embed": map[string]any{"allow_external": true}}, "limits.embed.allow_external"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			label := "fz-refused-" + tc.param
			status, body, resp := mcCall(t, f.h, f.a.ar, "model-consumer-create", map[string]any{"label": label, "limits": tc.limits})
			if status != http.StatusBadRequest {
				t.Fatalf("Z3 widening create: %d %s, want 400", status, body)
			}
			if p, _ := resp["param"].(string); p != tc.param {
				t.Errorf("Z3 widening create: param %q, want %q (%s)", p, tc.param, body)
			}
			if e, _ := resp["error"].(string); len(e) < 12 || e[:12] != "limit widens" {
				t.Errorf("Z3 widening create: error %q, want the gateway-limits-set wording \"limit widens: …\"", e)
			}
			if n := fzCount(t, f, `SELECT count(*) FROM context_model_consumers WHERE label = $1`, label); n != 0 {
				t.Errorf("Z3 widening create: %d consumers named %q, want none", n, label)
			}
		})
	}
}

// TestConsumerCreateLimitsInvalid: shape errors are 400 with the path as
// param before anything is written; an unknown op is an unknown field.
func TestConsumerCreateLimitsInvalid(t *testing.T) {
	f := newGLFixture(t)
	keysBefore := fzCount(t, f, `SELECT count(*) FROM context_model_keys WHERE consumer_id IS NOT NULL AND $1::int = 0`, 0)
	for _, tc := range []struct {
		name   string
		limits any
		param  string
	}{
		{"zero is no sentinel", map[string]any{"embed": map[string]any{"rpm": 0}}, "limits.embed.rpm"},
		{"concurrency above the cap", map[string]any{"chat": map[string]any{"max_concurrency": glFloor.ConcurrencyCap + 1}}, "limits.chat.max_concurrency"},
		{"negative cost", map[string]any{"rerank": map[string]any{"monthly_cost_usd": -1}}, "limits.rerank.monthly_cost_usd"},
		{"unknown op", map[string]any{"vision": map[string]any{"rpm": 1}}, ""},
		{"unknown dimension", map[string]any{"chat": map[string]any{"burst": 1}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			label := "fz-invalid-" + tc.name
			status, body, resp := mcCall(t, f.h, f.admin, "model-consumer-create", map[string]any{"label": label, "tenant_id": f.a.id, "limits": tc.limits})
			if status != http.StatusBadRequest {
				t.Fatalf("Z3 invalid limits: %d %s, want 400", status, body)
			}
			if tc.param != "" {
				if p, _ := resp["param"].(string); p != tc.param {
					t.Errorf("Z3 invalid limits: param %q, want %q (%s)", p, tc.param, body)
				}
			}
			if n := fzCount(t, f, `SELECT count(*) FROM context_model_consumers WHERE label = $1`, label); n != 0 {
				t.Errorf("Z3 invalid limits: %d consumers named %q, want none", n, label)
			}
		})
	}
	if keysAfter := fzCount(t, f, `SELECT count(*) FROM context_model_keys WHERE consumer_id IS NOT NULL AND $1::int = 0`, 0); keysAfter != keysBefore {
		t.Errorf("Z3 invalid limits: model keys %d → %d, want no key minted", keysBefore, keysAfter)
	}
}

// TestConsumerCreateWithoutLimitsShowsEffective: without limits nothing is
// written, and the answer still names every op's effective values — the
// settings floor, each with its source.
func TestConsumerCreateWithoutLimitsShowsEffective(t *testing.T) {
	f := newGLFixture(t)
	status, body, resp := mcCall(t, f.h, f.admin, "model-consumer-create", map[string]any{"label": "fz-plain", "tenant_id": f.a.id})
	if status != http.StatusOK {
		t.Fatalf("Z3 plain create: %d %s", status, body)
	}
	c, _ := resp["consumer"].(map[string]any)
	id, _ := c["id"].(string)
	if n := fzCount(t, f, `SELECT count(*) FROM context_gateway_limits WHERE consumer_id = $1::uuid`, id); n != 0 {
		t.Errorf("Z3 plain create: %d limits rows, want none", n)
	}
	for _, op := range []string{"chat", "embed", "rerank"} {
		e := fzEffective(t, resp, op)
		if e["rpm"] != float64(glFloor.RPM) || e["max_concurrency"] != float64(glFloor.Concurrency) || fzSource(e, "rpm") != string(gatewaylimits.SourceSettings) {
			t.Errorf("Z3 plain create %s: effective %v, want the settings floor (rpm %d, concurrency %d) with source settings", op, e, glFloor.RPM, glFloor.Concurrency)
		}
		if _, ok := e["daily_tokens"]; !ok {
			t.Errorf("Z3 plain create %s: effective without daily_tokens (null when unbounded, never absent): %v", op, e)
		}
	}
}

// TestModelAliasListEffectiveVision (Z2 exposition): model-alias-list names
// each alias's effective vision and its source — policy when set, else the
// probe answer of the chain's rows (backend/none).
func TestModelAliasListEffectiveVision(t *testing.T) {
	m := setupModelAliasHarness(t)
	capable, incapable := fzSGLang(t, true), fzSGLang(t, false)
	row := func(id, host, role string) backends.Backend {
		return backends.Backend{ID: id, Name: id, Host: host, Protocol: backends.ProtocolOpenAI, ProviderClass: backends.ProviderLlamaCpp,
			Scope: backends.GlobalScope, Trust: backends.TrustFull, Roles: []string{role}, Enabled: true,
			ModelMap: map[string]backends.ModelSpec{"default": {Model: "m"}}}
	}
	m.h.backendPool.SeedSnapshotForTest([]backends.Backend{row("fz-cap", capable.URL, "fzvis"), row("fz-nocap", incapable.URL, "fzno")})
	m.h.backendPool.ProbeCapabilities(context.Background())
	m.seed(backends.GlobalScope, "fz-vis", "chat", "fzvis", "credentials", `{}`)
	m.seed(backends.GlobalScope, "fz-novis", "chat", "fzno", "credentials", `{}`)
	m.seed(backends.GlobalScope, "fz-forced", "chat", "fzno", "credentials", `{"vision":true}`)
	m.seed(backends.GlobalScope, "fz-off", "chat", "fzvis", "credentials", `{"vision":false}`)
	status, out, body := m.do(m.admin, "model-alias-list", "", nil)
	if status != http.StatusOK {
		t.Fatalf("Z2 model-alias-list: %d %s", status, body)
	}
	items, _ := out["model_aliases"].([]any)
	got := map[string][2]any{}
	for _, it := range items {
		a, _ := it.(map[string]any)
		name, _ := a["name"].(string)
		got[name] = [2]any{a["effective_vision"], a["vision_source"]}
	}
	for name, want := range map[string][2]any{
		"fz-vis": {true, "backend"}, "fz-novis": {false, "none"}, "fz-forced": {true, "policy"}, "fz-off": {false, "policy"},
	} {
		if got[name] != want {
			t.Errorf("Z2 model-alias-list %s: effective_vision/vision_source %v, want %v", name, got[name], want)
		}
	}
}
