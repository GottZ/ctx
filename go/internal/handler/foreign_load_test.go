package handler

// W06-8 unit gates of the foreign-load detector (foreign_load.go): the
// Prometheus parser contract, the 2-round hysteresis of check (a), the hourly
// comparison of check (b) with injected counter values and an injected rollup
// sum, and the target derivation from the pool snapshot. The collector-level
// gates (status latency, Bearer, tenant exposure) run against a real database
// in foreign_load_integration_test.go.

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
)

var sglangProfile = foreignLoadProfiles[backends.ForeignLoadProbeSGLang]

// liveSGLangExcerpt is cut verbatim from the Spark /metrics body of
// 2026-11-03 14:48Z (qwen38-27b, idle): the three families the detector
// reads, next to families it must ignore — the arrival counter
// http_requests_total and sglang:num_requests_total among them. The one
// in-flight HTTP request is this probe's own GET on /metrics.
const liveSGLangExcerpt = `# HELP sglang:startup_time_seconds Engine startup duration by phase in seconds.
# TYPE sglang:startup_time_seconds gauge
sglang:startup_time_seconds{engine_type="unified",model_name="qwen38-27b",phase="load_weight"} 163.030130327
# HELP sglang:http_requests_total Total number of HTTP requests by endpoint and method
# TYPE sglang:http_requests_total counter
sglang:http_requests_total{endpoint="/model_info",method="GET"} 1.0
sglang:http_requests_total{endpoint="/v1/chat/completions",method="POST"} 11012.0
sglang:http_requests_total{endpoint="/health",method="GET"} 6505.0
sglang:http_requests_total{endpoint="/metrics",method="GET"} 20197.0
# HELP sglang:http_responses_total Total number of HTTP responses by endpoint and status code
# TYPE sglang:http_responses_total counter
sglang:http_responses_total{endpoint="/model_info",method="GET",status_code="200"} 1.0
sglang:http_responses_total{endpoint="/health",method="GET",status_code="503"} 1.0
sglang:http_responses_total{endpoint="/v1/chat/completions",method="POST",status_code="200"} 11012.0
sglang:http_responses_total{endpoint="/freeze_gc",method="POST",status_code="200"} 1.0
sglang:http_responses_total{endpoint="/metrics",method="GET",status_code="200"} 20196.0
sglang:http_responses_total{endpoint="/health",method="GET",status_code="200"} 6504.0
sglang:http_responses_total{endpoint="/get_server_info",method="GET",status_code="200"} 3.0
sglang:http_responses_total{endpoint="/server_info",method="GET",status_code="200"} 1.0
# HELP sglang:num_requests_total Number of requests processed.
# TYPE sglang:num_requests_total counter
sglang:num_requests_total{engine_type="unified",is_streaming="false",model_name="qwen38-27b"} 10996.0
sglang:num_requests_total{engine_type="unified",is_streaming="true",model_name="qwen38-27b"} 16.0
# HELP sglang:http_requests_active Number of currently active HTTP requests
# TYPE sglang:http_requests_active gauge
sglang:http_requests_active{endpoint="/model_info",method="GET"} 0.0
sglang:http_requests_active{endpoint="/v1/chat/completions",method="POST"} 0.0
sglang:http_requests_active{endpoint="/health",method="GET"} 0.0
sglang:http_requests_active{endpoint="/metrics",method="GET"} 1.0
# HELP sglang:num_running_reqs The number of running requests.
# TYPE sglang:num_running_reqs gauge
sglang:num_running_reqs{engine_type="unified",model_name="qwen38-27b",moe_ep_rank="0",pp_rank="0",tp_rank="0"} 0.0
# HELP sglang:num_queue_reqs The number of requests in the waiting queue.
# TYPE sglang:num_queue_reqs gauge
sglang:num_queue_reqs{engine_type="unified",model_name="qwen38-27b",moe_ep_rank="0",pp_rank="0",tp_rank="0"} 0.0
`

