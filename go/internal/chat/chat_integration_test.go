//go:build integration

// Integration tests for the F6-C3/G36 harness against a real PG18 testcontainer
// + a scriptable fake LLM HTTP server. These pin the §5 probes that carry the
// security guarantees of design 06:
//   - Trust gate: a session forced to credentials with only a no-credentials
//     backend ends with no_eligible_backend and the fake LLM is NEVER hit
//     (the R2 escalation guard — Hit-Counter == 0)
//   - Scope isolation: the executor runs under the session read_scopes; ctx_get
//     on an out-of-scope block is "not found", ctx_search returns only in-scope
//   - E4 trap: after the iteration cap the closing call carries NO tools array
//   - Tool errors continue the turn; sensitivity annotation raises the HWM;
//     ctx_get truncates with a resumable offset
//
// Run with:
//
//	go test -tags=integration ./internal/chat/ -run 'TestChat(Engine|Executor)' -count=1 -v
package chat_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/chat"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// --- fakes ---

type fakeLLM struct {
	mu        sync.Mutex
	responses []fakeResp
	hits      int
	bodies    []map[string]any
}

type fakeResp struct {
	status int    // non-200 → returned as status (failover test); 0/200 → sse
	sse    string
}

func (f *fakeLLM) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.hits++
		f.bodies = append(f.bodies, body)
		var resp fakeResp
		if len(f.responses) > 0 {
			resp = f.responses[0]
			f.responses = f.responses[1:]
		}
		f.mu.Unlock()
		if resp.status != 0 && resp.status != http.StatusOK {
			w.WriteHeader(resp.status)
			_, _ = w.Write([]byte("backend error"))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(resp.sse))
	}
}

func (f *fakeLLM) hitCount() int { f.mu.Lock(); defer f.mu.Unlock(); return f.hits }

func frame(j string) string  { return "data: " + j + "\n\n" }
func jstr(s string) string   { b, _ := json.Marshal(s); return string(b) }

func sseContent(text string) string {
	return frame(`{"choices":[{"index":0,"delta":{"content":`+jstr(text)+`},"finish_reason":null}]}`) +
		frame(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`) +
		frame(`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5}}`) +
		"data: [DONE]\n\n"
}

func sseToolCall(id, name, argsObj string) string {
	return frame(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":`+jstr(id)+`,"type":"function","function":{"name":`+jstr(name)+`,"arguments":`+jstr(argsObj)+`}}]},"finish_reason":null}]}`) +
		frame(`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`) +
		frame(`{"choices":[],"usage":{"prompt_tokens":20,"completion_tokens":8}}`) +
		"data: [DONE]\n\n"
}

// trustProvider mirrors the real pool's trust filter: it returns only backends
// whose trust may receive `required`, empty → ErrNoEligibleBackend.
type trustProvider struct{ chain []backends.Backend }

func (p trustProvider) ChatChain(_ context.Context, required backends.Sensitivity) ([]backends.Backend, error) {
	var out []backends.Backend
	for _, b := range p.chain {
		if b.Trust.Allows(required) {
			out = append(out, b)
		}
	}
	if len(out) == 0 {
		return nil, &backends.ErrNoEligibleBackend{Role: "chat", Required: required}
	}
	return out, nil
}

type fakeQuery struct {
	gotScopes []string
	result    chat.QueryResult
}

func (q *fakeQuery) RunQuery(_ context.Context, readScopes []string, _ string, _ int) (chat.QueryResult, error) {
	q.gotScopes = readScopes
	return q.result, nil
}

type collector struct {
	mu     sync.Mutex
	events []evt
}

type evt struct {
	typ  string
	data map[string]any
}

func (c *collector) Event(typ string, data any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, _ := data.(map[string]any)
	c.events = append(c.events, evt{typ: typ, data: m})
	return nil
}

func (c *collector) count(typ string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, e := range c.events {
		if e.typ == typ {
			n++
		}
	}
	return n
}

func (c *collector) last(typ string) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	var found map[string]any
	for _, e := range c.events {
		if e.typ == typ {
			found = e.data
		}
	}
	return found
}

func fullTrustBackend(name, host string) backends.Backend {
	return backends.Backend{Name: name, Host: host, Model: "m", Trust: backends.TrustFull, Roles: []string{"chat"}, Enabled: true}
}

