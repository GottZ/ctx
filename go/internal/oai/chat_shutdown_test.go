package oai_test

// Pins of the hardening after Fremd-Review 2 (W02-07, H-1 = F2-1): the
// shutdown path is deterministic. A lifecycle that already ended when the
// request enters reaches neither the planner nor an acquire nor the
// upstream; a shutdown or a kill switch that lands while the admission
// writer runs, or while the request waits in the queue, is answered by the
// route — never an empty 200. Every refusal source here holds before and
// after A03-W9 (H-6): a chain whose host the dispatcher does not offer
// externally (handler.ErrChainNotOffered) or an acquire refused by the
// fault planner — never the bulk lane.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/handler/v1census"
	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/GottZ/ctx/internal/oai"
)

// notOffered withdraws the external offer of every target: the planner's
// chain filter leaves nothing and Plan refuses with ErrChainNotOffered
// (503 model_unavailable) — a planner refusal that does not depend on the
// lane table.
func notOffered(r *chatRig) {
	r.disp.UpdatePolicy(dispatch.Policy{Targets: map[string]dispatch.TargetPolicy{}})
}

// shWindowPlanner puts the shutdown exactly into the writer window: its
// WriteAdmissionError ends the lifecycle, waits until the run context saw
// it, then delegates to the real writer — which stays silent on a canceled
// request context (the review's deterministic writer-window probe).
type shWindowPlanner struct {
	oai.AdmissionPlanner
	stop  func()
	calls *atomic.Int64
}

func (p shWindowPlanner) WriteAdmissionError(ctx context.Context, w http.ResponseWriter, err error) string {
	p.calls.Add(1)
	p.stop()
	<-ctx.Done()
	return p.AdmissionPlanner.WriteAdmissionError(ctx, w, err)
}

// shCountPlanner counts Plan calls of the real planner.
type shCountPlanner struct {
	oai.AdmissionPlanner
	plans *atomic.Int64
}

func (p shCountPlanner) Plan(ctx context.Context, ca *auth.ConsumerAuth, in oai.AdmissionInput) (oai.Plan, error) {
	p.plans.Add(1)
	return p.AdmissionPlanner.Plan(ctx, ca, in)
}

// wantShutdownAnswer is the one answer of a shutdown: 503 internal_error,
// x-should-retry true, a body — never an empty 200.
func wantShutdownAnswer(t *testing.T, name string, a chatAnswer) {
	t.Helper()
	if a.status == http.StatusOK && len(a.body) == 0 {
		t.Fatalf("%s: empty 200 (status=%d len=0)", name, a.status)
	}
	code, _, _ := a.envelope(t)
	if a.status != http.StatusServiceUnavailable || code != oai.CodeInternalError || a.header.Get("x-should-retry") != "true" {
		t.Fatalf("%s: %d %q (x-should-retry %q), want 503 internal_error, x-should-retry true", name, a.status, a.body, a.header.Get("x-should-retry"))
	}
}

// acquires counts the blocking and non-blocking acquires the recorder saw.
func acquires(evs []v1census.Event) int {
	n := 0
	for _, e := range evs {
		if e.Kind == v1census.KindAcquire || e.Kind == v1census.KindTry {
			n++
		}
	}
	return n
}

// TestShutdownInTheWriterWindowIsNoEmpty200 (H-1 gate (a)): the shutdown
// lands between the route's own verdict and the writer's context check, on
// both callers of the writer — the plan refusal (walk) and the acquire
// refusal (outcome). The writer returns "", the route answers the shutdown.
func TestShutdownInTheWriterWindowIsNoEmpty200(t *testing.T) {
	for _, tc := range []struct {
		name  string
		inner func(r *chatRig) oai.AdmissionPlanner
		prep  func(r *chatRig)
	}{
		{"plan refusal (chain not offered)", func(r *chatRig) oai.AdmissionPlanner { return r.planner }, notOffered},
		{"acquire refusal (would block)", func(r *chatRig) oai.AdmissionPlanner {
			return chFaultPlanner{AdmissionPlanner: r.planner, err: dispatch.ErrWouldBlock}
		}, func(*chatRig) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := newUpstream(t, answerSGLang)
			life, cancel := context.WithCancel(context.Background())
			defer cancel()
			var calls atomic.Int64
			rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 2, 1, func(r *chatRig) {
				tc.prep(r)
				r.deps.Lifecycle = life
				r.deps.Planner = shWindowPlanner{AdmissionPlanner: tc.inner(r), stop: cancel, calls: &calls}
			})
			a := rig.post(t, chatBody("chat"))
			if calls.Load() != 1 {
				t.Fatalf("writer calls = %d, want 1 (the refusal did not reach the writer window)", calls.Load())
			}
			wantShutdownAnswer(t, tc.name, a)
			if rec := <-rig.recs; rec.Reason != oai.CodeInternalError {
				t.Errorf("access record reason %q, want %q", rec.Reason, oai.CodeInternalError)
			}
			if n := u.hits.Load(); n != 0 {
				t.Errorf("wire hits = %d, want 0", n)
			}
			l := rig.limiter
			if l.admits.Load() != 1 || l.settles.Load()+l.aborts.Load()+l.rejects.Load() != 1 || l.settles.Load() != 0 {
				t.Errorf("reservation: admits %d settles %d aborts %d rejects %d, want 1 ended exactly once, never settled",
					l.admits.Load(), l.settles.Load(), l.aborts.Load(), l.rejects.Load())
			}
		})
	}
}

