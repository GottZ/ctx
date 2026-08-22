package llm

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
)

// wireRecorder is a dual-protocol chat stub. It records the request path +
// raw body and answers in the wire shape the PATH implies, so a call site
// that hits the wrong endpoint gets a shape its client cannot use — but the
// test assertion is on the recorded path itself, which catches the failure
// class "call site picks the wrong protocol field → translate/temporal
// degrade fail-open, invisible in the response, only a WARN in the server
// log" (F1 risk 1) even where the call site swallows the resulting error.
type wireRecorder struct {
	mu   sync.Mutex
	path string
	body string
	srv  *httptest.Server
}

func newWireRecorder(t *testing.T, content string) *wireRecorder {
	t.Helper()
	rec := &wireRecorder{}
	rec.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.path = r.URL.Path
		rec.body = string(b)
		rec.mu.Unlock()
		if r.URL.Path == "/v1/chat/completions" {
			fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":%q}}],"usage":{"completion_tokens":1,"prompt_tokens":1}}`, content)
			return
		}
		fmt.Fprintf(w, `{"message":{"role":"assistant","content":%q},"eval_count":1,"prompt_eval_count":1}`, content)
	}))
	t.Cleanup(rec.srv.Close)
	return rec
}

func (rec *wireRecorder) recorded() (path, body string) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.path, rec.body
}

// backend builds the chat tuple under test pointing at the recorder.
func (rec *wireRecorder) backend(p backends.Protocol) backends.Backend {
	return backends.Backend{Host: rec.srv.URL, Protocol: p, Model: "m"}
}

// wireCases pins protocol → endpoint plus a protocol-distinctive body marker
// (ollama nests sampling under "options"; the OpenAI dialect flattens
// NumPredict to "max_tokens" — every prod Options preset sets NumPredict).
var wireCases = []struct {
	name     string
	protocol backends.Protocol
	wantPath string
	bodyMark string
}{
	{"ollama", backends.ProtocolOllama, "/api/chat", `"options"`},
	{"openai", backends.ProtocolOpenAI, "/v1/chat/completions", `"max_tokens"`},
}

func assertWire(t *testing.T, rec *wireRecorder, wantPath, bodyMark string) {
	t.Helper()
	path, body := rec.recorded()
	if path != wantPath {
		t.Errorf("wire path = %q, want %q", path, wantPath)
	}
	if !strings.Contains(body, bodyMark) {
		t.Errorf("request body lacks %s marker; body = %s", bodyMark, body)
	}
}

// translatePool seeds a single-backend pool carrying the translate role —
// F3-P3 turned TranslateQuery/NormalizeTemporal into chain consumers.
func translatePool(b backends.Backend) *backends.Pool {
	b.ID, b.Name = "wire", "wire"
	b.Trust = backends.TrustFull
	b.Enabled = true
	b.Roles = []string{backends.RoleTranslate}
	bpool := backends.NewPool(nil, nil)
	bpool.SeedSnapshotForTest([]backends.Backend{b})
	return bpool
}

// Translate call site: hits the endpoint of the backend tuple's protocol.
func TestTranslateQueryWirePath(t *testing.T) {
	for _, tc := range wireCases {
		t.Run(tc.name, func(t *testing.T) {
			rec := newWireRecorder(t, "database status")
			got, err := TranslateQuery(testPrincipalCtx(), nil, translatePool(rec.backend(tc.protocol)), backends.SensPersonal, "wie ist der status der datenbank", "", testAdmission(t, dispatch.ClassInteractive))
			if err != nil {
				t.Fatalf("TranslateQuery: %v", err)
			}
			if got != "database status" {
				t.Errorf("translated = %q, want the stub answer (validation must pass)", got)
			}
			assertWire(t, rec, tc.wantPath, tc.bodyMark)
		})
	}
}

// Temporal call site: same protocol → endpoint contract.
func TestNormalizeTemporalWirePath(t *testing.T) {
	const temporalJSON = `{"dates":[{"ref":"yesterday","date":"2026-06-10","end":null,"dir":"past"}],"query":"q"}`
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	for _, tc := range wireCases {
		t.Run(tc.name, func(t *testing.T) {
			rec := newWireRecorder(t, temporalJSON)
			res, err := NormalizeTemporal(testPrincipalCtx(), nil, translatePool(rec.backend(tc.protocol)), backends.SensPersonal, "what happened yesterday", now, "", testAdmission(t, dispatch.ClassInteractive))
			if err != nil {
				t.Fatalf("NormalizeTemporal: %v", err)
			}
			if res == nil || len(res.Dates) != 1 {
				t.Errorf("result = %+v, want 1 resolved date", res)
			}
			assertWire(t, rec, tc.wantPath, tc.bodyMark)
		})
	}
}

