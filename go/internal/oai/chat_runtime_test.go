package oai_test

// Runtime gates of the chat route (W02-07): the herald and the internal
// partition (G12, B11), refusals without llmlog rows, the jittered
// Retry-After end to end and the write deadline at entry (G13, B22/B23),
// the watcher (G15, B12), and the admission census over the real handler
// with the real planner (G16, lead decisions 4 and 5).

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/handler/v1census"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/oai"
)

// blocker makes an upstream hold every request until released (or until the
// request is aborted) and reports entries and aborts.
type blocker struct {
	entered chan struct{}
	aborted chan time.Time
	release chan struct{}
	once    sync.Once
}

func newBlocker() *blocker {
	return &blocker{entered: make(chan struct{}, 256), aborted: make(chan time.Time, 256), release: make(chan struct{})}
}

func (b *blocker) open() { b.once.Do(func() { close(b.release) }) }

func (b *blocker) respond(answer string) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		b.entered <- struct{}{}
		select {
		case <-r.Context().Done():
			b.aborted <- time.Now()
			return
		case <-b.release:
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, answer)
	}
}

func waitEntered(t *testing.T, b *blocker, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-b.entered:
		case <-time.After(5 * time.Second):
			t.Fatalf("upstream entries: %d of %d", i, n)
		}
	}
}

// heldOn is the dispatcher's held lease count on host.
func heldOn(d *dispatch.Dispatcher, host string) (held, external int) {
	for _, ts := range d.Snapshot().Targets {
		if ts.Origin == host {
			return ts.Held, ts.HeldExternal
		}
	}
	return 0, 0
}

// --- G12 (B11): the herald and the internal partition ---.

// TestV1DoesNotRaiseHerald: a running /v1 request leaves the dispatcher's
// interactive demand at 0 — the route is no WithScheduler mount (design/02
// §4.11.4). The red control mounts the same handler through
// handler.WithScheduler: the demand rises.
func TestV1DoesNotRaiseHerald(t *testing.T) {
	for _, tc := range []struct {
		name      string
		wrap      bool
		wantRaise bool
	}{{"route as mounted", false, false}, {"red control: WithScheduler mount", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			u := newUpstream(t, answerSGLang)
			b := newBlocker()
			u.setRespond(b.respond(answerSGLang))
			rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
			if tc.wrap {
				rig.router = rig.mount(handler.WithScheduler(rig.disp, oai.ChatCompletions(rig.deps).ServeHTTP))
			}
			done := make(chan chatAnswer, 1)
			go func() { done <- rig.post(t, chatBody("chat")) }()
			waitEntered(t, b, 1)
			demand := rig.disp.InteractiveDemand()
			b.open()
			a := <-done
			if a.status != http.StatusOK {
				t.Fatalf("answer %d %s", a.status, a.body)
			}
			if raised := demand > 0; raised != tc.wantRaise {
				t.Errorf("InteractiveDemand() during the request = %d, want raised=%v", demand, tc.wantRaise)
			}
		})
	}
}

// TestExternalCannotStarveInternalBackground: on a target with S = 2, R = 1
// (E = 1), 40 external requests hold and wait while one background arm
// acquires — it is admitted on the reserved slot, no ErrQueueFull, no class
// downgrade, and the external class never holds more than E.
func TestExternalCannotStarveInternalBackground(t *testing.T) {
	u := newUpstream(t, answerSGLang)
	b := newBlocker()
	u.setRespond(b.respond(answerSGLang))
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 2, 1)
	var wg sync.WaitGroup
	answers := make(chan chatAnswer, 40)
	for i := 0; i < 40; i++ {
		wg.Go(func() { answers <- rig.post(t, chatBody("chat")) })
	}
	waitEntered(t, b, 1)
	time.Sleep(50 * time.Millisecond) // the rest queue up or are refused at once
	_, ext := heldOn(rig.disp, u.srv.URL)
	actx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	lease, _, err := rig.disp.Acquire(actx, dispatch.Request{Target: dispatch.Target{Origin: u.srv.URL}, Class: dispatch.ClassBackground, Role: "chat"})
	if err != nil {
		t.Fatalf("background arm under external load: %v (ErrQueueFull=%v)", err, errors.Is(err, dispatch.ErrQueueFull))
	}
	lease.Release()
	if ext > 1 {
		t.Errorf("external leases held = %d, want ≤ E = 1", ext)
	}
	if snap := rig.disp.Snapshot(); snap.ClassDowngrades != 0 {
		t.Errorf("class_downgrades = %d, want 0", snap.ClassDowngrades)
	}
	b.open()
	wg.Wait()
	close(answers)
	ok := 0
	for a := range answers {
		if a.status == http.StatusOK {
			ok++
		}
	}
	if ok == 0 {
		t.Error("no external request was served")
	}
}

