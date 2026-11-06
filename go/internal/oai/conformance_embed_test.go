package oai_test

// Conformance harness, Go half, for POST /v1/embeddings (wave W02-09, gate
// G1; design/02 §4.8, §7): the official OpenAI Go SDK
// (client.Embeddings.New) against the embeddings route of the harness. The
// wire bytes of every answer — float and base64 — are checked against the
// vendored CreateEmbeddingResponse (openai-openapi @ c2aecc2f, the same
// commit as the other excerpts); the SDK's typed answer against the
// contract: the vectors at their index, model = the alias name, usage > 0,
// dimensions honoured. The Go SDK decodes float lists only; the base64 path
// is the Python SDK's default and runs in test_embeddings.py.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/oai"
)

// embeddingResponseExcerpt is the vendored excerpt of the embeddings answer
// (CreateEmbeddingResponse and Embedding). The Python harness reads the
// same file.
const embeddingResponseExcerpt = "../../conformance/oai/testdata/openapi-c2aecc2f-create-embedding-response.json"

// cfEmbedServe serves h and keeps the bytes of the last embeddings answer.
func cfEmbedServe(t testing.TB, h http.Handler) (*httptest.Server, func() []byte) {
	t.Helper()
	var mu sync.Mutex
	var last []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cw := &captureWriter{ResponseWriter: w}
		h.ServeHTTP(cw, r)
		if r.URL.Path == "/v1/embeddings" {
			mu.Lock()
			last = cw.buf.Bytes()
			mu.Unlock()
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []byte {
		mu.Lock()
		defer mu.Unlock()
		return append([]byte(nil), last...)
	}
}

// TestConformanceGoSDKEmbeddings is G1 (Go half): the SDK parses the answer
// with the vectors of the texts at their index, in float and with
// dimensions; every wire body — float and base64 — is a valid
// CreateEmbeddingResponse.
func TestConformanceGoSDKEmbeddings(t *testing.T) {
	schema := loadSchema(t, embeddingResponseExcerpt, "CreateEmbeddingResponse")
	e := newEmbedEngine(t, 4096)
	rig := newEmbedRig(t, []backends.Backend{e.row("conf", backends.ProtocolOpenAI, 1)}, 33, 1)
	srv, wire := cfEmbedServe(t, rig.router)
	client := openai.NewClient(option.WithBaseURL(srv.URL+"/v1/"), option.WithAPIKey("ctxm_conformance"), option.WithMaxRetries(0))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	inputs := []string{"alpha", "beta", "gamma"}
	validate := func(name string) {
		t.Helper()
		var v any
		if err := json.Unmarshal(wire(), &v); err != nil {
			t.Fatalf("%s: wire body is not JSON: %v", name, err)
		}
		if err := schema.Validate(v); err != nil {
			t.Errorf("%s: wire body fails CreateEmbeddingResponse: %v", name, err)
		}
	}
	for _, dims := range []int64{0, 256} {
		p := openai.EmbeddingNewParams{
			Model:          "emb4k",
			Input:          openai.EmbeddingNewParamsInputUnion{OfArrayOfStrings: inputs},
			EncodingFormat: openai.EmbeddingNewParamsEncodingFormatFloat,
		}
		if dims > 0 {
			p.Dimensions = openai.Int(dims)
		}
		resp, err := client.Embeddings.New(ctx, p)
		if err != nil {
			t.Fatalf("dimensions %d: Embeddings.New: %v", dims, err)
		}
		if resp.Model != "emb4k" || resp.Object != "list" || len(resp.Data) != 3 {
			t.Fatalf("dimensions %d: model %q object %q data %d", dims, resp.Model, resp.Object, len(resp.Data))
		}
		if resp.Usage.PromptTokens <= 0 || resp.Usage.TotalTokens != resp.Usage.PromptTokens {
			t.Errorf("dimensions %d: usage %+v", dims, resp.Usage)
		}
		for i, d := range resp.Data {
			want := engineVec(inputs[i], 4096)
			if d.Index != int64(i) || d.Object != "embedding" {
				t.Errorf("dimensions %d: data[%d] index %d object %q", dims, i, d.Index, d.Object)
			}
			if dims > 0 {
				if len(d.Embedding) != int(dims) {
					t.Errorf("dimensions %d: data[%d] has %d components", dims, i, len(d.Embedding))
				}
				continue
			}
			if len(d.Embedding) != len(want) {
				t.Fatalf("data[%d] has %d components, want %d", i, len(d.Embedding), len(want))
			}
			for j := range want {
				if float32(d.Embedding[j]) != want[j] {
					t.Fatalf("data[%d][%d] = %v, want the engine's %v", i, j, d.Embedding[j], want[j])
				}
			}
		}
		validate("float")
	}
	// The base64 wire (the Python SDK's default) is valid against the same
	// schema (the Go SDK has no base64 decoder).
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/embeddings",
		strings.NewReader(`{"model":"emb4k","input":["alpha"],"encoding_format":"base64"}`))
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("base64: %d", res.StatusCode)
	}
	validate("base64")
}

// TestConformanceEmbeddingSchemaHasTeeth is G1's control: the vendored
// schema refuses what is no embeddings answer — the route's own error
// envelope and an answer without usage.
func TestConformanceEmbeddingSchemaHasTeeth(t *testing.T) {
	schema := loadSchema(t, embeddingResponseExcerpt, "CreateEmbeddingResponse")
	w := httptest.NewRecorder()
	oai.WriteInternal(w)
	noUsage := `{"object":"list","model":"emb","data":[{"object":"embedding","index":0,"embedding":[0.1]}]}`
	for name, raw := range map[string][]byte{"error envelope": w.Body.Bytes(), "no usage": []byte(noUsage)} {
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if schema.Validate(v) == nil {
			t.Errorf("%s passes CreateEmbeddingResponse — the schema check has no teeth", name)
		}
	}
}
