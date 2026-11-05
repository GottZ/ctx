package oai_test

// The meter at the stream half of the chat route (W02-08 rebase round on
// W06-10): the rule of the non-stream half holds for stream: true — a
// reservation goes back in full (Abort) exactly when no attempt, the first
// try of httpx.DoRetryOnce included, began to write the request to a
// backend; after wire contact the usage settles it — the usage chunk, else
// the estimate of a stream the backend began to answer (design/06 §4.2
// B-06-8: a stream cut short generated tokens) — or, without either, it
// keeps its charge; every reservation ends once. The six x-ratelimit-*
// headers go out with the stream's status (streamWriter.commitLocked),
// before its first body byte, on each of the three commits (the first
// frame, the grace timer, a stream of only [DONE]). Every case runs over
// the rig's real planner, dispatcher and meter (endMeter, frozen clock);
// streamBody reserves 1 042 tokens like chatBody.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/oai"
	"github.com/GottZ/ctx/internal/promptguard"
)

// streamMeterRig mounts rows with the production stream settings over a
// fresh endMeter; opts apply after them.
func streamMeterRig(t *testing.T, rows []backends.Backend, slots, reserved int, opts ...rigOption) (*chatRig, *endMeter) {
	t.Helper()
	m := newEndMeter()
	all := append([]rigOption{withStreamSettings}, opts...)
	all = append(all, withDeps(func(d *oai.Deps) { d.Limiter = m }))
	return newChatRig(t, rows, slots, reserved, all...), m
}

// waitEnded waits until the meter saw an end (a served stream's client can
// read the stream's end before the route has booked).
func waitEnded(m *endMeter) bool {
	return waitFor(3*time.Second, func() bool { return m.endsOf() != "" })
}

// streamEstimate is what the route books for a stream the backend began to
// answer without a usage chunk: the prompt estimate of the request's
// messages plus TokenEstimate over the streamed text (design/06 §4.2).
func streamEstimate(streamed string) int64 {
	prompt := promptguard.TokenEstimate(len([]rune(messagesOf([]byte(streamBody("chat"))))))
	return int64(prompt + promptguard.TokenEstimate(len([]rune(streamed))))
}

// postStreamTraced sends streamBody with trace on the inbound request
// context (the route composes its own trace with it) into a recorder.
func postStreamTraced(t *testing.T, rig *chatRig, trace *httptrace.ClientTrace) chatAnswer {
	t.Helper()
	ctx := context.Background()
	if trace != nil {
		ctx = httptrace.WithClientTrace(ctx, trace)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(streamBody("chat"))).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	rig.router.ServeHTTP(w, req)
	return chatAnswer{status: w.Code, header: w.Header(), body: w.Body.Bytes()}
}

// TestStreamMeterReadThenResetKeeps (F4-1 for the stream): the backend reads
// the whole request and dies (RST) before its first frame; DoRetryOnce's
// retry finds the host refusing. The first try wrote the request: Keep.
func TestStreamMeterReadThenResetKeeps(t *testing.T) {
	b := newRawBackend(t, func(b *rawBackend, c net.Conn, br *bufio.Reader, _ int64) {
		if b.read(br) {
			b.l.stop()
			reset(c)
		}
	})
	rig, m := streamMeterRig(t, []backends.Backend{wireRow("crash", b.host(), 10)}, 4, 1)
	a := rig.post(t, streamBody("chat"))
	held := m.held(t, rig)
	t.Logf("stream, read, reset, retry refused: answer %d; backend conns %d, requests read %d; ends %s; the bucket holds %d tokens",
		a.status, b.conns.Load(), b.requests.Load(), m.endsOf(), held)
	if b.requests.Load() != 1 || b.conns.Load() != 1 || a.status != http.StatusServiceUnavailable {
		t.Fatalf("answer %d, backend conns %d, requests %d — want a 503 before the commit after one connection that read the request",
			a.status, b.conns.Load(), b.requests.Load())
	}
	if held != wireReservation || m.endsOf() != "keep" {
		t.Errorf("the bucket holds %d tokens (ends %s) after a backend read the stream's request, want the whole reservation %d kept",
			held, m.endsOf(), wireReservation)
	}
}

