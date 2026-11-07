//go:build linux

package oai_test

// The L1 slot of the per-consumer meter (real keyquota meter, Concurrency 1)
// over every exit of the stream path — the probe of Delta-Review 5
// (SA-SLOT5, W02-08 part C), taken into the tree by hardening 4 (F5-3):
// exits 1, 2, 3, 5, 8, 10′, 12, 15′, 17, 18 and 19 had no other stream gate
// counting their end or their slot. Each subtest drives one exit and then
// checks
//
//  1. the exit's reservation ended exactly once as expected — endMeter
//     records every end the route calls, before keyquota's sync.Once, so a
//     second call shows as "x,y";
//  2. at the route, the slot is free exactly once: a holding stream gets 200
//     and a frame, a probe beside it is 429 concurrency_limit_exceeded, and
//     the probe adds no end (it took no reservation);
//  3. at the meter: one direct admission on the consumer's chat key passes, a
//     second one beside it is refused for concurrency.
//
// One log line per exit: "R5 <exit>: ends=… | route: … | meter: …". Exits
// whose rig cannot serve a holding stream afterwards (no planner, connect
// refused, shutdown) are checked at the meter only.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/keyquota"
	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/GottZ/ctx/internal/oai"
)

// r5L1 is a consumer with one open chat request at a time.
var r5L1 = keyquota.Limits{RPM: 1000, TPM: 1 << 30, CompletionReserve: 1024, Concurrency: 1}

func r5Meter() *endMeter { return &endMeter{rigMeter: newRigMeter(r5L1, true)} }

// r5HoldOn programs the upstreams for the holding stream: one frame, then
// hold until release, then the end and [DONE].
func r5HoldOn(us ...*upstream) func(<-chan struct{}) {
	return func(release <-chan struct{}) {
		for _, u := range us {
			u.setRespond(sseRespond([]sseStep{{data: sglangChunk("held", "")}, {hold: release},
				{data: sglangChunk(" end", "stop")}, {data: "[DONE]"}}, nil, nil))
		}
	}
}

// r5Code is the error code of an answer body or error frame ("" if none).
func r5Code(raw string) string {
	var e struct {
		Error struct{ Code string } `json:"error"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(raw)), &e) == nil && e.Error.Code != "" {
		return e.Error.Code
	}
	if i := strings.Index(raw, `"code":"`); i >= 0 {
		rest := raw[i+8:]
		if j := strings.IndexByte(rest, '"'); j >= 0 {
			return rest[:j]
		}
	}
	return ""
}

// r5MeterSlot admits twice on the consumer's chat key directly: the first
// must pass (the slot is free), the second must be refused for concurrency
// (it is free once). Both are ended at once.
func r5MeterSlot(m *endMeter, rig *chatRig) (string, bool) {
	ca := *rig.ca.Load()
	s := keyquota.Subject{ConsumerID: ca.ConsumerID, TenantID: ca.TenantID, Op: keyquota.OpChat, Lane: keyquota.LaneUrgent}
	r1, d1 := m.m.Admit(s, m.lim, keyquota.Estimate{ClientMax: true})
	r2, d2 := m.m.Admit(s, m.lim, keyquota.Estimate{ClientMax: true})
	if r1 != nil {
		r1.Abort()
	}
	if r2 != nil {
		r2.Abort()
	}
	ok := d1.Allowed && !d2.Allowed && d2.Reason == keyquota.ReasonConcurrency
	return fmt.Sprintf("first allowed=%v, second allowed=%v reason=%s", d1.Allowed, d2.Allowed, d2.Reason), ok
}

// r5Check is the slot check after one exit (see the file comment). program
// (nil: meter only) programs the rig's backends for the holding stream.
func r5Check(t *testing.T, name string, m *endMeter, rig *chatRig, srv *httptest.Server, want string, program func(<-chan struct{})) {
	t.Helper()
	if !waitFor(5*time.Second, func() bool { return endCount(m) >= 1 }) {
		t.Fatalf("R5 %s: the exit's reservation never ended (ends %q)", name, m.endsOf())
	}
	time.Sleep(150 * time.Millisecond) // a second end of the same request would land here
	ends := m.endsOf()
	if ends != want {
		t.Errorf("R5 %s: ends %q, want exactly one %q", name, ends, want)
	}
	route := "n/a (meter only)"
	if program != nil {
		release := make(chan struct{})
		program(release)
		ctx, cancel := context.WithCancel(context.Background())
		ls, each := openAndRead(t, ctx, srv, streamBody("chat"))
		_, frame := nextData(each, 5*time.Second)
		status := 0
		if r := ls.response(time.Second); r != nil {
			status = r.StatusCode
		}
		probe := postStream(context.Background(), t, srv, streamBody("chat"))
		code := r5Code(probe.raw)
		close(release)
		done := false
		for !done {
			e, ok := nextData(each, 5*time.Second)
			done = !ok || e.data == "[DONE]"
		}
		_ = ls.Close()
		cancel()
		holderEnded := waitFor(5*time.Second, func() bool { return endCount(m) >= 2 })
		time.Sleep(100 * time.Millisecond)
		total := endCount(m)
		route = fmt.Sprintf("holder %d frame=%v, probe %d %s, ends after the holder %d (%s)", status, frame, probe.status, code, total, m.endsOf())
		if status != http.StatusOK || !frame {
			t.Errorf("R5 %s: the holding stream got %d frame=%v — the slot was not freed", name, status, frame)
		}
		if probe.status != http.StatusTooManyRequests || code != oai.CodeConcurrencyLimitExceeded {
			t.Errorf("R5 %s: the probe beside the holder got %d %q — want 429 concurrency_limit_exceeded (freed once)", name, probe.status, code)
		}
		if !holderEnded || total != 2 {
			t.Errorf("R5 %s: %d ends after the holder (%s), want 2 (the exit's and the holder's; the probe took none)", name, total, m.endsOf())
		}
	}
	meter, ok := r5MeterSlot(m, rig)
	if !ok {
		t.Errorf("R5 %s: at the meter %s — want the first allowed and the second refused for concurrency", name, meter)
	}
	t.Logf("R5 %s: ends=%s | route: %s | meter: %s", name, ends, route, meter)
}

