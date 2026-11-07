package oai_test

// Pins of the stream writer and the stream lifecycle (W02-08, Härtung 2):
// the surviving mutants of Fremd-Review 2 (L-6) made permanent gates. Each
// test names what it pins and the mutant that turns it red; the mutants run
// as go test -overlay against a copy of the tree. The time gates keep a wide
// distance between the code and its mutant (the host runs under load).

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/oai"
)

// TestPinStreamTimerFrozenOnPanicBeforeCommit pins that the route freezes a
// stream's armed grace timer when the handler ends by a panic before the
// commit: the 503 envelope is the only status, nothing is written after the
// handler returned. Red: Fremd-Review 2 MD7 (close without sse.freeze — the
// timer commits a second WriteHeader on the finished response).
func TestPinStreamTimerFrozenOnPanicBeforeCommit(t *testing.T) {
	fastPhaseB(t, 300*time.Millisecond)
	const grace = 400 * time.Millisecond // clamp(0, wait 100 ms + distance 300 ms, 50 s)
	u1, u2 := newUpstream(t, ""), newUpstream(t, "")
	var failedAt atomic.Int64
	u1.setRespond(status502(20*time.Millisecond, &failedAt))
	u2.setRespond(sseRespond(streamShapes[0].steps(), nil, nil))
	rig := newChatRig(t, []backends.Backend{u1.row("one", backends.TrustFull, 20), u2.row("two", backends.TrustFull, 10)}, 4, 1,
		withStreamSettings, withWait(100*time.Millisecond), withStream(0, 50*time.Millisecond, 5*time.Second),
		func(r *chatRig) { r.deps.Planner = panicNextPlanner{r.deps.Planner} })
	var bc byteClock
	srv := serveHTTP(t, bc.wrap(rig.router))
	a := postStream(context.Background(), t, srv, streamBody("chat"))
	if failedAt.Load() == 0 || time.Unix(0, failedAt.Load()).Sub(a.start) >= grace {
		t.Fatalf("the first row did not fail before the grace commit (failed at %v after the request)", time.Unix(0, failedAt.Load()).Sub(a.start))
	}
	// The armed timer would fire at entry + grace; wait well past it.
	time.Sleep(grace + 800*time.Millisecond)
	if a.status != http.StatusServiceUnavailable || strings.Contains(a.raw, "keepalive") {
		t.Errorf("panic before the commit: status %d raw %q, want the 503 envelope and no keepalive", a.status, a.raw)
	}
	if n := bc.headers.Load(); n != 1 {
		t.Errorf("WriteHeader with a final status %d times, want 1: the grace timer wrote after the handler returned", n)
	}
}

// TestPinKeepaliveCadenceAfterTheLastFrame pins the keepalive cadence of a
// committed stream: after the grace commit and two data frames, the first
// keepalive of the silence comes keepalive_interval after the LAST frame,
// not one interval after the timer's previous tick. Red: Fremd-Review 2 MD9
// (the timer always re-armed with the full interval — the keepalive then
// comes up to two intervals after the last frame).
func TestPinKeepaliveCadenceAfterTheLastFrame(t *testing.T) {
	fastPhaseB(t, 50*time.Millisecond)
	const keepalive = 800 * time.Millisecond // grace_eff 150 ms = clamp(0, wait 100 ms + distance 50 ms, 50 s)
	u := newUpstream(t, "")
	u.setRespond(sseRespond([]sseStep{
		{wait: 200 * time.Millisecond, data: sglangChunk("a", "")}, // after the grace commit at 150 ms
		{wait: 50 * time.Millisecond, data: sglangChunk("b", "")},
		{wait: 2 * time.Second, data: sglangChunk(" end", "stop")},
		{data: "[DONE]"},
	}, nil, nil))
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1,
		withStreamSettings, withWait(100*time.Millisecond), withStream(0, keepalive, 5*time.Second))
	a := postStream(context.Background(), t, serveHTTP(t, rig.router), streamBody("chat"))
	if a.status != http.StatusOK || a.dones() != 1 || a.readErr != nil {
		t.Fatalf("answer %d dones %d read error %v: %q", a.status, a.dones(), a.readErr, a.raw)
	}
	if a.commentsBeforeData() < 1 {
		t.Fatalf("no grace commit before the first frame: %q", a.raw)
	}
	var lastFrame, firstKA time.Time
	datas := 0
	for _, e := range a.events {
		switch {
		case e.data != "" && datas < 2:
			datas++
			lastFrame = e.at
		case e.comment == "keepalive" && datas == 2 && firstKA.IsZero():
			firstKA = e.at
		}
	}
	if datas != 2 || firstKA.IsZero() {
		t.Fatalf("no keepalive after the second frame: %q", a.raw)
	}
	gap := firstKA.Sub(lastFrame)
	t.Logf("first keepalive %v after the last frame (interval %v)", gap, keepalive)
	if gap < keepalive/2 || gap > keepalive*7/5 {
		t.Errorf("first keepalive %v after the last frame, want ≈ %v (in [%v, %v]): the timer counts from its tick, not from the last write",
			gap, keepalive, keepalive/2, keepalive*7/5)
	}
}

