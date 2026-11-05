package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/httpx"
)

// Gateway passthrough (wave W02-06, design/02 §4.5.2, §4.5.3, §4.11.1).
// Overlay tests drive BuildUpstreamBody → ChatRaw against a capture fake, so
// every assertion is on the bytes the backend actually received.

const ptOKReply = `{"id":"up-1","model":"served","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18,"cost":0.25}}`

var ptMessages = json.RawMessage(`[{"role":"user","content":"hi"}]`)

// ptRow is a chat row of the live shape (openai wire, llamacpp class).
func ptRow(host string) backends.Backend {
	return backends.Backend{
		ID: "row-id", Name: "row", Host: host, Protocol: backends.ProtocolOpenAI,
		ProviderClass: "llamacpp", Scope: backends.GlobalScope, APIKey: "ROW-KEY",
		Roles:    []string{"chat"},
		ModelMap: map[string]backends.ModelSpec{"chat": {Model: "row-model"}},
	}
}

func ptClient(t *testing.T, kv map[string]any) map[string]json.RawMessage {
	t.Helper()
	m := make(map[string]json.RawMessage, len(kv))
	for k, v := range kv {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal client %s: %v", k, err)
		}
		m[k] = raw
	}
	return m
}

func ptSpec(client map[string]json.RawMessage) UpstreamSpec {
	return UpstreamSpec{Client: client, Messages: ptMessages, Role: "chat", MaxTokens: 1024}
}

// ptWire builds the body for b, sends it through ChatRaw to a capture fake
// and returns the decoded body the fake received.
func ptWire(t *testing.T, s UpstreamSpec, b backends.Backend) map[string]any {
	t.Helper()
	srv, body, _ := captureRequestBody(t, ptOKReply)
	defer srv.Close()
	b.Host = srv.URL
	raw, err := BuildUpstreamBody(s, &b)
	if err != nil {
		t.Fatalf("BuildUpstreamBody: %v", err)
	}
	if _, err := ChatRaw(context.Background(), b, raw, 5*time.Second, httpx.MaxResponseBytes); err != nil {
		t.Fatalf("ChatRaw: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(*body, &got); err != nil {
		t.Fatalf("decode wire body %q: %v", *body, err)
	}
	return got
}

func ptThinking(t *testing.T, body map[string]any) any {
	t.Helper()
	ctk, ok := body["chat_template_kwargs"].(map[string]any)
	if !ok {
		t.Fatalf("wire body without chat_template_kwargs object: %v", body["chat_template_kwargs"])
	}
	return ctk["enable_thinking"]
}

// B2: the row's extra_body thinking switch wins over the client's (stage 5
// after stage 2) — the live spark-chat force (ctx 01a01a82).
func TestRowExtraBodyWinsOverClient(t *testing.T) {
	b := ptRow("")
	b.ExtraBody = map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": false}}
	client := ptClient(t, map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": true}})
	if got := ptThinking(t, ptWire(t, ptSpec(client), b)); got != false {
		t.Fatalf("enable_thinking on the wire = %v, want false (row extra_body wins over the client)", got)
	}
}

// B2b: a row that carries the thinking switch only in model_map params
// (vLLM/LiteLLM pattern) forces it too — stage 5a via applyModelParams.
func TestModelMapThinkingWinsOverClient(t *testing.T) {
	b := ptRow("")
	b.ModelMap = map[string]backends.ModelSpec{"chat": {Model: "row-model", Params: map[string]any{
		"chat_template_kwargs": map[string]any{"enable_thinking": false},
	}}}
	client := ptClient(t, map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": true}})
	if got := ptThinking(t, ptWire(t, ptSpec(client), b)); got != false {
		t.Fatalf("enable_thinking on the wire = %v, want false (model_map params win over the client)", got)
	}
}

// Stage 5 after 5a: on the same key the row's extra_body wins over its
// model_map params.
func TestRowExtraBodyWinsOverModelMap(t *testing.T) {
	b := ptRow("")
	b.ModelMap = map[string]backends.ModelSpec{"chat": {Model: "row-model", Params: map[string]any{
		"chat_template_kwargs": map[string]any{"enable_thinking": true},
	}}}
	b.ExtraBody = map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": false}}
	if got := ptThinking(t, ptWire(t, ptSpec(nil), b)); got != false {
		t.Fatalf("enable_thinking on the wire = %v, want false (extra_body after model_map)", got)
	}
}

// Review (CodeRabbit on PR #94): the prompt is a frame key. A row whose
// model_map params or extra_body carry `messages` (a misconfiguration,
// silent otherwise) never replaces what the client sent.
func TestClientMessagesSurviveRowConfig(t *testing.T) {
	var want any
	if err := json.Unmarshal(ptMessages, &want); err != nil {
		t.Fatal(err)
	}
	rogue := []any{map[string]any{"role": "user", "content": "rogue"}}
	b := ptRow("")
	b.ModelMap = map[string]backends.ModelSpec{"chat": {Model: "row-model", Params: map[string]any{"messages": rogue}}}
	if got := ptWire(t, ptSpec(nil), b)["messages"]; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("model_map messages: wire messages = %v, want the client's %v", got, want)
	}
	b.ModelMap = map[string]backends.ModelSpec{"chat": {Model: "row-model"}}
	b.ExtraBody = map[string]any{"messages": rogue}
	if got := ptWire(t, ptSpec(nil), b)["messages"]; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("extra_body messages: wire messages = %v, want the client's %v", got, want)
	}
}

// Stage 1: row sampling params are defaults only — the client's value wins,
// an absent client key takes the row's.
func TestRowSamplingIsOnlyDefault(t *testing.T) {
	b := ptRow("")
	b.ModelMap = map[string]backends.ModelSpec{"chat": {Model: "row-model", Params: map[string]any{
		"temperature": 0.2, "top_p": 0.8, "top_k": float64(20), "min_p": float64(0),
		"presence_penalty": 0.5, "frequency_penalty": 0.3,
	}}}
	client := ptClient(t, map[string]any{"temperature": 0.9, "frequency_penalty": 0.1})
	got := ptWire(t, ptSpec(client), b)
	want := map[string]any{"temperature": 0.9, "frequency_penalty": 0.1, "top_p": 0.8, "top_k": float64(20), "presence_penalty": 0.5}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s on the wire = %v, want %v", k, got[k], v)
		}
	}
	// min_p 0 is the disabled value: omitted exactly like the internal openai
	// wire's omitempty (a require_parameters route must not narrow on it).
	if v, ok := got["min_p"]; ok {
		t.Errorf("min_p 0 reached the wire (%v), want omitted", v)
	}

	// temperature 0 is greedy decoding, not "unset": a row that names it
	// sends it when the client does not.
	b.ModelMap = map[string]backends.ModelSpec{"chat": {Model: "row-model", Params: map[string]any{"temperature": float64(0)}}}
	got = ptWire(t, ptSpec(nil), b)
	if v, ok := got["temperature"]; !ok || v != float64(0) {
		t.Errorf("row temperature 0: wire temperature = %v (present %v), want 0", v, ok)
	}
	// A temperature that is no number is no default (it would otherwise go
	// out as the zero applyModelParams left).
	b.ModelMap = map[string]backends.ModelSpec{"chat": {Model: "row-model", Params: map[string]any{"temperature": "hot"}}}
	if v, ok := ptWire(t, ptSpec(nil), b)["temperature"]; ok {
		t.Errorf("row temperature \"hot\": wire temperature = %v, want absent", v)
	}
	// A row without sampling params sends none of its own.
	b.ModelMap = map[string]backends.ModelSpec{"chat": {Model: "row-model"}}
	got = ptWire(t, ptSpec(nil), b)
	for _, k := range []string{"temperature", "top_p", "top_k", "min_p", "presence_penalty", "frequency_penalty"} {
		if v, ok := got[k]; ok {
			t.Errorf("row without params: %s = %v reached the wire", k, v)
		}
	}
}

