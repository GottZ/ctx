package oai_test

// The stream route's end paths (W02-08): the disconnect chain D2–D4
// (design/02 §4.11.3), the leak probe, the slow reader (B8), the shutdown
// (D7), the recovery after the commit, the kill switch and the alias
// removal (B12), the deadline split (Z1), the writer domain (Z2), aborts
// without a health fault (Z3) and the reservation ended exactly once (Z5).

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/oai"
)

// externalWaiting is the number of external waiters on host.
func externalWaiting(d *dispatch.Dispatcher, host string) int {
	for _, ts := range d.Snapshot().Targets {
		if ts.Origin == host {
			return ts.External.Waiting
		}
	}
	return 0
}

// liveStream is a stream request in flight: the request runs in its own
// goroutine (the response headers come only with the commit), its events
// arrive on each, its response on resp.
type liveStream struct {
	resp chan *http.Response
	each chan sseEvent
	got  *http.Response
}

// openAndRead starts a stream request and reads its events until the body
// ends; it returns at once.
func openAndRead(t *testing.T, ctx context.Context, srv *httptest.Server, body string) (*liveStream, <-chan sseEvent) {
	t.Helper()
	ls := &liveStream{resp: make(chan *http.Response, 1), each: make(chan sseEvent, 64)}
	go func() {
		defer close(ls.each)
		resp, err := openStream(ctx, srv, body)
		if err != nil {
			ls.resp <- nil
			return
		}
		ls.resp <- resp
		_, _ = readEvents(resp.Body, ls.each)
		_ = resp.Body.Close()
	}()
	return ls, ls.each
}

// response waits for the response headers (nil: the request failed).
func (ls *liveStream) response(d time.Duration) *http.Response {
	if ls.got != nil {
		return ls.got
	}
	select {
	case ls.got = <-ls.resp:
	case <-time.After(d):
	}
	return ls.got
}

// Close closes the body once the response is there (no-op otherwise).
func (ls *liveStream) Close() error {
	if r := ls.response(0); r != nil {
		return r.Body.Close()
	}
	return nil
}

// waitEnter waits up to 5 s for the upstream to see the request.
func waitEnter(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the upstream never saw the request within 5 s")
	}
}

// nextData waits for the next data event (false: none within d).
func nextData(each <-chan sseEvent, d time.Duration) (sseEvent, bool) {
	timeout := time.After(d)
	for {
		select {
		case e, ok := <-each:
			if !ok {
				return sseEvent{}, false
			}
			if e.data != "" {
				return e, true
			}
		case <-timeout:
			return sseEvent{}, false
		}
	}
}

// TestStreamDisconnectPhases is G8 (D2–D4 with abort_settle_ms 0): the
// client goes away in phase A (queued), B (admitted, before any frame and
// the commit) and C (after a frame) ⇒ no waiter left, the upstream sees the
// abort and the lease is free, each within 500 ms; the reservation ends
// once (Abort without wire, Settle with). Red: the upstream call detached
// from the request (context.WithoutCancel) — the upstream runs on.
func TestStreamDisconnectPhases(t *testing.T) {
	for _, phase := range []string{"A queued", "B admitted", "C relaying"} {
		t.Run(phase, func(t *testing.T) {
			hold := make(chan struct{})
			defer close(hold)
			end := newUpstreamEnd()
			entered := make(chan struct{}, 1)
			u := newUpstream(t, "")
			steps := []sseStep{{hold: hold}}
			if phase == "C relaying" {
				steps = []sseStep{{data: sglangChunk("hello", "")}, {hold: hold}}
			}
			u.setRespond(sseRespond(steps, entered, end))
			rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 2, 1, withStreamSettings, withWait(5*time.Second))
			srv := serveHTTP(t, rig.router)
			var release func()
			if phase == "A queued" {
				release = occupyExternal(t, rig.disp, u.srv.URL)
				defer release()
			}
			ctx, cancel := context.WithCancel(context.Background())
			ls, each := openAndRead(t, ctx, srv, streamBody("chat"))
			switch phase {
			case "A queued":
				waitExternalWaiting(t, rig.disp, u.srv.URL)
			case "B admitted":
				waitEnter(t, entered)
			case "C relaying":
				if _, ok := nextData(each, 5*time.Second); !ok {
					t.Fatal("no frame")
				}
			}
			at := time.Now()
			cancel()
			_ = ls.Close()
			if phase == "A queued" {
				if !waitFor(500*time.Millisecond, func() bool { return externalWaiting(rig.disp, u.srv.URL) == 0 }) {
					t.Fatalf("a waiter is still queued 500 ms after the client left")
				}
				release()
				if u.hits.Load() != 0 || !waitFor(time.Second, func() bool { return rig.limiter.aborts.Load() == 1 }) {
					t.Errorf("hits %d aborts %d, want 0/1 (no wire, the reservation aborted)", u.hits.Load(), rig.limiter.aborts.Load())
				}
				return
			}
			if !end.wait(500*time.Millisecond) || !end.canceled {
				t.Fatalf("the upstream did not see the abort within 500 ms (ended %v, canceled %v)", !end.at.IsZero(), end.canceled)
			}
			if !waitHeld(rig.disp, u.srv.URL, 0, 500*time.Millisecond-time.Since(at)) {
				held, _ := heldOn(rig.disp, u.srv.URL)
				t.Fatalf("held %d 500 ms after the abort, want 0", held)
			}
			if !waitFor(time.Second, func() bool { return rig.limiter.settles.Load() == 1 }) || rig.limiter.aborts.Load() != 0 {
				t.Errorf("settle/abort %d/%d, want 1/0 (the abort reached the wire)", rig.limiter.settles.Load(), rig.limiter.aborts.Load())
			}
		})
	}
}

