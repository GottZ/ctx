package handler

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// shrinkHeartbeat sets the keepalive cadence and write window for one test.
func shrinkHeartbeat(t *testing.T, interval, window time.Duration) {
	t.Helper()
	origInterval, origWindow := heartbeatInterval, heartbeatWriteWindow
	heartbeatInterval, heartbeatWriteWindow = interval, window
	t.Cleanup(func() { heartbeatInterval, heartbeatWriteWindow = origInterval, origWindow })
}

// slowQueryDelegate stands in for /api/query: it answers a canned body after
// d. With release set it ignores the request context and waits for the test
// instead, the way a pipeline stage that does not watch its context would.
type slowQueryDelegate struct {
	d       time.Duration
	release chan struct{}
	started chan struct{}
	once    sync.Once
	t       *testing.T
}

const slowQueryBody = `{"answer":"made-it","confidence":"high","sources":[]}`

func (h *slowQueryDelegate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, _ = io.ReadAll(r.Body)
	if h.started != nil {
		h.once.Do(func() { close(h.started) })
	}
	if h.release != nil {
		<-h.release
	} else {
		select {
		case <-time.After(h.d):
		case <-r.Context().Done():
			if h.t != nil {
				h.t.Error("request context canceled: the write deadline was not extended")
			}
		}
	}
	_, _ = w.Write([]byte(slowQueryBody))
}

// withTestAuth injects an AuthResult the way the Auth middleware does.
func withTestAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), authResultKey, typeFilterCtx().Value(authResultKey))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