// TestPinKeepaliveRollsTheWriteDeadline pins that every keepalive write
// rolls the connection's write deadline by client_write_window: a reading
// client stays connected through an upstream silence far longer than the
// window. Red: Fremd-Review 2 MD11 (only writes > 20 bytes roll — the
// keepalives do not, the stream breaks one window after the last frame).
func TestPinKeepaliveRollsTheWriteDeadline(t *testing.T) {
	fastPhaseB(t, 50*time.Millisecond)
	const window, silence = 500 * time.Millisecond, 1500 * time.Millisecond
	u := newUpstream(t, "")
	u.setRespond(sseRespond([]sseStep{{data: sglangChunk("hello", "")}, {wait: silence, data: sglangChunk(" end", "stop")}, {data: "[DONE]"}}, nil, nil))
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1,
		withStreamSettings, withWait(100*time.Millisecond), withStream(0, 100*time.Millisecond, window))
	a := postStream(context.Background(), t, serveHTTP(t, rig.router), streamBody("chat"))
	n := strings.Count(a.raw, ": keepalive")
	t.Logf("%d keepalives during %v of silence (window %v)", n, silence, window)
	if a.status != http.StatusOK || a.dones() != 1 || a.readErr != nil || n < 5 {
		t.Errorf("silence of %v under a window of %v: status %d dones %d read error %v keepalives %d, want a whole stream kept alive (every keepalive rolls the deadline)",
			silence, window, a.status, a.dones(), a.readErr, n)
	}
}

