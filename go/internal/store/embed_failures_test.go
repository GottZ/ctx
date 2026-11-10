package store

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/embed"
	"github.com/GottZ/ctx/internal/httpx"
)

// --- NormalizeEmbedError ---.

func TestNormalizeEmbedError_PrefixesClass(t *testing.T) {
	got := NormalizeEmbedError(EmbedFailureWire, "boom")
	if got != "wire: boom" {
		t.Errorf("NormalizeEmbedError = %q, want %q", got, "wire: boom")
	}
}

func TestNormalizeEmbedError_StripsControlChars(t *testing.T) {
	got := NormalizeEmbedError(EmbedFailureWire, "line1\nline2\ttab\x00null")
	if strings.ContainsAny(got, "\n\t\x00") {
		t.Errorf("NormalizeEmbedError = %q, control chars survived", got)
	}
}

func TestNormalizeEmbedError_CapsAt500Runes(t *testing.T) {
	raw := strings.Repeat("x", 2000)
	got := NormalizeEmbedError(EmbedFailureWire, raw)
	if n := len([]rune(got)); n != maxLastErrorLen {
		t.Errorf("normalized length = %d, want %d", n, maxLastErrorLen)
	}
}

// --- ClassifyEmbedError ---.

func TestClassifyEmbedError_StatusErrorExceedContextSize_Oversize(t *testing.T) {
	err := fmt.Errorf("embed: %w", &httpx.StatusError{
		Code: http.StatusBadRequest,
		Body: `{"error":{"type":"exceed_context_size_error","message":"input too long"}}`,
	})
	class, msg := ClassifyEmbedError(err)
	if class != EmbedFailureOversize {
		t.Errorf("class = %q, want %q", class, EmbedFailureOversize)
	}
	if !strings.HasPrefix(msg, "oversize: ") {
		t.Errorf("normalized message = %q, want oversize-prefixed", msg)
	}
	if !strings.Contains(msg, "400") {
		t.Errorf("normalized message = %q, want HTTP status embedded", msg)
	}
}

func TestClassifyEmbedError_StatusErrorOther_Wire(t *testing.T) {
	err := fmt.Errorf("embed: %w", &httpx.StatusError{
		Code: http.StatusBadGateway,
		Body: "upstream unavailable",
	})
	class, msg := ClassifyEmbedError(err)
	if class != EmbedFailureWire {
		t.Errorf("class = %q, want %q", class, EmbedFailureWire)
	}
	if !strings.HasPrefix(msg, "wire: ") {
		t.Errorf("normalized message = %q, want wire-prefixed", msg)
	}
}

// W04-4 (Lead-Messung 2026-07-24): oversize is the 400-AND-substring
// contract — a 5xx that merely ECHOES the token in its body is a transient
// server condition and must keep the retryable wire class, never the
// permanent infinity park.
func TestClassifyEmbedError_NonBadRequestWithMarker_Wire(t *testing.T) {
	err := fmt.Errorf("embed: %w", &httpx.StatusError{
		Code: http.StatusInternalServerError,
		Body: `{"error":"proxy log: upstream said exceed_context_size once"}`,
	})
	class, _ := ClassifyEmbedError(err)
	if class != EmbedFailureWire {
		t.Errorf("class = %q, want %q (oversize is a 400-only classification)", class, EmbedFailureWire)
	}
}

// FOLGE-EMBLIMIT G6: a ctx instance that embeds through another one's
// gateway (POST /v1/embeddings) gets the OpenAI code context_length_exceeded
// for an input the backend refused — oversize, under the same 400-AND-marker
// rule: a 500 that echoes the code stays wire, a 400 without a marker stays
// wire, llama.cpp's own body stays oversize.
func TestClassifyEmbedError_OpenAICodeContextLengthExceeded(t *testing.T) {
	gateway := `{"error":{"message":"This model's maximum context length is 8192 tokens; reduce the length of the messages.","type":"invalid_request_error","param":"input[0]","code":"context_length_exceeded"}}`
	cases := []struct {
		name string
		code int
		body string
		want EmbedFailureClass
	}{
		{"gateway 400", http.StatusBadRequest, gateway, EmbedFailureOversize},
		{"openai 400", http.StatusBadRequest, `{"error":{"message":"too long","type":"invalid_request_error","param":null,"code":"context_length_exceeded"}}`, EmbedFailureOversize},
		{"500 echoing the code", http.StatusInternalServerError, gateway, EmbedFailureWire},
		{"400 without a marker", http.StatusBadRequest, `{"error":{"message":"bad","type":"invalid_request_error","param":"input","code":"upstream_rejected"}}`, EmbedFailureWire},
		{"llama.cpp 400", http.StatusBadRequest, `{"error":{"code":400,"message":"request (33177 tokens) exceeds the available context size (32768 tokens), try increasing it","type":"exceed_context_size_error","n_prompt_tokens":33177,"n_ctx":32768}}`, EmbedFailureOversize},
	}
	for _, tc := range cases {
		class, msg := ClassifyEmbedError(fmt.Errorf("embed: %w", &httpx.StatusError{Code: tc.code, Body: tc.body}))
		if class != tc.want || !strings.HasPrefix(msg, string(tc.want)+": ") {
			t.Errorf("%s: class %q (%q), want %q", tc.name, class, msg, tc.want)
		}
	}
}

// FOLGE-EMBLIMIT G8: the body the gateway writes for a refused input
// (internal/oai TestEmbedFirstBlockOversizeNamesTheInput pins the route's
// bytes against this file) is read by the embed client this package
// classifies — served as the 400 it is, through embed.Embed's wire and its
// error body read — as oversize. One file, both ends: internal/oai may not
// import this package (depguard oai-isolation), so the fixture is the join.
func TestClassifyEmbedError_GatewayBodyThroughTheClient(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "oai", "testdata", "embed_context_length_exceeded.json"))
	if err != nil {
		t.Fatalf("the gateway's fixture: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("x-should-retry", "false")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	row := backends.Backend{Name: "peer", Host: srv.URL, Protocol: backends.ProtocolOpenAI, Model: "peer-embed"}
	_, _, err = embed.Embed(context.Background(), row, "a block too long for the peer's backend", embed.PrefixDocument)
	if err == nil {
		t.Fatal("embed.Embed answered a 400 without an error")
	}
	if class, msg := ClassifyEmbedError(err); class != EmbedFailureOversize {
		t.Errorf("class %q (%q), want %q: the client retries a block the gateway refused as too long", class, msg, EmbedFailureOversize)
	}
}

func TestClassifyEmbedError_NonStatusError_Wire(t *testing.T) {
	class, msg := ClassifyEmbedError(errors.New("connection refused"))
	if class != EmbedFailureWire {
		t.Errorf("class = %q, want %q", class, EmbedFailureWire)
	}
	if !strings.Contains(msg, "connection refused") {
		t.Errorf("normalized message = %q, want raw error text preserved", msg)
	}
}
