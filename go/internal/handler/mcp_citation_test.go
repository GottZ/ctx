// V-W1b gate (design/05 §7, follow-up to V-W1): the MCP `query` tool numbers
// its "Sources:" list with the `citation_index` the source carried in the
// prompt, not with its position in the response.
//
// The two orders differ for n >= 3 — LostInMiddleReorder permutes the prompt
// list (llm/synthesize.go:322-331) while the response keeps retrieval order —
// so a position-numbered list prints "[2]" in front of a source the answer's
// "[2]" does not name. The consumer of this text is a model session, i.e. the
// exact place where that offset turns into a wrong attribution.
//
// No database and no LLM: the tool delegates to a canned stub of /api/query,
// which is also the only way to pin the RED state — the offset lives in the
// rendering, not in the wire.
//
//	go test -short ./internal/handler/ -run TestMCPQuery -count=1 -v
package handler

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// cannedQueryDelegate stands in for the /api/query handler the query tool
// delegates to and answers a fixed body, so the probes below are about the
// rendering alone.
type cannedQueryDelegate struct{ body string }

func (h *cannedQueryDelegate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, _ = io.ReadAll(r.Body)
	_, _ = w.Write([]byte(h.body))
}

// mcpQueryText runs the query tool over a canned /api/query response and
// returns the tool's text content.
func mcpQueryText(t *testing.T, body string) string {
	t.Helper()
	cfg := MCPConfig{QueryHandler: &cannedQueryDelegate{body: body}}
	res, _, err := mcpQueryHandler(cfg)(typeFilterCtx(), nil, queryInput{Question: "warum bricht der Build"})
	if err != nil {
		t.Fatalf("query tool: transport error %v", err)
	}
	if res.IsError {
		t.Fatalf("query tool: IsError=true (%s), want the delegated answer", mcpTextOf(res))
	}
	return mcpTextOf(res)
}

// citationPermutationBody is the V-W1 permutation on the wire: five sources in
// retrieval order, carrying the prompt ordinals 1,5,2,3,4. That mapping is the
// one V-W1 verified against the rendered prompt (filtered[0]→1, filtered[2]→2,
// filtered[3]→3, filtered[4]→4, filtered[1]→5).
const citationPermutationBody = `{"answer":"stub answer","confidence":"confident","sources":[` +
	`{"id":"id1","title":"T1","category":"decisions","citation_index":1},` +
	`{"id":"id2","title":"T2","category":"decisions","citation_index":5},` +
	`{"id":"id3","title":"T3","category":"decisions","citation_index":2},` +
	`{"id":"id4","title":"T4","category":"decisions","citation_index":3},` +
	`{"id":"id5","title":"T5","category":"decisions","citation_index":4}]}`

// lineWithPrefix returns the single line of text starting with prefix.
func lineWithPrefix(t *testing.T, text, prefix string) string {
	t.Helper()
	var hit string
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, prefix) {
			if hit != "" {
				t.Fatalf("prefix %q appears twice in:\n%s", prefix, text)
			}
			hit = line
		}
	}
	if hit == "" {
		t.Fatalf("no line starts with %q in:\n%s", prefix, text)
	}
	return hit
}

// TestMCPQueryCitationIndexNumbersTheSourceList is the primary gate: the number
// in front of a source is its prompt ordinal, and the list reads in the order
// the answer cites it.
//
// RED before this wave: the list is numbered by response position, so "[2]"
// sits in front of id2 (prompt ordinal 5) and "[5]" in front of id5 (prompt
// ordinal 4).
func TestMCPQueryCitationIndexNumbersTheSourceList(t *testing.T) {
	got := mcpQueryText(t, citationPermutationBody)

	// The focused assertion first: "[2]" must name the source the model saw as
	// <source id="2">, which is id3 — never id2.
	if line := lineWithPrefix(t, got, "[2] "); !strings.Contains(line, "id:id3") {
		t.Errorf("[2] names %q, want the source with citation_index=2 (id3)", line)
	}

	want := "stub answer\n\nSources:\n" +
		"[1] T1 (decisions) id:id1\n" +
		"[2] T3 (decisions) id:id3\n" +
		"[3] T4 (decisions) id:id4\n" +
		"[4] T5 (decisions) id:id5\n" +
		"[5] T2 (decisions) id:id2\n"
	if got != want {
		t.Errorf("source list:\n%s\nwant:\n%s", got, want)
	}
}

// TestMCPQuerySourcesWithoutOrdinalKeepPositionNumbering is the byte-identity
// half: a retrieval-only answer (and any pre-V-W1 server) carries no
// citation_index at all, and that list is numbered by position exactly as it
// was before this wave. The literal below was captured from the unchanged
// handler.
func TestMCPQuerySourcesWithoutOrdinalKeepPositionNumbering(t *testing.T) {
	body := `{"answer":"stub answer","confidence":"confident","sources":[` +
		`{"id":"id1","title":"T1","category":"decisions"},` +
		`{"id":"id2","title":"T2","category":"reference"},` +
		`{"id":"id3","title":"T3","category":"learnings"}]}`

	want := "stub answer\n\nSources:\n" +
		"[1] T1 (decisions) id:id1\n" +
		"[2] T2 (reference) id:id2\n" +
		"[3] T3 (learnings) id:id3\n"
	if got := mcpQueryText(t, body); got != want {
		t.Errorf("retrieval-only list:\n%s\nwant (unchanged):\n%s", got, want)
	}

	// The empty-source branch is part of the same guarantee: no "Sources:"
	// block at all, the answer verbatim.
	empty := `{"answer":"stub answer","confidence":"no_relevant_blocks_found","sources":[]}`
	if got := mcpQueryText(t, empty); got != "stub answer" {
		t.Errorf("empty source list rendered %q, want the bare answer", got)
	}
}

// TestMCPQueryMarksSourcesThatNeverEnteredThePrompt is the mixed case: under
// low confidence the prompt carries LowConfidenceMaxSources sources
// (llm/synthesize.go:710-712) while the response carries all of them, so some
// sources have no ordinal. Those must not get a number — the answer cannot be
// citing them — and they keep the response order behind the cited ones.
func TestMCPQueryMarksSourcesThatNeverEnteredThePrompt(t *testing.T) {
	body := `{"answer":"stub answer","confidence":"low_confidence","sources":[` +
		`{"id":"id1","title":"T1","category":"decisions","citation_index":1},` +
		`{"id":"id2","title":"T2","category":"decisions"},` +
		`{"id":"id3","title":"T3","category":"decisions"},` +
		`{"id":"id4","title":"T4","category":"decisions","citation_index":2},` +
		`{"id":"id5","title":"T5","category":"decisions"}]}`

	want := "stub answer\n\nSources:\n" +
		"[1] T1 (decisions) id:id1\n" +
		"[2] T4 (decisions) id:id4\n" +
		"[n/a] T2 (decisions) id:id2\n" +
		"[n/a] T3 (decisions) id:id3\n" +
		"[n/a] T5 (decisions) id:id5\n"
	got := mcpQueryText(t, body)
	if got != want {
		t.Errorf("mixed list:\n%s\nwant:\n%s", got, want)
	}
	// A source without an ordinal must never carry a number that could be read
	// as a citation.
	for _, id := range []string{"id2", "id3", "id5"} {
		if strings.Contains(got, "] T"+id[2:]+" ") && !strings.Contains(got, "[n/a] T"+id[2:]+" ") {
			t.Errorf("source %s without a prompt ordinal got numbered:\n%s", id, got)
		}
	}
}