// routeGoroutines counts the goroutines that run code of the route or the
// wire (internal/oai, internal/llm): the watcher, the stream timer, the
// relay. Idle keep-alive connections of the HTTP transports (net/http only)
// are not the route's and are left out — a connection pool is no leak.
func routeGoroutines() int {
	buf := make([]byte, 1<<22)
	buf = buf[:runtime.Stack(buf, true)]
	n := 0
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, "GottZ/ctx/internal/oai.") || strings.Contains(g, "GottZ/ctx/internal/llm.") {
			n++
		}
	}
	return n
}

// TestStreamLeakProbe is G8's leak probe (masterplan gate; run with -race
// -count=3): 200 streams, each aborted at a random point (queued, admitted,
// relaying, done) ⇒ every lease released (held 0: Σacquire == Σrelease),
// every in-flight reservation back (budget 0), every limiter reservation
// ended once, no goroutine of the route left (the total count is logged).
// Σacquire may exceed Σwire here: an acquire whose run ended before the
// dial does not dial (W02-07 R4-1); the census gate G14 pins Σacquire ==
// Σwire without aborts.
func TestStreamLeakProbe(t *testing.T) {
	const n = 200
	u := newUpstream(t, "")
	u.setRespond(func(w http.ResponseWriter, r *http.Request) {
		steps := []sseStep{{data: sglangChunk("hello", "")}, {wait: time.Duration(rand.IntN(20)) * time.Millisecond, data: sglangChunk(" from", "")},
			{wait: time.Duration(rand.IntN(20)) * time.Millisecond, data: sglangChunk(" the engine", "stop")}, {data: sglangUsage}, {data: "[DONE]"}}
		sseRespond(steps, nil, nil)(w, r)
	})
	clock := &acqClock{}
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 41, 1, withStreamSettings, withAcqClock(clock))
	srv := serveHTTP(t, rig.router)
	baseTotal, baseRoute := runtime.NumGoroutine(), routeGoroutines()
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(rand.IntN(80))*time.Millisecond)
			defer cancel()
			resp, err := openStream(ctx, srv, streamBody("chat"))
			if err != nil {
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}()
	}
	wg.Wait()
	srv.Client().CloseIdleConnections()
	if !waitHeld(rig.disp, u.srv.URL, 0, 2*time.Second) {
		held, _ := heldOn(rig.disp, u.srv.URL)
		t.Fatalf("held %d after the 200 streams, want 0", held)
	}
	if !waitFor(2*time.Second, func() bool { return rig.budget.InUse() == 0 }) {
		t.Fatalf("in-flight budget %d after the streams, want 0", rig.budget.InUse())
	}
	l := rig.limiter
	if !waitFor(2*time.Second, func() bool { return l.settles.Load()+l.aborts.Load()+l.rejects.Load() == l.admits.Load() }) {
		t.Errorf("reservations: %d admitted, %d settled + %d aborted + %d rejected", l.admits.Load(), l.settles.Load(), l.aborts.Load(), l.rejects.Load())
	}
	if !waitFor(3*time.Second, func() bool { return routeGoroutines() <= baseRoute }) {
		t.Errorf("route goroutines %d after the streams, %d before", routeGoroutines(), baseRoute)
	}
	// The whole count, the gate's ±0 (Fremd-Review 1 F-3): once every
	// connection is closed — the client's, the route server's and the
	// route's own keep-alive connections to the engine — nothing may be
	// left, net/http frames included. Closed on every poll: a dial the
	// transport finished after its request was canceled lands in the idle
	// pool late (net/http keeps such a connection), and is no leak of the
	// route.
	if !waitFor(3*time.Second, func() bool {
		srv.CloseClientConnections()
		u.srv.CloseClientConnections()
		runtime.GC()
		return runtime.NumGoroutine() <= baseTotal
	}) {
		t.Errorf("goroutines %d after the streams and closed connections, %d before", runtime.NumGoroutine(), baseTotal)
	}
	acquires := len(clock.returned) + int(clock.tries.Load())
	t.Logf("200 streams: %d acquires, %d wire, %d settled, %d aborted, %d rejected; route goroutines %d → %d, all goroutines %d → %d (connections closed)",
		acquires, u.hits.Load(), l.settles.Load(), l.aborts.Load(), l.rejects.Load(), baseRoute, routeGoroutines(), baseTotal, runtime.NumGoroutine())
}