// TestPinStreamReserveIsTwoFrames pins the size of a stream's in-flight
// budget share: 2 × max_stream_frame_bytes on top of the request's share —
// 3 × Content-Length, grown by FOLGE-P's ledger past it (the read, the JSON
// check's nesting state, the attempt's upstream body; rebase round 2, A2)
// — under the production defaults (256 MiB budget, 1 MiB frame cap) 127
// concurrent streams. Red: Fremd-Review 2 R2-01 (the stream reserves 2 ×
// max_response_bytes like non-stream: 8 MiB, 31 streams).
func TestPinStreamReserveIsTwoFrames(t *testing.T) {
	hold := make(chan struct{})
	entered := make(chan struct{}, 1)
	u := newUpstream(t, "")
	u.setRespond(sseRespond([]sseStep{{hold: hold}, {data: sglangChunk("hello", "stop")}, {data: "[DONE]"}}, entered, nil))
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings)
	frame := int64(rig.settings.Load().MaxStreamFrameBytes)
	if frame != 1<<20 || int64(rig.settings.Load().MaxResponseBytes) == frame {
		t.Fatalf("rig settings: frame cap %d, response cap %d — the pin needs 1 MiB and a different response cap", frame, rig.settings.Load().MaxResponseBytes)
	}
	body := streamBody("chat")
	srv := serveHTTP(t, rig.router)
	done := make(chan streamAnswer, 1)
	go func() { done <- postStream(context.Background(), t, srv, body) }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		close(hold)
		t.Fatal("the upstream saw no request")
	}
	inUse := rig.budget.InUse()
	close(hold)
	a := <-done
	raws := rawsOf(u)
	if len(raws) != 1 {
		t.Fatalf("%d upstream requests, want 1", len(raws))
	}
	const upstreamSlack = 8 // the upstream body is booked at its encoder's size bound
	// A03-W10: the request share carries the stream's relay buffer (2 MiB
	// at the default), booked before it is made — the default budget holds
	// 63 such streams (N-3: 127 before the relay).
	b, depth := int64(len(body)), oai.JSONDepth([]byte(body))
	relay := rig.settings.Load().StreamRelayBufferBytes
	share := max(3*b, b+depth*jsonStateBytesPerLevel+int64(len(raws[0]))+relay)
	want := share + 2*frame
	if inUse < want || inUse > want+upstreamSlack {
		t.Errorf("in-flight budget in use during the stream: %d bytes, want the request share max(3 × %d, %d + %d × %d + %d upstream + %d relay buffer) = %d + 2 × %d (frame cap) = %d (+ ≤ %d)",
			inUse, b, b, depth, jsonStateBytesPerLevel, len(raws[0]), relay, share, frame, want, upstreamSlack)
	}
	const defaultBudget = 256 << 20 // the production default of gateway.inflight_budget
	if n := defaultBudget / inUse; n != 63 {
		t.Errorf("the default budget holds %d such streams, want 63 (N-3 with the relay buffer; 127 before A03-W10)", n)
	}
	if a.status != http.StatusOK || a.dones() != 1 {
		t.Errorf("answer %d dones %d: %q", a.status, a.dones(), a.raw)
	}
	if !waitFor(2*time.Second, func() bool { return rig.budget.InUse() == 0 }) {
		t.Errorf("budget in use after the stream: %d, want 0", rig.budget.InUse())
	}
}

// pinDeadlineClock records every write deadline the route sets on its
// response writer.
type pinDeadlineClock struct {
	mu  sync.Mutex
	set []time.Time
}

func (c *pinDeadlineClock) wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(&pinDeadlineWriter{ResponseWriter: w, c: c}, r)
	})
}

// last is the deadline in force (zero: none set).
func (c *pinDeadlineClock) last() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.set) == 0 {
		return time.Time{}
	}
	return c.set[len(c.set)-1]
}

type pinDeadlineWriter struct {
	http.ResponseWriter
	c *pinDeadlineClock
}

func (w *pinDeadlineWriter) SetWriteDeadline(t time.Time) error {
	w.c.mu.Lock()
	w.c.set = append(w.c.set, t)
	w.c.mu.Unlock()
	return http.NewResponseController(w.ResponseWriter).SetWriteDeadline(t)
}

func (w *pinDeadlineWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// TestPinStreamWriteDeadlineBeforeTheCommit pins the write deadline of a
// stream in phase B (design/02 §4.12): entry + grace_eff + 30 s once the
// parse knew the form, not the non-stream frame entry + MaxWait +
// request_deadline + 30 s. Red: Fremd-Review 2 R2-04 (the stream keeps the
// entry deadline — 590 s instead of 70 s under the defaults).
func TestPinStreamWriteDeadlineBeforeTheCommit(t *testing.T) {
	hold := make(chan struct{})
	entered := make(chan struct{}, 1)
	u := newUpstream(t, "")
	u.setRespond(sseRespond([]sseStep{{hold: hold}, {data: sglangChunk("hello", "stop")}, {data: "[DONE]"}}, entered, nil))
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings)
	const grace, margin = 40 * time.Second, 30 * time.Second // grace_eff = clamp(40 s, MaxWait 20 s + 5 s, 50 s); writeMargin
	var dc pinDeadlineClock
	srv := serveHTTP(t, dc.wrap(rig.router))
	start := time.Now()
	done := make(chan streamAnswer, 1)
	go func() { done <- postStream(context.Background(), t, srv, streamBody("chat")) }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		close(hold)
		t.Fatal("the upstream saw no request")
	}
	inForce, now := dc.last(), time.Now()
	close(hold)
	a := <-done
	if inForce.IsZero() {
		t.Fatal("the route set no write deadline")
	}
	lo, hi := start.Add(grace+margin), now.Add(grace+margin)
	if inForce.Before(lo) || inForce.After(hi) {
		t.Errorf("write deadline in phase B at request + %v, want entry + grace_eff + 30 s = request + %v … %v",
			inForce.Sub(start), lo.Sub(start), hi.Sub(start))
	}
	if a.status != http.StatusOK || a.dones() != 1 {
		t.Errorf("answer %d dones %d: %q", a.status, a.dones(), a.raw)
	}
}