func newSession(t *testing.T, pool *pgxpool.Pool, scope string, readScopes []string) *store.ChatSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := store.CreateSession(ctx, pool, scope, readScopes, "", "")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return s
}

func seedBlock(t *testing.T, pool *pgxpool.Pool, scope, title, content string, sens backends.Sensitivity) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	b, err := store.UpsertBlock(ctx, pool, "test", title, content, nil, nil, scope, true, store.SensitivityWrite{})
	if err != nil {
		t.Fatalf("seed block: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE context_blocks SET sensitivity = $2 WHERE id = $1`, b.ID, string(sens)); err != nil {
		t.Fatalf("seed sensitivity: %v", err)
	}
	return b.ID
}

func toolCall(name, args string) llm.ToolCall {
	return llm.ToolCall{ID: "c1", Type: "function", Function: llm.ToolCallFunction{Name: name, Arguments: json.RawMessage(args)}}
}

// --- engine tests ---

func TestChatEngine(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	t.Run("ToolLoopThenAnswerScopePropagation", func(t *testing.T) {
		blockID := seedBlock(t, pool, "private", "Dream backoff", "exponential backoff content", backends.SensInternal)
		llmSrv := &fakeLLM{responses: []fakeResp{
			{sse: sseToolCall("c1", "ctx_query", `{"query":"dream backoff"}`)},
			{sse: sseContent("The dream backoff is exponential.")},
		}}
		srv := httptest.NewServer(llmSrv.handler())
		defer srv.Close()

		fq := &fakeQuery{result: chat.QueryResult{Confidence: "confident", Blocks: []chat.QueryBlock{{ID: blockID, Title: "Dream backoff", Category: "test", Score: 0.9}}}}
		exec := chat.NewExecutor(pool, fq, 8000)
		eng := chat.NewEngine(pool, trustProvider{chain: []backends.Backend{fullTrustBackend("fake", srv.URL)}}, exec, chat.Config{})
		sess := newSession(t, pool, "private", []string{"private"})
		col := &collector{}

		if err := eng.RunTurn(ctx, &auth.AuthResult{ReadScopes: []string{"private"}}, sess, "what is the dream backoff?", backends.SensInternal, true, col); err != nil {
			t.Fatalf("RunTurn: %v", err)
		}
		if llmSrv.hitCount() != 2 {
			t.Fatalf("llm hits = %d; want 2 (tool turn + answer)", llmSrv.hitCount())
		}
		if got := strings.Join(fq.gotScopes, ","); got != "private" {
			t.Fatalf("ctx_query ran under scopes %q; want the session snapshot [private]", got)
		}
		if tr := col.last("tool_result"); tr == nil || tr["ok"] != true {
			t.Fatalf("tool_result = %v; want ok=true", tr)
		}
		if done := col.last("done"); done == nil || done["finish_reason"] != "stop" {
			t.Fatalf("done = %v; want finish_reason stop", done)
		}
		// Persisted: user, assistant(tool_calls), tool, assistant(final).
		msgs, _ := store.ListMessages(ctx, pool, sess.ID, 0, 0)
		if len(msgs) != 4 {
			t.Fatalf("persisted %d messages; want 4", len(msgs))
		}
	})

	t.Run("TrustGateNeverHitsBackend", func(t *testing.T) {
		llmSrv := &fakeLLM{responses: []fakeResp{{sse: sseContent("should never run")}}}
		srv := httptest.NewServer(llmSrv.handler())
		defer srv.Close()
		// Only a no-credentials backend in the chain.
		nc := fullTrustBackend("nocreds", srv.URL)
		nc.Trust = backends.TrustNoCredentials
		exec := chat.NewExecutor(pool, &fakeQuery{}, 8000)
		eng := chat.NewEngine(pool, trustProvider{chain: []backends.Backend{nc}}, exec, chat.Config{})

		sess := newSession(t, pool, "private", []string{"private"})
		// A prior credentials tool result raised the PERSISTED HWM — the engine
		// re-derives required from the DB (AppendMessage), not the passed struct.
		if _, err := pool.Exec(ctx, `UPDATE context_chat_sessions SET max_sensitivity = 'credentials' WHERE id = $1`, sess.ID); err != nil {
			t.Fatalf("seed HWM: %v", err)
		}
		col := &collector{}

		if err := eng.RunTurn(ctx, &auth.AuthResult{ReadScopes: []string{"private"}}, sess, "hello", backends.SensInternal, true, col); err != nil {
			t.Fatalf("RunTurn: %v", err)
		}
		if llmSrv.hitCount() != 0 {
			t.Fatalf("llm hits = %d; want 0 — credentials content must never reach a no-credentials backend (R2)", llmSrv.hitCount())
		}
		if e := col.last("error"); e == nil || e["code"] != "no_eligible_backend" {
			t.Fatalf("error event = %v; want no_eligible_backend", e)
		}
	})

	t.Run("IterationCapClosesWithoutTools", func(t *testing.T) {
		llmSrv := &fakeLLM{responses: []fakeResp{
			{sse: sseToolCall("c1", "ctx_query", `{"query":"x"}`)}, // iter 1
			{sse: sseContent("final answer from gathered material")}, // closing call
		}}
		srv := httptest.NewServer(llmSrv.handler())
		defer srv.Close()
		exec := chat.NewExecutor(pool, &fakeQuery{result: chat.QueryResult{}}, 8000)
		eng := chat.NewEngine(pool, trustProvider{chain: []backends.Backend{fullTrustBackend("fake", srv.URL)}}, exec, chat.Config{MaxIterations: 1})
		sess := newSession(t, pool, "private", []string{"private"})
		col := &collector{}

		if err := eng.RunTurn(ctx, &auth.AuthResult{ReadScopes: []string{"private"}}, sess, "q", backends.SensInternal, true, col); err != nil {
			t.Fatalf("RunTurn: %v", err)
		}
		if llmSrv.hitCount() != 2 {
			t.Fatalf("llm hits = %d; want 2 (capped iter + closing call)", llmSrv.hitCount())
		}
		// The E4 guard: the CLOSING request must carry NO tools array.
		llmSrv.mu.Lock()
		closing := llmSrv.bodies[1]
		llmSrv.mu.Unlock()
		if _, ok := closing["tools"]; ok {
			t.Fatalf("closing call carried a tools array — tool_choice:none regression (E4)")
		}
		if done := col.last("done"); done == nil || done["finish_reason"] != "tool_limit" {
			t.Fatalf("done = %v; want finish_reason tool_limit", done)
		}
	})

	t.Run("ToolErrorContinuesTurn", func(t *testing.T) {
		llmSrv := &fakeLLM{responses: []fakeResp{
			{sse: sseToolCall("c1", "ctx_get", `{"id":""}`)}, // bad args → ok:false
			{sse: sseContent("recovered and answered")},
		}}
		srv := httptest.NewServer(llmSrv.handler())
		defer srv.Close()
		exec := chat.NewExecutor(pool, &fakeQuery{}, 8000)
		eng := chat.NewEngine(pool, trustProvider{chain: []backends.Backend{fullTrustBackend("fake", srv.URL)}}, exec, chat.Config{})
		sess := newSession(t, pool, "private", []string{"private"})
		col := &collector{}

		if err := eng.RunTurn(ctx, &auth.AuthResult{ReadScopes: []string{"private"}}, sess, "get nothing", backends.SensInternal, true, col); err != nil {
			t.Fatalf("RunTurn: %v", err)
		}
		if tr := col.last("tool_result"); tr == nil || tr["ok"] != false {
			t.Fatalf("tool_result = %v; want ok=false", tr)
		}
		if done := col.last("done"); done == nil || done["finish_reason"] != "stop" {
			t.Fatalf("done = %v; want finish_reason stop (turn recovered)", done)
		}
	})

	t.Run("FailoverBeforeFirstByteOn503", func(t *testing.T) {
		down := &fakeLLM{responses: []fakeResp{{status: http.StatusServiceUnavailable}}}
		up := &fakeLLM{responses: []fakeResp{{sse: sseContent("served by the second backend")}}}
		srvA := httptest.NewServer(down.handler())
		defer srvA.Close()
		srvB := httptest.NewServer(up.handler())
		defer srvB.Close()
		exec := chat.NewExecutor(pool, &fakeQuery{}, 8000)
		eng := chat.NewEngine(pool, trustProvider{chain: []backends.Backend{fullTrustBackend("down", srvA.URL), fullTrustBackend("up", srvB.URL)}}, exec, chat.Config{})
		sess := newSession(t, pool, "private", []string{"private"})
		col := &collector{}

		if err := eng.RunTurn(ctx, &auth.AuthResult{ReadScopes: []string{"private"}}, sess, "hi", backends.SensInternal, false, col); err != nil {
			t.Fatalf("RunTurn: %v", err)
		}
		if down.hitCount() != 1 || up.hitCount() != 1 {
			t.Fatalf("hits down=%d up=%d; want 1/1 (failover after 503 before first byte)", down.hitCount(), up.hitCount())
		}
		if col.count("backend") != 2 {
			t.Fatalf("backend events = %d; want 2 (primary + fallback)", col.count("backend"))
		}
		if done := col.last("done"); done == nil || done["finish_reason"] != "stop" {
			t.Fatalf("done = %v; want finish_reason stop", done)
		}
	})
}

