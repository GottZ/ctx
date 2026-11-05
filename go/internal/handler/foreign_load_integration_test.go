//go:build integration

// W06-8 gates (design/06 §4.9, §5 B-06-26): the foreign-load detector against
// a real PG18 testcontainer and a fake SGLang GET /metrics (httptest).
//
// The collector gates are black-box on purpose: a read is Snapshot() + JSON,
// the dispatcher is a DispatchSource fake. Until W06-8b a tick was
// refreshForBroadcast alone (one buildCheap pass, which kicked the round), so
// no gate named a detector symbol and they ran RED against the pre-W06-8 tree
// (the foreign_load key never appears) — the "erst rot ohne Detektor" proof
// of B-06-26. Since W06-8b rounds come from the detector's own ticker, and
// tick() adds that ticker's one step (kickForeignLoad). The last test (the
// rollup read) calls into the detector directly.
//
//	go test -tags=integration -p 1 ./internal/handler/ -run 'Foreign' -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/testdb"
)

// flMetrics is a fake SGLang /metrics endpoint. Every field is read per
// request, so a test steers the next probe round by mutating it.
type flMetrics struct {
	srv *httptest.Server

	mu      sync.Mutex
	running float64 // sglang:num_running_reqs, split over two label sets
	// active is sglang:http_requests_active on the chat endpoint; negative
	// means "same as running" (one scheduler request per HTTP request).
	active float64
	chat   float64 // chat endpoint: http_requests_total and http_responses_total
	status int     // 0 = 200
	hang   time.Duration
	auth   []string // Authorization header of every request ("" = none)
	// gate, armed by round(), parks every /metrics GET until round()
	// releases it — see round() for why that makes a round atomic.
	gate chan struct{}

	hits atomic.Int32
}

func newFLMetrics(t *testing.T) *flMetrics {
	t.Helper()
	m := &flMetrics{active: -1}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.hits.Add(1)
		m.mu.Lock()
		m.auth = append(m.auth, r.Header.Get("Authorization"))
		running, active, chat, status, hang, gate := m.running, m.active, m.chat, m.status, m.hang, m.gate
		m.mu.Unlock()
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		if gate != nil {
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
		}
		if hang > 0 {
			select {
			case <-time.After(hang):
			case <-r.Context().Done():
				return
			}
		}
		if status != 0 && status != http.StatusOK {
			http.Error(w, "nope", status)
			return
		}
		// Shape copied from the live Spark body (2026-11-03): HELP/TYPE lines,
		// several label sets per family, families the detector must ignore.
		fmt.Fprintf(w, "# HELP sglang:num_running_reqs The number of running requests.\n")
		fmt.Fprintf(w, "# TYPE sglang:num_running_reqs gauge\n")
		fmt.Fprintf(w, "sglang:num_running_reqs{engine_type=\"unified\",model_name=\"qwen38-27b\",moe_ep_rank=\"0\",pp_rank=\"0\",tp_rank=\"0\"} %.1f\n", running-1)
		fmt.Fprintf(w, "sglang:num_running_reqs{engine_type=\"unified\",model_name=\"qwen38-27b\",moe_ep_rank=\"0\",pp_rank=\"0\",tp_rank=\"1\"} 1.0\n")
		fmt.Fprintf(w, "sglang:num_queue_reqs{engine_type=\"unified\",model_name=\"qwen38-27b\"} 7.0\n")
		if active < 0 {
			active = running
		}
		fmt.Fprintf(w, "# TYPE sglang:http_requests_active gauge\n")
		fmt.Fprintf(w, "sglang:http_requests_active{endpoint=\"/v1/chat/completions\",method=\"POST\"} %.1f\n", active)
		fmt.Fprintf(w, "sglang:http_requests_active{endpoint=\"/metrics\",method=\"GET\"} 1.0\n")
		fmt.Fprintf(w, "# TYPE sglang:http_requests_total counter\n")
		fmt.Fprintf(w, "sglang:http_requests_total{endpoint=\"/v1/chat/completions\",method=\"POST\"} %.1f\n", chat)
		fmt.Fprintf(w, "sglang:http_requests_total{endpoint=\"/health\",method=\"GET\"} 6505.0\n")
		fmt.Fprintf(w, "sglang:http_requests_total{endpoint=\"/metrics\",method=\"GET\"} 20197.0\n")
		fmt.Fprintf(w, "# TYPE sglang:http_responses_total counter\n")
		fmt.Fprintf(w, "sglang:http_responses_total{endpoint=\"/v1/chat/completions\",method=\"POST\",status_code=\"200\"} %.1f\n", chat)
		fmt.Fprintf(w, "sglang:http_responses_total{endpoint=\"/metrics\",method=\"GET\",status_code=\"200\"} 20196.0\n")
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *flMetrics) set(running float64) {
	m.mu.Lock()
	m.running = running
	m.mu.Unlock()
}

