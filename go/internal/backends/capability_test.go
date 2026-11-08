package backends

// FOLGE-Z Z1 gates: the capability probe against fake engines in the answer
// shapes the live pool serves (bodies trimmed from GET /get_model_info of the
// Spark SGLang and GET /props of llama-cpu on 2026-11-08, and the OpenRouter
// model list), its fail-closed reading (unknown, never a guessed true), its
// parallelism under hung engines, the change hook and ImageCapable — the
// predicate the alias registry resolves effective vision with.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v5"
)

// Live shapes (trimmed; field order and neighbours as the engines send them).
const (
	sglangModelInfoVision = `{"model_path":"/model","served_model_name":"qwen38-27b","is_generation":true,` +
		`"preferred_sampling_params":null,"weight_version":"default","has_image_understanding":true,` +
		`"has_audio_understanding":false,"model_type":"qwen3_5","architectures":["Qwen3_5ForConditionalGeneration"]}`
	sglangModelInfoText = `{"model_path":"/model","served_model_name":"text-only","is_generation":true,` +
		`"has_image_understanding":false,"has_audio_understanding":false}`
	llamaPropsNoVision = `{"default_generation_settings":{"n_ctx":262144},"total_slots":4,` +
		`"model_alias":"/models/Qwen3.6-27B-MTP-Q4_K_M.gguf","modalities":{"vision":false,"video":false,"audio":false},` +
		`"endpoint_slots":true,"endpoint_props":false}`
	llamaPropsVision   = `{"total_slots":2,"modalities":{"vision":true,"audio":false}}`
	llamaPropsNoField  = `{"default_generation_settings":{"n_ctx":4096},"total_slots":1,"chat_template":"x"}`
	openRouterModelsV1 = `{"data":[` +
		`{"id":"qwen/qwen3.6-27b","name":"Qwen3.6 27B","architecture":{"modality":"text+image->text","input_modalities":["text","image"],"output_modalities":["text"]}},` +
		`{"id":"text/only","architecture":{"modality":"text->text","input_modalities":["text"],"output_modalities":["text"]}}` +
		`]}`
)

// engine is a fake engine: path → (status, body); every request is counted
// per path. hang makes every request wait for release (or the client's
// cancellation).
type engine struct {
	srv     *httptest.Server
	mu      sync.Mutex
	routes  map[string]engineRoute
	hits    map[string]int
	auth    []string
	hang    bool
	release chan struct{}
}

type engineRoute struct {
	status int
	body   string
}

func newEngine(t *testing.T, routes map[string]engineRoute) *engine {
	t.Helper()
	e := &engine{routes: routes, hits: map[string]int{}, release: make(chan struct{})}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.hits[r.URL.Path]++
		e.auth = append(e.auth, r.Header.Get("Authorization"))
		rt, ok := e.routes[r.URL.Path]
		hang := e.hang
		e.mu.Unlock()
		if hang {
			select {
			case <-e.release:
			case <-r.Context().Done():
			}
			return
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rt.status)
		_, _ = w.Write([]byte(rt.body))
	}))
	t.Cleanup(func() {
		close(e.release)
		e.srv.Close()
	})
	return e
}

func (e *engine) set(path string, rt engineRoute) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.routes[path] = rt
}

func (e *engine) count(path string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.hits[path]
}

func (e *engine) total() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, v := range e.hits {
		n += v
	}
	return n
}

// capRow is an enabled _global chat row of class at host.
func capRow(id, host string, proto Protocol, class, model string) Backend {
	return Backend{ID: id, Name: id, Host: host, Protocol: proto, ProviderClass: class,
		Scope: GlobalScope, Trust: TrustFull, Roles: []string{RoleChat}, Enabled: true, Priority: 10,
		ModelMap: map[string]ModelSpec{"default": {Model: model}}}
}

func capPool(t *testing.T, rows ...Backend) *Pool {
	t.Helper()
	p := NewPool(nil, nil)
	p.SeedSnapshotForTest(rows)
	return p
}

func visionOf(c Capabilities) string {
	if c.Vision == nil {
		return "unknown"
	}
	if *c.Vision {
		return "true"
	}
	return "false"
}