// smallBufListener shrinks the send buffer of every accepted connection, so
// a client that does not read blocks the server's writes early.
type smallBufListener struct{ net.Listener }

func (l smallBufListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetWriteBuffer(4096)
	}
	return c, err
}

// TestSlowReaderReleasesLease is G9 (B8): the engine streams >= 8 MiB, the
// client never reads (a raw TCP connection with a small receive buffer, a
// server send buffer of 4 KiB) ⇒ the write fails at the rolling write
// deadline (client_write_window), the run ends, the upstream is aborted
// and the lease is free within the window + 1 s. Red: no rolling write
// deadline (only the one at entry, grace + 30 s).
func TestSlowReaderReleasesLease(t *testing.T) {
	const window = 500 * time.Millisecond
	end := newUpstreamEnd()
	u := newUpstream(t, "")
	big := strings.Repeat("x", 32<<10)
	steps := make([]sseStep, 0, 300)
	for i := 0; i < 288; i++ { // 288 × 32 KiB = 9 MiB
		steps = append(steps, sseStep{data: sglangChunk(big, "")})
	}
	u.setRespond(sseRespond(append(steps, sseStep{data: "[DONE]"}), nil, end))
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings,
		withStream(40*time.Second, 15*time.Second, window))
	srv := httptest.NewUnstartedServer(rig.router)
	srv.Listener = smallBufListener{srv.Listener}
	srv.Start()
	t.Cleanup(srv.Close)
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.(*net.TCPConn).SetReadBuffer(4096)
	body := streamBody("chat")
	if _, err := fmt.Fprintf(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: ctx\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if !waitFor(5*time.Second, func() bool { held, _ := heldOn(rig.disp, u.srv.URL); return held == 1 }) {
		t.Fatal("the stream never held its lease")
	}
	if !waitHeld(rig.disp, u.srv.URL, 0, window+time.Second-time.Since(start)) {
		held, _ := heldOn(rig.disp, u.srv.URL)
		t.Fatalf("held %d %v after the request with a reader that never reads, want 0 within the window + 1 s", held, time.Since(start))
	}
	freed := time.Since(start)
	if !end.wait(time.Second) || !end.canceled {
		t.Errorf("the upstream was not aborted (ended %v canceled %v)", !end.at.IsZero(), end.canceled)
	}
	t.Logf("slow reader: lease free %v after the request (window %v)", freed, window)
}

