package oai_test

// Delta-Review 4 of W02-08 (hardening 3). F4-1: the estimate of a stream cut
// short never books more than its reservation holds — the completion
// estimate stops at the max_tokens that went upstream, and an estimate at or
// above what Keep books ends the reservation with Keep. Every case runs over
// the rig's real planner, dispatcher and meter (endMeter, frozen clock).

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/keyquota"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/GottZ/ctx/internal/oai"
	"github.com/GottZ/ctx/internal/promptguard"
)

// promptEstimate is the prompt estimate of body: TokenEstimate over the runes
// of its messages, as the meter reserves and the route estimates them.
func promptEstimate(body string) int64 {
	return int64(promptguard.TokenEstimate(len([]rune(messagesOf([]byte(body))))))
}

// statusless is an HTTP/1.1 backend that reads the request and then holds
// without a status line until the connection or the test ends: wire contact,
// never a 200. read gets one value per request it read.
func statusless(t *testing.T) (*rawBackend, <-chan struct{}) {
	t.Helper()
	read := make(chan struct{}, 4)
	b := newRawBackend(t, func(b *rawBackend, c net.Conn, br *bufio.Reader, _ int64) {
		if !b.read(br) {
			return
		}
		read <- struct{}{}
		gone := make(chan struct{})
		go func() { _, _ = c.Read(make([]byte, 1)); close(gone) }()
		select {
		case <-gone:
		case <-b.done:
		}
	})
	return b, read
}

// cutThenNext runs one stream of body over a rig whose meter has lim, cut by
// its client either before the backend's status (beforeStatus: the backend
// read the request and holds — Keep) or after the first frame of a backend
// that then holds (the estimate); then the same consumer's next non-streamed
// request. It returns how the stream's reservation ended and that request's
// status and retry-after-ms.
func cutThenNext(t *testing.T, lim keyquota.Limits, body string, beforeStatus bool, opts ...rigOption) (ends string, status int, retryMs string) {
	t.Helper()
	m := &endMeter{rigMeter: newRigMeter(lim, true)}
	hold := make(chan struct{})
	defer close(hold)
	var rows []backends.Backend
	var read <-chan struct{}
	if beforeStatus {
		var b *rawBackend
		b, read = statusless(t)
		rows = []backends.Backend{wireRow("hold", b.host(), 10)}
	} else {
		u := newUpstream(t, "")
		u.setRespond(sseRespond([]sseStep{{data: sglangChunk("a cat", "")}, {hold: hold}}, nil, nil))
		rows = []backends.Backend{u.row("spark", backends.TrustFull, 10)}
	}
	all := append([]rigOption{withStreamSettings, withDeps(func(d *oai.Deps) { d.Limiter = m })}, opts...)
	rig := newChatRig(t, rows, 4, 1, all...)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ls, each := openAndRead(t, ctx, serveHTTP(t, rig.router), body)
	if beforeStatus {
		select {
		case <-read:
		case <-time.After(5 * time.Second):
			t.Fatal("the backend never read the request")
		}
	} else if _, ok := nextData(each, 5*time.Second); !ok {
		t.Fatal("no frame")
	}
	cancel()
	_ = ls.Close()
	if !waitEnded(m) {
		t.Fatal("the stream's reservation never ended")
	}
	a := rig.post(t, chatBody("chat"))
	return m.endsOf(), a.status, a.header.Get("Retry-After-Ms")
}