// TestPinStreamTTFTIsTheFirstFrame pins the value of the row's ttft_ms: the
// time from the request's entry to its FIRST data frame — not zero, not the
// last frame. Red: Fremd-Review 2 S08 (frame without s.frames++ — the
// last frame's time) and R2-09 (ttft always 0).
func TestPinStreamTTFTIsTheFirstFrame(t *testing.T) {
	const first, later = 300 * time.Millisecond, time.Second
	u := newUpstream(t, "")
	u.setRespond(sseRespond([]sseStep{{wait: first, data: sglangChunk("a", "")}, {wait: later, data: sglangChunk("b", "stop")}, {data: "[DONE]"}}, nil, nil))
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings)
	a := postStream(context.Background(), t, serveHTTP(t, rig.router), streamBody("chat"))
	if a.status != http.StatusOK || a.dones() != 1 {
		t.Fatalf("answer %d dones %d: %q", a.status, a.dones(), a.raw)
	}
	var firstSeen time.Time
	for _, e := range a.events {
		if e.data != "" {
			firstSeen = e.at
			break
		}
	}
	rows := rig.rows.all()
	if len(rows) != 1 || rows[0].Gateway.TTFTMs == nil {
		t.Fatalf("rows %+v, want one row with a ttft", rows)
	}
	ttft := time.Duration(*rows[0].Gateway.TTFTMs) * time.Millisecond
	// The client read the first frame after the route wrote it; the slack
	// covers the route's clock read after the flush returned.
	ceiling := firstSeen.Sub(a.start) + 250*time.Millisecond
	t.Logf("ttft %v; the client saw the first frame %v after the request (upstream: first frame after %v, the next %v later)", ttft, firstSeen.Sub(a.start), first, later)
	if ttft < first || ttft > ceiling {
		t.Errorf("ttft %v, want the first frame's time: >= %v (the upstream's delay) and <= %v (the client's first frame + slack)", ttft, first, ceiling)
	}
}

// TestPinStreamOfOnlyTheEndIsCommitted pins the commit of a stream whose
// upstream relays no frame (only [DONE], or only the usage chunk a client
// without include_usage does not get): 200 with the SSE headers and exactly
// "data: [DONE]". Red: Fremd-Review 2 S06 (done without its commit branch —
// net/http's implicit text/plain status without the SSE headers).
func TestPinStreamOfOnlyTheEndIsCommitted(t *testing.T) {
	for _, tc := range []struct {
		name  string
		steps []sseStep
	}{
		{"only [DONE]", []sseStep{{data: "[DONE]"}}},
		{"only the usage chunk without include_usage", []sseStep{{data: sglangUsage}, {data: "[DONE]"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := newUpstream(t, "")
			u.setRespond(sseRespond(tc.steps, nil, nil))
			rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings)
			a := postStream(context.Background(), t, serveHTTP(t, rig.router), streamBody("chat"))
			if a.status != http.StatusOK || a.readErr != nil || a.raw != "data: [DONE]\n\n" {
				t.Errorf("answer %d read error %v raw %q, want 200 and exactly one [DONE]", a.status, a.readErr, a.raw)
			}
			for k, want := range map[string]string{"Content-Type": "text/event-stream", "Cache-Control": "no-cache", "X-Accel-Buffering": "no"} {
				if got := a.header.Get(k); got != want {
					t.Errorf("header %s = %q, want %q (the stream commits before its [DONE])", k, got, want)
				}
			}
		})
	}
}
