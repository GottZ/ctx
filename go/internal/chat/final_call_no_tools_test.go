// H13a — engine half of the negative probe for "the chat's closing call
// carries NO tools array" (design/04 §4.8 table row 1, §2.5-g).
//
// stream_tools_absent_test.go (internal/llm) pins the WIRE half: the struct
// tag omits the key. This file pins the ENGINE half: finalCall — the single
// call the turn loop makes after the iteration cap / tool budget (engine.go:280
// → :519) — hands runStream toolsEnabled=false, so no tool definition ever
// reaches the request body of the closing call.
//
// The probe intercepts the real wire body of that closing call through an
// httptest backend and asserts the key's absence, then runs the SAME engine
// through runStream with toolsEnabled=true against a full-trust backend to
// show the array does appear otherwise (the assertion is not vacuum-green).
//
// The fake backend answers 502 so the outcome stays unserved: the closing call
// then ends in finishUnserved (error event), which touches no database — this
// keeps the probe in `go test -short` with a nil pool.
package chat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/store"
)

// bodyCapture is a fake LLM backend that records every decoded request body
// and answers 502 (an unserved, DB-free outcome).
type bodyCapture struct {
	mu     sync.Mutex
	bodies []map[string]any
}

func (c *bodyCapture) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		c.mu.Lock()
		c.bodies = append(c.bodies, body)
		c.mu.Unlock()
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (c *bodyCapture) only(t *testing.T) map[string]any {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.bodies) != 1 {
		t.Fatalf("captured %d wire bodies, want exactly 1", len(c.bodies))
	}
	return c.bodies[0]
}

// staticProvider is the narrow BackendProvider the closing call needs: one
// fixed chain, no trust arithmetic (the trust gate has its own probes).
type staticProvider struct{ chain []backends.Backend }

func (p staticProvider) ChatChain(context.Context, backends.Sensitivity, string) ([]backends.Backend, error) {
	return p.chain, nil
}

func TestFinalCallSendsNoToolsArray(t *testing.T) {
	cap := &bodyCapture{}
	srv := cap.server(t)
	b := streamBackend("closing", srv.URL)

	e := &Engine{
		provider: staticProvider{chain: []backends.Backend{b}},
		exec:     NewExecutor(nil, nil, 0),
		cfg:      Config{}.withDefaults(),
		now:      time.Now,
	}
	// The executor MUST have tools to offer — otherwise the absence below
	// would prove nothing about toolsEnabled.
	if len(e.exec.Defs()) == 0 {
		t.Fatal("executor offers no tool definitions — the probe would be vacuum-green")
	}

	sess := &store.ChatSession{ID: "sess-h13a", Scope: "private"}
	sink := newEventSink()
	err := e.finalCall(context.Background(), sess,
		[]llm.ChatMsg{{Role: "user", Content: "hi"}},
		backends.SensPublic, backends.SensPublic, backends.SensPublic,
		time.Now(), "key-h13a", "iterations",
		interactiveStreamAdmission(t, newStreamRecAdmitter(t)), sink)
	if err != nil {
		t.Fatalf("finalCall: %v", err)
	}

	body := cap.only(t)
	if v, has := body["tools"]; has {
		t.Fatalf("closing call carried the key \"tools\" (= %v) — engine.go:519 must pass toolsEnabled=false and the wire must omit the key (E4)", v)
	}
	// The E5 sibling: the closing call must not reach for tool_choice either.
	if v, has := body["tool_choice"]; has {
		t.Fatalf("closing call carried tool_choice = %v — must stay absent (E5)", v)
	}
}

// TestRunStreamWithToolsCarriesToolsArray is the contrast half: the same
// engine, the same wire path, toolsEnabled=true on a full-trust backend ⇒ the
// array IS on the wire. Without this, the absence assertion above could not
// distinguish "engine omits the array" from "this engine never sends one".
func TestRunStreamWithToolsCarriesToolsArray(t *testing.T) {
	cap := &bodyCapture{}
	srv := cap.server(t)

	e := &Engine{exec: NewExecutor(nil, nil, 0), cfg: Config{}.withDefaults(), now: time.Now}
	so := e.runStream(context.Background(),
		[]backends.Backend{streamBackend("with-tools", srv.URL)},
		[]llm.ChatMsg{{Role: "user", Content: "hi"}},
		true, 0, 1, "sess-h13a", "key-h13a", backends.SensPublic,
		interactiveStreamAdmission(t, newStreamRecAdmitter(t)), newEventSink())
	if so.served {
		t.Fatal("fake backend answered 502 — the outcome must stay unserved")
	}

	body := cap.only(t)
	arr, ok := body["tools"].([]any)
	if !ok || len(arr) != len(e.exec.Defs()) {
		t.Fatalf("tools = %v, want the executor's %d definitions on the wire", body["tools"], len(e.exec.Defs()))
	}
}
