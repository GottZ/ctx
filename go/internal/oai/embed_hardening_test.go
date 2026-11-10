package oai_test

// Gates of the hardening after Fremd-Review 2 (W02-09): the decode transient
// in the reserve (H-3), the named limit of two overlapping multi-block
// requests (H-4), the pins of the defence branches of the route (H-5: R02,
// R03, P01, P03, S03 over the route) and the early rune limit (H-7).

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/oai"
)

// TestEmbedDecodeTransientIsReservedBeforeTheWire (H-3): the bound of a
// block's decode buffer is reserved before its wire call — a budget that
// holds the request and its answer but not the transient is 503
// server_overloaded with no wire contact and no row; the same request under
// an ample budget is served.
func TestEmbedDecodeTransientIsReservedBeforeTheWire(t *testing.T) {
	e := newEmbedEngine(t, 4096)
	rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 33, 1)
	body := embedBody("emb4k", embedInputs(32))
	answer := int64(32*4096*oai.EmbedFloatBytes + oai.EmbedEncodeBuffer)
	rig.setSettings(func(s *oai.Settings) { s.InflightBudget = 3*int64(len(body)) + answer + 1<<20 })
	a := rig.postEmbed(t, body)
	code, _, _ := a.envelope(t)
	if a.status != http.StatusServiceUnavailable || code != oai.CodeServerOverloaded {
		t.Fatalf("%d %.200s, want 503 server_overloaded (the transient of a 32 × 4096 block does not fit)", a.status, a.body)
	}
	if e.hits.Load() != 0 || len(rig.rows.all()) != 0 {
		t.Errorf("wire hits %d rows %d, want 0/0", e.hits.Load(), len(rig.rows.all()))
	}
	if l := rig.limiter; l.settles.Load() != 0 || l.aborts.Load() != 1 {
		t.Errorf("limiter settles %d aborts %d, want 0/1", l.settles.Load(), l.aborts.Load())
	}
	rig.setSettings(func(s *oai.Settings) { s.InflightBudget = 256 << 20 })
	decodeEmbed(t, rig.postEmbed(t, body))
}

// TestEmbedHostileAnswersStayInTheReserves (H-3, Fremd-Review 2 R2-9): 32
// requests at once, each block answered with ≈ 16 MiB (the old transport cap)
// by a hostile row — every read stops at the block's cap (1 input × 8
// components), the answer is the oversized-answer doctrine of the pool
// (httpx.ErrResponseTooLarge: a malformed answer, 500 upstream_error,
// server_fault — stop the chain), and the live heap stays under the sum of
// the reserves plus a constant. Red: VmHWM 1.85 GB (Review 2), here the
// HeapInuse peak in the hundreds of MiB.
func TestEmbedHostileAnswersStayInTheReserves(t *testing.T) {
	const n = 32
	defer debug.SetMemoryLimit(debug.SetMemoryLimit(parseMemGoMemLimit))
	huge := []byte(`{"object":"list","data":[{"object":"embedding","index":0,"embedding":[` +
		strings.TrimSuffix(strings.Repeat("0.123456789,", (16<<20)/12), ",") + `]}]}`)
	e := newEmbedEngine(t, 8)
	e.setRespond(func(w http.ResponseWriter, _ *http.Request, _ int64, _ []string) bool {
		_, _ = w.Write(huge)
		return true
	})
	rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, n+1, 1, withoutCensus)
	rig.setSettings(func(s *oai.Settings) { s.InflightBudget = parseMemBudget })
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	base := m.HeapInuse
	var peak atomic.Uint64
	stop, sampled := make(chan struct{}), make(chan struct{})
	go heapPeak(stop, &peak, sampled)
	answers := make([]chatAnswer, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			answers[i] = rig.postEmbed(t, embedBody("embsmall", []string{fmt.Sprintf("hostile %d", i)}))
		}()
	}
	wg.Wait()
	close(stop)
	<-sampled
	over := int64(peak.Load()) - int64(base)
	// Σ reserves: n × (3 × body + 8 × 4 B + 64 KiB + the transient of a
	// one-input block) — under 4 MiB here; the constant covers the harness.
	const bound = 4<<20 + parseMemSlack
	t.Logf("%d requests against ≈ 16 MiB answers: HeapInuse peak %d MiB over a base of %d MiB (bound %d MiB)", n, over>>20, base>>20, bound>>20)
	for i, a := range answers {
		if code, _, _ := a.envelope(t); a.status != http.StatusInternalServerError || code != oai.CodeUpstreamError {
			t.Fatalf("request %d: %d %s, want 500 upstream_error (an oversized answer)", i, a.status, code)
		}
	}
	if over > bound {
		t.Errorf("HeapInuse peak %d MiB over the base, want ≤ Σ reserves (< 4 MiB) + %d MiB", over>>20, parseMemSlack>>20)
	}
	rows := rig.rows.all()
	if len(rows) != n {
		t.Fatalf("rows %d, want %d", len(rows), n)
	}
	if rows[0].Err == nil || rows[0].Err.Class != backends.ClassServerFault.String() {
		t.Errorf("first row err %+v, want err class server_fault", rows[0].Err)
	}
}