// backgroundPlanner is the planner bug of the red control: it binds the
// in-house background class (the Revision-0 interim, design/02 B11).
type backgroundPlanner struct{ oai.AdmissionPlanner }

type bgAcquirer struct{ llm.Admission }

func (a bgAcquirer) Class() dispatch.Class { return a.Admission.Class }
func (a bgAcquirer) First(ctx context.Context, b *backends.Backend, role string, t time.Duration) (*dispatch.Lease, context.Context, error) {
	return a.Acquire(ctx, b, role, t)
}
func (a bgAcquirer) Next(ctx context.Context, b *backends.Backend, role string, t time.Duration) (*dispatch.Lease, context.Context, error) {
	return a.Acquire(ctx, b, role, t)
}
func (a bgAcquirer) Continue(context.Context, *backends.Backend, string, time.Duration) (*dispatch.Lease, context.Context, error) {
	return nil, nil, llm.ErrNoContinuation
}
func (a bgAcquirer) Blocks() []int { return []int{1} }

func (p backgroundPlanner) Plan(ctx context.Context, ca *auth.ConsumerAuth, in oai.AdmissionInput) (oai.Plan, error) {
	plan, err := p.AdmissionPlanner.Plan(ctx, ca, in)
	if err != nil {
		return plan, err
	}
	plan.Acquirer = bgAcquirer{llm.Admission{Admitter: dispatchOf(ctx), Class: dispatch.ClassBackground}}
	return plan, nil
}

type dispatcherKey struct{}

func dispatchOf(ctx context.Context) dispatch.Admitter {
	return ctx.Value(dispatcherKey{}).(dispatch.Admitter)
}

// TestPlannerBindingBackgroundIsRefusedByTheWalker: a planner that binds the
// background class never reaches a lease — the walker's class assert
// (ErrWalkClass) refuses before any acquire: 503 internal_error, 0 wire
// hits, nothing held. This is the red side of G12 (the route answers 503
// instead of 200 under that planner bug).
func TestPlannerBindingBackgroundIsRefusedByTheWalker(t *testing.T) {
	u := newUpstream(t, answerSGLang)
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 2, 1)
	rig.deps.Planner = backgroundPlanner{rig.planner}
	h := oai.ChatCompletions(rig.deps)
	rig.router = rig.mount(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), dispatcherKey{}, dispatch.Admitter(rig.disp))))
	}))
	a := rig.post(t, chatBody("chat"))
	code, _, _ := a.envelope(t)
	if a.status != http.StatusServiceUnavailable || code != oai.CodeInternalError || a.header.Get("x-should-retry") != "false" {
		t.Fatalf("background planner: %d %s (x-should-retry %q), want 503 internal_error, x-should-retry false", a.status, a.body, a.header.Get("x-should-retry"))
	}
	if n := u.hits.Load(); n != 0 {
		t.Errorf("wire hits = %d, want 0", n)
	}
	if held, _ := heldOn(rig.disp, u.srv.URL); held != 0 {
		t.Errorf("held = %d, want 0", held)
	}
}

// --- G13 (B22/B23) ---.

// occupyExternal fills host's external partition with a lease of another
// consumer and returns its release.
func occupyExternal(t *testing.T, d *dispatch.Dispatcher, host string) func() {
	t.Helper()
	other := urgentApp()
	other.ConsumerID, other.TenantID = "00000000-0000-4000-8000-00000000cc99", "00000000-0000-4000-8000-00000000cc98"
	ctx := context.WithValue(context.Background(), consumerKey{}, other)
	l, _, err := d.Acquire(ctx, dispatch.Request{Target: dispatch.Target{Origin: host}, Class: dispatch.ClassExternal, Role: "chat"})
	if err != nil {
		t.Fatalf("occupy %s: %v", host, err)
	}
	return l.Release
}

