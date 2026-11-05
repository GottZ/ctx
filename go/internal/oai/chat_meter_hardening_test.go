package oai_test

// Hardening of the meter at the route after the second review of W06-10
// and the delta review 3: the refund of a held reservation (H-1), the debt
// (H-4), retry-after-ms rounded up (H-5, R10), no full refund after wire
// contact (H-7, the failover refusal H2-3), a refused connection is no wire
// contact (H2-4) and the usage counters are tested one by one (H2-5).

import (
	"context"
	"fmt"
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
	"github.com/GottZ/ctx/internal/keyquota"
	"github.com/GottZ/ctx/internal/oai"
)

func (c *meterClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// usageAnswer answers in the SGLang shape with the given usage.
func usageAnswer(prompt, completion int) func(w http.ResponseWriter, _ *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, strings.Replace(answerSGLang,
			`"usage":{"prompt_tokens":11,"total_tokens":14,"completion_tokens":3`,
			fmt.Sprintf(`"usage":{"prompt_tokens":%d,"total_tokens":%d,"completion_tokens":%d`, prompt, prompt+completion, completion), 1))
	}
}

// bigChat is a request whose estimate is above TPM 60 000 (charged at it).
func bigChat() string {
	return fmt.Sprintf(`{"model":"chat","messages":[{"role":"user","content":%q}]}`, strings.Repeat("a", 110000))
}

// holdPlanner holds the first Plan until released (a request waiting in the
// dispatcher or a long generation: the reservation is held while its charge
// runs off).
type holdPlanner struct {
	oai.AdmissionPlanner
	first   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (p *holdPlanner) Plan(ctx context.Context, ca *auth.ConsumerAuth, in oai.AdmissionInput) (oai.Plan, error) {
	if p.first.CompareAndSwap(false, true) {
		close(p.entered)
		<-p.release
	}
	return p.AdmissionPlanner.Plan(ctx, ca, in)
}

// TestMeterHeldRefundAtTheRoute is H-1 over the real route (review 2
// TestReview2DelayedRefundAtTheRoute, now a gate): A books the whole TPM
// bucket and is held 61 s; at 60 s B books the whole bucket and uses it; A
// ends with 14 real tokens. A's charge ran off before it ended, so its
// refund must not erase B's: C at 61 s is 429 with Retry-After 59.
func TestMeterHeldRefundAtTheRoute(t *testing.T) {
	u := newUpstream(t, answerSGLang)
	meter := newRigMeter(keyquota.Limits{RPM: 100, TPM: 60000, CompletionReserve: 1024}, true)
	hp := &holdPlanner{entered: make(chan struct{}), release: make(chan struct{})}
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withDeps(func(d *oai.Deps) {
		hp.AdmissionPlanner = d.Planner
		d.Planner = hp
		d.Limiter = meter
	}))
	doneA := make(chan chatAnswer, 1)
	go func() { doneA <- rig.post(t, bigChat()) }()
	select {
	case <-hp.entered:
	case a := <-doneA:
		t.Fatalf("A was answered without reaching the planner: %d %s", a.status, a.body)
	case <-time.After(10 * time.Second):
		t.Fatal("A never reached the planner")
	}
	meter.clock.advance(60 * time.Second)
	u.setRespond(usageAnswer(59000, 1000))
	b := rig.post(t, bigChat())
	meter.clock.advance(time.Second)
	u.setRespond(usageAnswer(11, 3))
	close(hp.release)
	a := <-doneA
	u.setRespond(usageAnswer(59000, 1000))
	c := rig.post(t, bigChat())
	t.Logf("A (held 61 s, 14 real tokens): %d; B at 60 s (60000 real): %d remaining-tokens %q; C at 61 s: %d Retry-After %q",
		a.status, b.status, b.header.Get("x-ratelimit-remaining-tokens"), c.status, c.header.Get("Retry-After"))
	if a.status != http.StatusOK || b.status != http.StatusOK {
		t.Fatalf("A %d, B %d, want both served", a.status, b.status)
	}
	if c.status != http.StatusTooManyRequests || c.header.Get("Retry-After") != "59" {
		t.Errorf("C at 61 s: %d Retry-After %q — want 429 with Retry-After 59 (B's 60 000 tokens are still owed; 120 000 within 1 s otherwise)",
			c.status, c.header.Get("Retry-After"))
	}
}

// TestMeterDebtAtTheRoute is H-4 (review 2 F-3, TestReview2DebtAtTheRoute
// as a gate): a request charged at TPM 60 000 whose answer reports 90 000
// real tokens leaves a debt — one minute later the same request waits 30 s.
func TestMeterDebtAtTheRoute(t *testing.T) {
	u := newUpstream(t, answerSGLang)
	u.setRespond(usageAnswer(85000, 5000))
	meter := newRigMeter(keyquota.Limits{RPM: 100, TPM: 60000, CompletionReserve: 1024}, true)
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withDeps(func(d *oai.Deps) { d.Limiter = meter }))
	if a := rig.post(t, bigChat()); a.status != http.StatusOK {
		t.Fatalf("first: %d %s", a.status, a.body)
	}
	meter.clock.advance(time.Minute)
	a := rig.post(t, bigChat())
	if a.status != http.StatusTooManyRequests || a.header.Get("Retry-After") != "30" || a.header.Get("retry-after-ms") != "30000" {
		t.Errorf("one minute after a 90 000-token answer: %d Retry-After %q retry-after-ms %q — want 429, 30, 30000",
			a.status, a.header.Get("Retry-After"), a.header.Get("retry-after-ms"))
	}
}

