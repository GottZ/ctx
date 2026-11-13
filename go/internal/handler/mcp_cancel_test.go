package handler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// #195: a client that disconnects while the query tool runs ends the delegated
// /api/query. Before, go-sdk v1.8.0 handed the tool a context whose Done was
// masked, and the delegate ran to its end for an answer nobody received.

// mcpCancelBound is how long after the client's disconnect the delegate's
// context may still be live. On loopback the server sees the disconnect at
// once; the bound only absorbs scheduling under -race and load.
const mcpCancelBound = 2 * time.Second

// cancelWatchDelegate stands in for /api/query and reports what happens to the
// request context it is handed: it blocks until that context ends or the test
// releases it, and answers only in the second case.
type cancelWatchDelegate struct {
	started   chan struct{}
	canceled  chan struct{}
	release   chan struct{}
	startOnce sync.Once
	relOnce   sync.Once
}

func newCancelWatchDelegate() *cancelWatchDelegate {
	return &cancelWatchDelegate{
		started:  make(chan struct{}),
		canceled: make(chan struct{}),
		release:  make(chan struct{}),
	}
}

func (d *cancelWatchDelegate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, _ = io.ReadAll(r.Body)
	d.startOnce.Do(func() { close(d.started) })
	select {
	case <-r.Context().Done():
		close(d.canceled)
	case <-d.release:
		_, _ = w.Write([]byte(slowQueryBody))
	}
}

func (d *cancelWatchDelegate) releaseNow() { d.relOnce.Do(func() { close(d.release) }) }

// mcpQueryCallFor renders the query tools/call for one protocol generation.
// 2026-07-28 carries its handshake per request (SEP-2575): the _meta triple in
// the params and the Mcp-* headers.
func mcpQueryCallFor(modern bool) (string, map[string]string) {
	if !modern {
		return mcpQueryCall, nil
	}
	const v2 = "2026-07-28"
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"query","arguments":{"question":"q"},` +
		`"_meta":{"io.modelcontextprotocol/protocolVersion":"` + v2 + `",` +
		`"io.modelcontextprotocol/clientInfo":{"name":"cancel-test","version":"0"},` +
		`"io.modelcontextprotocol/clientCapabilities":{}}}}`
	return body, map[string]string{
		"Mcp-Protocol-Version": v2,
		"Mcp-Method":           "tools/call",
		"Mcp-Name":             "query",
	}
}

// The client disconnects mid-call; the delegate's context must end within
// mcpCancelBound, and the handler must return with it. Covered on the default
// protocol and on 2026-07-28, each before the keepalive's first tick (nothing
// is on the wire yet) and after keepalive bytes went out (the 200 is
// committed).
func TestMCPQueryClientDisconnectCancelsDelegate(t *testing.T) {
	for _, proto := range []struct {
		name   string
		modern bool
	}{{"default", false}, {"2026-07-28", true}} {
		for _, ticking := range []bool{false, true} {
			name := proto.name + "/before first tick"
			if ticking {
				name = proto.name + "/after keepalive bytes"
			}
			t.Run(name, func(t *testing.T) {
				runMCPDisconnectCase(t, proto.modern, ticking)
			})
		}
	}
}

func runMCPDisconnectCase(t *testing.T, modern, ticking bool) {
	if ticking {
		shrinkHeartbeat(t, 20*time.Millisecond, time.Second)
	}
	delegate := newCancelWatchDelegate()
	h := NewMCPHandler(MCPConfig{QueryHandler: delegate})
	handlerReturned := make(chan struct{})
	srv := httptest.NewServer(withTestAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r)
		close(handlerReturned)
	})))
	defer srv.Close()
	// Registered after srv.Close, so it runs first: a red run must not leave
	// the delegate (and with it srv.Close) blocked.
	defer delegate.releaseNow()

	body, hdr := mcpQueryCallFor(modern)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	// Before the first tick no header arrives, so the round trip only returns
	// with the disconnect: it runs on its own goroutine.
	type rt struct {
		resp *http.Response
		err  error
	}
	respCh := make(chan rt, 1)
	go func() {
		resp, err := http.DefaultTransport.RoundTrip(req)
		respCh <- rt{resp, err}
	}()

	select {
	case <-delegate.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the query tool never reached the delegate")
	}
	if ticking {
		var got rt
		select {
		case got = <-respCh:
		case <-time.After(5 * time.Second):
			t.Fatal("no response headers: the keepalive never committed the 200")
		}
		if got.err != nil {
			t.Fatalf("POST: %v", got.err)
		}
		defer func() { _ = got.resp.Body.Close() }()
		buf := make([]byte, 2)
		if _, err := io.ReadFull(got.resp.Body, buf); err != nil || string(buf) != "  " {
			t.Fatalf("keepalive bytes: %q, %v", buf, err)
		}
	}

	disconnected := time.Now()
	cancel()

	select {
	case <-delegate.canceled:
		t.Logf("delegate context canceled %v after the disconnect", time.Since(disconnected))
	case <-time.After(mcpCancelBound):
		t.Fatalf("delegate context still live %v after the client disconnected", mcpCancelBound)
	}
	select {
	case <-handlerReturned:
	case <-time.After(mcpCancelBound):
		t.Fatalf("/mcp handler still running %v after the delegate's context ended", mcpCancelBound)
	}
}

// Without a disconnect nothing changes: the delegate's context stays live
// until it answers, and the answer reaches the client. A regression guard for
// the derivation itself (a context that ended at once, or with the tool's
// first keepalive tick, would fail here).
func TestMCPQueryNoDisconnectKeepsDelegate(t *testing.T) {
	shrinkHeartbeat(t, 20*time.Millisecond, time.Second)
	delegate := newCancelWatchDelegate()
	h := NewMCPHandler(MCPConfig{QueryHandler: delegate})
	srv := httptest.NewServer(withTestAuth(h))
	defer srv.Close()
	defer delegate.releaseNow()

	go func() {
		select {
		case <-delegate.started:
		case <-time.After(5 * time.Second):
			return // the POST failed before the tool ran; the test reports it
		}
		// Long enough for several keepalive ticks.
		time.Sleep(150 * time.Millisecond)
		delegate.releaseNow()
	}()
	resp := mcpPost(t, context.Background(), srv.URL, mcpQueryCall, nil)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	select {
	case <-delegate.canceled:
		t.Fatal("delegate context ended although the client stayed")
	default:
	}
	if !strings.Contains(string(b), "made-it") {
		t.Fatalf("body = %q; want the answer made-it", b)
	}
}