// Synthesize call site (first chain link since F3-P2).
func TestSynthesizeWirePath(t *testing.T) {
	settings := SynthesisSettings{ScoreThreshold: 0.001, ConfidentThreshold: 0.008, PromptVersion: PromptVersionV52}
	sources := []Source{{ID: "1", Title: "t", Category: "c", Content: "body", Score: 0.5, AgeDays: 1}}
	for _, tc := range wireCases {
		t.Run(tc.name, func(t *testing.T) {
			rec := newWireRecorder(t, "An answer.")
			b := rec.backend(tc.protocol)
			b.ID, b.Name = "wire", "wire"
			b.Trust = backends.TrustFull
			b.Enabled = true
			b.NumCtx = 8192 // H12: a synthesis chain member must declare its window
			b.Roles = []string{backends.RoleSynthesis}
			bpool := backends.NewPool(nil, nil)
			bpool.SeedSnapshotForTest([]backends.Backend{b})
			res, err := Synthesize(testPrincipalCtx(), nil, bpool, nil, settings, backends.SensPersonal, "q", sources, nil, "", "", testAdmission(t, dispatch.ClassInteractive))
			if err != nil {
				t.Fatalf("Synthesize: %v", err)
			}
			if res.Answer != "An answer." {
				t.Errorf("answer = %q, want the stub answer", res.Answer)
			}
			if res.Model != "m" {
				t.Errorf("model = %q, want the served backend's model", res.Model)
			}
			assertWire(t, rec, tc.wantPath, tc.bodyMark)
		})
	}
}

// ChatJSON (the ingest.Extract call-site class): protocol → endpoint plus the
// JSON-mode marker of the respective dialect.
func TestChatJSONWirePath(t *testing.T) {
	jsonMarks := map[backends.Protocol]string{
		backends.ProtocolOllama: `"format":"json"`,
		backends.ProtocolOpenAI: `"response_format"`,
	}
	for _, tc := range wireCases {
		t.Run(tc.name, func(t *testing.T) {
			rec := newWireRecorder(t, `{}`)
			if _, err := ChatJSON(context.Background(), rec.backend(tc.protocol), "sys", "user", Options{NumPredict: 5}, 5*time.Second); err != nil {
				t.Fatalf("ChatJSON: %v", err)
			}
			assertWire(t, rec, tc.wantPath, jsonMarks[tc.protocol])
		})
	}
}

// Every chain link follows ITS OWN tuple's protocol, not the first link's —
// prod runs an ollama-or-openai primary with an openai CPU sidecar; mixing
// the fields up would 404 exactly when the emergency path is needed.
// (Successor of the chatWithFallback wire-path test, F3-P2.)
func TestChatChainWirePathFollowsEachLinksProtocol(t *testing.T) {
	rec := newWireRecorder(t, "rescued")
	chain := []backends.Backend{
		{ID: "p", Name: "primary", Host: deadURL(t), Protocol: backends.ProtocolOllama,
			Model: "m", Trust: backends.TrustFull, Enabled: true, Roles: []string{backends.RoleSynthesis}},
		{ID: "f", Name: "fallback", Host: rec.srv.URL, Protocol: backends.ProtocolOpenAI,
			Model: "m", Trust: backends.TrustFull, Enabled: true, Roles: []string{backends.RoleSynthesis}},
	}
	resp, served, _, err := ChatChain(context.Background(), chain, backends.RoleSynthesis,
		"sys", "user", Options{NumPredict: 5}, "", 0, nil, testAdmission(t, dispatch.ClassBackground))
	if err != nil || served == nil || served.Name != "fallback" {
		t.Fatalf("want fallback served without error, got served=%v err=%v", served, err)
	}
	if resp.Message.Content != "rescued" {
		t.Errorf("content = %q, want rescued", resp.Message.Content)
	}
	assertWire(t, rec, "/v1/chat/completions", `"max_tokens"`)
}

// TestModelMapExtraPassthrough pins the wire contract of model_map params
// with no dedicated Options field (chat_template_kwargs, provider-specific
// knobs): applyModelParams must carry them into Options.Extra, and chatOpenAI
// must merge them into the request body BEFORE the backend's extra_body —
// which keeps precedence on key collision (applyOpenAIBodyExtras, last write
// wins). Regression for the v5.0.0 dream/translate failure: a
// chat_template_kwargs.enable_thinking disable in the serving row was silently
// dropped, chatOpenAI sent the OpenRouter-only `reasoning` field instead, and
// non-OpenRouter providers (vLLM/LiteLLM) kept thinking → structured JSON
// answers truncated at the token cap.
func TestModelMapExtraPassthrough(t *testing.T) {
	rec := newWireRecorder(t, "ok")
	b := rec.backend(backends.ProtocolOpenAI)
	b.ID, b.Name = "wire", "wire"
	b.Trust = backends.TrustFull
	b.Enabled = true
	b.ExtraBody = map[string]any{"temperature": 99} // backend wins on collision

	opts, think := applyModelParams(Options{NumPredict: 5}, map[string]any{
		"think":                false,
		"chat_template_kwargs": map[string]any{"enable_thinking": false},
		"temperature":          0.7,
	}, &b)
	if think == nil || *think {
		t.Fatalf("think = %v, want false", think)
	}
	if _, ok := opts.Extra["chat_template_kwargs"]; !ok {
		t.Fatalf("chat_template_kwargs not carried to Options.Extra: %+v", opts.Extra)
	}

	if _, err := Chat(context.Background(), b, "sys", "user", opts, 5*time.Second); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	_, body := rec.recorded()
	for _, want := range []string{`"chat_template_kwargs"`, `"enable_thinking"`, `"max_tokens":5`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s: %s", want, body)
		}
	}
	if !strings.Contains(body, `"temperature":99`) {
		t.Errorf("backend extra_body precedence broken (want temperature 99): %s", body)
	}
	if strings.Contains(body, `"temperature":0.7`) {
		t.Errorf("model_map temperature leaked over extra_body: %s", body)
	}
}