// TestStreamSlowBodyFullPartitionIs429 pins the construction that keeps K7
// (Fremd-Review 1 F-1): the commit grace counts from entry and the body
// read lies inside it, so a body slower than grace_eff puts entry +
// grace_eff before the end of the queue wait — only the commit timer armed
// after the acquire returned keeps the first byte after the admission: a
// full partition still answers 429 with no SSE byte. Red: the timer armed
// when the request's frame is derived (before the queue).
func TestStreamSlowBodyFullPartitionIs429(t *testing.T) {
	fastPhaseB(t, 100*time.Millisecond)
	u := newUpstream(t, "")
	u.setRespond(sseRespond(streamShapes[0].steps(), nil, nil))
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 2, 1, withStreamSettings,
		withWait(300*time.Millisecond), withStream(50*time.Millisecond, time.Second, 5*time.Second))
	srv := serveHTTP(t, rig.router)
	defer occupyExternal(t, rig.disp, u.srv.URL)()
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	body := streamBody("chat")
	half := len(body) / 2
	if _, err := fmt.Fprintf(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: ctx\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body[:half]); err != nil {
		t.Fatal(err)
	}
	time.Sleep(600 * time.Millisecond) // the body is slower than grace_eff (400 ms)
	if _, err := conn.Write([]byte(body[half:])); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Content-Type") == "text/event-stream" {
		t.Errorf("slow body, full partition: %d %s, want 429 and no stream (no commit before the admission, K7)", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}

// TestStreamShutdown is G11 (D7): the lifecycle ends during a committed
// stream ⇒ the error frame internal_error (the shutdown's code), no
// [DONE], the lease free — and srv.Shutdown returns well inside its 15-s
// budget; before the commit the 503 internal_error (x-should-retry true).
// Red: no lifecycle binding (AfterFunc) — the stream holds srv.Shutdown
// open until its budget runs out.
func TestStreamShutdown(t *testing.T) {
	for _, committed := range []bool{true, false} {
		t.Run(map[bool]string{true: "committed", false: "before the commit"}[committed], func(t *testing.T) {
			hold := make(chan struct{})
			defer close(hold)
			entered := make(chan struct{}, 1)
			u := newUpstream(t, "")
			steps := []sseStep{{hold: hold}}
			if committed {
				steps = []sseStep{{data: sglangChunk("hello", "")}, {hold: hold}}
			}
			u.setRespond(sseRespond(steps, entered, nil))
			life, stop := context.WithCancel(context.Background())
			defer stop()
			rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings,
				func(r *chatRig) { r.deps.Lifecycle = life })
			hs := &http.Server{Handler: rig.router, ReadHeaderTimeout: 5 * time.Second}
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			go func() { _ = hs.Serve(ln) }()
			srv := &httptest.Server{URL: "http://" + ln.Addr().String(), Listener: ln, Config: hs}
			ls, each := openAndRead(t, context.Background(), srv, streamBody("chat"))
			defer func() { _ = ls.Close() }()
			if committed {
				if _, ok := nextData(each, 5*time.Second); !ok {
					t.Fatal("no frame")
				}
			} else {
				waitEnter(t, entered)
			}
			stop()
			resp := ls.response(5 * time.Second)
			if resp == nil {
				t.Fatal("no response after the shutdown")
			}
			sctx, scancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer scancel()
			start := time.Now()
			if err := hs.Shutdown(sctx); err != nil {
				t.Fatalf("srv.Shutdown: %v after %v — a request held it open", err, time.Since(start))
			}
			t.Logf("srv.Shutdown returned after %v", time.Since(start))
			var events []sseEvent
			for e := range each {
				events = append(events, e)
			}
			a := streamAnswer{status: resp.StatusCode, events: events}
			if committed {
				if code, last := a.errorFrame(t); code != oai.CodeInternalError || !last || a.dones() != 0 {
					t.Errorf("shutdown frame %q (last %v), [DONE] %d — want internal_error last, no [DONE]", code, last, a.dones())
				}
			} else if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("x-should-retry") != "true" {
				t.Errorf("before the commit: %d x-should-retry %q, want 503 true", resp.StatusCode, resp.Header.Get("x-should-retry"))
			}
			if !waitHeld(rig.disp, u.srv.URL, 0, 500*time.Millisecond) {
				t.Error("the lease is still held after the shutdown")
			}
		})
	}
}

// panicNextPlanner's acquirer panics in Next: a panic in the stream path
// after the grace commit (the first row failed, the failover acquire).
type panicNextPlanner struct{ oai.AdmissionPlanner }