// B1: the CTX keys of the client map never reach the wire with the client's
// value — the spec is allowlist-filtered upstream, this is the defense
// against a caller error.
func TestUpstreamBodyStripsClientCTXKeys(t *testing.T) {
	client := ptClient(t, map[string]any{
		"model": "evil", "n": 4, "stream": true, "stream_options": map[string]any{"include_usage": false},
		"max_tokens": 999999, "max_completion_tokens": 999999, "reasoning": map[string]any{"enabled": true},
		"reasoning_effort": "high", "service_tier": "priority",
		"messages":    []any{map[string]any{"role": "user", "content": "smuggled"}},
		"temperature": 0.4,
	})
	got := ptWire(t, ptSpec(client), ptRow(""))
	if got["model"] != "row-model" {
		t.Errorf("model = %v, want the row's ModelFor(role).Model", got["model"])
	}
	if got["stream"] != false {
		t.Errorf("stream = %v, want false (non-stream spec)", got["stream"])
	}
	if got["max_tokens"] != float64(1024) {
		t.Errorf("max_tokens = %v, want the spec's 1024", got["max_tokens"])
	}
	for _, k := range []string{"n", "stream_options", "max_completion_tokens", "reasoning", "reasoning_effort", "service_tier"} {
		if v, ok := got[k]; ok {
			t.Errorf("client CTX key %s reached the wire: %v", k, v)
		}
	}
	msgs, _ := json.Marshal(got["messages"])
	if string(msgs) != `[{"content":"hi","role":"user"}]` {
		t.Errorf("messages = %s, want the spec's messages", msgs)
	}
	if got["temperature"] != 0.4 {
		t.Errorf("PASS key temperature = %v, want the client's 0.4", got["temperature"])
	}
}

// E4: no n reaches the wire from any stage — not from the client, not from
// model_map params, not from extra_body (n > 1 is RC-F).
func TestNoNReachesTheWire(t *testing.T) {
	b := ptRow("")
	b.ModelMap = map[string]backends.ModelSpec{"chat": {Model: "row-model", Params: map[string]any{"n": float64(2)}}}
	b.ExtraBody = map[string]any{"n": float64(3)}
	got := ptWire(t, ptSpec(ptClient(t, map[string]any{"n": 4})), b)
	if v, ok := got["n"]; ok {
		t.Fatalf("n = %v reached the wire, want absent (engine default 1)", v)
	}
}

// B10: max_tokens = min(spec, row limits.chat_max_tokens > 0, model_map cap
// > 0); a spec without a ceiling is an error — the ceiling always comes from
// the caller.
func TestMaxTokensClampedByRowCaps(t *testing.T) {
	spec := ptSpec(nil)
	spec.MaxTokens = 262144

	b := ptRow("")
	b.Limits = map[string]any{"chat_max_tokens": float64(8192)}
	if got := ptWire(t, spec, b)["max_tokens"]; got != float64(8192) {
		t.Errorf("row cap 8192: max_tokens = %v, want 8192", got)
	}
	b.Limits = nil
	if got := ptWire(t, spec, b)["max_tokens"]; got != float64(262144) {
		t.Errorf("row without cap: max_tokens = %v, want the spec's 262144", got)
	}
	b.ModelMap = map[string]backends.ModelSpec{"chat": {Model: "row-model", Params: map[string]any{"max_tokens": float64(4096)}}}
	if got := ptWire(t, spec, b)["max_tokens"]; got != float64(4096) {
		t.Errorf("model_map cap 4096: max_tokens = %v, want 4096", got)
	}
	// extra_body cannot lift the min(): the frame keys are written last.
	b.ModelMap = map[string]backends.ModelSpec{"chat": {Model: "row-model"}}
	b.Limits = map[string]any{"chat_max_tokens": float64(8192)}
	b.ExtraBody = map[string]any{"max_tokens": float64(999999)}
	if got := ptWire(t, spec, b)["max_tokens"]; got != float64(8192) {
		t.Errorf("extra_body max_tokens: wire max_tokens = %v, want the min() 8192", got)
	}

	for _, mt := range []int{0, -1} {
		spec.MaxTokens = mt
		if _, err := BuildUpstreamBody(spec, &b); err == nil {
			t.Errorf("MaxTokens %d: BuildUpstreamBody succeeded, want an error", mt)
		}
	}
}

// Stream frame: stream=true and stream_options.include_usage=true always (the
// count needs the usage chunk), whatever the client asked for.
func TestStreamFrameForcesUsage(t *testing.T) {
	spec := ptSpec(ptClient(t, map[string]any{"stream_options": map[string]any{"include_usage": false}}))
	spec.Stream = true
	raw, err := BuildUpstreamBody(spec, func() *backends.Backend { b := ptRow("http://x"); return &b }())
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["stream"] != true {
		t.Errorf("stream = %v, want true", got["stream"])
	}
	so, _ := got["stream_options"].(map[string]any)
	if so == nil || so["include_usage"] != true || len(so) != 1 {
		t.Errorf("stream_options = %v, want exactly {include_usage:true}", got["stream_options"])
	}
}

// Stage 4 (deviation from design §4.5.2, following the Ist chatOpenAI
// :364-370): reasoning off is provider-independent; a row think=false acts
// like the policy; without either no reasoning field is sent.
func TestReasoningOffProviderIndependent(t *testing.T) {
	spec := ptSpec(ptClient(t, map[string]any{"reasoning_effort": "high"}))
	spec.ReasoningOff = true
	got := ptWire(t, spec, ptRow(""))
	r, _ := got["reasoning"].(map[string]any)
	if r == nil || r["enabled"] != false || r["exclude"] != true {
		t.Errorf("ReasoningOff on a llamacpp row: reasoning = %v, want {enabled:false, exclude:true}", got["reasoning"])
	}
	if _, ok := got["reasoning_effort"]; ok {
		t.Errorf("client reasoning_effort reached the wire under ReasoningOff")
	}

	b := ptRow("")
	b.ModelMap = map[string]backends.ModelSpec{"chat": {Model: "row-model", Params: map[string]any{"think": false}}}
	if r, _ := ptWire(t, ptSpec(nil), b)["reasoning"].(map[string]any); r == nil || r["enabled"] != false {
		t.Errorf("row think=false: reasoning = %v, want off", r)
	}
	if v, ok := ptWire(t, ptSpec(nil), ptRow(""))["reasoning"]; ok {
		t.Errorf("no policy, no row think: reasoning = %v reached the wire", v)
	}
}

// Stage 5: the OpenRouter ZDR force runs last on the passthrough too — over
// the row's extra_body and over a provider object a caller error let through
// — and it never writes into the row's extra_body (the pool snapshot).
func TestOpenRouterZDRLastOnPassthrough(t *testing.T) {
	b := ptRow("")
	b.ProviderClass = backends.ProviderOpenRouter
	b.ExtraBody = map[string]any{"provider": map[string]any{"require_parameters": true, "zdr": false}}
	before, _ := json.Marshal(b.ExtraBody)
	got := ptWire(t, ptSpec(ptClient(t, map[string]any{"provider": map[string]any{"data_collection": "allow"}})), b)
	prov, _ := got["provider"].(map[string]any)
	if prov == nil || prov["zdr"] != true || prov["data_collection"] != "deny" || prov["require_parameters"] != true {
		t.Fatalf("provider = %v, want zdr:true, data_collection:deny, require_parameters:true", got["provider"])
	}
	if after, _ := json.Marshal(b.ExtraBody); string(after) != string(before) {
		t.Errorf("the force wrote into the row's extra_body: before %s, after %s", before, after)
	}

	// The confirm-gated escape keeps working.
	b.Metadata = map[string]any{"allow_data_collection": true}
	b.ExtraBody = nil
	if v, ok := ptWire(t, ptSpec(nil), b)["provider"]; ok {
		t.Errorf("escape set: provider = %v reached the wire, want none", v)
	}
	// A non-openrouter row gets no force.
	if v, ok := ptWire(t, ptSpec(nil), ptRow(""))["provider"]; ok {
		t.Errorf("llamacpp row: provider = %v, want none", v)
	}
}