// TestCapabilityProbeSGLang: an SGLang engine (registered as llamacpp, like
// the live spark-chat row) answers /get_model_info — has_image_understanding
// is the answer, the source names endpoint and field, probed_at is stamped.
func TestCapabilityProbeSGLang(t *testing.T) {
	for _, tc := range []struct {
		body, want string
	}{{sglangModelInfoVision, "true"}, {sglangModelInfoText, "false"}} {
		e := newEngine(t, map[string]engineRoute{"/get_model_info": {200, tc.body}})
		p := capPool(t, capRow("spark-chat", e.srv.URL, ProtocolOpenAI, ProviderLlamaCpp, "qwen38-27b"))
		before := time.Now()
		p.ProbeCapabilities(context.Background())
		c := p.Capabilities("spark-chat")
		if got := visionOf(c); got != tc.want || c.Source != CapSourceSGLang {
			t.Errorf("Z1 sglang probe: vision=%s source=%q, want vision=%s source=%q", got, c.Source, tc.want, CapSourceSGLang)
		}
		if c.ProbedAt.Before(before) {
			t.Errorf("Z1 sglang probe: probed_at %v not stamped by the round (before %v)", c.ProbedAt, before)
		}
	}
}

// TestCapabilityProbeLlamaCppProps: a llama.cpp engine has no
// /get_model_info (404) — /props modalities.vision answers.
func TestCapabilityProbeLlamaCppProps(t *testing.T) {
	for _, tc := range []struct {
		class, body, want string
	}{
		{ProviderLlamaCpp, llamaPropsNoVision, "false"},
		{ProviderLlamaCpp, llamaPropsVision, "true"},
		{ProviderGeneric, llamaPropsVision, "true"},
	} {
		e := newEngine(t, map[string]engineRoute{"/props": {200, tc.body}})
		p := capPool(t, capRow("llama", e.srv.URL, ProtocolOpenAI, tc.class, "qwen3.6:27b"))
		p.ProbeCapabilities(context.Background())
		c := p.Capabilities("llama")
		if got := visionOf(c); got != tc.want || c.Source != CapSourceLlamaCpp {
			t.Errorf("Z1 llama.cpp probe (%s): vision=%s source=%q, want vision=%s source=%q", tc.class, got, c.Source, tc.want, CapSourceLlamaCpp)
		}
	}
}

// TestCapabilityProbeOpenRouter: the model list is read ONCE per round for
// every openrouter row of the same base_url; the row's model_map.default
// model decides by architecture.input_modalities; a model the list does not
// carry reads unknown.
func TestCapabilityProbeOpenRouter(t *testing.T) {
	e := newEngine(t, map[string]engineRoute{"/v1/models": {200, openRouterModelsV1}})
	p := capPool(t,
		capRow("or-vision", e.srv.URL, ProtocolOpenAI, ProviderOpenRouter, "qwen/qwen3.6-27b"),
		capRow("or-text", e.srv.URL, ProtocolOpenAI, ProviderOpenRouter, "text/only"),
		capRow("or-missing", e.srv.URL, ProtocolOpenAI, ProviderOpenRouter, "not/listed"),
	)
	p.ProbeCapabilities(context.Background())
	for id, want := range map[string]string{"or-vision": "true", "or-text": "false", "or-missing": "unknown"} {
		c := p.Capabilities(id)
		if got := visionOf(c); got != want {
			t.Errorf("Z1 openrouter probe %s: vision=%s, want %s (source %q reason %q)", id, got, want, c.Source, c.Reason)
		}
		if want != "unknown" && c.Source != CapSourceOpenRouter {
			t.Errorf("Z1 openrouter probe %s: source %q, want %q", id, c.Source, CapSourceOpenRouter)
		}
	}
	if n := e.count("/v1/models"); n != 1 {
		t.Errorf("Z1 openrouter probe: %d model-list reads for three rows of one base_url, want 1 per round", n)
	}
}