// TestShutdownAtEntryReachesNoPlanNoWire (H-1 gate (b)): the lifecycle has
// ended before the request enters, normal lane, 500 requests: 0 plans, 0
// acquires, 0 wire hits, 0 llmlog rows, every reservation ended exactly once
// and never settled, every answer the shutdown's 503.
func TestShutdownAtEntryReachesNoPlanNoWire(t *testing.T) {
	const n = 500
	u := newUpstream(t, answerSGLang)
	life, cancel := context.WithCancel(context.Background())
	cancel()
	var plans atomic.Int64
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 2, 1, func(r *chatRig) {
		r.deps.Lifecycle = life
		r.deps.Planner = shCountPlanner{AdmissionPlanner: r.planner, plans: &plans}
	})
	_ = rig.recorder.Take()
	bad := map[string]int{}
	for i := 0; i < n; i++ {
		a := rig.post(t, chatBody("chat"))
		code := ""
		if len(a.body) > 0 {
			code, _, _ = a.envelope(t)
		}
		if a.status != http.StatusServiceUnavailable || code != oai.CodeInternalError || a.header.Get("x-should-retry") != "true" {
			bad[http.StatusText(a.status)+"/"+code]++
		}
	}
	if len(bad) > 0 {
		t.Errorf("answers other than 503 internal_error (x-should-retry true) over %d: %v", n, bad)
	}
	l := rig.limiter
	acq := acquires(rig.recorder.Take())
	t.Logf("plans=%d acquires=%d wire=%d rows=%d admits=%d settles=%d aborts=%d rejects=%d", plans.Load(),
		acq, u.hits.Load(), len(rig.rows.all()), l.admits.Load(), l.settles.Load(), l.aborts.Load(), l.rejects.Load())
	if p := plans.Load(); p != 0 {
		t.Errorf("plans = %d, want 0", p)
	}
	if acq != 0 {
		t.Errorf("acquires = %d, want 0", acq)
	}
	if h := u.hits.Load(); h != 0 {
		t.Errorf("wire hits = %d, want 0", h)
	}
	if r := len(rig.rows.all()); r != 0 {
		t.Errorf("llmlog rows = %d, want 0", r)
	}
	if l.settles.Load() != 0 || l.aborts.Load()+l.rejects.Load() != l.admits.Load() {
		t.Errorf("reservations: admits %d settles %d aborts %d rejects %d, want each ended exactly once by Abort/Reject",
			l.admits.Load(), l.settles.Load(), l.aborts.Load(), l.rejects.Load())
	}
	// Answered before the parse: the meter is not even asked (no
	// reservation to end; TestShutdownAfterTheLimiterAborts pins the end of
	// one that was taken).
	if a := l.admits.Load(); a != 0 {
		t.Errorf("limiter admits = %d, want 0 (a run ended at entry takes no reservation)", a)
	}
}

