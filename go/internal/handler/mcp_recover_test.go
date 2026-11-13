package handler

// #194 — a panic in an MCP tool handler must not end the process.
//
// go-sdk runs every incoming request on a goroutine of its own
// (internal/jsonrpc2/conn.go, handleAsync), so the house Recovery on the HTTP
// goroutine never sees a panic there, and an unrecovered panic on any
// goroutine ends the whole process. A probe of that therefore cannot run in
// the test binary itself: a regression would take every other test of the
// package down with it. The probes below re-exec this test binary into
// TestMCPPanicHelperProcess (the pattern of cmd/ctxd's command-line probes),
// let the child drive a real /mcp handler over HTTP and report what it saw as
// one JSON line; the parent judges. A child that dies before it reports is the
// failure the issue describes, and the parent prints its stderr.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"runtime/pprof"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// mcpPanicHelperEnv routes TestMCPPanicHelperProcess into a scenario.
	mcpPanicHelperEnv = "CTX_TEST_MCP_PANIC_HELPER"
	// mcpPanicReportPrefix marks the child's one report line on stdout.
	mcpPanicReportPrefix = "MCP-PANIC-REPORT "
	// mcpPanicNeedle is in every panic value the probes raise. It must reach
	// the server log and must never reach the client.
	mcpPanicNeedle = "i194-panic-needle"
	// mcpPanicCalls is how many panicking calls a scenario makes in a row —
	// enough that a per-panic leak of one goroutine or one slot cannot hide in
	// the noise of the baseline.
	mcpPanicCalls = 20
)

// mcpPanicCall is one tool call as the client saw it.
type mcpPanicCall struct {
	Tool    string `json:"tool"`
	Err     string `json:"err,omitempty"` // transport or JSON-RPC error
	IsError bool   `json:"is_error"`
	Text    string `json:"text"`
	Code    string `json:"code"` // structuredContent.code
}

// mcpPanicReport is what a child saw. Fields a scenario does not measure stay
// zero.
type mcpPanicReport struct {
	Scenario             string         `json:"scenario"`
	Calls                []mcpPanicCall `json:"calls"`
	FollowUp             mcpPanicCall   `json:"follow_up"`
	GoroutinesBaseline   int            `json:"goroutines_baseline"`
	GoroutinesAfter      int            `json:"goroutines_after"`
	GoroutineDump        string         `json:"goroutine_dump,omitempty"`
	SlotsArrived         int64          `json:"slots_arrived"`
	SlotsActive          int64          `json:"slots_active"`
	DelegateCtxs         int            `json:"delegate_ctxs"`
	DelegateCtxsCanceled int            `json:"delegate_ctxs_canceled"`
	Tools                []string       `json:"tools,omitempty"`
	PoolAcquired         int32          `json:"pool_acquired"`
	PoolTotal            int32          `json:"pool_total"`
}

// mcpPanicScenarios are the child bodies by name. The integration suite adds
// its own in an init of its file.
var mcpPanicScenarios = map[string]func(*testing.T) mcpPanicReport{
	"query":      mcpPanicQueryScenario,
	"keepalive":  mcpPanicKeepaliveScenario,
	"every-tool": mcpPanicEveryToolScenario,
}

// TestMCPPanicHelperProcess is not a test: it is the child body of the #194
// probes, skipped unless a probe below spawned it.
func TestMCPPanicHelperProcess(t *testing.T) {
	scenario := os.Getenv(mcpPanicHelperEnv)
	if scenario == "" {
		t.Skip("helper process body — only meaningful when spawned by the #194 panic probes")
	}
	// The parent reads the server log from stderr, one JSON object per line.
	slogToStderrJSON()
	run, ok := mcpPanicScenarios[scenario]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown scenario %q\n", scenario)
		os.Exit(9)
	}
	rep := run(t)
	rep.Scenario = scenario
	line, err := json.Marshal(rep)
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshal report: %v\n", err)
		os.Exit(9)
	}
	fmt.Println(mcpPanicReportPrefix + string(line))
}