// TestCapabilityProbeFailClosed: every answer that is not a clear field reads
// unknown with a value-free reason — never true.
func TestCapabilityProbeFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		routes map[string]engineRoute
	}{
		{"both endpoints 404", map[string]engineRoute{}},
		{"500", map[string]engineRoute{"/get_model_info": {500, `{"has_image_understanding":true}`}, "/props": {500, `{"modalities":{"vision":true}}`}}},
		{"garbage body", map[string]engineRoute{"/get_model_info": {200, `not json`}}},
		{"field is a string", map[string]engineRoute{"/get_model_info": {200, `{"has_image_understanding":"true"}`}}},
		{"props without the field", map[string]engineRoute{"/props": {200, llamaPropsNoField}}},
		{"modalities vision null", map[string]engineRoute{"/props": {200, `{"modalities":{"vision":null}}`}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEngine(t, tc.routes)
			p := capPool(t, capRow("row", e.srv.URL, ProtocolOpenAI, ProviderLlamaCpp, "m"))
			p.ProbeCapabilities(context.Background())
			c := p.Capabilities("row")
			if c.Vision != nil {
				t.Fatalf("Z1 fail-closed: vision=%s, want unknown", visionOf(c))
			}
			if c.Source != CapSourceUnknown || c.Reason == "" {
				t.Errorf("Z1 fail-closed: source=%q reason=%q, want source %q and a reason", c.Source, c.Reason, CapSourceUnknown)
			}
			if strings.Contains(c.Reason, e.srv.URL) || strings.Contains(c.Reason, "127.0.0.1") {
				t.Errorf("Z1 fail-closed: reason %q carries the URL — must be value-free", c.Reason)
			}
		})
	}
}

// TestCapabilityProbeUnprobedProtocols: ollama, anthropic, rerank rows and a
// ctx peer have no probe source — unknown, and no request is sent.
func TestCapabilityProbeUnprobedProtocols(t *testing.T) {
	e := newEngine(t, map[string]engineRoute{"/get_model_info": {200, sglangModelInfoVision}, "/props": {200, llamaPropsVision}})
	rows := []Backend{
		capRow("ollama", e.srv.URL, ProtocolOllama, ProviderGeneric, "m"),
		capRow("anthropic", e.srv.URL, ProtocolAnthropic, ProviderGeneric, "m"),
		capRow("rerank", e.srv.URL, Protocol("rerank"), ProviderLlamaCpp, "m"),
		capRow("peer", e.srv.URL, ProtocolOpenAI, ProviderCtx, "m"),
	}
	p := capPool(t, rows...)
	p.ProbeCapabilities(context.Background())
	for _, b := range rows {
		c := p.Capabilities(b.ID)
		if c.Vision != nil || c.Reason != "not_probed" {
			t.Errorf("Z1 unprobed protocol %s: vision=%s reason=%q, want unknown/not_probed", b.ID, visionOf(c), c.Reason)
		}
	}
	if n := e.total(); n != 0 {
		t.Errorf("Z1 unprobed protocols: %d requests sent, want 0", n)
	}
}

// TestCapabilityProbeSkipsParkedRows: a disabled row and a row of an active
// disable profile are not probed (the eject case: the GPU is lent out).
func TestCapabilityProbeSkipsParkedRows(t *testing.T) {
	e := newEngine(t, map[string]engineRoute{"/get_model_info": {200, sglangModelInfoVision}})
	off := capRow("off", e.srv.URL, ProtocolOpenAI, ProviderLlamaCpp, "m")
	off.Enabled = false
	parked := capRow("parked", e.srv.URL, ProtocolOpenAI, ProviderLlamaCpp, "m")
	p := NewPool(nil, nil)
	p.SeedSnapshotDisabledByForTest([]Backend{off, parked}, map[string]string{"parked": "gaming"})
	p.ProbeCapabilities(context.Background())
	if n := e.total(); n != 0 {
		t.Errorf("Z1 parked rows: %d requests sent, want 0", n)
	}
}

// TestCapabilityProbeBearer: a keyed row sends its resolved key, a keyless
// row none; an unresolved ref is not sent at all (unknown).
func TestCapabilityProbeBearer(t *testing.T) {
	e := newEngine(t, map[string]engineRoute{"/get_model_info": {200, sglangModelInfoVision}})
	keyed := capRow("keyed", e.srv.URL, ProtocolOpenAI, ProviderLlamaCpp, "m")
	keyed.APIKeyRef, keyed.APIKey = "spark", "sk-test"
	unresolved := capRow("unresolved", e.srv.URL, ProtocolOpenAI, ProviderLlamaCpp, "m")
	unresolved.APIKeyRef = "missing"
	p := capPool(t, keyed, unresolved)
	p.ProbeCapabilities(context.Background())
	e.mu.Lock()
	auth := append([]string(nil), e.auth...)
	e.mu.Unlock()
	if len(auth) != 1 || auth[0] != "Bearer sk-test" {
		t.Errorf("Z1 bearer: Authorization headers %q, want exactly one %q", auth, "Bearer sk-test")
	}
	if c := p.Capabilities("unresolved"); c.Vision != nil || c.Reason != "api_key_ref_unresolved" {
		t.Errorf("Z1 bearer: unresolved ref vision=%s reason=%q, want unknown/api_key_ref_unresolved", visionOf(c), c.Reason)
	}
}