func (m *flMetrics) origin(t *testing.T) string {
	t.Helper()
	o, err := dispatch.NormalizeOrigin(m.srv.URL)
	if err != nil {
		t.Fatalf("normalize %s: %v", m.srv.URL, err)
	}
	return o
}

// flDispatch is a DispatchSource fake: Held of one origin (the mutex keeps
// the probe goroutine's reads race-clean).
type flDispatch struct {
	mu     sync.Mutex
	origin string
	slots  int
	held   int
}

func (d *flDispatch) Snapshot() dispatch.Snapshot {
	d.mu.Lock()
	defer d.mu.Unlock()
	// HeldWeight = Held: in-house leases weigh 1 — the invariant of a real
	// dispatcher snapshot (every lease weighs ≥ 1), which the detector
	// pairing reads since A03-W8 (running against HeldWeight).
	return dispatch.Snapshot{Enabled: true, Targets: []dispatch.TargetSnapshot{{
		Origin: d.origin, Slots: d.slots, Held: d.held, HeldWeight: d.held, Inflight: d.held,
	}}}
}

func (d *flDispatch) Enforcing() bool { return true }

// flLogs captures the slog records the detector emits (slog.Default — the
// collector logs through it like every other status source).
type flLogs struct {
	mu   sync.Mutex
	recs []string // "LEVEL msg attrs…"
}

func (l *flLogs) Enabled(context.Context, slog.Level) bool { return true }
func (l *flLogs) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Level.String() + " " + r.Message)
	r.Attrs(func(a slog.Attr) bool {
		b.WriteString(" " + a.Key + "=" + a.Value.String())
		return true
	})
	l.mu.Lock()
	l.recs = append(l.recs, b.String())
	l.mu.Unlock()
	return nil
}
func (l *flLogs) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *flLogs) WithGroup(string) slog.Handler      { return l }

func (l *flLogs) count(prefix string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, r := range l.recs {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

func (l *flLogs) all() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.recs, "\n")
}

func captureFLLogs(t *testing.T) *flLogs {
	t.Helper()
	l := &flLogs{}
	prev := slog.Default()
	slog.SetDefault(slog.New(l))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return l
}

// flRow is the JSON view of one foreign_load row the gates read.
type flRow struct {
	Target          string     `json:"target"`
	Running         *int       `json:"running"`
	Held            *int       `json:"held"`
	Delta           *int       `json:"delta"`
	Suspected       bool       `json:"suspected"`
	Since           *time.Time `json:"since"`
	SuspectedHourly bool       `json:"suspected_hourly"`
	ProbeError      *string    `json:"probe_error"`
	ProbedAt        *time.Time `json:"probed_at"`
}

// flHarness wires one collector over a real DB, a seeded backend pool and the
// mutable dispatch fake. tick() is one collector pass plus one detector tick
// (kickForeignLoad, the step RunForeignLoad takes per interval — since W06-8b
// buildCheap kicks no round, and the harness runs no ticker of its own); the
// cache TTL is an hour so Snapshot() never ticks on its own — every probe
// round is one the test started.
type flHarness struct {
	t    *testing.T
	ctx  context.Context
	col  *StatusCollector
	disp *flDispatch
	last time.Time // probed_at of the last round round() saw land
}