func (p panicNextPlanner) Plan(ctx context.Context, ca *auth.ConsumerAuth, in oai.AdmissionInput) (oai.Plan, error) {
	plan, err := p.AdmissionPlanner.Plan(ctx, ca, in)
	if err == nil {
		plan.Acquirer = panicNext{plan.Acquirer}
	}
	return plan, err
}

type panicNext struct{ oai.RequestAcquirer }

func (panicNext) Next(context.Context, *backends.Backend, string, time.Duration) (*dispatch.Lease, context.Context, error) {
	panic("W02-08 G12 probe: a panic after the commit")
}

// TestStreamRecoverAfterCommit is G12: a panic after the commit (the grace
// commit is out, then the failover acquire panics) ⇒ the client reads an
// error (the connection is cut), never a cleanly ended body with an
// "internal server error" text appended; the lease is free. The red
// control mounts the house Recovery in place of oai.Recover: a clean end
// with the 500 text in the stream.
func TestStreamRecoverAfterCommit(t *testing.T) {
	fastPhaseB(t, 100*time.Millisecond)
	for _, red := range []bool{false, true} {
		t.Run(map[bool]string{false: "oai.Recover", true: "red control: house Recovery"}[red], func(t *testing.T) {
			u1, u2 := newUpstream(t, ""), newUpstream(t, "")
			var failedAt atomic.Int64
			u1.setRespond(status502(400*time.Millisecond, &failedAt))
			u2.setRespond(sseRespond(streamShapes[0].steps(), nil, nil))
			rig := newChatRig(t, []backends.Backend{u1.row("one", backends.TrustFull, 20), u2.row("two", backends.TrustFull, 10)}, 4, 1,
				withStreamSettings, withWait(100*time.Millisecond), withStream(0, 10*time.Second, 5*time.Second),
				func(r *chatRig) { r.deps.Planner = panicNextPlanner{r.deps.Planner} })
			h := http.Handler(rig.router)
			if red {
				h = redRecoveryMount(rig)
			}
			a := postStream(context.Background(), t, serveHTTP(t, h), streamBody("chat"))
			if a.status != http.StatusOK || a.commentsBeforeData() < 1 {
				t.Fatalf("answer %d %s, want the grace commit before the panic", a.status, a.raw)
			}
			clean := a.readErr == nil
			text := strings.Contains(strings.ToLower(a.raw), "internal server error")
			if red {
				if !clean && !text {
					t.Errorf("red control: read error %v, body %q — expected the house Recovery's clean end with its 500 text", a.readErr, a.raw)
				}
				return
			}
			if clean || text {
				t.Errorf("after the commit: read error %v, body %q — want a cut connection, no clean end, no 500 text", a.readErr, a.raw)
			}
			if !waitHeld(rig.disp, u1.srv.URL, 0, time.Second) {
				t.Error("the lease is still held after the panic")
			}
		})
	}
}

// redRecoveryMount is the rig's /v1 stack with the house Recovery where
// oai.Recover sits (G12's red control).
func redRecoveryMount(r *chatRig) http.Handler {
	rt := chi.NewRouter()
	rt.Use(handler.Recovery)
	inner := r.mount(oai.ChatCompletions(r.deps))
	rt.Mount("/", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// the rig's stack minus oai.Recover: call the route under the house
		// Recovery directly, with the rig's context values
		inner.ServeHTTP(noRecover{w}, req)
	}))
	return rt
}

// noRecover hides the hijacker so that oai.Recover, if it ran, could not
// cut the connection — the red control measures the house Recovery's end.
type noRecover struct{ http.ResponseWriter }