// TestCapabilityProbeParallelUnderHungEngines: 20 hung rows cost one probe
// timeout, not twenty — the round is parallel and each hung row reads
// unknown (timeout).
func TestCapabilityProbeParallelUnderHungEngines(t *testing.T) {
	e := newEngine(t, map[string]engineRoute{})
	e.hang = true
	var rows []Backend
	for i := 0; i < 20; i++ {
		rows = append(rows, capRow("hung-"+string(rune('a'+i)), e.srv.URL, ProtocolOpenAI, ProviderLlamaCpp, "m"))
	}
	p := capPool(t, rows...)
	p.capTimeout = 200 * time.Millisecond
	start := time.Now()
	p.ProbeCapabilities(context.Background())
	took := time.Since(start)
	if took > 10*p.capTimeout {
		t.Errorf("Z1 parallel: round over 20 hung rows took %v, want about one timeout (%v)", took, p.capTimeout)
	}
	for _, b := range rows {
		if c := p.Capabilities(b.ID); c.Vision != nil || c.Reason != "timeout" {
			t.Errorf("Z1 parallel: %s vision=%s reason=%q, want unknown/timeout", b.ID, visionOf(c), c.Reason)
		}
	}
	if e.total() == 0 {
		t.Errorf("Z1 parallel: no request reached the hung engine — the round did not probe")
	}
}

// TestCapabilityChangeHook: a round that changes a row runs the hooks once;
// a round without a change runs none; every change of a row is one INFO.
func TestCapabilityChangeHook(t *testing.T) {
	e := newEngine(t, map[string]engineRoute{"/get_model_info": {200, sglangModelInfoText}})
	p := capPool(t, capRow("spark", e.srv.URL, ProtocolOpenAI, ProviderLlamaCpp, "m"))
	var fired atomic.Int32
	p.OnChange(func() { fired.Add(1) })
	if n := p.ProbeCapabilities(context.Background()); n != 1 || fired.Load() != 1 {
		t.Fatalf("Z1 hook: first round changed=%d hooks=%d, want 1/1 (unknown → false is a change)", n, fired.Load())
	}
	if n := p.ProbeCapabilities(context.Background()); n != 0 || fired.Load() != 1 {
		t.Errorf("Z1 hook: unchanged round changed=%d hooks=%d, want 0/1", n, fired.Load())
	}
	e.set("/get_model_info", engineRoute{200, sglangModelInfoVision})
	if n := p.ProbeCapabilities(context.Background()); n != 1 || fired.Load() != 2 {
		t.Errorf("Z1 hook: false → true round changed=%d hooks=%d, want 1/2", n, fired.Load())
	}
	if !p.ImageCapable(RoleChat, SensCredentials, GlobalScope) {
		t.Errorf("Z1 hook: after the change ImageCapable(chat) is false")
	}
}

// TestRunCapabilityProbeCadence: RunCapabilityProbe probes at start, again
// every interval (read per wait) and returns when ctx ends.
func TestRunCapabilityProbeCadence(t *testing.T) {
	e := newEngine(t, map[string]engineRoute{"/get_model_info": {200, sglangModelInfoText}})
	p := capPool(t, capRow("spark", e.srv.URL, ProtocolOpenAI, ProviderLlamaCpp, "m"))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.RunCapabilityProbe(ctx, func() time.Duration { return 20 * time.Millisecond })
	}()
	deadline := time.Now().Add(5 * time.Second)
	for e.count("/get_model_info") < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Z1 cadence: RunCapabilityProbe did not return after ctx ended")
	}
	if n := e.count("/get_model_info"); n < 3 {
		t.Errorf("Z1 cadence: %d probe rounds within 5 s at a 20 ms interval, want >= 3", n)
	}
	if c := p.Capabilities("spark"); visionOf(c) != "false" {
		t.Errorf("Z1 cadence: vision=%s after the rounds, want false", visionOf(c))
	}
}