// "Client map verbatim": a client number beyond float64 precision reaches
// the wire digit for digit, also on a row whose extra_body/ZDR stage runs.
func TestClientNumbersReachWireVerbatim(t *testing.T) {
	const seed = "18446744073709551615"
	client := map[string]json.RawMessage{"seed": json.RawMessage(seed)}
	b := ptRow("")
	b.ProviderClass = backends.ProviderOpenRouter
	b.ExtraBody = map[string]any{"provider": map[string]any{"require_parameters": true}}
	srv, body, _ := captureRequestBody(t, ptOKReply)
	defer srv.Close()
	b.Host = srv.URL
	raw, err := BuildUpstreamBody(ptSpec(client), &b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ChatRaw(context.Background(), b, raw, 5*time.Second, httpx.MaxResponseBytes); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(*body), `"seed":`+seed) {
		t.Fatalf("wire body lost the client's seed digits: %s", *body)
	}
}

func TestBuildUpstreamBodyRefusesIncompleteSpec(t *testing.T) {
	b := ptRow("http://x")
	if _, err := BuildUpstreamBody(UpstreamSpec{Role: "chat", MaxTokens: 10}, &b); err == nil {
		t.Error("spec without messages: want an error")
	}
	b.ModelMap = nil
	if _, err := BuildUpstreamBody(ptSpec(nil), &b); err == nil {
		t.Error("row without a model for the role: want an error")
	}
}

// --- header capture (design/04 §5.2 S1, R-02-2, I-F1) ---.

// ptInbound is the credential-bearing header set of an inbound /v1 request.
// No wire function takes it — the assertion is that none of it reaches the
// backend, whatever the caller holds.
func ptInbound() *http.Request {
	in := httptest.NewRequest(http.MethodPost, "http://ctx.example/v1/chat/completions", nil)
	in.Header.Set("Authorization", "Bearer CLIENT-KEY")
	in.Header.Set("X-Api-Key", "CLIENT-X-API-KEY")
	in.Header.Set("Cookie", "ctx_session=CLIENT-COOKIE")
	in.Header.Set("Ctx-Foo", "CLIENT-CTX")
	in.Header.Set("X-Request-ID", "client-chosen-id")
	return in
}

// ptTransportHeaders are what net/http adds on its own.
var ptTransportHeaders = []string{"Accept-Encoding", "Content-Length", "User-Agent"}

func ptHeaderNames(h http.Header) []string {
	var names []string
	for k := range h {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

func TestPassthroughHeaderCapture(t *testing.T) {
	for _, stream := range []bool{false, true} {
		reply := ptOKReply
		if stream {
			reply = minimalStopStream
		}
		srv, _, hdr := captureRequestBody(t, reply)
		b := ptRow(srv.URL)
		b.ExtraHeaders = map[string]string{"X-Title": "ctx", "HTTP-Referer": "https://ctx.example"}
		inbound := ptInbound()
		var err error
		if stream {
			_, err = ChatRawStream(inbound.Context(), b, []byte(`{"stream":true}`), 5*time.Second, 1<<20, func([]byte) error { return nil })
		} else {
			_, err = ChatRaw(inbound.Context(), b, []byte(`{}`), 5*time.Second, httpx.MaxResponseBytes)
		}
		srv.Close()
		if err != nil {
			t.Fatalf("stream=%v: %v", stream, err)
		}
		want := append([]string{"Authorization", "Content-Type", "Http-Referer", "X-Title"}, ptTransportHeaders...)
		if stream {
			want = append(want, "Accept")
		}
		sort.Strings(want)
		if got := ptHeaderNames(*hdr); !reflect.DeepEqual(got, want) {
			t.Errorf("stream=%v: backend saw headers %v, want exactly %v", stream, got, want)
		}
		if got := hdr.Get("Authorization"); got != "Bearer ROW-KEY" {
			t.Errorf("stream=%v: Authorization = %q, want the row's key", stream, got)
		}
		for _, k := range []string{"X-Api-Key", "Cookie", "Ctx-Foo", "X-Request-Id"} {
			if v := hdr.Get(k); v != "" {
				t.Errorf("stream=%v: inbound %s reached the backend: %q", stream, k, v)
			}
		}
		if stream && hdr.Get("Accept") != "text/event-stream" {
			t.Errorf("stream: Accept = %q", hdr.Get("Accept"))
		}
	}
}

// The exact wire signatures (R-02-2): a further parameter of ANY type — a
// header map under another name, a variadic, a client — breaks this file's
// compilation, so the gate is red by construction.
var (
	_ func(context.Context, backends.Backend, []byte, time.Duration, int64) (*RawChatResult, error)                       = ChatRaw
	_ func(context.Context, backends.Backend, []byte, time.Duration, int64, func([]byte) error) (*RawStreamResult, error) = ChatRawStream
)

// TestChatRawSignatureHasNoHeaderOrClientParam pins R-02-2 structurally: no
// parameter of a function in passthrough.go that builds an upstream request
// (http.NewRequestWithContext) names a net/http or httpx type — no header
// map, no inbound request, no client. Keyed on the request construction, not
// on the two names, so a renamed hdr-taking helper behind ChatRaw is caught.
func TestChatRawSignatureHasNoHeaderOrClientParam(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "passthrough.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var wireFuncs []string
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		builds := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "NewRequestWithContext" {
					builds = true
				}
			}
			return true
		})
		if !builds {
			continue
		}
		wireFuncs = append(wireFuncs, fn.Name.Name)
		for _, p := range fn.Type.Params.List {
			ast.Inspect(p.Type, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok {
					if id, ok := sel.X.(*ast.Ident); ok && (id.Name == "http" || id.Name == "httpx") {
						t.Errorf("%s has a parameter of type %s.%s (R-02-2: headers come only from the row)", fn.Name.Name, id.Name, sel.Sel.Name)
					}
				}
				return true
			})
		}
	}
	sort.Strings(wireFuncs)
	if !reflect.DeepEqual(wireFuncs, []string{"ChatRaw", "ChatRawStream"}) {
		t.Errorf("request-building functions in passthrough.go = %v, want exactly [ChatRaw ChatRawStream]", wireFuncs)
	}
}

// --- egress (B19 behaviour half; the AST half lives in internal/chat) ---.

func TestPassthroughTenantRowNeverDialsLoopback(t *testing.T) {
	srv, hits := newLoopbackWire(t)
	tenant := ptRow(srv.URL)
	tenant.Scope = "acme"
	_, err := ChatRaw(context.Background(), tenant, []byte(`{}`), 5*time.Second, httpx.MaxResponseBytes)
	assertGuardRefusal(t, "ChatRaw", err, hits)
	_, err = ChatRawStream(context.Background(), tenant, []byte(`{"stream":true}`), 5*time.Second, 1<<20, func([]byte) error { return nil })
	assertGuardRefusal(t, "ChatRawStream", err, hits)

	// Positive control: the same server answers a _global row on both wires.
	global := ptRow(srv.URL)
	if _, err := ChatRaw(context.Background(), global, []byte(`{}`), 5*time.Second, httpx.MaxResponseBytes); err != nil {
		t.Errorf("_global ChatRaw: %v", err)
	}
	if _, err := ChatRawStream(context.Background(), global, []byte(`{"stream":true}`), 5*time.Second, 1<<20, func([]byte) error { return nil }); err != nil {
		t.Errorf("_global ChatRawStream: %v", err)
	}
	if hits.Load() != 2 {
		t.Errorf("loopback hits = %d, want 2 (the _global controls only)", hits.Load())
	}
}