// TestMeterRetryAfterMsRoundsUp is H-5 (review 2 R10): the OpenAI SDKs read
// retry-after-ms first; a wait with a fraction of a millisecond is rounded
// up, never down (a retry 1 ms early meets a second 429). At 7 rpm the
// eighth request of a burst waits 8.571428568 s.
func TestMeterRetryAfterMsRoundsUp(t *testing.T) {
	u := newUpstream(t, answerSGLang)
	meter := newRigMeter(keyquota.Limits{RPM: 7, TPM: 1 << 30, CompletionReserve: 1024}, true)
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withDeps(func(d *oai.Deps) { d.Limiter = meter }))
	var a chatAnswer
	for range 8 {
		a = rig.post(t, chatBody("chat"))
	}
	if a.status != http.StatusTooManyRequests || a.header.Get("retry-after-ms") != "8572" || a.header.Get("Retry-After") != "9" {
		t.Errorf("eighth request: %d retry-after-ms %q Retry-After %q — want 429, 8572, 9", a.status, a.header.Get("retry-after-ms"), a.header.Get("Retry-After"))
	}
}

// TestMeterWireContactNeverRefundsInFull is H-7 (review 2 F-8): once a
// request reached the wire, its reservation is never given back in full —
// with the usage unknown (an upstream error, an answer without usage, a
// panic after the wire, the client gone during the wire call) the
// reservation stays booked as charged. Each case runs on its own consumer
// bucket; the reservation of chatBody is 1 042 tokens (18 for the messages,
// 1 024 completion reserve).
func TestMeterWireContactNeverRefundsInFull(t *testing.T) {
	const reservation = 1042
	noUsage := strings.Replace(answerSGLang, `,"usage":{"prompt_tokens":11,"total_tokens":14,"completion_tokens":3,"prompt_tokens_details":null}`, "", 1)
	if noUsage == answerSGLang {
		t.Fatal("the fixture answer has no usage object to remove")
	}
	cases := []struct {
		name    string
		respond func(w http.ResponseWriter, r *http.Request)
		panicAt int64 // HasGrant call that panics (2 = the check before the write, after the wire)
		cancel  bool  // the client goes away while the wire call runs
	}{
		{name: "upstream 400", respond: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"bad"}}`)
		}},
		{name: "answer without usage", respond: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, noUsage)
		}},
		{name: "panic after the wire", panicAt: 2},
		{name: "client gone during the wire call", cancel: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := newUpstream(t, answerSGLang)
			meter := newRigMeter(keyquota.Limits{RPM: 100, TPM: 60_000_000, CompletionReserve: 1024}, true)
			var grants atomic.Int64
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withDeps(func(d *oai.Deps) {
				d.Limiter = meter
				inner := d.HasGrant
				d.HasGrant = func(ctx context.Context, consumerID, aliasID string) (bool, error) {
					if grants.Add(1) == tc.panicAt {
						panic("h7: panic after the wire")
					}
					return inner(ctx, consumerID, aliasID)
				}
			}))
			rig.setGrant(idChat, true)
			ca := *rig.ca.Load()
			ca.AllModels = false // the grant read runs, the check before the write reads it again
			rig.ca.Store(&ca)
			switch {
			case tc.respond != nil:
				u.setRespond(tc.respond)
			case tc.cancel:
				u.setRespond(func(w http.ResponseWriter, r *http.Request) {
					cancel()
					<-r.Context().Done()
				})
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBody("chat"))).WithContext(ctx)
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			func() {
				defer func() { _ = recover() }()
				rig.router.ServeHTTP(w, req)
			}()
			if u.hits.Load() != 1 {
				t.Fatalf("upstream hits %d, want 1 (the case needs wire contact)", u.hits.Load())
			}
			h := meter.probe(t, &ca, keyquota.OpChat)
			if got := meter.lim.TPM - h.RemainingTokens; got != reservation {
				t.Errorf("%s: answer %d; the bucket holds %d tokens after the request, want its whole reservation %d (no full refund after wire contact)",
					tc.name, w.Code, got, reservation)
			}
		})
	}
}

// TestMeterFailoverRefusalAfterTheWireKeeps is H2-3 (review 3 F-1, over the
// rig's real planner and dispatcher): the first row answers 503 after the
// wire, the failover row's only external slot is held, the dispatcher
// refuses the failover acquire — the route answers through the admission
// writer (429 server_busy), and the reservation stays booked: the first
// row did the work.
func TestMeterFailoverRefusalAfterTheWireKeeps(t *testing.T) {
	first := newUpstream(t, answerSGLang)  // prio 20: tried first
	second := newUpstream(t, answerSGLang) // prio 10: the failover row
	first.setRespond(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"engine busy"}}`)
	})
	meter := newRigMeter(keyquota.Limits{RPM: 100, TPM: 60_000_000, CompletionReserve: 1024}, true)
	rig := newChatRig(t, []backends.Backend{second.row("spark1", backends.TrustFull, 10), first.row("spark2", backends.TrustFull, 20)}, 2, 1,
		withDeps(func(d *oai.Deps) { d.Limiter = meter }))
	hctx := context.WithValue(context.Background(), consumerKey{}, rig.ca.Load())
	hold, _, err := rig.disp.Acquire(hctx, dispatch.Request{Target: dispatch.Target{Origin: second.srv.URL}, Class: dispatch.ClassExternal, Role: "chat"})
	if err != nil {
		t.Fatalf("holding the failover row's external slot: %v", err)
	}
	defer hold.Release()
	a := rig.post(t, chatBody("chat"))
	code, _, _ := a.envelope(t)
	ca := *rig.ca.Load()
	held := meter.lim.TPM - meter.probe(t, &ca, keyquota.OpChat).RemainingTokens
	t.Logf("first row 503 after the wire, failover row full: answer %d %s; upstream hits first %d, failover %d; the bucket holds %d tokens",
		a.status, code, first.hits.Load(), second.hits.Load(), held)
	if a.status != http.StatusTooManyRequests || code != "server_busy" || first.hits.Load() != 1 || second.hits.Load() != 0 {
		t.Fatalf("answer %d %s, hits %d/%d — want 429 server_busy after one wire call on the first row", a.status, code, first.hits.Load(), second.hits.Load())
	}
	if held != 1042 {
		t.Errorf("the bucket holds %d tokens after a failover refusal behind a wire call, want the whole reservation 1042 (no refund after wire contact)", held)
	}
}

