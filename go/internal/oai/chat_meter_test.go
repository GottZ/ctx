package oai_test

// The chat route over the REAL per-consumer meter (W06-10, internal/keyquota)
// in the chat rig: gates Z2 (the reservation ends exactly once per exit and
// the meter holds exactly the settled tokens), Z3 (the chain rules run
// before the meter), Z4 (the x-ratelimit-* and message contract), warming
// and the cap of B-06-28 at the route. rigMeter stands in for cmd/ctxd's
// meterLimiter, which this package cannot import: it maps the same fields
// one to one; the lane is bulk for service_tier flex (the production lane
// rule, handler.ResolveLane, is pinned in cmd/ctxd).

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/handler/v1census"
	"github.com/GottZ/ctx/internal/keyquota"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/GottZ/ctx/internal/oai"
	"github.com/GottZ/ctx/internal/promptguard"
)

// meterClock is a frozen clock for the rig's meter.
type meterClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *meterClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// rigMeter is the real meter behind the Limiter hook with fixed limits.
type rigMeter struct {
	m     *keyquota.Meter
	clock *meterClock
	lim   keyquota.Limits
}

func newRigMeter(lim keyquota.Limits, seeded bool) *rigMeter {
	c := &meterClock{t: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)}
	m := keyquota.New(c.now)
	if seeded {
		m.MarkSeeded()
	}
	return &rigMeter{m: m, clock: c, lim: lim}
}

func (r *rigMeter) Admit(_ context.Context, ca *auth.ConsumerAuth, _ modelalias.Alias, est oai.Estimate) (oai.LimitReservation, *oai.LimitRefusal) {
	op, _ := keyquota.ParseOp(est.Op)
	lane := keyquota.LaneUrgent
	if est.ServiceTier == "flex" {
		lane = keyquota.LaneBulk
	}
	res, d := r.m.Admit(keyquota.Subject{ConsumerID: ca.ConsumerID, TenantID: ca.TenantID, Op: op, Lane: lane}, r.lim,
		keyquota.Estimate{PromptRunes: est.PromptRunes, MaxCompletion: est.MaxTokens, ClientMax: est.ClientMaxTokens, InputRunes: est.InputRunes})
	if !d.Allowed {
		return nil, &oai.LimitRefusal{Code: oai.CodeRateLimitExceeded, Message: d.Message, RetryAfter: d.RetryAfter, ShouldRetry: d.Retryable}
	}
	return rigReservation{res}, nil
}

type rigReservation struct{ *keyquota.Reservation }

func (r rigReservation) Settle(u dispatch.Usage) {
	r.Reservation.Settle(keyquota.Usage{PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens})
}

// probe reads the state of (consumer, op) through one zero-cost admission
// on the urgent lane: the request it books is counted in the result.
func (r *rigMeter) probe(t *testing.T, ca *auth.ConsumerAuth, op keyquota.Op) keyquota.RateHeaders {
	t.Helper()
	_, d := r.m.Admit(keyquota.Subject{ConsumerID: ca.ConsumerID, TenantID: ca.TenantID, Op: op}, r.lim, keyquota.Estimate{ClientMax: true})
	if !d.Allowed {
		t.Fatalf("probe of %s refused: %+v", op, d)
	}
	return d.Headers
}

// The planner modes of the Z2 walk.
const (
	planNormal int32 = iota
	planRefuse       // Plan refuses: the chain does not offer the external class
	planPanic        // Plan panics after Admit
	planCancel       // the client goes away while Plan runs
)

// modePlanner wraps the rig's real planner with the Z2 exits.
type modePlanner struct {
	oai.AdmissionPlanner
	mode   atomic.Int32
	cancel atomic.Pointer[context.CancelFunc]
}

func (p *modePlanner) Plan(ctx context.Context, ca *auth.ConsumerAuth, in oai.AdmissionInput) (oai.Plan, error) {
	switch p.mode.Load() {
	case planRefuse:
		return oai.Plan{}, handler.ErrChainNotOffered
	case planPanic:
		panic("z2: the planner panics after the limiter admitted")
	case planCancel:
		(*p.cancel.Load())()
	}
	return p.AdmissionPlanner.Plan(ctx, ca, in)
}