// TestV1RejectionWritesNoLLMLogRow: 1 000 admission refusals (the partition
// full, Ctx-Max-Wait-Ms: 0 ⇒ refused at once as predicted) write 0 llmlog
// rows — the refusal is counted by the access log and the ledger, never a
// row with its own goroutine and INSERT (B22).
func TestV1RejectionWritesNoLLMLogRow(t *testing.T) {
	u := newUpstream(t, answerSGLang)
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 2, 1)
	defer occupyExternal(t, rig.disp, u.srv.URL)()
	for i := 0; i < 1000; i++ {
		a := rig.post(t, chatBody("chat"), "Ctx-Max-Wait-Ms", "0")
		if a.status != http.StatusTooManyRequests {
			t.Fatalf("refusal %d: %d %s, want 429", i, a.status, a.body)
		}
	}
	if n := len(rig.rows.all()); n != 0 {
		t.Errorf("llmlog rows after 1000 refusals = %d, want 0", n)
	}
	if n := u.hits.Load(); n != 0 {
		t.Errorf("wire hits = %d, want 0", n)
	}
	if rig.limiter.rejects.Load() != 1000 {
		t.Errorf("limiter rejects = %d, want 1000 (every refusal ends its reservation)", rig.limiter.rejects.Load())
	}
}

// raRecorder records the Retry-After estimate of every rejection.
type raRecorder struct {
	handler.V1Dispatch
	mu  sync.Mutex
	ras []time.Duration
}

func (r *raRecorder) Acquire(ctx context.Context, req dispatch.Request) (*dispatch.Lease, context.Context, error) {
	l, c, err := r.V1Dispatch.Acquire(ctx, req)
	var re *dispatch.RejectionError
	if errors.As(err, &re) {
		r.mu.Lock()
		r.ras = append(r.ras, re.RetryAfter)
		r.mu.Unlock()
	}
	return l, c, err
}

// TestRetryAfterJitter: 200 concurrent refusals end to end — the
// coefficient of variation of retry-after-ms is ≥ 0.1, and (sorted against
// the sorted estimates of the rejections) none is below the estimate of the
// RejectionError (A03-W8 decision 1: never below the estimate, never from
// RetryAfterHint).
func TestRetryAfterJitter(t *testing.T) {
	u := newUpstream(t, answerSGLang)
	ra := &raRecorder{}
	// A seeded uniform source behind a mutex: the 200 draws are the same
	// multiset whatever the goroutine order, so the gate measures the jitter
	// the writer applies, not the luck of one sample (u ∈ U[1; 1.5] has a
	// coefficient of variation of 0.115; a fresh sample of 200 falls below
	// 0.1 in about one run of 200).
	var jmu sync.Mutex
	src := rand.New(rand.NewPCG(7, 2_07)) //nolint:gosec // test jitter
	jitter := func() float64 { jmu.Lock(); defer jmu.Unlock(); return src.Float64() }
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 2, 1, func(r *chatRig) {
		ra.V1Dispatch = r.disp
		r.planner = handler.NewV1Admission(handler.V1AdmissionConfig{Dispatch: ra, Settings: r.deps.Settings, Jitter: jitter})
		r.deps.Planner = r.planner
		// 200 concurrent requests each reserve 2 × max_response_bytes; the
		// budget must hold them all, or some answer server_overloaded
		// before the planner (the budget's job, not this gate's).
		r.setSettings(func(s *oai.Settings) { s.InflightBudget = 4 << 30 })
	})
	defer occupyExternal(t, rig.disp, u.srv.URL)()
	var mu sync.Mutex
	var ms []float64
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Go(func() {
			a := rig.post(t, chatBody("chat"), "Ctx-Max-Wait-Ms", "0")
			if a.status != http.StatusTooManyRequests {
				t.Errorf("answer %d %s, want 429", a.status, a.body)
				return
			}
			v, err := strconv.ParseFloat(a.header.Get("retry-after-ms"), 64)
			if err != nil || a.header.Get("Retry-After") == "" {
				t.Errorf("headers %v", a.header)
				return
			}
			mu.Lock()
			ms = append(ms, v)
			mu.Unlock()
		})
	}
	wg.Wait()
	if len(ms) != 200 {
		t.Fatalf("%d answers with retry-after-ms, want 200", len(ms))
	}
	var sum, sq float64
	for _, v := range ms {
		sum += v
	}
	mean := sum / float64(len(ms))
	for _, v := range ms {
		sq += (v - mean) * (v - mean)
	}
	cv := math.Sqrt(sq/float64(len(ms))) / mean
	t.Logf("retry-after-ms: n=%d mean=%.1f cv=%.3f", len(ms), mean, cv)
	if cv < 0.1 {
		t.Errorf("coefficient of variation %.3f, want ≥ 0.1", cv)
	}
	ra.mu.Lock()
	ests := slices.Clone(ra.ras)
	ra.mu.Unlock()
	if len(ests) != 200 {
		t.Fatalf("%d rejection estimates, want 200", len(ests))
	}
	slices.Sort(ests)
	slices.Sort(ms)
	for i := range ms {
		if ms[i] < float64(ests[i].Milliseconds()) {
			t.Fatalf("retry-after-ms[%d] = %.0f below the rejection's estimate %v", i, ms[i], ests[i])
		}
	}
}