func (n noRecover) Flush() {
	if f, ok := n.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// TestKillSwitchEndsRunningStreams is G10 (B12): the flag hot to false
// during a committed stream ⇒ the error frame not_found within a watcher
// tick + 1 s, the lease free, the upstream aborted; the alias removed ⇒
// model_not_found. Red: no watcher — the stream runs on.
func TestKillSwitchEndsRunningStreams(t *testing.T) {
	defer oai.SetWatchEvery(50 * time.Millisecond)()
	for _, tc := range []struct {
		name string
		end  func(r *chatRig)
		code string
	}{
		{"kill switch", func(r *chatRig) { r.setSettings(func(s *oai.Settings) { s.Enabled = false }) }, oai.CodeNotFound},
		{"alias disabled", func(r *chatRig) {
			rows := chatAliases()
			rows[0].enabled = false
			r.reg.reload(t, rows)
		}, oai.CodeModelNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hold := make(chan struct{})
			defer close(hold)
			end := newUpstreamEnd()
			u := newUpstream(t, "")
			u.setRespond(sseRespond([]sseStep{{data: sglangChunk("hello", "")}, {hold: hold}}, nil, end))
			rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings)
			ls, each := openAndRead(t, context.Background(), serveHTTP(t, rig.router), streamBody("chat"))
			defer func() { _ = ls.Close() }()
			if _, ok := nextData(each, 5*time.Second); !ok {
				t.Fatal("no frame")
			}
			at := time.Now()
			tc.end(rig)
			e, ok := nextData(each, 50*time.Millisecond+time.Second)
			if !ok || !strings.Contains(e.data, `"code":"`+tc.code+`"`) {
				t.Fatalf("frame %q within a tick + 1 s, want the %s error frame", e.data, tc.code)
			}
			if _, more := nextData(each, 200*time.Millisecond); more {
				t.Error("a data event after the error frame")
			}
			if !end.wait(time.Second) || !end.canceled || !waitHeld(rig.disp, u.srv.URL, 0, time.Second) {
				t.Errorf("upstream aborted %v, lease free: %v", end.canceled, waitHeld(rig.disp, u.srv.URL, 0, 0))
			}
			if n := consecutiveFails(rig.pool, "spark"); n != 0 {
				t.Errorf("consecutive_fails %d, want 0 (Z3)", n)
			}
			// The row is written after the frame went out: wait for it.
			waitFor(time.Second, func() bool { return len(rig.rows.all()) == 1 })
			if rows := rig.rows.all(); len(rows) != 1 || rows[0].Attempts[0].Class != backends.ClassCanceled.String() {
				t.Errorf("rows %+v, want one row whose attempt is canceled (Z3)", rows)
			}
			t.Logf("%s: frame %v after the change", tc.name, e.at.Sub(at))
		})
	}
}

// TestStreamHasNoRequestDeadline is Z1 (RC-C Index F3, lead decision 2): a
// stream's acquire carries ServeBy zero and its context no deadline — a
// stream longer than gateway.request_deadline runs to its end — while a
// non-stream acquire carries ServeBy != 0. A stream with Ctx-Max-Wait-Ms: 0
// gets ServeBy = its entry (serve now or refuse, no wait). Red: the
// deadline for every request (W02-07's) — ServeBy set and the stream cut at
// the deadline.
func TestStreamHasNoRequestDeadline(t *testing.T) {
	u := newUpstream(t, "")
	steps := []sseStep{{data: sglangChunk("hello", "")}, {wait: 400 * time.Millisecond, data: sglangChunk(" from", "")},
		{wait: 400 * time.Millisecond, data: sglangChunk(" the engine", "stop")}, {data: "[DONE]"}}
	u.setRespond(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.Unmarshal(u.raws[len(u.raws)-1], &body)
		if body["stream"] != true {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, answerSGLang)
			return
		}
		sseRespond(steps, nil, nil)(w, r)
	})
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings,
		func(r *chatRig) {
			s := *r.settings.Load()
			s.RequestDeadline = 300 * time.Millisecond
			r.settings.Store(&s)
		})
	srv := serveHTTP(t, rig.router)
	a := postStream(context.Background(), t, srv, streamBody("chat"))
	checkRelay(t, a)
	if d := a.end.Sub(a.start); d < 700*time.Millisecond {
		t.Errorf("the stream took %v, the fixture runs 800 ms", d)
	}
	if na := rig.post(t, chatBody("chat")); na.status != http.StatusOK {
		t.Fatalf("non-stream: %d %s", na.status, na.body)
	}
	sb := rig.serveBy.all()
	if len(sb) != 2 || !sb[0].IsZero() || sb[1].IsZero() {
		t.Fatalf("ServeBy of the acquires %v, want [zero (stream), set (non-stream)]", sb)
	}
	before := time.Now()
	z := postStream(context.Background(), t, srv, streamBody("chat"), "Ctx-Max-Wait-Ms", "0")
	sb = rig.serveBy.all()
	if len(sb) != 3 {
		t.Fatalf("%d acquires, want 3", len(sb))
	}
	t.Logf("stream with Ctx-Max-Wait-Ms 0: status %d, ServeBy %v after entry", z.status, sb[2].Sub(before))
	if !sb[2].IsZero() && (sb[2].Before(before) || sb[2].Sub(before) > time.Second) {
		t.Errorf("Ctx-Max-Wait-Ms 0: ServeBy %v, want the entry (now)", sb[2])
	}
}

