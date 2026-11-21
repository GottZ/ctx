package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
)

// mcpServeFixtureKey is a key no answer, log line or frame may ever carry.
const mcpServeFixtureKey = "c0ffee409c0ffee409c0ffee409c0ffee409c0ffee409c0ffee409c0ffee409"

// closedAddr is a loopback address nothing listens on.
func closedAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// TestMCPServeStdoutCarriesProtocolFramesOnly runs `ctx mcp serve` through the
// cobra tree on real stdin/stdout pipes against a server that cannot be
// reached: every line on stdout is a JSON-RPC frame, the failures answer with
// their cause, the process keeps serving after a failed call, and it ends
// cleanly when stdin closes. The key is in no frame and no stderr line.
func TestMCPServeStdoutCarriesProtocolFramesOnly(t *testing.T) {
	addr := closedAddr(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CTX_BASE_URL", "http://"+addr)
	t.Setenv("CTX_KEY", mcpServeFixtureKey)

	inR, inW, _ := os.Pipe()
	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	oldIn, oldOut, oldErr := os.Stdin, os.Stdout, os.Stderr
	os.Stdin, os.Stdout, os.Stderr = inR, outW, errW
	t.Cleanup(func() { os.Stdin, os.Stdout, os.Stderr = oldIn, oldOut, oldErr })

	root := &cobra.Command{Use: "ctx", SilenceErrors: true, SilenceUsage: true}
	RegisterCommands(root)
	root.SetArgs([]string{"mcp", "serve"})
	done := make(chan error, 1)
	go func() {
		err := root.Execute()
		_ = outW.Close()
		_ = errW.Close()
		done <- err
	}()
	stderr := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(errR)
		stderr <- string(b)
	}()

	frames := make(chan map[string]any, 16)
	badLines := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			var f map[string]any
			if err := json.Unmarshal(sc.Bytes(), &f); err != nil || f["jsonrpc"] != "2.0" {
				badLines <- sc.Text()
				continue
			}
			frames <- f
		}
		close(frames)
	}()

	send := func(v map[string]any) {
		v["jsonrpc"] = "2.0"
		b, _ := json.Marshal(v)
		if _, err := inW.Write(append(b, '\n')); err != nil {
			t.Fatalf("write frame: %v", err)
		}
	}
	answers := map[float64]map[string]any{}
	await := func(id float64) map[string]any {
		t.Helper()
		deadline := time.After(20 * time.Second)
		for answers[id] == nil {
			select {
			case f, ok := <-frames:
				if !ok {
					t.Fatalf("stdout closed before the answer to request %v (command returned: %v)", id, <-done)
				}
				if fid, ok := f["id"].(float64); ok {
					answers[fid] = f
				}
			case line := <-badLines:
				t.Fatalf("stdout carried a line that is not a JSON-RPC frame: %q", line)
			case <-deadline:
				t.Fatalf("no answer to request %v", id)
			}
		}
		return answers[id]
	}

	send(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{
		"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "test", "version": "0"},
	}})
	if res, _ := await(1)["result"].(map[string]any); res == nil {
		t.Fatalf("initialize did not succeed: %v", answers[1])
	}
	send(map[string]any{"method": "notifications/initialized"})

	wantCause := "cannot use the server at http://" + addr + "/mcp"
	send(map[string]any{"id": 2, "method": "tools/list"})
	listErr, _ := await(2)["error"].(map[string]any)
	if msg, _ := listErr["message"].(string); !strings.Contains(msg, wantCause) || listErr["code"] != float64(-32603) {
		t.Errorf("tools/list answered %v, want an internal error (-32603) naming %q", answers[2], wantCause)
	}

	for _, id := range []float64{3, 4} { // a second failed call: the process still serves
		send(map[string]any{"id": id, "method": "tools/call", "params": map[string]any{
			"name": "store", "arguments": map[string]any{"category": "c", "title": "t", "content": "x"},
		}})
		res, _ := await(id)["result"].(map[string]any)
		text := mcpResultText(res)
		if res["isError"] != true || !strings.Contains(text, wantCause) {
			t.Errorf("tools/call #%v answered %v, want isError naming %q", id, answers[id], wantCause)
		}
	}
	send(map[string]any{"id": 5, "method": "ping"})
	if _, ok := await(5)["result"]; !ok {
		t.Errorf("ping after failed calls answered %v", answers[5])
	}

	_ = inW.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("ctx mcp serve ended with %v after stdin closed, want nil", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("ctx mcp serve did not end after stdin closed")
	}
	for f := range frames {
		_ = f
	}
	select {
	case line := <-badLines:
		t.Errorf("stdout carried a line that is not a JSON-RPC frame: %q", line)
	default:
	}
	for id, f := range answers {
		if b, _ := json.Marshal(f); strings.Contains(string(b), mcpServeFixtureKey) {
			t.Errorf("answer %v carries the key", id)
		}
	}
	if s := <-stderr; strings.Contains(s, mcpServeFixtureKey) {
		t.Errorf("stderr carries the key: %q", s)
	}
}