// TestQueueTimeoutReachesClient: a real server with WriteTimeout 1 s, the
// external wait budget 1 s and the partition full — the client gets the
// JSON 429 with Retry-After after the wait, not a broken connection: the
// route moved the write deadline at entry (B23).
func TestQueueTimeoutReachesClient(t *testing.T) {
	u := newUpstream(t, answerSGLang)
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 2, 1)
	set := dispatch.DefaultSettings()
	set.ExternalMaxWait = time.Second
	rig.disp.UpdateSettings(set)
	rig.deps.MaxWait = func() time.Duration { return time.Second }
	rig.router = rig.mount(oai.ChatCompletions(rig.deps))
	defer occupyExternal(t, rig.disp, u.srv.URL)()
	srv := httptest.NewUnstartedServer(rig.router)
	srv.Config.WriteTimeout = time.Second
	srv.Start()
	t.Cleanup(srv.Close)
	start := time.Now()
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(chatBody("chat")))
	if err != nil {
		t.Fatalf("after %v: the answer did not reach the client: %v", time.Since(start), err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("after %v: body read failed: %v", time.Since(start), err)
	}
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" || !json.Valid(body) {
		t.Fatalf("after %v: %d %q (Retry-After %q), want a JSON 429 with Retry-After", time.Since(start), resp.StatusCode, body, resp.Header.Get("Retry-After"))
	}
	if el := time.Since(start); el < time.Second {
		t.Errorf("answered after %v — the request did not wait out the budget, the gate proves nothing", el)
	}
}

// --- G15 (B12): the watcher ---.

