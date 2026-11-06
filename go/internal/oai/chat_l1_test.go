package oai_test

// L1 of the per-consumer meter at the chat route (wave W06-11, design/06
// §4.1 step 9, §4.2 row "L1 Concurrency", §4.4 row concurrency, B-06-16,
// B-06-27) over the real meter (rigMeter, the stand-in of cmd/ctxd's
// meterLimiter): a request beyond C open requests of its consumer and op is
// 429 concurrency_limit_exceeded with Retry-After 1 s, before any acquire;
// the bulk lane keeps the urgent lane's slot free; no exit of the route —
// panic and client cancel included — leaks a slot or a reservation.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/keyquota"
	"github.com/GottZ/ctx/internal/oai"
)

// noL1 keeps L1 out of the way of a test about L0: every reservation fits.
const noL1 = math.MaxInt64

// heldUpstream answers every request with the engine's shape once release is
// closed; arrived gets one value per request that reached it.
func heldUpstream(t *testing.T) (u *upstream, arrived chan struct{}, release chan struct{}) {
	t.Helper()
	u = newUpstream(t, answerSGLang)
	arrived, release = make(chan struct{}, 64), make(chan struct{})
	u.setRespond(func(w http.ResponseWriter, _ *http.Request) {
		arrived <- struct{}{}
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, answerSGLang)
	})
	return u, arrived, release
}

func waitArrived(t *testing.T, arrived chan struct{}, what string) {
	t.Helper()
	select {
	case <-arrived:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s never reached the backend", what)
	}
}

// postRefused sends body and waits at most 5 s for the answer: a request L1
// refuses must not wait for the held backend (under a mutant that admits it,
// the gate fails at once instead of waiting for the row's timeout).
func postRefused(t *testing.T, rig *chatRig, body string) chatAnswer {
	t.Helper()
	ch := make(chan chatAnswer, 1)
	go func() { ch <- rig.post(t, body) }()
	select {
	case a := <-ch:
		return a
	case <-time.After(5 * time.Second):
		t.Errorf("no answer within 5 s — the request was not refused at once, it waits at the held backend")
		return chatAnswer{header: http.Header{}, body: []byte(`{"error":{}}`)}
	}
}

// l1Refusal checks a 429 concurrency_limit_exceeded: rate_limit_error,
// Retry-After 1, retry-after-ms 1000, x-should-retry true, the message, no
// x-ratelimit-* header.
func l1Refusal(t *testing.T, what string, a chatAnswer, message string) {
	t.Helper()
	code, param, typ := a.envelope(t)
	var env struct {
		Error struct{ Message string } `json:"error"`
	}
	_ = json.Unmarshal(a.body, &env)
	if a.status != http.StatusTooManyRequests || code != oai.CodeConcurrencyLimitExceeded || typ != "rate_limit_error" || param != "" ||
		env.Error.Message != message {
		t.Errorf("%s: %d %s, want 429 rate_limit_error concurrency_limit_exceeded %q", what, a.status, a.body, message)
	}
	if ra, ms, sr := a.header.Get("Retry-After"), a.header.Get("retry-after-ms"), a.header.Get("x-should-retry"); ra != "1" || ms != "1000" || sr != "true" {
		t.Errorf("%s: Retry-After %q, retry-after-ms %q, x-should-retry %q — want 1, 1000, true", what, ra, ms, sr)
	}
	for k := range a.header {
		if strings.HasPrefix(strings.ToLower(k), "x-ratelimit-") {
			t.Errorf("%s: carries %s, want no x-ratelimit-* on a refusal", what, k)
		}
	}
}