func newFLHarness(t *testing.T, rows []backends.Backend, disp *flDispatch) *flHarness {
	t.Helper()
	pool := testdb.SetupTestDB(t)
	bp := backends.NewPool(nil, nil)
	bp.SeedSnapshotForTest(rows)
	cfg := config.Defaults()
	cfg.Events.TickInterval = time.Hour
	var src DispatchSource
	if disp != nil {
		src = disp
	}
	col := NewStatusCollector(pool, bp, fakeDreamMode{}, config.NewStore(cfg), nil, src)
	return &flHarness{t: t, ctx: context.Background(), col: col, disp: disp}
}

func (h *flHarness) tick() {
	h.col.refreshForBroadcast(h.ctx)
	h.col.kickForeignLoad(h.ctx, h.col.cfg.Snapshot())
}

// frame is the server-admin status as the wire carries it.
func (h *flHarness) frame() map[string]json.RawMessage {
	h.t.Helper()
	b, err := json.Marshal(h.col.Snapshot(h.ctx))
	if err != nil {
		h.t.Fatalf("marshal: %v", err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		h.t.Fatalf("unmarshal: %v", err)
	}
	return top
}

func (h *flHarness) rows() ([]flRow, bool) {
	h.t.Helper()
	raw, ok := h.frame()["foreign_load"]
	if !ok {
		return nil, false
	}
	var rows []flRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		h.t.Fatalf("unmarshal foreign_load: %v (%s)", err, raw)
	}
	return rows, true
}

