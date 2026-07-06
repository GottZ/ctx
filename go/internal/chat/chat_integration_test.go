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
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// testAdmitter is the MW5 pass-through admission for engine tests (empty
// policy = Durchreiche). The test AuthResults carry no ApiKeyID, so the B8
// downgrade to background fires — with an empty policy both classes are
// behavior-identical pass-through.
func testAdmitter(t *testing.T) dispatch.Admitter {
	t.Helper()
	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	return d
}

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

func (p trustProvider) ChatChain(_ context.Context, required backends.Sensitivity, _ string) ([]backends.Backend, error) {
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
	b, err := store.UpsertBlock(ctx, pool, "test", title, content, nil, nil, scope, true, store.SensitivityWrite{}, "")
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
		eng := chat.NewEngine(pool, trustProvider{chain: []backends.Backend{fullTrustBackend("fake", srv.URL)}}, exec, chat.Config{}, testAdmitter(t))
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
		eng := chat.NewEngine(pool, trustProvider{chain: []backends.Backend{nc}}, exec, chat.Config{}, testAdmitter(t))

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
		eng := chat.NewEngine(pool, trustProvider{chain: []backends.Backend{fullTrustBackend("fake", srv.URL)}}, exec, chat.Config{MaxIterations: 1}, testAdmitter(t))
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
		eng := chat.NewEngine(pool, trustProvider{chain: []backends.Backend{fullTrustBackend("fake", srv.URL)}}, exec, chat.Config{}, testAdmitter(t))
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
		eng := chat.NewEngine(pool, trustProvider{chain: []backends.Backend{fullTrustBackend("down", srvA.URL), fullTrustBackend("up", srvB.URL)}}, exec, chat.Config{}, testAdmitter(t))
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

	t.Run("LLMLogMetadataOnly", func(t *testing.T) {
		// One plain answer turn → one web-chat llmlog row. It must carry the
		// telemetry (pipeline/host/backend) but NO plaintext bodies: every chat
		// call holds the whole history, and context_llm_log is un-scoped +
		// outside the session CASCADE, so bodies there would be a shadow corpus
		// breaking the DELETE promise (§3.6/R9).
		llmSrv := &fakeLLM{responses: []fakeResp{{sse: sseContent("a plain answer")}}}
		srv := httptest.NewServer(llmSrv.handler())
		defer srv.Close()
		exec := chat.NewExecutor(pool, &fakeQuery{}, 8000)
		eng := chat.NewEngine(pool, trustProvider{chain: []backends.Backend{fullTrustBackend("metric-fake", srv.URL)}}, exec, chat.Config{}, testAdmitter(t))
		sess := newSession(t, pool, "private", []string{"private"})
		col := &collector{}
		if err := eng.RunTurn(ctx, &auth.AuthResult{ReadScopes: []string{"private"}}, sess, "hi there", backends.SensInternal, false, col); err != nil {
			t.Fatalf("RunTurn: %v", err)
		}

		// llmlog.Record is fire-and-forget — poll for this session's row. The
		// bodies are stored as raw strings (llmlog.insert), so metadata-only
		// shows up as EMPTY ("") rather than NULL — either way no plaintext.
		var pipeline, host, backendName, reqSys, reqUser, respContent string
		deadline := time.Now().Add(3 * time.Second)
		for {
			err := pool.QueryRow(ctx, `
				SELECT pipeline, host, COALESCE(backend_name,''),
				       COALESCE(request_system,''), COALESCE(request_user,''), COALESCE(response_content,'')
				FROM context_llm_log
				WHERE pipeline = 'web-chat' AND metadata->>'session_id' = $1
				ORDER BY created_at DESC LIMIT 1`, sess.ID).
				Scan(&pipeline, &host, &backendName, &reqSys, &reqUser, &respContent)
			if err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("no web-chat llmlog row for session %s after 3s: %v", sess.ID, err)
			}
			time.Sleep(50 * time.Millisecond)
		}
		if host == "" || backendName != "metric-fake" {
			t.Fatalf("telemetry missing: host=%q backend_name=%q", host, backendName)
		}
		if reqSys != "" || reqUser != "" || respContent != "" {
			t.Fatalf("web-chat llmlog must be metadata-only (§3.6/R9): request_system=%q request_user=%q response_content=%q", reqSys, reqUser, respContent)
		}
	})

	t.Run("ErrorEventLaundersBackendURL", func(t *testing.T) {
		// A dead backend (dial error) ends the turn with an error event that
		// carries only the class code + backend NAME — never the raw error, which
		// embeds the backend URL (interna topology disclosure to friend tenants,
		// §3.5). The whole event payload must not contain the host:port.
		deadHost := "http://127.0.0.1:1"
		exec := chat.NewExecutor(pool, &fakeQuery{}, 8000)
		eng := chat.NewEngine(pool, trustProvider{chain: []backends.Backend{fullTrustBackend("deadbackend", deadHost)}}, exec, chat.Config{}, testAdmitter(t))
		sess := newSession(t, pool, "private", []string{"private"})
		col := &collector{}
		if err := eng.RunTurn(ctx, &auth.AuthResult{ReadScopes: []string{"private"}}, sess, "hi", backends.SensInternal, false, col); err != nil {
			t.Fatalf("RunTurn: %v", err)
		}
		errEvt := col.last("error")
		if errEvt == nil {
			t.Fatal("expected an error event from the dead backend")
		}
		if errEvt["code"] == nil || errEvt["code"] == "" {
			t.Fatalf("error event missing class code: %v", errEvt)
		}
		if errEvt["backend"] != "deadbackend" {
			t.Fatalf("error event backend = %v; want the NAME deadbackend", errEvt["backend"])
		}
		raw, _ := json.Marshal(errEvt)
		if strings.Contains(string(raw), "127.0.0.1") {
			t.Fatalf("error event leaked the backend URL (topology disclosure): %s", raw)
		}
	})

	t.Run("TenantSuspendCutsRunningSession", func(t *testing.T) {
		// E6 / R-LEAK6 (T05c): sess.ReadScopes is frozen at session start, so a
		// tenant suspended MID-session would keep running tools under that snapshot
		// unless the engine re-checks the OWNER's tenant status each turn — the
		// auth-time ctx_auth gate (060) never re-fires for an already-open session.
		// Map a dedicated scope to a fresh tenant, run a turn while active, suspend
		// the tenant, then run AGAIN on the same open session: the second turn must
		// be cut before any work (backend never hit, nothing persisted), proving the
		// recheck is per-turn, not session-start only ("suspended = fully silent").
		const tenantID = "a5c0a5c0-a5c0-a5c0-a5c0-a5c0a5c0a5c0"
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_tenants (id, slug, display_name, status)
			 VALUES ($1, 'suspendco', 'Suspend Co', 'active') ON CONFLICT (slug) DO NOTHING`, tenantID); err != nil {
			t.Fatalf("seed tenant: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_tenant_scopes (scope, tenant_id) VALUES ('suspendscope', $1)
			 ON CONFLICT (scope) DO NOTHING`, tenantID); err != nil {
			t.Fatalf("map scope to tenant: %v", err)
		}

		llmSrv := &fakeLLM{responses: []fakeResp{
			{sse: sseContent("first turn while active")},
			{sse: sseContent("second turn must never run")},
		}}
		srv := httptest.NewServer(llmSrv.handler())
		defer srv.Close()
		exec := chat.NewExecutor(pool, &fakeQuery{}, 8000)
		eng := chat.NewEngine(pool, trustProvider{chain: []backends.Backend{fullTrustBackend("fake", srv.URL)}}, exec, chat.Config{}, testAdmitter(t))
		sess := newSession(t, pool, "suspendscope", []string{"suspendscope"})
		ar := &auth.AuthResult{ReadScopes: []string{"suspendscope"}}

		// Turn 1: owner tenant active → proceeds, hits the backend once.
		col1 := &collector{}
		if err := eng.RunTurn(ctx, ar, sess, "hello", backends.SensInternal, false, col1); err != nil {
			t.Fatalf("turn 1 RunTurn: %v", err)
		}
		if llmSrv.hitCount() != 1 {
			t.Fatalf("turn 1 llm hits = %d; want 1 (active owner proceeds)", llmSrv.hitCount())
		}
		if col1.last("done") == nil {
			t.Fatalf("turn 1 produced no done event; an active owner must answer normally")
		}

		// Suspend the OWNER tenant mid-session.
		if _, err := pool.Exec(ctx, `UPDATE context_tenants SET status = 'suspended' WHERE id = $1`, tenantID); err != nil {
			t.Fatalf("suspend owner tenant: %v", err)
		}

		// Turn 2 on the SAME open session: cut before any work.
		col2 := &collector{}
		if err := eng.RunTurn(ctx, ar, sess, "are you still there?", backends.SensInternal, false, col2); err != nil {
			t.Fatalf("turn 2 RunTurn: %v", err)
		}
		if llmSrv.hitCount() != 1 {
			t.Fatalf("turn 2 llm hits total = %d; want still 1 — a suspended owner's turn must never reach a backend (R-LEAK6)", llmSrv.hitCount())
		}
		if e := col2.last("error"); e == nil || e["code"] != "tenant_suspended" {
			t.Fatalf("turn 2 error event = %v; want code tenant_suspended", e)
		}
		if d := col2.last("done"); d != nil {
			t.Fatalf("turn 2 produced a done event %v; a cut turn must not complete", d)
		}
		// The cut turn appends no user message: only turn 1's user+assistant remain.
		msgs, _ := store.ListMessages(ctx, pool, sess.ID, 0, 0)
		if len(msgs) != 2 {
			t.Fatalf("messages after the cut turn = %d; want 2 (turn 1 only)", len(msgs))
		}

		// offboarding is the OTHER non-active status (mid-deletion). The == "active"
		// allowlist must cut it too — guards against a future != "suspended" denylist
		// regression that would leak an offboarding tenant (addendum §6.4: "suspended
		// UND offboarding").
		if _, err := pool.Exec(ctx, `UPDATE context_tenants SET status = 'offboarding' WHERE id = $1`, tenantID); err != nil {
			t.Fatalf("offboard owner tenant: %v", err)
		}
		col3 := &collector{}
		if err := eng.RunTurn(ctx, ar, sess, "still there?", backends.SensInternal, false, col3); err != nil {
			t.Fatalf("turn 3 RunTurn: %v", err)
		}
		if llmSrv.hitCount() != 1 {
			t.Fatalf("turn 3 llm hits total = %d; want still 1 — an offboarding owner is also cut", llmSrv.hitCount())
		}
		if e := col3.last("error"); e == nil || e["code"] != "tenant_suspended" {
			t.Fatalf("turn 3 error event = %v; want code tenant_suspended (offboarding ⇒ non-active ⇒ cut)", e)
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
