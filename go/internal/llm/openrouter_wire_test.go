// OpenRouter wire specifics (G29 / F3-P5): forced zdr/deny, extra_body merge
// semantics, attribution headers, usage.cost / response.model / response.id
// telemetry. All probes run against httptest mocks — no live key in CI
// (design 03 §5).
package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/llmlog"
)

// orRecorder captures the raw request body AND headers of one openai-wire
// call (the wireRecorder next door only keeps path+body) and answers in the
// OpenRouter response shape: id + model + usage.cost.
type orRecorder struct {
	mu     sync.Mutex
	body   map[string]any
	header http.Header
	srv    *httptest.Server
}

func newORRecorder(t *testing.T, respJSON string) *orRecorder {
	t.Helper()
	rec := &orRecorder{}
	rec.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		rec.mu.Lock()
		rec.body = m
		rec.header = r.Header.Clone()
		rec.mu.Unlock()
		_, _ = w.Write([]byte(respJSON))
	}))
	t.Cleanup(rec.srv.Close)
	return rec
}

func (rec *orRecorder) recorded() (map[string]any, http.Header) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.body, rec.header
}

const orRespOK = `{"id":"gen-12345","model":"qwen/qwen3.6-27b-served",
  "choices":[{"message":{"role":"assistant","content":"ok"}}],
  "usage":{"completion_tokens":1,"prompt_tokens":10,"cost":0.0123}}`

func orBackend(host string, trust backends.Trust) backends.Backend {
	return backends.Backend{
		Host: host, Protocol: backends.ProtocolOpenAI,
		ProviderClass: backends.ProviderOpenRouter,
		Model:         "qwen/qwen3.6-27b", Trust: trust,
	}
}

func providerObj(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	prov, _ := body["provider"].(map[string]any)
	return prov
}

// TestOpenRouterForcedZDRTrustIndependent is the decoupling probe (design 03
// negative test 9): even at trust=full-trust the request carries
// provider.zdr=true + provider.data_collection="deny". A trust-coupled
// enforcement ("only below full-trust") turns this red — exactly the silent
// ZDR loss the §3.3 decoupling prevents.
func TestOpenRouterForcedZDRTrustIndependent(t *testing.T) {
	for _, trust := range []backends.Trust{backends.TrustFull, backends.TrustNoCredentials} {
		t.Run(string(trust), func(t *testing.T) {
			rec := newORRecorder(t, orRespOK)
			b := orBackend(rec.srv.URL, trust)
			if _, err := Chat(context.Background(), b, "sys", "user", Options{NumPredict: 5}, 5*time.Second); err != nil {
				t.Fatalf("Chat: %v", err)
			}
			body, _ := rec.recorded()
			prov := providerObj(t, body)
			if prov == nil {
				t.Fatalf("request lacks the provider object; body = %v", body)
			}
			if prov["zdr"] != true {
				t.Errorf("provider.zdr = %v, want true (forced, trust-independent)", prov["zdr"])
			}
			if prov["data_collection"] != "deny" {
				t.Errorf("provider.data_collection = %v, want deny", prov["data_collection"])
			}
		})
	}
}

// TestOpenRouterExtraBodyTightensNeverLoosens: extra_body merges into the
// request (require_parameters survives), but the zdr/deny force runs AFTER
// the merge — an extra_body attempt to set zdr:false is overwritten.
func TestOpenRouterExtraBodyTightensNeverLoosens(t *testing.T) {
	rec := newORRecorder(t, orRespOK)
	b := orBackend(rec.srv.URL, backends.TrustNoCredentials)
	b.ExtraBody = map[string]any{
		"provider": map[string]any{"require_parameters": true, "zdr": false},
	}
	if _, err := Chat(context.Background(), b, "sys", "user", Options{NumPredict: 5}, 5*time.Second); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	body, _ := rec.recorded()
	prov := providerObj(t, body)
	if prov == nil {
		t.Fatal("request lacks the provider object")
	}
	if prov["require_parameters"] != true {
		t.Errorf("provider.require_parameters = %v, want true (extra_body must merge)", prov["require_parameters"])
	}
	if prov["zdr"] != true {
		t.Errorf("provider.zdr = %v, want true (force wins over extra_body loosening)", prov["zdr"])
	}
	if prov["data_collection"] != "deny" {
		t.Errorf("provider.data_collection = %v, want deny", prov["data_collection"])
	}
}

// TestOpenRouterDataCollectionEscape: metadata.allow_data_collection=true is
// the ONLY way out of the enforcement (confirm-gated at create/update); a
// string "true" stays armed (literal bool contract).
func TestOpenRouterDataCollectionEscape(t *testing.T) {
	rec := newORRecorder(t, orRespOK)
	b := orBackend(rec.srv.URL, backends.TrustNoCredentials)
	b.Metadata = map[string]any{"allow_data_collection": true}
	if _, err := Chat(context.Background(), b, "sys", "user", Options{NumPredict: 5}, 5*time.Second); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	body, _ := rec.recorded()
	if prov := providerObj(t, body); prov != nil {
		t.Errorf("escape armed but provider object still injected: %v", prov)
	}

	rec2 := newORRecorder(t, orRespOK)
	b2 := orBackend(rec2.srv.URL, backends.TrustNoCredentials)
	b2.Metadata = map[string]any{"allow_data_collection": "true"} // not a bool
	if _, err := Chat(context.Background(), b2, "sys", "user", Options{NumPredict: 5}, 5*time.Second); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	body2, _ := rec2.recorded()
	if providerObj(t, body2) == nil {
		t.Error(`metadata "true" (string) must NOT disarm the enforcement`)
	}
}

