package handler

// W06-8b gates: the foreign-load detector's own cadence (RunForeignLoad,
// status.foreign_load_interval). Every test here runs the ticker headless —
// no Snapshot, no buildCheap, no SSE loop — and asserts that the collector's
// status cache stayed empty throughout. The ticker's sleep is stepped
// (flStepper): each wait it asks for is handed to the test, and it wakes only
// when the test says so. Where hours matter the clock is artificial too and
// the test advances it by exactly the wait the ticker asked for, so cadence,
// boundary alignment and shutdown are deterministic, not timing-dependent.
// The production wiring (NewRouter starts the ticker, real time) is gated in
// cmd/ctxd/foreign_load_ticker_integration_test.go.

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
)

// flStepper stands in for the ticker's sleep: every wait the ticker asks for
// is queued on asked, and the ticker wakes only when the test sends on fire.
type flStepper struct {
	asked chan time.Duration
	fire  chan time.Time
}

func newFLStepper() *flStepper {
	return &flStepper{asked: make(chan time.Duration, 4096), fire: make(chan time.Time)}
}

func (s *flStepper) after(d time.Duration) <-chan time.Time {
	s.asked <- d
	return s.fire
}

// next is the wait the ticker asked for after its latest kick.
func (s *flStepper) next(t *testing.T) time.Duration {
	t.Helper()
	select {
	case d := <-s.asked:
		return d
	case <-time.After(5 * time.Second):
		t.Fatal("the ticker asked for no wait within 5s")
		return 0
	}
}

// wake ends the ticker's current wait.
func (s *flStepper) wake(t *testing.T) {
	t.Helper()
	select {
	case s.fire <- time.Time{}:
	case <-time.After(5 * time.Second):
		t.Fatal("the ticker was not waiting within 5s")
	}
}

// flTickDispatch is an immutable DispatchSource: one slotted origin.
type flTickDispatch struct {
	origin      string
	slots, held int
}

func (d flTickDispatch) Snapshot() dispatch.Snapshot {
	return dispatch.Snapshot{Enabled: true, Targets: []dispatch.TargetSnapshot{{
		Origin: d.origin, Slots: d.slots, Held: d.held, Inflight: d.held,
	}}}
}

func (flTickDispatch) Enforcing() bool { return true }

// flTickServer is a fake engine /metrics around flCounter that counts GETs.
type flTickServer struct {
	*flCounter
	srv  *httptest.Server
	hits atomic.Int32
}

