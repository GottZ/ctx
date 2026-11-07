package oai_test

// A03-W10 gates, relay half (design/03 §4.10 no. 1, §7 A03-W10 (b); design/02
// §4.11.3 D4): the lease of a /v1 stream ends at the upstream's end, never at
// the client's drain — the upstream reader puts every rewritten chunk into a
// bounded buffer, the client writer drains it on its own goroutine. A client
// that falls behind by more than the buffer is cut with an error frame (the
// run ends: the upstream is aborted, the lease settles); one that drains
// slower than gateway.stream_min_drain_rate over the window is cut.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/oai"
)

// withRelay sets the three relay keys of the rig's settings.
func withRelay(buffer, rate int64, window time.Duration) rigOption {
	return func(r *chatRig) {
		s := *r.settings.Load()
		s.StreamRelayBufferBytes, s.StreamMinDrainRate, s.StreamMinDrainWindow = buffer, rate, window
		r.settings.Store(&s)
	}
}

// withFrameCap sets gateway.max_stream_frame_bytes.
func withFrameCap(n int64) rigOption {
	return func(r *chatRig) {
		s := *r.settings.Load()
		s.MaxStreamFrameBytes = n
		r.settings.Store(&s)
	}
}

// withSettle gives every target of the rig (slots, reserved as passed to
// newChatRig) abort_settle_ms {stream, nonstream}.
func withSettle(slots, reserved int, stream, nonstream time.Duration) rigOption {
	return func(r *chatRig) {
		pol := dispatch.Policy{Targets: map[string]dispatch.TargetPolicy{}}
		for _, b := range r.pool.Snapshot() {
			pol.Targets[b.Host] = dispatch.TargetPolicy{Slots: slots, InternalReserved: reserved,
				AbortSettleStream: stream, AbortSettleNonstream: nonstream}
		}
		r.disp.UpdatePolicy(pol)
	}
}

// bigFrames is a stream of n content chunks of size bytes each, then the
// usage chunk and [DONE]; gap is the upstream's pause before each chunk.
func bigFrames(n, size int, gap time.Duration) []sseStep {
	big := strings.Repeat("x", size)
	steps := make([]sseStep, 0, n+2)
	for i := 0; i < n; i++ {
		steps = append(steps, sseStep{wait: gap, data: sglangChunk(big, "")})
	}
	return append(steps, sseStep{data: sglangUsage}, sseStep{data: "[DONE]"})
}

// slowClient is a raw HTTP/1.1 client with a 4 KiB receive buffer against a
// route server with a 4 KiB send buffer (smallBufListener): what the client
// does not read stays in the route's writes, not in kernel buffers.
type slowClient struct {
	conn net.Conn
	br   *bufio.Reader
	resp *http.Response
	body *bufio.Reader // over resp.Body, 4 KiB: what the client reads ahead stays small
	srv  *httptest.Server
}

func startSlowClient(t *testing.T, rig *chatRig) *slowClient {
	t.Helper()
	srv := httptest.NewUnstartedServer(rig.router)
	srv.Listener = smallBufListener{srv.Listener}
	srv.Start()
	t.Cleanup(srv.Close)
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.(*net.TCPConn).SetReadBuffer(4096)
	body := streamBody("chat")
	if _, err := fmt.Fprintf(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: ctx\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Minute))
	br := bufio.NewReaderSize(conn, 4096)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("response head: %v", err)
	}
	return &slowClient{conn: conn, br: br, resp: resp, body: bufio.NewReaderSize(resp.Body, 4096), srv: srv}
}

// event reads the next SSE event's data payload ("" for a comment), io.EOF at
// the end of the body.
func (c *slowClient) event() (string, error) {
	var data strings.Builder
	sawData := false
	for {
		line, err := c.body.ReadString('\n')
		if err != nil {
			return "", err
		}
		line = strings.TrimSuffix(line, "\n")
		switch {
		case line == "":
			if sawData {
				return data.String(), nil
			}
		case strings.HasPrefix(line, "data: "):
			sawData = true
			data.WriteString(strings.TrimPrefix(line, "data: "))
		}
	}
}