// r5Serve serves h on a plain http.Server without a cleanup that waits for
// its handlers.
func r5Serve(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = hs.Serve(ln) }()
	t.Cleanup(func() { _ = hs.Close() })
	return &httptest.Server{URL: "http://" + ln.Addr().String(), Listener: ln, Config: hs}
}

// r5Upstream502 answers status after wait (a backend failure without 200).
func r5Upstream502(status int, wait time.Duration) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(wait):
		case <-r.Context().Done():
			return
		}
		if status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "1")
		}
		w.WriteHeader(status)
	}
}

// r5OKThenReset sends a 200 (SSE), frames, then resets the connection.
func r5OKThenReset(wait time.Duration, frames ...string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(wait):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		rc := http.NewResponseController(w)
		_ = rc.Flush()
		for _, f := range frames {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", f)
			_ = rc.Flush()
		}
		time.Sleep(20 * time.Millisecond)
		if conn, _, err := rc.Hijack(); err == nil {
			reset(conn)
		}
	}
}

// r5HoldNoStatus reads the request and holds without a status line.
func r5HoldNoStatus(entered chan<- struct{}) func(http.ResponseWriter, *http.Request) {
	return func(_ http.ResponseWriter, r *http.Request) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-r.Context().Done()
	}
}

// r5Writer wraps the response writer: panicAt panics on the n-th data frame
// (counted over all requests), failDone fails the write of [DONE] once.
type r5Writer struct {
	http.ResponseWriter
	panicAt  int64
	count    *atomic.Int64
	failDone *atomic.Bool
}

func (w r5Writer) Write(b []byte) (int, error) {
	if w.panicAt > 0 && strings.Contains(string(b), "data: {") && w.count.Add(1) == w.panicAt {
		panic("R5: panic in the relay's write")
	}
	if w.failDone != nil && strings.Contains(string(b), "data: [DONE]") && w.failDone.CompareAndSwap(true, false) {
		return 0, io.ErrClosedPipe
	}
	return w.ResponseWriter.Write(b)
}

func (w r5Writer) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w r5Writer) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// r5Planner wraps the rig's planner with exits before the wire.
type r5Planner struct {
	oai.AdmissionPlanner
	mode   atomic.Int32
	cancel atomic.Pointer[context.CancelFunc]
}

const (
	r5PlanNormal        int32 = iota
	r5PlanCancelError         // the client goes away in Plan, Plan fails (exit 3)
	r5PlanRefuse              // Plan refuses (exit 4)
	r5PlanCancelOK            // the client goes away in Plan, Plan succeeds (exit 5)
	r5PlanPanic               // Plan panics (exit 19)
	r5PlanEmpty               // Plan returns no row (exit 18)
	r5PlanCancelAcquire       // the client goes away right after First (exit 8)
)

func (p *r5Planner) doCancel() {
	if f := p.cancel.Load(); f != nil {
		(*f)()
	}
}

func (p *r5Planner) Plan(ctx context.Context, ca *auth.ConsumerAuth, in oai.AdmissionInput) (oai.Plan, error) {
	switch p.mode.Load() {
	case r5PlanCancelError:
		p.doCancel()
		return oai.Plan{}, handler.ErrChainNotOffered
	case r5PlanRefuse:
		return oai.Plan{}, handler.ErrChainNotOffered
	case r5PlanCancelOK:
		p.doCancel()
	case r5PlanPanic:
		panic("R5: the planner panics before any wire contact")
	case r5PlanEmpty:
		plan, err := p.AdmissionPlanner.Plan(ctx, ca, in)
		plan.Chain = nil
		return plan, err
	case r5PlanCancelAcquire:
		plan, err := p.AdmissionPlanner.Plan(ctx, ca, in)
		if err == nil {
			plan.Acquirer = r5CancelFirst{RequestAcquirer: plan.Acquirer, p: p}
		}
		return plan, err
	}
	return p.AdmissionPlanner.Plan(ctx, ca, in)
}

type r5CancelFirst struct {
	oai.RequestAcquirer
	p *r5Planner
}

func (a r5CancelFirst) First(ctx context.Context, b *backends.Backend, role string, timeout time.Duration) (*dispatch.Lease, context.Context, error) {
	l, c, err := a.RequestAcquirer.First(ctx, b, role, timeout)
	a.p.doCancel()
	return l, c, err
}

// r5CancelLimiter cancels the request right after the meter admitted it
// (exit 1: the run ended before the planner).
type r5CancelLimiter struct {
	*endMeter
	cancel *atomic.Pointer[context.CancelFunc]
}