func mcpResultText(res map[string]any) string {
	content, _ := res["content"].([]any)
	var sb strings.Builder
	for _, c := range content {
		if m, ok := c.(map[string]any); ok {
			s, _ := m["text"].(string)
			sb.WriteString(s)
		}
	}
	return sb.String()
}

// connectProxy runs p's server on one end of an in-memory pair and returns a
// client session on the other.
func connectProxy(t *testing.T, p *MCPProxy) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := p.Server().Connect(ctx, st, nil)
	if err != nil {
		t.Fatalf("proxy server connect: %v", err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() {
		_ = cs.Close()
		_ = ss.Close()
		_ = p.Close()
	})
	return cs
}

// TestMCPServeMissingConfigNamesTheCause: without a usable configuration the
// proxy starts, and tools/list and every call answer the configuration error.
func TestMCPServeMissingConfigNamesTheCause(t *testing.T) {
	cfgErr := errors.New("no config found. Create /nowhere/ctx/config with CTX_BASE_URL and CTX_KEY")
	p, err := NewMCPProxy(Config{}, cfgErr, nil)
	if err != nil {
		t.Fatal(err)
	}
	cs := connectProxy(t, p)
	ctx := context.Background()
	_, err = cs.ListTools(ctx, nil)
	if err == nil || !strings.Contains(err.Error(), cfgErr.Error()) {
		t.Errorf("tools/list error = %v, want it to name %q", err, cfgErr)
	}
	for range 2 {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "search", Arguments: map[string]any{"query": "x"}})
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		text := res.Content[0].(*mcp.TextContent).Text
		if !res.IsError || !strings.Contains(text, cfgErr.Error()) {
			t.Errorf("call answered isError=%v %q, want the configuration error", res.IsError, text)
		}
	}
}

const upstreamInstructions = "upstream instructions"

// upstream is a stateless SDK server behind a handler that requires the key
// header. Tools: echo, and slow, which answers once release is closed. The
// request after failWith was set gets that status without a JSON-RPC body,
// as a proxy in front of the server could send it.
type upstream struct {
	srv      *httptest.Server
	failWith atomic.Int32
	release  chan struct{}
	holding  atomic.Bool // set: every request waits for release (a server that does not answer)
}

func newUpstream(t *testing.T) *upstream {
	t.Helper()
	u := &upstream{release: make(chan struct{})}
	up := mcp.NewServer(&mcp.Implementation{Name: "upstream", Version: "0"}, &mcp.ServerOptions{Instructions: upstreamInstructions})
	type echoIn struct {
		Content string `json:"content"`
	}
	mcp.AddTool(up, &mcp.Tool{Name: "echo", Description: "echo the content"},
		func(_ context.Context, _ *mcp.CallToolRequest, in echoIn) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: in.Content}}}, nil, nil
		})
	mcp.AddTool(up, &mcp.Tool{Name: "slow", Description: "answer once released"},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			select {
			case <-u.release:
			case <-ctx.Done():
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "slow done"}}}, nil, nil
		})
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return up },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Context-Key") != mcpServeFixtureKey {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		if u.holding.Load() {
			select {
			case <-u.release:
			case <-r.Context().Done():
				return
			}
		}
		if status := u.failWith.Swap(0); status != 0 {
			http.Error(w, "refused by a proxy", int(status))
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		select {
		case <-u.release:
		default:
			close(u.release)
		}
		u.srv.Close()
	})
	return u
}