const mcpQueryCall = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"query","arguments":{"question":"q"}}}`

func mcpPost(t *testing.T, ctx context.Context, url, body string, hdr map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	return resp
}

// The /mcp twin of TestHeartbeatOutlivesServerWriteTimeout: a query whose
// pipeline runs 3x the server's ABSOLUTE WriteTimeout must still reach an MCP
// client. Without the keepalive nothing rolls the write deadline, the server
// cancels the request at WriteTimeout and the client gets no result.
func TestMCPKeepaliveOutlivesServerWriteTimeout(t *testing.T) {
	shrinkHeartbeat(t, 50*time.Millisecond, 500*time.Millisecond)

	h := NewMCPHandler(MCPConfig{QueryHandler: &slowQueryDelegate{d: 900 * time.Millisecond, t: t}})
	srv := httptest.NewUnstartedServer(withTestAuth(h))
	srv.Config.WriteTimeout = 300 * time.Millisecond
	srv.Start()
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "keepalive-test", Version: "0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             srv.URL,
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = session.Close() }()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "query",
		Arguments: map[string]any{"question": "q"},
	})
	if err != nil {
		t.Fatalf("tools/call query (connection killed by WriteTimeout?): %v", err)
	}
	if res.IsError || !strings.Contains(mcpTextOf(res), "made-it") {
		t.Fatalf("IsError=%v text=%q; want the answer made-it", res.IsError, mcpTextOf(res))
	}
}

// Cadence on the wire: the first byte arrives within 2x the interval, no gap
// between bytes is longer than that until the result arrives, the bytes in
// front of the result are spaces only, and the whole body is still one JSON
// document under a 200 application/json.
func TestMCPKeepaliveCadence(t *testing.T) {
	const interval = 100 * time.Millisecond
	shrinkHeartbeat(t, interval, 2*time.Second)

	h := NewMCPHandler(MCPConfig{QueryHandler: &slowQueryDelegate{d: 1050 * time.Millisecond}})
	srv := httptest.NewServer(withTestAuth(h))
	defer srv.Close()

	start := time.Now()
	resp := mcpPost(t, context.Background(), srv.URL, mcpQueryCall, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}

	br := bufio.NewReader(resp.Body)
	last, maxGap, spaces := start, time.Duration(0), 0
	for {
		b, err := br.ReadByte()
		if err != nil {
			t.Fatalf("read before the result arrived: %v", err)
		}
		now := time.Now()
		if gap := now.Sub(last); gap > maxGap {
			maxGap = gap
		}
		last = now
		if b != ' ' {
			if err := br.UnreadByte(); err != nil {
				t.Fatal(err)
			}
			break
		}
		spaces++
	}
	if maxGap > 2*interval {
		t.Errorf("longest gap %v, want <= %v", maxGap, 2*interval)
	}
	if spaces < 5 {
		t.Errorf("keepalive bytes = %d, want >= 5 in ~1s at %v", spaces, interval)
	}
	t.Logf("keepalive bytes=%d longest gap=%v total=%v", spaces, maxGap, time.Since(start))

	rest, err := io.ReadAll(br)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	var rpc struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rest, &rpc); err != nil {
		t.Fatalf("result is not JSON: %v (%q)", err, rest)
	}
	if rpc.Result.IsError || len(rpc.Result.Content) == 0 || !strings.Contains(rpc.Result.Content[0].Text, "made-it") {
		t.Errorf("result = %q; want the answer made-it", rest)
	}
}

// afterReturnSpy sits between the server and the keepalive. It counts every
// call that reaches the real ResponseWriter after the wrapped handler has
// returned, which net/http forbids.
type afterReturnSpy struct {
	http.ResponseWriter
	returned atomic.Bool
	late     atomic.Int64
	writes   atomic.Int64
}

func (s *afterReturnSpy) note() {
	if s.returned.Load() {
		s.late.Add(1)
	}
}
func (s *afterReturnSpy) WriteHeader(code int) { s.note(); s.ResponseWriter.WriteHeader(code) }
func (s *afterReturnSpy) Write(b []byte) (int, error) {
	s.note()
	s.writes.Add(1)
	return s.ResponseWriter.Write(b)
}
func (s *afterReturnSpy) FlushError() error {
	s.note()
	return http.NewResponseController(s.ResponseWriter).Flush()
}
func (s *afterReturnSpy) SetWriteDeadline(t time.Time) error {
	s.note()
	return http.NewResponseController(s.ResponseWriter).SetWriteDeadline(t)
}

// A client that goes away mid-call, while the tool keeps running (the
// delegate ignores its context). The keepalive must go quiet once the request
// context is canceled, and nothing may reach the writer after ServeHTTP
// returned. Run under -race.
func TestMCPKeepaliveClientDisconnectMidCall(t *testing.T) {
	const interval = 20 * time.Millisecond
	shrinkHeartbeat(t, interval, time.Second)

	delegate := &slowQueryDelegate{release: make(chan struct{}), started: make(chan struct{})}
	h := NewMCPHandler(MCPConfig{QueryHandler: delegate})

	spyCh := make(chan *afterReturnSpy, 1)
	ctxDone := make(chan struct{})
	handlerReturned := make(chan struct{})
	srv := httptest.NewServer(withTestAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		spy := &afterReturnSpy{ResponseWriter: w}
		spyCh <- spy
		go func() { <-r.Context().Done(); close(ctxDone) }()
		h.ServeHTTP(spy, r)
		spy.returned.Store(true)
		close(handlerReturned)
	})))
	defer srv.Close()
	// Registered after srv.Close, so it runs first: a failing test must not
	// leave the delegate (and with it srv.Close) blocked.
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(delegate.release) }) }
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The tool never finishes on its own here, so without a keepalive neither
	// the response headers nor a body byte ever arrive. Bound the wait: the
	// timer cancels the request, and the POST or the read below fails within
	// keepaliveWait instead of hanging until the go test timeout.
	const keepaliveWait = 2 * time.Second
	var timedOut atomic.Bool
	deadline := time.AfterFunc(keepaliveWait, func() { timedOut.Store(true); cancel() })
	defer deadline.Stop()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, strings.NewReader(mcpQueryCall))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		if timedOut.Load() {
			t.Fatalf("no response within %v: the keepalive never committed the 200 (%v)", keepaliveWait, err)
		}
		t.Fatalf("POST: %v", err)
	}
	spy := <-spyCh
	// Wait for keepalive bytes, so the disconnect lands mid-stream.
	buf := make([]byte, 1)
	for i := 0; i < 3; i++ {
		if _, err := io.ReadFull(resp.Body, buf); err != nil || buf[0] != ' ' {
			if timedOut.Load() {
				t.Fatalf("keepalive byte %d did not arrive within %v (%v)", i, keepaliveWait, err)
			}
			t.Fatalf("keepalive byte %d: %q, %v", i, buf, err)
		}
	}
	deadline.Stop()
	<-delegate.started
	cancel()
	_ = resp.Body.Close()

	select {
	case <-ctxDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the server never saw the client disconnect")
	}
	// A tick that was already past its checks may still land.
	time.Sleep(2 * interval)
	quiet := spy.writes.Load()
	// The tool is still running. Give an unguarded ticker ten chances.
	time.Sleep(10 * interval)
	if n := spy.writes.Load(); n != quiet {
		t.Errorf("keepalive kept writing after the disconnect: %d -> %d writes", quiet, n)
	}

	release()
	select {
	case <-handlerReturned:
	case <-time.After(5 * time.Second):
		t.Fatal("ServeHTTP did not return after the tool finished")
	}
	time.Sleep(5 * interval)
	if n := spy.late.Load(); n != 0 {
		t.Errorf("%d calls reached the ResponseWriter after ServeHTTP returned", n)
	}
}

// recordingWriter is a ResponseWriter for the wrapper-level tests. status is
// the first status, as on the wire; statuses is every WriteHeader call that
// reached it, so a test can tell a dropped status from a superfluous one.
// With writeErr set, Write fails and records nothing.
type recordingWriter struct {
	mu       sync.Mutex
	hdr      http.Header
	status   int
	statuses []int
	body     []byte
	calls    int
	writeErr error
}

func (r *recordingWriter) Header() http.Header { return r.hdr }
func (r *recordingWriter) WriteHeader(code int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.statuses = append(r.statuses, code)
	if r.status == 0 {
		r.status = code
	}
}
func (r *recordingWriter) Write(b []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.writeErr != nil {
		return 0, r.writeErr
	}
	if r.status == 0 {
		r.status = http.StatusOK
	}
	r.body = append(r.body, b...)
	return len(b), nil
}
func (r *recordingWriter) FlushError() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return nil
}
func (r *recordingWriter) SetWriteDeadline(time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return nil
}
func (r *recordingWriter) snapshot() (status int, body string, calls int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status, string(r.body), r.calls
}
func (r *recordingWriter) statusCalls() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int(nil), r.statuses...)
}

func keepalivePost() *http.Request {
	return httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}"))
}

// The closed guard on its own: the handler returns while an arm is still
// live and the request context is NOT canceled. No tick may follow.
func TestMCPKeepaliveNoWriteAfterHandlerReturn(t *testing.T) {
	const interval = 10 * time.Millisecond
	shrinkHeartbeat(t, interval, time.Second)

	rec := &recordingWriter{hdr: make(http.Header)}
	var stop func()
	withMCPKeepalive(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		stop = armMCPKeepalive(r.Context()) // never stopped before the return
		time.Sleep(5 * interval)
	})).ServeHTTP(rec, keepalivePost())
	defer stop()

	status, body, calls := rec.snapshot()
	if status != http.StatusOK || body == "" || strings.Trim(body, " ") != "" {
		t.Fatalf("while armed: status %d body %q; want 200 and spaces only", status, body)
	}
	time.Sleep(10 * interval)
	if _, _, after := rec.snapshot(); after != calls {
		t.Errorf("writer calls went from %d to %d after ServeHTTP returned", calls, after)
	}
}

// stop() is a hard edge as well: after it, nothing follows the payload.
func TestMCPKeepaliveNoTickAfterStopOrPayload(t *testing.T) {
	const interval = 10 * time.Millisecond
	shrinkHeartbeat(t, interval, time.Second)

	rec := &recordingWriter{hdr: make(http.Header)}
	withMCPKeepalive(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stop := armMCPKeepalive(r.Context())
		time.Sleep(5 * interval)
		stop()
		_, _ = w.Write([]byte(`{"ok":true}`))
		time.Sleep(5 * interval)
	})).ServeHTTP(rec, keepalivePost())

	_, body, _ := rec.snapshot()
	if !strings.HasSuffix(body, `{"ok":true}`) || strings.Trim(strings.TrimSuffix(body, `{"ok":true}`), " ") != "" {
		t.Errorf("body = %q; want spaces, then the payload, then nothing", body)
	}
	var v map[string]bool
	if err := json.Unmarshal([]byte(body), &v); err != nil || !v["ok"] {
		t.Errorf("body does not decode as the payload: %v (%q)", err, body)
	}
}

// newIdleKeepalive is a keepalive whose ticker never fires (the cadence is one
// hour), so a test can call tick itself, with the wrapper in a state it chose.
// The returned finish closes the keepalive unless the test already did.
func newIdleKeepalive(t *testing.T) (k *mcpKeepalive, rec *recordingWriter, cancel context.CancelFunc) {
	t.Helper()
	shrinkHeartbeat(t, time.Hour, time.Second)
	rec = &recordingWriter{hdr: make(http.Header)}
	ctx, cancel := context.WithCancel(context.Background())
	k = &mcpKeepalive{w: rec, rc: http.NewResponseController(rec), ctx: ctx, done: make(chan struct{})}
	t.Cleanup(func() {
		cancel()
		k.mu.Lock()
		closed := k.closed
		k.mu.Unlock()
		if !closed {
			k.close()
		}
	})
	return k, rec, cancel
}

// Each condition under which a tick must not touch the writer, one at a time
// and with every other condition in the state that would let the tick through.
// tick is called directly, so no case depends on timing: dropping any one of
// the six checks in tick fails its case.
func TestMCPKeepaliveTickGuards(t *testing.T) {
	t.Run("an armed call ticks", func(t *testing.T) {
		k, rec, _ := newIdleKeepalive(t)
		defer k.arm()()
		for i := 0; i < 2; i++ {
			if !k.tick(time.Second) {
				t.Fatalf("tick %d gave up on an armed, open, live keepalive", i)
			}
		}
		if status, body, _ := rec.snapshot(); status != http.StatusOK || body != "  " {
			t.Errorf("status %d body %q; want 200 and two spaces", status, body)
		}
	})
	t.Run("one of two calls still armed ticks", func(t *testing.T) {
		k, rec, _ := newIdleKeepalive(t)
		stopA, stopB := k.arm(), k.arm()
		defer stopB()
		stopA()
		stopA() // a second stop of the same arm must not count twice
		if !k.tick(time.Second) {
			t.Fatal("tick gave up while a call was still armed")
		}
		if _, body, _ := rec.snapshot(); body != " " {
			t.Errorf("body %q; want one space", body)
		}
	})

	quiet := func(t *testing.T, k *mcpKeepalive, rec *recordingWriter, wantBody string) {
		t.Helper()
		if k.tick(time.Second) {
			t.Error("tick reported the keepalive as still alive")
		}
		status, body, calls := rec.snapshot()
		if body != wantBody {
			t.Errorf("body %q, want %q: the tick wrote", body, wantBody)
		}
		if wantBody == "" && (status != 0 || calls != 0) {
			t.Errorf("status %d, %d writer calls; want an untouched writer", status, calls)
		}
	}
	t.Run("never armed", func(t *testing.T) {
		k, rec, _ := newIdleKeepalive(t)
		quiet(t, k, rec, "")
	})
	t.Run("after the last stop", func(t *testing.T) {
		k, rec, _ := newIdleKeepalive(t)
		stopA, stopB := k.arm(), k.arm()
		stopA()
		stopB()
		quiet(t, k, rec, "")
	})
	t.Run("after the payload started", func(t *testing.T) {
		k, rec, _ := newIdleKeepalive(t)
		defer k.arm()()
		// The early 200 and one space are out, then the SDK starts its body.
		if !k.tick(time.Second) {
			t.Fatal("tick gave up on an armed, open, live keepalive")
		}
		if _, err := k.Write([]byte(`{"ok":true}`)); err != nil {
			t.Fatal(err)
		}
		quiet(t, k, rec, ` {"ok":true}`)
	})
	t.Run("request context canceled", func(t *testing.T) {
		k, rec, cancel := newIdleKeepalive(t)
		defer k.arm()()
		cancel()
		quiet(t, k, rec, "")
	})
	t.Run("handler returned", func(t *testing.T) {
		k, rec, _ := newIdleKeepalive(t)
		defer k.arm()()
		k.close()
		quiet(t, k, rec, "")
	})
	t.Run("the SDK chose a content type other than JSON", func(t *testing.T) {
		k, rec, _ := newIdleKeepalive(t)
		defer k.arm()()
		k.Header().Set("Content-Type", "text/event-stream")
		quiet(t, k, rec, "")
		if ct := rec.hdr.Get("Content-Type"); ct != "text/event-stream" {
			t.Errorf("Content-Type = %q; the tick replaced the SDK's choice", ct)
		}
	})
	// The SDK has sent a status of its own but no body byte yet, and a call is
	// still armed: the response is the SDK's, so the tick must not put a space
	// behind that status, nor a second status.
	t.Run("the SDK committed its own status", func(t *testing.T) {
		k, rec, _ := newIdleKeepalive(t)
		defer k.arm()()
		k.Header().Set("Content-Type", "application/json")
		k.WriteHeader(http.StatusNotFound)
		_, _, before := rec.snapshot()
		if k.tick(time.Second) {
			t.Error("tick reported the keepalive as still alive")
		}
		status, body, calls := rec.snapshot()
		if status != http.StatusNotFound || body != "" || calls != before {
			t.Errorf("status %d body %q, writer calls %d -> %d; want the SDK's 404 and nothing else",
				status, body, before, calls)
		}
		if got := rec.statusCalls(); len(got) != 1 || got[0] != http.StatusNotFound {
			t.Errorf("WriteHeader calls on the writer = %v, want [404]", got)
		}
	})
	// Not a guard in front of the write but the same exit: a byte that cannot
	// be written (the connection is gone) ends the ticker.
	t.Run("the write of the byte fails", func(t *testing.T) {
		k, rec, _ := newIdleKeepalive(t)
		defer k.arm()()
		rec.writeErr = io.ErrClosedPipe
		if k.tick(time.Second) {
			t.Error("tick reported the keepalive as still alive after a failed write")
		}
		k.mu.Lock()
		ticking := k.ticking
		k.mu.Unlock()
		if ticking {
			t.Error("ticking still set after the tick gave up")
		}
	})
}

// Once the handler has returned (close), no writer method of the wrapper may
// reach the real ResponseWriter, each one on its own: net/http forbids it, and
// the SDK runs tool handlers on a goroutine that may still hold the wrapper.
// The methods that can report it answer http.ErrHandlerTimeout.
func TestMCPKeepaliveWriterMethodsAfterClose(t *testing.T) {
	cases := []struct {
		name    string
		call    func(k *mcpKeepalive) error
		wantErr bool
	}{
		{"WriteHeader", func(k *mcpKeepalive) error { k.WriteHeader(http.StatusNotFound); return nil }, false},
		{"Write", func(k *mcpKeepalive) error { _, err := k.Write([]byte("x")); return err }, true},
		{"FlushError", func(k *mcpKeepalive) error { return k.FlushError() }, true},
		{"Flush", func(k *mcpKeepalive) error { k.Flush(); return nil }, false},
		{"SetWriteDeadline", func(k *mcpKeepalive) error { return k.SetWriteDeadline(time.Now().Add(time.Second)) }, true},
		// The way the SDK and net/http reach the last two.
		{"ResponseController.Flush", func(k *mcpKeepalive) error { return http.NewResponseController(k).Flush() }, true},
		{"ResponseController.SetWriteDeadline", func(k *mcpKeepalive) error {
			return http.NewResponseController(k).SetWriteDeadline(time.Now().Add(time.Second))
		}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k, rec, _ := newIdleKeepalive(t)
			// Open, the same call goes through: the case really exercises the
			// closed guard, not a writer that ignores the call anyway.
			if err := c.call(k); err != nil {
				t.Fatalf("on an open keepalive: %v", err)
			}
			if _, _, calls := rec.snapshot(); calls != 1 {
				t.Fatalf("on an open keepalive: %d writer calls, want 1", calls)
			}

			k, rec, _ = newIdleKeepalive(t)
			k.close()
			err := c.call(k)
			if c.wantErr && !errors.Is(err, http.ErrHandlerTimeout) {
				t.Errorf("err = %v, want http.ErrHandlerTimeout", err)
			}
			if status, body, calls := rec.snapshot(); status != 0 || body != "" || calls != 0 {
				t.Errorf("status %d body %q, %d writer calls; want an untouched writer", status, body, calls)
			}
		})
	}
}

// A ticker that gave up because no call was armed must not block the next
// call: a later arm starts a new one.
func TestMCPKeepaliveRearmsAfterTickerGaveUp(t *testing.T) {
	const interval = 10 * time.Millisecond
	shrinkHeartbeat(t, interval, time.Second)

	rec := &recordingWriter{hdr: make(http.Header)}
	withMCPKeepalive(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		armMCPKeepalive(r.Context())() // armed and stopped before any tick
		time.Sleep(5 * interval)       // the ticker fires, finds no call, ends
		if _, body, _ := rec.snapshot(); body != "" {
			t.Errorf("body %q after a call that ended before the first tick", body)
		}
		stop := armMCPKeepalive(r.Context())
		defer stop()
		time.Sleep(10 * interval)
	})).ServeHTTP(rec, keepalivePost())

	if status, body, _ := rec.snapshot(); status != http.StatusOK || body == "" || strings.Trim(body, " ") != "" {
		t.Errorf("second call: status %d body %q; want 200 and spaces only", status, body)
	}
}

// The ticker goroutine ends with the handler, not at its next tick: with the
// production cadence a finished request would otherwise hold one for up to
// heartbeatInterval. The request context is never canceled here, so only
// close() can end them.
func TestMCPKeepaliveTickerEndsWithHandler(t *testing.T) {
	shrinkHeartbeat(t, time.Hour, time.Hour)

	const requests, slack = 50, 10
	before := runtime.NumGoroutine()
	for i := 0; i < requests; i++ {
		rec := &recordingWriter{hdr: make(http.Header)}
		var stop func()
		withMCPKeepalive(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			stop = armMCPKeepalive(r.Context()) // still armed when the handler returns
		})).ServeHTTP(rec, keepalivePost())
		defer stop()
	}
	var after int
	for wait := time.Now().Add(2 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if after = runtime.NumGoroutine(); after <= before+slack || time.Now().After(wait) {
			break
		}
	}
	if after > before+slack {
		t.Errorf("goroutines: %d before, %d after %d requests; the tickers outlive their handlers", before, after, requests)
	}
}

// overlapDelegate answers the canned body after d and records how many calls
// were in flight at once.
type overlapDelegate struct {
	d        time.Duration
	inFlight atomic.Int32
	maxSeen  atomic.Int32
	calls    atomic.Int32
}

func (h *overlapDelegate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, _ = io.ReadAll(r.Body)
	h.calls.Add(1)
	n := h.inFlight.Add(1)
	for {
		seen := h.maxSeen.Load()
		if n <= seen || h.maxSeen.CompareAndSwap(seen, n) {
			break
		}
	}
	time.Sleep(h.d)
	h.inFlight.Add(-1)
	_, _ = w.Write([]byte(slowQueryBody))
}

// A JSON-RPC batch with two query calls (protocol 2025-03-26 and earlier; the
// SDK runs them concurrently) arms the keepalive twice. The response still has
// ONE cadence: one space per interval, then one JSON array with both results.
func TestMCPKeepaliveBatchOfTwoQueriesHasOneCadence(t *testing.T) {
	const interval = 100 * time.Millisecond
	shrinkHeartbeat(t, interval, 2*time.Second)

	delegate := &overlapDelegate{d: 1050 * time.Millisecond}
	srv := httptest.NewServer(withTestAuth(NewMCPHandler(MCPConfig{QueryHandler: delegate})))
	defer srv.Close()

	batch := `[` + mcpQueryCall + `,` + strings.Replace(mcpQueryCall, `"id":1`, `"id":2`, 1) + `]`
	start := time.Now()
	resp := mcpPost(t, context.Background(), srv.URL, batch, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	br := bufio.NewReader(resp.Body)
	spaces := 0
	for {
		b, err := br.ReadByte()
		if err != nil {
			t.Fatalf("read before the result arrived: %v", err)
		}
		if b != ' ' {
			if err := br.UnreadByte(); err != nil {
				t.Fatal(err)
			}
			break
		}
		spaces++
	}
	elapsed := time.Since(start)
	rest, err := io.ReadAll(br)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	var results []struct {
		ID     int `json:"id"`
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rest, &results); err != nil || len(results) != 2 {
		t.Fatalf("want a JSON array with two results: %v (%q)", err, rest)
	}
	for _, r := range results {
		if len(r.Result.Content) == 0 || !strings.Contains(r.Result.Content[0].Text, "made-it") {
			t.Errorf("result id %d = %q; want the answer made-it", r.ID, rest)
		}
	}
	// The test only means something if both calls really ran at once.
	if c, m := delegate.calls.Load(), delegate.maxSeen.Load(); c != 2 || m != 2 {
		t.Fatalf("delegate saw %d calls, %d at once; expected 2 and 2 (go-sdk behavior this test relies on)", c, m)
	}
	// One ticker: at most one space per elapsed interval. Two would write twice that.
	limit := int(elapsed/interval) + 1
	if spaces < 5 || spaces > limit {
		t.Errorf("keepalive bytes = %d in %v at %v; want 5..%d (one cadence)", spaces, elapsed, interval, limit)
	}
	t.Logf("keepalive bytes=%d elapsed=%v", spaces, elapsed)
}

// The status the SDK picks survives whenever the call is done before the
// first tick, even for an armed call: the SEP-2575 override included.
func TestMCPKeepaliveStatusBeforeFirstTickSurvives(t *testing.T) {
	shrinkHeartbeat(t, time.Hour, time.Hour)

	rec := &recordingWriter{hdr: make(http.Header)}
	withMCPKeepalive(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stop := armMCPKeepalive(r.Context())
		stop()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"x"}`))
	})).ServeHTTP(rec, keepalivePost())

	if status, body, _ := rec.snapshot(); status != http.StatusNotFound || body != `{"error":"x"}` {
		t.Errorf("status %d body %q; want 404 and the body untouched", status, body)
	}
}