// TestStreamMeterNoByteGivesBack: no try wrote the request — every connect
// refused, or the host dropping every SYN until the attempt deadline: Abort.
func TestStreamMeterNoByteGivesBack(t *testing.T) {
	t.Run("connect refused", func(t *testing.T) {
		rig, m := streamMeterRig(t, []backends.Backend{wireRow("refused", "http://"+refusedAddr(t), 10)}, 4, 1)
		a := rig.post(t, streamBody("chat"))
		held := m.held(t, rig)
		t.Logf("stream, connect refused: answer %d; ends %s; the bucket holds %d tokens", a.status, m.endsOf(), held)
		if held != 0 || m.endsOf() != "abort" {
			t.Errorf("the bucket holds %d tokens (ends %s) after every connect was refused, want 0 by Abort", held, m.endsOf())
		}
	})
	t.Run("connect timeout", func(t *testing.T) {
		l := newHeldListener(t)
		l.dropSYNs()
		rig, m := streamMeterRig(t, []backends.Backend{noTimeouts(wireRow("blackhole", "http://"+l.Addr().String(), 10))}, 4, 1)
		shortAttempts(rig)
		start := time.Now()
		a := rig.post(t, streamBody("chat"))
		el := time.Since(start)
		held := m.held(t, rig)
		t.Logf("stream, connect timeout: answer %d after %v; ends %s; the bucket holds %d tokens", a.status, el.Round(time.Millisecond), m.endsOf(), held)
		if el < 200*time.Millisecond {
			t.Fatalf("answered after %v — the connect did not hang until the attempt deadline", el)
		}
		if held != 0 || m.endsOf() != "abort" {
			t.Errorf("the bucket holds %d tokens (ends %s) after a connect that timed out, want 0 by Abort", held, m.endsOf())
		}
	})
}

// TestStreamMeterUsageChunkSettles: a stream with the backend's usage chunk
// (11 + 3) settles with it, over HTTP/1.1 and HTTP/2.
func TestStreamMeterUsageChunkSettles(t *testing.T) {
	t.Run("http/1.1", func(t *testing.T) {
		u := newUpstream(t, "")
		u.setRespond(sseRespond(streamShapes[0].steps(), nil, nil))
		rig, m := streamMeterRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
		a := postStream(context.Background(), t, serveHTTP(t, rig.router), streamBody("chat"))
		if !waitEnded(m) {
			t.Fatal("the reservation never ended")
		}
		held := m.held(t, rig)
		t.Logf("stream with usage: answer %d, [DONE] %d; ends %s; the bucket holds %d tokens", a.status, a.dones(), m.endsOf(), held)
		if a.status != http.StatusOK || a.dones() != 1 {
			t.Fatalf("answer %d, [DONE] %d: %s — want a whole stream", a.status, a.dones(), a.raw)
		}
		if held != 14 || m.endsOf() != "settle" {
			t.Errorf("the bucket holds %d tokens (ends %s), want the usage chunk's 14 by Settle", held, m.endsOf())
		}
	})
	t.Run("http/2", func(t *testing.T) {
		var h2 atomic.Int64
		steps := sseRespond(streamShapes[0].steps(), nil, nil)
		url := serveH2(t, newHeldListener(t), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ProtoMajor == 2 {
				h2.Add(1)
			}
			_, _ = io.ReadAll(r.Body)
			steps(w, r)
		}))
		rig, m := streamMeterRig(t, []backends.Backend{wireRow("h2", url, 10)}, 4, 1)
		pt := &h2Trace{}
		a := postStreamTraced(t, rig, pt.trace())
		held := m.held(t, rig)
		t.Logf("h2 stream with usage: answer %d; connections %s; h2 handler calls %d; ends %s; the bucket holds %d tokens",
			a.status, pt, h2.Load(), m.endsOf(), held)
		if !strings.HasPrefix(pt.String(), "h2") || h2.Load() != 1 || a.status != http.StatusOK {
			t.Fatalf("answer %d, connections %q, h2 handler calls %d — want one h2 stream", a.status, pt, h2.Load())
		}
		if held != 14 || m.endsOf() != "settle" {
			t.Errorf("the bucket holds %d tokens (ends %s), want the usage chunk's 14 by Settle", held, m.endsOf())
		}
	})
	t.Run("http/2, request read, stream reset", func(t *testing.T) {
		url := serveH2(t, newHeldListener(t), http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body)
			panic(http.ErrAbortHandler)
		}))
		rig, m := streamMeterRig(t, []backends.Backend{wireRow("h2", url, 10)}, 4, 1)
		pt := &h2Trace{}
		a := postStreamTraced(t, rig, pt.trace())
		held := m.held(t, rig)
		t.Logf("h2 stream, request read, RST_STREAM: answer %d; connections %s; ends %s; the bucket holds %d tokens", a.status, pt, m.endsOf(), held)
		if !strings.HasPrefix(pt.String(), "h2") {
			t.Fatalf("connections %q — want h2", pt)
		}
		if held != wireReservation || m.endsOf() != "keep" {
			t.Errorf("the bucket holds %d tokens (ends %s), want the reservation %d kept", held, m.endsOf(), wireReservation)
		}
	})
}