func TestPassthroughRefusesNonOpenAIWire(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	for _, p := range []backends.Protocol{backends.ProtocolAnthropic, "rerank", ""} {
		b := ptRow(srv.URL)
		b.Protocol = p
		if _, err := ChatRaw(context.Background(), b, []byte(`{}`), time.Second, httpx.MaxResponseBytes); !errors.Is(err, ErrPassthroughProtocol) {
			t.Errorf("ChatRaw protocol %q: err = %v, want ErrPassthroughProtocol", p, err)
		}
		if _, err := ChatRawStream(context.Background(), b, []byte(`{}`), time.Second, 1<<20, func([]byte) error { return nil }); !errors.Is(err, ErrPassthroughProtocol) {
			t.Errorf("ChatRawStream protocol %q: err = %v, want ErrPassthroughProtocol", p, err)
		}
	}
	if hits.Load() != 0 {
		t.Errorf("a refused protocol reached the wire %d time(s)", hits.Load())
	}
}

// --- ChatRaw / ChatRawStream response handling ---.

func TestChatRawReturnsBodyVerbatim(t *testing.T) {
	srv, _, _ := captureRequestBody(t, ptOKReply)
	defer srv.Close()
	b := ptRow(srv.URL)
	res, err := ChatRaw(context.Background(), b, []byte(`{}`), 5*time.Second, httpx.MaxResponseBytes)
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Body) != ptOKReply {
		t.Errorf("Body = %s, want the upstream body verbatim", res.Body)
	}
	if res.PromptTokens != 11 || res.CompletionTokens != 7 || res.FinishReason != "stop" {
		t.Errorf("usage/finish = %d/%d/%q, want 11/7/stop", res.PromptTokens, res.CompletionTokens, res.FinishReason)
	}
	if res.CostUSD != nil || res.ServedModel != "" || res.ProviderRequestID != "" {
		t.Errorf("llamacpp row carries openrouter provenance: %+v", res)
	}

	b.ProviderClass = backends.ProviderOpenRouter
	b.Metadata = map[string]any{"allow_data_collection": true}
	res, err = ChatRaw(context.Background(), b, []byte(`{}`), 5*time.Second, httpx.MaxResponseBytes)
	if err != nil {
		t.Fatal(err)
	}
	if res.CostUSD == nil || *res.CostUSD != 0.25 || res.ServedModel != "served" || res.ProviderRequestID != "up-1" {
		t.Errorf("openrouter provenance = %+v, want cost 0.25, served, up-1", res)
	}
}

func TestChatRawErrors(t *testing.T) {
	var status atomic.Int64
	var reply atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(status.Load()))
		_, _ = io.WriteString(w, reply.Load().(string))
	}))
	defer srv.Close()
	b := ptRow(srv.URL)

	status.Store(400)
	reply.Store(`{"error":{"message":"context too long"}}`)
	_, err := ChatRaw(context.Background(), b, []byte(`{}`), 5*time.Second, httpx.MaxResponseBytes)
	var se *httpx.StatusError
	if !errors.As(err, &se) || se.Code != 400 {
		t.Errorf("400: err = %v, want StatusError{400}", err)
	}

	status.Store(200)
	reply.Store(ptOKReply)
	if _, err := ChatRaw(context.Background(), b, []byte(`{}`), 5*time.Second, 16); !errors.Is(err, httpx.ErrResponseTooLarge) {
		t.Errorf("maxResp 16: err = %v, want ErrResponseTooLarge", err)
	}
	reply.Store(`{"choices":[]}`)
	if _, err := ChatRaw(context.Background(), b, []byte(`{}`), 5*time.Second, httpx.MaxResponseBytes); err == nil {
		t.Error("empty choices: want an error")
	}
	reply.Store(`not json`)
	if _, err := ChatRaw(context.Background(), b, []byte(`{}`), 5*time.Second, httpx.MaxResponseBytes); err == nil {
		t.Error("undecodable body: want an error")
	}
	if _, err := ChatRaw(context.Background(), b, []byte(`{}`), 0, httpx.MaxResponseBytes); err == nil {
		t.Error("timeout 0: want an error")
	}
}

// maxResp is clamped to httpx.MaxResponseBytes: a larger (or ≤ 0) cap reads
// at most that many bytes — a valid answer padded one byte past the harness
// cap is refused, never decoded (fremd-review 2 W02-06 F-2).
func TestChatRawClampsResponseCap(t *testing.T) {
	padded := ptOKReply + strings.Repeat(" ", int(httpx.MaxResponseBytes)-len(ptOKReply)+1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, padded)
	}))
	defer srv.Close()
	for _, maxResp := range []int64{2 * httpx.MaxResponseBytes, 0, -1} {
		if _, err := ChatRaw(context.Background(), ptRow(srv.URL), []byte(`{}`), 30*time.Second, maxResp); !errors.Is(err, httpx.ErrResponseTooLarge) {
			t.Errorf("maxResp %d, body MaxResponseBytes+1: err = %v, want ErrResponseTooLarge (clamped to the harness cap)", maxResp, err)
		}
	}
}

const ptStream = ": OPENROUTER PROCESSING\n\n" +
	"data: {\"id\":\"up-s\",\"model\":\"served\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"up-s\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"up-s\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: {\"id\":\"up-s\",\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"cost\":0.5}}\n\n" +
	"data: [DONE]\n\n"