// slowReaderCase is gate (b) in one scale.
type slowReaderCase struct {
	frames   int           // content chunks of 32 KiB
	gap      time.Duration // upstream pause before each chunk
	pause    time.Duration // client pause after each event
	heldBy   time.Duration // after the request: the lease must be free by then
	minDrain time.Duration // the client drain lasts at least this (the slow-reader regime)
	heldAlso time.Duration // on a red run: also report held at this time after the request (0: no)
	window   time.Duration // client_write_window (0: the rig's 30 s)
	rate     int64         // gateway.stream_min_drain_rate (0: the rig's 1 KiB/s)
}

// TestRelaySlowReaderReleasesAtTheUpstreamEnd is gate (b), the shared gate of
// W02-08 and A03-W10 (design/03 §7 A03-W10 (b)): the upstream delivers its
// whole stream fast, the client reads one frame per pause. The lease is free
// within 2.5 s of the upstream's end — long before the client has drained —
// and the client still gets every frame and [DONE]. Red against the W02-08
// relay (writes in the upstream callback): the lease is held until the
// client has drained. Scaled by default (5 × 32 KiB, 200 ms per frame);
// CTX_A03W10_REALTIME=1 runs the design's numbers (upstream in 2 s, one frame
// per 25 s — 6 frames, so the drain outlasts 60 s against loopback buffers —
// the lease reported at 60 s for the red side; client_write_window 120 s, so
// D4 does not cut the client on either side).
func TestRelaySlowReaderReleasesAtTheUpstreamEnd(t *testing.T) {
	tc := slowReaderCase{frames: 5, gap: 0, pause: 200 * time.Millisecond, heldBy: 700 * time.Millisecond, minDrain: 800 * time.Millisecond}
	if os.Getenv("CTX_A03W10_REALTIME") == "1" {
		tc = slowReaderCase{frames: 6, gap: 333 * time.Millisecond, pause: 25 * time.Second, heldBy: 4500 * time.Millisecond, minDrain: 120 * time.Second, heldAlso: 60 * time.Second,
			window: 120 * time.Second, rate: 512}
	}
	end := newUpstreamEnd()
	u := newUpstream(t, "")
	u.setRespond(sseRespond(bigFrames(tc.frames, 32<<10, tc.gap), nil, end))
	row := u.row("spark", backends.TrustFull, 10)
	row.Timeouts = map[string]int{"chat": 600} // the drain lasts the attempt's timeout at most
	opts := []rigOption{withStreamSettings}
	if tc.window > 0 {
		// A 32 KiB frame read through 4 KiB buffers takes seconds under host
		// load: one frame per 25 s would meet the 30 s write window (D4, a
		// different gate) on both sides.
		opts = append(opts, withStream(40*time.Second, 15*time.Second, tc.window))
	}
	if tc.rate > 0 {
		// Under host load a 32 KiB frame read through 4 KiB buffers takes
		// seconds: one frame per 25 s plus the read measured 0.94 KiB/s,
		// below the 1 KiB/s minimum — the drain rule (its own gate) would cut
		// the client; this gate is the lease's end and the delivery.
		opts = append(opts, withRelay(2<<20, tc.rate, 30*time.Second))
	}
	rig := newChatRig(t, []backends.Backend{row}, 4, 1, opts...)
	start := time.Now()
	c := startSlowClient(t, rig)
	if c.resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", c.resp.StatusCode)
	}
	type result struct {
		frames int
		done   bool
		err    error
		at     time.Time
	}
	res := make(chan result, 1)
	go func() {
		var r result
		for {
			data, err := c.event()
			if err != nil {
				r.err = err
				break
			}
			if data == "[DONE]" {
				r.done = true
				break
			}
			if strings.Contains(data, `"content":"xxx`) {
				r.frames++
			}
			time.Sleep(tc.pause)
		}
		r.at = time.Now()
		res <- r
	}()
	if !waitFor(tc.heldBy, func() bool { h, _ := heldOn(rig.disp, u.srv.URL); return h == 0 && !end.at.IsZero() }) {
		held, _ := heldOn(rig.disp, u.srv.URL)
		if tc.heldAlso > 0 {
			// The design's red side (realtime): the lease is still held at ≥ 60 s.
			time.Sleep(time.Until(start.Add(tc.heldAlso)))
			later, _ := heldOn(rig.disp, u.srv.URL)
			t.Logf("held %d at %v after the request", later, time.Since(start))
		}
		t.Fatalf("held %d at %v after the request (upstream ended: %v) — want 0 by %v, within 2.5 s of the upstream's end while the client drains",
			held, time.Since(start), !end.at.IsZero(), tc.heldBy)
	}
	freed := time.Since(start)
	t.Logf("lease free %v after the request, upstream ended %v after it", freed, end.at.Sub(start))
	if end.canceled {
		t.Fatal("the upstream was aborted; the relay must read it to its end")
	}
	r := <-res
	if !r.done || r.frames != tc.frames {
		t.Fatalf("client got %d frames, [DONE] %v, err %v — want all %d frames and [DONE]", r.frames, r.done, r.err, tc.frames)
	}
	if drain := r.at.Sub(start); drain < tc.minDrain {
		t.Fatalf("client drained in %v, want ≥ %v (else the gate is not in the slow-reader regime)", drain, tc.minDrain)
	}
}