// TestMeterReservationEndsOncePerExit is gate Z2: 1 000 requests through the
// real route end in turn by success (Settle 14 tokens), an upstream error
// after the wire (Keep: the usage is unknown, the reservation of 1 042
// tokens stays booked — W06-10 hardening H-7; before it, Settle 0 gave it
// back), a planner refusal (Reject), a panic after Admit (close() aborts)
// and a client gone during Plan (Abort) — afterwards the token bucket of the
// consumer holds exactly the settled tokens plus the kept reservations, and
// every request is counted once; only the successful answers carry
// x-ratelimit-*.
func TestMeterReservationEndsOncePerExit(t *testing.T) {
	u := newUpstream(t, answerSGLang)
	meter := newRigMeter(keyquota.Limits{RPM: 1 << 20, TPM: 60_000_000, CompletionReserve: 1024}, true)
	mp := &modePlanner{}
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 64, 1, withDeps(func(d *oai.Deps) {
		mp.AdmissionPlanner = d.Planner
		d.Planner = mp
		d.Limiter = meter
	}))
	const n = 1000
	// The reservation of chatBody: TokenEstimate of its 32-rune messages
	// (18) plus the completion reserve.
	const reservation = 18 + 1024
	var settled, kept int64
	counts := map[string]int{}
	for i := range n {
		mode := []string{"success", "upstream", "refuse", "panic", "cancel"}[i%5]
		counts[mode]++
		mp.mode.Store(planNormal)
		u.setRespond(nil)
		switch mode {
		case "upstream":
			// A 400 of the engine: a wire attempt without usage that
			// leaves the row's health alone.
			u.setRespond(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":{"message":"engine down"}}`)
			})
		case "refuse":
			mp.mode.Store(planRefuse)
		case "panic":
			mp.mode.Store(planPanic)
		case "cancel":
			mp.mode.Store(planCancel)
		}
		ctx, cancel := context.WithCancel(context.Background())
		mp.cancel.Store(&cancel)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBody("chat"))).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		func() {
			defer func() { _ = recover() }() // a panic past the rig's recovery would end the test
			rig.router.ServeHTTP(w, req)
		}()
		cancel()
		// The x-ratelimit-* headers ride on the successful answer only — an
		// answer after Admit that refused, failed or aborted carries none
		// (lead decision M2 (c): set ahead, they would count a reservation
		// that went back).
		limitHeader := w.Result().Header.Get("x-ratelimit-limit-requests")
		if mode == "success" {
			if w.Code != http.StatusOK || limitHeader == "" {
				t.Fatalf("request %d (%s): %d %s (x-ratelimit-limit-requests %q)", i, mode, w.Code, w.Body.Bytes(), limitHeader)
			}
			settled += 11 + 3
		} else if limitHeader != "" {
			t.Fatalf("request %d (%s): %d carries x-ratelimit headers %v, want none", i, mode, w.Code, w.Result().Header)
		}
		if mode == "upstream" {
			kept += reservation
		}
	}
	h := meter.probe(t, rig.ca.Load(), keyquota.OpChat)
	if got := meter.lim.TPM - h.RemainingTokens; got != settled+kept || h.ResetTokens != time.Duration(settled+kept)*time.Microsecond {
		t.Errorf("token bucket holds %d tokens (%v) after %v, want exactly the settled %d plus the kept %d", got, h.ResetTokens, counts, settled, kept)
	}
	if got := meter.lim.RPM - h.RemainingRequests; got != n+1 {
		t.Errorf("request bucket holds %d, want %d (every request counted once, plus the probe)", got, n+1)
	}
}

// TestMeterAfterTheChainRules is gate Z3 (design/06 §4.1, lead decision 6):
// an empty chain (503 no_eligible_backend) and the tenant gate's 429 come
// before the meter — its buckets stay untouched.
func TestMeterAfterTheChainRules(t *testing.T) {
	u := newUpstream(t, answerSGLang)
	meter := newRigMeter(keyquota.Limits{RPM: 100, TPM: 60000, CompletionReserve: 1024}, true)
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withDeps(func(d *oai.Deps) { d.Limiter = meter }))
	a := rig.post(t, chatBody("tchat")) // a tenant alias whose tenant has no row
	if code, _, _ := a.envelope(t); a.status != http.StatusServiceUnavailable || code != oai.CodeNoEligibleBackend {
		t.Fatalf("empty chain: %d %s, want 503 no_eligible_backend", a.status, a.body)
	}
	zero := 0
	rig.quota.SeedTenantForTest(rig.ca.Load().TenantID, backends.TenantQuota{Enabled: true, DailyCalls: &zero}, 0, 0)
	a = rig.post(t, chatBody("chat"))
	if code, _, _ := a.envelope(t); a.status != http.StatusTooManyRequests || code != oai.CodeInsufficientQuota {
		t.Fatalf("tenant gate: %d %s, want 429 insufficient_quota", a.status, a.body)
	}
	h := meter.probe(t, rig.ca.Load(), keyquota.OpChat)
	if h.RemainingRequests != 99 || h.RemainingTokens != 60000 {
		t.Errorf("meter after two chain refusals: %+v, want 99 requests and 60000 tokens remaining (only the probe counted)", h)
	}
	if u.hits.Load() != 0 {
		t.Errorf("upstream hits %d, want 0", u.hits.Load())
	}
}