func newFLTickServer(t *testing.T) *flTickServer {
	t.Helper()
	s := &flTickServer{flCounter: &flCounter{}}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		s.serve(w, r)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *flTickServer) origin(t *testing.T) string {
	t.Helper()
	o, err := dispatch.NormalizeOrigin(s.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func flTickMarked(id, host string) backends.Backend {
	return backends.Backend{ID: id, Name: "gpu-" + id, Host: host, Scope: backends.GlobalScope, Enabled: true,
		Roles: []string{backends.RoleChat}, Metadata: map[string]any{"foreign_load_probe": "sglang"}}
}

// flTicker is one collector wired for the ticker gates: a seeded pool, no
// database (the hourly read is injected where a test needs one), a live
// config store and the stepped sleep; RunForeignLoad runs until stop.
type flTicker struct {
	col    *StatusCollector
	store  *config.Store
	step   *flStepper
	cancel context.CancelFunc
	done   chan struct{}
}

func startFLTicker(t *testing.T, rows []backends.Backend, cfg *config.Config, disp DispatchSource, prep func(*foreignLoadDetector)) *flTicker {
	t.Helper()
	bp := backends.NewPool(nil, nil)
	bp.SeedSnapshotForTest(rows)
	store := config.NewStore(cfg)
	col := NewStatusCollector(nil, bp, nil, store, nil, disp)
	step := newFLStepper()
	col.foreignLoad.after = step.after
	if prep != nil {
		prep(col.foreignLoad)
	}
	ctx, cancel := context.WithCancel(context.Background())
	tk := &flTicker{col: col, store: store, step: step, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(tk.done)
		col.RunForeignLoad(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-tk.done:
		case <-time.After(5 * time.Second):
			t.Error("RunForeignLoad still running 5s after its context ended")
		}
	})
	return tk
}

// landed waits until the round that sent GET number n has finished: the GET
// arrived and the single-flight flag is down again. No later round can start
// before the test wakes the ticker, so the flag cannot belong to another one.
func (tk *flTicker) landed(t *testing.T, s *flTickServer, n int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s.hits.Load() >= n && !tk.col.foreignLoad.inflight.Load() {
			if got := s.hits.Load(); got != n {
				t.Fatalf("%d /metrics GETs, want %d — one round per tick", got, n)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("round %d never landed within 5s (GETs %d, in flight %v)", n, s.hits.Load(), tk.col.foreignLoad.inflight.Load())
}

func (tk *flTicker) row(t *testing.T) foreignLoadRow {
	t.Helper()
	rows := tk.col.foreignLoad.section()
	if rows == nil || len(*rows) != 1 {
		t.Fatalf("foreign_load rows = %v, want one target", rows)
	}
	return (*rows)[0]
}

// headless asserts that nothing built a status snapshot: the rounds came from
// the ticker alone.
func (tk *flTicker) headless(t *testing.T) {
	t.Helper()
	if tk.col.cache.Load() != nil {
		t.Fatal("a status snapshot was built — the gate must run without a single status read")
	}
}

func flClock(t *testing.T, s string) time.Time {
	t.Helper()
	c, err := time.Parse("2006-01-02 15:04:05", "2026-11-03 "+s)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestForeignLoadWaitCutsAtHourBoundary pins the ticker's sleep: one
// interval, cut short at the next UTC hour boundary — so the first round of
// each hour lands on the boundary, whatever the interval.
func TestForeignLoadWaitCutsAtHourBoundary(t *testing.T) {
	cet := time.FixedZone("CET", 3600)
	cases := []struct {
		now      time.Time
		interval time.Duration
		want     time.Duration
	}{
		{flClock(t, "10:20:00"), 5 * time.Second, 5 * time.Second},
		{flClock(t, "10:59:58"), 5 * time.Second, 2 * time.Second},
		{flClock(t, "10:59:59").Add(500 * time.Millisecond), 5 * time.Second, 500 * time.Millisecond},
		{flClock(t, "11:00:00"), 5 * time.Second, 5 * time.Second},
		{flClock(t, "10:59:55"), 5 * time.Second, 5 * time.Second},
		{flClock(t, "10:20:00"), 2 * time.Hour, 40 * time.Minute},
		{time.Date(2026, 11, 3, 10, 59, 58, 0, cet), 5 * time.Second, 2 * time.Second},
	}
	for _, c := range cases {
		if got := foreignLoadWait(c.now, c.interval); got != c.want {
			t.Errorf("foreignLoadWait(%s, %v) = %v, want %v", c.now.Format(time.RFC3339Nano), c.interval, got, c.want)
		}
	}
}

// TestForeignLoadTickerHourlyHeadless is the headless gate for both checks:
// with no status reader at all the ticker rounds on its own; check (a) turns
// suspected after two rounds of 3 running against 0 held, and on the
// artificial clock the ticker crosses two hour boundaries — its sleep is cut
// at each one, so the boundary rounds land ON 10:00:00 and 11:00:00, well
// inside grace — after which the hour-10 rollup read is due once grace past
// 11:00 and the hour is evaluated: engine Δ 110 against ctx 100 ⇒
// suspected_hourly. The interval is 7 s so that neither boundary is a
// multiple of it from the start: the cut, not arithmetic luck, hits them.
func TestForeignLoadTickerHourlyHeadless(t *testing.T) {
	s := newFLTickServer(t)
	logs := &flCapture{}
	var mu sync.Mutex
	clock := flClock(t, "09:59:58")
	var reads []string
	cfg := config.Defaults()
	cfg.Status.ForeignLoadInterval = 7 * time.Second // grace = 7 s + 2 s timeout
	responses := func(c time.Time) int64 {
		switch h10, h11 := flClock(t, "10:00:00"), flClock(t, "11:00:00"); {
		case c.Before(h10):
			return 1000
		case c.Before(h11):
			return 1000 + int64(110*c.Sub(h10)/time.Hour)
		default:
			return 1110 + int64(c.Sub(h11)/time.Second)
		}
	}
	s.set(responses(clock), 3)
	tk := startFLTicker(t, []backends.Backend{flTickMarked("a", s.srv.URL)}, cfg,
		flTickDispatch{origin: s.origin(t), slots: 4}, func(d *foreignLoadDetector) {
			d.logger = slog.New(logs)
			d.now = func() time.Time {
				mu.Lock()
				defer mu.Unlock()
				return clock
			}
			d.hourlySum = func(_ context.Context, hour time.Time, _ []string) (int64, error) {
				mu.Lock()
				defer mu.Unlock()
				reads = append(reads, hour.Format("15:04")+" read at "+clock.Format("15:04:05"))
				return 100, nil
			}
		})

	var boundaries []string
	var cuts []string
	end := flClock(t, "11:00:20")
	for n := int32(1); ; n++ {
		tk.landed(t, s, n)
		mu.Lock()
		now := clock
		mu.Unlock()
		if now.Equal(now.Truncate(time.Hour)) {
			boundaries = append(boundaries, now.Format("15:04:05"))
		}
		if n == 2 {
			if r := tk.row(t); !r.Suspected || r.Since == nil {
				t.Fatalf("after 2 headless rounds of running=3 held=0: suspected=%v — check (a) needs no reader", r.Suspected)
			}
		}
		if !now.Before(end) {
			break
		}
		w := tk.step.next(t)
		if w != cfg.Status.ForeignLoadInterval {
			cuts = append(cuts, now.Format("15:04:05")+"+"+w.String())
		}
		mu.Lock()
		clock = clock.Add(w)
		next := clock
		mu.Unlock()
		s.set(responses(next), 3)
		tk.step.wake(t)
	}
	tk.headless(t)
	t.Logf("boundary rounds %v, cut waits %v, rollup reads %v, rounds %d", boundaries, cuts, reads, s.hits.Load())
	if strings.Join(boundaries, ",") != "10:00:00,11:00:00" {
		t.Fatalf("rounds landed on the boundaries %v, want 10:00:00 and 11:00:00", boundaries)
	}
	if strings.Join(cuts, ",") != "09:59:58+2s,10:59:58+2s" {
		t.Fatalf("waits cut at the hour %v, want 09:59:58+2s and 10:59:58+2s", cuts)
	}
	if strings.Join(reads, ";") != "10:00 read at 11:00:14" {
		t.Fatalf("rollup reads %v, want exactly one, of hour 10, in the first round at least grace (9s) past 11:00", reads)
	}
	r := tk.row(t)
	if r.Hourly == nil || !r.Hourly.Hour.Equal(flClock(t, "10:00:00")) || r.Hourly.EngineRequests != 110 || r.Hourly.CtxRequests != 100 || !r.SuspectedHourly {
		t.Fatalf("hourly = %+v suspected_hourly=%v, want hour 10 engine=110 ctx=100 suspected", r.Hourly, r.SuspectedHourly)
	}
	if a, b := logs.count("WARN status: foreign load suspected"), logs.count("WARN status: foreign load hourly mismatch"); a != 1 || b != 1 {
		t.Fatalf("WARNs suspected=%d hourly=%d, want 1/1 (per state change)", a, b)
	}
}

// TestForeignLoadTickerStopsWithContext is the shutdown gate: cancelling the
// server context ends the ticker, cancels the round in flight instead of
// letting it run out its probe timeout (30 s here, so the difference shows),
// records nothing from that round, starts no round after the stop, and leaves
// the goroutine count where it was before the ticker started.
func TestForeignLoadTickerStopsWithContext(t *testing.T) {
	var hits atomic.Int32
	parked := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) >= 2 {
			parked <- struct{}{}
			<-r.Context().Done() // hangs until the client gives up
			return
		}
		(&flCounter{inflight: 3}).serve(w, r)
	}))
	defer srv.Close()
	origin, _ := dispatch.NormalizeOrigin(srv.URL)
	logs := &flCapture{}
	cfg := config.Defaults()
	cfg.Status.ForeignLoadTimeout = 30 * time.Second

	// settle drops the keep-alive connections of both ends (their goroutines
	// are the transport's, not the ticker's) and waits until the count is at
	// most limit, or 3 s.
	settle := func(limit int) int {
		foreignLoadClients.For(false).CloseIdleConnections()
		srv.CloseClientConnections()
		n := runtime.NumGoroutine()
		for deadline := time.Now().Add(3 * time.Second); n > limit && time.Now().Before(deadline); n = runtime.NumGoroutine() {
			time.Sleep(10 * time.Millisecond)
		}
		return n
	}
	settle(runtime.NumGoroutine())
	time.Sleep(50 * time.Millisecond)
	before := runtime.NumGoroutine()

	bp := backends.NewPool(nil, nil)
	bp.SeedSnapshotForTest([]backends.Backend{flTickMarked("a", srv.URL)})
	col := NewStatusCollector(nil, bp, nil, config.NewStore(cfg), nil, flTickDispatch{origin: origin, slots: 4})
	step := newFLStepper()
	col.foreignLoad.after = step.after
	col.foreignLoad.logger = slog.New(logs)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		col.RunForeignLoad(ctx)
	}()

	for deadline := time.Now().Add(5 * time.Second); hits.Load() < 1 || col.foreignLoad.inflight.Load(); {
		if time.Now().After(deadline) {
			t.Fatal("round 1 never landed within 5s")
		}
		time.Sleep(time.Millisecond)
	}
	first := *(*col.foreignLoad.section())[0].ProbedAt
	step.next(t)
	step.wake(t) // round 2 starts; its GET parks
	select {
	case <-parked:
	case <-time.After(5 * time.Second):
		t.Fatal("round 2 never reached /metrics")
	}
	step.next(t) // the ticker is back in its wait
	running := runtime.NumGoroutine()

	stop := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunForeignLoad did not return within 1s of the context ending")
	}
	for col.foreignLoad.inflight.Load() {
		if time.Since(stop) > time.Second {
			t.Fatal("the round in flight outlived the stop by 1s — it runs out its 30s probe timeout instead of being cancelled")
		}
		time.Sleep(time.Millisecond)
	}
	ended := time.Since(stop)
	time.Sleep(300 * time.Millisecond)
	if n := hits.Load(); n != 2 {
		t.Fatalf("%d /metrics GETs, want 2 — a round started after the stop", n)
	}
	if n := len(step.asked); n != 0 {
		t.Fatalf("the ticker asked for %d more waits after the stop", n)
	}
	r := (*col.foreignLoad.section())[0]
	if !r.ProbedAt.Equal(first) || r.ProbeError != nil {
		t.Fatalf("the cancelled round left a trace: probed_at %v (first %v), probe_error %v", r.ProbedAt, first, r.ProbeError)
	}
	if n := logs.count("WARN status: foreign load probe failed"); n != 0 {
		t.Fatalf("%d probe-failed WARNs — a shutdown is not an engine failure", n)
	}
	after := settle(before)
	t.Logf("goroutines before=%d running=%d after=%d, round in flight ended %v after the stop", before, running, after, ended)
	if after > before {
		t.Fatalf("goroutines after the stop %d > before the start %d — the ticker or its round leaked", after, before)
	}
	if col.cache.Load() != nil {
		t.Fatal("a status snapshot was built — the gate must run without a single status read")
	}
}