// TestStreamMeterCutEstimateNeverAboveKeep is F4-1 (Delta-Review 4): a stream
// without a usage chunk that ends after the backend's 200 books its estimate
// only below what the reservation holds.
//
//   - max_tokens 8, one frame of 49 runes, the client gone: the completion
//     estimate is min(TokenEstimate(49) = 28, 8) = 8, the estimate 18 + 8 = 26
//     is the reservation 18 + 8 — not below it: Keep, 26 in the bucket, and
//     the row books 18 + 8 (red before: Settle 46).
//   - max_tokens 8, the backend's timings.prompt_n 1, one frame of 30 runes:
//     the estimate 1 + min(17, 8) = 9 is below the reservation 26: Settle 9
//     (red before: Settle 1 + 17 = 18).
//   - a prompt above the bucket (TPM 2 000, prompt estimate 4 462 > the
//     capacity 2 000; B-06-28 charges the capacity) and a vision stream under
//     the default chat TPM 60 000 (a data: image of 300 000 characters,
//     prompt estimate 166 741): the stream cut after its first frame leaves
//     the bucket where a stream cut before the backend's status (Keep) leaves
//     it — the next request waits 31 260 ms resp. 1 042 ms (red before:
//     105 210 ms and 107 786 ms).
func TestStreamMeterCutEstimateNeverAboveKeep(t *testing.T) {
	t.Run("max_tokens 8, a frame of 49 runes, client gone", func(t *testing.T) {
		hold := make(chan struct{})
		defer close(hold)
		body := streamBody("chat", `"max_tokens":8`)
		text := "The quick brown fox jumps over the lazy dog again"
		u := newUpstream(t, "")
		u.setRespond(sseRespond([]sseStep{{data: sglangChunk(text, "")}, {hold: hold}}, nil, nil))
		rig, m := streamMeterRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
		ctx, cancel := context.WithCancel(context.Background())
		ls, each := openAndRead(t, ctx, serveHTTP(t, rig.router), body)
		if _, ok := nextData(each, 5*time.Second); !ok {
			cancel()
			t.Fatal("no frame")
		}
		cancel()
		_ = ls.Close()
		if !waitEnded(m) || !waitFor(2*time.Second, func() bool { return len(rig.rows.all()) == 1 }) {
			t.Fatal("no end or no row")
		}
		prompt, held, r := promptEstimate(body), m.held(t, rig), rig.rows.all()[0]
		t.Logf("max_tokens 8, %d runes streamed, client gone: ends %s; the bucket holds %d tokens (reservation %d); row %q %d/%d",
			len([]rune(text)), m.endsOf(), held, prompt+8, r.Gateway.Usage, r.Telemetry.PromptTokens, r.Telemetry.CompletionTokens)
		if r.Gateway.Usage != llmlog.UsageEstimated || int64(r.Telemetry.PromptTokens) != prompt || r.Telemetry.CompletionTokens != 8 {
			t.Errorf("row %q %d/%d, want the estimate %d/8 (the completion estimate stops at max_tokens 8)",
				r.Gateway.Usage, r.Telemetry.PromptTokens, r.Telemetry.CompletionTokens, prompt)
		}
		if m.endsOf() != "keep" || held != prompt+8 {
			t.Errorf("ends %s, the bucket holds %d tokens — want Keep of the reservation %d (the estimate %d is not below it)",
				m.endsOf(), held, prompt+8, prompt+8)
		}
	})
	t.Run("max_tokens 8, prompt_n 1, a frame of 30 runes, client gone", func(t *testing.T) {
		hold := make(chan struct{})
		defer close(hold)
		body := streamBody("chat", `"max_tokens":8`)
		text := "abcdefghij abcdefghij abcdefgh"
		frame := strings.TrimSuffix(llamaChunk(text, ""), "}") + `,"timings":{"prompt_n":1}}`
		u := newUpstream(t, "")
		u.setRespond(sseRespond([]sseStep{{data: frame}, {hold: hold}}, nil, nil))
		rig, m := streamMeterRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
		ctx, cancel := context.WithCancel(context.Background())
		ls, each := openAndRead(t, ctx, serveHTTP(t, rig.router), body)
		if _, ok := nextData(each, 5*time.Second); !ok {
			cancel()
			t.Fatal("no frame")
		}
		cancel()
		_ = ls.Close()
		if !waitEnded(m) || !waitFor(2*time.Second, func() bool { return len(rig.rows.all()) == 1 }) {
			t.Fatal("no end or no row")
		}
		held, r := m.held(t, rig), rig.rows.all()[0]
		t.Logf("max_tokens 8, prompt_n 1, %d runes streamed (TokenEstimate %d), client gone: ends %s; the bucket holds %d tokens (reservation %d); row %q %d/%d",
			len([]rune(text)), promptguard.TokenEstimate(len([]rune(text))), m.endsOf(), held, promptEstimate(body)+8, r.Gateway.Usage,
			r.Telemetry.PromptTokens, r.Telemetry.CompletionTokens)
		// The estimate 1 + min(17, 8) = 9 is below the reservation 18 + 8 =
		// 26: Settle 9 (without the max_tokens bound: Settle 1 + 17 = 18).
		if m.endsOf() != "settle" || held != 9 || r.Telemetry.PromptTokens != 1 || r.Telemetry.CompletionTokens != 8 {
			t.Errorf("ends %s, the bucket holds %d tokens, row %d/%d — want Settle of the estimate 1 + 8 = 9",
				m.endsOf(), held, r.Telemetry.PromptTokens, r.Telemetry.CompletionTokens)
		}
	})
	big := strings.Replace(streamBody("chat"), `"content":"hi"`, `"content":"`+strings.Repeat("abcdefgh", 1000)+`"`, 1)
	image := `{"model":"visbig","stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"what is this"},` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,` + strings.Repeat("A", 300000) + `"}}]}]}`
	visbig := func(r *chatRig) {
		r.reg.reload(t, append(chatAliases(), chatAlias{"00000000-0000-4000-8000-0000000c4a7b", backends.GlobalScope, "visbig", "chat", "chat",
			backends.SensCredentials, `{"vision":true,"vision_max_bytes":4000000}`, true}))
	}
	for _, tc := range []struct {
		name   string
		lim    keyquota.Limits
		body   string
		opts   []rigOption
		prompt int64
		wait   string
	}{
		{"a prompt above the bucket, TPM 2 000", keyquota.Limits{RPM: 1000, TPM: 2000, CompletionReserve: 1024, Concurrency: noL1}, big, nil, 4462, "31260"},
		{"vision under the default chat TPM 60 000", keyquota.Limits{RPM: 30, TPM: 60000, CompletionReserve: 1024, Concurrency: noL1}, image, []rigOption{visbig}, 166741, "1042"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if p := promptEstimate(tc.body); p != tc.prompt {
				t.Fatalf("prompt estimate %d, want %d", p, tc.prompt)
			}
			keepEnds, keepStatus, keepWait := cutThenNext(t, tc.lim, tc.body, true, tc.opts...)
			cutEnds, cutStatus, cutWait := cutThenNext(t, tc.lim, tc.body, false, tc.opts...)
			t.Logf("%s: cut before the status: ends %s, next request %d retry-after-ms %s; cut after the first frame: ends %s, next request %d retry-after-ms %s",
				tc.name, keepEnds, keepStatus, keepWait, cutEnds, cutStatus, cutWait)
			if keepEnds != "keep" || keepStatus != http.StatusTooManyRequests || keepWait != tc.wait {
				t.Fatalf("cut before the status: ends %s, next %d after %s ms — want Keep and a 429 after %s ms", keepEnds, keepStatus, keepWait, tc.wait)
			}
			if cutEnds != "keep" || cutStatus != http.StatusTooManyRequests || cutWait != tc.wait {
				t.Errorf("cut after the first frame: ends %s, next %d after %s ms — want Keep and the wait of a kept reservation, %s ms",
					cutEnds, cutStatus, cutWait, tc.wait)
			}
		})
	}
}