// TestImageCapableIsTheChainFilter: the predicate of the effective vision —
// enabled, not profile-disabled, visible to the alias scope, carrying the
// role, trust allowing the floor, and vision=true (unknown is not capable).
func TestImageCapableIsTheChainFilter(t *testing.T) {
	yes, no := true, false
	row := func(id, scope string, trust Trust, enabled bool, vision *bool) (Backend, *bool) {
		b := capRow(id, "http://unused", ProtocolOpenAI, ProviderLlamaCpp, "m")
		b.Scope, b.Trust, b.Enabled = scope, trust, enabled
		return b, vision
	}
	for _, tc := range []struct {
		name     string
		rows     []func() (Backend, *bool)
		disabled map[string]string
		role     string
		sens     Sensitivity
		scope    string
		want     bool
	}{
		{"capable global row", []func() (Backend, *bool){func() (Backend, *bool) { return row("a", GlobalScope, TrustFull, true, &yes) }}, nil, RoleChat, SensCredentials, GlobalScope, true},
		{"unknown is not capable", []func() (Backend, *bool){func() (Backend, *bool) { return row("a", GlobalScope, TrustFull, true, nil) }}, nil, RoleChat, SensCredentials, GlobalScope, false},
		{"reported false", []func() (Backend, *bool){func() (Backend, *bool) { return row("a", GlobalScope, TrustFull, true, &no) }}, nil, RoleChat, SensCredentials, GlobalScope, false},
		{"disabled row", []func() (Backend, *bool){func() (Backend, *bool) { return row("a", GlobalScope, TrustFull, false, &yes) }}, nil, RoleChat, SensCredentials, GlobalScope, false},
		{"profile-disabled row", []func() (Backend, *bool){func() (Backend, *bool) { return row("a", GlobalScope, TrustFull, true, &yes) }}, map[string]string{"a": "eject"}, RoleChat, SensCredentials, GlobalScope, false},
		{"other role", []func() (Backend, *bool){func() (Backend, *bool) { return row("a", GlobalScope, TrustFull, true, &yes) }}, nil, RoleEmbed, SensCredentials, GlobalScope, false},
		{"trust below the floor", []func() (Backend, *bool){func() (Backend, *bool) { return row("a", GlobalScope, TrustPublic, true, &yes) }}, nil, RoleChat, SensCredentials, GlobalScope, false},
		{"tenant row, global alias", []func() (Backend, *bool){func() (Backend, *bool) { return row("a", "tenant-a", TrustFull, true, &yes) }}, nil, RoleChat, SensCredentials, GlobalScope, false},
		{"tenant row, own tenant alias", []func() (Backend, *bool){func() (Backend, *bool) { return row("a", "tenant-a", TrustFull, true, &yes) }}, nil, RoleChat, SensCredentials, "tenant-a", true},
		{"tenant row, foreign tenant alias", []func() (Backend, *bool){func() (Backend, *bool) { return row("a", "tenant-a", TrustFull, true, &yes) }}, nil, RoleChat, SensCredentials, "tenant-b", false},
		{"one capable among incapable", []func() (Backend, *bool){
			func() (Backend, *bool) { return row("a", GlobalScope, TrustFull, true, &no) },
			func() (Backend, *bool) { return row("b", GlobalScope, TrustFull, true, nil) },
			func() (Backend, *bool) { return row("c", GlobalScope, TrustFull, true, &yes) },
		}, nil, RoleChat, SensCredentials, GlobalScope, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewPool(nil, nil)
			var rows []Backend
			caps := map[string]capEntry{}
			for _, f := range tc.rows {
				b, v := f()
				rows = append(rows, b)
				src := CapSourceUnknown
				if v != nil {
					src = CapSourceSGLang
				}
				caps[b.ID] = capEntry{fp: capFingerprint(&b), caps: Capabilities{Vision: v, Source: src, ProbedAt: time.Now()}}
			}
			p.SeedSnapshotDisabledByForTest(rows, tc.disabled)
			p.capM.Lock()
			p.caps = caps
			p.capM.Unlock()
			if got := p.ImageCapable(tc.role, tc.sens, tc.scope); got != tc.want {
				t.Errorf("Z1/Z2 ImageCapable(%s, %s, %s) = %v, want %v", tc.role, tc.sens, tc.scope, got, tc.want)
			}
		})
	}
}

