package oai_test

// W06-12 gates (a) and (b) on the chat route, through the real planner's
// writer: (a) I-06-2 against design/03 (K-06-22) — 1 000 external L2
// refusals write no LLM-log row and are 1 000 capacity/server_busy
// declarations, one ledger cell; (b) the survivorship probe (B-06-25) — 100
// requests whose wait budget ran out after 100 ms are 100 waits in the
// ledger, wait_ms_sum >= 100 × 100 ms. The limiter is a double of cmd/ctxd's
// meterReservation (Reject hands the wait to the request's record), whose
// own behaviour cmd/ctxd's TestMeterLimiterDeclaresItsRefusals pins.

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/keyquota"
	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/GottZ/ctx/internal/oai"
)

// slowFaultPlanner is the real planner whose acquirer's First waits `wait`
// and then answers err — a refusal after a vain queue wait.
type slowFaultPlanner struct {
	oai.AdmissionPlanner
	err  error
	wait time.Duration
}

func (p slowFaultPlanner) Plan(ctx context.Context, ca *auth.ConsumerAuth, in oai.AdmissionInput) (oai.Plan, error) {
	plan, err := p.AdmissionPlanner.Plan(ctx, ca, in)
	if err == nil {
		plan.Acquirer = slowFaultAcquirer{RequestAcquirer: plan.Acquirer, err: p.err, wait: p.wait}
	}
	return plan, err
}

type slowFaultAcquirer struct {
	oai.RequestAcquirer
	err  error
	wait time.Duration
}

func (a slowFaultAcquirer) First(ctx context.Context, _ *backends.Backend, _ string, _ time.Duration) (*dispatch.Lease, context.Context, error) {
	select {
	case <-time.After(a.wait):
	case <-ctx.Done():
	}
	return nil, nil, a.err
}

// recordLimiter admits everything; its reservation's Reject hands the wait
// to the request's record, as cmd/ctxd's meterReservation does.
type recordLimiter struct{}

type recordReservation struct{ rec *oai.AccessRecord }

func (recordLimiter) Admit(ctx context.Context, _ *auth.ConsumerAuth, _ modelalias.Alias, _ oai.Estimate) (oai.LimitReservation, *oai.LimitRefusal) {
	return recordReservation{rec: oai.AccessRecordFrom(ctx)}, nil
}

func (recordReservation) Settle(dispatch.Usage) {}
func (recordReservation) Keep()                 {}
func (recordReservation) Abort()                {}
func (r recordReservation) Reject(_ string, waited time.Duration) {
	r.rec.NoteRejectionWait(waited)
}
func (recordReservation) Headers() http.Header { return nil }
func (recordReservation) Held() int64          { return 0 }

// ledgerOf notes the rig's records as the access-log stage would (status
// >= 400 and a declaration) and returns the drained cells.
func ledgerOf(t *testing.T, r *chatRig, n int) []keyquota.RejectionDelta {
	t.Helper()
	l := keyquota.NewLedger(time.Now)
	for i := range n {
		select {
		case rec := <-r.recs:
			if rej, ok := rec.Rejection(); ok {
				l.Note(rej)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("record %d missing", i)
		}
	}
	if d := l.Dropped(); d != 0 {
		t.Fatalf("ledger dropped %d note(s)", d)
	}
	return l.Drain()
}

// TestRefusalLedgerGateA_L2RefusalsWriteNoLLMLogRow is gate (a).
func TestRefusalLedgerGateA_L2RefusalsWriteNoLLMLogRow(t *testing.T) {
	u := newUpstream(t, answerSGLang)
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withDeps(func(d *oai.Deps) {
		d.Limiter = recordLimiter{}
	}))
	// The refusal comes as the acquire's bare error: the walk wraps it into
	// the AdmissionError with the row's host (the target of the ledger).
	rig.deps.Planner = slowFaultPlanner{AdmissionPlanner: rig.planner,
		err: &dispatch.RejectionError{Err: dispatch.ErrPrincipalSaturated, RetryAfter: time.Second}}
	rig.router = rig.mount(oai.ChatCompletions(rig.deps))
	rows := len(rig.rows.all())
	const n = 1000
	for i := range n {
		a := rig.post(t, chatBody("chat"))
		if code, _, _ := a.envelope(t); a.status != http.StatusTooManyRequests || code != oai.CodeServerBusy {
			t.Fatalf("request %d: %d %s, want 429 server_busy", i, a.status, a.body)
		}
	}
	if got := len(rig.rows.all()) - rows; got != 0 {
		t.Fatalf("%d LLM-log row(s) for %d L2 refusals, want 0 (I-06-2)", got, n)
	}
	if hits := u.hits.Load(); hits != 0 {
		t.Fatalf("upstream hits %d, want 0", hits)
	}
	d := ledgerOf(t, rig, n)
	if len(d) != 1 || d[0].N != n || d[0].Reason != keyquota.RejectCapacity || d[0].Detail != keyquota.DetailServerBusy ||
		d[0].Class != dispatch.ClassExternal.String() || d[0].Target != u.srv.URL {
		t.Fatalf("ledger %+v, want one capacity/server_busy cell n=%d, class %s, target %s", d, n, dispatch.ClassExternal, u.srv.URL)
	}
}

// TestRefusalLedgerGateB_Survivorship is gate (b): 100 requests whose wait
// budget ran out after 100 ms (MaxWait) ⇒ wait_n 100, wait_ms_sum >= 10 000.
func TestRefusalLedgerGateB_Survivorship(t *testing.T) {
	const maxWait = 100 * time.Millisecond
	u := newUpstream(t, answerSGLang)
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withDeps(func(d *oai.Deps) {
		d.Limiter = recordLimiter{}
		d.MaxWait = func() time.Duration { return maxWait }
	}))
	rig.deps.Planner = slowFaultPlanner{AdmissionPlanner: rig.planner, wait: maxWait,
		err: &dispatch.RejectionError{Err: dispatch.ErrWaitExpired, RetryAfter: time.Second}}
	rig.router = rig.mount(oai.ChatCompletions(rig.deps))
	// The 100 requests wait side by side (the wait is the acquirer's sleep,
	// not a shared queue): the probe costs one MaxWait, not a hundred.
	const n = 100
	var wg sync.WaitGroup
	answers := make(chan chatAnswer, n)
	begin := time.Now()
	for range n {
		wg.Go(func() { answers <- rig.post(t, chatBody("chat")) })
	}
	wg.Wait()
	close(answers)
	t.Logf("survivorship: %d requests in %s", n, time.Since(begin).Round(time.Millisecond))
	for a := range answers {
		if code, _, _ := a.envelope(t); a.status != http.StatusTooManyRequests || code != oai.CodeServerBusy {
			t.Fatalf("a request answered %d %s, want 429 server_busy", a.status, a.body)
		}
	}
	d := ledgerOf(t, rig, n)
	if len(d) != 1 || d[0].Detail != keyquota.DetailWaitExpired || d[0].N != n || d[0].WaitN != n || d[0].WaitMsSum < n*maxWait.Milliseconds() ||
		d[0].Target != u.srv.URL || d[0].Class != dispatch.ClassExternal.String() {
		t.Fatalf("ledger %+v, want one capacity/wait_expired cell n=%d wait_n=%d wait_ms_sum>=%d", d, n, n, n*maxWait.Milliseconds())
	}
	t.Logf("survivorship: n=%d wait_n=%d wait_ms_sum=%d (floor %d)", d[0].N, d[0].WaitN, d[0].WaitMsSum, n*maxWait.Milliseconds())
}