// TestStreamWriterDomain is Z2 (Fremd-Review 1 F-1 of W02-07, A03-W8
// review 1 F-1): (i) a shutdown before the plan and a plan refusal answer
// the 503 envelope, never an empty 200; (ii) an upstream timeout before
// the commit is a 504 upstream_timeout, after it the error frame
// upstream_timeout; (iii) after the commit no second WriteHeader. Red: (i)
// without H-1's entry check; (ii) attempt errors to the admission writer;
// (iii) a refusal after the commit written as a response.
func TestStreamWriterDomain(t *testing.T) {
	t.Run("shutdown before the plan", func(t *testing.T) {
		u := newUpstream(t, "")
		u.setRespond(sseRespond(streamShapes[0].steps(), nil, nil))
		life, stop := context.WithCancel(context.Background())
		stop()
		rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings,
			func(r *chatRig) { r.deps.Lifecycle = life })
		// H-1 for the stream: answered before the parse — 500 requests, no
		// limiter reservation, no wire contact, every answer the 503.
		for i := 0; i < 500; i++ {
			a := rig.post(t, streamBody("chat"))
			code, _, _ := a.envelope(t)
			if a.status != http.StatusServiceUnavailable || code != oai.CodeInternalError {
				t.Fatalf("shutdown at entry, request %d: %d %s — want 503 internal_error", i, a.status, a.body)
			}
		}
		if u.hits.Load() != 0 || rig.limiter.admits.Load() != 0 {
			t.Fatalf("wire hits %d, limiter admits %d — want 0/0 (the run ended at entry reaches neither)", u.hits.Load(), rig.limiter.admits.Load())
		}
	})
	t.Run("shutdown in the writer window", func(t *testing.T) {
		// The shutdown lands between the plan refusal and the writer, which
		// is silent on the canceled request context (W02-07 F-1, H-1): the
		// route answers the shutdown itself — never an empty 200.
		u := newUpstream(t, "")
		life, stop := context.WithCancel(context.Background())
		defer stop()
		var calls atomic.Int64
		rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 2, 1, withStreamSettings, func(r *chatRig) {
			notOffered(r)
			r.deps.Lifecycle = life
			r.deps.Planner = shWindowPlanner{AdmissionPlanner: r.planner, stop: stop, calls: &calls}
		})
		a := rig.post(t, streamBody("chat"))
		if calls.Load() != 1 {
			t.Fatalf("writer calls %d, want 1", calls.Load())
		}
		wantShutdownAnswer(t, "stream writer window", a)
	})
	t.Run("plan refusal", func(t *testing.T) {
		u := newUpstream(t, "")
		rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings, notOffered)
		a := rig.post(t, streamBody("chat"))
		code, _, _ := a.envelope(t)
		if a.status != http.StatusServiceUnavailable || code != oai.CodeModelUnavailable {
			t.Fatalf("plan refusal: %d %s, want 503 model_unavailable", a.status, a.body)
		}
	})
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprintf("upstream timeout committed=%v", committed), func(t *testing.T) {
			hold := make(chan struct{})
			defer close(hold)
			u := newUpstream(t, "")
			steps := []sseStep{{hold: hold}}
			if committed {
				steps = []sseStep{{data: sglangChunk("hello", "")}, {hold: hold}}
			}
			u.setRespond(sseRespond(steps, nil, nil))
			rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings,
				func(r *chatRig) { r.reg.reload(t, chSlowAliases()) })
			var bc byteClock
			a := postStream(context.Background(), t, serveHTTP(t, bc.wrap(rig.router)), streamBody("slow"))
			if committed {
				code, last := a.errorFrame(t)
				if a.status != http.StatusOK || code != oai.CodeUpstreamTimeout || !last || a.dones() != 0 {
					t.Errorf("after the commit: %d frame %q last %v: %s — want the upstream_timeout frame", a.status, code, last, a.raw)
				}
			} else {
				var env struct{ Error struct{ Code string } }
				_ = json.Unmarshal([]byte(a.raw), &env)
				if a.status != http.StatusGatewayTimeout || env.Error.Code != oai.CodeUpstreamTimeout {
					t.Errorf("before the commit: %d %s, want 504 upstream_timeout", a.status, a.raw)
				}
			}
			if n := bc.headers.Load(); n != 1 {
				t.Errorf("WriteHeader called %d times, want 1 (no second status after the commit)", n)
			}
		})
	}
}

