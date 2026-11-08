package handler

// FOLGE-Z Z1 exposition gates: backend-list carries each row's probe result
// as `capabilities` ({vision, probed_at, source}, null = unknown) and the
// admin /api/status dispatch targets list the capabilities of the rows
// behind each origin. The probe runs for real against a fake SGLang engine
// (the production path, no seeded result).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/gatewaylimits"
	"github.com/GottZ/ctx/internal/keyquota"
)

func fzSGLang(t *testing.T, vision bool) *httptest.Server {
	t.Helper()
	body := `{"served_model_name":"qwen38-27b","has_image_understanding":false}`
	if vision {
		body = `{"served_model_name":"qwen38-27b","has_image_understanding":true}`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/get_model_info" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func fzProbedPool(t *testing.T, srvURL string) *backends.Pool {
	t.Helper()
	bp := backends.NewPool(nil, nil)
	bp.SeedSnapshotForTest([]backends.Backend{
		{ID: "spark", Name: "spark-chat", Host: srvURL, Protocol: backends.ProtocolOpenAI, ProviderClass: backends.ProviderLlamaCpp,
			Scope: backends.GlobalScope, Trust: backends.TrustFull, Roles: []string{backends.RoleChat}, Enabled: true,
			ModelMap: map[string]backends.ModelSpec{"default": {Model: "qwen38-27b"}}},
		{ID: "off", Name: "parked", Host: srvURL, Protocol: backends.ProtocolOpenAI, ProviderClass: backends.ProviderLlamaCpp,
			Scope: backends.GlobalScope, Trust: backends.TrustFull, Roles: []string{backends.RoleChat}, Enabled: false},
	})
	bp.ProbeCapabilities(context.Background())
	return bp
}

func TestBackendListCarriesCapabilities(t *testing.T) {
	bp := fzProbedPool(t, fzSGLang(t, true).URL)
	rec := manageReqWithPool(t, serverAdminScopedAR(), bp, map[string]any{"action": "backend-list"})
	if rec.Code != http.StatusOK {
		t.Fatalf("backend-list: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Backends []map[string]json.RawMessage `json:"backends"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	got := map[string]map[string]any{}
	for _, b := range out.Backends {
		var name string
		_ = json.Unmarshal(b["name"], &name)
		var caps map[string]any
		if raw, ok := b["capabilities"]; ok {
			_ = json.Unmarshal(raw, &caps)
		}
		got[name] = caps
	}
	spark := got["spark-chat"]
	if spark == nil || spark["vision"] != true || spark["source"] != backends.CapSourceSGLang || spark["probed_at"] == nil {
		t.Errorf("Z1 backend-list spark-chat capabilities %v, want vision true, source %q, probed_at set", spark, backends.CapSourceSGLang)
	}
	parked, ok := got["parked"]
	if !ok || parked == nil {
		t.Fatalf("Z1 backend-list parked row without capabilities: %s", rec.Body)
	}
	if v, present := parked["vision"]; !present || v != nil || parked["source"] != backends.CapSourceUnknown {
		t.Errorf("Z1 backend-list parked row capabilities %v, want vision null (unknown), source %q", parked, backends.CapSourceUnknown)
	}
}

func TestDispatchTargetsCarryCapabilities(t *testing.T) {
	srv := fzSGLang(t, true)
	bp := fzProbedPool(t, srv.URL)
	origin, err := dispatch.NormalizeOrigin(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	caps := dispatchTargetCapabilitiesOf(bp)
	rows := caps[origin]
	if len(rows) != 2 {
		t.Fatalf("Z1 status: %d capability rows at %s, want 2 (both rows behind the origin): %+v", len(rows), origin, caps)
	}
	byName := map[string]dispatchTargetCapability{}
	for _, r := range rows {
		byName[r.Backend] = r
	}
	if r := byName["spark-chat"]; r.Capabilities.Vision == nil || !*r.Capabilities.Vision {
		t.Errorf("Z1 status: spark-chat capabilities %+v, want vision true", r)
	}
	ds := buildDispatchAdmin(dispatch.Snapshot{Targets: []dispatch.TargetSnapshot{{Origin: origin}}}, true, nil, nil, nil, nil)
	withTargetCapabilities(ds, caps)
	if len(ds.Targets) != 1 || len(ds.Targets[0].Capabilities) != 2 {
		t.Fatalf("Z1 status: dispatch target capabilities %+v, want the two rows", ds.Targets)
	}
	raw, _ := json.Marshal(ds.Targets[0])
	var wire map[string]json.RawMessage
	_ = json.Unmarshal(raw, &wire)
	if _, ok := wire["capabilities"]; !ok {
		t.Errorf("Z1 status: target JSON without capabilities: %s", raw)
	}
}

// TestConsumerEffectiveLimitsOverlayWrittenRows (Z3): the answer's
// effective_limits include the rows the request just committed even when the
// registry has not loaded them yet (a failed or lagging reload) — the answer
// never shows a stale default for a limit that was just written.
func TestConsumerEffectiveLimitsOverlayWrittenRows(t *testing.T) {
	floor := gatewaylimits.Defaults{RPM: 30, TPM: 60000, Concurrency: 2, DailyTokens: 2000000, MonthlyCostUSD: 1,
		TenantEnvelopeFactor: 5, InstanceEnvelopeFactor: 10, ConcurrencyCap: 8}
	stale := gatewaylimits.NewRegistry(func(context.Context) ([]gatewaylimits.Row, error) { return nil, nil })
	h := &ManageHandler{}
	h.SetGatewayLimits(stale, func(keyquota.Op) gatewaylimits.Defaults { return floor })
	eight := int64(8)
	written := []gatewaylimits.Row{{Kind: gatewaylimits.SubjectConsumer, ConsumerID: "c1", Tier: gatewaylimits.TierServer,
		Op: keyquota.OpChat, Values: gatewaylimits.Values{MaxConcurrency: &eight}}}
	got := h.consumerEffectiveLimits("c1", "t1", written, true)
	chat := got["chat"]
	if chat.MaxConcurrency != 8 || chat.Sources["max_concurrency"] != gatewaylimits.SourceConsumerServer {
		t.Errorf("Z3 overlay: effective chat max_concurrency %d from %q, want 8 from consumer_server (the written row)", chat.MaxConcurrency, chat.Sources["max_concurrency"])
	}
	if e := got["embed"]; e.MaxConcurrency != 2 || e.Sources["max_concurrency"] != gatewaylimits.SourceSettings {
		t.Errorf("Z3 overlay: effective embed %+v, want the settings floor 2", e)
	}
	if len(got) != 3 {
		t.Errorf("Z3 overlay: %d ops, want chat, embed, rerank", len(got))
	}
	if (&ManageHandler{}).consumerEffectiveLimits("c1", "t1", nil, true) != nil {
		t.Errorf("Z3: effective_limits without a wired registry must be nil (JSON null)")
	}
}

// TestConsumerEffectiveLimitsBulkConcurrency (FOLGE-Z §3 amendment, S-1 of
// the SPA cluster): effective_limits.<op>.bulk_max_concurrency is the
// concurrency a request of this consumer gets in the bulk lane — the meter's
// own share of max_concurrency under gateway.xu_reserve_ratio (default 0.25:
// 2 ⇒ 1, 8 ⇒ 6), present whatever the consumer's lane.
func TestConsumerEffectiveLimitsBulkConcurrency(t *testing.T) {
	for _, tc := range []struct {
		concurrency, want int64
	}{{2, 1}, {8, 6}, {1, 1}} {
		floor := gatewaylimits.Defaults{RPM: 30, TPM: 60000, Concurrency: tc.concurrency, DailyTokens: 2000000, MonthlyCostUSD: 1,
			TenantEnvelopeFactor: 5, InstanceEnvelopeFactor: 10}
		reg := gatewaylimits.NewRegistry(func(context.Context) ([]gatewaylimits.Row, error) { return nil, nil })
		h := &ManageHandler{}
		h.SetGatewayLimits(reg, func(keyquota.Op) gatewaylimits.Defaults { return floor })
		for _, op := range []string{"chat", "embed", "rerank"} {
			raw, err := json.Marshal(h.consumerEffectiveLimits("c1", "t1", nil, false)[op])
			if err != nil {
				t.Fatal(err)
			}
			var wire map[string]any
			_ = json.Unmarshal(raw, &wire)
			got, ok := wire["bulk_max_concurrency"].(float64)
			if !ok || int64(got) != tc.want {
				t.Errorf("Z3 bulk_max_concurrency (%s, max_concurrency %d, r 0.25) = %v (present %v), want %d — %s", op, tc.concurrency, wire["bulk_max_concurrency"], ok, tc.want, raw)
			}
		}
	}
}