// TestForeignLoadTickerIntervalHot: status.foreign_load_interval is read per
// tick — 10 s, then a settings swap to 1 s makes the very next wait 1 s; a
// swap to 0 is refused by V37 on the same path a settings write takes, and the
// cadence stays at 1 s. Grace follows the interval too (interval + timeout).
func TestForeignLoadTickerIntervalHot(t *testing.T) {
	s := newFLTickServer(t)
	var mu sync.Mutex
	clock := flClock(t, "10:20:00")
	cfg := config.Defaults()
	cfg.Server.DBPass = "test-password" // Replace validates the whole config, boot-required keys included
	cfg.Status.ForeignLoadInterval = 10 * time.Second
	tk := startFLTicker(t, []backends.Backend{flTickMarked("a", s.srv.URL)}, cfg,
		flTickDispatch{origin: s.origin(t), slots: 4}, func(d *foreignLoadDetector) {
			d.now = func() time.Time {
				mu.Lock()
				defer mu.Unlock()
				return clock
			}
		})
	advance := func(w time.Duration) {
		mu.Lock()
		clock = clock.Add(w)
		mu.Unlock()
		tk.step.wake(t)
	}

	tk.landed(t, s, 1)
	w1 := tk.step.next(t)
	hot := *cfg
	hot.Status.ForeignLoadInterval = time.Second
	if err := tk.store.Replace(&hot); err != nil {
		t.Fatalf("swap to 1s: %v", err)
	}
	advance(w1)
	tk.landed(t, s, 2)
	w2 := tk.step.next(t)
	advance(w2)
	tk.landed(t, s, 3)
	w3 := tk.step.next(t)

	zero := hot
	zero.Status.ForeignLoadInterval = 0
	err := tk.store.Replace(&zero)
	advance(w3)
	tk.landed(t, s, 4)
	w4 := tk.step.next(t)
	t.Logf("waits %v → swap 1s → %v %v → swap 0 refused (%v) → %v", w1, w2, w3, err, w4)
	if w1 != 10*time.Second || w2 != time.Second || w3 != time.Second || w4 != time.Second {
		t.Fatalf("waits %v %v %v %v, want 10s then 1s from the next tick on", w1, w2, w3, w4)
	}
	if err == nil || !strings.Contains(err.Error(), "status.foreign_load_interval") {
		t.Fatalf("interval 0 swap error = %v, want a V37 refusal on status.foreign_load_interval", err)
	}
	if g := foreignLoadSettingsOf(tk.store.Snapshot()).grace; g != time.Second+cfg.Status.ForeignLoadTimeout {
		t.Fatalf("grace %v, want interval 1s + timeout %v", g, cfg.Status.ForeignLoadTimeout)
	}
	tk.headless(t)
}