// TestGenericProviderNoZDRInjection: the force is bound to the provider
// class, not the openai wire — a generic/llamacpp backend gets no provider
// object (llama.cpp would reject unknown fields at worst, mislead at best).
func TestGenericProviderNoZDRInjection(t *testing.T) {
	rec := newORRecorder(t, orRespOK)
	b := orBackend(rec.srv.URL, backends.TrustFull)
	b.ProviderClass = backends.ProviderGeneric
	if _, err := Chat(context.Background(), b, "sys", "user", Options{NumPredict: 5}, 5*time.Second); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	body, _ := rec.recorded()
	if prov := providerObj(t, body); prov != nil {
		t.Errorf("generic provider got a provider object injected: %v", prov)
	}
}

// TestOpenRouterResponseTelemetry: usage.cost, the top-level model (the model
// that ACTUALLY answered — models-fallback can differ) and the response id
// reach ChatResponse for openrouter-class backends; a generic backend leaves
// all three zero even when its response carries them (llmlog's model column
// stays row-faithful locally).
func TestOpenRouterResponseTelemetry(t *testing.T) {
	rec := newORRecorder(t, orRespOK)
	b := orBackend(rec.srv.URL, backends.TrustNoCredentials)
	resp, err := Chat(context.Background(), b, "sys", "user", Options{NumPredict: 5}, 5*time.Second)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.CostUSD == nil || *resp.CostUSD != 0.0123 {
		t.Errorf("CostUSD = %v, want 0.0123", resp.CostUSD)
	}
	if resp.ServedModel != "qwen/qwen3.6-27b-served" {
		t.Errorf("ServedModel = %q, want the response's top-level model", resp.ServedModel)
	}
	if resp.ProviderRequestID != "gen-12345" {
		t.Errorf("ProviderRequestID = %q, want gen-12345", resp.ProviderRequestID)
	}

	rec2 := newORRecorder(t, orRespOK)
	b2 := orBackend(rec2.srv.URL, backends.TrustFull)
	b2.ProviderClass = backends.ProviderGeneric
	resp2, err := Chat(context.Background(), b2, "sys", "user", Options{NumPredict: 5}, 5*time.Second)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp2.CostUSD != nil || resp2.ServedModel != "" || resp2.ProviderRequestID != "" {
		t.Errorf("generic backend filled provider telemetry: cost=%v model=%q id=%q",
			resp2.CostUSD, resp2.ServedModel, resp2.ProviderRequestID)
	}
}

// TestOpenRouterExtraHeaders: attribution headers reach the wire; the
// Authorization derived from api_key_ref is set LAST and cannot be
// overridden by a row edited past the credential-carrier denylist.
func TestOpenRouterExtraHeaders(t *testing.T) {
	rec := newORRecorder(t, orRespOK)
	b := orBackend(rec.srv.URL, backends.TrustNoCredentials)
	b.APIKey = "sk-or-real"
	b.ExtraHeaders = map[string]string{
		"HTTP-Referer":  "https://github.com/GottZ/ctx",
		"X-Title":       "ctx",
		"Authorization": "Bearer sk-smuggled",
	}
	if _, err := Chat(context.Background(), b, "sys", "user", Options{NumPredict: 5}, 5*time.Second); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	_, hdr := rec.recorded()
	if got := hdr.Get("HTTP-Referer"); got != "https://github.com/GottZ/ctx" {
		t.Errorf("HTTP-Referer = %q", got)
	}
	if got := hdr.Get("X-Title"); got != "ctx" {
		t.Errorf("X-Title = %q", got)
	}
	if got := hdr.Get("Authorization"); got != "Bearer sk-or-real" {
		t.Errorf("Authorization = %q, want the api_key_ref-derived bearer to win", got)
	}
}

// TestApplyProviderTelemetry: the llmlog row picks up cost_usd, the served
// model overwrite and metadata.provider_request_id; zero-value responses
// (local backends) and a nil response (the walk never got an answer) leave
// the entry untouched.
func TestApplyProviderTelemetry(t *testing.T) {
	cost := 0.0123
	entry := llmlog.Entry{Model: "row-model", Metadata: map[string]any{"chain": "x"}}
	ApplyProviderTelemetry(&entry, &ChatResponse{
		CostUSD: &cost, ServedModel: "served-model", ProviderRequestID: "gen-1",
	})
	if entry.CostUSD == nil || *entry.CostUSD != cost {
		t.Errorf("CostUSD = %v, want %v", entry.CostUSD, cost)
	}
	if entry.Model != "served-model" {
		t.Errorf("Model = %q, want the served override", entry.Model)
	}
	if entry.Metadata["provider_request_id"] != "gen-1" {
		t.Errorf("metadata.provider_request_id = %v", entry.Metadata["provider_request_id"])
	}

	local := llmlog.Entry{Model: "row-model"}
	ApplyProviderTelemetry(&local, &ChatResponse{})
	if local.CostUSD != nil || local.Model != "row-model" || local.Metadata != nil {
		t.Errorf("zero-value response mutated the entry: %+v", local)
	}

	// nil response: the error path of every unconditional caller. Without the
	// guard this line dereferences nil instead of leaving the row alone.
	failed := llmlog.Entry{Model: "row-model"}
	ApplyProviderTelemetry(&failed, nil)
	if failed.CostUSD != nil || failed.Model != "row-model" || failed.Metadata != nil {
		t.Errorf("nil response mutated the entry: %+v", failed)
	}
}