// F4-2 (Delta-Review 4): a panic while the stream writer holds its mutex — a
// writer of the chain in the relay's write on the handler's goroutine, the
// header source in the grace commit on the timer's — must not leave the
// mutex held: the route's close() freezes the writer under it.

// relayPanicWriter panics on the n-th data frame written through it.
type relayPanicWriter struct {
	http.ResponseWriter
	n     int
	count *atomic.Int64
}

func (p relayPanicWriter) Write(b []byte) (int, error) {
	if strings.HasPrefix(string(b), "data: {") && int(p.count.Add(1)) == p.n {
		panic("hardening 3: panic in the relay's write")
	}
	return p.ResponseWriter.Write(b)
}

func (p relayPanicWriter) Unwrap() http.ResponseWriter { return p.ResponseWriter }

// gatedPanicWriter holds the data frames from the second on until gate is
// closed, then panics like relayPanicWriter (A03-W10: a panic of the client
// writer after the upstream ended).
type gatedPanicWriter struct {
	relayPanicWriter
	gate <-chan struct{}
}

func (p gatedPanicWriter) Write(b []byte) (int, error) {
	if strings.HasPrefix(string(b), "data: {") && p.count.Load() >= 1 {
		<-p.gate
	}
	return p.relayPanicWriter.Write(b)
}

func (p gatedPanicWriter) Unwrap() http.ResponseWriter { return p.ResponseWriter }

// tickPanicMeter is the rig's endMeter whose reservations' header source
// panics when the stream's timer goroutine reads it (the grace commit).
type tickPanicMeter struct{ *endMeter }

