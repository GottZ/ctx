package handler

// Issue #280, the rendering half of the untrusted_origin record (no database):
// the MCP query tool's "Sources:" list and its retrieval-only JSON, fed the
// /api/query answer as wire JSON — so the probes compile against the tree
// before the change and fail there at run time.

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRetypeOriginMCPSourceLines: a source with a record gets the note on a
// line of its own under its source line; a source without one keeps its exact
// line, and the source line itself keeps the shape a parser that reads up to
// the id expects.
func TestRetypeOriginMCPSourceLines(t *testing.T) {
	const body = `{"answer":"stub answer","confidence":"confident","sources":[` +
		`{"id":"id1","title":"T1","category":"decisions","citation_index":1,"untrusted_origin":"tool-evidence"},` +
		`{"id":"id2","title":"T2","category":"projects","citation_index":2},` +
		`{"id":"id3","title":"T3","category":"projects","untrusted_origin":"checkpoint"}]}`
	text, isErr := runMCPQuery(t, &cannedQueryDelegate{body: body}, `{"question":"q"}`)
	if isErr {
		t.Fatalf("IsError=true: %s", text)
	}
	want := "stub answer\n\nSources:\n" +
		"[1] T1 (decisions) id:id1\n" +
		"    untrusted_origin:tool-evidence (agent-produced text: re-typed out of a type marked untrusted)\n" +
		"[2] T2 (projects) id:id2\n" +
		"[n/a] T3 (projects) id:id3\n" +
		"    untrusted_origin:checkpoint (agent-produced text: re-typed out of a type marked untrusted)\n"
	if text != want {
		t.Errorf("Sources list:\n%s\nwant:\n%s", text, want)
	}
}

// TestRetypeOriginMCPRetrievalOnly: the retrieval-only JSON carries the field
// under /api/query's name and leaves it out for a source without a record.
func TestRetypeOriginMCPRetrievalOnly(t *testing.T) {
	const body = `{"success":true,"confidence":"confident","sources":[` +
		`{"id":"id1","title":"T1","category":"decisions","score":0.5,"untrusted_origin":"tool-evidence"},` +
		`{"id":"id2","title":"T2","category":"projects","score":0.4}]}`
	text, isErr := runMCPQuery(t, &cannedQueryDelegate{body: body}, `{"question":"q","synthesize":false}`)
	if isErr {
		t.Fatalf("IsError=true: %s", text)
	}
	var out struct {
		Sources []map[string]any `json:"sources"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("decode: %v in %s", err, text)
	}
	if len(out.Sources) != 2 || out.Sources[0]["untrusted_origin"] != "tool-evidence" {
		t.Errorf("retrieval-only sources = %v, want the first with untrusted_origin tool-evidence", out.Sources)
	}
	if _, ok := out.Sources[1]["untrusted_origin"]; ok || strings.Count(text, "untrusted_origin") != 1 {
		t.Errorf("a source without a record carries the key:\n%s", text)
	}
}