// TestStreamMeterCutStreamSettlesTheEstimate: a stream the backend began to
// answer and that ends without a usage chunk books the estimate (design/06
// §4.2 B-06-8) — never a full refund, never the whole reservation: the
// backend's connection reset after three frames, and the client gone after
// two.
func TestStreamMeterCutStreamSettlesTheEstimate(t *testing.T) {
	t.Run("backend reset after three frames", func(t *testing.T) {
		cut := make(chan struct{})
		b := newRawBackend(t, func(b *rawBackend, c net.Conn, br *bufio.Reader, _ int64) {
			if !b.read(br) {
				return
			}
			_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nCache-Control: no-cache\r\n\r\n")
			for _, d := range streamDeltas {
				_, _ = fmt.Fprintf(c, "data: %s\n\n", sglangChunk(d, ""))
			}
			select {
			case <-cut:
			case <-b.done:
			}
			reset(c)
		})
		rig, m := streamMeterRig(t, []backends.Backend{wireRow("cut", b.host(), 10)}, 4, 1)
		ls, each := openAndRead(t, context.Background(), serveHTTP(t, rig.router), streamBody("chat"))
		defer func() { _ = ls.Close() }()
		for i := 0; i < 3; i++ {
			if _, ok := nextData(each, 5*time.Second); !ok {
				t.Fatalf("frame %d of 3 did not arrive", i+1)
			}
		}
		close(cut)
		var last string
		for e := range each {
			if e.data != "" {
				last = e.data
			}
		}
		if !waitEnded(m) {
			t.Fatal("the reservation never ended")
		}
		held, want := m.held(t, rig), streamEstimate(streamText)
		t.Logf("stream cut by a backend reset after 3 frames: last frame %s; ends %s; the bucket holds %d tokens (estimate %d)",
			last, m.endsOf(), held, want)
		if !strings.Contains(last, `"error"`) {
			t.Fatalf("last frame %q — want the error frame after the commit", last)
		}
		if held != want || m.endsOf() != "settle" {
			t.Errorf("the bucket holds %d tokens (ends %s), want the estimate %d by Settle", held, m.endsOf(), want)
		}
	})
	t.Run("client gone after two frames", func(t *testing.T) {
		hold := make(chan struct{})
		defer close(hold)
		end := newUpstreamEnd()
		u := newUpstream(t, "")
		u.setRespond(sseRespond([]sseStep{{data: sglangChunk("hello", "")}, {data: sglangChunk(" from", "")}, {hold: hold}}, nil, end))
		rig, m := streamMeterRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
		ctx, cancel := context.WithCancel(context.Background())
		ls, each := openAndRead(t, ctx, serveHTTP(t, rig.router), streamBody("chat"))
		for i := 0; i < 2; i++ {
			if _, ok := nextData(each, 5*time.Second); !ok {
				cancel()
				t.Fatalf("frame %d of 2 did not arrive", i+1)
			}
		}
		cancel()
		_ = ls.Close()
		if !end.wait(2*time.Second) || !waitEnded(m) {
			t.Fatal("the upstream did not see the abort or the reservation never ended")
		}
		held, want := m.held(t, rig), streamEstimate("hello from")
		t.Logf("client gone after 2 frames: ends %s; the bucket holds %d tokens (estimate %d)", m.endsOf(), held, want)
		if held != want || m.endsOf() != "settle" {
			t.Errorf("the bucket holds %d tokens (ends %s), want the estimate %d by Settle", held, m.endsOf(), want)
		}
	})
}

