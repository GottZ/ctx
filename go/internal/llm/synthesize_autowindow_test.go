// E10-W2 call-site probes: AUTO windows measured through the REAL synthesis
// path — pool chain, budget, prompt build, wire call. Every assertion reads
// the body the backend actually received; the discovery route is served by the
// same httptest stub as the chat route, so no seam is injected anywhere and
// the production cache is the one under test.
//
// No call reaches openrouter.ai: the stub IS the backend host, and the
// discovery URL derives from it.
package llm

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/promptguard"
)

// autoStub answers both routes of one openrouter-class backend: the discovery
// GET and the chat POST. It records the chat body and counts chat calls, so
// "this member was skipped" is measurable as an absence.
type autoStub struct {
	srv           *httptest.Server
	mu            sync.Mutex
	chatBody      map[string]any
	chatCalls     int
	discoveryErr  bool
	endpointsJSON string
}

func newAutoStub(t *testing.T, endpointsJSON string) *autoStub {
	t.Helper()
	s := &autoStub{endpointsJSON: endpointsJSON}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/endpoints") {
			s.mu.Lock()
			failing := s.discoveryErr
			body := s.endpointsJSON
			s.mu.Unlock()
			if failing {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = w.Write([]byte(body))
			return
		}
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		s.mu.Lock()
		s.chatBody = m
		s.chatCalls++
		s.mu.Unlock()
		_, _ = w.Write([]byte(orRespOK))
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *autoStub) chat() (map[string]any, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.chatBody, s.chatCalls
}

// endpointsJSON renders a discovery document from provider triples.
func endpointsJSON(eps ...backends.ProviderEndpoint) string {
	var sb strings.Builder
	// The model-level context_length is the MAXIMUM over the endpoints — the
	// number a lazy implementation would read. It is present in every fixture
	// so that reading it stays a detectable mistake.
	sb.WriteString(`{"data":{"id":"qwen/qwen3.6-27b","context_length":262144,"endpoints":[`)
	for i, e := range eps {
		if i > 0 {
			sb.WriteString(",")
		}
		b, _ := json.Marshal(map[string]any{
			"provider_name": e.ProviderName, "context_length": e.ContextLength,
			"max_completion_tokens": e.MaxCompletionTokens,
		})
		sb.Write(b)
	}
	sb.WriteString(`]}}`)
	return sb.String()
}

// autoBackend is an openrouter-class row with NULL num_ctx — the live shape
// this wave is about.
func autoBackend(id, host string, params map[string]any) backends.Backend {
	spec := backends.ModelSpec{Model: "qwen/qwen3.6-27b", Params: params}
	return backends.Backend{
		ID: id, Name: id, Host: host, Protocol: backends.ProtocolOpenAI,
		ProviderClass: backends.ProviderOpenRouter, Trust: backends.TrustFull,
		Locality: backends.LocalityExternal, Enabled: true, NumCtx: 0,
		Roles:    []string{backends.RoleSynthesis},
		ModelMap: map[string]backends.ModelSpec{"default": spec},
	}
}

// declaredBackend is an ordinary member that declares its own window — the
// failover leg AUTO members skip onto.
func declaredBackend(id, host string, numCtx int) backends.Backend {
	return backends.Backend{
		ID: id, Name: id, Host: host, Protocol: backends.ProtocolOpenAI,
		ProviderClass: backends.ProviderGeneric, Trust: backends.TrustFull,
		Enabled: true, NumCtx: numCtx, Roles: []string{backends.RoleSynthesis},
		ModelMap: map[string]backends.ModelSpec{"default": {Model: "local"}},
	}
}

func autoSettings(fallback int) SynthesisSettings {
	s := budgetSettings(fallback)
	s.OpenRouterWindowTTL = 3600
	return s
}

