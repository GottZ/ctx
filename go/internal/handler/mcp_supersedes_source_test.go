// Issue #197, second comment: in synthesis mode the MCP `query` tool lists its
// sources as prose lines, which carried no superseded_by — a client could not
// tell that a cited source was replaced. The line now ends with
// " superseded_by:<id>" for such a source and is unchanged for every other one.
//
//	go test -short ./internal/handler/ -run TestMCPQuerySourceLineSupersededBy -count=1 -v
package handler

import "testing"

func TestMCPQuerySourceLineSupersededBy(t *testing.T) {
	body := `{"answer":"stub answer","confidence":"confident","sources":[` +
		`{"id":"id1","title":"T1","category":"decisions","citation_index":1,"superseded_by":"id9"},` +
		`{"id":"id2","title":"T2","category":"decisions","citation_index":2},` +
		`{"id":"id3","title":"T3","category":"decisions"}]}`
	want := "stub answer\n\nSources:\n" +
		"[1] T1 (decisions) id:id1 superseded_by:id9\n" +
		"[2] T2 (decisions) id:id2\n" +
		mcpNoCitationMarker + " T3 (decisions) id:id3\n"
	if got := mcpQueryText(t, body); got != want {
		t.Errorf("source list:\n%s\nwant:\n%s", got, want)
	}

	positional := `{"answer":"stub answer","confidence":"confident","sources":[` +
		`{"id":"id1","title":"T1","category":"decisions","superseded_by":"id9"}]}`
	wantPositional := "stub answer\n\nSources:\n[1] T1 (decisions) id:id1 superseded_by:id9\n"
	if got := mcpQueryText(t, positional); got != wantPositional {
		t.Errorf("positional list:\n%s\nwant:\n%s", got, wantPositional)
	}
}