// panickingQueryDelegate stands in for /api/query: it panics on the question
// "panic", panics after slow on "slow-panic" (by then the keepalive has
// committed the response), and answers otherwise. It keeps every request
// context so the probe can see they were all released.
type panickingQueryDelegate struct {
	slow time.Duration
	mu   sync.Mutex
	ctxs []context.Context
}

func (d *panickingQueryDelegate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Query string `json:"query"`
	}
	_ = json.NewDecoder(r.Body).Decode(&p)
	d.mu.Lock()
	d.ctxs = append(d.ctxs, r.Context())
	d.mu.Unlock()
	switch p.Query {
	case "panic":
		panic(mcpPanicNeedle + " in the query pipeline")
	case "slow-panic":
		time.Sleep(d.slow)
		panic(mcpPanicNeedle + " after the keepalive committed the response")
	}
	_, _ = w.Write([]byte(`{"answer":"still-serving","confidence":"high","sources":[]}`))
}

// countingNotifier is the admission herald WithScheduler wraps the query
// handler in: a slot is held from arrival until the returned func runs.
type countingNotifier struct{ arrived, active atomic.Int64 }

func (n *countingNotifier) InteractiveArrived() func() {
	n.arrived.Add(1)
	n.active.Add(1)
	var once sync.Once
	return func() { once.Do(func() { n.active.Add(-1) }) }
}

// mcpPanicQueryScenario drives the production /mcp handler behind the house
// chain (RequestID, Logger, Recovery — main.go's order — and the auth
// context) with a query delegate that panics, the delegate wrapped in the
// admission slot exactly as server.go wires it.
func mcpPanicQueryScenario(t *testing.T) mcpPanicReport {
	shrinkHeartbeat(t, 20*time.Millisecond, 500*time.Millisecond)
	delegate := &panickingQueryDelegate{slow: 150 * time.Millisecond}
	notifier := &countingNotifier{}
	h := NewMCPHandler(MCPConfig{QueryHandler: WithScheduler(notifier, delegate.ServeHTTP)})
	srv := httptest.NewServer(RequestID(Logger(Recovery(withTestAuth(h)))))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	session := mcpPanicConnect(t, ctx, srv.URL)
	defer func() { _ = session.Close() }()

	var rep mcpPanicReport
	// Warm-up: the connection, the client's and the server's per-connection
	// goroutines exist before the baseline is taken.
	if warm := mcpPanicCallTool(ctx, session, "query", map[string]any{"question": "warm-up"}); warm.IsError || warm.Err != "" {
		t.Fatalf("warm-up call failed: %+v", warm)
	}
	time.Sleep(50 * time.Millisecond)
	rep.GoroutinesBaseline = runtime.NumGoroutine()

	for range mcpPanicCalls {
		rep.Calls = append(rep.Calls, mcpPanicCallTool(ctx, session, "query", map[string]any{"question": "panic"}))
	}
	rep.Calls = append(rep.Calls, mcpPanicCallTool(ctx, session, "query", map[string]any{"question": "slow-panic"}))
	// The same client session, after the panics.
	rep.FollowUp = mcpPanicCallTool(ctx, session, "query", map[string]any{"question": "after"})

	rep.GoroutinesAfter = settleGoroutines(rep.GoroutinesBaseline, 5*time.Second)
	if rep.GoroutinesAfter > rep.GoroutinesBaseline {
		var buf bytes.Buffer
		_ = pprof.Lookup("goroutine").WriteTo(&buf, 1)
		rep.GoroutineDump = buf.String()
	}
	rep.SlotsArrived, rep.SlotsActive = notifier.arrived.Load(), notifier.active.Load()
	delegate.mu.Lock()
	ctxs := append([]context.Context(nil), delegate.ctxs...)
	delegate.mu.Unlock()
	rep.DelegateCtxs = len(ctxs)
	deadline := time.Now().Add(2 * time.Second)
	for {
		rep.DelegateCtxsCanceled = 0
		for _, c := range ctxs {
			if c.Err() != nil {
				rep.DelegateCtxsCanceled++
			}
		}
		if rep.DelegateCtxsCanceled == len(ctxs) || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return rep
}

// panickingWriter is a ResponseWriter whose connection-facing methods panic —
// the one thing the keepalive ticker touches on its own goroutine.
type panickingWriter struct{ hdr http.Header }

func (p *panickingWriter) Header() http.Header { return p.hdr }
func (p *panickingWriter) WriteHeader(int) {
	panic(mcpPanicNeedle + " in the response writer")
}
func (p *panickingWriter) Write([]byte) (int, error) {
	panic(mcpPanicNeedle + " in the response writer")
}

// mcpPanicKeepaliveScenario panics inside the /mcp keepalive's ticker
// goroutine (mcp_keepalive.go, run → tick): an armed call that outlives the
// first tick, over a writer that panics on the tick's write.
func mcpPanicKeepaliveScenario(t *testing.T) mcpPanicReport {
	shrinkHeartbeat(t, 10*time.Millisecond, 500*time.Millisecond)
	armed := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		stop := armMCPKeepalive(r.Context())
		defer stop()
		time.Sleep(60 * time.Millisecond) // several intervals: the ticker fires
	})
	var rep mcpPanicReport
	rep.GoroutinesBaseline = runtime.NumGoroutine()
	for range 5 {
		withMCPKeepalive(armed).ServeHTTP(&panickingWriter{hdr: http.Header{}},
			httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}")))
	}
	rep.GoroutinesAfter = settleGoroutines(rep.GoroutinesBaseline, 5*time.Second)
	return rep
}