func runAutoSynthesize(t *testing.T, chain []backends.Backend, s SynthesisSettings) (*SynthesisResult, error) {
	t.Helper()
	bpool := backends.NewPool(nil, nil)
	bpool.SeedSnapshotForTest(chain)
	return Synthesize(testPrincipalCtx(), nil, bpool, nil, s, backends.SensPublic,
		"the question", budgetSources(20), nil, "", "", testAdmission(t, dispatch.ClassInteractive))
}

// providerOf reads request.provider as the wire carries it.
func providerOf(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	if body == nil {
		t.Fatal("no chat request was recorded")
	}
	prov, ok := body["provider"].(map[string]any)
	if !ok {
		t.Fatalf("request carries no provider object: %v", body)
	}
	return prov
}

func onlyList(t *testing.T, prov map[string]any) []string {
	t.Helper()
	raw, ok := prov["only"].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, _ := v.(string)
		out = append(out, s)
	}
	return out
}

// TestAutoWindowConstrainsToProvidersThatHoldThePrompt is probe (a) and probe
// (b) on the wire: NULL num_ctx on an openrouter row resolves through
// discovery, and the request that leaves ctx names EXACTLY the providers whose
// own context length holds it.
//
// Falsifying implementations, both caught here:
//   - AUTO window and eligibility taken from the MODEL-level context_length
//     (262144, present in the fixture): "Io Net" would stay in the only-list
//     and a 8192-token provider would be a legal route for a ~19k-token prompt.
//   - only max_tokens set, no provider.only ("OpenRouter will figure it out"):
//     the provider object then carries no only key at all — and OpenRouter's
//     routing does NOT filter on prompt size, so nothing else would.
func TestAutoWindowConstrainsToProvidersThatHoldThePrompt(t *testing.T) {
	stub := newAutoStub(t, endpointsJSON(
		backends.ProviderEndpoint{ProviderName: "Io Net", ContextLength: 8192, MaxCompletionTokens: 8192},
		backends.ProviderEndpoint{ProviderName: "DeepInfra", ContextLength: 262144, MaxCompletionTokens: 81920},
	))
	res, err := runAutoSynthesize(t, []backends.Backend{autoBackend("or", stub.srv.URL, nil)}, autoSettings(0))
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if res.Answer != "ok" {
		t.Errorf("answer = %q, want the stub answer", res.Answer)
	}
	body, calls := stub.chat()
	if calls != 1 {
		t.Fatalf("chat calls = %d, want 1", calls)
	}
	prov := providerOf(t, body)
	got := onlyList(t, prov)
	if len(got) == 0 {
		t.Fatalf("provider.only missing (%v) — OpenRouter does not filter on prompt size, so nothing constrains the input side", prov)
	}
	if len(got) != 1 || got[0] != "DeepInfra" {
		t.Errorf("provider.only = %v, want [DeepInfra] — an 8192-token provider cannot hold this prompt", got)
	}
	// The output side stays the second layer, unchanged from today.
	if body["max_tokens"] != float64(500) {
		t.Errorf("max_tokens = %v, want 500", body["max_tokens"])
	}
	// The forced ZDR terms must survive the merge (G29 contract).
	if prov["zdr"] != true || prov["data_collection"] != "deny" {
		t.Errorf("provider = %v — the forced zdr/deny terms were lost", prov)
	}
}

// TestAutoWindowHonoursCompletionBoundOnTheWire is probe (c) at the call site:
// a request whose answer bound exceeds a provider's max_completion_tokens must
// not be routed to that provider, even though its context length is the
// largest in the mix.
//
// Falsifying implementation: eligibility on context_length alone — "Chutes"
// (262144 / 65536) then appears in the only-list for a 70 000-token answer.
func TestAutoWindowHonoursCompletionBoundOnTheWire(t *testing.T) {
	stub := newAutoStub(t, endpointsJSON(
		backends.ProviderEndpoint{ProviderName: "Chutes", ContextLength: 262144, MaxCompletionTokens: 65536},
		backends.ProviderEndpoint{ProviderName: "SiliconFlow", ContextLength: 262144, MaxCompletionTokens: 262144},
	))
	b := autoBackend("or", stub.srv.URL, map[string]any{"max_tokens": float64(70000)})
	if _, err := runAutoSynthesize(t, []backends.Backend{b}, autoSettings(0)); err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	body, _ := stub.chat()
	got := onlyList(t, providerOf(t, body))
	if len(got) != 1 || got[0] != "SiliconFlow" {
		t.Errorf("provider.only = %v, want [SiliconFlow] — Chutes caps completions at 65536 < 70000", got)
	}
	if body["max_tokens"] != float64(70000) {
		t.Errorf("max_tokens = %v, want 70000 — the bound the filter used must be the bound the wire carries", body["max_tokens"])
	}
}