func TestParseForeignLoadMetrics(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		running   int64
		active    int64
		hasActive bool
		responses int64
		err       string
	}{
		{name: "live excerpt", body: liveSGLangExcerpt, running: 0, active: 0, hasActive: true, responses: 11012},
		{name: "sums over rank label sets and model endpoints, status codes summed", body: strings.Join([]string{
			`sglang:num_running_reqs{tp_rank="0"} 2.0`,
			`sglang:num_running_reqs{tp_rank="1"} 1`,
			`sglang:http_requests_active{endpoint="/v1/chat/completions",method="POST"} 4`,
			`sglang:http_requests_active{endpoint="/v1/embeddings",method="POST"} 1`,
			`sglang:http_requests_active{endpoint="/metrics",method="GET"} 1`,
			`sglang:http_responses_total{endpoint="/v1/chat/completions",method="POST",status_code="200"} 100`,
			`sglang:http_responses_total{endpoint="/v1/chat/completions",method="POST",status_code="500"} 3`,
			`sglang:http_responses_total{endpoint="/v1/embeddings",method="POST",status_code="200"} 1.0e+01`,
			`sglang:http_responses_total{endpoint="/generate",method="POST",status_code="200"} 5 1730645000000`,
			`sglang:http_responses_total{endpoint="/health",method="GET",status_code="200"} 999`,
			`sglang:http_responses_total{endpoint="/metrics",method="GET",status_code="200"} 999`,
			`sglang:http_responses_total{endpoint="/some/new/route",method="POST",status_code="200"} 999`,
			`sglang:http_responses_total{method="POST",status_code="200"} 999`,
			`sglang:http_requests_total{endpoint="/v1/chat/completions",method="POST"} 999`,
		}, "\n"), running: 3, active: 5, hasActive: true, responses: 118},
		{name: "tolerant: garbage, prefix twins, NaN, broken label set, CRLF, escapes", body: strings.Join([]string{
			`this is not a metric line at all`,
			`sglang:num_running_reqs_max{tp_rank="0"} 50`,
			`sglang:num_running_reqsx 50`,
			`sglang:num_running_reqs{tp_rank="0"} NaN`,
			`sglang:num_running_reqs{tp_rank="0" 7`,
			`sglang:num_running_reqs{note="a } inside \"quotes\" \\ ok",tp_rank="1"} 4` + "\r",
			`sglang:num_running_reqs`,
			`sglang:http_responses_total{note="x\",endpoint=\"/v1/chat/completions",endpoint="/v1/completions"} 6`,
			`sglang:http_responses_total{endpoint="/v1/chat/completions" , method="POST",} 2`,
			`   `,
		}, "\n"), running: 4, responses: 8},
		{name: "declared counter without samples reads 0, active absent", body: "# TYPE sglang:http_responses_total counter\nsglang:num_running_reqs 1\n",
			running: 1, responses: 0},
		{name: "foreign vocabulary", body: "# TYPE llamacpp:requests_processing gauge\nllamacpp:requests_processing 2\n",
			err: "metric family absent: sglang:num_running_reqs, sglang:http_responses_total"},
		{name: "running gauge missing", body: "sglang:http_responses_total{endpoint=\"/generate\"} 1\n",
			err: "metric family absent: sglang:num_running_reqs"},
		{name: "only the arrival counter", body: "sglang:num_running_reqs 1\nsglang:http_requests_total{endpoint=\"/generate\"} 1\n",
			err: "metric family absent: sglang:http_responses_total"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := parseForeignLoadMetrics([]byte(tc.body), sglangProfile)
			if tc.err != "" {
				if err == nil || err.Error() != tc.err {
					t.Fatalf("err = %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if r.running != tc.running || r.active != tc.active || r.hasActive != tc.hasActive || r.responses != tc.responses {
				t.Fatalf("got running=%d active=%d/%v responses=%d, want %d %d/%v %d",
					r.running, r.active, r.hasActive, r.responses, tc.running, tc.active, tc.hasActive, tc.responses)
			}
		})
	}
}

// flCapture is a slog handler collecting "LEVEL msg" lines.
type flCapture struct {
	mu   sync.Mutex
	recs []string
}

func (c *flCapture) Enabled(context.Context, slog.Level) bool { return true }
func (c *flCapture) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	c.recs = append(c.recs, r.Level.String()+" "+r.Message)
	c.mu.Unlock()
	return nil
}
func (c *flCapture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *flCapture) WithGroup(string) slog.Handler      { return c }

func (c *flCapture) count(line string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, r := range c.recs {
		if r == line {
			n++
		}
	}
	return n
}

