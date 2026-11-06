package oai_test

// Pin parity with the chat route (lead decision M1 (a), the W02-07 H-1
// pins as the template): the exit invariants of the embeddings route in
// every section — a lifecycle ended at entry reaches no plan, no acquire, no
// wire; a shutdown or kill switch while the First waits, between two blocks,
// while a continuation waits at the class head, and in the writer window is
// answered by the route (never an empty 200); a client gone during a block
// gets nothing written; every reservation ends exactly once; no llmlog row
// without a wire call; no lease is left held.

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
	"github.com/GottZ/ctx/internal/oai"
)

// reservationsEndedOnce checks the fake limiter: every admitted reservation
// ended exactly once; settled of them with a usage (a wire call whose blocks
// reported one) and kept of them (wire contact without a usage — never a
// full refund after the wire, the meter rule of W06-10 hardening H-7).
func reservationsEndedOnce(t *testing.T, l *fakeLimiter, settled, kept int64) {
	t.Helper()
	ends := l.settles.Load() + l.keeps.Load() + l.aborts.Load() + l.rejects.Load()
	if ends != l.admits.Load() || l.settles.Load() != settled || l.keeps.Load() != kept {
		t.Errorf("reservations: admits %d settles %d keeps %d aborts %d rejects %d, want each ended once, %d settled, %d kept",
			l.admits.Load(), l.settles.Load(), l.keeps.Load(), l.aborts.Load(), l.rejects.Load(), settled, kept)
	}
}

// noLeaseHeld checks the dispatcher holds nothing on host.
func noLeaseHeld(t *testing.T, d *dispatch.Dispatcher, host string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		held, _ := heldOn(d, host)
		if held == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("%d lease(s) held on %s after the answer", held, host)
			return
		}
		time.Sleep(time.Millisecond)
	}
}

// TestEmbedShutdownAtEntryReachesNothing: the lifecycle ended before the
// request entered — 200 requests of 5 inputs: 0 plans, 0 acquires, 0 wire
// hits, 0 rows, no reservation, every answer the shutdown's 503.
func TestEmbedShutdownAtEntryReachesNothing(t *testing.T) {
	e := newEmbedEngine(t, 8)
	life, cancel := context.WithCancel(context.Background())
	cancel()
	var plans atomic.Int64
	rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 2, 1, func(r *chatRig) {
		r.deps.Lifecycle = life
		r.deps.Planner = shCountPlanner{AdmissionPlanner: r.planner, plans: &plans}
	})
	_ = rig.recorder.Take()
	for i := 0; i < 200; i++ {
		wantShutdownAnswer(t, "at entry", rig.postEmbed(t, embedBody("emb", embedInputs(5))))
	}
	if p, a := plans.Load(), acquires(rig.recorder.Take()); p != 0 || a != 0 {
		t.Errorf("plans %d acquires %d, want 0/0", p, a)
	}
	if e.hits.Load() != 0 || len(rig.rows.all()) != 0 || rig.limiter.admits.Load() != 0 {
		t.Errorf("wire hits %d rows %d limiter admits %d, want 0/0/0", e.hits.Load(), len(rig.rows.all()), rig.limiter.admits.Load())
	}
}