// mcpPanicEveryToolScenario holds the guard to the server, not to a handler:
// on the production server (newMCPServer) every registered tool is replaced
// by one that panics under the same name, and one tool the production code
// never registered is added the way every tool is (mcp.AddTool). Each call
// must come back as the internal tool error. A guard wired per handler, or
// per tool name, fails here for the first tool it does not know.
func mcpPanicEveryToolScenario(t *testing.T) mcpPanicReport {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	server := newMCPServer(MCPConfig{})
	serverT, clientT := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer func() { _ = ss.Close() }()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "i194-probe", Version: "0"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer func() { _ = cs.Close() }()

	var rep mcpPanicReport
	for tool, err := range cs.Tools(ctx, nil) {
		if err != nil {
			t.Fatalf("tools/list: %v", err)
		}
		rep.Tools = append(rep.Tools, tool.Name)
	}
	for _, name := range rep.Tools {
		server.AddTool(&mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}},
			func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				panic(mcpPanicNeedle + " in tool " + name)
			})
	}
	const newTool = "i194_tool_registered_later"
	mcp.AddTool(server, &mcp.Tool{Name: newTool}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
		panic(mcpPanicNeedle + " in tool " + newTool)
	})
	for _, name := range append(append([]string(nil), rep.Tools...), newTool) {
		rep.Calls = append(rep.Calls, mcpPanicCallTool(ctx, cs, name, map[string]any{}))
	}
	return rep
}

func mcpPanicConnect(t *testing.T, ctx context.Context, url string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "i194-probe", Version: "0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             url,
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	return session
}

func mcpPanicCallTool(ctx context.Context, session *mcp.ClientSession, tool string, args map[string]any) mcpPanicCall {
	call := mcpPanicCall{Tool: tool}
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		call.Err = err.Error()
		return call
	}
	call.IsError, call.Text = res.IsError, mcpTextOf(res)
	if res.StructuredContent != nil {
		raw, _ := json.Marshal(res.StructuredContent)
		var env mcpErrorEnvelope
		_ = json.Unmarshal(raw, &env)
		call.Code = env.Code
	}
	return call
}