func okProbe(running, responses int64) foreignLoadProbe {
	return foreignLoadProbe{reading: foreignLoadReading{running: running, responses: responses}}
}

// TestForeignLoadHysteresis drives check (a) round by round: the entry needs
// status.foreign_load_hysteresis_ticks consecutive excess rounds, the exit the
// same number of clean ones, each transition logs exactly one WARN, a failed
// round breaks the streak without touching the state, and a pass-through
// target (held unknown) never suspects.
func TestForeignLoadHysteresis(t *testing.T) {
	for _, hyst := range []int{2, 3} {
		t.Run(fmt.Sprintf("hysteresis=%d", hyst), func(t *testing.T) {
			logs := &flCapture{}
			log := slog.New(logs)
			set := foreignLoadSettings{hysteresis: hyst}
			st := &foreignLoadState{target: foreignLoadTarget{origin: "http://gpu:30000"}}
			now := time.Date(2026, 11, 3, 15, 0, 30, 0, time.UTC)
			step := func(p foreignLoadProbe, held int, known bool) {
				now = now.Add(5 * time.Second)
				st.observe(now, p, held, known, set, log)
			}
			for i := 1; i < hyst; i++ {
				step(okProbe(3, 0), 0, true)
				if st.suspected {
					t.Fatalf("suspected after %d of %d excess rounds", i, hyst)
				}
			}
			step(foreignLoadProbe{errText: "timeout"}, 0, true) // breaks the streak
			if st.suspected || st.running != nil || st.probeErr != "timeout" {
				t.Fatalf("failed round: suspected=%v running=%v err=%q", st.suspected, st.running, st.probeErr)
			}
			for i := 1; i < hyst; i++ {
				step(okProbe(3, 0), 0, true)
				if st.suspected {
					t.Fatalf("suspected after the failed round plus %d of %d excess rounds — a failed round must break the streak", i, hyst)
				}
			}
			step(okProbe(3, 0), 0, true)
			if !st.suspected || st.since == nil || *st.delta != 3 {
				t.Fatalf("after %d excess rounds: suspected=%v since=%v", hyst, st.suspected, st.since)
			}
			entered := *st.since
			for i := 1; i < hyst; i++ {
				step(okProbe(2, 2), 2, true) // running <= held: clean, but not yet enough
				if !st.suspected {
					t.Fatalf("cleared after %d of %d clean rounds", i, hyst)
				}
			}
			if !st.since.Equal(entered) {
				t.Fatal("since moved while suspected")
			}
			step(okProbe(0, 0), 4, true)
			if st.suspected || st.since != nil {
				t.Fatalf("after %d clean rounds: suspected=%v since=%v", hyst, st.suspected, st.since)
			}
			if a, b := logs.count("WARN status: foreign load suspected"), logs.count("WARN status: foreign load cleared"); a != 1 || b != 1 {
				t.Fatalf("WARNs suspected=%d cleared=%d, want 1/1", a, b)
			}
			if n := logs.count("WARN status: foreign load probe failed"); n != 1 {
				t.Fatalf("probe-failed WARNs = %d, want 1", n)
			}
			for i := 0; i < 2*hyst; i++ {
				step(okProbe(9, 0), 0, false) // pass-through: held not comparable
			}
			if st.suspected || st.held != nil || st.delta != nil || *st.running != 9 {
				t.Fatalf("pass-through target: suspected=%v held=%v delta=%v", st.suspected, st.held, st.delta)
			}
		})
	}
}