// TestStreamMeterAfterTheGraceCommitKeeps: the grace commit is out, the
// first row then answers 502 (wire contact) — a full failover row refuses
// the Next acquire (the server_busy frame), or the Next acquire panics (the
// connection cut). Either way the backend had the request: Keep, never the
// refund of Reject or Abort.
func TestStreamMeterAfterTheGraceCommitKeeps(t *testing.T) {
	fastPhaseB(t, 100*time.Millisecond)
	opts := []rigOption{withWait(100 * time.Millisecond), withStream(0, 10*time.Second, 5*time.Second)}
	t.Run("failover refused", func(t *testing.T) {
		u1, u2 := newUpstream(t, ""), newUpstream(t, "")
		var failedAt atomic.Int64
		u1.setRespond(status502(400*time.Millisecond, &failedAt))
		u2.setRespond(sseRespond(streamShapes[0].steps(), nil, nil))
		rig, m := streamMeterRig(t, []backends.Backend{u1.row("one", backends.TrustFull, 20), u2.row("two", backends.TrustFull, 10)}, 2, 1, opts...)
		srv := serveHTTP(t, rig.router)
		defer occupyExternal(t, rig.disp, u2.srv.URL)()
		a := postStream(context.Background(), t, srv, streamBody("chat"))
		code, last := a.errorFrame(t)
		if !waitEnded(m) {
			t.Fatal("the reservation never ended")
		}
		held := m.held(t, rig)
		t.Logf("grace commit, 502, failover refused: answer %d, frame %q; ends %s; the bucket holds %d tokens", a.status, code, m.endsOf(), held)
		if a.status != http.StatusOK || code != oai.CodeServerBusy || !last || u2.hits.Load() != 0 {
			t.Fatalf("answer %d, frame %q (last %v), second row hits %d: %s — want the server_busy frame after the grace commit",
				a.status, code, last, u2.hits.Load(), a.raw)
		}
		if held != wireReservation || m.endsOf() != "keep" {
			t.Errorf("the bucket holds %d tokens (ends %s), want the reservation %d kept", held, m.endsOf(), wireReservation)
		}
	})
	t.Run("panic in the failover acquire", func(t *testing.T) {
		u1, u2 := newUpstream(t, ""), newUpstream(t, "")
		var failedAt atomic.Int64
		u1.setRespond(status502(400*time.Millisecond, &failedAt))
		u2.setRespond(sseRespond(streamShapes[0].steps(), nil, nil))
		rig, m := streamMeterRig(t, []backends.Backend{u1.row("one", backends.TrustFull, 20), u2.row("two", backends.TrustFull, 10)}, 4, 1,
			append(opts, func(r *chatRig) { r.deps.Planner = panicNextPlanner{r.deps.Planner} })...)
		a := postStream(context.Background(), t, serveHTTP(t, rig.router), streamBody("chat"))
		if !waitEnded(m) {
			t.Fatal("the reservation never ended")
		}
		held := m.held(t, rig)
		t.Logf("grace commit, 502, panic in Next: answer %d, read error %v; ends %s; the bucket holds %d tokens", a.status, a.readErr, m.endsOf(), held)
		if a.status != http.StatusOK || a.readErr == nil {
			t.Fatalf("answer %d, read error %v: %s — want the grace commit and a cut connection", a.status, a.readErr, a.raw)
		}
		if held != wireReservation || m.endsOf() != "keep" {
			t.Errorf("the bucket holds %d tokens (ends %s), want the reservation %d kept by close()", held, m.endsOf(), wireReservation)
		}
	})
}