// settleGoroutines waits until the goroutine count is at most limit or d has
// passed, and returns the last count.
func settleGoroutines(limit int, d time.Duration) int {
	deadline := time.Now().Add(d)
	for {
		n := runtime.NumGoroutine()
		if n <= limit || time.Now().After(deadline) {
			return n
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// mcpPanicChild is a finished child run.
type mcpPanicChild struct {
	code   int
	stdout string
	stderr string
	report *mcpPanicReport
	logs   []map[string]any // the JSON log lines on stderr
}

// runMCPPanicChild re-execs this test binary into one scenario and waits for
// it. A child that has not reported is a fatal failure: it is the process
// ending the issue describes (or a harness fault — the output says which).
func runMCPPanicChild(t *testing.T, scenario string, extraEnv ...string) mcpPanicChild {
	t.Helper()
	budget := 90 * time.Second
	if td, ok := t.Deadline(); ok {
		if margin := time.Until(td) - 10*time.Second; margin < budget {
			budget = max(margin, 10*time.Second)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], //nolint:gosec // re-exec of the own test binary
		"-test.run=^TestMCPPanicHelperProcess$", "-test.count=1", fmt.Sprintf("-test.timeout=%s", budget))
	cmd.Env = append(append(os.Environ(), mcpPanicHelperEnv+"="+scenario), extraEnv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	child := mcpPanicChild{stdout: stdout.String(), stderr: stderr.String()}
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("running the %s child: %v", scenario, err)
		}
		child.code = exitErr.ExitCode()
	}
	for _, line := range strings.Split(child.stdout, "\n") {
		if rest, ok := strings.CutPrefix(line, mcpPanicReportPrefix); ok {
			child.report = &mcpPanicReport{}
			if err := json.Unmarshal([]byte(rest), child.report); err != nil {
				t.Fatalf("decode the %s child's report: %v\n%s", scenario, err, rest)
			}
		}
	}
	sc := bufio.NewScanner(strings.NewReader(child.stderr))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var rec map[string]any
		if json.Unmarshal(sc.Bytes(), &rec) == nil {
			child.logs = append(child.logs, rec)
		}
	}
	if child.report == nil || child.code != 0 {
		t.Fatalf("the %s child ended with exit code %d before or after its report — the process did not survive the panic\nstdout:\n%.2000s\nstderr (tail):\n%s",
			scenario, child.code, child.stdout, tail(child.stderr, 6000))
	}
	return child
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// panicLogs returns the log records with the given message.
func (c mcpPanicChild) panicLogs(msg string) []map[string]any {
	var out []map[string]any
	for _, rec := range c.logs {
		if rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

// assertInternalToolError holds one call's answer to the house form of a
// server-side fault on MCP: a tool result with isError, the opaque text and
// the code "internal" — never the panic value, a stack frame or a path.
func assertInternalToolError(t *testing.T, i int, c mcpPanicCall) {
	t.Helper()
	if c.Err != "" {
		t.Errorf("call %d (%s): transport/protocol error %q, want a tool result with isError", i, c.Tool, c.Err)
		return
	}
	if !c.IsError || c.Text != "Internal server error" || c.Code != "internal" {
		t.Errorf("call %d (%s): isError=%v text=%q code=%q, want isError=true text=%q code=%q",
			i, c.Tool, c.IsError, c.Text, c.Code, "Internal server error", "internal")
	}
	for _, leak := range []string{mcpPanicNeedle, "goroutine", ".go:", "panic"} {
		if strings.Contains(c.Text, leak) {
			t.Errorf("call %d (%s): the client answer carries %q: %q", i, c.Tool, leak, c.Text)
		}
	}
}

// slogToStderrJSON makes the child's default logger write JSON lines to stderr.
func slogToStderrJSON() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
}

// TestMCPToolPanicIsAToolErrorAndTheProcessLives is the #194 acceptance probe:
// a query whose pipeline panics — twenty times in a row, once after the
// keepalive has already committed the response — is answered as a tool error,
// the stack lands in the server log, nothing the request held stays held, and
// the same client session keeps working.
func TestMCPToolPanicIsAToolErrorAndTheProcessLives(t *testing.T) {
	child := runMCPPanicChild(t, "query")
	rep := child.report

	if len(rep.Calls) != mcpPanicCalls+1 {
		t.Fatalf("child made %d panicking calls, want %d", len(rep.Calls), mcpPanicCalls+1)
	}
	for i, c := range rep.Calls {
		assertInternalToolError(t, i, c)
	}
	if f := rep.FollowUp; f.Err != "" || f.IsError || !strings.Contains(f.Text, "still-serving") {
		t.Errorf("follow-up call in the same session: %+v, want the answer still-serving", f)
	}

	// Released: goroutines (the SDK's request goroutine, the keepalive
	// ticker), the admission slot, the request context.
	t.Logf("%d panicking calls: goroutines baseline %d after %d; slots arrived %d held %d; request contexts canceled %d/%d",
		len(rep.Calls), rep.GoroutinesBaseline, rep.GoroutinesAfter, rep.SlotsArrived, rep.SlotsActive,
		rep.DelegateCtxsCanceled, rep.DelegateCtxs)
	if rep.GoroutinesAfter > rep.GoroutinesBaseline {
		t.Errorf("goroutines after %d panics: %d, baseline %d — something the request started outlived it\n%s",
			len(rep.Calls), rep.GoroutinesAfter, rep.GoroutinesBaseline, tail(rep.GoroutineDump, 8000))
	}
	// warm-up + the panicking calls + the follow-up each took one slot.
	if want := int64(len(rep.Calls) + 2); rep.SlotsArrived != want || rep.SlotsActive != 0 {
		t.Errorf("admission slots: arrived %d (want %d), still held %d (want 0)", rep.SlotsArrived, want, rep.SlotsActive)
	}
	if rep.DelegateCtxsCanceled != rep.DelegateCtxs || rep.DelegateCtxs != len(rep.Calls)+2 {
		t.Errorf("request contexts: %d of %d canceled after the calls (want all of %d)",
			rep.DelegateCtxsCanceled, rep.DelegateCtxs, len(rep.Calls)+2)
	}

	// The server log: one ERROR per panic, with the panic value, the stack of
	// the panicking frame, the MCP method, the tool and the request id.
	logs := child.panicLogs("mcp: panic recovered")
	if len(logs) != len(rep.Calls) {
		t.Fatalf("%d \"mcp: panic recovered\" log lines, want %d (one per panic)\nstderr (tail):\n%s",
			len(logs), len(rep.Calls), tail(child.stderr, 4000))
	}
	for i, rec := range logs {
		errText, _ := rec["error"].(string)
		stack, _ := rec["stack"].(string)
		reqID, _ := rec["request_id"].(string)
		if rec["level"] != "ERROR" || !strings.Contains(errText, mcpPanicNeedle) ||
			!strings.Contains(stack, "panickingQueryDelegate") ||
			rec["mcp_method"] != "tools/call" || rec["tool"] != "query" || reqID == "" {
			t.Errorf("log line %d lacks the expected fields (level ERROR, error with the panic value, stack through the panicking frame, mcp_method tools/call, tool query, request_id): %v", i, rec)
		}
	}
}

// TestMCPKeepalivePanicDoesNotEndTheProcess: the /mcp keepalive ticker (#189)
// is a goroutine of ctx's own next to the SDK's; a panic on it is logged with
// its stack and ends the ticker, never the process.
func TestMCPKeepalivePanicDoesNotEndTheProcess(t *testing.T) {
	child := runMCPPanicChild(t, "keepalive")
	rep := child.report
	logs := child.panicLogs("mcp keepalive: panic recovered")
	if len(logs) != 5 {
		t.Fatalf("%d \"mcp keepalive: panic recovered\" log lines, want 5 (one per armed request)\nstderr (tail):\n%s",
			len(logs), tail(child.stderr, 4000))
	}
	for i, rec := range logs {
		errText, _ := rec["error"].(string)
		stack, _ := rec["stack"].(string)
		if rec["level"] != "ERROR" || !strings.Contains(errText, mcpPanicNeedle) || !strings.Contains(stack, "panickingWriter") {
			t.Errorf("log line %d lacks level ERROR, the panic value or the stack through the panicking frame: %v", i, rec)
		}
	}
	if rep.GoroutinesAfter > rep.GoroutinesBaseline {
		t.Errorf("goroutines after the keepalive panics: %d, baseline %d — a ticker outlived its request", rep.GoroutinesAfter, rep.GoroutinesBaseline)
	}
}

// TestMCPEveryRegisteredToolIsBehindTheRecovery: the guard sits on the server,
// so every tool the production server registers — and one registered later —
// answers a panic as the internal tool error, with one log line naming it.
func TestMCPEveryRegisteredToolIsBehindTheRecovery(t *testing.T) {
	child := runMCPPanicChild(t, "every-tool")
	rep := child.report
	if !slices.Contains(rep.Tools, "query") || len(rep.Calls) != len(rep.Tools)+1 {
		t.Fatalf("tools listed %v, calls %d — the probe did not walk the production tool set", rep.Tools, len(rep.Calls))
	}
	t.Logf("%d production tools + 1 registered later, each panicking: %v", len(rep.Tools), rep.Tools)
	logged := map[string]int{}
	for _, rec := range child.panicLogs("mcp: panic recovered") {
		tool, _ := rec["tool"].(string)
		logged[tool]++
	}
	for i, c := range rep.Calls {
		assertInternalToolError(t, i, c)
		if logged[c.Tool] != 1 {
			t.Errorf("tool %s: %d \"mcp: panic recovered\" log lines, want 1", c.Tool, logged[c.Tool])
		}
	}
}

// TestMCPRecoverAnswersByMethodKind: the guard's answer follows the protocol of
// the method it wraps — a tool result for tools/call, the JSON-RPC internal
// error for every other request and notification — and a method that does not
// panic passes through untouched.
func TestMCPRecoverAnswersByMethodKind(t *testing.T) {
	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, nil)))
	defer slog.SetDefault(prev)

	panicking := func(context.Context, string, mcp.Request) (mcp.Result, error) {
		panic(mcpPanicNeedle)
	}
	// guarded runs the guard in a frame of its own, so a guard that lets the
	// panic through fails this test instead of the test binary.
	guarded := func(next mcp.MethodHandler, method string, req mcp.Request) (res mcp.Result, escaped any, err error) {
		defer func() { escaped = recover() }()
		res, err = mcpRecover(next)(context.Background(), method, req)
		return res, nil, err
	}

	res, escaped, err := guarded(panicking, "tools/call", &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "probe"}})
	if escaped != nil || err != nil {
		t.Fatalf("tools/call: escaped=%v err=%v, want a tool result", escaped, err)
	}
	ctr, ok := res.(*mcp.CallToolResult)
	if !ok || !ctr.IsError || mcpTextOf(ctr) != mcpInternalErrorMsg || ctr.StructuredContent != (mcpErrorEnvelope{Code: "internal"}) {
		t.Errorf("tools/call answer = %#v, want isError with %q and code internal", res, mcpInternalErrorMsg)
	}

	for _, tc := range []struct {
		method string
		req    mcp.Request
	}{
		{"resources/read", &mcp.ReadResourceRequest{Params: &mcp.ReadResourceParams{URI: "ctx://probe"}}},
		{"prompts/get", &mcp.GetPromptRequest{Params: &mcp.GetPromptParams{Name: "probe"}}},
		{"notifications/initialized", &mcp.InitializedRequest{Params: &mcp.InitializedParams{}}},
	} {
		res, escaped, err := guarded(panicking, tc.method, tc.req)
		var wire *jsonrpc.Error
		if escaped != nil || res != nil || !errors.As(err, &wire) ||
			wire.Code != jsonrpc.CodeInternalError || wire.Message != mcpInternalErrorMsg {
			t.Errorf("%s: escaped=%v res=%#v err=%v, want the JSON-RPC internal error %q", tc.method, escaped, res, err, mcpInternalErrorMsg)
		}
	}

	// Not a panic: result and error pass through as they are.
	want := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "fine"}}}
	wantErr := errors.New("handler error")
	res, escaped, err = guarded(func(context.Context, string, mcp.Request) (mcp.Result, error) { return want, wantErr },
		"tools/call", &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "probe"}})
	if escaped != nil || res != mcp.Result(want) || err != wantErr { //nolint:errorlint // identity is the point
		t.Errorf("pass-through: res=%#v err=%v escaped=%v, want the handler's own values", res, err, escaped)
	}

	if n := strings.Count(logBuf.String(), `"msg":"mcp: panic recovered"`); n != 4 {
		t.Errorf("%d panic log lines, want 4 (one per panicking call)\n%s", n, logBuf.String())
	}
}