// TestEmbedRunEndedWhileQueued: the shutdown and the kill switch while the
// First waits (the partition held by another consumer): the route's answer,
// no wire contact, no row, the reservation ended once without a Settle.
func TestEmbedRunEndedWhileQueued(t *testing.T) {
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
			e := newEmbedEngine(t, 8)
			life, cancel := context.WithCancel(context.Background())
			defer cancel()
			rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 2, 1, func(r *chatRig) { r.deps.Lifecycle = life })
			release := occupyExternal(t, rig.disp, e.srv.URL)
			done := make(chan chatAnswer, 1)
			go func() { done <- rig.postEmbed(t, embedBody("emb", embedInputs(3)), "Ctx-Max-Wait-Ms", "10000") }()
			waitExternalWaiting(t, rig.disp, e.srv.URL)
			tc.end(rig, cancel)
			a := <-done
			release()
			if a.status == http.StatusOK && len(a.body) == 0 {
				t.Fatal("empty 200")
			}
			if code, _, _ := a.envelope(t); a.status != tc.want || code != tc.code {
				t.Fatalf("%d %s, want %d %s", a.status, a.body, tc.want, tc.code)
			}
			if e.hits.Load() != 0 || len(rig.rows.all()) != 0 {
				t.Errorf("wire hits %d rows %d, want 0/0", e.hits.Load(), len(rig.rows.all()))
			}
			reservationsEndedOnce(t, rig.limiter, 0, 0)
			noLeaseHeld(t, rig.disp, e.srv.URL)
		})
	}
}

// stopAtContinue ends the lifecycle at the continuation door: the shutdown
// lands between two blocks — after block one's lease went back, when block
// two asks.
type stopAtContinue struct {
	oai.AdmissionPlanner
	stop func()
}

func (p stopAtContinue) Plan(ctx context.Context, ca *auth.ConsumerAuth, in oai.AdmissionInput) (oai.Plan, error) {
	plan, err := p.AdmissionPlanner.Plan(ctx, ca, in)
	if err == nil {
		plan.Acquirer = stopAcquirer{RequestAcquirer: plan.Acquirer, stop: p.stop}
	}
	return plan, err
}

type stopAcquirer struct {
	oai.RequestAcquirer
	stop func()
}

func (a stopAcquirer) Continue(ctx context.Context, b *backends.Backend, role string, t time.Duration) (*dispatch.Lease, context.Context, error) {
	a.stop()
	<-ctx.Done() // the request context carries the shutdown
	return a.RequestAcquirer.Continue(ctx, b, role, t)
}

// TestEmbedRunEndedBetweenBlocks: the shutdown during block one's wire call
// — D7: the call is aborted, 503 internal_error (x-should-retry true); the
// shutdown at the continuation door — block one done, no block two; the
// kill switch during block two's wire call — aborted, 404 not_found. Every
// case: no further wire hit, the row says aborted with the blocks that got
// a result, the wire work is settled once, no lease is held, the row is no
// health fault.
func TestEmbedRunEndedBetweenBlocks(t *testing.T) {
	defer oai.SetWatchEvery(20 * time.Millisecond)()
	for _, tc := range []struct {
		name       string
		hit        int64
		end        func(r *chatRig, stop func(), req *http.Request)
		atDoor     bool
		want       int
		code       string
		wires      int64
		blocksDone int
	}{
		{"shutdown in block one", 1, func(_ *chatRig, stop func(), req *http.Request) { stop(); <-req.Context().Done() },
			false, http.StatusServiceUnavailable, oai.CodeInternalError, 1, 0},
		{"shutdown at the continuation", 0, nil, true, http.StatusServiceUnavailable, oai.CodeInternalError, 1, 1},
		{"kill switch in block two", 2, func(r *chatRig, _ func(), req *http.Request) {
			r.setSettings(func(s *oai.Settings) { s.Enabled = false })
			<-req.Context().Done() // the watcher ends the run: the wire call is aborted
		}, false, http.StatusNotFound, oai.CodeNotFound, 2, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEmbedEngine(t, 8)
			life, cancel := context.WithCancel(context.Background())
			defer cancel()
			rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 2, 1, func(r *chatRig) {
				r.deps.Lifecycle = life
				if tc.atDoor {
					r.deps.Planner = stopAtContinue{AdmissionPlanner: r.planner, stop: cancel}
				}
			})
			e.setRespond(func(_ http.ResponseWriter, r *http.Request, n int64, _ []string) bool {
				if n == tc.hit {
					tc.end(rig, cancel, r)
				}
				return false
			})
			a := rig.postEmbed(t, embedBody("emb", embedInputs(4)))
			if a.status == http.StatusOK && len(a.body) == 0 {
				t.Fatal("empty 200")
			}
			if code, _, _ := a.envelope(t); a.status != tc.want || code != tc.code {
				t.Fatalf("%d %s, want %d %s", a.status, a.body, tc.want, tc.code)
			}
			if tc.code == oai.CodeInternalError && a.header.Get("x-should-retry") != "true" {
				t.Errorf("shutdown answer x-should-retry %q, want true", a.header.Get("x-should-retry"))
			}
			if h := e.hits.Load(); h != tc.wires {
				t.Errorf("wire hits %d, want %d", h, tc.wires)
			}
			rows := rig.rows.all()
			if len(rows) != 1 || rows[0].Gateway.Finish != "aborted" || rows[0].Telemetry.Metadata["sub_batches_done"] != tc.blocksDone {
				t.Fatalf("rows %+v, want one aborted row with %d block(s) done", rows, tc.blocksDone)
			}
			// The blocks that answered settle their usage; block one aborted
			// during its wire call (none answered) keeps the charge.
			settled, kept := int64(1), int64(0)
			if tc.blocksDone == 0 {
				settled, kept = 0, 1
			}
			reservationsEndedOnce(t, rig.limiter, settled, kept)
			noLeaseHeld(t, rig.disp, e.srv.URL)
			if n := consecutiveFails(rig.pool, "spark-embed"); n != 0 {
				t.Errorf("consecutive_fails %d, want 0", n)
			}
		})
	}
}