// TestMeterConnectRefusedGivesBack is H2-4 (review 3 F-4): a row whose host
// refuses the connection — no byte of the request reached any backend — is
// no wire contact for the meter: the reservation goes back (Abort), the
// client sees 503 upstream_unavailable.
func TestMeterConnectRefusedGivesBack(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	host := dead.URL
	dead.Close()
	u := newUpstream(t, answerSGLang)
	row := u.row("dead", backends.TrustFull, 10)
	row.Host = host
	meter := newRigMeter(keyquota.Limits{RPM: 100, TPM: 60_000_000, CompletionReserve: 1024}, true)
	rig := newChatRig(t, []backends.Backend{row}, 4, 1, withDeps(func(d *oai.Deps) { d.Limiter = meter }))
	a := rig.post(t, chatBody("chat"))
	code, _, _ := a.envelope(t)
	ca := *rig.ca.Load()
	held := meter.lim.TPM - meter.probe(t, &ca, keyquota.OpChat).RemainingTokens
	t.Logf("connection refused before any byte: answer %d %s; the bucket holds %d tokens", a.status, code, held)
	if a.status != http.StatusServiceUnavailable || code != oai.CodeUpstreamUnavailable {
		t.Fatalf("answer %d %s, want 503 upstream_unavailable", a.status, code)
	}
	if held != 0 {
		t.Errorf("the bucket holds %d tokens for a request no backend saw, want 0 (the reservation goes back)", held)
	}
}

// TestMeterUsageCountersOneByOne is H2-5 (review 3 F-5): the route tests
// each usage counter on its own, not their int sum — prompt_tokens
// MaxInt64 with completion_tokens 1 is a usage (Settle, a saturated debt),
// not "no usage" (Keep) by an overflowed sum.
func TestMeterUsageCountersOneByOne(t *testing.T) {
	u := newUpstream(t, answerSGLang)
	u.setRespond(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, strings.Replace(answerSGLang,
			`"usage":{"prompt_tokens":11,"total_tokens":14,"completion_tokens":3`,
			`"usage":{"prompt_tokens":9223372036854775807,"total_tokens":1,"completion_tokens":1`, 1))
	})
	meter := newRigMeter(keyquota.Limits{RPM: 100, TPM: 60000, CompletionReserve: 1024}, true)
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withDeps(func(d *oai.Deps) { d.Limiter = meter }))
	if a := rig.post(t, chatBody("chat")); a.status != http.StatusOK {
		t.Fatalf("answer %d %s", a.status, a.body)
	}
	ca := *rig.ca.Load()
	_, d := meter.m.Admit(keyquota.Subject{ConsumerID: ca.ConsumerID, TenantID: ca.TenantID, Op: keyquota.OpChat}, meter.lim, keyquota.Estimate{ClientMax: true})
	t.Logf("usage prompt MaxInt64, completion 1: next admission allowed=%v Retry-After %v", d.Allowed, d.RetryAfter)
	if d.Allowed || d.RetryAfter < 24*time.Hour {
		t.Errorf("after a usage of MaxInt64 + 1 tokens: %+v, want refused behind the saturated debt (settled, not kept)", d)
	}
}
