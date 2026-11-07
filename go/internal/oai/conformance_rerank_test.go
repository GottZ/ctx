package oai_test

// Conformance harness, Go half, for POST /v1/rerank (Jina) and POST
// /v2/rerank (Cohere v2) (wave W02-10, gate G1; design/02 §4.9): both rerank
// routes of the harness behind a real HTTP server, every answer decoded
// strictly against the answer form of §4.9 — {id, model, results:[{index,
// relevance_score, document?:{text}}], usage:{prompt_tokens, total_tokens}},
// no other member, each of its JSON type — and checked against the fixture:
// the relevance scores are the sigmoid of the engine's logits, descending,
// ties in index order, top_n cuts, /v1 echoes the documents by default and
// /v2 does not, {text} objects score like strings. Neither wire has an
// official Go SDK; the Cohere Python SDK and the Jina form run in
// test_rerank.py against the same fixture (conformanceRerank).

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
)

// cfRerankQuery and cfRerankDocs are the fixture of both rerank halves: the
// fake engine answers each document's logit (newCfRerankEngine), so the
// order of the answer is known — cfRerankOrder: document 3 first, the tie
// of 0 and 4 in index order, then 1 and 2. test_rerank.py carries the same
// table.
const cfRerankQuery = "What is the capital of the United States?"

var cfRerankDocs = []struct {
	text  string
	logit float64
}{
	{"Carson City is the capital city of the American state of Nevada.", -1},
	{"Saipan is the capital of the Northern Mariana Islands.", -2.5},
	{"Capitalization is the use of a capital letter at the start of a word.", -6},
	{"Washington, D.C. is the capital of the United States.", 7.5},
	{"Capital gains are profits from the sale of an asset.", -1},
}

var cfRerankOrder = []int{3, 0, 4, 1, 2}

// newCfRerankEngine is a fake cross-encoder that answers the logits of
// cfRerankDocs.
func newCfRerankEngine(t testing.TB) *rerankEngine {
	t.Helper()
	e := newRerankEngine(t)
	e.logits = make(map[string]float64, len(cfRerankDocs))
	for _, d := range cfRerankDocs {
		e.logits[d.text] = d.logit
	}
	return e
}

// newCfRerankRig mounts both rerank routes over one row on a fresh
// newCfRerankEngine.
func newCfRerankRig(t *testing.T) *chatRig {
	t.Helper()
	return newRerankRig(t, []backends.Backend{newCfRerankEngine(t).row("conf-rerank", 10)}, 4, 1)
}

// TestConformanceRerank is G1 (Go half): over HTTP, both paths answer
// application/json in exactly the form of design/02 §4.9 with the fixture's
// order and scores; /v1 echoes every returned document, /v2 echoes none,
// top_n cuts, {text} objects score like strings.
func TestConformanceRerank(t *testing.T) {
	srv := httptest.NewServer(newCfRerankRig(t).router)
	t.Cleanup(srv.Close)
	texts := make([]string, len(cfRerankDocs))
	objects := make([]map[string]string, len(cfRerankDocs))
	for i, d := range cfRerankDocs {
		texts[i] = d.text
		objects[i] = map[string]string{"text": d.text}
	}
	for _, tc := range []struct {
		name, path string
		objects    bool
		topN       int
		echo       bool
	}{
		{"v1 Jina strings echo by default", "/v1/rerank", false, 0, true},
		{"v1 Jina text objects echo and score like strings", "/v1/rerank", true, 0, true},
		{"v1 Jina top_n cuts", "/v1/rerank", false, 2, true},
		{"v2 Cohere strings echo nothing by default", "/v2/rerank", false, 0, false},
		{"v2 Cohere text objects score like strings", "/v2/rerank", true, 0, false},
		{"v2 Cohere top_n cuts", "/v2/rerank", false, 3, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := map[string]any{"model": "rr", "query": cfRerankQuery, "documents": texts}
			if tc.objects {
				req["documents"] = objects
			}
			if tc.topN > 0 {
				req["top_n"] = tc.topN
			}
			body, _ := json.Marshal(req)
			status, ctype, raw := cfRerankPost(t, srv, tc.path, body)
			if status != http.StatusOK {
				t.Fatalf("%s: %d %.300s, want 200", tc.path, status, raw)
			}
			if ctype != "application/json" {
				t.Errorf("Content-Type %q, want application/json", ctype)
			}
			r := cfRerankStrict(t, raw, tc.echo)
			want := cfRerankOrder
			if tc.topN > 0 {
				want = want[:tc.topN]
			}
			var order []int
			for k, res := range r.Results {
				order = append(order, res.Index)
				if res.Index < 0 || res.Index >= len(cfRerankDocs) {
					t.Fatalf("result %d: index %d outside [0, %d)", k, res.Index, len(cfRerankDocs))
				}
				d := cfRerankDocs[res.Index]
				if s := 1 / (1 + math.Exp(-d.logit)); res.RelevanceScore != s {
					t.Errorf("result %d (document %d): relevance_score %v, want the sigmoid %v of the logit %v", k, res.Index, res.RelevanceScore, s, d.logit)
				}
				if res.RelevanceScore < 0 || res.RelevanceScore > 1 {
					t.Errorf("result %d: relevance_score %v outside [0, 1]", k, res.RelevanceScore)
				}
				if k > 0 && res.RelevanceScore > r.Results[k-1].RelevanceScore {
					t.Errorf("result %d: score %v above its predecessor's %v", k, res.RelevanceScore, r.Results[k-1].RelevanceScore)
				}
				switch {
				case tc.echo && res.Document == nil:
					t.Errorf("result %d: no document echo", k)
				case tc.echo && res.Document.Text != d.text:
					t.Errorf("result %d: document.text %q, want document %d's text %q", k, res.Document.Text, res.Index, d.text)
				case !tc.echo && res.Document != nil:
					t.Errorf("result %d: a document echo without return_documents", k)
				}
			}
			if !slices.Equal(order, want) {
				t.Errorf("order %v, want %v (descending, the tie of 0 and 4 in index order, top_n %d)", order, want, tc.topN)
			}
			if !strings.HasPrefix(r.ID, "rerank-") || len(r.ID) == len("rerank-") || r.Model != "rr" {
				t.Errorf("id %q model %q, want rerank-<request id> and the alias rr", r.ID, r.Model)
			}
			if n := 2 * len(cfRerankDocs); r.Usage.PromptTokens != n || r.Usage.TotalTokens != n {
				t.Errorf("usage %+v, want the engine's %d prompt tokens in both", r.Usage, n)
			}
		})
	}
}