// TestStreamReservationEndsOnce is Z5 (W02-07 review 1 F-7 LM1): every end
// of a stream ends the limiter's reservation exactly once — Abort without
// wire contact (queued abort), Settle with partial or estimated usage after
// it (admitted abort, error frame, success), Reject on an admission refusal.
// Red: no Settle on the abort path (close() aborts instead).
func TestStreamReservationEndsOnce(t *testing.T) {
	type ends struct{ settle, abort, reject int64 }
	run := func(t *testing.T, name string, want ends, drive func(t *testing.T) *fakeLimiter) {
		t.Run(name, func(t *testing.T) {
			l := drive(t)
			if !waitFor(2*time.Second, func() bool { return l.settles.Load()+l.aborts.Load()+l.rejects.Load() >= 1 }) {
				t.Fatal("the reservation never ended")
			}
			time.Sleep(50 * time.Millisecond)
			got := ends{l.settles.Load(), l.aborts.Load(), l.rejects.Load()}
			if got != want || l.admits.Load() != 1 {
				t.Errorf("admits %d, settle/abort/reject %+v, want 1 and %+v", l.admits.Load(), got, want)
			}
		})
	}
	run(t, "success", ends{1, 0, 0}, func(t *testing.T) *fakeLimiter {
		u := newUpstream(t, "")
		u.setRespond(sseRespond(streamShapes[0].steps(), nil, nil))
		rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings)
		postStream(context.Background(), t, serveHTTP(t, rig.router), streamBody("chat"))
		return rig.limiter
	})
	run(t, "error frame", ends{1, 0, 0}, func(t *testing.T) *fakeLimiter {
		u := newUpstream(t, "")
		u.setRespond(sseRespond([]sseStep{{data: sglangChunk("hello", "")}, {data: `{"error":{"message":"x","code":"internal"}}`}}, nil, nil))
		rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings)
		postStream(context.Background(), t, serveHTTP(t, rig.router), streamBody("chat"))
		return rig.limiter
	})
	run(t, "admitted abort", ends{1, 0, 0}, func(t *testing.T) *fakeLimiter {
		hold := make(chan struct{})
		t.Cleanup(func() { close(hold) })
		entered := make(chan struct{}, 1)
		u := newUpstream(t, "")
		u.setRespond(sseRespond([]sseStep{{hold: hold}}, entered, nil))
		rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings)
		ctx, cancel := context.WithCancel(context.Background())
		ls, _ := openAndRead(t, ctx, serveHTTP(t, rig.router), streamBody("chat"))
		waitEnter(t, entered)
		cancel()
		_ = ls.Close()
		return rig.limiter
	})
	run(t, "queued abort", ends{0, 1, 0}, func(t *testing.T) *fakeLimiter {
		u := newUpstream(t, "")
		rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 2, 1, withStreamSettings)
		t.Cleanup(occupyExternal(t, rig.disp, u.srv.URL))
		ctx, cancel := context.WithCancel(context.Background())
		ls, _ := openAndRead(t, ctx, serveHTTP(t, rig.router), streamBody("chat"))
		waitExternalWaiting(t, rig.disp, u.srv.URL)
		cancel()
		_ = ls.Close()
		return rig.limiter
	})
	run(t, "admission refusal", ends{0, 0, 1}, func(t *testing.T) *fakeLimiter {
		u := newUpstream(t, "")
		rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 2, 1, withStreamSettings, withWait(50*time.Millisecond))
		t.Cleanup(occupyExternal(t, rig.disp, u.srv.URL))
		if a := postStream(context.Background(), t, serveHTTP(t, rig.router), streamBody("chat")); a.status != http.StatusTooManyRequests {
			t.Fatalf("answer %d, want 429", a.status)
		}
		return rig.limiter
	})
	_ = llmlog.FinishAborted
}
