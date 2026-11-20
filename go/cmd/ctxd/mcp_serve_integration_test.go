//go:build integration

// `ctx mcp serve` (#409) against a real server: the production router
// (NewRouter, through mcpBodyCapRouter) on a throwaway database behind an
// httptest listener. Every proxied answer is held against the same call sent
// straight to /mcp with an SDK client — the proxy mirrors the server, it does
// not re-implement it.
//
//	go test -tags=integration ./cmd/ctxd/ -run TestMCPServe -count=1 -v
package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/cli"
	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// keyHeader is the direct client's credential, the header the CLI sends.
type keyHeader struct{ key string }

func (k keyHeader) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header.Set("X-Context-Key", k.key)
	return http.DefaultTransport.RoundTrip(r)
}

// directSession is an SDK client straight on <url>/mcp: the reference.
func directSession(t *testing.T, url, key string) *mcp.ClientSession {
	t.Helper()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "direct", Version: "0"}, nil).Connect(context.Background(),
		&mcp.StreamableClientTransport{Endpoint: url + "/mcp", HTTPClient: &http.Client{Transport: keyHeader{key}}, DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatalf("direct connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// proxySession is an SDK client on a `ctx mcp serve` proxy for url and key.
func proxySession(t *testing.T, url, key string, allowFiles []string) *mcp.ClientSession {
	t.Helper()
	p, err := cli.NewMCPProxy(cli.Config{BaseURL: url, Key: key}, nil, allowFiles)
	if err != nil {
		t.Fatalf("proxy: %v", err)
	}
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := p.Server().Connect(ctx, st, nil)
	if err != nil {
		t.Fatalf("proxy server: %v", err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "via-proxy", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("proxy client: %v", err)
	}
	t.Cleanup(func() {
		_ = cs.Close()
		_ = ss.Close()
		_ = p.Close()
	})
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	return res
}

func resultText(res *mcp.CallToolResult) string {
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

// sameJSON reports whether a and b marshal to equal JSON values.
func sameJSON(t *testing.T, a, b any) bool {
	t.Helper()
	var va, vb any
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if err := json.Unmarshal(ja, &va); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(jb, &vb); err != nil {
		t.Fatal(err)
	}
	return reflect.DeepEqual(va, vb)
}

func listTools(t *testing.T, cs *mcp.ClientSession) []*mcp.Tool {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	return res.Tools
}

var storedID = regexp.MustCompile(`id: ([0-9a-f-]{36})`)

func blockCount(t *testing.T, pool *pgxpool.Pool, category string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM context_blocks WHERE category = $1`, category).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestMCPServeParity_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	router, pool, key := mcpBodyCapRouter(t)
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	direct := directSession(t, srv.URL, key)
	proxy := proxySession(t, srv.URL, key, nil)

	t.Run("instructions and tool list are the server's", func(t *testing.T) {
		want := direct.InitializeResult().Instructions
		if want == "" || proxy.InitializeResult().Instructions != want {
			t.Errorf("instructions via proxy = %q, want the server's %q", proxy.InitializeResult().Instructions, want)
		}
		got, ref := listTools(t, proxy), listTools(t, direct)
		if len(ref) == 0 || !sameJSON(t, got, ref) {
			t.Errorf("tools/list via proxy differs from /mcp:\nproxy  %d tools\ndirect %d tools", len(got), len(ref))
		}
	})

	t.Run("a write and a read have the effect and answer of /mcp", func(t *testing.T) {
		args := map[string]any{"category": "i409", "title": "round trip", "content": "written through the proxy", "sensitivity": "internal"}
		stored := call(t, proxy, "store", args)
		m := storedID.FindStringSubmatch(resultText(stored))
		if stored.IsError || m == nil {
			t.Fatalf("store via proxy: %q", resultText(stored))
		}
		id := m[1]
		// The same write straight to /mcp finds the block the proxy wrote.
		if again := resultText(call(t, direct, "store", args)); !strings.Contains(again, "No change needed") || !strings.Contains(again, id) {
			t.Errorf("the same store on /mcp answered %q, want the no-op naming %s", again, id)
		}
		for _, read := range []struct {
			tool string
			args map[string]any
		}{
			{"get", map[string]any{"id": id}},
			{"search", map[string]any{"category": "i409"}},
			{"recent", map[string]any{"category": "i409"}},
		} {
			got, want := call(t, proxy, read.tool, read.args), call(t, direct, read.tool, read.args)
			if !sameJSON(t, got, want) {
				t.Errorf("%s via proxy = %q, /mcp = %q", read.tool, resultText(got), resultText(want))
			}
		}
	})

	t.Run("a refusal reads in the server's words", func(t *testing.T) {
		_, perr := proxy.CallTool(context.Background(), &mcp.CallToolParams{Name: "no_such_tool", Arguments: map[string]any{}})
		_, derr := direct.CallTool(context.Background(), &mcp.CallToolParams{Name: "no_such_tool", Arguments: map[string]any{}})
		var pw, dw *jsonrpc.Error
		if !errors.As(perr, &pw) || !errors.As(derr, &dw) || pw.Code != dw.Code || pw.Message != dw.Message {
			t.Errorf("unknown tool via proxy = %v, /mcp = %v", perr, derr)
		}
		args := map[string]any{"category": "i409", "title": "no content", "content": ""}
		if got, want := call(t, proxy, "store", args), call(t, direct, "store", args); !got.IsError || !sameJSON(t, got, want) {
			t.Errorf("store without content via proxy = %q, /mcp = %q", resultText(got), resultText(want))
		}
		withFile := map[string]any{"category": "i409", "title": "file", "content": "x", "content_file": "/etc/hostname"}
		if got, want := call(t, proxy, "store", withFile), call(t, direct, "store", withFile); !got.IsError || !sameJSON(t, got, want) {
			t.Errorf("content_file without --allow-files via proxy = %q, /mcp = %q", resultText(got), resultText(want))
		}
	})

	t.Run("a staged write and its confirm", func(t *testing.T) {
		ctx := context.Background()
		row, flagged, err := store.CreateApiKey(ctx, pool, "i409-flagged", "private", nil, store.DefaultTenantID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE context_api_keys SET confirm_writes = true WHERE id = $1`, row.ID); err != nil {
			t.Fatal(err)
		}
		staged := proxySession(t, srv.URL, flagged, nil)
		res := call(t, staged, "store", map[string]any{"category": "i409-staged", "title": "staged", "content": "held until confirmed", "sensitivity": "internal"})
		hash := regexp.MustCompile(`payload_hash: ([0-9a-f]+)`).FindStringSubmatch(resultText(res))
		if !res.IsError || hash == nil {
			t.Fatalf("store with a confirm_writes key via proxy: %q", resultText(res))
		}
		if n := blockCount(t, pool, "i409-staged"); n != 0 {
			t.Fatalf("staged write stored %d blocks before the confirm", n)
		}
		confirmed := call(t, staged, "confirm", map[string]any{"payload_hash": hash[1]})
		if confirmed.IsError || blockCount(t, pool, "i409-staged") != 1 {
			t.Errorf("confirm via proxy = %q, blocks = %d", resultText(confirmed), blockCount(t, pool, "i409-staged"))
		}
	})

	t.Run("a rejected key names the cause and never the key", func(t *testing.T) {
		const wrong = "0409deadbeef0409deadbeef0409deadbeef0409deadbeef0409deadbeef0409"
		bad := proxySession(t, srv.URL, wrong, nil)
		res := call(t, bad, "search", map[string]any{"query": "x"})
		if !res.IsError || !strings.Contains(resultText(res), "refused the API key") || strings.Contains(resultText(res), wrong) {
			t.Errorf("call with a rejected key answered %q", resultText(res))
		}
		if _, err := bad.ListTools(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "refused the API key") || strings.Contains(err.Error(), wrong) {
			t.Errorf("tools/list with a rejected key: %v", err)
		}
	})
}

// fileDirs builds an allowed directory and an outside one with the fixtures the
// file parameters are probed with.
func fileDirs(t *testing.T) (allowed, outside string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	allowed, outside = filepath.Join(base, "allowed"), filepath.Join(base, "outside")
	for _, d := range []string{allowed, outside} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		filepath.Join(allowed, "note.md"):   "the file's text",
		filepath.Join(allowed, "empty.md"):  "",
		filepath.Join(allowed, "big.md"):    strings.Repeat("b", 50*1024+1),
		filepath.Join(outside, "secret.md"): "outside text",
	}
	for p, c := range files {
		if err := os.WriteFile(p, []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(outside, "secret.md"), filepath.Join(allowed, "leak.md")); err != nil {
		t.Fatal(err)
	}
	return allowed, outside
}

func TestMCPServeFiles_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	router, pool, key := mcpBodyCapRouter(t)
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	allowed, outside := fileDirs(t)
	direct := directSession(t, srv.URL, key)
	proxy := proxySession(t, srv.URL, key, []string{allowed})

	t.Run("the server's tools plus the two file parameters", func(t *testing.T) {
		got, ref := listTools(t, proxy), listTools(t, direct)
		if len(got) != len(ref) {
			t.Fatalf("proxy lists %d tools, /mcp %d", len(got), len(ref))
		}
		withContentFile := 0
		for i, tool := range got {
			var schema map[string]any
			raw, _ := json.Marshal(tool.InputSchema)
			_ = json.Unmarshal(raw, &schema)
			props, _ := schema["properties"].(map[string]any)
			if _, ok := props["content_file"]; ok {
				withContentFile++
				delete(props, "content_file")
				// content stays a parameter but is no longer required: the
				// server's required list minus content.
				var refSchema map[string]any
				rr, _ := json.Marshal(ref[i].InputSchema)
				_ = json.Unmarshal(rr, &refSchema)
				schema["required"] = refSchema["required"]
			}
			if _, ok := props["output_file"]; ok != (tool.Name == "get") {
				t.Errorf("%s: output_file offered = %v", tool.Name, ok)
			}
			delete(props, "output_file")
			tc := *tool
			tc.InputSchema = schema
			if !sameJSON(t, &tc, ref[i]) {
				t.Errorf("%s differs from /mcp beyond the file parameters", tool.Name)
			}
		}
		// store, update, issue_create, issue_comment take a content string.
		if withContentFile < 4 {
			t.Errorf("content_file offered on %d tools, want at least 4", withContentFile)
		}
	})

	t.Run("content_file inside is sent as content", func(t *testing.T) {
		res := call(t, proxy, "store", map[string]any{"category": "i409-files", "title": "from a file", "content_file": filepath.Join(allowed, "note.md"), "sensitivity": "internal"})
		m := storedID.FindStringSubmatch(resultText(res))
		if res.IsError || m == nil {
			t.Fatalf("store with content_file: %q", resultText(res))
		}
		var block struct{ Content string }
		_ = json.Unmarshal([]byte(resultText(call(t, direct, "get", map[string]any{"id": m[1]}))), &block)
		if block.Content != "the file's text" {
			t.Errorf("stored content = %q, want the file's text", block.Content)
		}
	})

	t.Run("content_file refusals send nothing", func(t *testing.T) {
		for name, tc := range map[string]struct {
			tool    string
			args    map[string]any
			errPart string
		}{
			"outside":              {"store", map[string]any{"content_file": filepath.Join(outside, "secret.md")}, "outside the directories"},
			"symlink leaving":      {"store", map[string]any{"content_file": filepath.Join(allowed, "leak.md")}, "outside the directories"},
			"missing":              {"store", map[string]any{"content_file": filepath.Join(allowed, "missing.md")}, "does not exist"},
			"empty":                {"store", map[string]any{"content_file": filepath.Join(allowed, "empty.md")}, "is empty"},
			"over the limit":       {"store", map[string]any{"content_file": filepath.Join(allowed, "big.md")}, "more than the"},
			"with content":         {"store", map[string]any{"content_file": filepath.Join(allowed, "note.md"), "content": "x"}, "not both"},
			"tool without content": {"search", map[string]any{"content_file": filepath.Join(allowed, "note.md")}, "takes no content"},
		} {
			tc.args["category"], tc.args["title"] = "i409-refused", name
			res := call(t, proxy, tc.tool, tc.args)
			if !res.IsError || !strings.Contains(resultText(res), tc.errPart) || !strings.Contains(resultText(res), "nothing was sent") {
				t.Errorf("%s: answered %q, want a refusal containing %q", name, resultText(res), tc.errPart)
			}
		}
		if n := blockCount(t, pool, "i409-refused"); n != 0 {
			t.Errorf("refused content_file calls stored %d blocks", n)
		}
	})

	stored := call(t, direct, "store", map[string]any{"category": "i409-files", "title": "to a file", "content": "block content for the file", "sensitivity": "internal"})
	id := storedID.FindStringSubmatch(resultText(stored))[1]

	t.Run("output_file inside receives the content", func(t *testing.T) {
		target := filepath.Join(allowed, "out.md")
		res := call(t, proxy, "get", map[string]any{"id": id, "output_file": target})
		if res.IsError {
			t.Fatalf("get with output_file: %q", resultText(res))
		}
		if b, _ := os.ReadFile(target); string(b) != "block content for the file" {
			t.Errorf("file holds %q", b)
		}
		var got, want map[string]any
		_ = json.Unmarshal([]byte(resultText(res)), &got)
		_ = json.Unmarshal([]byte(resultText(call(t, direct, "get", map[string]any{"id": id}))), &want)
		ref, _ := got["output_file"].(map[string]any)
		if ref["path"] != target || ref["bytes"] != float64(len("block content for the file")) {
			t.Errorf("output_file answer = %v", ref)
		}
		if _, has := got["content"]; has {
			t.Error("the answer still carries the content")
		}
		delete(got, "output_file")
		delete(want, "content")
		if !reflect.DeepEqual(got, want) {
			t.Errorf("the rest of the answer differs from /mcp:\n%v\n%v", got, want)
		}
	})

	t.Run("output_file refusals write nothing", func(t *testing.T) {
		for name, tc := range map[string]struct {
			tool, path, errPart string
		}{
			"outside":           {"get", filepath.Join(outside, "x.md"), "outside the directories"},
			"symlink leaving":   {"get", filepath.Join(allowed, "leak.md"), "outside the directories"},
			"missing directory": {"get", filepath.Join(allowed, "nodir", "x.md"), "does not exist"},
			"not get":           {"search", filepath.Join(allowed, "s.md"), "get only"},
		} {
			res := call(t, proxy, tc.tool, map[string]any{"id": id, "query": "x", "output_file": tc.path})
			if !res.IsError || !strings.Contains(resultText(res), tc.errPart) {
				t.Errorf("%s: answered %q, want a refusal containing %q", name, resultText(res), tc.errPart)
			}
		}
		if b, _ := os.ReadFile(filepath.Join(outside, "secret.md")); string(b) != "outside text" {
			t.Errorf("the file behind the leaving symlink changed: %q", b)
		}
		for _, p := range []string{filepath.Join(outside, "x.md"), filepath.Join(allowed, "s.md")} {
			if _, err := os.Stat(p); err == nil {
				t.Errorf("%s was written", p)
			}
		}
	})

	t.Run("a server refusal passes and nothing is written", func(t *testing.T) {
		target := filepath.Join(allowed, "never.md")
		args := map[string]any{"id": "01a19999-0000-7000-8000-000000000000"}
		want := call(t, direct, "get", args)
		args["output_file"] = target
		got := call(t, proxy, "get", args)
		if !got.IsError || !sameJSON(t, got, want) {
			t.Errorf("get of an unknown id via proxy = %q, /mcp = %q", resultText(got), resultText(want))
		}
		if _, err := os.Stat(target); err == nil {
			t.Error("a refused get wrote the file")
		}
	})
}

// TestMCPServeUnreachableAndRestart_Integration: a server that cannot be
// reached answers with the cause and leaves the proxy serving; once a server
// listens on the address again, the same proxy reaches it.
func TestMCPServeUnreachableAndRestart_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	router, _, key := mcpBodyCapRouter(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	start := func(l net.Listener) *httptest.Server {
		s := httptest.NewUnstartedServer(router)
		_ = s.Listener.Close()
		s.Listener = l
		s.Start()
		return s
	}
	srv := start(l)
	proxy := proxySession(t, "http://"+addr, key, nil)
	search := map[string]any{"query": "anything"}
	if res := call(t, proxy, "search", search); res.IsError {
		t.Fatalf("search before the restart: %q", resultText(res))
	}

	srv.Close()
	for range 2 {
		res := call(t, proxy, "search", search)
		if !res.IsError || !strings.Contains(resultText(res), "cannot use the server at http://"+addr+"/mcp") || strings.Contains(resultText(res), key) {
			t.Errorf("search while the server is down answered %q", resultText(res))
		}
	}
	if _, err := proxy.ListTools(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "cannot use the server") {
		t.Errorf("tools/list while the server is down: %v", err)
	}

	l2, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listen again on %s: %v", addr, err)
	}
	srv2 := start(l2)
	t.Cleanup(srv2.Close)
	if res := call(t, proxy, "search", search); res.IsError {
		t.Errorf("search after the restart: %q", resultText(res))
	}
	if tools := listTools(t, proxy); len(tools) == 0 {
		t.Error("tools/list after the restart is empty")
	}
}