func (l r5CancelLimiter) Admit(ctx context.Context, ca *auth.ConsumerAuth, a modelalias.Alias, est oai.Estimate) (oai.LimitReservation, *oai.LimitRefusal) {
	res, ref := l.endMeter.Admit(ctx, ca, a, est)
	if res != nil {
		if f := l.cancel.Load(); f != nil {
			(*f)()
		}
	}
	return res, ref
}

// r5HeaderPanic is endMeter whose next reservation's Headers panics once
// (the header source at the grace commit, on the timer goroutine: exit 21).
type r5HeaderPanic struct {
	*endMeter
	armed *atomic.Bool
}

func (l r5HeaderPanic) Admit(ctx context.Context, ca *auth.ConsumerAuth, a modelalias.Alias, est oai.Estimate) (oai.LimitReservation, *oai.LimitRefusal) {
	res, ref := l.endMeter.Admit(ctx, ca, a, est)
	if res == nil {
		return nil, ref
	}
	return r5PanicHeaders{LimitReservation: res, armed: l.armed}, nil
}

type r5PanicHeaders struct {
	oai.LimitReservation
	armed *atomic.Bool
}

func (r r5PanicHeaders) Headers() http.Header {
	if r.armed.CompareAndSwap(true, false) {
		panic("R5: the header source panics at the grace commit")
	}
	return r.LimitReservation.Headers()
}

// r5Recorded runs one stream request into a recorder; cancelSlot gets the
// request's cancel before it runs and is cleared after it.
func r5Recorded(rig *chatRig, cancelSlot *atomic.Pointer[context.CancelFunc]) *httptest.ResponseRecorder {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelSlot.Store(&cancel)
	defer cancelSlot.Store(nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(streamBody("chat"))).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	func() {
		defer func() { _ = recover() }()
		rig.router.ServeHTTP(w, req)
	}()
	return w
}