// TestL1ConcurrencyOneAtTheRoute is the gate "max_concurrency = 1": while one
// request of the consumer is open (held at the backend), the second is 429
// concurrency_limit_exceeded with Retry-After 1 — refused before the
// dispatcher (no acquire, A03-W8 handover "L1-Ablehnung ohne Acquire"); once
// the first ended, the next is served.
func TestL1ConcurrencyOneAtTheRoute(t *testing.T) {
	u, arrived, release := heldUpstream(t)
	meter := newRigMeter(keyquota.Limits{RPM: 100, TPM: 1 << 30, CompletionReserve: 1024, XUReserve: 0.25, Concurrency: 1}, true)
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withDeps(func(d *oai.Deps) { d.Limiter = meter }))
	first := make(chan chatAnswer, 1)
	go func() { first <- rig.post(t, chatBody("chat")) }()
	waitArrived(t, arrived, "the first request")
	acquires := len(rig.serveBy.all())
	second := postRefused(t, rig, chatBody("chat"))
	l1Refusal(t, "second, parallel", second, "Concurrency limit reached for requests (chat): 1 at a time.")
	if n := len(rig.serveBy.all()); n != acquires {
		t.Errorf("acquires %d after the L1 refusal, %d before — the refused request reached the dispatcher", n, acquires)
	}
	close(release)
	if a := <-first; a.status != http.StatusOK {
		t.Fatalf("first: %d %s, want 200", a.status, a.body)
	}
	if a := rig.post(t, chatBody("chat")); a.status != http.StatusOK {
		t.Errorf("third, after the first ended: %d %s, want 200", a.status, a.body)
	}
	if n := u.hits.Load(); n != 2 {
		t.Errorf("backend hits %d, want 2 (the refused request never reached it)", n)
	}
}

// TestL1LaneAtTheRoute is B-06-27 for L1 at the route: C = 2, r = 0.25 —
// of two bulk requests (service_tier flex) one runs and the other is 429
// (the bulk lane's share, 1); an urgent request of the same consumer then
// passes L1 and reaches L2 (the dispatcher counts its acquire), no 429.
func TestL1LaneAtTheRoute(t *testing.T) {
	u, arrived, release := heldUpstream(t)
	meter := newRigMeter(keyquota.Limits{RPM: 100, TPM: 1 << 30, CompletionReserve: 1024, XUReserve: 0.25, Concurrency: 2}, true)
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withDeps(func(d *oai.Deps) { d.Limiter = meter }))
	flex := chatBody("chat", `"service_tier":"flex"`)
	answers := make(chan chatAnswer, 2)
	go func() { answers <- rig.post(t, flex) }()
	waitArrived(t, arrived, "the first bulk request")
	acquires := len(rig.serveBy.all())
	second := postRefused(t, rig, flex)
	l1Refusal(t, "second bulk", second, "Concurrency limit reached for requests (chat, bulk lane): 1 at a time.")
	if n := len(rig.serveBy.all()); n != acquires {
		t.Errorf("acquires %d after the bulk L1 refusal, %d before", n, acquires)
	}
	go func() { answers <- rig.post(t, chatBody("chat")) }()
	waitArrived(t, arrived, "the urgent request")
	if n := len(rig.serveBy.all()); n != acquires+1 {
		t.Errorf("acquires %d after the urgent request, want %d (it reached L2)", n, acquires+1)
	}
	close(release)
	for range 2 {
		if a := <-answers; a.status != http.StatusOK {
			t.Errorf("bulk or urgent request: %d %s, want 200", a.status, a.body)
		}
	}
}

// The exits of the leak gate.
const (
	l1Success    = iota // Settle with the engine's usage
	l1Upstream          // the engine answers 400: Keep (wire contact, no usage)
	l1Refuse            // the planner refuses: Reject
	l1Panic             // the planner panics after Admit: close() aborts
	l1CancelPlan        // the client goes away while Plan runs: Abort
	l1CancelWire        // the client goes away while the engine holds the request: Keep
	l1Modes
)

// l1Run is one request of the leak gate: its exit and the client's cancel.
type l1Run struct {
	mode   int
	cancel context.CancelFunc
}

type l1RunKey struct{}

// l1Planner wraps the rig's real planner with the exits of the leak gate.
type l1Planner struct{ oai.AdmissionPlanner }

func (p l1Planner) Plan(ctx context.Context, ca *auth.ConsumerAuth, in oai.AdmissionInput) (oai.Plan, error) {
	if run, ok := ctx.Value(l1RunKey{}).(*l1Run); ok {
		switch run.mode {
		case l1Refuse:
			return oai.Plan{}, handler.ErrChainNotOffered
		case l1Panic:
			panic("w06-11: the planner panics after the limiter admitted")
		case l1CancelPlan:
			run.cancel()
		}
	}
	return p.AdmissionPlanner.Plan(ctx, ca, in)
}