func (m tickPanicMeter) Admit(ctx context.Context, ca *auth.ConsumerAuth, a modelalias.Alias, est oai.Estimate) (oai.LimitReservation, *oai.LimitRefusal) {
	res, ref := m.endMeter.Admit(ctx, ca, a, est)
	if res == nil {
		return nil, ref
	}
	return tickPanicReservation{res}, nil
}

type tickPanicReservation struct{ oai.LimitReservation }

func (r tickPanicReservation) Headers() http.Header {
	if strings.Contains(string(debug.Stack()), "(*streamWriter).tick") {
		panic("hardening 3: the header source panics at the grace commit")
	}
	return r.LimitReservation.Headers()
}

// writerGoroutines counts the goroutines inside a method of the stream
// writer — a handler or a timer waiting on its mutex.
func writerGoroutines() int {
	buf := make([]byte, 1<<22)
	buf = buf[:runtime.Stack(buf, true)]
	n := 0
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, "oai.(*streamWriter)") {
			n++
		}
	}
	return n
}

// serveUnwaited serves h on a plain server whose cleanup closes the listener
// without waiting for handlers: a hung handler fails the gate instead of
// hanging the test binary.
func serveUnwaited(t *testing.T, h http.Handler) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = hs.Serve(ln) }()
	t.Cleanup(func() { _ = ln.Close() })
	return "http://" + ln.Addr().String()
}

// recovered reports whether oai.Recover logged a recovered panic whose error
// names want.
func recovered(sink *logSink, want string) bool {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	for _, r := range sink.recs {
		if r.Message != "gateway: panic recovered" {
			continue
		}
		found := false
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "error" && strings.Contains(fmt.Sprint(a.Value.Any()), want) {
				found = true
				return false
			}
			return true
		})
		if found {
			return true
		}
	}
	return false
}