// TestRelayOverflowCutsAndSettles is gate (b)'s overflow half: a 64 KiB
// relay buffer, the upstream writes 32 × 32 KiB fast, the client reads
// slowly — the buffer overflows: the upstream is aborted, the lease settles
// for abort_settle_ms.stream (400 ms) and is then free, and the client's
// stream ends with an error frame server_overloaded, never [DONE].
func TestRelayOverflowCutsAndSettles(t *testing.T) {
	end := newUpstreamEnd()
	u := newUpstream(t, "")
	// 10 ms between the chunks: the upstream is still writing when the
	// buffer overflows (loopback buffers would swallow 1 MiB at once and
	// hide the abort).
	u.setRespond(sseRespond(bigFrames(32, 32<<10, 10*time.Millisecond), nil, end))
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings,
		withFrameCap(48<<10), withRelay(64<<10, 1024, 30*time.Second), withSettle(4, 1, 400*time.Millisecond, 0))
	c := startSlowClient(t, rig)
	if !end.wait(5 * time.Second) {
		t.Fatal("the upstream never ended")
	}
	aborted := end.at
	if !end.canceled {
		t.Fatal("the upstream ran to its end; an overflow must abort it")
	}
	time.Sleep(150 * time.Millisecond)
	if held, _ := heldOn(rig.disp, u.srv.URL); held != 1 {
		t.Fatalf("held %d 150 ms after the overflow, want 1 (settling for abort_settle_ms.stream 400 ms)", held)
	}
	if !waitHeld(rig.disp, u.srv.URL, 0, 2*time.Second) {
		t.Fatal("the lease never ended after the settle")
	}
	if settled := time.Since(aborted); settled < 380*time.Millisecond {
		t.Fatalf("lease free %v after the abort, want ≥ the 400 ms settle", settled)
	}
	var last string
	frames := 0
	for {
		data, err := c.event()
		if err != nil {
			break
		}
		if data == "[DONE]" {
			t.Fatal("[DONE] after an overflow")
		}
		frames++
		last = data
	}
	if !strings.Contains(last, `"code":"server_overloaded"`) {
		t.Fatalf("last event %.120q after %d frames, want the error frame server_overloaded", last, frames)
	}
	if frames >= 32 {
		t.Fatalf("client got %d frames, want fewer than the 32 the upstream wrote", frames)
	}
}

// TestRelayMinDrainRateCuts is gate (b)'s drain half: the upstream's 1 MiB
// fits the buffer and ends at once (the lease ends with it); the client
// drains 2 KiB per 100 ms, below gateway.stream_min_drain_rate (64 KiB/s)
// over the window (300 ms) — the stream is cut long before it could end:
// no [DONE], the connection closed.
func TestRelayMinDrainRateCuts(t *testing.T) {
	end := newUpstreamEnd()
	u := newUpstream(t, "")
	u.setRespond(sseRespond(bigFrames(32, 32<<10, 0), nil, end))
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings,
		withRelay(2<<20, 64<<10, 300*time.Millisecond))
	start := time.Now()
	c := startSlowClient(t, rig)
	if !end.wait(5*time.Second) || end.canceled {
		t.Fatalf("the upstream did not run to its end (canceled %v)", end.canceled)
	}
	if !waitHeld(rig.disp, u.srv.URL, 0, 2500*time.Millisecond) {
		t.Fatal("the lease outlived the upstream's end by 2.5 s while the client drains")
	}
	buf := make([]byte, 2<<10)
	got, done := 0, false
	for {
		n, err := c.body.Read(buf)
		got += n
		if strings.Contains(string(buf[:n]), "[DONE]") {
			done = true
		}
		if err != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
		if time.Since(start) > 20*time.Second {
			t.Fatalf("still reading after 20 s (%d bytes) — the drain rate never cut the stream", got)
		}
	}
	if done {
		t.Fatalf("[DONE] reached a client draining below the minimum rate (%d bytes)", got)
	}
	if got >= 32*32<<10 {
		t.Fatalf("client got %d bytes, want fewer than the stream (cut early)", got)
	}
	t.Logf("cut after %v, %d bytes drained", time.Since(start), got)
}