// TestStreamMeterUsageCountersSaturate (H3-5 for the stream): a usage chunk
// whose counters overflow an int sum (prompt MaxInt64, completion 1) is the
// backend's usage — the row says backend and the reservation settles with
// it — never the estimate of a stream without one.
func TestStreamMeterUsageCountersSaturate(t *testing.T) {
	huge := fmt.Sprintf(`{"id":"ID-CANARY-sglang","object":"chat.completion.chunk","created":1700000000,"model":"MODEL-CANARY",`+
		`"choices":[],"usage":{"prompt_tokens":%d,"completion_tokens":1,"total_tokens":1}}`, int64(math.MaxInt64))
	u := newUpstream(t, "")
	u.setRespond(sseRespond([]sseStep{{data: sglangChunk("hello", "stop")}, {data: huge}, {data: "[DONE]"}}, nil, nil))
	rig, m := streamMeterRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
	a := postStream(context.Background(), t, serveHTTP(t, rig.router), streamBody("chat"))
	if !waitEnded(m) || !waitFor(2*time.Second, func() bool { return len(rig.rows.all()) == 1 }) {
		t.Fatal("no end or no row")
	}
	r := rig.rows.all()[0]
	t.Logf("usage MaxInt64 + 1: answer %d; row usage %q tokens %d/%d; ends %s", a.status, r.Gateway.Usage, r.Telemetry.PromptTokens, r.Telemetry.CompletionTokens, m.endsOf())
	if r.Gateway.Usage != llmlog.UsageBackend || r.Telemetry.PromptTokens != math.MaxInt64 || m.endsOf() != "settle" {
		t.Errorf("row usage %q prompt %d, ends %s — want the backend's counters (MaxInt64) settled", r.Gateway.Usage, r.Telemetry.PromptTokens, m.endsOf())
	}
}

// rawStream sends body over a raw TCP connection to srv and returns the
// response's status line, its header block — the bytes before the body, as
// they crossed the socket — and the first SSE line of the body.
func rawStream(t *testing.T, srv *httptest.Server, body string) (status string, hdr textproto.MIMEHeader, first string) {
	t.Helper()
	c, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := fmt.Fprintf(c, "POST /v1/chat/completions HTTP/1.1\r\nHost: ctx\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body); err != nil {
		t.Fatal(err)
	}
	tp := textproto.NewReader(bufio.NewReader(c))
	if status, err = tp.ReadLine(); err != nil {
		t.Fatalf("status line: %v", err)
	}
	if hdr, err = tp.ReadMIMEHeader(); err != nil {
		t.Fatalf("header block: %v", err)
	}
	for {
		line, err := tp.ReadLine()
		if err != nil {
			t.Fatalf("body: %v", err)
		}
		if strings.HasPrefix(line, "data:") || strings.HasPrefix(line, ":") {
			return status, hdr, line
		}
	}
}