func (u *upstream) proxy(t *testing.T) (*MCPProxy, *mcp.ClientSession) {
	t.Helper()
	p, err := NewMCPProxy(Config{BaseURL: u.srv.URL, Key: mcpServeFixtureKey}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p, connectProxy(t, p)
}

func callEcho(t *testing.T, cs *mcp.ClientSession) (*mcp.CallToolResult, string) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"content": "hi"}})
	if err != nil {
		t.Fatalf("echo: %v", err)
	}
	return res, res.Content[0].(*mcp.TextContent).Text
}

// TestMCPServeMirrorsInstructions: the server's instructions reach the local
// client through initialize (the handshake of protocol versions up to
// 2025-11-25, which clients such as Claude Code speak) and through
// server/discover (2026-07-28, the SDK client's default).
func TestMCPServeMirrorsInstructions(t *testing.T) {
	u := newUpstream(t)
	for _, version := range []string{"2025-11-25", ""} {
		p, err := NewMCPProxy(Config{BaseURL: u.srv.URL, Key: mcpServeFixtureKey}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		st, ct := mcp.NewInMemoryTransports()
		ss, err := p.Server().Connect(context.Background(), st, nil)
		if err != nil {
			t.Fatal(err)
		}
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(context.Background(), ct,
			&mcp.ClientSessionOptions{ProtocolVersion: version})
		if err != nil {
			t.Fatalf("connect (protocol %q): %v", version, err)
		}
		if got := cs.InitializeResult().Instructions; got != upstreamInstructions {
			t.Errorf("protocol %q: instructions = %q, want the server's %q", version, got, upstreamInstructions)
		}
		_ = cs.Close()
		_ = ss.Close()
		_ = p.Close()
	}
}

// TestMCPServeReconnectsAfterABrokenSession: an answer the SDK client treats
// as fatal for its connection (a 403 without a JSON-RPC body; a 500 the SDK
// v1.8.0 takes as transient) is answered with the cause, and the next call
// opens a new session instead of failing for the rest of the process.
func TestMCPServeReconnectsAfterABrokenSession(t *testing.T) {
	u := newUpstream(t)
	_, cs := u.proxy(t)
	if res, text := callEcho(t, cs); res.IsError || text != "hi" {
		t.Fatalf("echo before the failure: %q", text)
	}
	u.failWith.Store(http.StatusForbidden)
	if res, text := callEcho(t, cs); !res.IsError || !strings.Contains(text, "cannot use the server at "+u.srv.URL+"/mcp") {
		t.Errorf("echo on a 403 answered isError=%v %q", res.IsError, text)
	}
	if res, text := callEcho(t, cs); res.IsError || text != "hi" {
		t.Errorf("echo after the failure answered %q, want the proxy to reconnect", text)
	}
}

// TestMCPServeTransientFailureDoesNotWaitForOtherCalls: a failure the SDK
// takes as transient (a 503) is answered at once while another call is still
// running, and that call ends normally: the session is not torn down under it.
func TestMCPServeTransientFailureDoesNotWaitForOtherCalls(t *testing.T) {
	u := newUpstream(t)
	_, cs := u.proxy(t)
	if res, _ := callEcho(t, cs); res.IsError {
		t.Fatal("echo before the failure failed")
	}
	slow := make(chan *mcp.CallToolResult, 1)
	go func() {
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "slow", Arguments: map[string]any{}})
		if err != nil {
			res = refusalResult(err.Error())
		}
		slow <- res
	}()
	time.Sleep(200 * time.Millisecond) // the slow call is in flight
	u.failWith.Store(http.StatusServiceUnavailable)
	start := time.Now()
	res, text := callEcho(t, cs)
	if !res.IsError || !strings.Contains(text, "cannot use the server") {
		t.Errorf("echo on a 503 answered isError=%v %q", res.IsError, text)
	}
	if waited := time.Since(start); waited > 2*time.Second {
		t.Errorf("the 503 answer took %v, held by the running call", waited)
	}
	close(u.release)
	if res := <-slow; res.IsError {
		t.Errorf("the running call ended with %q", res.Content[0].(*mcp.TextContent).Text)
	}
}