// TestStreamClientAbortSettles is gate (a) through the route (Request.Stream
// end to end): a client that leaves a running stream ends the run; the lease
// stays settling for abort_settle_ms.stream (500 ms) — the nonstream field is
// 0, so only a lease that knows it is a stream settles — and is then free.
func TestStreamClientAbortSettles(t *testing.T) {
	end := newUpstreamEnd()
	u := newUpstream(t, "")
	steps := []sseStep{{data: sglangChunk("one", "")}, {data: sglangChunk("two", "")}, {wait: 30 * time.Second, data: sglangChunk("late", "stop")}, {data: "[DONE]"}}
	u.setRespond(sseRespond(steps, nil, end))
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings,
		withSettle(4, 1, 500*time.Millisecond, 0))
	srv := serveHTTP(t, rig.router)
	ctx, cancel := context.WithCancel(context.Background())
	resp, err := openStream(ctx, srv, streamBody("chat"))
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(resp.Body)
	for seen := 0; seen < 2; {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if strings.Contains(line, `"content":"`) {
			seen++
		}
	}
	cancel()
	_ = resp.Body.Close()
	if !end.wait(2*time.Second) || !end.canceled {
		t.Fatal("the upstream was not aborted after the client left")
	}
	aborted := end.at
	time.Sleep(200 * time.Millisecond)
	if held, _ := heldOn(rig.disp, u.srv.URL); held != 1 {
		t.Fatalf("held %d 200 ms after the client left, want 1 (settling for abort_settle_ms.stream 500 ms)", held)
	}
	if !waitHeld(rig.disp, u.srv.URL, 0, 2*time.Second) {
		t.Fatal("the lease never ended after the settle")
	}
	if got := time.Since(aborted); got < 480*time.Millisecond {
		t.Fatalf("lease free %v after the abort, want ≥ 500 ms", got)
	}
}