// TestMeterHeaderAndTextContract is gate Z4 (design/06 §4.4): a successful
// answer carries the six x-ratelimit-* headers of the requested op's bucket
// as the request's lane sees it — an exhausted embed bucket of the same
// consumer and another consumer's exhausted chat bucket do not show — and a
// refusal names only the caller's own limit.
func TestMeterHeaderAndTextContract(t *testing.T) {
	u := newUpstream(t, answerSGLang)
	meter := newRigMeter(keyquota.Limits{RPM: 10, TPM: 60000, CompletionReserve: 1024, XUReserve: 0.25}, true)
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withDeps(func(d *oai.Deps) { d.Limiter = meter }))
	ca := rig.ca.Load()
	// The same consumer's embed bucket and another consumer's chat bucket,
	// both in debt.
	for _, s := range []keyquota.Subject{
		{ConsumerID: ca.ConsumerID, TenantID: ca.TenantID, Op: keyquota.OpEmbed},
		{ConsumerID: "00000000-0000-4000-8000-0000000000c9", TenantID: ca.TenantID, Op: keyquota.OpChat},
	} {
		for range 10 {
			res, d := meter.m.Admit(s, meter.lim, keyquota.Estimate{InputRunes: 9, ClientMax: true})
			if d.Allowed {
				res.Settle(keyquota.Usage{PromptTokens: 100000})
			}
		}
	}

	a := rig.post(t, chatBody("chat"))
	if a.status != http.StatusOK {
		t.Fatalf("urgent: %d %s", a.status, a.body)
	}
	// TokenEstimate of the messages as they go upstream + the completion
	// reserve (no client max_tokens).
	reserve := int64(promptguard.TokenEstimate(len(`[{"role":"user","content":"hi"}]`)) + 1024)
	want := map[string]string{
		"x-ratelimit-limit-requests": "10", "x-ratelimit-remaining-requests": "9",
		"x-ratelimit-limit-tokens": "60000", "x-ratelimit-remaining-tokens": strconv.FormatInt(60000-reserve, 10),
		"x-ratelimit-reset-requests": "6s", "x-ratelimit-reset-tokens": (time.Duration(reserve) * time.Millisecond).String(),
	}
	for k, v := range want {
		if got := a.header.Get(k); got != v {
			t.Errorf("urgent: %s = %q, want %q", k, got, v)
		}
	}
	// The bulk lane's view: limit 1 − r of each bucket.
	a = rig.post(t, chatBody("chat", `"service_tier":"flex"`))
	if a.status != http.StatusOK {
		t.Fatalf("bulk: %d %s", a.status, a.body)
	}
	if a.header.Get("x-ratelimit-limit-requests") != "7" || a.header.Get("x-ratelimit-limit-tokens") != "45000" {
		t.Errorf("bulk: limits %q / %q, want 7 / 45000 (the bulk lane's share)",
			a.header.Get("x-ratelimit-limit-requests"), a.header.Get("x-ratelimit-limit-tokens"))
	}
	var took time.Duration
	for range 9 { // requests 3 to 11 of the minute
		start := time.Now()
		a = rig.post(t, chatBody("chat"))
		took = time.Since(start)
	}
	// L0 refuses at once, it never waits (design/06 §4.1; "at once" = 50 ms,
	// Index Q19).
	if took > 50*time.Millisecond {
		t.Errorf("the refusal took %v, want at once (≤ 50 ms)", took)
	}
	code, _, typ := a.envelope(t)
	if a.status != http.StatusTooManyRequests || code != oai.CodeRateLimitExceeded || typ != "rate_limit_error" {
		t.Fatalf("eleventh request: %d %s, want 429 rate_limit_exceeded", a.status, a.body)
	}
	if !strings.Contains(string(a.body), `"message":"Rate limit reached for requests (chat): 10/min."`) {
		t.Errorf("refusal body %s, want only the caller's own limit", a.body)
	}
	for _, foreign := range []string{"c9", "embed", "100000", "45000", "instance", "deployment"} {
		if strings.Contains(string(a.body), foreign) {
			t.Errorf("refusal body %s names %q — a foreign limit or consumer", a.body, foreign)
		}
	}
	if a.header.Get("x-ratelimit-limit-requests") != "" {
		t.Errorf("refusal carries x-ratelimit headers %v, want none (success answers only)", a.header)
	}
	if ra, ms := a.header.Get("Retry-After"), a.header.Get("retry-after-ms"); ra != "6" || ms != "6000" || a.header.Get("x-should-retry") != "true" {
		t.Errorf("refusal: Retry-After %q, retry-after-ms %q, x-should-retry %q — want 6, 6000, true", ra, ms, a.header.Get("x-should-retry"))
	}
}