// The documented cost of the early 200: a status that arrives after a tick is
// dropped, the body is still delivered as valid JSON behind the whitespace.
// Dropped means it never reaches the ResponseWriter: net/http would log a
// superfluous WriteHeader for it, so the test counts the WriteHeader calls and
// does not settle for the first status being 200.
func TestMCPKeepaliveStatusAfterTickIsDroppedBodyKept(t *testing.T) {
	const interval = 10 * time.Millisecond
	shrinkHeartbeat(t, interval, time.Second)

	rec := &recordingWriter{hdr: make(http.Header)}
	withMCPKeepalive(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stop := armMCPKeepalive(r.Context())
		time.Sleep(5 * interval)
		stop()
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"x"}`))
	})).ServeHTTP(rec, keepalivePost())

	status, body, _ := rec.snapshot()
	if status != http.StatusOK {
		t.Errorf("status %d; the early 200 is on the wire and cannot change", status)
	}
	if got := rec.statusCalls(); len(got) != 1 || got[0] != http.StatusOK {
		t.Errorf("WriteHeader calls on the writer = %v, want [200]: the late 404 was passed on", got)
	}
	if strings.Trim(strings.TrimSuffix(body, `{"error":"x"}`), " ") != "" || !strings.HasPrefix(body, " ") {
		t.Errorf("body %q; want keepalive spaces, then the error payload", body)
	}
	var v map[string]string
	if err := json.Unmarshal([]byte(body), &v); err != nil || v["error"] != "x" {
		t.Errorf("body %q does not decode as the error payload: %v", body, err)
	}
}

// A keepalive space is only valid in front of a JSON body. If the SDK has
// chosen another content type (SSE), the keepalive stays silent.
func TestMCPKeepaliveSilentOnNonJSONResponse(t *testing.T) {
	const interval = 10 * time.Millisecond
	shrinkHeartbeat(t, interval, time.Second)

	rec := &recordingWriter{hdr: make(http.Header)}
	withMCPKeepalive(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		stop := armMCPKeepalive(r.Context())
		defer stop()
		time.Sleep(5 * interval)
	})).ServeHTTP(rec, keepalivePost())

	if status, body, calls := rec.snapshot(); status != 0 || body != "" || calls != 0 {
		t.Errorf("status %d body %q calls %d; want an untouched writer", status, body, calls)
	}
}

// wireResponse is what a client can observe of one response.
type wireResponse struct {
	Status int
	Header http.Header
	Body   string
}

func mcpWire(t *testing.T, h http.Handler, body string, hdr map[string]string) wireResponse {
	t.Helper()
	srv := httptest.NewServer(withTestAuth(h))
	defer srv.Close()
	resp := mcpPost(t, context.Background(), srv.URL, body, hdr)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	resp.Header.Del("Date")
	return wireResponse{Status: resp.StatusCode, Header: resp.Header, Body: string(b)}
}

// Everything that is not a slow query answers exactly as the bare SDK handler
// does: status, headers (Content-Length included, so no chunking sneaks in)
// and body, compared on the wire. The cases cover get, search, a fast query,
// an unknown tool, invalid params and an unknown method, each on the default
// protocol and on 2026-07-28, where the SDK overrides the HTTP status
// (SEP-2575).
func TestMCPKeepaliveLeavesOtherResponsesUnchanged(t *testing.T) {
	// The production cadence: none of these calls lives to a first tick.
	cfg := MCPConfig{Pool: brokenPool(t), QueryHandler: &cannedQueryDelegate{body: slowQueryBody}}
	bare, wrapped := newMCPSDKHandler(cfg), NewMCPHandler(cfg)

	const v2 = "2026-07-28"
	const v2Meta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"` + v2 + `",` +
		`"io.modelcontextprotocol/clientInfo":{"name":"keepalive-test","version":"0"},` +
		`"io.modelcontextprotocol/clientCapabilities":{}}`
	// raw is sent as is; otherwise the case is a tools/call of tool with args.
	cases := []struct {
		name, tool, args, method, raw string
	}{
		{name: "get", tool: "get", args: `{"id":"019e0011-aaaa-7000-9000-000000000001"}`},
		{name: "get without id", tool: "get", args: `{"id":""}`},
		{name: "search", tool: "search", args: `{"query":"alpha"}`},
		{name: "fast query", tool: "query", args: `{"question":"q"}`},
		{name: "unknown tool", tool: "no_such_tool", args: `{}`},
		{name: "invalid params", tool: "query", args: `{"question":42}`},
		{name: "missing required param", tool: "get", args: `{}`},
		{name: "unknown method", method: "no/such"},
		{name: "tools/list", method: "tools/list"},
		{name: "malformed body", raw: `{"jsonrpc":`},
	}
	// build renders a case for one protocol generation. 2026-07-28 carries its
	// handshake per request: the _meta triple and the Mcp-* headers (SEP-2575).
	build := func(tool, args, method, raw string, modern bool) (string, map[string]string) {
		hdr := map[string]string{}
		meta := ""
		if modern {
			hdr["Mcp-Protocol-Version"] = v2
			meta = v2Meta
		}
		switch {
		case raw != "":
			return raw, hdr
		case method != "":
			if modern {
				hdr["Mcp-Method"] = method
			}
			return `{"jsonrpc":"2.0","id":7,"method":"` + method + `","params":{` + meta + `}}`, hdr
		}
		if modern {
			hdr["Mcp-Method"], hdr["Mcp-Name"] = "tools/call", tool
			meta = "," + meta
		}
		return `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"` + tool + `","arguments":` + args + meta + `}}`, hdr
	}
	versions := []struct {
		name   string
		modern bool
	}{
		{"default", false},
		{v2, true},
	}
	statuses := map[string]int{}
	for _, v := range versions {
		for _, c := range cases {
			t.Run(v.name+"/"+c.name, func(t *testing.T) {
				body, hdr := build(c.tool, c.args, c.method, c.raw, v.modern)
				want := mcpWire(t, bare, body, hdr)
				got := mcpWire(t, wrapped, body, hdr)
				statuses[v.name+"/"+c.name] = want.Status
				if got.Status != want.Status {
					t.Errorf("status = %d, bare SDK handler answers %d", got.Status, want.Status)
				}
				if got.Body != want.Body {
					t.Errorf("body differs\n got: %q\nwant: %q", got.Body, want.Body)
				}
				gh, _ := json.Marshal(got.Header)
				wh, _ := json.Marshal(want.Header)
				if string(gh) != string(wh) {
					t.Errorf("headers differ\n got: %s\nwant: %s", gh, wh)
				}
				t.Logf("status %d: %.160s", want.Status, want.Body)
			})
		}
	}
	// The comparison only means something for SEP-2575 if the bare handler
	// really overrides the status on these inputs.
	for name, want := range map[string]int{
		"default/unknown tool":      http.StatusOK,
		"2026-07-28/unknown tool":   http.StatusBadRequest,
		"2026-07-28/unknown method": http.StatusNotFound,
		"2026-07-28/get":            http.StatusOK,
	} {
		if statuses[name] != want {
			t.Errorf("bare SDK handler: %s = %d, expected %d (go-sdk behavior this test relies on)", name, statuses[name], want)
		}
	}
}