// TestHeldOfAndMerge pins the held-comparability rule against the dispatcher
// snapshot: slotted + enabled ⇒ comparable, pass-through ⇒ not, absent (never
// acquired since boot) ⇒ a factual 0, no dispatcher ⇒ not; the two snapshots
// around a probe merge to the larger count.
func TestHeldOfAndMerge(t *testing.T) {
	snap := func(enabled bool, slots, held int) dispatch.Snapshot {
		return dispatch.Snapshot{Enabled: enabled, Targets: []dispatch.TargetSnapshot{{Origin: "http://gpu:30000", Slots: slots, Held: held}}}
	}
	for _, tc := range []struct {
		name  string
		s     dispatch.Snapshot
		held  int
		known bool
	}{
		{"slotted", snap(true, 4, 3), 3, true},
		{"no slots", snap(true, 0, 0), 0, false},
		{"dispatcher disabled", snap(false, 4, 0), 0, false},
		{"absent", dispatch.Snapshot{Enabled: true}, 0, true},
		{"absent, dispatcher disabled", dispatch.Snapshot{Enabled: false}, 0, false},
	} {
		if h, k := heldOf(tc.s, "http://gpu:30000"); h != tc.held || k != tc.known {
			t.Errorf("%s: heldOf = %d/%v, want %d/%v", tc.name, h, k, tc.held, tc.known)
		}
	}
	if h, k := mergeHeld(snap(true, 4, 1), snap(true, 4, 3), true, "http://gpu:30000"); h != 3 || !k {
		t.Errorf("merge = %d/%v, want 3/true", h, k)
	}
	if _, k := mergeHeld(dispatch.Snapshot{}, dispatch.Snapshot{}, false, "http://gpu:30000"); k {
		t.Error("no dispatcher wired must not be comparable")
	}
}

// flCounter is a fake /metrics with an engine that counts a request on
// ARRIVAL (http_requests_total) and on COMPLETION (http_responses_total); the
// difference is what is in flight (http_requests_active). The test sets the
// completed count and the in-flight count per round.
type flCounter struct {
	mu                  sync.Mutex
	responses, inflight int64
}

func (c *flCounter) set(responses, inflight int64) {
	c.mu.Lock()
	c.responses, c.inflight = responses, inflight
	c.mu.Unlock()
}

func (c *flCounter) serve(w http.ResponseWriter, _ *http.Request) {
	c.mu.Lock()
	resp, infl := c.responses, c.inflight
	c.mu.Unlock()
	fmt.Fprintf(w, "sglang:num_running_reqs{tp_rank=\"0\"} %d\n", infl)
	fmt.Fprintf(w, "sglang:http_requests_active{endpoint=\"/v1/chat/completions\",method=\"POST\"} %d\n", infl)
	fmt.Fprintf(w, "sglang:http_requests_total{endpoint=\"/v1/chat/completions\",method=\"POST\"} %d\n", resp+infl)
	fmt.Fprintf(w, "sglang:http_responses_total{endpoint=\"/v1/chat/completions\",method=\"POST\",status_code=\"200\"} %d\n", resp)
}