// waitExternalWaiting waits until a request queues for host's external
// partition.
func waitExternalWaiting(t *testing.T, d *dispatch.Dispatcher, host string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, ts := range d.Snapshot().Targets {
			if ts.Origin == host && ts.External.Waiting > 0 {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("no external waiter on %s within 5 s", host)
}

// TestRunEndedWhileQueuedIsAnswered (H-1 gate (c), Fremd-Review 2 O1): the
// shutdown and the kill switch while the request waits in the queue (the
// partition held by another consumer, no lease): 503 internal_error resp. the
// switched-off 404 — never an empty 200, no wire contact, no row.
func TestRunEndedWhileQueuedIsAnswered(t *testing.T) {
	defer oai.SetWatchEvery(20 * time.Millisecond)()
	for _, tc := range []struct {
		name string
		end  func(r *chatRig, stop func())
		want int
		code string
	}{
		{"shutdown", func(_ *chatRig, stop func()) { stop() }, http.StatusServiceUnavailable, oai.CodeInternalError},
		{"kill switch", func(r *chatRig, _ func()) { r.setSettings(func(s *oai.Settings) { s.Enabled = false }) }, http.StatusNotFound, oai.CodeNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := newUpstream(t, answerSGLang)
			life, cancel := context.WithCancel(context.Background())
			defer cancel()
			rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 2, 1, func(r *chatRig) { r.deps.Lifecycle = life })
			defer occupyExternal(t, rig.disp, u.srv.URL)()
			done := make(chan chatAnswer, 1)
			go func() { done <- rig.post(t, chatBody("chat"), "Ctx-Max-Wait-Ms", "10000") }()
			waitExternalWaiting(t, rig.disp, u.srv.URL)
			tc.end(rig, cancel)
			var a chatAnswer
			select {
			case a = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("no answer within 5 s of the end")
			}
			if a.status == http.StatusOK && len(a.body) == 0 {
				t.Fatalf("%s while queued: empty 200", tc.name)
			}
			code, _, _ := a.envelope(t)
			if a.status != tc.want || code != tc.code {
				t.Fatalf("%s while queued: %d %q, want %d %s", tc.name, a.status, a.body, tc.want, tc.code)
			}
			if u.hits.Load() != 0 || len(rig.rows.all()) != 0 {
				t.Errorf("wire hits %d, rows %d, want 0/0", u.hits.Load(), len(rig.rows.all()))
			}
		})
	}
}

// shLimiter ends the lifecycle inside Admit and waits until the run saw it:
// the shutdown lands after the reservation was taken and before the plan.
type shLimiter struct {
	*fakeLimiter
	stop func()
}

func (l shLimiter) Admit(ctx context.Context, ca *auth.ConsumerAuth, a modelalias.Alias, e oai.Estimate) (oai.LimitReservation, *oai.LimitRefusal) {
	res, refusal := l.fakeLimiter.Admit(ctx, ca, a, e)
	l.stop()
	<-ctx.Done()
	return res, refusal
}

// TestShutdownAfterTheLimiterAborts (H-1 gate (b), the reservation half): a
// shutdown after the limiter admitted reaches neither the planner nor an
// acquire nor the wire, and the reservation ends exactly once with Abort.
func TestShutdownAfterTheLimiterAborts(t *testing.T) {
	u := newUpstream(t, answerSGLang)
	life, cancel := context.WithCancel(context.Background())
	defer cancel()
	var plans atomic.Int64
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 2, 1, func(r *chatRig) {
		r.deps.Lifecycle = life
		r.deps.Planner = shCountPlanner{AdmissionPlanner: r.planner, plans: &plans}
		r.deps.Limiter = shLimiter{fakeLimiter: r.limiter, stop: cancel}
	})
	_ = rig.recorder.Take()
	wantShutdownAnswer(t, "shutdown after the limiter", rig.post(t, chatBody("chat")))
	l := rig.limiter
	if acq := acquires(rig.recorder.Take()); plans.Load() != 0 || acq != 0 || u.hits.Load() != 0 || len(rig.rows.all()) != 0 {
		t.Errorf("plans %d acquires %d wire %d rows %d, want 0/0/0/0", plans.Load(), acq, u.hits.Load(), len(rig.rows.all()))
	}
	if l.admits.Load() != 1 || l.aborts.Load() != 1 || l.settles.Load()+l.rejects.Load() != 0 {
		t.Errorf("reservation: admits %d settles %d aborts %d rejects %d, want 1 admit ended by exactly one Abort",
			l.admits.Load(), l.settles.Load(), l.aborts.Load(), l.rejects.Load())
	}
}

// shGrantRead ends the lifecycle during the grant probe on call number at
// (1 = the resolve, 2 = the check before the write) and fails that read
// with the context's error — what a DB read on the canceled run returns.
func shGrantRead(stop func(), at int64) func(context.Context, string, string) (bool, error) {
	var n atomic.Int64
	return func(ctx context.Context, _, _ string) (bool, error) {
		if n.Add(1) == at {
			stop()
			<-ctx.Done()
			return false, ctx.Err()
		}
		return true, nil
	}
}