// discardAnswer is a ResponseWriter that counts the bytes of a 200 instead
// of holding them (the vectors of a near-cap answer would be test heap
// outside every reserve) and keeps the body of any other status.
type discardAnswer struct {
	h    http.Header
	code int
	n    int64
	body bytes.Buffer
}

func (d *discardAnswer) Header() http.Header { return d.h }
func (d *discardAnswer) WriteHeader(code int) {
	if d.code == 0 {
		d.code = code
	}
}

func (d *discardAnswer) Write(p []byte) (int, error) {
	d.WriteHeader(http.StatusOK)
	d.n += int64(len(p))
	if d.code != http.StatusOK {
		d.body.Write(p)
	}
	return len(p), nil
}

func (d *discardAnswer) Flush() {}

// TestEmbedNearCapAnswersStayInTheReserves (H-3, the decode factor): an
// alias without embedding.native_dims (read cap 16 MiB), a row whose
// healthy answer to one input is float64 literals of two sizes — the
// largest the wire accepts, and one just past a growth step of
// json.Decoder's buffer (2 × cap + 512 B: an answer of 8 MiB + 64 KiB holds
// 16 MiB beside the old 8 MiB, three times the answer — the case
// embedDecodeFactor is sized for). One request at a time: the live heap
// stays under its own reserve plus nearCapSlack (the transient is in the
// reserve, not beside it). 32 at once under the default budget and the
// container's GOMEMLIMIT: each admitted request reserves embedDecodeFactor
// × the read cap before its wire call, the rest are 503 server_overloaded
// before any wire contact, and the live heap stays under the peak of the
// reserves plus parseMemSlack. Red before H-3: every request is admitted
// with the answer reserve alone (4 MiB). Under the race detector the
// shortened form of nearCapForms.
func TestEmbedNearCapAnswersStayInTheReserves(t *testing.T) {
	defer debug.SetMemoryLimit(debug.SetMemoryLimit(parseMemGoMemLimit))
	for _, tc := range nearCapForms() {
		t.Run(tc.name, func(t *testing.T) { nearCapAnswers(t, tc.bytes, tc.n, tc.slack) })
	}
}

// The answer sizes of TestEmbedNearCapAnswersStayInTheReserves: the
// largest the wire accepts, one just past a growth step of json.Decoder's
// buffer, and the race build's (nearCapForms).
const nearCapLargest, nearCapPastAStep, nearCapRaceBytes = 31 << 19, 8<<20 + 64<<10, 1 << 20

// nearCapForm is one case of TestEmbedNearCapAnswersStayInTheReserves.
type nearCapForm struct {
	name     string
	bytes, n int
	slack    int64
}