// cfRerankPost posts body to srv's path and returns the status, the
// Content-Type and the body.
func cfRerankPost(t *testing.T, srv *httptest.Server, path string, body []byte) (int, string, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("%s: read the body: %v", path, err)
	}
	return res.StatusCode, res.Header.Get("Content-Type"), raw
}

// cfRerankStrict checks raw member by member against the answer form of
// design/02 §4.9 — exactly these members, each of its JSON type, index and
// token counts integers, document only with echo — and returns it decoded
// with unknown members refused and nothing after the object.
func cfRerankStrict(t *testing.T, raw []byte, echo bool) rerankReply {
	t.Helper()
	top := cfRerankObject(t, "answer", raw, map[string]string{"id": "string", "model": "string", "results": "array", "usage": "object"})
	var results []json.RawMessage
	if err := json.Unmarshal(top["results"], &results); err != nil {
		t.Fatalf("results: %v", err)
	}
	member := map[string]string{"index": "integer", "relevance_score": "number"}
	if echo {
		member["document"] = "object"
	}
	for k, res := range results {
		m := cfRerankObject(t, "results["+strconv.Itoa(k)+"]", res, member)
		if echo {
			cfRerankObject(t, "results["+strconv.Itoa(k)+"].document", m["document"], map[string]string{"text": "string"})
		}
	}
	cfRerankObject(t, "usage", top["usage"], map[string]string{"prompt_tokens": "integer", "total_tokens": "integer"})
	var r rerankReply
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		t.Fatalf("answer does not decode strictly: %v (%.300s)", err, raw)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		t.Fatalf("data after the answer object: %v", err)
	}
	return r
}

// cfRerankObject decodes raw as a JSON object whose members are exactly the
// keys of want, each of the JSON type want names (a "number" may be written
// as an integer).
func cfRerankObject(t *testing.T, what string, raw json.RawMessage, want map[string]string) map[string]json.RawMessage {
	t.Helper()
	if got := cfJSONType(raw); got != "object" {
		t.Fatalf("%s is a JSON %s, want an object: %.200s", what, got, raw)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	wantKeys := make([]string, 0, len(want))
	for k := range want {
		wantKeys = append(wantKeys, k)
	}
	slices.Sort(keys)
	slices.Sort(wantKeys)
	if !slices.Equal(keys, wantKeys) {
		t.Fatalf("%s has the members %v, want exactly %v", what, keys, wantKeys)
	}
	for k, typ := range want {
		got := cfJSONType(m[k])
		if got != typ && (typ != "number" || got != "integer") {
			t.Errorf("%s.%s is a JSON %s (%.60s), want %s", what, k, got, m[k], typ)
		}
	}
	return m
}

// cfJSONType names the JSON type of raw; a number without fraction and
// exponent that fits an int64 is an "integer".
func cfJSONType(raw json.RawMessage) string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "invalid"
	}
	switch x := v.(type) {
	case string:
		return "string"
	case json.Number:
		if _, err := strconv.ParseInt(x.String(), 10, 64); err == nil {
			return "integer"
		}
		return "number"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case bool:
		return "boolean"
	case nil:
		return "null"
	}
	return "invalid"
}