// TestStreamPanicUnderTheWriterLock is F4-2 (Delta-Review 4): the handler
// returns within 6 s, the panic reaches oai.Recover (its log names it), the
// reservation keeps its charge (the backend had the request), the lease and
// the in-flight budget are free and no goroutine is left in the writer.
//
//   - the relay's write panics at the second frame (handler goroutine): red
//     before — close() blocks on the mutex the panicking frame held, the
//     reservation never ends, 2 MiB of in-flight budget stay held;
//   - the header source panics at the grace commit (timer goroutine, before
//     the status): red before — the timer's panic is unrecovered and ends
//     the process. Now the timer catches it, ends the run, and the handler
//     raises it again after freeze: the client gets oai.Recover's 503
//     internal_error.
func TestStreamPanicUnderTheWriterLock(t *testing.T) {
	check := func(t *testing.T, rig *chatRig, m *endMeter, host string, returned <-chan struct{}, sink *logSink, want string) {
		t.Helper()
		select {
		case <-returned:
		case <-time.After(6 * time.Second):
			lease, _ := heldOn(rig.disp, host)
			t.Fatalf("the handler did not return within 6 s: ends %q, lease %d, in-flight budget %d, writer goroutines %d",
				m.endsOf(), lease, rig.budget.InUse(), writerGoroutines())
		}
		if !waitEnded(m) {
			t.Fatal("the reservation never ended")
		}
		budgetFree := waitFor(2*time.Second, func() bool { return rig.budget.InUse() == 0 })
		leaseFree := waitHeld(rig.disp, host, 0, 2*time.Second)
		noWriter := waitFor(2*time.Second, func() bool { return writerGoroutines() == 0 })
		held := m.held(t, rig)
		t.Logf("%s: ends %s, the bucket holds %d tokens; in-flight budget %d, lease free %v, writer goroutines %d; oai.Recover logged it %v",
			want, m.endsOf(), held, rig.budget.InUse(), leaseFree, writerGoroutines(), recovered(sink, want))
		if m.endsOf() != "keep" || held != wireReservation {
			t.Errorf("ends %s, the bucket holds %d tokens — want Keep of %d (a panic after wire contact)", m.endsOf(), held, wireReservation)
		}
		if !budgetFree || !leaseFree || !noWriter {
			t.Errorf("in-flight budget %d, lease free %v, writer goroutines %d — want 0, true, 0", rig.budget.InUse(), leaseFree, writerGoroutines())
		}
		if !recovered(sink, want) {
			t.Errorf("oai.Recover logged no recovered panic naming %q", want)
		}
	}
	t.Run("the relay's write panics at the second frame", func(t *testing.T) {
		sink := captureLog(t)
		u := newUpstream(t, "")
		// The upstream is still streaming when the client writer panics
		// (A03-W10: the writer drains on its own goroutine — the panic ends
		// the run, the upstream is aborted, close() keeps the charge). A
		// panic after the upstream ended is the drain's case below.
		hold := make(chan struct{})
		defer close(hold)
		u.setRespond(sseRespond([]sseStep{{data: sglangChunk("a", "")}, {data: sglangChunk("b", "")}, {hold: hold}, {data: sglangChunk("c", "stop")}, {data: "[DONE]"}}, nil, nil))
		rig, m := streamMeterRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
		var count atomic.Int64
		returned := make(chan struct{})
		url := serveUnwaited(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer close(returned)
			defer func() { _ = recover() }()
			rig.router.ServeHTTP(relayPanicWriter{ResponseWriter: w, n: 2, count: &count}, r)
		}))
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		answer := make(chan streamAnswer, 1)
		go func() { answer <- postStream(ctx, t, &httptest.Server{URL: url}, streamBody("chat")) }()
		check(t, rig, m, u.srv.URL, returned, sink, "panic in the relay's write")
		select {
		case a := <-answer:
			if a.status != http.StatusOK || a.readErr == nil {
				t.Errorf("answer %d, read error %v — want the committed stream cut (no clean end)", a.status, a.readErr)
			}
		case <-time.After(3 * time.Second):
			t.Error("the client's read did not end")
		}
	})
	t.Run("the client writer panics after the upstream ended", func(t *testing.T) {
		// A03-W10 hardening 1 (Fremd-Review 2 F3b): the lease ended with the
		// upstream, the reservation — the consumer's slot — stays open while
		// the client writer drains; a panic of the writer during the drain
		// ends it through close(): Keep after wire contact, the rule of a
		// panic (W02-08 F4-2, exit 20 again). The panic still reaches
		// oai.Recover, nothing is left.
		sink := captureLog(t)
		end := newUpstreamEnd()
		u := newUpstream(t, "")
		u.setRespond(sseRespond([]sseStep{{data: sglangChunk("a", "")}, {data: sglangChunk("b", "")}, {data: sglangChunk("c", "stop")}, {data: sglangUsage}, {data: "[DONE]"}}, nil, end))
		rig, m := streamMeterRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
		var count atomic.Int64
		returned := make(chan struct{})
		gate := make(chan struct{})
		open := sync.OnceFunc(func() { close(gate) })
		defer open() // a failed check must not leave the writer parked for the next case
		url := serveUnwaited(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer close(returned)
			defer func() { _ = recover() }()
			rig.router.ServeHTTP(gatedPanicWriter{relayPanicWriter: relayPanicWriter{ResponseWriter: w, n: 2, count: &count}, gate: gate}, r)
		}))
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		go func() { _ = postStream(ctx, t, &httptest.Server{URL: url}, streamBody("chat")) }()
		if !end.wait(5*time.Second) || end.canceled {
			t.Fatal("the upstream did not run to its end")
		}
		if !waitHeld(rig.disp, u.srv.URL, 0, 2500*time.Millisecond) {
			t.Fatal("the lease did not end with the upstream")
		}
		time.Sleep(50 * time.Millisecond)
		if got := m.endsOf(); got != "" {
			t.Fatalf("ends %q while the writer drains, want none (the slot ends at the drain's end)", got)
		}
		open() // now the writer reaches its second frame and panics
		select {
		case <-returned:
		case <-time.After(6 * time.Second):
			t.Fatal("the handler did not return within 6 s")
		}
		if got := m.endsOf(); got != "keep" {
			t.Fatalf("ends %s, want keep (a panic after wire contact ends the reservation through close())", got)
		}
		if !waitHeld(rig.disp, u.srv.URL, 0, 2*time.Second) || !waitFor(2*time.Second, func() bool { return rig.budget.InUse() == 0 && writerGoroutines() == 0 }) {
			t.Fatalf("lease, budget %d or writer goroutines %d left", rig.budget.InUse(), writerGoroutines())
		}
		if !recovered(sink, "panic in the relay's write") {
			t.Error("oai.Recover logged no recovered panic of the client writer")
		}
	})
	t.Run("the header source panics at the grace commit", func(t *testing.T) {
		fastPhaseB(t, 200*time.Millisecond)
		sink := captureLog(t)
		hold := make(chan struct{})
		defer close(hold)
		u := newUpstream(t, "")
		u.setRespond(sseRespond([]sseStep{{hold: hold}}, nil, nil))
		m := newEndMeter()
		rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings,
			withWait(100*time.Millisecond), withStream(0, 10*time.Second, 5*time.Second),
			withDeps(func(d *oai.Deps) { d.Limiter = tickPanicMeter{m} }))
		returned := make(chan struct{})
		url := serveUnwaited(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer close(returned)
			rig.router.ServeHTTP(w, r)
		}))
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		answer := make(chan streamAnswer, 1)
		go func() { answer <- postStream(ctx, t, &httptest.Server{URL: url}, streamBody("chat")) }()
		check(t, rig, m, u.srv.URL, returned, sink, "the header source panics at the grace commit")
		select {
		case a := <-answer:
			if a.status != http.StatusServiceUnavailable || !strings.Contains(a.raw, `"internal_error"`) {
				t.Errorf("answer %d %q — want oai.Recover's 503 internal_error (the panic came before the status)", a.status, a.raw)
			}
		case <-time.After(3 * time.Second):
			t.Error("the client got no answer")
		}
	})
}

