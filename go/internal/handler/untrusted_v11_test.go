// V-11 gate (design/02 §5.1 BA7, Schicht 3): the untrusted framing has to
// survive the WIRE, not just the synthesis prompt.
//
// Before this wave `llm.Source.Untrusted` carried `json:"-"` and the query
// response DTO (`sourceResponse`) had no counterpart field at all, so a
// checkpoint source reached every /api/query consumer indistinguishable from a
// first-party knowledge block. Both probes below decode the MARSHALLED bytes
// rather than reading the Go field — the silent absence of the key IS the red
// state, and a struct-literal assertion would not see it.
//
//	go test ./internal/handler/ -run TestUntrustedV11 -count=1 -v
package handler

import (
	"encoding/json"
	"testing"

	"github.com/GottZ/ctx/internal/llm"
)

// untrustedKeyOf marshals v and reports the `untrusted` key: present + value.
func untrustedKeyOf(t *testing.T, v any) (bool, bool) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	got, ok := m["untrusted"]
	if !ok {
		return false, false
	}
	b, isBool := got.(bool)
	if !isBool {
		t.Fatalf("untrusted key of %s is %T, want bool", raw, got)
	}
	return true, b
}

// TestUntrustedV11SourceSerializes pins the llm.Source tag itself. RED before
// the wave: the field is `json:"-"`, so the key never appears.
func TestUntrustedV11SourceSerializes(t *testing.T) {
	present, val := untrustedKeyOf(t, llm.Source{ID: "s1", Title: "T", Untrusted: true})
	if !present || !val {
		t.Fatalf("llm.Source{Untrusted:true} marshals untrusted=(present=%v,value=%v), want (true,true)", present, val)
	}

	// omitempty: a first-party source keeps its pre-wave bytes.
	if present, _ := untrustedKeyOf(t, llm.Source{ID: "s2", Title: "T"}); present {
		t.Fatalf("llm.Source{Untrusted:false} carries an untrusted key, want it omitted (omitempty)")
	}
}

// TestUntrustedV11QueryResponseCarriesFlag is the /api/query half of the gate:
// the response DTO is built by buildSourceResponses, and that mapping — not the
// llm.Source tag — is what a REST consumer actually reads.
//
// RED before the wave: sourceResponse has no Untrusted field, so the flag is
// dropped in the mapping even with the tag fixed.
func TestUntrustedV11QueryResponseCarriesFlag(t *testing.T) {
	out := buildSourceResponses([]llm.Source{
		{ID: "checkpoint-1", Title: "Compaction checkpoint", Category: "reference", Untrusted: true},
		{ID: "knowledge-1", Title: "Architecture note", Category: "reference"},
	}, nil, false)
	if len(out) != 2 {
		t.Fatalf("buildSourceResponses returned %d rows, want 2", len(out))
	}

	present, val := untrustedKeyOf(t, out[0])
	if !present || !val {
		t.Fatalf("untrusted source response marshals untrusted=(present=%v,value=%v), want (true,true)", present, val)
	}
	if present, _ := untrustedKeyOf(t, out[1]); present {
		t.Fatalf("first-party source response carries an untrusted key, want it omitted (omitempty)")
	}
}