// TestAutoWindowSkipsMemberWithoutEligibleProvider is probe (f): a member no
// provider can serve is skipped for THIS request and the chain fails over.
//
// Falsifying implementation: hard-failing the call when the eligible set is
// empty. The second, perfectly capable chain member would then never be tried
// — a per-request routing constraint would have become an outage.
func TestAutoWindowSkipsMemberWithoutEligibleProvider(t *testing.T) {
	// Every provider can hold the prompt but none can produce the answer.
	orStub := newAutoStub(t, endpointsJSON(
		backends.ProviderEndpoint{ProviderName: "Chutes", ContextLength: 262144, MaxCompletionTokens: 1000},
		backends.ProviderEndpoint{ProviderName: "Io Net", ContextLength: 262144, MaxCompletionTokens: 900},
	))
	localStub := newAutoStub(t, "")
	chain := []backends.Backend{
		autoBackend("or", orStub.srv.URL, map[string]any{"max_tokens": float64(70000)}),
		declaredBackend("local", localStub.srv.URL, 32768),
	}
	res, err := runAutoSynthesize(t, chain, autoSettings(0))
	if err != nil {
		t.Fatalf("Synthesize: %v — an unroutable member must be SKIPPED, not fail the call", err)
	}
	if res.Answer != "ok" {
		t.Errorf("answer = %q, want the stub answer", res.Answer)
	}
	if _, calls := orStub.chat(); calls != 0 {
		t.Errorf("the unroutable member was called %d times — the skip did not happen", calls)
	}
	if _, calls := localStub.chat(); calls != 1 {
		t.Errorf("the failover member was called %d times, want 1", calls)
	}
}

// TestAutoWindowEmptyChainStaysAnError: skipping is failover, not silence — a
// chain in which NO member can serve the prompt still errors, and contacts
// nobody.
func TestAutoWindowEmptyChainStaysAnError(t *testing.T) {
	stub := newAutoStub(t, endpointsJSON(
		backends.ProviderEndpoint{ProviderName: "Chutes", ContextLength: 262144, MaxCompletionTokens: 1000},
	))
	b := autoBackend("or", stub.srv.URL, map[string]any{"max_tokens": float64(70000)})
	_, err := runAutoSynthesize(t, []backends.Backend{b}, autoSettings(0))
	var noBackend *backends.ErrNoEligibleBackend
	if !errors.As(err, &noBackend) {
		t.Errorf("err = %v, want *backends.ErrNoEligibleBackend", err)
	}
	if _, calls := stub.chat(); calls != 0 {
		t.Errorf("chat calls = %d, want 0", calls)
	}
}

