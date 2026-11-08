package oai

// The exact length of rerank.Score's body (W02-10; FOLGE-R since RC-D S2):
// the wire term books the body's build on rerankWireBodyLen, so the length
// must be the wire's, byte for byte, for every form a document can take.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestRerankWireBodyLenIsTheEncoders: rerankWireBodyLen equals the length of
// rerank.Score's body as encoding/json writes it without HTML escaping
// (json.Encoder with SetEscapeHTML(false), its newline cut: model, query,
// documents, return_documents false — the byte writer's form, FOLGE-R) —
// ASCII, the HTML characters (one byte each on the wire), quotes and
// backslashes, control characters, U+2028/U+2029, multi-byte runes and
// invalid UTF-8. Until FOLGE-R the oracle was json.Marshal (HTML escaping,
// no return_documents): the body the wire wrote then.
func TestRerankWireBodyLenIsTheEncoders(t *testing.T) {
	type wireReq struct {
		Model           string   `json:"model"`
		Query           string   `json:"query"`
		Documents       []string `json:"documents"`
		ReturnDocuments bool     `json:"return_documents"`
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
				var buf bytes.Buffer
				enc := json.NewEncoder(&buf)
				enc.SetEscapeHTML(false)
				if err := enc.Encode(wireReq{model, q, docs, false}); err != nil {
					t.Fatal(err)
				}
				raw := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
				if got := rerankWireBodyLen(model, q, docs); got != len(raw) {
					t.Errorf("model %.40q query %.40q docs %.40q: rerankWireBodyLen %d, the encoder %d", model, q, docs, got, len(raw))
				}
			}
		}
	}
}