// TestCapabilitiesWire: the §3 wire shape — vision null and probed_at null
// when unknown and never probed, RFC3339 otherwise.
func TestCapabilitiesWire(t *testing.T) {
	raw, err := json.Marshal(NewPool(nil, nil).Capabilities("nope"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	if got["vision"] != nil || got["probed_at"] != nil || got["source"] != CapSourceUnknown {
		t.Errorf("Z1 wire (unprobed): %s, want vision null, probed_at null, source %q", raw, CapSourceUnknown)
	}
	yes := true
	at := time.Date(2026, 11, 8, 16, 0, 0, 0, time.UTC)
	raw, _ = json.Marshal(Capabilities{Vision: &yes, ProbedAt: at, Source: CapSourceSGLang})
	if string(raw) != `{"vision":true,"probed_at":"2026-11-08T16:00:00Z","source":"sglang:/get_model_info has_image_understanding"}` {
		t.Errorf("Z1 wire (probed): %s", raw)
	}
}

// TestReloadKeepsUnchangedAndDropsChangedCapabilities: a Reload runs the
// change hooks and kicks a probe round; a row whose fingerprint (base_url,
// protocol, provider class, default model) is unchanged keeps its result, a
// moved row reads unknown until the next round, a vanished row is dropped.
func TestReloadKeepsUnchangedAndDropsChangedCapabilities(t *testing.T) {
	e := newEngine(t, map[string]engineRoute{"/get_model_info": {200, sglangModelInfoVision}})
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)
	cols := []string{"id", "name", "base_url", "protocol", "provider_class", "api_key_ref", "trust", "locality", "roles",
		"model_map", "timeouts", "num_ctx", "priority", "enabled", "extra_headers", "extra_body", "limits", "metadata", "scope"}
	expect := func(rows ...[2]string) { // {id, base_url}
		r := pgxmock.NewRows(cols)
		for _, x := range rows {
			r.AddRow(x[0], x[0], x[1], "openai", ProviderLlamaCpp, nil, string(TrustFull), LocalityLAN, []string{RoleChat},
				[]byte(`{"default":{"model":"m"}}`), []byte(`{}`), nil, 10, true, []byte(`{}`), []byte(`{}`), []byte(`{}`), []byte(`{}`), GlobalScope)
		}
		mock.ExpectQuery(`FROM context_backends`).WillReturnRows(r)
		mock.ExpectQuery(`FROM context_disable_profiles`).WillReturnRows(pgxmock.NewRows([]string{"id", "name", "label", "description", "active", "reserved", "scope"}))
		mock.ExpectQuery(`FROM context_disable_profile_backends`).WillReturnRows(pgxmock.NewRows([]string{"profile_id", "backend_id"}))
	}
	p := NewPool(mock, nil)
	var fired atomic.Int32
	p.OnChange(func() { fired.Add(1) })
	expect([2]string{"keep", e.srv.URL}, [2]string{"move", e.srv.URL}, [2]string{"gone", e.srv.URL})
	if err := p.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fired.Load() != 1 {
		t.Errorf("Z1 reload hook: %d hooks after a Reload, want 1", fired.Load())
	}
	select {
	case <-p.capKick:
	default:
		t.Errorf("Z1 reload kick: a Reload did not kick a probe round")
	}
	p.ProbeCapabilities(context.Background())
	for _, id := range []string{"keep", "move", "gone"} {
		if visionOf(p.Capabilities(id)) != "true" {
			t.Fatalf("Z1 reload: %s not probed true before the second reload", id)
		}
	}
	expect([2]string{"keep", e.srv.URL}, [2]string{"move", e.srv.URL + "/elsewhere"})
	if err := p.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := visionOf(p.Capabilities("keep")); got != "true" {
		t.Errorf("Z1 reload: unchanged row reads %s after a reload, want its result kept (true)", got)
	}
	if got := visionOf(p.Capabilities("move")); got != "unknown" {
		t.Errorf("Z1 reload: a row with a new base_url reads %s, want unknown until the next round", got)
	}
	p.capM.Lock()
	_, kept := p.caps["gone"]
	p.capM.Unlock()
	if kept {
		t.Errorf("Z1 reload: a vanished row's result is still stored")
	}
}

// Hardening after review 1 (FOLGE-Z C-GO).

// TestChainVisionViewExportRule (R1-1): a tenant alias whose role the server
// admin did not export reaches no operator row (oai.BuildChain step 4) — a
// capable _global row must not make it image-capable.
func TestChainVisionViewExportRule(t *testing.T) {
	yes, no := true, false
	g := capRow("g", "http://g", ProtocolOpenAI, ProviderLlamaCpp, "m")
	tr := capRow("t", "http://t", ProtocolOpenAI, ProviderLlamaCpp, "m")
	tr.Scope = "tenant-a"
	p := NewPool(nil, nil)
	p.SeedSnapshotForTest([]Backend{g, tr})
	p.capM.Lock()
	p.caps = map[string]capEntry{
		"g": {fp: capFingerprint(&g), caps: Capabilities{Vision: &yes, Source: CapSourceSGLang}},
		"t": {fp: capFingerprint(&tr), caps: Capabilities{Vision: &no, Source: CapSourceSGLang}},
	}
	p.capM.Unlock()
	var exported []string
	v := ChainVisionView{Pool: p, ExportableRoles: func() []string { return exported }}
	if v.ImageCapable(RoleChat, SensCredentials, "tenant-a") {
		t.Errorf("R1-1: tenant alias of an unexported role counts the _global row as capable")
	}
	if !v.ImageCapable(RoleChat, SensCredentials, GlobalScope) {
		t.Errorf("R1-1: a _global alias lost its _global row")
	}
	exported = []string{RoleChat}
	if !v.ImageCapable(RoleChat, SensCredentials, "tenant-a") {
		t.Errorf("R1-1: exported role — the _global row must count for the tenant alias")
	}
}

// TestCapabilityProbeOpenRouterPerRoleModels (R1-4): the chain sends
// ModelFor(role); an openrouter row is image-capable only when the model of
// every role it carries is — a text model behind one role reads false, an
// unlisted one unknown.
func TestCapabilityProbeOpenRouterPerRoleModels(t *testing.T) {
	e := newEngine(t, map[string]engineRoute{"/v1/models": {200, openRouterModelsV1}})
	row := func(id string, roles []string, mm map[string]ModelSpec) Backend {
		b := capRow(id, e.srv.URL, ProtocolOpenAI, ProviderOpenRouter, "")
		b.Roles, b.ModelMap = roles, mm
		return b
	}
	p := capPool(t,
		row("chat-text", []string{RoleChat}, map[string]ModelSpec{"default": {Model: "qwen/qwen3.6-27b"}, RoleChat: {Model: "text/only"}}),
		row("all-vision", []string{RoleChat, RoleSynthesis}, map[string]ModelSpec{"default": {Model: "qwen/qwen3.6-27b"}}),
		row("one-unlisted", []string{RoleChat, RoleSynthesis}, map[string]ModelSpec{"default": {Model: "qwen/qwen3.6-27b"}, RoleSynthesis: {Model: "not/listed"}}),
	)
	p.ProbeCapabilities(context.Background())
	for id, want := range map[string]string{"chat-text": "false", "all-vision": "true", "one-unlisted": "unknown"} {
		if got := visionOf(p.Capabilities(id)); got != want {
			t.Errorf("R1-4 %s: vision=%s, want %s", id, got, want)
		}
	}
}

// TestCapabilityProbeBodyCaps (R1-2): an engine answer past the engine cap
// reads unknown (body_too_large) — the probe never holds a big body.
func TestCapabilityProbeBodyCaps(t *testing.T) {
	big := `{"has_image_understanding":true,"pad":"` + strings.Repeat("x", 2<<20) + `"}`
	e := newEngine(t, map[string]engineRoute{"/get_model_info": {200, big}})
	p := capPool(t, capRow("big", e.srv.URL, ProtocolOpenAI, ProviderLlamaCpp, "m"))
	p.ProbeCapabilities(context.Background())
	if c := p.Capabilities("big"); c.Vision != nil || c.Reason != "body_too_large" {
		t.Errorf("R1-2: 2 MiB engine answer: vision=%s reason=%q, want unknown/body_too_large", visionOf(c), c.Reason)
	}
}

// TestCapabilityProbeBoundedParallelism (R1-2): at most capProbeParallel
// rows are in flight at once.
func TestCapabilityProbeBoundedParallelism(t *testing.T) {
	var cur, peak atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := cur.Add(1)
		defer cur.Add(-1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		_, _ = w.Write([]byte(sglangModelInfoText))
	}))
	t.Cleanup(srv.Close)
	var rows []Backend
	for i := 0; i < 40; i++ {
		rows = append(rows, capRow("r"+strconv.Itoa(i), srv.URL, ProtocolOpenAI, ProviderLlamaCpp, "m"))
	}
	p := capPool(t, rows...)
	p.ProbeCapabilities(context.Background())
	if got := peak.Load(); got > capProbeParallel || got < 2 {
		t.Errorf("R1-2: peak in-flight probes %d, want between 2 and %d", got, capProbeParallel)
	}
}

