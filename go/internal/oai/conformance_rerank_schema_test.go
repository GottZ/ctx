package oai_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// rerankResponseExcerpt is the ctx-owned schema of the rerank answer of both
// dialects (W06-19 S2: OpenAI has no rerank endpoint, so the schema set had
// a gap there — TestContractSchemaSetComplete).
const rerankResponseExcerpt = "../../conformance/oai/testdata/ctx-rerank-response.json"

// TestConformanceRerankSchema validates every answer of the rerank harness —
// /v1 (Jina, documents echoed) and /v2 (Cohere v2, no echo), strings and
// {text} objects, with and without top_n — against the vendored rerank
// schema, and shows the schema's teeth on answers it must refuse.
func TestConformanceRerankSchema(t *testing.T) {
	schema := loadSchema(t, rerankResponseExcerpt, "RerankResponse")
	srv := httptest.NewServer(newCfRerankRig(t).router)
	t.Cleanup(srv.Close)
	texts := make([]string, len(cfRerankDocs))
	objects := make([]map[string]string, len(cfRerankDocs))
	for i, d := range cfRerankDocs {
		texts[i] = d.text
		objects[i] = map[string]string{"text": d.text}
	}
	var valid []byte
	for _, path := range []string{"/v1/rerank", "/v2/rerank"} {
		for _, docs := range []any{texts, objects} {
			for _, topN := range []int{0, 2} {
				req := map[string]any{"model": "rr", "query": cfRerankQuery, "documents": docs}
				if topN > 0 {
					req["top_n"] = topN
				}
				body, _ := json.Marshal(req)
				status, _, raw := cfRerankPost(t, srv, path, body)
				if status != http.StatusOK {
					t.Fatalf("%s: %d %.200s, want 200", path, status, raw)
				}
				var v any
				if err := json.Unmarshal(raw, &v); err != nil {
					t.Fatalf("%s: answer is not JSON: %v", path, err)
				}
				if err := schema.Validate(v); err != nil {
					t.Errorf("%s (top_n %d): answer fails RerankResponse: %v (%.300s)", path, topN, err, raw)
				}
				if path == "/v1/rerank" {
					valid = raw
				}
			}
		}
	}
	// Teeth: the schema refuses what a client would trip over.
	for name, mutate := range map[string]func(m map[string]any){
		"extra top-level member":   func(m map[string]any) { m["object"] = "list" },
		"no usage":                 func(m map[string]any) { delete(m, "usage") },
		"score above 1":            func(m map[string]any) { firstResult(m)["relevance_score"] = 1.5 },
		"score as string":          func(m map[string]any) { firstResult(m)["relevance_score"] = "0.9" },
		"negative index":           func(m map[string]any) { firstResult(m)["index"] = -1 },
		"document without text":    func(m map[string]any) { firstResult(m)["document"] = map[string]any{} },
		"extra member in a result": func(m map[string]any) { firstResult(m)["score"] = 0.5 },
	} {
		var m map[string]any
		if err := json.Unmarshal(valid, &m); err != nil {
			t.Fatal(err)
		}
		mutate(m)
		if err := schema.Validate(m); err == nil {
			t.Errorf("teeth: the rerank schema accepts an answer with %s", name)
		}
	}
}

// firstResult is the first result of a decoded rerank answer.
func firstResult(m map[string]any) map[string]any {
	return m["results"].([]any)[0].(map[string]any)
}
