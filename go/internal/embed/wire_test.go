package embed

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
)

// embedWireRecorder is a dual-protocol embed stub. It records the request
// path, headers and raw body, and answers in the wire shape the PATH implies,
// so a call that hits the wrong endpoint gets a response its decoder cannot
// use — but the test assertion is on the recorded path itself, mirroring the
// llm wire_test.go pattern (F1-W3/W5): the failure class "call site builds a
// backend tuple whose Protocol does not match its Host" must surface as a
// path mismatch, not as a silent fallback.
type embedWireRecorder struct {
	mu   sync.Mutex
	path string
	auth string
	body string
	srv  *httptest.Server
}

// wireVectorJSON returns a 1024-element JSON array of alternating 1,-1 —
// after L2-normalization that vector has norm 1 and variance 1/1024, which
// passes the quality gate (norm in [0.99,1.01], variance >= 0.0001).
func wireVectorJSON() string {
	var sb strings.Builder
	for i := 0; i < TargetDims; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		if i%2 == 0 {
			sb.WriteString("1")
		} else {
			sb.WriteString("-1")
		}
	}
	return sb.String()
}

func newEmbedWireRecorder(t *testing.T) *embedWireRecorder {
	t.Helper()
	vec := wireVectorJSON()
	rec := &embedWireRecorder{}
	rec.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.path = r.URL.Path
		rec.auth = r.Header.Get("Authorization")
		rec.body = string(b)
		rec.mu.Unlock()
		if r.URL.Path == "/v1/embeddings" {
			fmt.Fprintf(w, `{"data":[{"embedding":[%s]}]}`, vec)
			return
		}
		fmt.Fprintf(w, `{"embeddings":[[%s]]}`, vec)
	}))
	t.Cleanup(rec.srv.Close)
	return rec
}

func (rec *embedWireRecorder) recorded() (path, auth, body string) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.path, rec.auth, rec.body
}

// embedWireCases pins protocol → endpoint. The empty-protocol case pins the
// pre-W5 dispatch semantics: anything other than "openai" takes the ollama
// path.
var embedWireCases = []struct {
	name     string
	protocol backends.Protocol
	wantPath string
}{
	{"ollama", backends.ProtocolOllama, "/api/embed"},
	{"openai", backends.ProtocolOpenAI, "/v1/embeddings"},
	{"empty defaults to ollama", "", "/api/embed"},
}

// TestEmbedWirePath asserts the backend tuple's protocol selects the wire
// endpoint, NumCtx travels only in the ollama dialect (options.num_ctx — the
// OpenAI embeddings dialect has no such knob), and prefix + text + model +
// bearer header arrive unchanged. This is the W5 invariance pin: consolidating
// Embed/EmbedWithProtocol onto backends.Backend must be wire-identical.
func TestEmbedWirePath(t *testing.T) {
	for _, tc := range embedWireCases {
		t.Run(tc.name, func(t *testing.T) {
			rec := newEmbedWireRecorder(t)
			b := backends.Backend{
				Host:     rec.srv.URL,
				APIKey:   "test-key",
				Protocol: tc.protocol,
				Model:    "m",
				NumCtx:   2048,
			}
			vec, _, err := Embed(context.Background(), b, "hello wire", PrefixQuery)
			if err != nil {
				t.Fatalf("Embed: %v", err)
			}
			if len(vec) != TargetDims {
				t.Errorf("len(vec) = %d, want %d", len(vec), TargetDims)
			}

			path, auth, body := rec.recorded()
			if path != tc.wantPath {
				t.Errorf("wire path = %q, want %q", path, tc.wantPath)
			}
			if auth != "Bearer test-key" {
				t.Errorf("Authorization = %q, want %q", auth, "Bearer test-key")
			}
			if tc.wantPath == "/api/embed" {
				if !strings.Contains(body, `"num_ctx":2048`) {
					t.Errorf("ollama body lacks num_ctx option; body = %s", body)
				}
			} else if strings.Contains(body, "num_ctx") {
				t.Errorf("openai body must not carry the ollama num_ctx option; body = %s", body)
			}
			if !strings.Contains(body, `"model":"m"`) {
				t.Errorf("body lacks model field; body = %s", body)
			}
			if !strings.Contains(body, "Represent the query for retrieving relevant documents") {
				t.Errorf("body lacks the query instruction prefix; body = %s", body)
			}
			if !strings.Contains(body, "hello wire") {
				t.Errorf("body lacks the input text; body = %s", body)
			}
		})
	}
}