// TestMCPServeWaitingForAHangingConnectIsCancellable: while one request
// opens the session against a server that does not answer, another request
// gives up with its own context instead of queueing behind the first.
func TestMCPServeWaitingForAHangingConnectIsCancellable(t *testing.T) {
	u := newUpstream(t)
	u.holding.Store(true)
	p, err := NewMCPProxy(Config{BaseURL: u.srv.URL, Key: mcpServeFixtureKey}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	first, cancelFirst := context.WithCancel(context.Background())
	t.Cleanup(cancelFirst)
	go func() { _, _ = p.remote(first) }()
	time.Sleep(200 * time.Millisecond) // the first request holds the connect
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := p.remote(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("second request = %v, want its own deadline", err)
	}
	if waited := time.Since(start); waited > 2*time.Second {
		t.Errorf("second request waited %v behind the hanging connect", waited)
	}
}

// TestMCPServeKeyNeverLeavesTheOrigin: a redirect from /mcp to another
// origin is refused with its cause and the other host never sees a request,
// let alone the key; a redirect within the configured origin is followed.
func TestMCPServeKeyNeverLeavesTheOrigin(t *testing.T) {
	var foreignRequests, foreignKeys atomic.Int32
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignRequests.Add(1)
		if r.Header.Get("X-Context-Key") != "" {
			foreignKeys.Add(1)
		}
		http.Error(w, "not an MCP server", http.StatusNotFound)
	}))
	t.Cleanup(foreign.Close)
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, foreign.URL+"/mcp", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirecting.Close)

	p, err := NewMCPProxy(Config{BaseURL: redirecting.URL, Key: mcpServeFixtureKey}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	cs := connectProxy(t, p)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"content": "hi"}})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if !res.IsError || !strings.Contains(text, "redirect") || !strings.Contains(text, foreign.URL) || strings.Contains(text, mcpServeFixtureKey) {
		t.Errorf("call through a redirect to another origin answered isError=%v %q", res.IsError, text)
	}
	if n, k := foreignRequests.Load(), foreignKeys.Load(); n != 0 || k != 0 {
		t.Errorf("the other origin got %d requests, %d of them with the key", n, k)
	}

	// Within the origin a redirect is followed (a server moving /mcp).
	u := newUpstream(t)
	moved := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/mcp" {
			http.Redirect(w, r, "/moved", http.StatusPermanentRedirect)
			return
		}
		u.srv.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(moved.Close)
	p2, err := NewMCPProxy(Config{BaseURL: moved.URL, Key: mcpServeFixtureKey}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res, text := callEcho(t, connectProxy(t, p2)); res.IsError || text != "hi" {
		t.Errorf("call through a redirect within the origin answered %q", text)
	}
}