// TestEmbedShutdownWhileAContinuationWaits: the continuation waits at the
// class head (a foreign consumer took the unit at block one's release);
// the shutdown ends it — the route's 503, never the writer's silence.
func TestEmbedShutdownWhileAContinuationWaits(t *testing.T) {
	e := newEmbedEngine(t, 8)
	life, cancel := context.WithCancel(context.Background())
	defer cancel()
	rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 2, 1, func(r *chatRig) { r.deps.Lifecycle = life })
	var foreign atomic.Pointer[dispatch.Lease]
	defer func() {
		if l := foreign.Load(); l != nil {
			l.Release()
		}
	}()
	e.setRespond(func(http.ResponseWriter, *http.Request, int64, []string) bool {
		if e.hits.Load() == 1 {
			go func() {
				ctx := context.WithValue(context.Background(), consumerKey{}, foreignConsumer(urgentApp(), "sh"))
				if l, _, err := rig.disp.Acquire(ctx, dispatch.Request{Target: dispatch.Target{Origin: e.srv.URL}, Class: dispatch.ClassExternal, Role: "embed"}); err == nil {
					foreign.Store(l)
				}
			}()
			waitWaiting(t, rig.disp, e.srv.URL, 1, false)
		}
		return false
	})
	done := make(chan chatAnswer, 1)
	go func() { done <- rig.postEmbed(t, embedBody("emb", embedInputs(3))) }()
	// A's continuation queues once the foreign consumer holds the unit.
	deadline := time.Now().Add(5 * time.Second)
	for foreign.Load() == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	waitWaiting(t, rig.disp, e.srv.URL, 1, false)
	cancel()
	a := <-done
	wantShutdownAnswer(t, "continuation waiting", a)
	if h := e.hits.Load(); h != 1 {
		t.Errorf("wire hits %d, want 1", h)
	}
	reservationsEndedOnce(t, rig.limiter, 1, 0)
	if rows := rig.rows.all(); len(rows) != 1 || rows[0].Gateway.Finish != "aborted" {
		t.Errorf("rows %+v, want one aborted row", rows)
	}
}