// nearCapForms are the cases of TestEmbedNearCapAnswersStayInTheReserves:
// both near-cap answers, one request and 32 at once. Under the race
// detector (W02-09 hardening 7) one case: 32 at once against a 1-MiB
// answer. The admission under contention — each admitted request reserves
// embedDecodeFactor × the alias's 16-MiB read cap before its wire call,
// the rest are 503 server_overloaded before any wire contact — does not
// depend on the answer's size and stays race-checked; the near-cap sizes
// and the one-request heap bounds measure the decode, which the detector
// weighs (44.9 s of the CI unit job's package timeout under -race,
// GOMAXPROCS=2), and run in every build without -race.
func nearCapForms() []nearCapForm {
	if raceEnabled {
		return []nearCapForm{{"a 1-MiB answer, 32 at once", nearCapRaceBytes, 32, parseMemSlack}}
	}
	return []nearCapForm{
		{"largest answer under the cap, one request", nearCapLargest, 1, nearCapSlack},
		{"just past a buffer growth step, one request", nearCapPastAStep, 1, nearCapSlack},
		{"largest answer under the cap, 32 at once", nearCapLargest, 32, parseMemSlack},
		{"just past a buffer growth step, 32 at once", nearCapPastAStep, 32, parseMemSlack},
	}
}

// nearCapSlack is the live-heap allowance over one request's reserve: the
// harness's share (sampler, the engine's copy of the request, headers) —
// far below the decode transient it must not hide.
const nearCapSlack = 4 << 20

func nearCapAnswers(t *testing.T, size, n int, slack int64) {
	const lit = "0.012345678901234567" // 20 bytes + the comma: a float64 in shortest form
	comps := size / (len(lit) + 1)
	answer := []byte(`{"object":"list","data":[{"object":"embedding","index":0,"embedding":[` +
		strings.TrimSuffix(strings.Repeat(lit+",", comps), ",") + `]}],"model":"m","usage":{"prompt_tokens":1,"total_tokens":1}}`)
	e := newEmbedEngine(t, 8)
	e.setRespond(func(w http.ResponseWriter, _ *http.Request, _ int64, _ []string) bool {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(answer)
		return true
	})
	rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, n+1, 1, withoutCensus)
	rig.setSettings(func(s *oai.Settings) { s.InflightBudget = parseMemBudget })
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	base := m.HeapInuse
	var peak atomic.Uint64
	var inUse atomic.Int64
	stop, sampled, budgetDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go heapPeak(stop, &peak, sampled)
	go func() {
		defer close(budgetDone)
		for {
			if u := rig.budget.InUse(); u > inUse.Load() {
				inUse.Store(u)
			}
			select {
			case <-stop:
				return
			case <-time.After(200 * time.Microsecond):
			}
		}
	}()
	answers := make([]*discardAnswer, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(embedBody("emb", []string{fmt.Sprintf("near cap %d", i)})))
			req.Header.Set("Content-Type", "application/json")
			d := &discardAnswer{h: http.Header{}}
			rig.router.ServeHTTP(d, req)
			answers[i] = d
		}()
	}
	wg.Wait()
	close(stop)
	<-sampled
	<-budgetDone
	over := int64(peak.Load()) - int64(base)
	bound := inUse.Load() + slack
	served, refused := 0, 0
	for i, a := range answers {
		if a.code == http.StatusOK {
			served++
			continue
		}
		if code, _, _ := (chatAnswer{status: a.code, header: a.h, body: a.body.Bytes()}).envelope(t); a.code != http.StatusServiceUnavailable || code != oai.CodeServerOverloaded {
			t.Fatalf("request %d: %d %s, want 200 or 503 server_overloaded", i, a.code, a.body.Bytes())
		}
		refused++
	}
	t.Logf("%d requests against %d-component answers of %d B: %d served, %d refused, %d wire calls; HeapInuse peak %d MiB over a base of %d MiB, reserves peak %d MiB (bound %d MiB)",
		n, comps, len(answer), served, refused, e.hits.Load(), over>>20, base>>20, inUse.Load()>>20, bound>>20)
	if served == 0 || int64(served) != e.hits.Load() {
		t.Errorf("served %d, wire calls %d: want at least one, and no wire call for a refused request", served, e.hits.Load())
	}
	if over > bound {
		t.Errorf("HeapInuse peak %d MiB over the base, want ≤ the reserves' peak %d MiB + %d MiB", over>>20, inUse.Load()>>20, slack>>20)
	}
}

