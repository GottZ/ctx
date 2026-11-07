package oai

// The exact length of rerank.Score's body (W02-10): the wire term books its
// factor on rerankWireBodyLen, so the length must be json.Marshal's, byte
// for byte, for every form a document can take.

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRerankWireBodyLenIsMarshals: rerankWireBodyLen equals the length of
// json.Marshal of rerank.Score's body (internal/rerank rerankRequest: model,
// query, documents) — ASCII, the HTML characters json.Marshal escapes to six
// bytes, quotes and backslashes, control characters, U+2028/U+2029,
// multi-byte runes and invalid UTF-8.
func TestRerankWireBodyLenIsMarshals(t *testing.T) {
	type wireReq struct {
		Model     string   `json:"model"`
		Query     string   `json:"query"`
		Documents []string `json:"documents"`
	}
	forms := []string{
		"plain text", "", "<b>&amp;</b>", `"quoted" \ back`, "\x01\x1f\t\n\r\b\f",
		"line sep ", "äöü ß 😀", "bad \xff\xfe bytes", strings.Repeat("<>&", 1000),
	}
	for _, model := range []string{"M", "<model>"} {
		for _, q := range forms {
			for k := 1; k <= 3; k++ {
				docs := make([]string, k)
				for i := range docs {
					docs[i] = forms[(i*3+k)%len(forms)]
				}
				raw, err := json.Marshal(wireReq{model, q, docs})
				if err != nil {
					t.Fatal(err)
				}
				if got := rerankWireBodyLen(model, q, docs); got != len(raw) {
					t.Errorf("model %q query %q docs %q: rerankWireBodyLen %d, json.Marshal %d", model, q, docs, got, len(raw))
				}
			}
		}
	}
}