// TestCapabilitiesReadChecksFingerprint (R1-7): a stored answer for a row
// whose base_url moved since reads unknown on the display path too.
func TestCapabilitiesReadChecksFingerprint(t *testing.T) {
	e := newEngine(t, map[string]engineRoute{"/get_model_info": {200, sglangModelInfoVision}})
	b := capRow("row", e.srv.URL, ProtocolOpenAI, ProviderLlamaCpp, "m")
	p := capPool(t, b)
	p.ProbeCapabilities(context.Background())
	b.Host = e.srv.URL + "/moved"
	p.SeedSnapshotForTest([]Backend{b})
	if c := p.Capabilities("row"); c.Vision != nil {
		t.Errorf("R1-7: answer of the old base_url still shown: vision=%s", visionOf(c))
	}
}

// TestKickRoundProbesOnlyRowsWithoutAnswer (R1-6): the round a reload kicks
// asks only rows without a valid answer; the cadence round asks all.
func TestKickRoundProbesOnlyRowsWithoutAnswer(t *testing.T) {
	e := newEngine(t, map[string]engineRoute{"/get_model_info": {200, sglangModelInfoVision}})
	a := capRow("a", e.srv.URL, ProtocolOpenAI, ProviderLlamaCpp, "m")
	p := capPool(t, a)
	p.ProbeCapabilities(context.Background())
	before := e.count("/get_model_info")
	b := capRow("b", e.srv.URL, ProtocolOpenAI, ProviderLlamaCpp, "m")
	p.SeedSnapshotForTest([]Backend{a, b})
	p.probeRound(context.Background(), true)
	if n := e.count("/get_model_info") - before; n != 1 {
		t.Errorf("R1-6: kick round sent %d probes, want 1 (only the new row)", n)
	}
	if visionOf(p.Capabilities("b")) != "true" {
		t.Errorf("R1-6: the new row was not probed")
	}
}