// TestEmbedTwoMultiBlockRequestsLockOutOthersNamedLimit (H-4, Fremd-Review
// 2 R2-11) pins a named limit, not a goal: the continuation of a multi-block
// request waits at the head of its class, ahead of every other waiter
// (A03-W17), and the route makes up to ⌈max_inputs / E⌉ continuations
// (O-09-1 (b)). One multi-block request lets a waiting third consumer
// through at its first lease boundary; two overlapping ones hand the unit to
// each other at every boundary, and the third consumer gets none until the
// first of them has ended. The decision (keep, turn O-09-1 back, or a
// dispatcher wave) is the user's.
func TestEmbedTwoMultiBlockRequestsLockOutOthersNamedLimit(t *testing.T) {
	run := func(t *testing.T, overlapping bool) []string {
		e := newEmbedEngine(t, 8)
		rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 2, 1, withoutCensus)
		host := e.srv.URL
		var mu sync.Mutex
		var order []string
		var wg sync.WaitGroup
		post := func(tag string, inputs []string) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if a := rig.postEmbed(t, embedBody("emb1", inputs), foreignTenantHeader, tag); a.status != http.StatusOK {
					t.Errorf("%s: %d %s", tag, a.status, a.body)
				}
			}()
		}
		waiting := func() int {
			for _, ts := range rig.disp.Snapshot().Targets {
				if ts.Origin == host {
					return ts.External.Waiting
				}
			}
			return 0
		}
		waitMore := func(than int) {
			deadline := time.Now().Add(5 * time.Second)
			for waiting() <= than && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
		}
		e.setRespond(func(_ http.ResponseWriter, _ *http.Request, _ int64, inputs []string) bool {
			who := inputs[0][:1]
			mu.Lock()
			firstOfB := who == "b" && !strings.Contains(strings.Join(order, ""), "b")
			order = append(order, who)
			first := len(order) == 1
			mu.Unlock()
			switch {
			case first && overlapping: // A's block 1: B queues
				before := waiting()
				post("b", tagged("b", 8))
				waitMore(before)
			case first: // A's block 1: C queues
				before := waiting()
				post("c", tagged("c", 1))
				waitMore(before)
			case firstOfB && overlapping: // B's block 1: C queues
				before := waiting()
				post("c", tagged("c", 1))
				waitMore(before)
			}
			time.Sleep(10 * time.Millisecond)
			return false
		})
		post("a", tagged("a", 8))
		wg.Wait()
		return order
	}
	limitChanged := func(format string, args ...any) {
		t.Helper()
		t.Fatalf("the named limit changed; rewrite this pin into the gate of fairness at the lease boundary: "+format, args...)
	}
	single := run(t, false)
	t.Logf("one multi-block request: %s", strings.Join(single, " "))
	if idx := strings.Index(strings.Join(single, ""), "c"); idx != 1 {
		t.Fatalf("one multi-block request: the third consumer was call %d, want 1 (the first lease boundary)", idx)
	}
	both := run(t, true)
	seq := strings.Join(both, "")
	t.Logf("two overlapping multi-block requests: %s", strings.Join(both, " "))
	c := strings.Index(seq, "c")
	firstEnd := min(strings.LastIndex(seq, "a"), strings.LastIndex(seq, "b"))
	if c < firstEnd {
		limitChanged("the third consumer was call %d, before the first multi-block request ended (call %d): %s", c, firstEnd, seq)
	}
}

func tagged(tag string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s-%d", tag, i)
	}
	return out
}

