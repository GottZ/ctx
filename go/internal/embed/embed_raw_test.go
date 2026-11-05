package embed

// W02-09 (a), (b): the array wire, EmbedRaw and the byte-identical internal
// body (gate G2), no prefix and no 1024-d cut on the raw path (gate G10),
// the re-alignment by data[].index with its coverage check, the raw quality
// check, and the internal path's numbers unchanged by the generic decode.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
)

// rawEngine is a dual-protocol embed engine for the raw path: it records
// every body as sent and answers one vector per input — component j of
// input i is (i+1) + j/1000, dims long — in the shape the path implies; the
// OpenAI shape lists the entries in REVERSED order with their index, so a
// reader that does not re-align by index gets the vectors swapped. answer,
// when set, replaces the body of the answer.
type rawEngine struct {
	mu     sync.Mutex
	bodies []string
	dims   int
	answer func(path string, n int) string
	srv    *httptest.Server
}

func newRawEngine(t *testing.T, dims int) *rawEngine {
	t.Helper()
	e := &rawEngine{dims: dims}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		e.mu.Lock()
		e.bodies = append(e.bodies, string(raw))
		answer := e.answer
		e.mu.Unlock()
		var req struct {
			Input json.RawMessage `json:"input"`
		}
		_ = json.Unmarshal(raw, &req)
		n := 1
		var list []string
		if json.Unmarshal(req.Input, &list) == nil {
			n = len(list)
		}
		if answer != nil {
			_, _ = io.WriteString(w, answer(r.URL.Path, n))
			return
		}
		vec := func(i int) string {
			parts := make([]string, e.dims)
			for j := range parts {
				parts[j] = strconv.FormatFloat(float64(i+1)+float64(j)/1000, 'f', -1, 64)
			}
			return "[" + strings.Join(parts, ",") + "]"
		}
		var items []string
		if r.URL.Path == "/v1/embeddings" {
			for i := n - 1; i >= 0; i-- {
				items = append(items, fmt.Sprintf(`{"object":"embedding","index":%d,"embedding":%s}`, i, vec(i)))
			}
			fmt.Fprintf(w, `{"object":"list","data":[%s],"usage":{"prompt_tokens":%d,"total_tokens":%d}}`, strings.Join(items, ","), 3*n, 3*n)
			return
		}
		for i := range n {
			items = append(items, vec(i))
		}
		fmt.Fprintf(w, `{"embeddings":[%s],"prompt_eval_count":%d}`, strings.Join(items, ","), 3*n)
	}))
	t.Cleanup(e.srv.Close)
	return e
}

func (e *rawEngine) lastBody() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.bodies) == 0 {
		return ""
	}
	return e.bodies[len(e.bodies)-1]
}

func rawRow(host string, p backends.Protocol, numCtx int) backends.Backend {
	return backends.Backend{Name: "row", Host: host, Protocol: p, Model: "MODEL", NumCtx: numCtx, Trust: backends.TrustFull}
}

// TestInternalEmbedBodyByteIdentical (gate G2): the internal path's ONE
// input goes out as the bare string — the body byte for byte what every
// deployed backend has seen, on the openai row and on the ollama row (with
// its num_ctx option). Red: a naive array switch ("input":["…"]).
func TestInternalEmbedBodyByteIdentical(t *testing.T) {
	// json.Marshal escapes < and > (\u003c, \u003e): the golden is the
	// body as the single-string wire always sent it.
	const text = `Instruct: Represent the query for retrieving relevant documents\nContent: hällo \"wörld\" \u003ctag\u003e`
	for _, tc := range []struct {
		proto  backends.Protocol
		numCtx int
		want   string
	}{
		{backends.ProtocolOpenAI, 0, `{"model":"MODEL","input":"` + text + `"}`},
		{backends.ProtocolOllama, 2048, `{"model":"MODEL","input":"` + text + `","options":{"num_ctx":2048}}`},
		{backends.ProtocolOllama, 0, `{"model":"MODEL","input":"` + text + `"}`},
	} {
		e := newRawEngine(t, TargetDims)
		e.answer = func(path string, _ int) string {
			if path == "/v1/embeddings" {
				return `{"data":[{"embedding":[` + wireVectorJSON() + `]}]}`
			}
			return `{"embeddings":[[` + wireVectorJSON() + `]]}`
		}
		if _, _, err := Embed(context.Background(), rawRow(e.srv.URL, tc.proto, tc.numCtx), `hällo "wörld" <tag>`, PrefixQuery); err != nil {
			t.Fatalf("%s: Embed: %v", tc.proto, err)
		}
		if got := e.lastBody(); got != tc.want {
			t.Errorf("%s num_ctx=%d: internal body\n got %s\nwant %s", tc.proto, tc.numCtx, got, tc.want)
		}
	}
}