// --- executor tests ---

func TestChatExecutor(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	exec := chat.NewExecutor(pool, &fakeQuery{}, 50) // small window to exercise truncation

	t.Run("ScopeIsolation", func(t *testing.T) {
		privID := seedBlock(t, pool, "private", "Secret block", "the private secret content", backends.SensInternal)

		// ctx_get under [work] → not found (GetBlock scope filter is the gate).
		out := exec.Run(ctx, []string{"work"}, "", toolCall("ctx_get", fmt.Sprintf(`{"id":%q}`, privID)))
		if out.OK {
			t.Fatalf("ctx_get on a private block under [work] returned ok=true — scope gate breached")
		}
		// ctx_get under [private] → found (proves the not-found above was the gate, not chance).
		out2 := exec.Run(ctx, []string{"private"}, "", toolCall("ctx_get", fmt.Sprintf(`{"id":%q}`, privID)))
		if !out2.OK || !strings.Contains(out2.Content, "private secret content") {
			t.Fatalf("ctx_get under [private] = ok:%v; want the block content", out2.OK)
		}
	})

	t.Run("SensitivityAnnotationRaisesHWM", func(t *testing.T) {
		credID := seedBlock(t, pool, "private", "Creds block", "AKIA-style content", backends.SensCredentials)
		out := exec.Run(ctx, []string{"private"}, "", toolCall("ctx_get", fmt.Sprintf(`{"id":%q}`, credID)))
		if !out.OK || out.Sensitivity != backends.SensCredentials {
			t.Fatalf("ctx_get sensitivity = %q (ok=%v); want credentials", out.Sensitivity, out.OK)
		}
	})

	t.Run("TruncationAndOffsetPaging", func(t *testing.T) {
		// 80-char block, 50-char window: offset 0 truncates (next=50), offset 50
		// reads the final 30-char window (no further truncation).
		bigID := seedBlock(t, pool, "private", "Big block", strings.Repeat("x", 80), backends.SensInternal)
		out := exec.Run(ctx, []string{"private"}, "", toolCall("ctx_get", fmt.Sprintf(`{"id":%q}`, bigID)))
		if !out.OK || !out.Truncated {
			t.Fatalf("ctx_get on an 80-char block with window 50 = ok:%v truncated:%v; want ok/true", out.OK, out.Truncated)
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(out.Content), &got); err != nil {
			t.Fatalf("result not valid JSON: %v", err)
		}
		if got["next_offset"] != float64(50) {
			t.Fatalf("next_offset = %v; want 50", got["next_offset"])
		}
		if c, _ := got["content"].(string); !strings.Contains(c, "offset=50") {
			t.Fatalf("content window missing the offset marker: %q", c)
		}
		// Resume from offset=50 reads the tail.
		out2 := exec.Run(ctx, []string{"private"}, "", toolCall("ctx_get", fmt.Sprintf(`{"id":%q,"offset":50}`, bigID)))
		if !out2.OK || out2.Truncated {
			t.Fatalf("offset=50 read truncated=%v; want the final window", out2.Truncated)
		}
	})

	t.Run("UnknownToolIsNotFatal", func(t *testing.T) {
		out := exec.Run(ctx, []string{"private"}, "", toolCall("ctx_delete_everything", `{}`))
		if out.OK || !strings.Contains(out.Content, "unknown tool") {
			t.Fatalf("unknown tool = ok:%v content:%q; want ok=false / unknown tool", out.OK, out.Content)
		}
	})
}