// TestEmbedLearnedDimensionGrowsTheReserve (H-5, R02): without a native
// dimension the answer reserve is the floats ceiling; when block one names
// a dimension whose answer exceeds it, the reserve grows — and a budget that
// cannot hold the growth is 503 server_overloaded after block one, never an
// answer outside the budget.
func TestEmbedLearnedDimensionGrowsTheReserve(t *testing.T) {
	e := newEmbedEngine(t, 8)
	rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 2, 1)
	rig.setSettings(func(s *oai.Settings) { s.EmbeddingMaxFloats = 8 }) // 4 inputs × 8 components learned = 32 floats
	var during atomic.Int64
	e.setRespond(func(http.ResponseWriter, *http.Request, int64, []string) bool {
		if e.hits.Load() == 1 {
			during.Store(rig.budget.InUse())
		}
		return false
	})
	body := embedBody("emb", embedInputs(4))
	decodeEmbed(t, rig.postEmbed(t, body)) // the probe: what the request holds during block one
	held := during.Load()
	e.hits.Store(0)
	rig.setSettings(func(s *oai.Settings) { s.InflightBudget = held })
	a := rig.postEmbed(t, body)
	code, _, _ := a.envelope(t)
	if a.status != http.StatusServiceUnavailable || code != oai.CodeServerOverloaded {
		t.Fatalf("%d %.200s, want 503 server_overloaded (the learned answer does not fit a budget of %d B)", a.status, a.body, held)
	}
	if h := e.hits.Load(); h != 1 {
		t.Errorf("wire hits %d, want 1 (block one served, then refused)", h)
	}
}

// TestEmbedDeadlineInBlockOneIsTheRowsTimeout (H-5, R03): the request
// deadline that ends the wire call of block one is answered like the chat
// route's attempt timeout — 504 upstream_timeout, the row's timeout counted
// (consecutive_fails 1); only from block two on is the request deadline no
// fault of the row (errBlockDeadline, F-3).
func TestEmbedDeadlineInBlockOneIsTheRowsTimeout(t *testing.T) {
	e := newEmbedEngine(t, 8)
	rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 2, 1)
	rig.setSettings(func(s *oai.Settings) { s.RequestDeadline = 150 * time.Millisecond })
	e.setRespond(func(_ http.ResponseWriter, r *http.Request, _ int64, _ []string) bool {
		<-r.Context().Done()
		return true
	})
	a := rig.postEmbed(t, embedBody("emb", embedInputs(3)))
	if code, _, _ := a.envelope(t); a.status != http.StatusGatewayTimeout || code != oai.CodeUpstreamTimeout {
		t.Fatalf("%d %s, want 504 upstream_timeout", a.status, a.body)
	}
	if n := consecutiveFails(rig.pool, "spark-embed"); n != 1 {
		t.Errorf("consecutive_fails %d, want 1 (a block-one timeout is the row's, as on the chat route)", n)
	}
}

// TestEmbedTenantAliasCeilingClampsMaxInputs (H-5, P01): a tenant alias is
// clamped per request to gateway.tenant_alias_ceiling.embedding_max_inputs,
// whatever its own embedding.max_inputs says.
func TestEmbedTenantAliasCeilingClampsMaxInputs(t *testing.T) {
	e := newEmbedEngine(t, 8)
	rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 2, 1)
	rig.setSettings(func(s *oai.Settings) { s.TenantAliasCeiling.EmbeddingMaxInputs = 4 })
	a := rig.postEmbed(t, embedBody("temb", embedInputs(5)))
	code, param, _ := a.envelope(t)
	if a.status != http.StatusBadRequest || code != oai.CodeUnsupportedValue || param != "input" || !bytes.Contains(a.body, []byte("at most 4 inputs")) {
		t.Fatalf("%d %s, want 400 unsupported_value input \"at most 4 inputs\" (the tenant ceiling, the alias says 32)", a.status, a.body)
	}
}

// TestEmbedRuneLimitIsNotTheWindow (H-5, P03 inverted by FOLGE-EMBLIMIT):
// without embedding.max_input_runes no window of the chain's rows limits an
// input — the pin was "the 9/5 rune budget of the smallest known window"
// (512 tokens ⇒ 921 runes); now 922 and 20 000 runes reach the backend on
// that row, which judges the length itself.
func TestEmbedRuneLimitIsNotTheWindow(t *testing.T) {
	small := newEmbedEngine(t, 8)
	large := newEmbedEngine(t, 8)
	rs, rl := small.row("small", backends.ProtocolOpenAI, 10), large.row("large", backends.ProtocolOpenAI, 20)
	rs.NumCtx, rl.NumCtx = 512, 8192
	rig := newEmbedRig(t, []backends.Backend{rs, rl}, 2, 1)
	for _, n := range []int{922, 20_000} {
		decodeEmbed(t, rig.postEmbed(t, embedBody("emb", []string{strings.Repeat("x", n)})))
	}
	if h := small.hits.Load() + large.hits.Load(); h != 2 {
		t.Errorf("wire hits %d, want 2 (both inputs reached a backend)", h)
	}
}