// TestStreamEveryExitFreesTheSlotOnce drives every exit of the stream path's
// table (bau report W02-08, "Ausgangs-Tabelle mit Slot-Spalte") through a
// consumer with one slot and checks that the slot comes back exactly once.
func TestStreamEveryExitFreesTheSlotOnce(t *testing.T) {
	one := func(u *upstream) []backends.Backend {
		return []backends.Backend{u.row("spark", backends.TrustFull, 10)}
	}

	t.Run("0b budget refuses every row's upstream body before the commit", func(t *testing.T) {
		u1, u2 := newUpstream(t, ""), newUpstream(t, "")
		r1, r2 := u1.row("one", backends.TrustFull, 20), u2.row("two", backends.TrustFull, 10)
		r1.ExtraBody = map[string]any{"vendor_blob": strings.Repeat("y", 64<<10)}
		r2.ExtraBody = map[string]any{"vendor_blob": strings.Repeat("z", 96<<10)}
		m := r5Meter()
		rig := newChatRig(t, []backends.Backend{r1, r2}, 4, 1, withStreamSettings, withDeps(func(d *oai.Deps) { d.Limiter = m }))
		srv := serveHTTP(t, rig.router)
		body := streamBody("chat")
		rig.setSettings(func(s *oai.Settings) { s.InflightBudget = 3*int64(len(body)) + 2*s.MaxStreamFrameBytes + 8<<10 })
		a := postStream(context.Background(), t, srv, body)
		t.Logf("R5 0b: answer %d %s, wire hits %d/%d", a.status, r5Code(a.raw), u1.hits.Load(), u2.hits.Load())
		if a.status != http.StatusServiceUnavailable || r5Code(a.raw) != oai.CodeServerOverloaded || u1.hits.Load()+u2.hits.Load() != 0 {
			t.Errorf("answer %d %q, hits %d — want 503 server_overloaded before any wire", a.status, a.raw, u1.hits.Load()+u2.hits.Load())
		}
		rig.setSettings(func(s *oai.Settings) { s.InflightBudget = 1 << 30 })
		r5Check(t, "0b budget before the commit", m, rig, srv, "abort", r5HoldOn(u1, u2))
	})

	t.Run("0c budget refuses the failover row after the grace commit", func(t *testing.T) {
		fastPhaseB(t, 100*time.Millisecond)
		u1, u2 := newUpstream(t, ""), newUpstream(t, "")
		u1.setRespond(r5Upstream502(http.StatusBadGateway, 400*time.Millisecond))
		r1, r2 := u1.row("one", backends.TrustFull, 20), u2.row("two", backends.TrustFull, 10)
		r2.ExtraBody = map[string]any{"vendor_blob": strings.Repeat("z", 96<<10)}
		m := r5Meter()
		rig := newChatRig(t, []backends.Backend{r1, r2}, 4, 1, withStreamSettings, withWait(100*time.Millisecond),
			withStream(0, 10*time.Second, 5*time.Second), withDeps(func(d *oai.Deps) { d.Limiter = m }))
		srv := serveHTTP(t, rig.router)
		body := streamBody("chat")
		rig.setSettings(func(s *oai.Settings) { s.InflightBudget = 3*int64(len(body)) + 2*s.MaxStreamFrameBytes + 8<<10 })
		a := postStream(context.Background(), t, srv, body)
		code, last := a.errorFrame(t)
		t.Logf("R5 0c: answer %d, frame %q last=%v, hits %d/%d", a.status, code, last, u1.hits.Load(), u2.hits.Load())
		if a.status != http.StatusOK || code != oai.CodeServerOverloaded || u2.hits.Load() != 0 {
			t.Errorf("answer %d frame %q, row two hits %d — want 200 and the server_overloaded frame", a.status, code, u2.hits.Load())
		}
		rig.setSettings(func(s *oai.Settings) { s.InflightBudget = 1 << 30 })
		r5Check(t, "0c budget after the grace commit", m, rig, srv, "keep", r5HoldOn(u1, u2))
	})

	t.Run("1 run ended before the planner", func(t *testing.T) {
		u := newUpstream(t, "")
		m := r5Meter()
		var cs atomic.Pointer[context.CancelFunc]
		rig := newChatRig(t, one(u), 4, 1, withStreamSettings, withDeps(func(d *oai.Deps) { d.Limiter = r5CancelLimiter{m, &cs} }))
		w := r5Recorded(rig, &cs)
		t.Logf("R5 1: recorder %d, wire hits %d", w.Code, u.hits.Load())
		r5Check(t, "1 ended before the planner", m, rig, serveHTTP(t, rig.router), "abort", r5HoldOn(u))
	})

	t.Run("2 no planner", func(t *testing.T) {
		u := newUpstream(t, "")
		m := r5Meter()
		rig := newChatRig(t, one(u), 4, 1, withStreamSettings, withDeps(func(d *oai.Deps) { d.Limiter = m; d.Planner = nil }))
		a := postStream(context.Background(), t, serveHTTP(t, rig.router), streamBody("chat"))
		t.Logf("R5 2: answer %d %s", a.status, r5Code(a.raw))
		r5Check(t, "2 no planner", m, rig, nil, "abort", nil)
	})

	for _, tc := range []struct {
		name, want string
		mode       int32
		recorded   bool
	}{
		{"3 plan error on an ended run", "reject", r5PlanCancelError, true},
		{"4 plan refused", "reject", r5PlanRefuse, false},
		{"5 run ended after the plan", "abort", r5PlanCancelOK, true},
		{"8 run ended between acquire and wire", "abort", r5PlanCancelAcquire, true},
		{"18 no row in the plan", "abort", r5PlanEmpty, false},
		{"19 panic before wire contact", "abort", r5PlanPanic, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := newUpstream(t, "")
			m := r5Meter()
			p := &r5Planner{}
			rig := newChatRig(t, one(u), 4, 1, withStreamSettings, withDeps(func(d *oai.Deps) {
				p.AdmissionPlanner = d.Planner
				d.Planner = p
				d.Limiter = m
			}))
			srv := serveHTTP(t, rig.router)
			p.mode.Store(tc.mode)
			if tc.recorded {
				w := r5Recorded(rig, &p.cancel)
				t.Logf("R5 %s: recorder %d, wire hits %d", tc.name, w.Code, u.hits.Load())
			} else {
				a := postStream(context.Background(), t, srv, streamBody("chat"))
				t.Logf("R5 %s: answer %d %s, wire hits %d", tc.name, a.status, r5Code(a.raw), u.hits.Load())
			}
			if u.hits.Load() != 0 {
				t.Errorf("wire hits %d, want 0", u.hits.Load())
			}
			p.mode.Store(r5PlanNormal)
			r5Check(t, tc.name, m, rig, srv, tc.want, r5HoldOn(u))
		})
	}

	t.Run("6 first acquire refused (phase A)", func(t *testing.T) {
		u := newUpstream(t, "")
		m := r5Meter()
		rig := newChatRig(t, one(u), 2, 1, withStreamSettings, withWait(0), withDeps(func(d *oai.Deps) { d.Limiter = m }))
		srv := serveHTTP(t, rig.router)
		release := occupyExternal(t, rig.disp, u.srv.URL)
		a := postStream(context.Background(), t, srv, streamBody("chat"))
		t.Logf("R5 6: answer %d %s", a.status, r5Code(a.raw))
		release()
		r5Check(t, "6 first acquire refused", m, rig, srv, "reject", r5HoldOn(u))
	})

	t.Run("7 client gone in the queue", func(t *testing.T) {
		u := newUpstream(t, "")
		m := r5Meter()
		rig := newChatRig(t, one(u), 2, 1, withStreamSettings, withWait(10*time.Second), withDeps(func(d *oai.Deps) { d.Limiter = m }))
		srv := serveHTTP(t, rig.router)
		release := occupyExternal(t, rig.disp, u.srv.URL)
		ctx, cancel := context.WithCancel(context.Background())
		ls, _ := openAndRead(t, ctx, srv, streamBody("chat"))
		waitExternalWaiting(t, rig.disp, u.srv.URL)
		cancel()
		_ = ls.Close()
		if !waitFor(5*time.Second, func() bool { return endCount(m) >= 1 }) {
			t.Fatal("no end")
		}
		release()
		r5Check(t, "7 client gone in the queue", m, rig, srv, "abort", r5HoldOn(u))
	})

	t.Run("9 failover refused before the commit", func(t *testing.T) {
		u1, u2 := newUpstream(t, ""), newUpstream(t, "")
		u1.setRespond(r5Upstream502(http.StatusBadGateway, 0))
		m := r5Meter()
		rig := newChatRig(t, []backends.Backend{u1.row("one", backends.TrustFull, 20), u2.row("two", backends.TrustFull, 10)}, 2, 1,
			withStreamSettings, withWait(100*time.Millisecond), withDeps(func(d *oai.Deps) { d.Limiter = m }))
		srv := serveHTTP(t, rig.router)
		release := occupyExternal(t, rig.disp, u2.srv.URL)
		a := postStream(context.Background(), t, srv, streamBody("chat"))
		t.Logf("R5 9a: answer %d %s, hits %d/%d", a.status, r5Code(a.raw), u1.hits.Load(), u2.hits.Load())
		if a.status != http.StatusTooManyRequests || u2.hits.Load() != 0 {
			t.Errorf("answer %d %q — want 429 before the commit, row two untouched", a.status, a.raw)
		}
		release()
		r5Check(t, "9 failover refused before the commit", m, rig, srv, "keep", r5HoldOn(u1, u2))
	})

	t.Run("9 grace commit, upstream 429, failover refused", func(t *testing.T) {
		fastPhaseB(t, 100*time.Millisecond)
		u1, u2 := newUpstream(t, ""), newUpstream(t, "")
		u1.setRespond(r5Upstream502(http.StatusTooManyRequests, 400*time.Millisecond))
		m := r5Meter()
		rig := newChatRig(t, []backends.Backend{u1.row("one", backends.TrustFull, 20), u2.row("two", backends.TrustFull, 10)}, 2, 1,
			withStreamSettings, withWait(100*time.Millisecond), withStream(0, 10*time.Second, 5*time.Second),
			withDeps(func(d *oai.Deps) { d.Limiter = m }))
		srv := serveHTTP(t, rig.router)
		release := occupyExternal(t, rig.disp, u2.srv.URL)
		a := postStream(context.Background(), t, srv, streamBody("chat"))
		code, last := a.errorFrame(t)
		t.Logf("R5 9g: answer %d, keepalives before data %d, frame %q last=%v, hits %d/%d", a.status, a.commentsBeforeData(), code, last, u1.hits.Load(), u2.hits.Load())
		if a.status != http.StatusOK || code != oai.CodeServerBusy || !last || u2.hits.Load() != 0 {
			t.Errorf("answer %d frame %q — want the grace commit and the server_busy frame", a.status, code)
		}
		release()
		r5Check(t, "9 grace commit + failover refused", m, rig, srv, "keep", r5HoldOn(u1, u2))
	})

	t.Run("10 upstream 503 before any frame, no 200", func(t *testing.T) {
		u := newUpstream(t, "")
		u.setRespond(r5Upstream502(http.StatusServiceUnavailable, 0))
		m := r5Meter()
		rig := newChatRig(t, one(u), 4, 1, withStreamSettings, withDeps(func(d *oai.Deps) { d.Limiter = m }))
		srv := serveHTTP(t, rig.router)
		a := postStream(context.Background(), t, srv, streamBody("chat"))
		t.Logf("R5 10a: answer %d %s, hits %d", a.status, r5Code(a.raw), u.hits.Load())
		if a.status == http.StatusOK {
			t.Errorf("answer 200, want a status before the commit")
		}
		r5Check(t, "10 upstream 503 without 200", m, rig, srv, "keep", r5HoldOn(u))
	})

	t.Run("10 upstream 200 then reset before any frame", func(t *testing.T) {
		u := newUpstream(t, "")
		u.setRespond(r5OKThenReset(0))
		m := r5Meter()
		rig := newChatRig(t, one(u), 4, 1, withStreamSettings, withDeps(func(d *oai.Deps) { d.Limiter = m }))
		srv := serveHTTP(t, rig.router)
		a := postStream(context.Background(), t, srv, streamBody("chat"))
		t.Logf("R5 10b: answer %d %s, hits %d", a.status, r5Code(a.raw), u.hits.Load())
		if a.status == http.StatusOK {
			t.Errorf("answer 200, want a status before the commit")
		}
		r5Check(t, "10 upstream 200 + reset before a frame", m, rig, srv, "settle", r5HoldOn(u))
	})

	t.Run("11 connect refused", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()
		m := r5Meter()
		rig := newChatRig(t, []backends.Backend{wireRow("refused", "http://"+addr, 10)}, 4, 1, withStreamSettings,
			withDeps(func(d *oai.Deps) { d.Limiter = m }))
		a := postStream(context.Background(), t, serveHTTP(t, rig.router), streamBody("chat"))
		t.Logf("R5 11: answer %d %s", a.status, r5Code(a.raw))
		r5Check(t, "11 connect refused", m, rig, nil, "abort", nil)
	})

	for _, with200 := range []bool{false, true} {
		name := map[bool]string{false: "12 grace commit, upstream 502 before any frame", true: "12 grace commit, upstream 200 then reset before any frame"}[with200]
		want := map[bool]string{false: "keep", true: "settle"}[with200]
		t.Run(name, func(t *testing.T) {
			fastPhaseB(t, 100*time.Millisecond)
			u := newUpstream(t, "")
			if with200 {
				u.setRespond(r5OKThenReset(400 * time.Millisecond))
			} else {
				u.setRespond(r5Upstream502(http.StatusBadGateway, 400*time.Millisecond))
			}
			m := r5Meter()
			rig := newChatRig(t, one(u), 4, 1, withStreamSettings, withWait(100*time.Millisecond),
				withStream(0, 10*time.Second, 5*time.Second), withDeps(func(d *oai.Deps) { d.Limiter = m }))
			srv := serveHTTP(t, rig.router)
			a := postStream(context.Background(), t, srv, streamBody("chat"))
			code, last := a.errorFrame(t)
			t.Logf("R5 %s: answer %d, keepalives %d, frame %q last=%v", name, a.status, a.commentsBeforeData(), code, last)
			if a.status != http.StatusOK || code == "" {
				t.Errorf("answer %d frame %q — want the grace commit and an error frame", a.status, code)
			}
			r5Check(t, name, m, rig, srv, want, r5HoldOn(u))
		})
	}

	t.Run("13 upstream reset after two frames", func(t *testing.T) {
		u := newUpstream(t, "")
		u.setRespond(r5OKThenReset(0, sglangChunk("one", ""), sglangChunk(" two", "")))
		m := r5Meter()
		rig := newChatRig(t, one(u), 4, 1, withStreamSettings, withDeps(func(d *oai.Deps) { d.Limiter = m }))
		srv := serveHTTP(t, rig.router)
		a := postStream(context.Background(), t, srv, streamBody("chat"))
		code, _ := a.errorFrame(t)
		t.Logf("R5 13: answer %d, chunks %d, frame %q", a.status, len(a.chunks()), code)
		r5Check(t, "13 reset after two frames", m, rig, srv, "settle", r5HoldOn(u))
	})

	t.Run("14 grace commit, client gone before the backend's status", func(t *testing.T) {
		fastPhaseB(t, 100*time.Millisecond)
		u := newUpstream(t, "")
		entered := make(chan struct{}, 1)
		u.setRespond(r5HoldNoStatus(entered))
		m := r5Meter()
		rig := newChatRig(t, one(u), 4, 1, withStreamSettings, withWait(100*time.Millisecond),
			withStream(0, 10*time.Second, 5*time.Second), withDeps(func(d *oai.Deps) { d.Limiter = m }))
		srv := serveHTTP(t, rig.router)
		ctx, cancel := context.WithCancel(context.Background())
		ls, _ := openAndRead(t, ctx, srv, streamBody("chat"))
		waitEnter(t, entered)
		r := ls.response(3 * time.Second)
		status := 0
		if r != nil {
			status = r.StatusCode
		}
		cancel()
		_ = ls.Close()
		t.Logf("R5 14: status at the grace commit %d", status)
		if status != http.StatusOK {
			t.Errorf("no grace commit before the cut: status %d", status)
		}
		r5Check(t, "14 grace commit, client gone before the 200", m, rig, srv, "keep", r5HoldOn(u))
	})

	t.Run("15 client gone after three frames", func(t *testing.T) {
		hold := make(chan struct{})
		defer close(hold)
		u := newUpstream(t, "")
		u.setRespond(sseRespond([]sseStep{{data: sglangChunk("a", "")}, {data: sglangChunk("b", "")}, {data: sglangChunk("c", "")}, {hold: hold}}, nil, nil))
		m := r5Meter()
		rig := newChatRig(t, one(u), 4, 1, withStreamSettings, withDeps(func(d *oai.Deps) { d.Limiter = m }))
		srv := serveHTTP(t, rig.router)
		ctx, cancel := context.WithCancel(context.Background())
		ls, each := openAndRead(t, ctx, srv, streamBody("chat"))
		for i := 0; i < 3; i++ {
			if _, ok := nextData(each, 5*time.Second); !ok {
				t.Fatalf("frame %d missing", i+1)
			}
		}
		cancel()
		_ = ls.Close()
		r5Check(t, "15 client gone after three frames", m, rig, srv, "settle", r5HoldOn(u))
	})

	t.Run("15 watcher: grant withdrawn", func(t *testing.T) {
		t.Cleanup(oai.SetReauthEvery(100 * time.Millisecond))
		hold := make(chan struct{})
		defer close(hold)
		f := &reauthFake{}
		u := newUpstream(t, "")
		u.setRespond(sseRespond([]sseStep{{data: sglangChunk("hello", "")}, {hold: hold}}, nil, nil))
		m := r5Meter()
		rig := newChatRig(t, one(u), 4, 1, withStreamSettings, withReauth(f), withDeps(func(d *oai.Deps) { d.Limiter = m }))
		ca := urgentApp()
		ca.AllModels = false
		rig.ca.Store(ca)
		rig.setGrant(idChat, true)
		srv := serveHTTP(t, rig.router)
		ls, each := openAndRead(t, context.Background(), srv, streamBody("chat"))
		if _, ok := nextData(each, 5*time.Second); !ok {
			t.Fatal("no frame")
		}
		rig.setGrant(idChat, false)
		e, ok := nextData(each, 2*time.Second)
		_ = ls.Close()
		t.Logf("R5 15w-grant: frame %v %q", ok, r5Code(e.data))
		if !ok || r5Code(e.data) != oai.CodeModelNotFound {
			t.Errorf("frame %q, want model_not_found", e.data)
		}
		rig.setGrant(idChat, true)
		r5Check(t, "15 watcher: grant withdrawn", m, rig, srv, "settle", r5HoldOn(u))
	})

	t.Run("15 watcher: key revoked", func(t *testing.T) {
		t.Cleanup(oai.SetReauthEvery(100 * time.Millisecond))
		hold := make(chan struct{})
		defer close(hold)
		f := &reauthFake{}
		f.set(reauthAnswer{ok: true}, reauthAnswer{ok: false})
		u := newUpstream(t, "")
		u.setRespond(sseRespond([]sseStep{{data: sglangChunk("hello", "")}, {hold: hold}}, nil, nil))
		m := r5Meter()
		rig := newChatRig(t, one(u), 4, 1, withStreamSettings, withReauth(f), withDeps(func(d *oai.Deps) { d.Limiter = m }))
		srv := serveHTTP(t, rig.router)
		ls, each := openAndRead(t, context.Background(), srv, streamBody("chat"))
		if _, ok := nextData(each, 5*time.Second); !ok {
			t.Fatal("no frame")
		}
		e, ok := nextData(each, 2*time.Second)
		_ = ls.Close()
		t.Logf("R5 15w-revoke: frame %v %q", ok, r5Code(e.data))
		if !ok || r5Code(e.data) != oai.CodeInvalidAPIKey {
			t.Errorf("frame %q, want invalid_api_key", e.data)
		}
		f.set(reauthAnswer{ok: true})
		r5Check(t, "15 watcher: key revoked", m, rig, srv, "settle", r5HoldOn(u))
	})

	t.Run("15 shutdown after the commit", func(t *testing.T) {
		hold := make(chan struct{})
		defer close(hold)
		u := newUpstream(t, "")
		u.setRespond(sseRespond([]sseStep{{data: sglangChunk("hello", "")}, {hold: hold}}, nil, nil))
		life, stop := context.WithCancel(context.Background())
		defer stop()
		m := r5Meter()
		rig := newChatRig(t, one(u), 4, 1, withStreamSettings, func(r *chatRig) { r.deps.Lifecycle = life },
			withDeps(func(d *oai.Deps) { d.Limiter = m }))
		srv := r5Serve(t, rig.router)
		ls, each := openAndRead(t, context.Background(), srv, streamBody("chat"))
		if _, ok := nextData(each, 5*time.Second); !ok {
			t.Fatal("no frame")
		}
		stop()
		e, ok := nextData(each, 2*time.Second)
		_ = ls.Close()
		t.Logf("R5 15s: frame %v %q", ok, r5Code(e.data))
		if !ok || r5Code(e.data) != oai.CodeInternalError {
			t.Errorf("frame %q, want internal_error (the shutdown)", e.data)
		}
		r5Check(t, "15 shutdown after the commit", m, rig, nil, "settle", nil)
	})

	t.Run("16 served to [DONE] with a usage chunk", func(t *testing.T) {
		u := newUpstream(t, "")
		u.setRespond(sseRespond(streamShapes[0].steps(), nil, nil))
		m := r5Meter()
		rig := newChatRig(t, one(u), 4, 1, withStreamSettings, withDeps(func(d *oai.Deps) { d.Limiter = m }))
		srv := serveHTTP(t, rig.router)
		a := postStream(context.Background(), t, srv, streamBody("chat"))
		t.Logf("R5 16: answer %d dones %d", a.status, a.dones())
		r5Check(t, "16 served to [DONE]", m, rig, srv, "settle", r5HoldOn(u))
	})

	t.Run("17 the [DONE] write fails", func(t *testing.T) {
		u := newUpstream(t, "")
		u.setRespond(sseRespond(streamShapes[0].steps(), nil, nil))
		m := r5Meter()
		rig := newChatRig(t, one(u), 4, 1, withStreamSettings, withDeps(func(d *oai.Deps) { d.Limiter = m }))
		var fail atomic.Bool
		fail.Store(true)
		srv := r5Serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() { _ = recover() }()
			rig.router.ServeHTTP(r5Writer{ResponseWriter: w, failDone: &fail}, r)
		}))
		a := postStream(context.Background(), t, srv, streamBody("chat"))
		t.Logf("R5 17: answer %d dones %d read error %v, write failed %v", a.status, a.dones(), a.readErr, !fail.Load())
		if fail.Load() {
			t.Error("the [DONE] write was never attempted")
		}
		r5Check(t, "17 [DONE] write fails", m, rig, srv, "settle", r5HoldOn(u))
	})

	t.Run("20 panic in the relay's write at the second frame", func(t *testing.T) {
		u := newUpstream(t, "")
		u.setRespond(sseRespond([]sseStep{{data: sglangChunk("a", "")}, {data: sglangChunk("b", "")}, {data: sglangChunk("c", "stop")}, {data: "[DONE]"}}, nil, nil))
		m := r5Meter()
		rig := newChatRig(t, one(u), 4, 1, withStreamSettings, withDeps(func(d *oai.Deps) { d.Limiter = m }))
		var count atomic.Int64
		srv := r5Serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() { _ = recover() }()
			rig.router.ServeHTTP(r5Writer{ResponseWriter: w, panicAt: 2, count: &count}, r)
		}))
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		a := postStream(ctx, t, srv, streamBody("chat"))
		t.Logf("R5 20: answer %d, read error %v, frames written %d", a.status, a.readErr, count.Load())
		if a.status != http.StatusOK || a.readErr == nil {
			t.Errorf("answer %d read error %v — want the committed stream cut", a.status, a.readErr)
		}
		r5Check(t, "20 panic in the relay's write", m, rig, srv, "keep", r5HoldOn(u))
	})

	t.Run("21 header source panics at the grace commit", func(t *testing.T) {
		fastPhaseB(t, 100*time.Millisecond)
		u := newUpstream(t, "")
		entered := make(chan struct{}, 1)
		u.setRespond(r5HoldNoStatus(entered))
		m := r5Meter()
		var armed atomic.Bool
		armed.Store(true)
		rig := newChatRig(t, one(u), 4, 1, withStreamSettings, withWait(100*time.Millisecond),
			withStream(0, 10*time.Second, 5*time.Second), withDeps(func(d *oai.Deps) { d.Limiter = r5HeaderPanic{m, &armed} }))
		srv := r5Serve(t, rig.router)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		a := postStream(ctx, t, srv, streamBody("chat"))
		t.Logf("R5 21: answer %d %s, panicked %v", a.status, r5Code(a.raw), !armed.Load())
		if a.status != http.StatusServiceUnavailable || r5Code(a.raw) != oai.CodeInternalError || armed.Load() {
			t.Errorf("answer %d %q — want oai.Recover's 503 internal_error", a.status, a.raw)
		}
		r5Check(t, "21 header source panics at the grace commit", m, rig, srv, "keep", r5HoldOn(u))
	})
}