// TestForeignLoadHourly is the hourly gate (check b) with injected values:
// counter samples at the hour boundaries, the rollup sum injected. Δ 110 vs.
// rollup 100 ⇒ suspected_hourly with one WARN — and a second suspected hour
// adds no WARN; Δ 102 vs. 100 ⇒ false with one "cleared" WARN (tolerance
// max(5, 1 %)). The read waits one grace past the boundary. Six requests in
// flight across a boundary are not foreign load: the engine counter that
// counts on arrival would show them in the closing hour, while ctx logs them
// in the next — the comparison reads the completion counter. Then the ratio
// term binds at 1 000 ctx requests, a late boundary sample makes the hour not
// comparable, an engine restart (counter backwards) is skipped, min_abs is
// read from the settings, and a deficit never suspects.
func TestForeignLoadHourly(t *testing.T) {
	fake := &flCounter{}
	srv := httptest.NewServer(http.HandlerFunc(fake.serve))
	defer srv.Close()
	origin, _ := dispatch.NormalizeOrigin(srv.URL)
	targets := []foreignLoadTarget{{origin: origin, kind: backends.ForeignLoadProbeSGLang, names: []string{"spark-chat"},
		rollupKeys: []string{"spark-chat", srv.URL}}}

	logs := &flCapture{}
	var clock time.Time
	var rollup int64
	var reads []string
	d := newForeignLoadDetector(nil, nil)
	d.logger = slog.New(logs)
	d.now = func() time.Time { return clock }
	d.hourlySum = func(_ context.Context, hour time.Time, keys []string) (int64, error) {
		reads = append(reads, hour.Format("15:04")+" "+strings.Join(keys, ","))
		return rollup, nil
	}
	set := foreignLoadSettings{timeout: 2 * time.Second, hysteresis: 2, minAbs: 5, minRatio: 0.01, grace: 7 * time.Second}
	d.mu.Lock()
	d.syncLocked(targets)
	d.mu.Unlock()
	at := func(hhmmss string, responses, inflight int64) foreignLoadRow {
		t.Helper()
		var err error
		clock, err = time.Parse("2006-01-02 15:04:05", "2026-11-03 "+hhmmss)
		if err != nil {
			t.Fatal(err)
		}
		fake.set(responses, inflight)
		d.round(context.Background(), targets, set)
		return (*d.section())[0]
	}
	hourly := func(r foreignLoadRow) string {
		if r.Hourly == nil {
			return "none"
		}
		return fmt.Sprintf("%s engine=%d ctx=%d suspected=%v", r.Hourly.Hour.Format("15:04"), r.Hourly.EngineRequests, r.Hourly.CtxRequests, r.SuspectedHourly)
	}
	expect := func(r foreignLoadRow, want string) {
		t.Helper()
		if got := hourly(r); got != want {
			t.Fatalf("hourly = %q, want %q", got, want)
		}
	}

	rollup = 100
	at("10:00:03", 1000, 0)
	at("10:30:00", 1050, 0)
	at("11:00:02", 1110, 0) // hour 10 pending: Δ 110
	if r := at("11:00:05", 1110, 0); r.Hourly != nil || len(reads) != 0 {
		t.Fatalf("hour 10 read 5 s after the boundary, inside the 7 s grace: %s, reads %v", hourly(r), reads)
	}
	r := at("11:00:10", 1111, 0)
	t.Logf("Δ110 vs rollup 100: %s", hourly(r))
	expect(r, "10:00 engine=110 ctx=100 suspected=true")
	if len(reads) != 1 || reads[0] != "10:00 spark-chat,"+srv.URL {
		t.Fatalf("rollup reads %v, want one read of hour 10 over the origin's rollup keys", reads)
	}
	at("12:00:01", 1220, 0) // hour 11: Δ 110 again
	expect(at("12:00:09", 1220, 0), "11:00 engine=110 ctx=100 suspected=true")
	if n := logs.count("WARN status: foreign load hourly mismatch"); n != 1 {
		t.Fatalf("hourly mismatch WARNs after two suspected hours = %d, want 1 (one per state change)", n)
	}

	at("13:00:01", 1322, 0) // hour 12: Δ 102
	r = at("13:00:09", 1322, 0)
	t.Logf("Δ102 vs rollup 100: %s", hourly(r))
	expect(r, "12:00 engine=102 ctx=100 suspected=false")
	if n := logs.count("WARN status: foreign load hourly mismatch cleared"); n != 1 {
		t.Fatalf("cleared WARNs = %d, want 1", n)
	}

	// Six ctx requests are in flight across 14:00: the engine has counted
	// their arrival in hour 13, ctx logs them in hour 14.
	at("14:00:01", 1422, 6)
	r = at("14:00:09", 1422, 6)
	t.Logf("boundary in flight: %s", hourly(r))
	expect(r, "13:00 engine=100 ctx=100 suspected=false")

	rollup = 1000 // ratio term binds: tolerance max(5, 10) = 10
	at("15:00:01", 2433, 6)
	expect(at("15:00:09", 2433, 6), "14:00 engine=1011 ctx=1000 suspected=true")
	at("16:00:01", 3443, 6)
	expect(at("16:00:09", 3443, 6), "15:00 engine=1010 ctx=1000 suspected=false")

	before := len(reads)
	at("17:00:30", 4000, 0) // first sample of hour 17 is past grace: no boundary for 17
	at("18:00:01", 5000, 0) // hour 17 has no start mark ⇒ not comparable
	at("18:00:09", 5000, 0)
	if len(reads) != before {
		t.Fatalf("a late boundary sample still produced a rollup read: %v", reads[before:])
	}
	at("19:00:01", 10, 0) // counter went backwards: engine restart, hour 18 skipped
	at("19:00:09", 10, 0)
	if len(reads) != before {
		t.Fatalf("a counter reset still produced a rollup read: %v", reads[before:])
	}

	set.minAbs = 20 // the floor is a setting: Δ110 vs 100 is now inside the tolerance
	rollup = 100
	at("20:00:01", 120, 0) // hour 19: 120 - 10 = 110
	expect(at("20:00:09", 120, 0), "19:00 engine=110 ctx=100 suspected=false")

	// A deficit is not foreign load: ctx logged 150 calls the engine counted
	// only 90 of (refused before its HTTP layer) — far outside the tolerance,
	// and still no suspicion.
	rollup = 150
	at("21:00:01", 210, 0) // hour 20: 210 - 120 = 90
	expect(at("21:00:09", 210, 0), "20:00 engine=90 ctx=150 suspected=false")
}