// TestEmbedShutdownInTheWriterWindow: a continuation refusal reaches the
// planner's writer, and the shutdown lands in the writer window (the writer
// stays silent on the canceled run): the route answers the shutdown.
func TestEmbedShutdownInTheWriterWindow(t *testing.T) {
	e := newEmbedEngine(t, 8)
	life, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int64
	rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 3, 1, func(r *chatRig) {
		r.deps.Lifecycle = life
		inner := contPlanner{AdmissionPlanner: r.planner, err: &dispatch.AdmissionError{Err: dispatch.ErrQueueFull}}
		r.deps.Planner = shWindowPlanner{AdmissionPlanner: inner, stop: cancel, calls: &calls}
	})
	a := rig.postEmbed(t, embedBody("emb", embedInputs(4)))
	if calls.Load() != 1 {
		t.Fatalf("writer calls %d, want 1", calls.Load())
	}
	wantShutdownAnswer(t, "writer window", a)
	reservationsEndedOnce(t, rig.limiter, 1, 0)
	noLeaseHeld(t, rig.disp, e.srv.URL)
}

// writeProbe records whether anything was written.
type writeProbe struct {
	h       http.Header
	written atomic.Bool
}

func (p *writeProbe) Header() http.Header         { return p.h }
func (p *writeProbe) WriteHeader(int)             { p.written.Store(true) }
func (p *writeProbe) Write(b []byte) (int, error) { p.written.Store(true); return len(b), nil }

// TestEmbedClientGoneDuringABlock: the client hangs up during block two's
// wire call — nothing is written to it, no further block, the row says
// aborted, the wire work is settled once, no lease is held.
func TestEmbedClientGoneDuringABlock(t *testing.T) {
	e := newEmbedEngine(t, 8)
	rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 2, 1)
	ctx, hangUp := context.WithCancel(context.Background())
	defer hangUp()
	e.setRespond(func(_ http.ResponseWriter, r *http.Request, n int64, _ []string) bool {
		if n == 2 {
			hangUp()
			<-r.Context().Done()
		}
		return false
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(embedBody("emb", embedInputs(4)))).WithContext(ctx)
	w := &writeProbe{h: http.Header{}}
	rig.router.ServeHTTP(w, req)
	if w.written.Load() {
		t.Error("an answer was written to a client that hung up")
	}
	if h := e.hits.Load(); h != 2 {
		t.Errorf("wire hits %d, want 2", h)
	}
	if rows := rig.rows.all(); len(rows) != 1 || rows[0].Gateway.Finish != "aborted" {
		t.Errorf("rows %+v, want one aborted row", rows)
	}
	reservationsEndedOnce(t, rig.limiter, 1, 0)
	noLeaseHeld(t, rig.disp, e.srv.URL)
}

// postCancelable runs one embeddings request whose client hangs up when
// hangUp is called; it returns what was written and waits for the handler.
func postCancelable(rig *chatRig, body string) (w *writeProbe, hangUp func(), done chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	w = &writeProbe{h: http.Header{}}
	done = make(chan struct{})
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(body)).WithContext(ctx)
	go func() {
		defer close(done)
		rig.router.ServeHTTP(w, req)
	}()
	return w, cancel, done
}

// foreignHold is a foreign consumer that queues for host's one unit during
// block one, takes the unit at block one's release — the request's
// continuation then waits at the class head — and keeps it until release.
type foreignHold struct {
	lease atomic.Pointer[dispatch.Lease]
	done  chan struct{}
}

func holdAtBlockOne(t *testing.T, rig *chatRig, e *embedEngine) *foreignHold {
	t.Helper()
	h := &foreignHold{done: make(chan struct{})}
	e.setRespond(func(http.ResponseWriter, *http.Request, int64, []string) bool {
		if e.hits.Load() == 1 {
			go func() {
				defer close(h.done)
				ctx := context.WithValue(context.Background(), consumerKey{}, foreignConsumer(urgentApp(), "hold"))
				if l, _, err := rig.disp.Acquire(ctx, dispatch.Request{Target: dispatch.Target{Origin: e.srv.URL}, Class: dispatch.ClassExternal, Role: "embed"}); err == nil {
					h.lease.Store(l)
				}
			}()
			waitWaiting(t, rig.disp, e.srv.URL, 1, false)
		}
		return false
	})
	return h
}