// TestShutdownDuringAGrantReadIsTheShutdownAnswer (H-1, every exit): a grant
// read the shutdown ended — in the resolve or in the check before the write —
// is answered as the shutdown (503 internal_error, x-should-retry TRUE: the
// client retries elsewhere), never as a grant read fault (x-should-retry
// false, an ERROR on every request of a draining instance).
func TestShutdownDuringAGrantReadIsTheShutdownAnswer(t *testing.T) {
	for _, tc := range []struct {
		name string
		at   int64
		hits int64
	}{{"resolve", 1, 0}, {"check before the write", 2, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			u := newUpstream(t, answerSGLang)
			life, cancel := context.WithCancel(context.Background())
			defer cancel()
			rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 2, 1, func(r *chatRig) {
				ca := urgentApp()
				ca.AllModels = false
				r.ca.Store(ca)
				r.deps.Lifecycle = life
				r.deps.HasGrant = shGrantRead(cancel, tc.at)
			})
			wantShutdownAnswer(t, tc.name, rig.post(t, chatBody("chat")))
			if h := u.hits.Load(); h != tc.hits {
				t.Errorf("wire hits = %d, want %d", h, tc.hits)
			}
		})
	}
}

// shSilentPlanner's writer writes nothing and returns "" on a live request —
// a writer defect the route must still answer.
type shSilentPlanner struct{ oai.AdmissionPlanner }

func (shSilentPlanner) WriteAdmissionError(context.Context, http.ResponseWriter, error) string {
	return ""
}

// TestSilentWriterOnALiveRequestIsAnswered (H-1 (ii), "never a way back
// without a written answer"): the writer returned "" although the client is
// there and no cause of the route ended the run ⇒ 503 internal_error, never
// an empty 200.
func TestSilentWriterOnALiveRequestIsAnswered(t *testing.T) {
	u := newUpstream(t, answerSGLang)
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 2, 1, func(r *chatRig) {
		r.deps.Planner = shSilentPlanner{chFaultPlanner{AdmissionPlanner: r.planner, err: dispatch.ErrWouldBlock}}
	})
	a := rig.post(t, chatBody("chat"))
	if a.status == http.StatusOK && len(a.body) == 0 {
		t.Fatal("silent writer: empty 200")
	}
	if code, _, _ := a.envelope(t); a.status != http.StatusServiceUnavailable || code != oai.CodeInternalError {
		t.Fatalf("silent writer: %d %q, want 503 internal_error", a.status, a.body)
	}
}

// shBody ends the lifecycle at the first Read of the request body: the
// shutdown lands after the entry, while the request is read and parsed.
type shBody struct {
	r    io.Reader
	once atomic.Bool
	stop func()
}

func (b *shBody) Read(p []byte) (int, error) {
	if b.once.CompareAndSwap(false, true) {
		b.stop()
	}
	return b.r.Read(p)
}

// TestShutdownDuringTheBodyReadReachesNoPlan (Fremd-Review 3 R3-1): a
// lifecycle that ends after the entry — during the body read — is seen
// synchronously by the route's verdict before the plan: 200 requests, every
// answer the shutdown's 503, 0 plans, 0 wire hits, 0 rows, every reservation
// ended exactly once. (AfterFunc alone delivers the cause from its own
// goroutine and loses that race.)
func TestShutdownDuringTheBodyReadReachesNoPlan(t *testing.T) {
	const n = 200
	u := newUpstream(t, answerSGLang)
	var plans atomic.Int64
	var rows int
	for i := 0; i < n; i++ {
		life, cancel := context.WithCancel(context.Background())
		rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 2, 1, func(r *chatRig) {
			r.deps.Lifecycle = life
			r.deps.Planner = shCountPlanner{AdmissionPlanner: r.planner, plans: &plans}
		})
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", &shBody{r: strings.NewReader(chatBody("chat")), stop: cancel})
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		rig.router.ServeHTTP(w, req)
		wantShutdownAnswer(t, "shutdown during the body read", chatAnswer{status: w.Code, header: w.Header(), body: w.Body.Bytes()})
		rows += len(rig.rows.all())
		l := rig.limiter
		if l.settles.Load()+l.aborts.Load()+l.rejects.Load() != l.admits.Load() || l.settles.Load() != 0 {
			t.Fatalf("request %d: admits %d settles %d aborts %d rejects %d, want each reservation ended once, never settled",
				i, l.admits.Load(), l.settles.Load(), l.aborts.Load(), l.rejects.Load())
		}
		cancel()
	}
	if plans.Load() != 0 || u.hits.Load() != 0 || rows != 0 {
		t.Fatalf("over %d requests: plans %d, wire hits %d, rows %d, want 0/0/0", n, plans.Load(), u.hits.Load(), rows)
	}
}