// F4-5 (Delta-Review 4): the stream half of the wire trace — the retry of
// httpx.DoRetryOnce and the failover to a second row both report wire
// contact into the stream's reservation, and the context rule of
// wireContext (done) runs for the stream's wire call.

// TestStreamMeterRetriesReachTheTrace: a stream whose first try reached no
// backend books what its next try streamed (usage 11 + 3 by Settle), never a
// refund by Abort.
//
//   - DoRetryOnce's retry after a reset TLS handshake (h2, no request byte in
//     the first try) — red: O10 (the retry's context hides the trace), T3
//     (the trace marks only the first dial);
//   - the failover to a second row after a refused connect (a second
//     attempt, a second wireContext) — red: R5 (the trace only on the
//     request's first wire call).
func TestStreamMeterRetriesReachTheTrace(t *testing.T) {
	t.Run("DoRetryOnce after a reset handshake (h2)", func(t *testing.T) {
		rl := &resetFirst{Listener: newHeldListener(t)}
		var hits atomic.Int64
		steps := sseRespond(streamShapes[0].steps(), nil, nil)
		url := serveH2(t, rl, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			_, _ = io.ReadAll(r.Body)
			steps(w, r)
		}))
		rig, m := streamMeterRig(t, []backends.Backend{wireRow("flaky", url, 10)}, 4, 1)
		a := postStream(context.Background(), t, serveHTTP(t, rig.router), streamBody("chat"))
		if !waitEnded(m) {
			t.Fatal("the reservation never ended")
		}
		held := m.held(t, rig)
		t.Logf("stream, handshake reset, retry served: answer %d, [DONE] %d; accepted %d, handler hits %d; ends %s; the bucket holds %d tokens",
			a.status, a.dones(), rl.n.Load(), hits.Load(), m.endsOf(), held)
		if a.status != http.StatusOK || a.dones() != 1 || rl.n.Load() != 2 || hits.Load() != 1 {
			t.Fatalf("answer %d, [DONE] %d, accepted %d, hits %d — want the retry's whole stream", a.status, a.dones(), rl.n.Load(), hits.Load())
		}
		if held != 14 || m.endsOf() != "settle" {
			t.Errorf("the bucket holds %d tokens (ends %s), want the retry's usage 14 by Settle", held, m.endsOf())
		}
	})
	t.Run("failover after a refused connect", func(t *testing.T) {
		u := newUpstream(t, "")
		u.setRespond(sseRespond(streamShapes[0].steps(), nil, nil))
		rig, m := streamMeterRig(t, []backends.Backend{wireRow("refused", "http://"+refusedAddr(t), 20), u.row("two", backends.TrustFull, 10)}, 4, 1)
		a := postStream(context.Background(), t, serveHTTP(t, rig.router), streamBody("chat"))
		if !waitEnded(m) {
			t.Fatal("the reservation never ended")
		}
		held := m.held(t, rig)
		t.Logf("stream, first row refused, second row streams: answer %d, [DONE] %d; second row hits %d; ends %s; the bucket holds %d tokens",
			a.status, a.dones(), u.hits.Load(), m.endsOf(), held)
		if a.status != http.StatusOK || a.dones() != 1 || u.hits.Load() != 1 {
			t.Fatalf("answer %d, [DONE] %d, second row hits %d — want the second row's whole stream", a.status, a.dones(), u.hits.Load())
		}
		if held != 14 || m.endsOf() != "settle" {
			t.Errorf("the bucket holds %d tokens (ends %s), want the second row's usage 14 by Settle", held, m.endsOf())
		}
	})
}