// TestRelaySlotEndsWithTheDrain (Fremd-Review 2 F3b; lead decision of
// hardening 1: the consumer's L1 slot ends at the drain's end, the lease
// still at the upstream's end — the lease measures engine capacity, the
// slot what a consumer binds on the instance: the buffer in the in-flight
// budget, a socket, the writer). A consumer with max_concurrency k opens k
// streams whose upstreams run to their end while the clients read nothing
// more: each lease is free within 2.5 s of its upstream's end, but no
// reservation ends — every further stream of the consumer is 429
// concurrency_limit_exceeded, so the budget its draining streams hold stays
// within k × (buffer + stream reserve + request share), and a consumer of
// another tenant is served (200). When one client has read its stream to
// [DONE], its reservation ends — exactly once. Red against d22c8423 (the
// slot ended with the lease): the reservation ends at the upstream's end,
// the next stream of the consumer is 200 — 63 such streams fill the
// production budget and every other consumer gets 503 (the review's probe).
func TestRelaySlotEndsWithTheDrain(t *testing.T) {
	for _, k := range []int{1, 2} {
		t.Run(fmt.Sprintf("max_concurrency %d", k), func(t *testing.T) {
			var ended atomic.Int64
			u := newUpstream(t, "")
			inner := sseRespond(bigFrames(5, 32<<10, 0), nil, nil)
			u.setRespond(func(w http.ResponseWriter, r *http.Request) { inner(w, r); ended.Add(1) })
			lim := l1One
			lim.Concurrency = int64(k)
			m := &endMeter{rigMeter: newRigMeter(lim, true)}
			rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings,
				withDeps(func(d *oai.Deps) { d.Limiter = m }))
			const budget = 268435456 // gateway.inflight_budget's default
			rig.setSettings(func(s *oai.Settings) { s.InflightBudget = budget })
			s := *rig.settings.Load()
			body := streamBody("chat")
			perStream := max(s.StreamRelayBufferBytes, s.MaxStreamFrameBytes+oai.RelayRecordHeader) + 2*s.MaxStreamFrameBytes + 3*int64(len(body)) + 4<<10

			clients := make([]*slowClient, 0, k)
			for i := range k {
				c := startSlowClient(t, rig)
				if c.resp.StatusCode != http.StatusOK {
					t.Fatalf("stream %d of the consumer: status %d", i, c.resp.StatusCode)
				}
				if _, err := c.event(); err != nil {
					t.Fatalf("stream %d: %v", i, err)
				}
				clients = append(clients, c)
				if !waitFor(5*time.Second, func() bool { return ended.Load() == int64(i+1) }) {
					t.Fatalf("stream %d: the upstream did not run to its end", i)
				}
				if !waitHeld(rig.disp, u.srv.URL, 0, 2500*time.Millisecond) {
					held, _ := heldOn(rig.disp, u.srv.URL)
					t.Fatalf("stream %d: lease held %d 2.5 s after its upstream's end — the lease ends with the upstream", i, held)
				}
			}
			time.Sleep(50 * time.Millisecond)
			if got := endCount(m); got != 0 {
				t.Fatalf("ends %q while %d clients drain, want none — the slot ends at the drain's end", m.endsOf(), k)
			}
			for j := range 3 {
				a := postStream(context.Background(), t, clients[0].srv, body)
				if a.status == http.StatusOK {
					t.Fatalf("stream %d of the consumer while %d drain: 200 — want 429 concurrency_limit_exceeded (a draining stream holds its slot); budget in use %d B",
						k+j, k, rig.budget.InUse())
				}
				checkL1Refusal(t, fmt.Sprintf("stream %d of the consumer while %d drain", k+j, k), a)
			}
			inUse := rig.budget.InUse()
			t.Logf("max_concurrency %d: %d draining streams hold %d B of %d (bound %d B = k × %d); ends %q",
				k, k, inUse, budget, int64(k)*perStream, perStream, m.endsOf())
			if inUse > int64(k)*perStream {
				t.Fatalf("budget in use %d B with %d draining streams, want ≤ %d (k × buffer + stream reserve + request share)", inUse, k, int64(k)*perStream)
			}

			other := urgentApp()
			other.ConsumerID, other.TenantID, other.ModelKeyID = "00000000-0000-4000-8000-0000000000c9", "00000000-0000-4000-8000-0000000000f9", "00000000-0000-4000-8000-0000000000e9"
			rig.ca.Store(other)
			b := postStream(context.Background(), t, clients[0].srv, body)
			if b.status != http.StatusOK || b.dones() != 1 {
				t.Fatalf("consumer of another tenant while %d streams drain: %d dones %d %q — want 200", k, b.status, b.dones(), b.raw)
			}
			if !waitFor(2*time.Second, func() bool { return endCount(m) == 1 }) {
				t.Fatalf("ends %q after the other consumer's stream, want its one end", m.endsOf())
			}

			frames := 1
			for {
				data, err := clients[0].event()
				if err != nil {
					t.Fatalf("first stream after %d frames: %v", frames, err)
				}
				if data == "[DONE]" {
					break
				}
				frames++
			}
			if frames != 5 {
				t.Fatalf("first stream: %d frames, want its 5 content chunks", frames)
			}
			if !waitFor(2*time.Second, func() bool { return endCount(m) == 2 }) {
				t.Fatalf("ends %q after the first client read [DONE], want its one end at the drain's end", m.endsOf())
			}
			for _, c := range clients {
				_ = c.conn.Close()
			}
			if !waitFor(5*time.Second, func() bool { return endCount(m) == k+1 && rig.budget.InUse() == 0 }) {
				t.Fatalf("after the clients left: ends %q, budget in use %d B — want %d ends, 0 B", m.endsOf(), rig.budget.InUse(), k+1)
			}
			time.Sleep(50 * time.Millisecond)
			if got := endCount(m); got != k+1 {
				t.Fatalf("ends %q, want exactly %d — one end per exit", m.endsOf(), k+1)
			}
		})
	}
}

