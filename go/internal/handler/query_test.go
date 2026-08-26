package handler

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/llm"
)

// In heartbeat mode the 200 header commits up front, the keepalive emits leading
// whitespace, and the final body must still decode as JSON (RFC 8259 allows
// leading whitespace).
func TestQueryHeartbeat_WhitespaceThenValidJSON(t *testing.T) {
	old := heartbeatInterval
	heartbeatInterval = 5 * time.Millisecond
	defer func() { heartbeatInterval = old }()

	rec := httptest.NewRecorder()
	hb := startHeartbeat(rec, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("200 header not committed up front: code=%d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	time.Sleep(40 * time.Millisecond) // let a few heartbeats fire
	hb.finish(http.StatusOK, queryResponse{Success: true, Answer: "ok", Confidence: "confident"})

	body := rec.Body.String()
	if !strings.HasPrefix(body, " ") {
		t.Errorf("expected leading heartbeat whitespace, got prefix %q", body[:min(20, len(body))])
	}
	var resp queryResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("body not valid JSON despite leading whitespace: %v | body=%q", err, body)
	}
	if !resp.Success || resp.Answer != "ok" {
		t.Errorf("decoded response wrong: %+v", resp)
	}
}

// Inactive (fast path) = plain writeJSON: honours the status code, no whitespace.
func TestQueryHeartbeat_InactivePlainWriteJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	hb := startHeartbeat(rec, false)
	hb.finish(http.StatusInternalServerError, map[string]any{"success": false, "error": "boom"})
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("inactive code = %d, want 500", rec.Code)
	}
	if strings.HasPrefix(rec.Body.String(), " ") {
		t.Error("inactive path must not emit heartbeat whitespace")
	}
}

// Regression for the first live 504: the Logger middleware's responseWriter
// wrapper had no Unwrap(), so http.NewResponseController could not reach the
// real Flusher and every heartbeat flush buffered until the handler returned —
// no streaming, the proxy read timeout fired anyway. Run the heartbeat under
// the real global middleware stack (server.go order) on a real TCP server and
// require the first body byte to arrive while the handler is still inside its
// slow stage.
func TestQueryHeartbeat_StreamsThroughMiddlewareStack(t *testing.T) {
	old := heartbeatInterval
	heartbeatInterval = 10 * time.Millisecond
	defer func() { heartbeatInterval = old }()

	const handlerWork = 300 * time.Millisecond
	h := SecurityHeaders(RequestID(Logger(Recovery(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hb := startHeartbeat(w, true)
		time.Sleep(handlerWork) // stands in for the slow rerank stage
		hb.finish(http.StatusOK, queryResponse{Success: true, Answer: "done", Confidence: "confident"})
	})))))
	srv := httptest.NewServer(h)
	defer srv.Close()

	start := time.Now()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	// http.Get returns once headers are in; the streaming proof is the first
	// BODY byte (a heartbeat space) arriving well before the handler finishes.
	buf := make([]byte, 1)
	n, err := resp.Body.Read(buf)
	firstByteAfter := time.Since(start)
	if err != nil || n != 1 {
		t.Fatalf("first body read: n=%d err=%v", n, err)
	}
	if firstByteAfter >= handlerWork {
		t.Fatalf("first body byte after %v (handler works for %v) — flushes are buffered, not streamed (Unwrap regression?)",
			firstByteAfter, handlerWork)
	}

	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var qr queryResponse
	if err := json.Unmarshal(append(buf[:n], rest...), &qr); err != nil {
		t.Fatalf("streamed body not valid JSON: %v", err)
	}
	if !qr.Success || qr.Answer != "done" {
		t.Errorf("decoded response wrong: %+v", qr)
	}
}

func TestBuildSourceResponses(t *testing.T) {
	rs := 0.5
	orig := 0.008
	sources := []llm.Source{
		{ID: "a", Title: "A", Category: "x", Score: 0.92, AgeDays: 3, RerankScore: &rs, RRFScoreOriginal: &orig},
		{ID: "b", Title: "B", Category: "y", Score: 0.10, AgeDays: 1},
	}
	supersedes := map[string][]string{"a": {"superseder-1", "superseder-2"}}

	out := buildSourceResponses(sources, supersedes, false)
	if len(out) != 2 {
		t.Fatalf("got %d responses, want 2", len(out))
	}
	// Field mapping + pointers carried through.
	if out[0].ID != "a" || out[0].Title != "A" || out[0].Category != "x" || out[0].Score != 0.92 || out[0].AgeDays != 3 {
		t.Errorf("source[0] fields mismatch: %+v", out[0])
	}
	if out[0].RerankScore == nil || *out[0].RerankScore != 0.5 {
		t.Errorf("source[0] RerankScore not carried: %+v", out[0].RerankScore)
	}
	// First superseder is attached.
	if out[0].SupersededBy == nil || *out[0].SupersededBy != "superseder-1" {
		t.Errorf("source[0] SupersededBy = %v, want superseder-1", out[0].SupersededBy)
	}
	// No supersedes entry → nil.
	if out[1].SupersededBy != nil {
		t.Errorf("source[1] SupersededBy = %v, want nil", *out[1].SupersededBy)
	}
}

// TestBuildSourceResponses_CitationIndex is the API half of V-W1: the ordinal
// the synthesis step resolved has to reach the wire, and a source without one
// must leave the field out entirely — a "citation_index": 0 would name source
// id="0", which no prompt ever renders.
func TestBuildSourceResponses_CitationIndex(t *testing.T) {
	two := 2
	out := buildSourceResponses([]llm.Source{
		{ID: "a", Title: "A", CitationIndex: &two},
		{ID: "b", Title: "B"},
	}, nil, false)
	if out[0].CitationIndex == nil || *out[0].CitationIndex != 2 {
		t.Errorf("source[0] CitationIndex = %v, want 2", out[0].CitationIndex)
	}
	if out[1].CitationIndex != nil {
		t.Errorf("source[1] CitationIndex = %v, want nil", *out[1].CitationIndex)
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); !strings.Contains(got, `"citation_index":2`) {
		t.Errorf("citation_index missing from the wire shape: %s", got)
	} else if strings.Count(got, "citation_index") != 1 {
		t.Errorf("the source without an ordinal serialized the key anyway: %s", got)
	}
}

func TestBuildSourceResponses_NilMap(t *testing.T) {
	// A nil supersedes map must not panic and yields no SupersededBy.
	out := buildSourceResponses([]llm.Source{{ID: "a", Title: "A"}}, nil, false)
	if len(out) != 1 || out[0].SupersededBy != nil {
		t.Errorf("nil map: got %+v", out)
	}
}

func TestBuildSourceResponses_IncludeContent(t *testing.T) {
	long := strings.Repeat("ä", maxRetrievalSnippet+500) // multibyte, over the cap
	sources := []llm.Source{{ID: "a", Title: "A", Content: long}}
	// Default (eval.sh / A-B sweep): no content attached, responses unchanged.
	if out := buildSourceResponses(sources, nil, false); out[0].Content != "" {
		t.Errorf("include_content=false must not attach content")
	}
	// include_content: snippet capped at maxRetrievalSnippet runes (rune-safe).
	out := buildSourceResponses(sources, nil, true)
	if n := len([]rune(out[0].Content)); n != maxRetrievalSnippet {
		t.Errorf("snippet = %d runes, want %d", n, maxRetrievalSnippet)
	}
}