// TestWatcherKillSwitchEndsRequest: gateway.enabled hot to false while the
// upstream runs ⇒ the upstream is aborted within a tick plus one second,
// the client gets the gate's 404 (never a 200), and nothing stays held.
func TestWatcherKillSwitchEndsRequest(t *testing.T) {
	defer oai.SetWatchEvery(100 * time.Millisecond)()
	u := newUpstream(t, answerSGLang)
	b := newBlocker()
	defer b.open()
	u.setRespond(b.respond(answerSGLang))
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
	done := make(chan chatAnswer, 1)
	go func() { done <- rig.post(t, chatBody("chat")) }()
	waitEntered(t, b, 1)
	flipped := time.Now()
	rig.setSettings(func(s *oai.Settings) { s.Enabled = false })
	select {
	case at := <-b.aborted:
		if el := at.Sub(flipped); el > 100*time.Millisecond+time.Second {
			t.Errorf("upstream aborted %v after the switch, want ≤ tick + 1 s", el)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the upstream was not aborted within 3 s of the switch")
	}
	a := <-done
	code, _, _ := a.envelope(t)
	if a.status != http.StatusNotFound || code != oai.CodeNotFound {
		t.Fatalf("answer %d %s, want the gate's 404 not_found", a.status, a.body)
	}
	if held, _ := heldOn(rig.disp, u.srv.URL); held != 0 {
		t.Errorf("held = %d after the abort, want 0", held)
	}
}

// TestWatcherAliasGoneEndsRequest: the alias disabled (registry reload) or
// the grant withdrawn while the upstream runs ⇒ 404 model_not_found, never
// the 200 of the finished upstream. The grant has no generation of its own:
// the check right before the write catches it (D8).
func TestWatcherAliasGoneEndsRequest(t *testing.T) {
	defer oai.SetWatchEvery(100 * time.Millisecond)()
	for _, tc := range []struct {
		name   string
		revoke func(*chatRig)
	}{
		{"alias disabled", func(r *chatRig) {
			rows := chatAliases()
			rows[0].enabled = false
			r.reg.reload(r.t, rows)
		}},
		{"grant withdrawn", func(r *chatRig) { r.setGrant(idChat, false) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := newUpstream(t, answerSGLang)
			b := newBlocker()
			defer b.open()
			u.setRespond(b.respond(answerSGLang))
			rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
			ca := urgentApp()
			ca.AllModels = false
			rig.ca.Store(ca)
			rig.setGrant(idChat, true)
			done := make(chan chatAnswer, 1)
			go func() { done <- rig.post(t, chatBody("chat")) }()
			waitEntered(t, b, 1)
			tc.revoke(rig)
			time.Sleep(300 * time.Millisecond)
			b.open()
			a := <-done
			code, _, _ := a.envelope(t)
			if a.status != http.StatusNotFound || code != oai.CodeModelNotFound {
				t.Fatalf("answer %d %s, want 404 model_not_found", a.status, a.body)
			}
		})
	}
}

// TestWatcherLeavesNoGoroutine: 100 requests later the goroutine count is
// back where it started (±0 after settling) — one watcher per request, gone
// with it.
func TestWatcherLeavesNoGoroutine(t *testing.T) {
	u := newUpstream(t, answerSGLang)
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
	_ = rig.post(t, chatBody("chat")) // warm the transport's idle connection
	settle := func() int {
		n := runtime.NumGoroutine()
		for i := 0; i < 50; i++ {
			time.Sleep(10 * time.Millisecond)
			if m := runtime.NumGoroutine(); m == n {
				return n
			} else {
				n = m
			}
		}
		return n
	}
	before := settle()
	for i := 0; i < 100; i++ {
		if a := rig.post(t, chatBody("chat")); a.status != http.StatusOK {
			t.Fatalf("request %d: %d %s", i, a.status, a.body)
		}
	}
	after := settle()
	t.Logf("goroutines before %d, after 100 requests %d", before, after)
	if after != before {
		t.Errorf("goroutines %d → %d after 100 requests, want ±0", before, after)
	}
}

// TestWatchTickIsTheDesignValue pins the period (design/02 §4.11.3 D8,
// lead decision O2 (a)).
func TestWatchTickIsTheDesignValue(t *testing.T) {
	if oai.WatchTick != 5*time.Second {
		t.Errorf("watch tick %v, want 5s (design/02 §4.11.3 D8)", oai.WatchTick)
	}
}

// --- G16: the admission census over the real handler ---.

// TestChatAdmissionCensus: v1census over the route with the real planner on
// the recorder (A03-W8 handover, review 1 F-6): the chat route is the
// admitting route — at least one acquire, classes external, origins = row
// hosts — and every blocking acquire carries ServeBy ≠ 0: the request
// deadline is on the context before the planner (lead decision 5).
func TestChatAdmissionCensus(t *testing.T) {
	u := newUpstream(t, answerSGLang)
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
	v1census.Assert(t, rig.router, rig.recorder, v1census.Options{
		Allowed:   rig.planner.AllowedClasses(),
		Hosts:     []string{u.srv.URL},
		Admitting: []string{"POST /v1/chat/completions"},
		Body:      func(string, string) string { return chatBody("chat") },
	})
	sb := rig.serveBy.all()
	if len(sb) == 0 {
		t.Fatal("the census made no blocking acquire")
	}
	for i, at := range sb {
		if at.IsZero() {
			t.Errorf("acquire %d without ServeBy — the request deadline is not on the context before the planner", i)
		}
	}
}

// TestChatAcquireEqualsWire: Σacquire == Σwire over the real handler for
// success, failover (502 → next), a misconfigured row (404 → next), a
// window-rule skip and an exhausted chain — one lease per physical contact,
// no contact without a lease (I-D1 over the route, X13).
func TestChatAcquireEqualsWire(t *testing.T) {
	ok := newUpstream(t, answerSGLang)
	bad := newUpstream(t, answerSGLang)
	small := newUpstream(t, answerLlamaCpp)
	status := func(code int, body string) func(w http.ResponseWriter, r *http.Request) {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
			_, _ = io.WriteString(w, body)
		}
	}
	for _, tc := range []struct {
		name     string
		badReply func(w http.ResponseWriter, r *http.Request)
		rows     func() []backends.Backend
		want     int64
	}{
		{"success", nil, func() []backends.Backend { return []backends.Backend{ok.row("ok", backends.TrustFull, 10)} }, 1},
		{"failover 502", status(502, "bad gateway"), func() []backends.Backend {
			return []backends.Backend{bad.row("bad", backends.TrustFull, 20), ok.row("ok", backends.TrustFull, 10)}
		}, 2},
		{"404 next", status(404, "no such model"), func() []backends.Backend {
			return []backends.Backend{bad.row("bad", backends.TrustFull, 20), ok.row("ok", backends.TrustFull, 10)}
		}, 2},
		{"window skip", status(400, "prompt too long for this row"), func() []backends.Backend {
			b1, b2 := bad.row("bad", backends.TrustFull, 20), small.row("small", backends.TrustFull, 10)
			b1.NumCtx, b2.NumCtx = 32768, 8192
			return []backends.Backend{b1, b2}
		}, 1},
		{"exhausted", status(503, "busy"), func() []backends.Backend {
			b2 := bad.row("bad2", backends.TrustFull, 10)
			b2.ID = "id-bad2"
			return []backends.Backend{bad.row("bad", backends.TrustFull, 20), b2}
		}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad.setRespond(tc.badReply)
			rows := tc.rows()
			rig := newChatRig(t, rows, 4, 1)
			hosts := []*upstream{ok, bad, small}
			var before int64
			for _, h := range hosts {
				before += h.hits.Load()
			}
			rig.recorder.Take()
			_ = rig.post(t, chatBody("chat"))
			var wire int64
			for _, h := range hosts {
				wire += h.hits.Load()
			}
			wire -= before
			var acq int64
			for _, e := range rig.recorder.Take() {
				if e.Kind == v1census.KindAcquire || e.Kind == v1census.KindTry {
					acq++
				}
			}
			if acq != wire || wire != tc.want {
				t.Errorf("acquires %d, wire contacts %d, want %d/%d", acq, wire, tc.want, tc.want)
			}
		})
	}
}