// TestMCPServeConnectHasADeadline: a server that accepts the connection and
// never answers ends the connect after the proxy's wait with the cause, the
// connect token is free again for the next request, and once the server
// answers the proxy reaches it.
func TestMCPServeConnectHasADeadline(t *testing.T) {
	u := newUpstream(t)
	u.holding.Store(true)
	p, err := NewMCPProxy(Config{BaseURL: u.srv.URL, Key: mcpServeFixtureKey}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.connectWait = 300 * time.Millisecond
	t.Cleanup(func() { _ = p.Close() })
	for i := range 2 { // the second request is not held by the first
		done := make(chan error, 1)
		go func() {
			_, err := p.remote(context.Background())
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(p.answer(context.Background(), nil, err).Error(), "within 300ms") {
				t.Errorf("connect #%d against a silent server: %v, want the wait named", i+1, err)
			}
		case <-time.After(2 * time.Second): // the wait, not the wait plus the SDK's cleanup
			t.Fatalf("connect #%d against a silent server did not end within its wait", i+1)
		}
	}
	u.holding.Store(false)
	if _, err := p.remote(context.Background()); err != nil {
		t.Errorf("connect once the server answers: %v", err)
	}
}

// TestMCPServeNullFileParametersAreAbsent: a client that sends optional
// fields as null keeps working with --allow-files, and the null is not sent.
func TestMCPServeNullFileParametersAreAbsent(t *testing.T) {
	roots := &mcpFileRoots{cwd: "/work"}
	args := json.RawMessage(`{"category":"c","title":"t","content":"x","content_file":null,"output_file":null}`)
	out, target, refusal, err := roots.prepareCall(context.Background(), nil, "store", args)
	if err != nil || refusal != "" || target != nil {
		t.Fatalf("prepareCall = target %v, refusal %q, err %v", target, refusal, err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(m, map[string]any{"category": "c", "title": "t", "content": "x"}) {
		t.Errorf("arguments sent = %v", m)
	}
	untouched := json.RawMessage(`{"content":"x"}`)
	if out, _, _, _ := roots.prepareCall(context.Background(), nil, "store", untouched); string(out) != string(untouched) {
		t.Errorf("arguments without file parameters changed: %s", out)
	}
}

func TestMCPServeAllowFilesMustBeDirectories(t *testing.T) {
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{file, filepath.Join(t.TempDir(), "missing"), "", " "} {
		if _, err := NewMCPProxy(Config{BaseURL: "http://127.0.0.1:1", Key: "k"}, nil, []string{dir}); err == nil {
			t.Errorf("--allow-files %s accepted, want an error", dir)
		}
	}
}

// TestMCPServeSchemaFileParams pins how the mirrored schemas gain the two
// parameters: content_file on a tool with a content string (and content no
// longer required), output_file on get, nothing on any other tool, and the
// remote's result left untouched.
func TestMCPServeSchemaFileParams(t *testing.T) {
	roots := &mcpFileRoots{roots: []mcpFileRoot{{dir: "/allowed"}}, cwd: "/work"}
	schema := func(js string) any {
		var v any
		if err := json.Unmarshal([]byte(js), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	store := schema(`{"type":"object","properties":{"title":{"type":"string"},"content":{"type":"string"}},"required":["title","content"],"additionalProperties":false}`)
	update := schema(`{"type":"object","properties":{"id":{"type":"string"},"content":{"type":["null","string"]}},"required":["id"]}`)
	get := schema(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`)
	search := schema(`{"type":"object","properties":{"query":{"type":"string"}}}`)
	blob := schema(`{"type":"object","properties":{"content":{"type":"object"}}}`)
	res := &mcp.ListToolsResult{Tools: []*mcp.Tool{
		{Name: "store", InputSchema: store}, {Name: "update", InputSchema: update},
		{Name: "get", InputSchema: get}, {Name: "search", InputSchema: search}, {Name: "blobish", InputSchema: blob},
	}}
	before, _ := json.Marshal(res)
	out := roots.withFileParams(res)
	if after, _ := json.Marshal(res); string(after) != string(before) {
		t.Fatalf("withFileParams changed the remote's result:\n%s\n%s", before, after)
	}
	props := func(tool *mcp.Tool) map[string]any {
		return tool.InputSchema.(map[string]any)["properties"].(map[string]any)
	}
	for i, tool := range out.Tools {
		_, hasCF := props(tool)[mcpContentFileParam]
		_, hasOF := props(tool)[mcpOutputFileParam]
		wantCF := tool.Name == "store" || tool.Name == "update"
		if hasCF != wantCF || hasOF != (tool.Name == "get") {
			t.Errorf("%s: content_file=%v output_file=%v", tool.Name, hasCF, hasOF)
		}
		if !wantCF && tool.Name != "get" && !reflect.DeepEqual(tool.InputSchema, res.Tools[i].InputSchema) {
			t.Errorf("%s: schema changed without a file parameter", tool.Name)
		}
	}
	if req := out.Tools[0].InputSchema.(map[string]any)["required"]; !reflect.DeepEqual(req, []any{"title"}) {
		t.Errorf("store required = %v, want [title]", req)
	}
	desc, _ := props(out.Tools[0])[mcpContentFileParam].(map[string]any)["description"].(string)
	if !strings.Contains(desc, "/allowed") || !strings.Contains(desc, "/work") {
		t.Errorf("content_file description names neither the directory nor the working directory: %q", desc)
	}
}