// TestForeignLoadHourlyReadFailureWarnsOnce: a rollup read that keeps
// failing logs ONE WARN, not one per round, keeps the hour pending, and the
// first successful read logs the recovery and settles the hour.
func TestForeignLoadHourlyReadFailureWarnsOnce(t *testing.T) {
	fake := &flCounter{}
	srv := httptest.NewServer(http.HandlerFunc(fake.serve))
	defer srv.Close()
	origin, _ := dispatch.NormalizeOrigin(srv.URL)
	targets := []foreignLoadTarget{{origin: origin, kind: backends.ForeignLoadProbeSGLang, rollupKeys: []string{"spark-chat"}}}
	logs := &flCapture{}
	var clock time.Time
	failing := true
	d := newForeignLoadDetector(nil, nil)
	d.logger = slog.New(logs)
	d.now = func() time.Time { return clock }
	d.hourlySum = func(context.Context, time.Time, []string) (int64, error) {
		if failing {
			return 0, fmt.Errorf("pool closed")
		}
		return 100, nil
	}
	set := foreignLoadSettings{timeout: time.Second, hysteresis: 2, minAbs: 5, minRatio: 0.01, grace: 7 * time.Second}
	d.mu.Lock()
	d.syncLocked(targets)
	d.mu.Unlock()
	at := func(hhmmss string, responses int64) {
		t.Helper()
		clock, _ = time.Parse("2006-01-02 15:04:05", "2026-11-03 "+hhmmss)
		fake.set(responses, 0)
		d.round(context.Background(), targets, set)
	}
	at("10:00:01", 0)
	at("11:00:01", 110)
	for _, ts := range []string{"11:00:10", "11:00:15", "11:00:20", "11:00:25"} {
		at(ts, 110)
	}
	if n := logs.count("WARN status: foreign load hourly rollup read failed"); n != 1 {
		t.Fatalf("%d read-failure WARNs over 4 failing rounds, want 1", n)
	}
	if r := (*d.section())[0]; r.Hourly != nil {
		t.Fatalf("a failed read settled the hour: %+v", r.Hourly)
	}
	failing = false
	at("11:00:30", 110)
	r := (*d.section())[0]
	if logs.count("INFO status: foreign load hourly rollup read recovered") != 1 || r.Hourly == nil || !r.SuspectedHourly {
		t.Fatalf("recovery: hourly=%+v suspected=%v logs=%v", r.Hourly, r.SuspectedHourly, logs.recs)
	}
}

// TestForeignLoadSettingsOf pins the A-E2 wiring: every policy value of a
// round comes from its registry key (here all four off their defaults), and
// grace is derived from events.tick_interval + status.foreign_load_timeout.
func TestForeignLoadSettingsOf(t *testing.T) {
	cfg := config.Defaults()
	cfg.Events.TickInterval = 10 * time.Second
	cfg.Status.ForeignLoadTimeout = 3 * time.Second
	cfg.Status.ForeignLoadHysteresisTicks = 4
	cfg.Status.ForeignLoadHourlyMinAbs = 7
	cfg.Status.ForeignLoadHourlyMinRatio = 0.05
	got := foreignLoadSettingsOf(cfg)
	want := foreignLoadSettings{timeout: 3 * time.Second, hysteresis: 4, minAbs: 7, minRatio: 0.05, grace: 13 * time.Second}
	if got != want {
		t.Fatalf("settings = %+v, want %+v", got, want)
	}
	def := config.Defaults()
	got = foreignLoadSettingsOf(def)
	want = foreignLoadSettings{timeout: 2 * time.Second, hysteresis: 2, minAbs: 5, minRatio: 0.01, grace: def.Events.TickInterval + 2*time.Second}
	if got != want {
		t.Fatalf("default settings = %+v, want %+v", got, want)
	}
}

