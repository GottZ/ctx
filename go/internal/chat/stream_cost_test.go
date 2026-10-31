package chat

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/llmlog"
)

// orCostTail ends an OpenRouter stream: the served model differs from the
// requested "m" (models-fallback), and the usage chunk carries usage.cost.
const orCostTail = "data: {\"id\":\"gen-1\",\"model\":\"qwen/served\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: {\"id\":\"gen-1\",\"model\":\"qwen/served\",\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":6,\"total_tokens\":17,\"cost\":0.0123}}\n\n" +
	"data: [DONE]\n\n"

// streamCostRow runs one streamed turn against a backend of the given
// provider class and returns the web-chat llmlog row (recordEntry seam, no DB).
func streamCostRow(t *testing.T, class string) llmlog.Entry {
	t.Helper()
	var mu sync.Mutex
	var entries []llmlog.Entry
	orig := recordEntry
	recordEntry = func(_ *pgxpool.Pool, e llmlog.Entry) {
		mu.Lock()
		entries = append(entries, e)
		mu.Unlock()
	}
	t.Cleanup(func() { recordEntry = orig })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, sseDelta("hi")+orCostTail)
	}))
	t.Cleanup(srv.Close)

	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	adm := interactiveStreamAdmission(t, d)

	b := streamBackend("or", srv.URL)
	b.ProviderClass = class
	b.Locality = backends.LocalityExternal
	b.Trust = backends.TrustNoCredentials

	so := streamEngine().runStream(context.Background(), []backends.Backend{b},
		[]llm.ChatMsg{{Role: "user", Content: "hi"}}, false, false, 0, 1, "sess", "key-s", backends.SensPublic, adm, newEventSink())
	if !so.served || so.err != nil {
		t.Fatalf("outcome = served %v err %v", so.served, so.err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	return entries[0]
}

// TestRunStreamBooksOpenRouterCost (§C1): the tenant cost budget sums
// context_llm_log.cost_usd, so a web-chat turn on an openrouter-class backend
// must book the cost the provider reported in its final usage chunk — and the
// model that actually answered — exactly like the non-streaming funnel
// (llm.ApplyProviderTelemetry). A generic backend whose stream carries the
// same fields books neither: cost is an openrouter-class contract.
func TestRunStreamBooksOpenRouterCost(t *testing.T) {
	en := streamCostRow(t, backends.ProviderOpenRouter)
	if en.CostUSD == nil || *en.CostUSD != 0.0123 {
		t.Errorf("cost_usd = %v, want 0.0123 — a streamed OpenRouter answer must book its reported cost", en.CostUSD)
	}
	if en.Model != "qwen/served" {
		t.Errorf("model = %q, want the served model qwen/served", en.Model)
	}
	if en.Metadata["provider_request_id"] != "gen-1" {
		t.Errorf("metadata.provider_request_id = %v, want gen-1", en.Metadata["provider_request_id"])
	}
	if en.PromptTokens != 11 || en.CompletionTokens != 6 {
		t.Errorf("tokens = %d/%d, want 11/6", en.PromptTokens, en.CompletionTokens)
	}

	gen := streamCostRow(t, backends.ProviderGeneric)
	if gen.CostUSD != nil || gen.Model != "m" {
		t.Errorf("generic backend row: cost_usd = %v model = %q, want nil and the requested model m", gen.CostUSD, gen.Model)
	}
	if _, ok := gen.Metadata["provider_request_id"]; ok {
		t.Errorf("generic backend row carries provider_request_id %v", gen.Metadata["provider_request_id"])
	}
}