// TestStreamRateLimitHeadersAtTheCommit: the stream's 200 carries the six
// x-ratelimit-* headers of its admission in its header block — on the
// socket, before the first body byte — whichever of the three commits sends
// it: the first data frame, the grace timer (a keepalive first), a stream of
// only [DONE]. Values: RPM 1 000 and TPM 60 000 000 of the rig's meter,
// this request's 1 042 tokens reserved (reset = 60 s / 1 000 per request,
// 1 042 µs rounded up to 2 ms).
func TestStreamRateLimitHeadersAtTheCommit(t *testing.T) {
	fastPhaseB(t, 100*time.Millisecond)
	want := map[string]string{
		"X-Ratelimit-Limit-Requests": "1000", "X-Ratelimit-Remaining-Requests": "999", "X-Ratelimit-Reset-Requests": "60ms",
		"X-Ratelimit-Limit-Tokens": "60000000", "X-Ratelimit-Remaining-Tokens": "59998958", "X-Ratelimit-Reset-Tokens": "2ms",
	}
	for _, tc := range []struct {
		name  string
		steps []sseStep
		first string
	}{
		{"first frame", streamShapes[0].steps(), "data: {"},
		{"grace timer", append([]sseStep{{wait: 600 * time.Millisecond}}, streamShapes[0].steps()...), ": keepalive"},
		{"only [DONE]", []sseStep{{data: "[DONE]"}}, "data: [DONE]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := newUpstream(t, "")
			u.setRespond(sseRespond(tc.steps, nil, nil))
			rig, _ := streamMeterRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1,
				withWait(100*time.Millisecond), withStream(0, 10*time.Second, 5*time.Second))
			status, hdr, first := rawStream(t, serveHTTP(t, rig.router), streamBody("chat"))
			t.Logf("%s: %s; first body line %q; x-ratelimit-* %v", tc.name, status, first, rateHeaders(hdr))
			if !strings.Contains(status, " 200 ") || !strings.HasPrefix(first, tc.first) {
				t.Fatalf("status %q, first body line %q — want 200 and the commit by %s", status, first, tc.name)
			}
			for k, v := range want {
				if got := hdr.Get(k); got != v {
					t.Errorf("%s: %q, want %q in the header block of the stream's 200", k, got, v)
				}
			}
		})
	}
	t.Run("refused before the commit carries none", func(t *testing.T) {
		u := newUpstream(t, "")
		u.setRespond(chRespond(http.StatusBadRequest, `{"error":{"message":"bad"}}`))
		rig, _ := streamMeterRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
		a := rig.post(t, streamBody("chat"))
		if a.status != http.StatusBadRequest || len(rateHeaders(textproto.MIMEHeader(a.header))) != 0 {
			t.Errorf("answer %d, x-ratelimit-* %v — want the 400 without them", a.status, rateHeaders(textproto.MIMEHeader(a.header)))
		}
	})
}

// TestStreamLimiterRefusalCarriesRetryAfterMs: the limiter's 429 of a
// stream request is the same as non-stream: Retry-After and retry-after-ms,
// before any byte of a stream (the meter runs before the walk).
func TestStreamLimiterRefusalCarriesRetryAfterMs(t *testing.T) {
	u := newUpstream(t, "")
	u.setRespond(sseRespond(streamShapes[0].steps(), nil, nil))
	m := newEndMeter()
	m.lim.RPM = 1
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings,
		withDeps(func(d *oai.Deps) { d.Limiter = m }))
	if a := postStream(context.Background(), t, serveHTTP(t, rig.router), streamBody("chat")); a.status != http.StatusOK {
		t.Fatalf("first stream %d, want 200", a.status)
	}
	a := rig.post(t, streamBody("chat"))
	code, _, _ := a.envelope(t)
	t.Logf("second stream at RPM 1: %d %s; Retry-After %q retry-after-ms %q", a.status, code, a.header.Get("Retry-After"), a.header.Get("Retry-After-Ms"))
	if a.status != http.StatusTooManyRequests || code != oai.CodeRateLimitExceeded || a.header.Get("Retry-After") == "" || a.header.Get("Retry-After-Ms") == "" {
		t.Errorf("answer %d %s, Retry-After %q, retry-after-ms %q — want 429 rate_limit_exceeded with both", a.status, code,
			a.header.Get("Retry-After"), a.header.Get("Retry-After-Ms"))
	}
	if len(rateHeaders(textproto.MIMEHeader(a.header))) != 0 {
		t.Errorf("the refusal carries x-ratelimit-* %v, want none", rateHeaders(textproto.MIMEHeader(a.header)))
	}
}

// rateHeaders are the x-ratelimit-* entries of h.
func rateHeaders(h textproto.MIMEHeader) map[string]string {
	out := map[string]string{}
	for k, v := range h {
		if strings.HasPrefix(strings.ToLower(k), "x-ratelimit-") {
			out[k] = strings.Join(v, ",")
		}
	}
	return out
}