// TestRelayLeakProbe is the race/leak gate of the relay half: 1 000 streams,
// 50 at a time, with a random upstream (3–20 chunks of 100 B–12 KiB, gaps of
// 0–3 ms), a relay buffer of 32 KiB (overflows happen) and a random client —
// one that leaves after 0–30 ms, one that reads at once, one that reads
// 1 KiB per 0–3 ms. After the run every lease ended exactly once (held 0,
// settling 0, admits = settle + keep + abort + reject), the budget is 0, the
// route's goroutines — readers, writers, settle timers — are back to the
// baseline, every goroutine is gone once the connections are closed, and the
// heap's peak stays within 50 × the buffer + a base.
func TestRelayLeakProbe(t *testing.T) {
	const n, wave = 1000, 50
	u := newUpstream(t, "")
	u.setRespond(func(w http.ResponseWriter, r *http.Request) {
		k := 3 + rand.IntN(18)
		steps := make([]sseStep, 0, k+2)
		for i := 0; i < k; i++ {
			steps = append(steps, sseStep{wait: time.Duration(rand.IntN(3)) * time.Millisecond,
				data: sglangChunk(strings.Repeat("y", 100+rand.IntN(12<<10)), "")})
		}
		sseRespond(append(steps, sseStep{data: sglangUsage}, sseStep{data: "[DONE]"}), nil, nil)(w, r)
	})
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 64, 1, withStreamSettings,
		withFrameCap(16<<10), withRelay(32<<10, 1024, 30*time.Second), withSettle(64, 1, 5*time.Millisecond, 5*time.Millisecond))
	srv := serveHTTP(t, rig.router)
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	baseTotal, baseRoute := runtime.NumGoroutine(), routeGoroutines()
	var peak atomic.Uint64
	stopSample := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		var ms runtime.MemStats
		for {
			select {
			case <-stopSample:
				return
			case <-time.After(5 * time.Millisecond):
			}
			runtime.ReadMemStats(&ms)
			if ms.HeapInuse > peak.Load() {
				peak.Store(ms.HeapInuse)
			}
		}
	}()
	for done := 0; done < n; done += wave {
		var wg sync.WaitGroup
		for i := 0; i < wave; i++ {
			wg.Add(1)
			go func(kind int) {
				defer wg.Done()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if kind == 0 {
					var stop context.CancelFunc
					ctx, stop = context.WithTimeout(ctx, time.Duration(rand.IntN(30))*time.Millisecond)
					defer stop()
				}
				resp, err := openStream(ctx, srv, streamBody("chat"))
				if err != nil {
					return
				}
				defer func() { _ = resp.Body.Close() }()
				if kind != 2 {
					_, _ = io.Copy(io.Discard, resp.Body)
					return
				}
				buf := make([]byte, 1<<10)
				for {
					if _, err := resp.Body.Read(buf); err != nil {
						return
					}
					time.Sleep(time.Duration(rand.IntN(3)) * time.Millisecond)
				}
			}(rand.IntN(3))
		}
		wg.Wait()
	}
	close(stopSample)
	<-sampled
	srv.Client().CloseIdleConnections()
	if !waitHeld(rig.disp, u.srv.URL, 0, 3*time.Second) {
		held, _ := heldOn(rig.disp, u.srv.URL)
		t.Fatalf("held %d after the %d streams, want 0", held, n)
	}
	for _, ts := range rig.disp.Snapshot().Targets {
		if ts.Settling != 0 || ts.Inflight != 0 {
			t.Fatalf("target %s: settling %d inflight %d after the run, want 0/0", ts.Origin, ts.Settling, ts.Inflight)
		}
	}
	if !waitFor(2*time.Second, func() bool { return rig.budget.InUse() == 0 }) {
		t.Fatalf("in-flight budget %d after the streams, want 0", rig.budget.InUse())
	}
	l := rig.limiter
	if !waitFor(2*time.Second, func() bool {
		return l.settles.Load()+l.keeps.Load()+l.aborts.Load()+l.rejects.Load() == l.admits.Load()
	}) {
		t.Errorf("reservations: %d admitted, %d settled + %d kept + %d aborted + %d rejected", l.admits.Load(), l.settles.Load(), l.keeps.Load(),
			l.aborts.Load(), l.rejects.Load())
	}
	if !waitFor(3*time.Second, func() bool { return routeGoroutines() <= baseRoute }) {
		t.Errorf("route goroutines %d after the streams, %d before", routeGoroutines(), baseRoute)
	}
	if !waitFor(3*time.Second, func() bool {
		srv.CloseClientConnections()
		u.srv.CloseClientConnections()
		runtime.GC()
		return runtime.NumGoroutine() <= baseTotal
	}) {
		t.Errorf("goroutines %d after the streams and closed connections, %d before", runtime.NumGoroutine(), baseTotal)
	}
	const base = 96 << 20
	if grow := int64(peak.Load()) - int64(before.HeapInuse); grow > wave*(32<<10)+base {
		t.Errorf("heap peak grew %d B, want <= %d streams × 32 KiB buffer + %d B base", grow, wave, base)
	}
	t.Logf("%d streams: %d settled, %d kept, %d aborted, %d rejected; heap peak +%d B; route goroutines %d → %d, all %d → %d",
		n, l.settles.Load(), l.keeps.Load(), l.aborts.Load(), l.rejects.Load(), int64(peak.Load())-int64(before.HeapInuse),
		baseRoute, routeGoroutines(), baseTotal, runtime.NumGoroutine())
}