// TestStreamL1RefusalRawOnTheWire: with one slot held by a running stream, a
// second stream of the same consumer gets the L1 refusal over a raw TCP
// connection — status line and header block read byte for byte — before
// any byte of a stream, without reaching the engine and without taking a
// slot: after the first ends, one holding stream gets the slot and one probe
// beside it is refused.
func TestStreamL1RefusalRawOnTheWire(t *testing.T) {
	release := make(chan struct{})
	u := newUpstream(t, "")
	r5HoldOn(u)(release)
	m := r5Meter()
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings,
		withDeps(func(d *oai.Deps) { d.Limiter = m }))
	srv := serveHTTP(t, rig.router)
	ls, each := openAndRead(t, context.Background(), srv, streamBody("chat"))
	if _, ok := nextData(each, 5*time.Second); !ok {
		close(release)
		t.Fatal("the first stream relayed no frame")
	}
	hitsBefore := u.hits.Load()

	body := streamBody("chat")
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	req := "POST /v1/chat/completions HTTP/1.1\r\nHost: " + srv.Listener.Addr().String() +
		"\r\nContent-Type: application/json\r\nAccept: text/event-stream\r\nConnection: close\r\nContent-Length: " +
		fmt.Sprint(len(body)) + "\r\n\r\n" + body
	if _, err := io.WriteString(conn, req); err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(bufio.NewReader(conn))
	_ = conn.Close()
	text := string(raw)
	head, rest, found := strings.Cut(text, "\r\n\r\n")
	if !found {
		t.Fatalf("no header block in %q", text)
	}
	t.Logf("R5 wire: header block (raw):\n%s", head)
	t.Logf("R5 wire: body (raw): %q", rest)
	lines := strings.Split(head, "\r\n")
	if lines[0] != "HTTP/1.1 429 Too Many Requests" {
		t.Errorf("status line %q, want HTTP/1.1 429 Too Many Requests", lines[0])
	}
	hdr := map[string][]string{}
	for _, ln := range lines[1:] {
		k, v, _ := strings.Cut(ln, ": ")
		hdr[strings.ToLower(k)] = append(hdr[strings.ToLower(k)], v)
	}
	for k, want := range map[string]string{"retry-after": "1", "retry-after-ms": "1000", "x-should-retry": "true"} {
		if got := hdr[k]; len(got) != 1 || got[0] != want {
			t.Errorf("%s %q, want exactly one %q", k, got, want)
		}
	}
	if ct := hdr["content-type"]; len(ct) != 1 || !strings.HasPrefix(ct[0], "application/json") {
		t.Errorf("Content-Type %q, want application/json", ct)
	}
	for k := range hdr {
		if strings.HasPrefix(k, "x-ratelimit-") {
			t.Errorf("the refusal carries %s %q", k, hdr[k])
		}
	}
	if strings.Contains(rest, "data:") || strings.Contains(rest, ": keepalive") {
		t.Errorf("the refusal body carries stream bytes: %q", rest)
	}
	bodyJSON := rest
	if te := hdr["transfer-encoding"]; len(te) == 1 && te[0] == "chunked" {
		bodyJSON = dechunk(rest)
	}
	var env struct {
		Error struct{ Message, Type, Code string } `json:"error"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(bodyJSON)), &env); err != nil || env.Error.Code != oai.CodeConcurrencyLimitExceeded || env.Error.Type != "rate_limit_error" {
		t.Errorf("body %q (%v), want the JSON envelope concurrency_limit_exceeded / rate_limit_error", bodyJSON, err)
	}
	if u.hits.Load() != hitsBefore {
		t.Errorf("the engine was reached: hits %d → %d", hitsBefore, u.hits.Load())
	}
	if n := endCount(m); n != 0 {
		t.Errorf("ends %q while the first stream runs, want none (the refusal took no reservation)", m.endsOf())
	}
	close(release)
	for {
		e, ok := nextData(each, 5*time.Second)
		if !ok || e.data == "[DONE]" {
			break
		}
	}
	_ = ls.Close()
	if !waitFor(5*time.Second, func() bool { return endCount(m) == 1 }) {
		t.Fatalf("ends %q, want the first stream's one", m.endsOf())
	}
	r5Check(t, "0 L1 refusal takes no slot", m, rig, srv, m.endsOf(), r5HoldOn(u))
	t.Logf("R5 wire: engine hits %d (the first stream and the holder), ends %s", u.hits.Load(), m.endsOf())
}

// dechunk decodes a chunked HTTP/1.1 body.
func dechunk(s string) string {
	var out strings.Builder
	for {
		size, rest, ok := strings.Cut(s, "\r\n")
		if !ok {
			return out.String()
		}
		var n int
		if _, err := fmt.Sscanf(size, "%x", &n); err != nil || n == 0 || len(rest) < n {
			return out.String()
		}
		out.WriteString(rest[:n])
		s = strings.TrimPrefix(rest[n:], "\r\n")
	}
}