// release frees the foreign unit once its acquire returned.
func (h *foreignHold) release() {
	select {
	case <-h.done:
	case <-time.After(5 * time.Second):
	}
	if l := h.lease.Load(); l != nil {
		l.Release()
	}
}

// waitContinuationQueued waits until the foreign consumer holds the unit and
// the request's continuation queues behind it.
func waitContinuationQueued(t *testing.T, rig *chatRig, e *embedEngine, h *foreignHold) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for h.lease.Load() == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if h.lease.Load() == nil {
		t.Fatal("the foreign consumer never took the unit")
	}
	waitWaiting(t, rig.disp, e.srv.URL, 1, false)
}

// TestEmbedClientGoneWhileWaiting (Fremd-Review 1 F-7): the client hangs up
// while the First waits, resp. while a continuation waits at the class
// head — nothing is written, no further wire call, the reservation ends
// once (settled iff a block reached the wire), no lease of the request is
// left.
func TestEmbedClientGoneWhileWaiting(t *testing.T) {
	t.Run("first", func(t *testing.T) {
		e := newEmbedEngine(t, 8)
		rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 2, 1)
		release := occupyExternal(t, rig.disp, e.srv.URL)
		defer release()
		w, hangUp, done := postCancelable(rig, embedBody("emb", embedInputs(3)))
		waitExternalWaiting(t, rig.disp, e.srv.URL)
		hangUp()
		<-done
		if w.written.Load() || e.hits.Load() != 0 || len(rig.rows.all()) != 0 {
			t.Errorf("written %v wire hits %d rows %d, want nothing", w.written.Load(), e.hits.Load(), len(rig.rows.all()))
		}
		reservationsEndedOnce(t, rig.limiter, 0, 0)
	})
	t.Run("continuation", func(t *testing.T) {
		e := newEmbedEngine(t, 8)
		rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 2, 1, withoutCensus)
		hold := holdAtBlockOne(t, rig, e)
		defer hold.release()
		w, hangUp, done := postCancelable(rig, embedBody("emb", embedInputs(3)))
		waitContinuationQueued(t, rig, e, hold)
		hangUp()
		<-done
		if w.written.Load() || e.hits.Load() != 1 {
			t.Errorf("written %v wire hits %d, want nothing written, 1 hit", w.written.Load(), e.hits.Load())
		}
		if rows := rig.rows.all(); len(rows) != 1 || rows[0].Gateway.Finish != "aborted" {
			t.Errorf("rows %+v, want one aborted row", rows)
		}
		reservationsEndedOnce(t, rig.limiter, 1, 0)
		hold.release()
		noLeaseHeld(t, rig.disp, e.srv.URL)
	})
}

// TestEmbedKillSwitchWhileAContinuationWaits (F-7): the gateway switched
// off while the continuation waits — the watcher ends the run: 404
// not_found, the block done is logged and settled.
func TestEmbedKillSwitchWhileAContinuationWaits(t *testing.T) {
	defer oai.SetWatchEvery(20 * time.Millisecond)()
	e := newEmbedEngine(t, 8)
	rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 2, 1, withoutCensus)
	hold := holdAtBlockOne(t, rig, e)
	defer hold.release()
	done := make(chan chatAnswer, 1)
	go func() { done <- rig.postEmbed(t, embedBody("emb", embedInputs(3))) }()
	waitContinuationQueued(t, rig, e, hold)
	rig.setSettings(func(s *oai.Settings) { s.Enabled = false })
	a := <-done
	if code, _, _ := a.envelope(t); a.status != http.StatusNotFound || code != oai.CodeNotFound {
		t.Fatalf("%d %s, want 404 not_found", a.status, a.body)
	}
	if rows := rig.rows.all(); len(rows) != 1 || rows[0].Gateway.Finish != "aborted" || e.hits.Load() != 1 {
		t.Errorf("rows %+v hits %d, want one aborted row, 1 hit", rows, e.hits.Load())
	}
	reservationsEndedOnce(t, rig.limiter, 1, 0)
}