// TestRelayOversizeChunkReachesAFastClient (Fremd-Review 1 F1): one upstream
// frame of 400 KiB under the 1 MiB frame cap whose rewrite outgrows the 2 MiB
// relay buffer — json.Marshal escapes '<' six-fold (2.4 MiB). A fast client is
// no slow client: it gets the whole chunk and [DONE], as with 'x'. Red
// before: 503 server_overloaded "the client read the stream slower …" on an
// empty buffer.
func TestRelayOversizeChunkReachesAFastClient(t *testing.T) {
	for _, ch := range []string{"x", "<"} {
		t.Run(ch, func(t *testing.T) {
			end := newUpstreamEnd()
			u := newUpstream(t, "")
			u.setRespond(sseRespond([]sseStep{{data: sglangChunk(strings.Repeat(ch, 400<<10), "stop")}, {data: sglangUsage}, {data: "[DONE]"}}, nil, end))
			rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings,
				withFrameCap(1<<20), withRelay(2<<20, 1024, 30*time.Second))
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			a := postStream(ctx, t, serveHTTP(t, rig.router), streamBody("chat"))
			if a.status != http.StatusOK || a.dones() != 1 || end.canceled {
				t.Fatalf("char %q: status %d, dones %d, upstream canceled %v, %d bytes: %.200q — want the whole stream",
					ch, a.status, a.dones(), end.canceled, len(a.raw), a.raw)
			}
			if ch == "<" && !strings.Contains(a.raw, strings.Repeat(`\u003c`, 64)) {
				t.Fatalf("char %q: the escaped content did not arrive (%d bytes)", ch, len(a.raw))
			}
			t.Logf("char %q: %d bytes, dones %d", ch, len(a.raw), a.dones())
		})
	}
}

// TestRelayDrainEndsWithTheAttempt (Fremd-Review 1 F3 (a), through the
// route): the row's attempt timeout is 1 s; the upstream ends at once (the
// lease with it), the client would need ≈ 2 s to drain — the stream is cut
// at the attempt's deadline, no [DONE]: a stream lasts its attempt's
// timeout, the rest from the buffer included.
func TestRelayDrainEndsWithTheAttempt(t *testing.T) {
	end := newUpstreamEnd()
	u := newUpstream(t, "")
	u.setRespond(sseRespond(bigFrames(6, 32<<10, 0), nil, end))
	row := u.row("spark", backends.TrustFull, 10)
	row.Timeouts = map[string]int{"chat": 1}
	rig := newChatRig(t, []backends.Backend{row}, 4, 1, withStreamSettings)
	start := time.Now()
	c := startSlowClient(t, rig)
	if !end.wait(5*time.Second) || end.canceled {
		t.Fatal("the upstream did not run to its end")
	}
	if !waitHeld(rig.disp, u.srv.URL, 0, 500*time.Millisecond) {
		t.Fatal("the lease outlived the upstream's end")
	}
	frames, done := 0, false
	for {
		data, err := c.event()
		if err != nil {
			break
		}
		if data == "[DONE]" {
			done = true
			break
		}
		frames++
		time.Sleep(400 * time.Millisecond)
	}
	took := time.Since(start)
	if done || frames >= 6 {
		t.Fatalf("client got %d frames, [DONE] %v after %v — want the stream cut at the 1 s attempt deadline", frames, done, took)
	}
	if took > 5*time.Second {
		t.Fatalf("the cut came after %v, want about the 1 s attempt deadline", took)
	}
	t.Logf("cut after %d frames, %v", frames, took)
}
