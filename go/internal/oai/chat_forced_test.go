package oai_test

// The route's half of the shutdown's forced settle (wave W06-13 (d), (e);
// design/06 §4.2 "Usage-Quellen beim Settle", "Shutdown-Reihenfolge" step
// 2): a stream hands the runes it relays to its reservation (AddStreamed),
// every route registers its row writer (OnForcedSettle) at the admission,
// and a reservation the forced settle ended gets exactly one row — the
// forced one with usage=estimated in the columns — never a second one from
// the route.

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/GottZ/ctx/internal/oai"
)

// forcedLimiter hands out reservations with the forced-settle half: the
// runes they were told, the writer they got and whether a test forced them.
type forcedLimiter struct {
	streamUsageLimiter
	mu       sync.Mutex
	last     *forcedReservation
	admitted chan *forcedReservation
}

type forcedReservation struct {
	streamUsageReservation
	runes  atomic.Int64
	forced atomic.Bool
	ended  atomic.Bool
	mu     sync.Mutex
	fn     func(dispatch.Usage)
}

func (r *forcedReservation) end(f func()) {
	if r.ended.CompareAndSwap(false, true) {
		f()
	}
}
func (r *forcedReservation) Settle(u dispatch.Usage) {
	r.end(func() { r.streamUsageReservation.Settle(u) })
}
func (r *forcedReservation) Keep()  { r.end(r.streamUsageReservation.Keep) }
func (r *forcedReservation) Abort() { r.end(r.streamUsageReservation.Abort) }
func (r *forcedReservation) Reject(reason string, w time.Duration) {
	r.end(func() { r.streamUsageReservation.Reject(reason, w) })
}
func (r *forcedReservation) AddStreamed(n int) { r.runes.Add(int64(n)) }
func (r *forcedReservation) OnForcedSettle(fn func(dispatch.Usage)) {
	r.mu.Lock()
	r.fn = fn
	r.mu.Unlock()
}
func (r *forcedReservation) Forced() bool { return r.forced.Load() }

// force is ForceSettleAll for one reservation: it wins the end, then writes.
func (r *forcedReservation) force(u dispatch.Usage) bool {
	won := false
	r.end(func() { won = true; r.forced.Store(true) })
	if won {
		r.mu.Lock()
		fn := r.fn
		r.mu.Unlock()
		if fn != nil {
			fn(u)
		}
	}
	return won
}

func (l *forcedLimiter) Admit(_ context.Context, _ *auth.ConsumerAuth, _ modelalias.Alias, e oai.Estimate) (oai.LimitReservation, *oai.LimitRefusal) {
	l.admits.Add(1)
	r := &forcedReservation{streamUsageReservation: streamUsageReservation{l: &l.streamUsageLimiter, held: fakeChatCharge(e)}}
	l.mu.Lock()
	l.last = r
	l.mu.Unlock()
	if l.admitted != nil {
		l.admitted <- r
	}
	return r, nil
}

// TestStreamForcedSettleWritesOneRow: a stream that relayed "hello from"
// (10 runes) told its reservation exactly those runes; the shutdown's forced
// settle ends it while the upstream still holds the stream — its writer
// writes the request's one row (usage estimated, the forced tokens in the
// columns) — and the route, ending afterwards, writes none. Red: the route
// knows no forced settle (no AddStreamed, no writer, a second row).
func TestStreamForcedSettleWritesOneRow(t *testing.T) {
	hold := make(chan struct{})
	end := newUpstreamEnd()
	u := newUpstream(t, "")
	u.setRespond(sseRespond([]sseStep{{data: sglangChunk("hello", "")}, {data: sglangChunk(" from", "")}, {hold: hold}}, nil, end))
	lim := &forcedLimiter{admitted: make(chan *forcedReservation, 1)}
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings,
		func(r *chatRig) { r.deps.Limiter = lim })
	srv := serveHTTP(t, rig.router)
	resp, err := openStream(context.Background(), srv, streamBody("chat"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	each := make(chan sseEvent, 16)
	go func() {
		_, _ = readEvents(resp.Body, each)
		close(each)
	}()
	for got := 0; got < 2; got++ {
		if _, ok := nextData(each, 5*time.Second); !ok {
			t.Fatalf("frame %d of 2 did not arrive (status %d)", got+1, resp.StatusCode)
		}
	}
	r := <-lim.admitted
	if !waitFor(2*time.Second, func() bool { return r.runes.Load() == 10 }) {
		t.Fatalf("AddStreamed told %d runes, want 10 (\"hello from\")", r.runes.Load())
	}
	if !r.force(dispatch.Usage{PromptTokens: 7, CompletionTokens: 6}) {
		t.Fatal("the forced settle lost against a route that had not ended")
	}
	close(hold)
	if !end.wait(5 * time.Second) {
		t.Fatal("the upstream did not end")
	}
	if !waitFor(2*time.Second, func() bool { return lim.admits.Load() == 1 && len(rig.rows.all()) >= 1 }) {
		t.Fatal("no row")
	}
	time.Sleep(50 * time.Millisecond) // a second row would land by now
	rows := rig.rows.all()
	if len(rows) != 1 {
		t.Fatalf("%d rows, want exactly the forced one", len(rows))
	}
	row := rows[0]
	if row.Gateway.Usage != llmlog.UsageEstimated || row.Telemetry.PromptTokens != 7 || row.Telemetry.CompletionTokens != 6 ||
		row.Pipeline != llmlog.PipelineGatewayChat || row.ConsumerID == "" || !row.Gateway.Stream || row.Gateway.Finish != llmlog.FinishAborted {
		t.Errorf("forced row %+v, want gateway-chat, the consumer, stream, aborted, estimated 7/6 in the columns", row)
	}
	if lim.settles.Load()+lim.keeps.Load()+lim.aborts.Load()+lim.rejects.Load() != 0 {
		t.Errorf("the route ended the forced reservation again: settle/keep/abort/reject %d/%d/%d/%d",
			lim.settles.Load(), lim.keeps.Load(), lim.aborts.Load(), lim.rejects.Load())
	}
}