// TestMeterWarmingAtTheRoute: an unseeded meter answers 429
// rate_limit_exceeded, Retry-After 5, retry-after-ms 5000, x-should-retry
// true — before any acquire or wire contact (design/06 §4.1 step 7, §4.4;
// lead decision O-10-2 (a)).
func TestMeterWarmingAtTheRoute(t *testing.T) {
	u := newUpstream(t, answerSGLang)
	meter := newRigMeter(keyquota.Limits{RPM: 10, TPM: 60000, CompletionReserve: 1024}, false)
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withDeps(func(d *oai.Deps) { d.Limiter = meter }))
	rig.recorder.Take()
	a := rig.post(t, chatBody("chat"))
	code, _, typ := a.envelope(t)
	if a.status != http.StatusTooManyRequests || code != oai.CodeRateLimitExceeded || typ != "rate_limit_error" {
		t.Fatalf("warming: %d %s, want 429 rate_limit_exceeded", a.status, a.body)
	}
	if a.header.Get("Retry-After") != "5" || a.header.Get("retry-after-ms") != "5000" || a.header.Get("x-should-retry") != "true" {
		t.Errorf("warming headers %v, want Retry-After 5, retry-after-ms 5000, x-should-retry true", a.header)
	}
	for _, e := range rig.recorder.Take() {
		if e.Kind == v1census.KindAcquire || e.Kind == v1census.KindTry {
			t.Errorf("an acquire ran behind the warming refusal: %+v", e)
		}
	}
	if u.hits.Load() != 0 || len(rig.rows.all()) != 0 {
		t.Errorf("hits %d, rows %d, want 0/0", u.hits.Load(), len(rig.rows.all()))
	}
}

// TestMeterCapAtTheRoute is B-06-28 at the route: a prompt estimated above
// TPM 60 000 is served on a full bucket; its 45 000 real tokens settle, the
// same request again waits the debt off (429, Retry-After 45).
func TestMeterCapAtTheRoute(t *testing.T) {
	u := newUpstream(t, answerSGLang)
	u.setRespond(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, strings.Replace(answerSGLang,
			`"usage":{"prompt_tokens":11,"total_tokens":14,"completion_tokens":3`,
			`"usage":{"prompt_tokens":44000,"total_tokens":45000,"completion_tokens":1000`, 1))
	})
	meter := newRigMeter(keyquota.Limits{RPM: 100, TPM: 60000, CompletionReserve: 1024}, true)
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withDeps(func(d *oai.Deps) { d.Limiter = meter }))
	big := fmt.Sprintf(`{"model":"chat","messages":[{"role":"user","content":%q}]}`, strings.Repeat("a", 110000))
	a := rig.post(t, big)
	if a.status != http.StatusOK || a.header.Get("x-ratelimit-remaining-tokens") != "0" {
		t.Fatalf("estimate above TPM on a full bucket: %d %s (remaining tokens %q), want 200 with the bucket charged at its limit",
			a.status, a.body, a.header.Get("x-ratelimit-remaining-tokens"))
	}
	a = rig.post(t, big)
	code, _, _ := a.envelope(t)
	if a.status != http.StatusTooManyRequests || code != oai.CodeRateLimitExceeded || a.header.Get("Retry-After") != "45" || a.header.Get("retry-after-ms") != "45000" {
		t.Errorf("follow-up: %d %s (Retry-After %q, retry-after-ms %q), want 429 waiting the 45 000 settled tokens off",
			a.status, a.body, a.header.Get("Retry-After"), a.header.Get("retry-after-ms"))
	}
	if n := u.hits.Load(); n != 1 {
		t.Errorf("upstream hits %d, want 1", n)
	}
	if rows := rig.rows.all(); len(rows) != 1 || rows[0].Pipeline != llmlog.PipelineGatewayChat {
		t.Errorf("rows %d, want one gateway-chat row", len(rows))
	}
}