// TestEmbedModelGoneDuringABlock (F-7): the alias is disabled while block
// one runs — the watcher's errModelGone: 404 model_not_found, the wire call
// aborted, no health fault.
func TestEmbedModelGoneDuringABlock(t *testing.T) {
	defer oai.SetWatchEvery(20 * time.Millisecond)()
	e := newEmbedEngine(t, 8)
	rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 2, 1)
	e.setRespond(func(_ http.ResponseWriter, r *http.Request, n int64, _ []string) bool {
		if n == 1 {
			rows := embedAliases()
			for i := range rows {
				if rows[i].name == "emb" {
					rows[i].enabled = false
				}
			}
			rig.reg.reload(t, rows)
			<-r.Context().Done()
		}
		return false
	})
	a := rig.postEmbed(t, embedBody("emb", embedInputs(3)))
	if code, _, _ := a.envelope(t); a.status != http.StatusNotFound || code != oai.CodeModelNotFound {
		t.Fatalf("%d %s, want 404 model_not_found", a.status, a.body)
	}
	if n := consecutiveFails(rig.pool, "spark-embed"); n != 0 || e.hits.Load() != 1 {
		t.Errorf("consecutive_fails %d hits %d, want 0 and 1", n, e.hits.Load())
	}
	reservationsEndedOnce(t, rig.limiter, 0, 1) // the engine had block one, no usage: kept
}

// TestEmbedGrantWithdrawnBeforeTheWrite (F-7, D8): the consumer's grant is
// withdrawn during the last block — the check right before the write
// answers 404 model_not_found; nothing of the vectors is sent.
func TestEmbedGrantWithdrawnBeforeTheWrite(t *testing.T) {
	e := newEmbedEngine(t, 8)
	rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 2, 1)
	ca := urgentApp()
	ca.AllModels = false
	rig.ca.Store(ca)
	rig.setGrant(idChatEmbed, true)
	e.setRespond(func(http.ResponseWriter, *http.Request, int64, []string) bool {
		if e.hits.Load() == 3 {
			rig.setGrant(idChatEmbed, false)
		}
		return false
	})
	a := rig.postEmbed(t, embedBody("emb", embedInputs(3)))
	if code, _, _ := a.envelope(t); a.status != http.StatusNotFound || code != oai.CodeModelNotFound || strings.Contains(string(a.body), "embedding\":[") {
		t.Fatalf("%d %.200s, want 404 model_not_found and no vector", a.status, a.body)
	}
	if rows := rig.rows.all(); len(rows) != 1 || rows[0].Gateway.Finish != "aborted" || rows[0].Telemetry.Metadata["sub_batches_done"] != 3 {
		t.Errorf("rows %+v, want one aborted row with 3 blocks done", rows)
	}
	reservationsEndedOnce(t, rig.limiter, 1, 0)
}

// brokenWriter accepts the header and fails every body write: the client
// went away while the answer streams.
type brokenWriter struct{ h http.Header }

func (b *brokenWriter) Header() http.Header       { return b.h }
func (b *brokenWriter) WriteHeader(int)           {}
func (b *brokenWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

// TestEmbedClientGoneDuringTheAnswer (F-7): the connection breaks while the
// answer streams — the row says aborted, the reservation is settled once
// (the wire work happened), no lease is left.
func TestEmbedClientGoneDuringTheAnswer(t *testing.T) {
	e := newEmbedEngine(t, 4096)
	rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 33, 1)
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(embedBody("emb4k", embedInputs(20))))
	rig.router.ServeHTTP(&brokenWriter{h: http.Header{}}, req)
	if rows := rig.rows.all(); len(rows) != 1 || rows[0].Gateway.Finish != "aborted" {
		t.Errorf("rows %+v, want one aborted row", rows)
	}
	reservationsEndedOnce(t, rig.limiter, 1, 0)
	noLeaseHeld(t, rig.disp, e.srv.URL)
}