// TestEmbedAliasRuneLimitOverridesTheChain (H-5, P04 after H-7): an
// alias's embedding.max_input_runes is the limit whatever the chain's
// windows (row num_ctx 512, whose 9/5 budget was 921 runes): 1 500 runes
// are served, 2 001 are 400 at the alias's 2 000 — the early check (H-7),
// the only rune check of the route (FOLGE-EMBLIMIT).
func TestEmbedAliasRuneLimitOverridesTheChain(t *testing.T) {
	e := newEmbedEngine(t, 8)
	row := e.row("small", backends.ProtocolOpenAI, 10)
	row.NumCtx = 512
	rig := newEmbedRig(t, []backends.Backend{row}, 2, 1)
	decodeEmbed(t, rig.postEmbed(t, embedBody("embrunes", []string{strings.Repeat("x", 1500)})))
	a := rig.postEmbed(t, embedBody("embrunes", []string{strings.Repeat("x", 2001)}))
	code, param, _ := a.envelope(t)
	if a.status != http.StatusBadRequest || code != oai.CodeUnsupportedValue || param != "input[0]" || !bytes.Contains(a.body, []byte("at most 2000")) {
		t.Fatalf("%d %s, want 400 unsupported_value input[0] \"at most 2000\"", a.status, a.body)
	}
}

// overcoverPlanner hands out a weight plan covering one unit more than the
// request (a planner defect).
type overcoverPlanner struct{ oai.AdmissionPlanner }

func (p overcoverPlanner) Plan(ctx context.Context, ca *auth.ConsumerAuth, in oai.AdmissionInput) (oai.Plan, error) {
	plan, err := p.AdmissionPlanner.Plan(ctx, ca, in)
	if err == nil {
		plan.Acquirer = overcoverAcquirer{plan.Acquirer, in.Units + 1}
	}
	return plan, err
}

type overcoverAcquirer struct {
	oai.RequestAcquirer
	units int
}

func (a overcoverAcquirer) Blocks() []int { return []int{a.units} }

// TestEmbedOvercoveringPlanIsInternal (H-5, S03): a weight plan covering
// more units than the request is a planner defect — 503 internal_error
// before any wire call, never a slice past the inputs.
func TestEmbedOvercoveringPlanIsInternal(t *testing.T) {
	e := newEmbedEngine(t, 8)
	rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 33, 1,
		func(r *chatRig) { r.deps.Planner = overcoverPlanner{r.planner} })
	a := rig.postEmbed(t, embedBody("emb", embedInputs(3)))
	if code, _, _ := a.envelope(t); a.status != http.StatusServiceUnavailable || code != oai.CodeInternalError {
		t.Fatalf("%d %s, want 503 internal_error", a.status, a.body)
	}
	if e.hits.Load() != 0 {
		t.Errorf("wire hits %d, want 0", e.hits.Load())
	}
}

// TestEmbedRuneLimitBeforeTheReserve (H-7, Fremd-Review 2 R2-3): an input
// over the alias's embedding.max_input_runes is a 400 with its param
// before the answer reserve, the sensitivity scan and the chain build: a
// budget too small for the answer does not turn it into 503
// server_overloaded.
func TestEmbedRuneLimitBeforeTheReserve(t *testing.T) {
	e := newEmbedEngine(t, 8)
	rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 2, 1)
	body := embedBody("embsmall", []string{"ok", strings.Repeat("ä", 17)})
	rig.setSettings(func(s *oai.Settings) { s.InflightBudget = 3*int64(len(body)) + 1 })
	a := rig.postEmbed(t, body)
	code, param, _ := a.envelope(t)
	if a.status != http.StatusBadRequest || code != oai.CodeUnsupportedValue || param != "input[1]" {
		t.Fatalf("%d %s/%q, want 400 unsupported_value input[1] before the reserve", a.status, code, param)
	}
}