// TestStreamMeterDeadlineWithAConnectionKeeps is the context rule for the
// stream (wireContext's done; TestMeterWireHTTP2 pins it non-stream): an
// HTTP/2 stream whose attempt deadline ends while it holds a connection,
// before its HEADERS frame is written — net/http's HTTP/2 client returns on
// the context without waiting for the stream's writer, which may still
// write — keeps its reservation. Red: O8 (done never called in the stream:
// Abort), O9 (the rule removed).
func TestStreamMeterDeadlineWithAConnectionKeeps(t *testing.T) {
	var hits atomic.Int64
	url := serveH2(t, newHeldListener(t), http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	rig, m := streamMeterRig(t, []backends.Backend{noTimeouts(wireRow("h2slow", url, 10))}, 4, 1)
	shortAttempts(rig)
	pt := &h2Trace{}
	stall := pt.trace()
	record := stall.GotConn
	stall.GotConn = func(i httptrace.GotConnInfo) {
		record(i)
		time.Sleep(400 * time.Millisecond)
	}
	a := postStreamTraced(t, rig, stall)
	if !waitEnded(m) {
		t.Fatal("the reservation never ended")
	}
	held := m.held(t, rig)
	t.Logf("h2 stream, deadline before the headers: answer %d; connections %s; handler hits %d; ends %s; the bucket holds %d tokens",
		a.status, pt, hits.Load(), m.endsOf(), held)
	if pt.String() != "h2" || hits.Load() != 0 {
		t.Fatalf("connections %q, handler hits %d — want one h2 connection and no request", pt, hits.Load())
	}
	if held != wireReservation || m.endsOf() != "keep" {
		t.Errorf("the bucket holds %d tokens (ends %s), want the reservation %d kept (the writer may still write)", held, m.endsOf(), wireReservation)
	}
}

// settlePanicMeter is the rig's endMeter whose reservations panic in Settle —
// on the handler's goroutine, in answerStream's book: since hardening 1
// (Fremd-Review 2 F3b) after the relay's drain, no longer at the upstream's
// end.
type settlePanicMeter struct{ *endMeter }

func (m settlePanicMeter) Admit(ctx context.Context, ca *auth.ConsumerAuth, a modelalias.Alias, est oai.Estimate) (oai.LimitReservation, *oai.LimitRefusal) {
	res, ref := m.endMeter.Admit(ctx, ca, a, est)
	if res == nil {
		return nil, ref
	}
	return settlePanicReservation{res}, nil
}

type settlePanicReservation struct{ oai.LimitReservation }

func (settlePanicReservation) Settle(dispatch.Usage) {
	panic("A03-W10: the meter panics at its end")
}

// TestRelayWriterJoinedOnAReaderPanic (Fremd-Review 1 F2, through the route):
// the relay's writer is stuck in a write to a client that does not read, the
// meter panics at its end — since hardening 1 (F3b) the meter ends after the
// drain, so the writer is cut by client_write_window (500 ms) first; the
// handler returns with no goroutine left in the stream writer and the panic
// reaches oai.Recover. The join of a writer that still runs when the reader
// panics is pinned without the route: TestRelayCloseJoinsTheWriter (R13).
func TestRelayWriterJoinedOnAReaderPanic(t *testing.T) {
	sink := captureLog(t)
	u := newUpstream(t, "")
	u.setRespond(sseRespond(bigFrames(8, 32<<10, 0), nil, nil))
	m := newEndMeter()
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings,
		withStream(40*time.Second, 15*time.Second, 500*time.Millisecond),
		withDeps(func(d *oai.Deps) { d.Limiter = settlePanicMeter{m} }))
	returned := make(chan int, 1)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { returned <- writerGoroutines() }()
		defer func() { _ = recover() }()
		rig.router.ServeHTTP(w, r)
	}))
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
	select {
	case n := <-returned:
		if n != 0 {
			t.Fatalf("%d goroutines still in the stream writer when the handler returned — close() did not join the relay's writer", n)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the handler did not return")
	}
	if !recovered(sink, "the meter panics at its end") {
		t.Error("oai.Recover logged no recovered panic of the meter")
	}
}