// TestEmbedRawSendsTheTextsAsAList (gate G10, wire half): EmbedRaw sends
// the texts as sent — no instruct prefix — always as a list (also one), and
// answers one vector per input at the row's native length (no 1024-d cut),
// in input order whatever order the engine lists them in.
func TestEmbedRawSendsTheTextsAsAList(t *testing.T) {
	const dims = 4096
	for _, proto := range []backends.Protocol{backends.ProtocolOpenAI, backends.ProtocolOllama} {
		e := newRawEngine(t, dims)
		inputs := []string{"alpha", "beta", "gamma"}
		vecs, tokens, err := EmbedRaw(context.Background(), rawRow(e.srv.URL, proto, 0), "RAWMODEL", inputs, 0)
		if err != nil {
			t.Fatalf("%s: EmbedRaw: %v", proto, err)
		}
		if want := `{"model":"RAWMODEL","input":["alpha","beta","gamma"]}`; e.lastBody() != want {
			t.Errorf("%s: body %s, want %s", proto, e.lastBody(), want)
		}
		if strings.Contains(e.lastBody(), "Instruct") {
			t.Errorf("%s: the raw body carries an instruct prefix", proto)
		}
		if tokens != 9 {
			t.Errorf("%s: prompt tokens %d, want 9", proto, tokens)
		}
		if len(vecs) != 3 {
			t.Fatalf("%s: %d vectors, want 3", proto, len(vecs))
		}
		for i, v := range vecs {
			if len(v) != dims {
				t.Errorf("%s: vector %d has %d dims, want the native %d", proto, i, len(v), dims)
			}
			if v[0] != float32(i+1) {
				t.Errorf("%s: vector %d starts with %v, want %v (re-aligned by index)", proto, i, v[0], float32(i+1))
			}
		}
		one, _, err := EmbedRaw(context.Background(), rawRow(e.srv.URL, proto, 0), "RAWMODEL", []string{"solo"}, dims)
		if err != nil || len(one) != 1 {
			t.Fatalf("%s: one input: %v (%d vectors)", proto, err, len(one))
		}
		if want := `{"model":"RAWMODEL","input":["solo"]}`; e.lastBody() != want {
			t.Errorf("%s: one-input body %s, want the list form %s", proto, e.lastBody(), want)
		}
	}
}

// TestEmbedRawRefusesMisalignedAnswers: a missing, repeated or invented
// index, a count mismatch, an empty vector, a length other than the expected
// one, a non-finite component and a zero vector are refused — a vector in
// the wrong place is a silently wrong embedding.
func TestEmbedRawRefusesMisalignedAnswers(t *testing.T) {
	for _, tc := range []struct {
		name, answer string
		dims         int
	}{
		{"repeated index", `{"data":[{"index":0,"embedding":[1,2]},{"index":0,"embedding":[3,4]}]}`, 0},
		{"index past n", `{"data":[{"index":0,"embedding":[1,2]},{"index":2,"embedding":[3,4]}]}`, 0},
		{"negative index", `{"data":[{"index":-1,"embedding":[1,2]},{"index":1,"embedding":[3,4]}]}`, 0},
		{"too few", `{"data":[{"index":0,"embedding":[1,2]}]}`, 0},
		{"too many", `{"data":[{"index":0,"embedding":[1,2]},{"index":1,"embedding":[3,4]},{"index":2,"embedding":[5,6]}]}`, 0},
		{"empty vector", `{"data":[{"index":0,"embedding":[]},{"index":1,"embedding":[3,4]}]}`, 0},
		{"no data", `{"data":[]}`, 0},
		{"length mismatch", `{"data":[{"index":0,"embedding":[1,2]},{"index":1,"embedding":[3,4,5]}]}`, 0},
		{"wrong expected length", `{"data":[{"index":0,"embedding":[1,2]},{"index":1,"embedding":[3,4]}]}`, 3},
		{"zero vector", `{"data":[{"index":0,"embedding":[0,0]},{"index":1,"embedding":[3,4]}]}`, 0},
		{"overflowing component", `{"data":[{"index":0,"embedding":[1e39,1]},{"index":1,"embedding":[3,4]}]}`, 0},
	} {
		e := newRawEngine(t, 2)
		e.answer = func(string, int) string { return tc.answer }
		if vecs, _, err := EmbedRaw(context.Background(), rawRow(e.srv.URL, backends.ProtocolOpenAI, 0), "m", []string{"a", "b"}, tc.dims); err == nil {
			t.Errorf("%s: accepted (%v)", tc.name, vecs)
		}
	}
	e := newRawEngine(t, 2)
	e.answer = func(string, int) string { return `{"embeddings":[[1,2]]}` }
	if _, _, err := EmbedRaw(context.Background(), rawRow(e.srv.URL, backends.ProtocolOllama, 0), "m", []string{"a", "b"}, 0); err == nil {
		t.Error("ollama: one vector for two inputs accepted")
	}
	if _, _, err := EmbedRaw(context.Background(), rawRow(e.srv.URL, backends.ProtocolOllama, 0), "m", nil, 0); err == nil {
		t.Error("no input accepted")
	}
}

// TestInternalEmbedNumbersUnchanged: the internal path decodes into float64
// as before the generic wire (its L2 normalization and quality gate are
// bit-identical) — a float32 decode would round every component first.
func TestInternalEmbedNumbersUnchanged(t *testing.T) {
	parts := make([]string, TargetDims)
	ref := make([]float64, TargetDims)
	for j := range parts {
		x := math.Sin(float64(j)*0.731) + 0.1234567890123*float64(j%7)
		ref[j] = x
		parts[j] = strconv.FormatFloat(x, 'g', -1, 64)
	}
	e := newRawEngine(t, 0)
	e.answer = func(string, int) string {
		return `{"data":[{"index":0,"embedding":[` + strings.Join(parts, ",") + `]}]}`
	}
	got, _, err := Embed(context.Background(), rawRow(e.srv.URL, backends.ProtocolOpenAI, 0), "x", PrefixDocument)
	if err != nil {
		t.Fatal(err)
	}
	want := l2Normalize(ref)
	for j := range want {
		if math.Float32bits(got[j]) != math.Float32bits(want[j]) {
			t.Fatalf("component %d: %v, want the float64-path value %v bit for bit", j, got[j], want[j])
		}
	}
}
