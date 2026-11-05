package oai

import (
	"bytes"
	"encoding/json"
)

// The repeated-key check of the embeddings parse as root had it before
// FOLGE-P (jsonkeys.go@8e00a12e): FOLGE-P replaced it in the chat parse with
// levelKeys and removed it from jsonkeys.go. Kept here for the embeddings
// parse until that parse is ported to levelKeys as well.

// repeatedKey names the first member name of the object raw that occurs
// twice after decoding — the error path only, so it may allocate.
func repeatedKey(raw []byte) string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return ""
	}
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return ""
		}
		k, _ := tok.(string)
		if seen[k] {
			return k
		}
		seen[k] = true
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return ""
		}
	}
	return ""
}

// uniqueMembers refuses an object whose map form m (decoded from raw) lost a
// member to a repeated key.
func uniqueMembers(at string, raw []byte, m map[string]json.RawMessage) *paramError {
	if objectMembers(raw) == len(m) {
		return nil
	}
	return repeated(at, repeatedKey(raw))
}