// TestEmbedWirePathDocumentPrefix pins the second prefix role on the wire:
// document embeddings must carry the document instruction, never the query
// one (asymmetric-embedding contract — mixing the two degrades retrieval
// silently).
func TestEmbedWirePathDocumentPrefix(t *testing.T) {
	rec := newEmbedWireRecorder(t)
	b := backends.Backend{Host: rec.srv.URL, Protocol: backends.ProtocolOllama, Model: "m"}
	if _, _, err := Embed(context.Background(), b, "doc body", PrefixDocument); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	_, _, body := rec.recorded()
	if !strings.Contains(body, "Represent the document for retrieval") {
		t.Errorf("body lacks the document instruction prefix; body = %s", body)
	}
	if strings.Contains(body, "Represent the query for retrieving") {
		t.Errorf("document embed must not carry the query prefix; body = %s", body)
	}
}

// Voyage AI total_tokens fallback tests.

// newUsageServer returns an OpenAI-wire embed stub whose response carries the
// given raw JSON usage fragment (e.g. `"usage":{"total_tokens":42}`). Pass ""
// for no usage field at all.
func newUsageServer(t *testing.T, usageJSON string) *httptest.Server {
	t.Helper()
	vec := wireVectorJSON()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if usageJSON != "" {
			fmt.Fprintf(w, `{"data":[{"embedding":[%s]}],%s}`, vec, usageJSON)
		} else {
			fmt.Fprintf(w, `{"data":[{"embedding":[%s]}]}`, vec)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func openAIBackend(host string) backends.Backend {
	return backends.Backend{Host: host, Protocol: backends.ProtocolOpenAI, Model: "voyage-4"}
}

// TestEmbedOpenAI_VoyageTotalTokensFallback is the load-bearing regression
// test: Voyage AI reports usage.total_tokens instead of usage.prompt_tokens
// on the /v1/embeddings endpoint. Before the fix, promptTokens was always 0
// for Voyage, so llmlog rows carried no token accounting.
func TestEmbedOpenAI_VoyageTotalTokensFallback(t *testing.T) {
	srv := newUsageServer(t, `"usage":{"total_tokens":42}`)
	_, ptoks, err := Embed(context.Background(), openAIBackend(srv.URL), "hello", PrefixQuery)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if ptoks != 42 {
		t.Errorf("promptTokens = %d, want 42 (total_tokens fallback)", ptoks)
	}
}

// TestEmbedOpenAI_PromptTokensPreferred: when both prompt_tokens and
// total_tokens are present (OpenAI sends both), prompt_tokens wins.
func TestEmbedOpenAI_PromptTokensPreferred(t *testing.T) {
	srv := newUsageServer(t, `"usage":{"prompt_tokens":10,"total_tokens":10}`)
	_, ptoks, err := Embed(context.Background(), openAIBackend(srv.URL), "hello", PrefixQuery)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if ptoks != 10 {
		t.Errorf("promptTokens = %d, want 10 (prompt_tokens preferred)", ptoks)
	}
}

// TestEmbedOpenAI_PromptTokensOnly: standard OpenAI-compatible servers that
// send only prompt_tokens must keep working unchanged.
func TestEmbedOpenAI_PromptTokensOnly(t *testing.T) {
	srv := newUsageServer(t, `"usage":{"prompt_tokens":77}`)
	_, ptoks, err := Embed(context.Background(), openAIBackend(srv.URL), "hello", PrefixQuery)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if ptoks != 77 {
		t.Errorf("promptTokens = %d, want 77", ptoks)
	}
}

// TestEmbedOpenAI_NoUsage: a server that omits usage entirely must return 0
// tokens (uncharged, never an estimate — C1 doctrine).
func TestEmbedOpenAI_NoUsage(t *testing.T) {
	srv := newUsageServer(t, "")
	_, ptoks, err := Embed(context.Background(), openAIBackend(srv.URL), "hello", PrefixQuery)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if ptoks != 0 {
		t.Errorf("promptTokens = %d, want 0 (no usage field)", ptoks)
	}
}

// TestEmbedOpenAI_EmptyUsage: usage present but both fields zero.
func TestEmbedOpenAI_EmptyUsage(t *testing.T) {
	srv := newUsageServer(t, `"usage":{"prompt_tokens":0,"total_tokens":0}`)
	_, ptoks, err := Embed(context.Background(), openAIBackend(srv.URL), "hello", PrefixQuery)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if ptoks != 0 {
		t.Errorf("promptTokens = %d, want 0", ptoks)
	}
}

// TestEmbedOpenAI_VectorStillCorrect: the token fallback must not disturb the
// embedding vector itself — verify dimensionality and quality gate pass.
func TestEmbedOpenAI_VectorStillCorrect(t *testing.T) {
	srv := newUsageServer(t, `"usage":{"total_tokens":99}`)
	vec, _, err := Embed(context.Background(), openAIBackend(srv.URL), "hello", PrefixQuery)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vec) != TargetDims {
		t.Errorf("len(vec) = %d, want %d", len(vec), TargetDims)
	}
}