// TestL1LeaksNothing is the leak gate (B-06-16; run it under -race): 10 000
// requests of four consumers, eight at a time, each with a random exit —
// success, an engine error, a planner refusal, a panic after Admit, the
// client gone during Plan or during the backend call — and every one of them
// either refused by L1 (C = 2) or admitted. Afterwards every consumer has
// all C slots free (C admissions pass, one more is refused by L1), its token
// bucket holds exactly the settled usage plus the kept reservations, its
// request bucket exactly the admitted requests, and the process runs no
// more goroutines than before.
func TestL1LeaksNothing(t *testing.T) {
	const (
		requests = 10000
		workers  = 8
		c        = 2
	)
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.DiscardHandler)) // ≈ 1 700 recovered panics would log their stacks
	t.Cleanup(func() { slog.SetDefault(prev) })

	// The engine reads the request's exit off its content (the shared
	// upstream fake reads the body before its respond hook).
	runs := sync.Map{} // content → *l1Run
	var hits atomic.Int64
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		var body struct {
			Messages []struct{ Content string } `json:"messages"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		var run *l1Run
		if len(body.Messages) == 1 {
			if v, ok := runs.Load(body.Messages[0].Content); ok {
				run = v.(*l1Run)
			}
		}
		switch {
		case run != nil && run.mode == l1Upstream:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"engine refuses"}}`)
			return
		case run != nil && run.mode == l1CancelWire:
			run.cancel()
			select {
			case <-r.Context().Done():
			case <-time.After(10 * time.Second):
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, answerSGLang)
	}))
	t.Cleanup(engine.Close)
	u := &upstream{srv: engine} // for its row
	meter := newRigMeter(keyquota.Limits{RPM: 1 << 20, TPM: 60_000_000, CompletionReserve: 1024, XUReserve: 0.25, Concurrency: c}, true)
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 64, 1, withDeps(func(d *oai.Deps) {
		d.Planner = l1Planner{d.Planner}
		d.Limiter = meter
	}))
	consumers := make([]*auth.ConsumerAuth, 4)
	for i := range consumers {
		ca := urgentApp()
		ca.ConsumerID = fmt.Sprintf("leak-%d", i)
		consumers[i] = ca
	}
	rig.ca.Store(nil) // each request carries its own consumer
	send := func(ca *auth.ConsumerAuth, content string, run *l1Run) chatAnswer {
		ctx, cancel := context.WithCancel(context.WithValue(context.Background(), consumerKey{}, ca))
		defer cancel()
		if run != nil {
			run.cancel = cancel
			ctx = context.WithValue(ctx, l1RunKey{}, run)
		}
		body := fmt.Sprintf(`{"model":"chat","messages":[{"role":"user","content":%q}]}`, content)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		func() {
			defer func() { _ = recover() }() // a panic past the rig's recovery would end the test
			rig.router.ServeHTTP(w, req)
		}()
		return chatAnswer{status: w.Code, header: w.Header(), body: w.Body.Bytes()}
	}

	// The reservation of one request (its prompt estimate plus the reserve):
	// a calibration consumer keeps one on an engine error.
	calib := urgentApp()
	calib.ConsumerID = "leak-calibration"
	calibRun := &l1Run{mode: l1Upstream}
	runs.Store("x000000", calibRun)
	if a := send(calib, "x000000", calibRun); a.status == http.StatusOK || a.status == http.StatusTooManyRequests {
		t.Fatalf("calibration: %d %s, want the engine's refusal", a.status, a.body)
	}
	reservation := meter.lim.TPM - meter.probe(t, calib, keyquota.OpChat).RemainingTokens

	// One request starts what runs lazily; then the engine drops every
	// connection (idle keep-alive connections are not leaks) and the count
	// settles.
	if a := send(consumers[0], "warm", nil); a.status != http.StatusOK {
		t.Fatalf("warm-up: %d %s", a.status, a.body)
	}
	before := settledGoroutines(engine)
	warmed := int64(1) // the warm-up request of consumers[0]
	type tally struct{ admitted, refused, tokens int64 }
	tallies := make([]tally, len(consumers))
	var mu sync.Mutex
	modes := make([]int64, l1Modes)
	var next atomic.Int64
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			rng := rand.New(rand.NewPCG(uint64(w), 611))
			for {
				id := next.Add(1)
				if id > requests {
					return
				}
				ci := rng.IntN(len(consumers))
				run := &l1Run{mode: rng.IntN(l1Modes)}
				content := fmt.Sprintf("q%06d", id)
				runs.Store(content, run)
				a := send(consumers[ci], content, run)
				runs.Delete(content)
				code := ""
				if a.status == http.StatusTooManyRequests {
					code, _, _ = a.envelope(t)
				}
				mu.Lock()
				switch {
				case code == oai.CodeConcurrencyLimitExceeded:
					tallies[ci].refused++
				case code != "":
					t.Errorf("request %d (mode %d): 429 %s, want only L1 refusals", id, run.mode, a.body)
				default:
					tallies[ci].admitted++
					modes[run.mode]++
					switch run.mode {
					case l1Success:
						if a.status != http.StatusOK {
							t.Errorf("request %d (success): %d %s", id, a.status, a.body)
						}
						tallies[ci].tokens += 11 + 3
					case l1Upstream, l1CancelWire:
						tallies[ci].tokens += reservation
					}
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()

	refused := int64(0)
	for i, ca := range consumers {
		tl := tallies[i]
		refused += tl.refused
		if i == 0 {
			tl.tokens += 11 + 3 // the warm-up request
		}
		var probes []*keyquota.Reservation
		var last keyquota.Decision
		for range c {
			r, d := meter.m.Admit(keyquota.Subject{ConsumerID: ca.ConsumerID, TenantID: ca.TenantID, Op: keyquota.OpChat}, meter.lim, keyquota.Estimate{ClientMax: true})
			if !d.Allowed {
				t.Errorf("%s: a probe of its C = %d slots refused (%s) — a slot leaked; %+v", ca.ConsumerID, c, d.Reason, tl)
				break
			}
			probes, last = append(probes, r), d
		}
		if _, d := meter.m.Admit(keyquota.Subject{ConsumerID: ca.ConsumerID, TenantID: ca.TenantID, Op: keyquota.OpChat}, meter.lim, keyquota.Estimate{ClientMax: true}); d.Reason != keyquota.ReasonConcurrency {
			t.Errorf("%s: probe C+1: %+v, want refused by L1", ca.ConsumerID, d)
		}
		if len(probes) == c {
			if got := meter.lim.TPM - last.Headers.RemainingTokens; got != tl.tokens {
				t.Errorf("%s: token bucket holds %d, want exactly the settled usage plus the kept reservations %d (%+v)", ca.ConsumerID, got, tl.tokens, tl)
			}
			if i == 0 {
				tl.admitted += warmed
			}
			if got := meter.lim.RPM - last.Headers.RemainingRequests; got != tl.admitted+c {
				t.Errorf("%s: request bucket holds %d, want the %d admitted plus %d probes", ca.ConsumerID, got, tl.admitted, c)
			}
		}
		for _, r := range probes {
			r.Abort()
		}
	}
	after := settledGoroutines(engine)
	t.Logf("%d requests: admitted by exit (success, engine error, refuse, panic, cancel in plan, cancel on the wire) %v, refused by L1 %d; reservation %d tokens; goroutines before %d, after %d",
		requests, modes, refused, reservation, before, after)
	if after > before {
		buf := make([]byte, 1<<20)
		t.Errorf("goroutines %d after the run, %d before:\n%s", after, before, buf[:runtime.Stack(buf, true)])
	}
	if refused == 0 || modes[l1Panic] == 0 || modes[l1CancelWire] == 0 {
		t.Errorf("the run did not exercise every exit and L1: refused %d, modes %v", refused, modes)
	}
}

// settledGoroutines closes every connection the engine holds and returns the
// goroutine count once it has not changed for 100 ms (at most 10 s).
func settledGoroutines(engine *httptest.Server) int {
	engine.CloseClientConnections()
	n, stable := runtime.NumGoroutine(), 0
	for deadline := time.Now().Add(10 * time.Second); stable < 5 && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
		if m := runtime.NumGoroutine(); m == n {
			stable++
		} else {
			n, stable = m, 0
		}
	}
	return n
}