// TestCadenceRoundSurvivesFrequentReloads (PR #178 CodeRabbit 1): reload
// kicks closer together than pool.capability_probe_interval must not push
// the full round out — a model swap behind the same base_url (vision true ⇒
// false, fingerprint unchanged, so a kick round skips the row) is seen
// within one interval.
func TestCadenceRoundSurvivesFrequentReloads(t *testing.T) {
	e := newEngine(t, map[string]engineRoute{"/get_model_info": {200, sglangModelInfoVision}})
	p := capPool(t, capRow("spark", e.srv.URL, ProtocolOpenAI, ProviderLlamaCpp, "m"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.RunCapabilityProbe(ctx, func() time.Duration { return 150 * time.Millisecond })
	}()
	deadline := time.Now().Add(5 * time.Second)
	for visionOf(p.Capabilities("spark")) != "true" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if visionOf(p.Capabilities("spark")) != "true" {
		t.Fatal("precondition: the boot round did not read vision=true")
	}
	e.set("/get_model_info", engineRoute{200, sglangModelInfoText})
	stopKicks := make(chan struct{})
	go func() {
		tick := time.NewTicker(30 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stopKicks:
				return
			case <-tick.C:
				select {
				case p.capKick <- struct{}{}:
				default:
				}
			}
		}
	}()
	deadline = time.Now().Add(2 * time.Second)
	for visionOf(p.Capabilities("spark")) != "false" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	close(stopKicks)
	got := visionOf(p.Capabilities("spark"))
	cancel()
	<-done
	if got != "false" {
		t.Errorf("CR-1: under reload kicks every 30 ms (interval 150 ms) the model swap stayed vision=%s after 2 s, want false — the cadence round never ran", got)
	}
}