// TestForeignLoadTickerIdleWithoutTarget: with no probed row — unmarked rows,
// a marked tenant row, a marked but disabled row — 50 ticks send no GET,
// start no round goroutine, publish no section, and a tick allocates nothing:
// its whole cost is the pool snapshot scan.
func TestForeignLoadTickerIdleWithoutTarget(t *testing.T) {
	s := newFLTickServer(t)
	rows := make([]backends.Backend, 0, 24)
	for i := 0; i < 21; i++ {
		rows = append(rows, backends.Backend{ID: "u" + string(rune('a'+i)), Name: "plain-" + string(rune('a'+i)),
			Host: s.srv.URL, Scope: backends.GlobalScope, Enabled: true, Roles: []string{backends.RoleChat},
			Metadata: map[string]any{"score_domain": "logit"}})
	}
	tenant := flTickMarked("t", s.srv.URL)
	tenant.Scope = "acme"
	disabled := flTickMarked("d", s.srv.URL)
	disabled.Enabled = false
	rows = append(rows, tenant, disabled, backends.Backend{ID: "n", Name: "nil-meta", Host: s.srv.URL, Scope: backends.GlobalScope, Enabled: true})

	cfg := config.Defaults()
	baseline := runtime.NumGoroutine()
	tk := startFLTicker(t, rows, cfg, flTickDispatch{origin: s.origin(t), slots: 4}, nil)
	peak := 0
	for i := 0; i < 50; i++ {
		tk.step.next(t)
		if n := runtime.NumGoroutine(); n > peak {
			peak = n
		}
		if tk.col.foreignLoad.inflight.Load() {
			t.Fatalf("tick %d started a probe round with no target", i+1)
		}
		tk.step.wake(t)
	}
	tk.step.next(t)
	if n := s.hits.Load(); n != 0 {
		t.Fatalf("%d /metrics GETs over 50 ticks with no probed row, want 0", n)
	}
	if tk.col.foreignLoad.section() != nil {
		t.Fatal("foreign_load section published with no probed row")
	}
	ctx := context.Background()
	allocs := testing.AllocsPerRun(200, func() { tk.col.kickForeignLoad(ctx, cfg) })
	const n = 20000
	start := time.Now()
	for i := 0; i < n; i++ {
		tk.col.kickForeignLoad(ctx, cfg)
	}
	per := time.Since(start) / n
	t.Logf("51 ticks over %d rows: 0 GETs, goroutines baseline=%d peak=%d (+1 = the ticker), %.0f allocs and %v per tick", len(rows), baseline, peak, allocs, per)
	if allocs != 0 {
		t.Fatalf("a tick with no target allocates %.0f times, want 0", allocs)
	}
	if peak > baseline+1 {
		t.Fatalf("goroutines peaked at %d with baseline %d — more than the ticker itself ran", peak, baseline)
	}
	tk.headless(t)
}