// round runs exactly ONE probe round and waits until it has landed.
//
// The fake's gate makes that deterministic instead of timing-dependent: while
// round() runs, every /metrics GET of m parks at the gate. round() ticks until
// a GET has ARRIVED (hits moved) and then stops ticking. The round that sent
// it cannot finish while its GET is parked, so its single-flight flag stays
// set and every later tick is a no-op — no tick can start a second round,
// however the scheduler interleaves under host load. (The previous helper
// only slept between ticks: a round that started, probed and finished between
// its hits check and the next tick let that tick start a second round, and
// "tick 1" then saw two rounds — the 2-tick hysteresis tripped early.) Only
// then is the GET released and the landing awaited: target's row carries a
// probed_at newer than the last round this harness saw. Ticks before the
// previous round's single-flight tail has cleared are no-ops too, so the
// loop also absorbs that tail. Every test's first collector call is a tick
// (refreshForBroadcast builds and caches directly) and the cache TTL is an
// hour, so a Snapshot read never kicks a round of its own.
func (h *flHarness) round(m *flMetrics, target string) flRow {
	h.t.Helper()
	gate := make(chan struct{})
	m.mu.Lock()
	m.gate = gate
	m.mu.Unlock()
	released := false
	defer func() {
		if !released {
			close(gate)
		}
		m.mu.Lock()
		m.gate = nil
		m.mu.Unlock()
	}()
	hits := m.hits.Load()
	deadline := time.Now().Add(6 * time.Second)
	for m.hits.Load() == hits && time.Now().Before(deadline) {
		h.tick()
		time.Sleep(5 * time.Millisecond)
	}
	if n := m.hits.Load() - hits; n != 1 {
		keys := []string{}
		for k := range h.frame() {
			keys = append(keys, k)
		}
		h.t.Fatalf("%d probe GETs reached %s within 6s, want exactly 1 — foreign_load rows absent or the round not atomic; status keys: %v", n, target, keys)
	}
	close(gate)
	released = true
	for time.Now().Before(deadline) {
		if rows, ok := h.rows(); ok {
			for _, r := range rows {
				if r.Target == target && r.ProbedAt != nil && r.ProbedAt.After(h.last) {
					h.last = *r.ProbedAt
					if n := m.hits.Load() - hits; n != 1 {
						h.t.Fatalf("%d probe GETs during one round(), want 1", n)
					}
					return r
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatalf("the released probe round for %s never landed within 6s", target)
	return flRow{}
}

func flMarked(id, name, host string) backends.Backend {
	return backends.Backend{
		ID: id, Name: name, Host: host, Scope: backends.GlobalScope, Enabled: true,
		Roles:    []string{backends.RoleChat},
		Metadata: map[string]any{"foreign_load_probe": "sglang"},
	}
}

func intv(p *int) string {
	if p == nil {
		return "null"
	}
	return fmt.Sprint(*p)
}

// B-06-26 / Gate 1: the fake reports 3 running requests while the dispatcher
// holds 0 leases. One tick is not enough (hysteresis), the second flips
// suspected with exactly one WARN, a third tick adds no WARN (per state change,
// not per tick).
func TestForeignLoadSuspectedAfterTwoTicks(t *testing.T) {
	logs := captureFLLogs(t)
	m := newFLMetrics(t)
	m.set(3)
	disp := &flDispatch{origin: m.origin(t), slots: 4, held: 0}
	h := newFLHarness(t, []backends.Backend{flMarked("b1", "spark-chat", m.srv.URL)}, disp)
	target := m.origin(t)

	r1 := h.round(m, target)
	t.Logf("tick 1: running=%s held=%s delta=%s suspected=%v", intv(r1.Running), intv(r1.Held), intv(r1.Delta), r1.Suspected)
	if r1.Running == nil || *r1.Running != 3 || r1.Held == nil || *r1.Held != 0 || r1.Delta == nil || *r1.Delta != 3 {
		t.Fatalf("tick 1: want running=3 held=0 delta=3, got running=%s held=%s delta=%s", intv(r1.Running), intv(r1.Held), intv(r1.Delta))
	}
	if r1.Suspected || r1.Since != nil {
		t.Fatalf("tick 1: suspected=%v since=%v — one tick must not trip the 2-tick hysteresis", r1.Suspected, r1.Since)
	}
	if n := logs.count("WARN status: foreign load suspected"); n != 0 {
		t.Fatalf("tick 1: %d suspected WARNs, want 0", n)
	}

	r2 := h.round(m, target)
	t.Logf("tick 2: running=%s held=%s delta=%s suspected=%v since=%v", intv(r2.Running), intv(r2.Held), intv(r2.Delta), r2.Suspected, r2.Since)
	if !r2.Suspected || r2.Since == nil {
		t.Fatalf("tick 2: suspected=%v since=%v — want true with an entry stamp", r2.Suspected, r2.Since)
	}
	if n := logs.count("WARN status: foreign load suspected"); n != 1 {
		t.Fatalf("tick 2: %d suspected WARNs, want exactly 1\n%s", n, logs.all())
	}

	r3 := h.round(m, target)
	if !r3.Suspected || r3.Since == nil || !r3.Since.Equal(*r2.Since) {
		t.Fatalf("tick 3: suspected=%v since=%v — want still true with the tick-2 stamp %v", r3.Suspected, r3.Since, r2.Since)
	}
	if n := logs.count("WARN status: foreign load suspected"); n != 1 {
		t.Fatalf("tick 3: %d suspected WARNs, want still 1 (one WARN per state change, not per tick)", n)
	}
}

// Gate 1b (Fremd-Review W06-8, Major 1): a saturated engine. ctx holds all
// 10 slots and the scheduler runs 10 requests, while two foreign requests sit
// in the engine's waiting queue — the very case in which foreign load
// overtakes ctx's order. The scheduler gauge alone reads running = held; the
// in-flight HTTP gauge of the model endpoints reads 12 > 10 and must suspect.
func TestForeignLoadSaturatedEngineStillSuspects(t *testing.T) {
	captureFLLogs(t)
	m := newFLMetrics(t)
	m.mu.Lock()
	m.running, m.active = 10, 12
	m.mu.Unlock()
	h := newFLHarness(t, []backends.Backend{flMarked("b1", "spark-chat", m.srv.URL)},
		&flDispatch{origin: m.origin(t), slots: 10, held: 10})
	target := m.origin(t)

	h.round(m, target)
	r := h.round(m, target)
	t.Logf("saturated: running=%s held=%s delta=%s suspected=%v", intv(r.Running), intv(r.Held), intv(r.Delta), r.Suspected)
	if !r.Suspected || r.Delta == nil || *r.Delta != 2 {
		t.Fatalf("saturated engine with 2 foreign requests queued: suspected=%v delta=%s — want true/2", r.Suspected, intv(r.Delta))
	}
}

// Gate 2: running <= held never suspects; after a suspected phase the return
// to running <= held clears it (after the same hysteresis) with exactly one
// "cleared" WARN.
func TestForeignLoadRunningWithinHeldAndClear(t *testing.T) {
	logs := captureFLLogs(t)
	m := newFLMetrics(t)
	m.set(3)
	disp := &flDispatch{origin: m.origin(t), slots: 4, held: 3}
	h := newFLHarness(t, []backends.Backend{flMarked("b1", "spark-chat", m.srv.URL)}, disp)
	target := m.origin(t)

	for i := 1; i <= 3; i++ {
		r := h.round(m, target)
		if r.Suspected || r.Delta == nil || *r.Delta != 0 {
			t.Fatalf("running=held=3 tick %d: suspected=%v delta=%s — want false/0", i, r.Suspected, intv(r.Delta))
		}
	}
	m.set(1) // lease-before-engine lag: running < held
	if r := h.round(m, target); r.Suspected || r.Delta == nil || *r.Delta != -2 {
		t.Fatalf("running=1 held=3: suspected=%v delta=%s — want false/-2", r.Suspected, intv(r.Delta))
	}
	if n := logs.count("WARN status: foreign load"); n != 0 {
		t.Fatalf("running <= held produced %d foreign-load WARNs, want 0\n%s", n, logs.all())
	}

	m.set(5) // two foreign requests on top of ctx's three leases
	h.round(m, target)
	if r := h.round(m, target); !r.Suspected {
		t.Fatalf("running=5 held=3 for 2 ticks: suspected=false")
	}
	m.set(3)
	if r := h.round(m, target); !r.Suspected {
		t.Fatal("first clean tick cleared suspected — the exit needs the same hysteresis as the entry")
	}
	r := h.round(m, target)
	if r.Suspected || r.Since != nil {
		t.Fatalf("second clean tick: suspected=%v since=%v — want cleared", r.Suspected, r.Since)
	}
	h.round(m, target)
	if n := logs.count("WARN status: foreign load cleared"); n != 1 {
		t.Fatalf("%d cleared WARNs, want exactly 1\n%s", n, logs.all())
	}
	if n := logs.count("WARN status: foreign load suspected"); n != 1 {
		t.Fatalf("%d suspected WARNs, want exactly 1\n%s", n, logs.all())
	}
}

// Gate 3a: a /metrics answering 401 and an origin nobody listens on both land
// as probe_error (running null, no suspicion), and a WARN names the failure
// once — not per tick.
func TestForeignLoadProbeErrors(t *testing.T) {
	logs := captureFLLogs(t)
	m := newFLMetrics(t)
	m.mu.Lock()
	m.status = http.StatusUnauthorized
	m.mu.Unlock()

	// A port that was listening a moment ago and is closed now: connection refused.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadURL := "http://" + ln.Addr().String()
	ln.Close()
	deadOrigin, _ := dispatch.NormalizeOrigin(deadURL)

	disp := &flDispatch{origin: m.origin(t), slots: 4}
	h := newFLHarness(t, []backends.Backend{
		flMarked("b1", "auth-chat", m.srv.URL),
		flMarked("b2", "dead-chat", deadURL),
	}, disp)

	r := h.round(m, m.origin(t))
	if r.ProbeError == nil || !strings.Contains(*r.ProbeError, "401") || r.Running != nil || r.Suspected {
		t.Fatalf("401 /metrics: probe_error=%v running=%s suspected=%v — want a 401 probe_error, running null", r.ProbeError, intv(r.Running), r.Suspected)
	}
	t.Logf("401 probe_error=%q", *r.ProbeError)
	rows, _ := h.rows()
	var dead *flRow
	for i := range rows {
		if rows[i].Target == deadOrigin {
			dead = &rows[i]
		}
	}
	if dead == nil || dead.ProbeError == nil || dead.Running != nil {
		t.Fatalf("unreachable /metrics: row=%+v — want a probe_error, running null", dead)
	}
	t.Logf("unreachable probe_error=%q", *dead.ProbeError)
	h.round(m, m.origin(t))
	if n := logs.count("WARN status: foreign load probe failed"); n != 2 {
		t.Fatalf("%d probe-failed WARNs over two rounds × two targets, want 2 (one per target, on the state change)\n%s", n, logs.all())
	}
}

// Gate 3b: a /metrics that hangs past the probe timeout (2 s default) must
// not be carried by the status latency: the tick returns at DB speed, reads
// serve the cache meanwhile, and the round lands as a timeout probe_error.
func TestForeignLoadHangingProbeDoesNotCarryStatusLatency(t *testing.T) {
	captureFLLogs(t)
	m := newFLMetrics(t)
	m.mu.Lock()
	m.hang = 5 * time.Second
	m.mu.Unlock()
	disp := &flDispatch{origin: m.origin(t), slots: 4}
	h := newFLHarness(t, []backends.Backend{flMarked("b1", "slow-chat", m.srv.URL)}, disp)
	h.tick() // cold start outside the measurement

	start := time.Now()
	h.tick()
	tickDur := time.Since(start)
	var worst time.Duration
	for i := 0; i < 20; i++ {
		s := time.Now()
		h.frame()
		if d := time.Since(s); d > worst {
			worst = d
		}
		time.Sleep(50 * time.Millisecond)
	}
	for i := 0; i < 3; i++ {
		h.tick() // single-flight: the hung round must not be joined by another GET
	}
	if n := m.hits.Load(); n != 1 {
		t.Fatalf("%d /metrics GETs while the first one hangs, want 1 (single-flight)", n)
	}
	t.Logf("tick %v, worst of 20 status reads during the hanging probe %v", tickDur, worst)
	if tickDur > time.Second {
		t.Fatalf("tick took %v while /metrics hangs — the probe sits on the status path", tickDur)
	}
	if worst > 200*time.Millisecond {
		t.Fatalf("a status read took %v while /metrics hangs — the probe sits on the read path", worst)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if rows, ok := h.rows(); ok && len(rows) == 1 && rows[0].ProbeError != nil {
			if !strings.Contains(*rows[0].ProbeError, "timeout") {
				t.Fatalf("hanging /metrics: probe_error=%q — want a timeout", *rows[0].ProbeError)
			}
			t.Logf("hanging probe_error=%q after %v", *rows[0].ProbeError, time.Since(start))
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("hanging /metrics never surfaced as probe_error")
}

// Gate 3c: with api_key_ref the probe sends the row's resolved secret as a
// Bearer, without it no Authorization header at all; the secret appears
// neither in the status frame nor in a log line (here: a 401 path, the one
// most likely to echo request material).
func TestForeignLoadBearerOnlyWithAPIKeyRef(t *testing.T) {
	logs := captureFLLogs(t)
	const secret = "sk-spark-0123456789-secret"
	withKey := newFLMetrics(t)
	withKey.mu.Lock()
	withKey.status = http.StatusUnauthorized
	withKey.mu.Unlock()
	noKey := newFLMetrics(t)

	keyed := flMarked("b1", "keyed-chat", withKey.srv.URL)
	keyed.APIKeyRef = "spark-api-key"
	keyed.APIKey = secret
	unkeyed := flMarked("b2", "open-chat", noKey.srv.URL)
	unkeyed.APIKey = "stale-value-without-ref" // no ref ⇒ never sent

	h := newFLHarness(t, []backends.Backend{keyed, unkeyed}, &flDispatch{origin: withKey.origin(t), slots: 4})
	h.round(withKey, withKey.origin(t))
	h.round(noKey, noKey.origin(t))

	withKey.mu.Lock()
	gotKeyed := append([]string(nil), withKey.auth...)
	withKey.mu.Unlock()
	noKey.mu.Lock()
	gotOpen := append([]string(nil), noKey.auth...)
	noKey.mu.Unlock()
	if len(gotKeyed) == 0 || gotKeyed[0] != "Bearer "+secret {
		t.Fatalf("keyed probe sent Authorization %q, want the Bearer of api_key_ref", gotKeyed)
	}
	for _, a := range gotOpen {
		if a != "" {
			t.Fatalf("probe without api_key_ref sent Authorization %q, want none", a)
		}
	}
	b, _ := json.Marshal(h.col.Snapshot(h.ctx))
	if strings.Contains(string(b), secret) || strings.Contains(string(b), "stale-value-without-ref") {
		t.Fatalf("status frame carries a backend secret: %s", b)
	}
	if strings.Contains(logs.all(), secret) {
		t.Fatalf("a log line carries the backend secret:\n%s", logs.all())
	}
}

// Gate 5 + shape decision: the server-admin frame carries foreign_load when a
// target is marked; the tenant frame (SnapshotForTenant) never does; with no
// marked target the key is absent on the server-admin path as well ("nothing
// measured" must not read as "measured, clean"); a tenant row carrying the
// marker (written past the validator) is never probed.
func TestForeignLoadExposure(t *testing.T) {
	captureFLLogs(t)
	m := newFLMetrics(t)
	m.set(2)
	tenantMetrics := newFLMetrics(t)
	tenantRow := flMarked("t1", "tenant-chat", tenantMetrics.srv.URL)
	tenantRow.Scope = "st-a"
	h := newFLHarness(t, []backends.Backend{flMarked("b1", "spark-chat", m.srv.URL), tenantRow},
		&flDispatch{origin: m.origin(t), slots: 4})

	h.round(m, m.origin(t))
	h.round(m, m.origin(t))
	rows, ok := h.rows()
	if !ok || len(rows) != 1 || rows[0].Target != m.origin(t) {
		t.Fatalf("server-admin foreign_load = %+v (present=%v) — want exactly the _global target", rows, ok)
	}
	if n := tenantMetrics.hits.Load(); n != 0 {
		t.Fatalf("the tenant row's /metrics was probed %d times — the marker is _global-only", n)
	}

	snap := h.col.SnapshotForTenant(h.ctx, "st-a", nil)
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"foreign_load"`) {
		t.Fatalf("tenant /api/status carries foreign_load (server-global leak): %s", b)
	}

	unmarked := newFLHarness(t, []backends.Backend{{ID: "u", Name: "plain", Host: m.srv.URL, Scope: backends.GlobalScope, Enabled: true}}, nil)
	unmarked.tick()
	time.Sleep(100 * time.Millisecond)
	if _, ok := unmarked.frame()["foreign_load"]; ok {
		t.Fatal("foreign_load present with no marked target — absent must mean nothing is measured")
	}
}

// W06-8b: a status read never starts a probe round. The detector's own ticker
// (RunForeignLoad) is the only round source, so neither the SSE refresh
// (refreshForBroadcast, one buildCheap pass) nor a server-admin poll nor the
// tenant path sends a GET or publishes the section, however often they run —
// with a marked target on a live fake. Red against a tree whose buildCheap
// kicks rounds (v5.30.0): the reads would probe.
func TestForeignLoadStatusReadStartsNoRound(t *testing.T) {
	captureFLLogs(t)
	m := newFLMetrics(t)
	m.set(3)
	h := newFLHarness(t, []backends.Backend{flMarked("b1", "spark-chat", m.srv.URL)}, &flDispatch{origin: m.origin(t), slots: 4})
	for i := 0; i < 10; i++ {
		h.col.refreshForBroadcast(h.ctx)
		h.col.Snapshot(h.ctx)
		h.col.SnapshotForTenant(h.ctx, "st-a", nil)
	}
	time.Sleep(300 * time.Millisecond)
	if n := m.hits.Load(); n != 0 {
		t.Fatalf("%d /metrics GETs from 30 status reads — a status read must never start a probe round", n)
	}
	if _, ok := h.frame()["foreign_load"]; ok {
		t.Fatal("foreign_load published by status reads alone — only a detector tick syncs the targets")
	}
}

// The hourly gate's ctx side against the real rollup (migration 159): rows
// written into context_llm_log land in context_llm_usage_hourly by trigger,
// and queryForeignLoadHourly sums duration_n of ONE bucket over the origin's
// rollup keys — wire calls of every row on the origin (by name, and by raw
// base_url for a row logged without backend_name), never another backend,
// never another hour, never a K9 rejection row (no duration). A key longer
// than the rollup's 100-character cut still matches its cut row.
func TestForeignLoadHourlyRollupRead(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	hour := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	longName := strings.Repeat("ä", 120) // 120 characters, cut to 100 by the trigger
	ins := func(at time.Time, backend, host *string, duration *int) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO context_llm_log (pipeline, model, host, backend_name, duration_ms, created_at)
			VALUES ('query-synthesize', 'qwen38-27b', $1, $2, $3, $4)`, host, backend, duration, at); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	s := func(v string) *string { return &v }
	ms := func(v int) *int { return &v }
	spark, raw := "spark-chat", "http://spark:30000"
	for i := 0; i < 7; i++ {
		ins(hour.Add(time.Duration(i)*time.Minute), s(spark), s(raw), ms(800))
	}
	ins(hour.Add(30*time.Minute), nil, s(raw), ms(500))         // logged by host only
	ins(hour.Add(31*time.Minute), s(longName), s(raw), ms(500)) // name past the 100-char cut
	ins(hour.Add(32*time.Minute), s(spark), s(raw), nil)        // K9 rejection: no wire call
	ins(hour.Add(33*time.Minute), s("other-gpu"), s("http://other:1"), ms(500))
	ins(hour.Add(-time.Minute), s(spark), s(raw), ms(500)) // previous hour
	ins(hour.Add(time.Hour), s(spark), s(raw), ms(500))    // next hour

	n, err := queryForeignLoadHourly(ctx, pool, hour, []string{spark, raw, longName})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if n != 9 {
		t.Fatalf("ctx requests of hour 10 = %d, want 9 (7 named + 1 host-only + 1 long name)", n)
	}
	if n, _ := queryForeignLoadHourly(ctx, pool, hour, []string{"nobody"}); n != 0 {
		t.Fatalf("unknown key read %d, want 0", n)
	}

	// Plan probe for the report: the read is one bucket over a bucket_start-
	// led index, not a scan of the rollup.
	if _, err := pool.Exec(ctx, `ANALYZE context_llm_usage_hourly`); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only plan probe
	if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query(ctx, `EXPLAIN SELECT COALESCE(sum(duration_n), 0)::bigint FROM context_llm_usage_hourly
		WHERE bucket_start = $1 AND backend_name IN (SELECT left(k, 100) FROM unnest($2::text[]) AS k)`, hour, []string{spark})
	if err != nil {
		t.Fatal(err)
	}
	var plan []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, line)
	}
	rows.Close()
	t.Logf("plan:\n%s", strings.Join(plan, "\n"))
	if !strings.Contains(strings.Join(plan, "\n"), "bucket_start = ") {
		t.Fatalf("plan does not drive the bucket_start equality through an index:\n%s", strings.Join(plan, "\n"))
	}
}