// --- G11 (grammar pin of the upstream id) ---.

// TestAttemptRequestIDGrammar: the upstream id of an attempt is inside the
// grammar fedhop.validRequestID (internal/fedhop/grammar.go) and the house
// RequestID middleware (internal/handler/middleware.go isValidRequestID)
// both define — [0-9A-Fa-f-]{1,64} — for every attempt number a walk can
// reach (FR2-4); a "chatcmpl-" prefix would not be (red control).
func TestAttemptRequestIDGrammar(t *testing.T) {
	grammar := func(s string) bool {
		if len(s) == 0 || len(s) > 64 {
			return false
		}
		return strings.Trim(s, "0123456789abcdefABCDEF-") == ""
	}
	id := "01a1aa79-5fe9-7c1c-9e87-ff2af1e1507b"
	for _, n := range []int{1, 2, 9, 10, 99999} {
		if got := oai.AttemptRequestID(id, n); !grammar(got) {
			t.Errorf("attempt %d: %q is outside the request-id grammar", n, got)
		}
	}
	if grammar("chatcmpl-" + id) {
		t.Error("red control: the answer id form passes the grammar — the pin proves nothing")
	}
	if got := oai.AttemptRequestID(id, 3); got != id+"-a3" {
		t.Errorf("attempt id %q, want %q (design/02 §4.5.3)", got, id+"-a3")
	}
}