// TestAutoWindowPreservesOperatorExtraBody is probe (g): the operator's own
// provider fields survive the constraint, and the constraint respects them.
//
// Falsifying implementation: replacing the provider object with the computed
// only-list. provider.ignore and allow_fallbacks would vanish — an operator's
// deliberate exclusion silently undone by a routing optimisation.
func TestAutoWindowPreservesOperatorExtraBody(t *testing.T) {
	stub := newAutoStub(t, endpointsJSON(
		backends.ProviderEndpoint{ProviderName: "Io Net", ContextLength: 8192, MaxCompletionTokens: 8192},
		backends.ProviderEndpoint{ProviderName: "Phala", ContextLength: 262144, MaxCompletionTokens: 262140},
		backends.ProviderEndpoint{ProviderName: "DeepInfra", ContextLength: 262144, MaxCompletionTokens: 81920},
	))
	b := autoBackend("or", stub.srv.URL, nil)
	b.ExtraBody = map[string]any{
		"provider": map[string]any{"ignore": []any{"Phala"}, "allow_fallbacks": false},
	}
	if _, err := runAutoSynthesize(t, []backends.Backend{b}, autoSettings(0)); err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	body, _ := stub.chat()
	prov := providerOf(t, body)
	if got := onlyList(t, prov); len(got) != 1 || got[0] != "DeepInfra" {
		t.Errorf("provider.only = %v, want [DeepInfra] — Io Net is too small, Phala is operator-ignored", got)
	}
	ignore, _ := prov["ignore"].([]any)
	if len(ignore) != 1 || ignore[0] != "Phala" {
		t.Errorf("provider.ignore = %v — the operator's exclusion was dropped", prov["ignore"])
	}
	if prov["allow_fallbacks"] != false {
		t.Errorf("provider.allow_fallbacks = %v — the operator's field was dropped", prov["allow_fallbacks"])
	}
	if prov["zdr"] != true || prov["data_collection"] != "deny" {
		t.Errorf("provider = %v — the forced zdr/deny terms were lost", prov)
	}
	// The snapshot's map must be untouched: it is shared by every later
	// request on this backend.
	if _, leaked := b.ExtraBody["provider"].(map[string]any)["only"]; leaked {
		t.Error("the constraint was written into the backend row's extra_body — later requests would inherit this request's provider set")
	}
}

// TestAutoWindowFailsClosedWithoutDiscovery is probe (h): the H12 floor is
// unchanged. Discovery that answers nothing leaves an openrouter row with NULL
// num_ctx exactly as undeclared as it was before this wave — no prompt is
// built, no backend is contacted.
//
// Falsifying implementation: substituting a rate value (the model-level
// context_length, a compiled-in 32768) when discovery fails. That variant
// reaches the wire; this case requires zero wire contact.
func TestAutoWindowFailsClosedWithoutDiscovery(t *testing.T) {
	stub := newAutoStub(t, "")
	stub.discoveryErr = true
	_, err := runAutoSynthesize(t, []backends.Backend{autoBackend("or", stub.srv.URL, nil)}, autoSettings(0))
	if !errors.Is(err, promptguard.ErrUndeclaredWindow) {
		t.Errorf("err = %v, want promptguard.ErrUndeclaredWindow", err)
	}
	if _, calls := stub.chat(); calls != 0 {
		t.Errorf("chat calls = %d — the prompt must not be built at all", calls)
	}
}

// TestAutoWindowDiscoveryOffFallsBackToOperatorFloor: with discovery off the
// row behaves exactly as it did under H12 — the operator fallback carries it,
// and no provider constraint is invented from data ctx does not have.
func TestAutoWindowDiscoveryOffFallsBackToOperatorFloor(t *testing.T) {
	stub := newAutoStub(t, endpointsJSON(
		backends.ProviderEndpoint{ProviderName: "DeepInfra", ContextLength: 262144, MaxCompletionTokens: 81920},
	))
	s := autoSettings(32768)
	s.OpenRouterWindowTTL = 0
	if _, err := runAutoSynthesize(t, []backends.Backend{autoBackend("or", stub.srv.URL, nil)}, s); err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	body, calls := stub.chat()
	if calls != 1 {
		t.Fatalf("chat calls = %d, want 1", calls)
	}
	prov, _ := body["provider"].(map[string]any)
	if prov != nil {
		if _, present := prov["only"]; present {
			t.Errorf("provider.only = %v with discovery off — a constraint was invented without endpoint data", prov["only"])
		}
	}
}