func ptStreamServer(t *testing.T, sse string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestChatRawStreamRelaysFramesVerbatim(t *testing.T) {
	srv := ptStreamServer(t, ptStream)
	var frames []string
	res, err := ChatRawStream(context.Background(), ptRow(srv.URL), []byte(`{"stream":true}`), 5*time.Second, 1<<20,
		func(p []byte) error { frames = append(frames, string(p)); return nil })
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		`{"id":"up-s","model":"served","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
		`{"id":"up-s","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}`,
		`{"id":"up-s","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"id":"up-s","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"cost":0.5}}`,
	}
	if !reflect.DeepEqual(frames, want) {
		t.Fatalf("frames = %q\nwant %q (comments and [DONE] never relayed)", frames, want)
	}
	if res.Frames != 4 || res.PromptTokens != 10 || res.CompletionTokens != 5 || res.FinishReason != "stop" {
		t.Errorf("result = %+v, want 4 frames, 10/5, stop", res)
	}
	if res.CostUSD != nil || res.ServedModel != "" || res.ProviderRequestID != "" {
		t.Errorf("llamacpp row carries openrouter provenance: %+v", res)
	}
	b := ptRow(srv.URL)
	b.ProviderClass = backends.ProviderOpenRouter
	b.Metadata = map[string]any{"allow_data_collection": true}
	res, err = ChatRawStream(context.Background(), b, []byte(`{"stream":true}`), 5*time.Second, 1<<20, func([]byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if res.CostUSD == nil || *res.CostUSD != 0.5 || res.ServedModel != "served" || res.ProviderRequestID != "up-s" {
		t.Errorf("openrouter provenance = %+v", res)
	}
}

func TestChatRawStreamTimingsFallback(t *testing.T) {
	srv := ptStreamServer(t, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"timings\":{\"prompt_n\":9,\"predicted_n\":3}}\n\ndata: [DONE]\n\n")
	res, err := ChatRawStream(context.Background(), ptRow(srv.URL), []byte(`{}`), 5*time.Second, 1<<20, func([]byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if res.PromptTokens != 9 || res.CompletionTokens != 3 {
		t.Errorf("timings fallback = %d/%d, want 9/3", res.PromptTokens, res.CompletionTokens)
	}
}

func TestChatRawStreamErrors(t *testing.T) {
	noop := func([]byte) error { return nil }
	ctx := context.Background()

	// A mid-stream error event is lifted to a StreamError and never relayed.
	srv := ptStreamServer(t, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a\"}}]}\n\ndata: {\"error\":{\"code\":502,\"message\":\"provider down\"}}\n\n")
	var relayed []string
	res, err := ChatRawStream(ctx, ptRow(srv.URL), []byte(`{}`), 5*time.Second, 1<<20, func(p []byte) error { relayed = append(relayed, string(p)); return nil })
	var sErr *StreamError
	if !errors.As(err, &sErr) || sErr.Code != "502" {
		t.Errorf("error event: err = %v, want StreamError code 502", err)
	}
	if len(relayed) != 1 || res == nil || res.Frames != 1 {
		t.Errorf("error event: relayed %d frame(s), res %+v — want the one data frame before it, Frames 1", len(relayed), res)
	}

	// finish_reason "error" is an error, not a finish.
	srv = ptStreamServer(t, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"error\"}]}\n\n")
	if _, err := ChatRawStream(ctx, ptRow(srv.URL), []byte(`{}`), 5*time.Second, 1<<20, noop); !errors.As(err, &sErr) {
		t.Errorf("finish_reason error: err = %v, want StreamError", err)
	}

	// EOF before any finish_reason is a hard error.
	srv = ptStreamServer(t, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a\"}}]}\n\n")
	if _, err := ChatRawStream(ctx, ptRow(srv.URL), []byte(`{}`), 5*time.Second, 1<<20, noop); err == nil {
		t.Error("EOF without finish: want an error")
	}

	// The frame cap is the caller's maxFrame.
	srv = ptStreamServer(t, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\""+strings.Repeat("x", 256)+"\"}}]}\n\n")
	if _, err := ChatRawStream(ctx, ptRow(srv.URL), []byte(`{}`), 5*time.Second, 64, noop); !errors.Is(err, httpx.ErrResponseTooLarge) {
		t.Errorf("maxFrame 64: err = %v, want ErrResponseTooLarge", err)
	}

	// A callback error aborts the stream and travels wrapped.
	srv = ptStreamServer(t, ptStream)
	stop := errors.New("client gone")
	res, err = ChatRawStream(ctx, ptRow(srv.URL), []byte(`{}`), 5*time.Second, 1<<20, func([]byte) error { return stop })
	if !errors.Is(err, stop) || res == nil || res.Frames != 1 {
		t.Errorf("callback error: err = %v res = %+v, want the callback error after 1 frame", err, res)
	}

	// Non-200 before the first byte is a StatusError (failover-able), no result.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer bad.Close()
	res, err = ChatRawStream(ctx, ptRow(bad.URL), []byte(`{}`), 5*time.Second, 1<<20, noop)
	var se *httpx.StatusError
	if !errors.As(err, &se) || se.Code != 503 || res != nil {
		t.Errorf("503: err = %v res = %+v, want StatusError{503} and no result", err, res)
	}
	if _, err := ChatRawStream(ctx, ptRow(bad.URL), []byte(`{}`), 5*time.Second, 1<<20, nil); err == nil {
		t.Error("nil onFrame: want an error")
	}
}

// --- WalkChain (design/02 §4.11.1) ---.

// ptAcquirer is the test Acquirer: First = blocking Acquire, Next =
// TryAcquire, both on a real pass-through dispatcher (real leases);
// Continue answers ErrNoContinuation (A03-W17 not built).
type ptAcquirer struct {
	d                 *dispatch.Dispatcher
	class             dispatch.Class
	first, next, cont atomic.Int64
	firstErr, nextErr error
	mu                sync.Mutex
	cancel            context.CancelCauseFunc
	timeouts          []time.Duration // the deadline hint of every First/Next
}

func newPTAcquirer(t *testing.T) *ptAcquirer {
	t.Helper()
	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	return &ptAcquirer{d: d, class: dispatch.ClassBackground}
}

func (a *ptAcquirer) Class() dispatch.Class { return a.class }

func (a *ptAcquirer) req(b *backends.Backend, role string, timeout time.Duration) dispatch.Request {
	a.mu.Lock()
	a.timeouts = append(a.timeouts, timeout)
	a.mu.Unlock()
	return dispatch.Request{Target: dispatch.Target{Origin: b.Host}, Class: a.class, Role: role, DeadlineIn: timeout}
}

// wrap derives a runCtx the test can cancel with a dispatcher cause.
func (a *ptAcquirer) wrap(l *dispatch.Lease, rc context.Context, err error) (*dispatch.Lease, context.Context, error) {
	if err != nil {
		return nil, nil, err
	}
	w, cancel := context.WithCancelCause(rc)
	a.mu.Lock()
	a.cancel = cancel
	a.mu.Unlock()
	return l, w, nil
}

func (a *ptAcquirer) First(ctx context.Context, b *backends.Backend, role string, timeout time.Duration) (*dispatch.Lease, context.Context, error) {
	a.first.Add(1)
	if a.firstErr != nil {
		return nil, nil, a.firstErr
	}
	return a.wrap(a.d.Acquire(ctx, a.req(b, role, timeout)))
}

func (a *ptAcquirer) Next(ctx context.Context, b *backends.Backend, role string, timeout time.Duration) (*dispatch.Lease, context.Context, error) {
	a.next.Add(1)
	if a.nextErr != nil {
		return nil, nil, a.nextErr
	}
	return a.wrap(a.d.TryAcquire(ctx, a.req(b, role, timeout)))
}

func (a *ptAcquirer) Continue(context.Context, *backends.Backend, string, time.Duration) (*dispatch.Lease, context.Context, error) {
	a.cont.Add(1)
	return nil, nil, ErrNoContinuation
}

// ptStep is one scripted attempt outcome per row name.
type ptStep struct {
	status    int // 0/200 = success
	committed bool
	err       error
}

type ptReport struct {
	id    string
	class backends.ErrClass
}

type ptWalk struct {
	calls   []string
	reports []ptReport
}

func (w *ptWalk) report(id string, c backends.ErrClass, _ time.Duration) {
	w.reports = append(w.reports, ptReport{id, c})
}

func (w *ptWalk) attempt(script map[string]ptStep) AttemptFunc {
	return func(runCtx context.Context, b *backends.Backend, lease *dispatch.Lease, timeout time.Duration) (dispatch.Usage, bool, error) {
		w.calls = append(w.calls, b.Name)
		if lease == nil || runCtx == nil || timeout <= 0 {
			return dispatch.Usage{}, false, fmt.Errorf("attempt without lease/runCtx/timeout")
		}
		s := script[b.Name]
		switch {
		case s.err != nil:
			return dispatch.Usage{}, s.committed, s.err
		case s.status != 0 && s.status != 200:
			return dispatch.Usage{PromptTokens: 1}, s.committed, fmt.Errorf("llm: %w", &httpx.StatusError{Code: s.status, Body: "x"})
		}
		return dispatch.Usage{PromptTokens: 10, CompletionTokens: 5}, true, nil
	}
}

func ptChainRow(name string, numCtx int) backends.Backend {
	return backends.Backend{
		ID: name + "-id", Name: name, Host: "http://" + name + ".invalid:8000", Protocol: backends.ProtocolOpenAI,
		NumCtx: numCtx, Roles: []string{"chat"}, ModelMap: map[string]backends.ModelSpec{"chat": {Model: "m"}},
	}
}

var ptRules = WalkRules{WindowRule: true, AllowedClasses: []dispatch.Class{dispatch.ClassBackground}}

func ptRun(t *testing.T, acq *ptAcquirer, chain []backends.Backend, rules WalkRules, script map[string]ptStep) (*ptWalk, *backends.Backend, []ChainAttempt, error) {
	t.Helper()
	w := &ptWalk{}
	served, attempts, err := WalkChain(context.Background(), chain, "chat", acq, w.report, rules, w.attempt(script))
	return w, served, attempts, err
}

// K27 window rule: a 400 on the smaller window moves on to the larger
// (here: cooled, sorted to the end) row.
func TestWalkWindowRuleLargerRowServes(t *testing.T) {
	acq := newPTAcquirer(t)
	w, served, attempts, err := ptRun(t, acq, []backends.Backend{ptChainRow("cpu", 98304), ptChainRow("spark", 262144)}, ptRules,
		map[string]ptStep{"cpu": {status: 400}})
	if err != nil || served == nil || served.Name != "spark" {
		t.Fatalf("served = %v err = %v, want spark", served, err)
	}
	if !reflect.DeepEqual(w.calls, []string{"cpu", "spark"}) || len(attempts) != 2 {
		t.Errorf("calls = %v attempts = %d, want [cpu spark]/2", w.calls, len(attempts))
	}
	if acq.first.Load() != 1 || acq.next.Load() != 1 {
		t.Errorf("acquires First=%d Next=%d, want 1/1 (attempt 2 non-blocking)", acq.first.Load(), acq.next.Load())
	}
}

// 404 (row does not know the model) always moves on — even to a smaller window.
func TestWalk404AlwaysNext(t *testing.T) {
	acq := newPTAcquirer(t)
	w, served, _, err := ptRun(t, acq, []backends.Backend{ptChainRow("spark", 262144), ptChainRow("cpu", 98304)}, ptRules,
		map[string]ptStep{"spark": {status: 404}})
	if err != nil || served == nil || served.Name != "cpu" {
		t.Fatalf("served = %v err = %v, want cpu", served, err)
	}
	if !reflect.DeepEqual(w.calls, []string{"spark", "cpu"}) {
		t.Errorf("calls = %v", w.calls)
	}
}

// A 400 with no later larger window stops: zero attempts on the smaller row,
// the client gets the 400.
func TestWalkWindowRuleStopsWithoutLargerRow(t *testing.T) {
	acq := newPTAcquirer(t)
	w, served, attempts, err := ptRun(t, acq, []backends.Backend{ptChainRow("spark", 262144), ptChainRow("cpu", 98304)}, ptRules,
		map[string]ptStep{"spark": {status: 400}})
	var se *httpx.StatusError
	if served != nil || !errors.As(err, &se) || se.Code != 400 {
		t.Fatalf("served = %v err = %v, want STOP with the 400", served, err)
	}
	if !reflect.DeepEqual(w.calls, []string{"spark"}) || len(attempts) != 1 {
		t.Errorf("calls = %v attempts = %d, want [spark]/1 — 0 attempts on the smaller window", w.calls, len(attempts))
	}
	if acq.first.Load() != 1 || acq.next.Load() != 0 {
		t.Errorf("acquires First=%d Next=%d, want 1/0", acq.first.Load(), acq.next.Load())
	}
	if !reflect.DeepEqual(w.reports, []ptReport{{"spark-id", backends.ClassBadRequest}}) {
		t.Errorf("reports = %v", w.reports)
	}
}

// NumCtx == 0 (unknown) counts as larger; smaller/equal windows are skipped
// without an attempt; only a KNOWN window sets the floor.
func TestWalkWindowRuleUnknownAndSkip(t *testing.T) {
	cases := []struct {
		name      string
		chain     []backends.Backend
		wantCalls []string
		served    string
	}{
		{"unknown is larger", []backends.Backend{ptChainRow("cpu", 98304), ptChainRow("or", 0)}, []string{"cpu", "or"}, "or"},
		{"smaller and equal skipped", []backends.Backend{ptChainRow("cpu", 98304), ptChainRow("tiny", 65536), ptChainRow("same", 98304), ptChainRow("spark", 262144)}, []string{"cpu", "spark"}, "spark"},
		{"unknown failure sets no floor", []backends.Backend{ptChainRow("or", 0), ptChainRow("cpu", 98304), ptChainRow("tiny", 65536), ptChainRow("or2", 0)}, []string{"or", "cpu", "or2"}, "or2"},
		{"floor stops after an unknown failure", []backends.Backend{ptChainRow("or", 0), ptChainRow("spark", 262144), ptChainRow("cpu", 98304)}, []string{"or", "spark"}, ""},
		// The live chat chain during a spark-chat cooldown (priority order,
		// cooled row last): openrouter's refusal must not hide spark-chat.
		{"live cooldown order", []backends.Backend{ptChainRow("herbert", 98304), ptChainRow("openrouter", 0), ptChainRow("llamacpu", 98304), ptChainRow("spark", 262144)}, []string{"herbert", "openrouter", "spark"}, "spark"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			script := map[string]ptStep{}
			for _, b := range c.chain {
				if b.Name != c.served {
					script[b.Name] = ptStep{status: 400}
				}
			}
			w, served, _, err := ptRun(t, newPTAcquirer(t), c.chain, ptRules, script)
			if !reflect.DeepEqual(w.calls, c.wantCalls) {
				t.Errorf("calls = %v, want %v", w.calls, c.wantCalls)
			}
			switch {
			case c.served == "" && (served != nil || err == nil):
				t.Errorf("served = %v err = %v, want STOP", served, err)
			case c.served != "" && (served == nil || served.Name != c.served || err != nil):
				t.Errorf("served = %v err = %v, want %s", served, err, c.served)
			}
		})
	}
}

// A bad_request without an HTTP status (a provider error event typed
// invalid_request_error) is request-specific: window-bound like a 400.
func TestWalkStatuslessBadRequestIsWindowBound(t *testing.T) {
	ev := &StreamError{Code: "invalid_request_error", Message: "prompt too long"}
	if c := backends.Classify(ev, ""); c != backends.ClassBadRequest {
		t.Fatalf("precondition: Classify = %v, want bad_request", c)
	}
	w, served, _, err := ptRun(t, newPTAcquirer(t), []backends.Backend{ptChainRow("spark", 262144), ptChainRow("cpu", 98304)}, ptRules,
		map[string]ptStep{"spark": {err: ev}})
	if served != nil || !errors.As(err, new(*StreamError)) || !reflect.DeepEqual(w.calls, []string{"spark"}) {
		t.Errorf("served = %v calls = %v err = %v, want STOP on spark", served, w.calls, err)
	}
}

// The status-less twins of a 404 — the anthropic not_found_error, the
// gateway codes not_found/model_not_found — move on like the 404 itself
// instead of falling under the window rule (fremd-review 2 W02-06 F-3).
func TestWalkStatuslessNotFoundMovesOn(t *testing.T) {
	for _, code := range []string{"not_found_error", "not_found", "model_not_found"} {
		ev := &StreamError{Code: code, Message: "no such model"}
		if c := backends.Classify(ev, ""); c != backends.ClassBadRequest {
			t.Fatalf("precondition %s: Classify = %v, want bad_request", code, c)
		}
		w, served, _, err := ptRun(t, newPTAcquirer(t), []backends.Backend{ptChainRow("spark", 262144), ptChainRow("cpu", 98304)}, ptRules,
			map[string]ptStep{"spark": {err: ev}})
		if err != nil || served == nil || served.Name != "cpu" || !reflect.DeepEqual(w.calls, []string{"spark", "cpu"}) {
			t.Errorf("%s: served = %v calls = %v err = %v, want cpu (like a 404)", code, served, w.calls, err)
		}
	}
}

func ptInflight(d *dispatch.Dispatcher) int {
	n := 0
	for _, ts := range d.Snapshot().Targets {
		n += ts.Inflight
	}
	return n
}

// Every lease the walker takes is released — after a failover, a committed
// failure, a window STOP and a panicking attempt (defer, B1).
func TestWalkReleasesEveryLease(t *testing.T) {
	acq := newPTAcquirer(t)
	chain := []backends.Backend{ptChainRow("a", 98304), ptChainRow("b", 0)}
	for name, script := range map[string]map[string]ptStep{
		"failover":    {"a": {status: 502}},
		"committed":   {"a": {status: 502, committed: true}},
		"window stop": {"a": {status: 400}, "b": {status: 400}},
		"served":      nil,
	} {
		_, _, _, _ = ptRun(t, acq, chain, ptRules, script)
		if n := ptInflight(acq.d); n != 0 {
			t.Errorf("%s: %d lease(s) still in flight", name, n)
		}
	}
	func() {
		defer func() { _ = recover() }()
		_, _, _ = WalkChain(context.Background(), chain, "chat", acq, nil, ptRules,
			func(context.Context, *backends.Backend, *dispatch.Lease, time.Duration) (dispatch.Usage, bool, error) {
				panic("relay panic")
			})
	}()
	if n := ptInflight(acq.d); n != 0 {
		t.Errorf("after a panicking attempt: %d lease(s) still in flight (release not panic-safe)", n)
	}
}

// The attempt's usage reaches the lease and is charged at Release (an
// interactive lease with a principal: uncharged stays 0, tokens booked).
func TestWalkReportsUsageToLease(t *testing.T) {
	acq := newPTAcquirer(t)
	acq.class = dispatch.ClassInteractive
	rules := WalkRules{WindowRule: true, AllowedClasses: []dispatch.Class{dispatch.ClassInteractive}}
	ctx := withTestPrincipal(context.Background(), dispatch.Principal{ApiKeyID: "k1", TenantID: "t1", HomeScope: "scope-a"})
	w := &ptWalk{}
	if _, _, err := WalkChain(ctx, []backends.Backend{ptChainRow("a", 0)}, "chat", acq, nil, rules, w.attempt(nil)); err != nil {
		t.Fatal(err)
	}
	snap := acq.d.Snapshot()
	if snap.UnchargedCalls != 0 {
		t.Errorf("UnchargedCalls = %d, want 0 (usage reported to the lease)", snap.UnchargedCalls)
	}
	var tokens int64
	for _, ts := range snap.Targets {
		for _, bk := range ts.Buckets {
			if bk.FairKey == "scope-a" {
				tokens += bk.Tokens
			}
		}
	}
	if tokens != 15 {
		t.Errorf("charged tokens = %d, want 15 (10 prompt + 5 completion)", tokens)
	}
}

// The class assert also covers the ADMITTED lease: an interactive acquire
// without a principal is downgraded by the dispatcher (B8) — the walker
// hands that lease back before any wire contact.
func TestWalkRefusesLeaseOutsideAllowedClasses(t *testing.T) {
	acq := newPTAcquirer(t)
	acq.class = dispatch.ClassInteractive
	rules := WalkRules{WindowRule: true, AllowedClasses: []dispatch.Class{dispatch.ClassInteractive}}
	w := &ptWalk{}
	served, attempts, err := WalkChain(context.Background(), []backends.Backend{ptChainRow("a", 0)}, "chat", acq, w.report, rules, w.attempt(nil))
	if served != nil || !errors.Is(err, ErrWalkClass) || len(w.calls) != 0 || len(attempts) != 0 {
		t.Fatalf("downgraded lease: served = %v err = %v calls = %v, want ErrWalkClass before the wire", served, err, w.calls)
	}
	if n := ptInflight(acq.d); n != 0 {
		t.Errorf("the refused lease is still in flight (%d)", n)
	}
}

// leakyAcquirer breaks the contract: a lease AND an error.
type leakyAcquirer struct{ *ptAcquirer }

func (a leakyAcquirer) First(ctx context.Context, b *backends.Backend, role string, timeout time.Duration) (*dispatch.Lease, context.Context, error) {
	l, rc, _ := a.d.Acquire(ctx, a.req(b, role, timeout))
	return l, rc, errors.New("planner says no")
}

func TestWalkReleasesLeaseOnAcquirerContractBreach(t *testing.T) {
	acq := newPTAcquirer(t)
	_, _, err := WalkChain(context.Background(), []backends.Backend{ptChainRow("a", 0)}, "chat", leakyAcquirer{acq}, nil, ptRules, (&ptWalk{}).attempt(nil))
	if !IsAdmissionError(err) {
		t.Errorf("err = %v, want a terminal AdmissionError", err)
	}
	if n := ptInflight(acq.d); n != 0 {
		t.Errorf("lease returned next to an error still in flight (%d)", n)
	}
}

// Without the window rule a bad_request moves on like ChatChainVia; a 3xx
// (cross-origin redirect refused by the pool client, F-W1 — a row config
// fault) is not window-bound either: it moves on like the 404.
func TestWalkBadRequestWithoutWindowBound(t *testing.T) {
	chain := []backends.Backend{ptChainRow("spark", 262144), ptChainRow("cpu", 98304)}
	rules := ptRules
	rules.WindowRule = false
	if w, served, _, _ := ptRun(t, newPTAcquirer(t), chain, rules, map[string]ptStep{"spark": {status: 400}}); served == nil || served.Name != "cpu" {
		t.Errorf("window rule off: served = %v calls = %v, want cpu", served, w.calls)
	}
	if w, served, _, _ := ptRun(t, newPTAcquirer(t), chain, ptRules, map[string]ptStep{"spark": {status: 307}}); served == nil || served.Name != "cpu" {
		t.Errorf("3xx: served = %v calls = %v, want cpu", served, w.calls)
	}
}

// Attempt ≥ 2 uses Next (TryAcquire semantics): one blocking First per walk.
func TestWalkFollowUpsAreNonBlocking(t *testing.T) {
	acq := newPTAcquirer(t)
	_, served, _, err := ptRun(t, acq, []backends.Backend{ptChainRow("a", 0), ptChainRow("b", 0), ptChainRow("c", 0)}, ptRules,
		map[string]ptStep{"a": {status: 502}, "b": {status: 429}})
	if err != nil || served == nil || served.Name != "c" {
		t.Fatalf("served = %v err = %v, want c", served, err)
	}
	if acq.first.Load() != 1 || acq.next.Load() != 2 || acq.cont.Load() != 0 {
		t.Errorf("First=%d Next=%d Continue=%d, want 1/2/0", acq.first.Load(), acq.next.Load(), acq.cont.Load())
	}

	// A would-block on the follow-up is terminal: an admission error, no
	// attempt, no report for that row.
	acq = newPTAcquirer(t)
	acq.nextErr = &AdmissionError{Err: dispatch.ErrWouldBlock}
	w, served, attempts, err := ptRun(t, acq, []backends.Backend{ptChainRow("a", 0), ptChainRow("b", 0)}, ptRules, map[string]ptStep{"a": {status: 502}})
	if served != nil || !errors.Is(err, dispatch.ErrWouldBlock) || !IsAdmissionError(err) {
		t.Fatalf("would-block: served = %v err = %v, want terminal admission error", served, err)
	}
	if !reflect.DeepEqual(w.calls, []string{"a"}) || len(attempts) != 1 || len(w.reports) != 1 {
		t.Errorf("would-block: calls = %v attempts = %d reports = %v", w.calls, len(attempts), w.reports)
	}
}

// A first-acquire error is terminal and always an admission error — even
// when the Acquirer returned a bare sentinel.
func TestWalkAcquireErrorTerminal(t *testing.T) {
	acq := newPTAcquirer(t)
	acq.firstErr = dispatch.ErrTargetSaturated
	w, served, attempts, err := ptRun(t, acq, []backends.Backend{ptChainRow("a", 0), ptChainRow("b", 0)}, ptRules, nil)
	if served != nil || !IsAdmissionError(err) || !errors.Is(err, dispatch.ErrTargetSaturated) {
		t.Fatalf("served = %v err = %v, want terminal AdmissionError", served, err)
	}
	if len(w.calls) != 0 || len(attempts) != 0 || len(w.reports) != 0 || acq.next.Load() != 0 {
		t.Errorf("calls = %v attempts = %d reports = %v next = %d, want nothing", w.calls, len(attempts), w.reports, acq.next.Load())
	}
}

// Committed: an error after the first data frame never fails over.
func TestWalkCommittedNoFailover(t *testing.T) {
	acq := newPTAcquirer(t)
	w, served, attempts, err := ptRun(t, acq, []backends.Backend{ptChainRow("a", 0), ptChainRow("b", 0)}, ptRules,
		map[string]ptStep{"a": {status: 502, committed: true}})
	if served != nil || err == nil {
		t.Fatalf("served = %v err = %v, want the committed error", served, err)
	}
	if !reflect.DeepEqual(w.calls, []string{"a"}) || len(attempts) != 1 || acq.next.Load() != 0 {
		t.Errorf("calls = %v attempts = %d next = %d, want no failover", w.calls, len(attempts), acq.next.Load())
	}
	if !reflect.DeepEqual(w.reports, []ptReport{{"a-id", backends.ClassGateway}}) {
		t.Errorf("reports = %v, want the health report for a", w.reports)
	}
}

// Klassen-Assert: a class outside AllowedClasses fails before any acquire.
func TestWalkClassAssert(t *testing.T) {
	for _, allowed := range [][]dispatch.Class{{dispatch.ClassInteractive}, nil} {
		acq := newPTAcquirer(t)
		w, served, attempts, err := ptRun(t, acq, []backends.Backend{ptChainRow("a", 0)},
			WalkRules{WindowRule: true, AllowedClasses: allowed}, nil)
		if served != nil || !errors.Is(err, ErrWalkClass) {
			t.Errorf("allowed %v: served = %v err = %v, want ErrWalkClass", allowed, served, err)
		}
		if IsAdmissionError(err) {
			t.Errorf("allowed %v: the class assert is a code-bug sentinel, not an admission error", allowed)
		}
		if n := acq.first.Load() + acq.next.Load() + acq.cont.Load(); n != 0 || len(w.calls) != 0 || len(attempts) != 0 {
			t.Errorf("allowed %v: %d acquires, calls %v — want 0 before the assert", allowed, n, w.calls)
		}
	}
}

// Dispatcher abort (preempt/reap) is terminal and reports nothing.
func TestWalkDispatcherAbortTerminal(t *testing.T) {
	acq := newPTAcquirer(t)
	w := &ptWalk{}
	attempt := func(runCtx context.Context, b *backends.Backend, lease *dispatch.Lease, timeout time.Duration) (dispatch.Usage, bool, error) {
		w.calls = append(w.calls, b.Name)
		acq.mu.Lock()
		acq.cancel(dispatch.ErrPreempted)
		acq.mu.Unlock()
		return dispatch.Usage{}, false, runCtx.Err()
	}
	served, attempts, err := WalkChain(context.Background(), []backends.Backend{ptChainRow("a", 0), ptChainRow("b", 0)}, "chat", acq, w.report, ptRules, attempt)
	if served != nil || err == nil {
		t.Fatalf("served = %v err = %v, want terminal", served, err)
	}
	if !reflect.DeepEqual(w.calls, []string{"a"}) || len(w.reports) != 0 {
		t.Errorf("calls = %v reports = %v, want [a] and no report", w.calls, w.reports)
	}
	if len(attempts) != 1 || attempts[0].Class != "preempted" {
		t.Errorf("attempts = %+v, want one preempted", attempts)
	}
}

// STOP classes, parent cancel, rows without a model.
func TestWalkStopsAndSkips(t *testing.T) {
	// 500: the server ran the request — STOP.
	w, served, _, err := ptRun(t, newPTAcquirer(t), []backends.Backend{ptChainRow("a", 0), ptChainRow("b", 0)}, ptRules, map[string]ptStep{"a": {status: 500}})
	if served != nil || err == nil || !reflect.DeepEqual(w.calls, []string{"a"}) {
		t.Errorf("500: served = %v calls = %v err = %v, want STOP", served, w.calls, err)
	}

	// Parent canceled after a Next-class failure: stop at once.
	ctx, cancel := context.WithCancel(context.Background())
	acq := newPTAcquirer(t)
	pw := &ptWalk{}
	inner := pw.attempt(map[string]ptStep{"a": {status: 502}})
	served, _, err = WalkChain(ctx, []backends.Backend{ptChainRow("a", 0), ptChainRow("b", 0)}, "chat", acq, pw.report, ptRules,
		func(rc context.Context, b *backends.Backend, l *dispatch.Lease, to time.Duration) (dispatch.Usage, bool, error) {
			defer cancel()
			return inner(rc, b, l, to)
		})
	if served != nil || err == nil || !reflect.DeepEqual(pw.calls, []string{"a"}) {
		t.Errorf("parent canceled: served = %v calls = %v err = %v", served, pw.calls, err)
	}

	// A row without a model for the role is skipped: no acquire, no attempt
	// entry (attempts are wire attempts only).
	noModel := ptChainRow("nomodel", 0)
	noModel.ModelMap = nil
	acq = newPTAcquirer(t)
	w, served, attempts, err := ptRun(t, acq, []backends.Backend{noModel, ptChainRow("b", 0)}, ptRules, nil)
	if err != nil || served == nil || served.Name != "b" || len(attempts) != 1 || acq.first.Load() != 1 || acq.next.Load() != 0 {
		t.Errorf("no model: served = %v attempts = %+v first = %d next = %d err = %v", served, attempts, acq.first.Load(), acq.next.Load(), err)
	}
	if !reflect.DeepEqual(w.calls, []string{"b"}) {
		t.Errorf("no model: calls = %v", w.calls)
	}

	// Chain exhausted: wrapped last error.
	_, served, _, err = ptRun(t, newPTAcquirer(t), []backends.Backend{ptChainRow("a", 0)}, ptRules, map[string]ptStep{"a": {status: 502}})
	var se *httpx.StatusError
	if served != nil || !errors.As(err, &se) || se.Code != 502 {
		t.Errorf("exhausted: served = %v err = %v", served, err)
	}
	if _, _, err := WalkChain(context.Background(), nil, "chat", newPTAcquirer(t), nil, ptRules, (&ptWalk{}).attempt(nil)); err == nil {
		t.Error("empty chain: want an error")
	}
}

// The walker hands the SAME timeout to the acquire hint and to the attempt
// (rule V1); rules.Timeout overrides the row's role timeout.
func TestWalkTimeoutSingleSource(t *testing.T) {
	var got []time.Duration
	acq := newPTAcquirer(t)
	rules := ptRules
	rules.Timeout = func(b *backends.Backend) time.Duration { return 7 * time.Second }
	_, _, err := WalkChain(context.Background(), []backends.Backend{ptChainRow("a", 0)}, "chat", acq, nil, rules,
		func(_ context.Context, _ *backends.Backend, _ *dispatch.Lease, to time.Duration) (dispatch.Usage, bool, error) {
			got = append(got, to)
			return dispatch.Usage{}, false, nil
		})
	if err != nil || !reflect.DeepEqual(got, []time.Duration{7 * time.Second}) {
		t.Errorf("rules.Timeout: got %v err %v, want [7s]", got, err)
	}
	if !reflect.DeepEqual(acq.timeouts, got) {
		t.Errorf("acquire deadline hints %v, attempt timeouts %v — want one source (V1)", acq.timeouts, got)
	}
	acq.timeouts = nil
	got = nil
	row := ptChainRow("a", 0)
	row.Timeouts = map[string]int{"chat": 42}
	_, _, _ = WalkChain(context.Background(), []backends.Backend{row}, "chat", acq, nil, ptRules,
		func(_ context.Context, _ *backends.Backend, _ *dispatch.Lease, to time.Duration) (dispatch.Usage, bool, error) {
			got = append(got, to)
			return dispatch.Usage{}, false, nil
		})
	if !reflect.DeepEqual(got, []time.Duration{42 * time.Second}) || !reflect.DeepEqual(acq.timeouts, got) {
		t.Errorf("row timeout: attempt %v, acquire %v, want [42s] on both", got, acq.timeouts)
	}
}
