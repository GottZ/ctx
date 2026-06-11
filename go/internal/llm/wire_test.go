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

// Translate call site: hits the endpoint of the backend tuple's protocol.
func TestTranslateQueryWirePath(t *testing.T) {
	for _, tc := range wireCases {
		t.Run(tc.name, func(t *testing.T) {
			rec := newWireRecorder(t, "database status")
			got, err := TranslateQuery(context.Background(), rec.backend(tc.protocol), "wie ist der status der datenbank")
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
			res, err := NormalizeTemporal(context.Background(), rec.backend(tc.protocol), "what happened yesterday", now)
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

// Synthesize call site (primary leg of chatWithFallback).
func TestSynthesizeWirePath(t *testing.T) {
	settings := SynthesisSettings{ScoreThreshold: 0.001, ConfidentThreshold: 0.008, PromptVersion: PromptVersionV52}
	sources := []Source{{ID: "1", Title: "t", Category: "c", Content: "body", Score: 0.5, AgeDays: 1}}
	for _, tc := range wireCases {
		t.Run(tc.name, func(t *testing.T) {
			rec := newWireRecorder(t, "An answer.")
			res, err := Synthesize(context.Background(), nil, rec.backend(tc.protocol), nil, settings, "q", sources, nil)
			if err != nil {
				t.Fatalf("Synthesize: %v", err)
			}
			if res.Answer != "An answer." {
				t.Errorf("answer = %q, want the stub answer", res.Answer)
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

// The fallback leg follows the FALLBACK tuple's protocol, not the primary's —
// prod runs an ollama-or-openai primary with an openai CPU sidecar; mixing
// the fields up would 404 exactly when the emergency path is needed.
func TestChatWithFallbackWirePathFollowsFallbackProtocol(t *testing.T) {
	rec := newWireRecorder(t, "rescued")
	fallback := &backends.Backend{Host: rec.srv.URL, Protocol: backends.ProtocolOpenAI, Timeout: 5 * time.Second}

	resp, used, err := chatWithFallback(context.Background(), deadBackend(t), fallback, "sys", "user", Options{NumPredict: 5}, 2*time.Second)
	if err != nil || !used {
		t.Fatalf("want fallback used without error, got used=%v err=%v", used, err)
	}
	if resp.Message.Content != "rescued" {
		t.Errorf("content = %q, want rescued", resp.Message.Content)
	}
	assertWire(t, rec, "/v1/chat/completions", `"max_tokens"`)
}