// TestFetchForeignLoadBodyCap: a body past foreignLoadBodyCap is a probe
// error, never a truncated parse; one just under it parses.
func TestFetchForeignLoadBodyCap(t *testing.T) {
	families := "sglang:num_running_reqs 1\nsglang:http_responses_total{endpoint=\"/generate\"} 5\nsglang:http_requests_total{endpoint=\"/generate\"} 5\n"
	var size int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		filler := size - len(families)
		_, _ = w.Write([]byte(families + strings.Repeat("#", filler)))
	}))
	defer srv.Close()
	origin, _ := dispatch.NormalizeOrigin(srv.URL)
	target := foreignLoadTarget{origin: origin, kind: backends.ForeignLoadProbeSGLang}
	size = foreignLoadBodyCap + 1
	if p := fetchForeignLoad(context.Background(), target, 5*time.Second); p.errText != fmt.Sprintf("metrics body over %d bytes", foreignLoadBodyCap) {
		t.Fatalf("body of cap+1 bytes: errText %q", p.errText)
	}
	size = foreignLoadBodyCap
	if p := fetchForeignLoad(context.Background(), target, 5*time.Second); p.errText != "" || p.reading.running != 1 {
		t.Fatalf("body of cap bytes: errText %q reading %+v", p.errText, p.reading)
	}
}

// TestForeignLoadTargets pins the target derivation: only enabled, not
// profile-disabled, shared rows with a known probe kind are probed; rows
// sharing an origin collapse into one target whose rollup keys cover every
// row on that origin (any scope, any state) by name and raw base_url; the
// Bearer comes from a keyed marked row only, an unresolved ref is reported.
func TestForeignLoadTargets(t *testing.T) {
	marked := func(id, name, host, scope string) backends.Backend {
		return backends.Backend{ID: id, Name: name, Host: host, Scope: scope, Enabled: true,
			Metadata: map[string]any{"foreign_load_probe": "sglang"}}
	}
	keyed := marked("k", "spark-chat", "http://Spark:30000", backends.GlobalScope)
	keyed.APIKeyRef, keyed.APIKey = "spark-key", "s3cret"
	disabled := marked("d", "off", "http://off:1", backends.GlobalScope)
	disabled.Enabled = false
	unresolved := marked("u", "unresolved", "http://unres:1", backends.GlobalScope)
	unresolved.APIKeyRef = "missing-secret"
	rows := []backends.Backend{
		keyed,
		marked("t", "tenant-gpu", "https://tenant.example.com", "acme"),
		disabled,
		marked("p", "ejected", "http://ejected:1", backends.GlobalScope),
		marked("x", "typo", "http://typo:1", backends.GlobalScope),
		unresolved,
		{ID: "o", Name: "spark-digest", Host: "http://spark:30000/", Scope: backends.GlobalScope},
		{ID: "o2", Name: "acme-direct", Host: "http://spark:30000", Scope: "acme"},
		{ID: "z", Name: "elsewhere", Host: "http://other:9", Scope: backends.GlobalScope},
	}
	rows[4].Metadata["foreign_load_probe"] = "vllm"
	got := foreignLoadTargets(rows, map[string]string{"p": "eject"})
	if len(got) != 2 {
		t.Fatalf("targets = %+v, want spark + unres", got)
	}
	s := got[0]
	if s.origin != "http://spark:30000" || s.bearer != "s3cret" || s.unresolvedRef {
		t.Fatalf("spark target = %+v", s)
	}
	if strings.Join(s.names, ",") != "acme-direct,spark-chat,spark-digest" {
		t.Fatalf("names = %v", s.names)
	}
	if strings.Join(s.rollupKeys, ",") != "acme-direct,http://Spark:30000,http://spark:30000,http://spark:30000/,spark-chat,spark-digest" {
		t.Fatalf("rollup keys = %v", s.rollupKeys)
	}
	if u := got[1]; u.origin != "http://unres:1" || !u.unresolvedRef || u.bearer != "" {
		t.Fatalf("unresolved target = %+v", u)
	}
	if p := fetchForeignLoad(context.Background(), got[1], time.Second); p.errText != "api_key_ref unresolved" {
		t.Fatalf("unresolved ref probe = %q, want no request and a named cause", p.errText)
	}
	if foreignLoadTargets(rows[6:], nil) != nil {
		t.Fatal("no marked row must yield nil targets (section absent)")
	}
}
