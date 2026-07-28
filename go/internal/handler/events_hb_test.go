package handler

// Heartbeat gates for GET /api/events (RC-1 wave S3). The keepalive must be an
// OBSERVABLE event carrying a success stamp, not a bare ": ping" comment: an SSE
// comment is dropped by every eventsource parser (including this repo's own FE
// client, sse.svelte.ts), so a comment-only keepalive proves transport liveness
// to the SOCKET and nothing at all to the CLIENT — which then cannot tell "the
// pipe is warm" from "the server is answering".
//
// The wire-level tests here drive the real HandleEvents loop over httptest, so
// they pin the SHIPPED framing, not a helper's return value.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
)

// sseFrameRec is one parsed frame off a live stream: the event name (empty for a
// comment), its raw data line and the arrival time (the cadence probe needs the
// spacing, not just the count).
type sseFrameRec struct {
	name string
	data string
	at   time.Time
}

// readSSEFrames streams the handler's response and records frames until ctx ends
// or n frames of interest arrived. Comments (": ping") are recorded with an
// empty name so a probe can assert what the client would NOT see.
func readSSEFrames(t *testing.T, body io.Reader, stop <-chan struct{}) []sseFrameRec {
	t.Helper()
	var (
		mu   sync.Mutex
		out  []sseFrameRec
		done = make(chan struct{})
	)
	go func() {
		defer close(done)
		sc := bufio.NewScanner(body)
		cur := sseFrameRec{}
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				cur.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				cur.data = strings.TrimPrefix(line, "data: ")
			case strings.HasPrefix(line, ": "):
				mu.Lock()
				out = append(out, sseFrameRec{name: "", data: strings.TrimPrefix(line, ": "), at: time.Now()})
				mu.Unlock()
			case line == "":
				if cur.name != "" {
					cur.at = time.Now()
					mu.Lock()
					out = append(out, cur)
					mu.Unlock()
				}
				cur = sseFrameRec{}
			}
		}
	}()
	select {
	case <-stop:
	case <-done:
	}
	mu.Lock()
	defer mu.Unlock()
	cp := make([]sseFrameRec, len(out))
	copy(cp, out)
	return cp
}

// hbStreamProbe opens one live /api/events stream against a fake provider and
// collects frames for d. tick is deliberately LONG relative to d so the status /
// backends diff stays silent after the initial full state — every further frame
// then has to come from the keepalive path.
func hbStreamProbe(t *testing.T, fs *fakeStatus, ping, tick, d time.Duration) []sseFrameRec {
	t.Helper()
	orig := sseWriteWindow
	sseWriteWindow = 5 * time.Second
	t.Cleanup(func() { sseWriteWindow = orig })

	life, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h := newTestHub(life, fs, eventsCfgStore(tick, ping, 8))
	eh := &EventsHandler{hub: h}
	srv := httptest.NewServer(http.HandlerFunc(eh.HandleEvents))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL) //nolint:noctx // test client on a local httptest server
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	stop := make(chan struct{})
	go func() {
		time.Sleep(d)
		close(stop)
		cancel() // ends the handler so the body reader unblocks
	}()
	return readSSEFrames(t, resp.Body, stop)
}

func wireFramesNamed(frames []sseFrameRec, name string) []sseFrameRec {
	var out []sseFrameRec
	for _, f := range frames {
		if f.name == name {
			out = append(out, f)
		}
	}
	return out
}

// TestEventsKeepaliveIsAnObservableEvent is the dead-branch gate: the keepalive
// the stream writes under diff silence must reach the client's event dispatcher.
// A ": ping" comment does not — eventsource parsers drop comment lines natively
// — so a stream whose only keepalive is a comment fires NOTHING client-side.
func TestEventsKeepaliveIsAnObservableEvent(t *testing.T) {
	fs := &fakeStatus{snap: statusResponse{AsOf: time.Unix(1, 0), Backends: []backends.BackendStatus{}}}
	frames := hbStreamProbe(t, fs, 20*time.Millisecond, time.Second, 150*time.Millisecond)

	hb := wireFramesNamed(frames, "hb")
	if len(hb) == 0 {
		var comments int
		for _, f := range frames {
			if f.name == "" {
				comments++
			}
		}
		t.Fatalf("no `hb` event in the stream — the keepalive is client-invisible (%d bare comment frames, which no eventsource parser dispatches)", comments)
	}
	if hb[0].data == "" {
		t.Error("hb frame carries no data line — a payload-free heartbeat has no success stamp")
	}
}

// TestEventsHeartbeatKeepsCadenceUnderDiffSilence pins the keepalive contract:
// with the snapshot frozen (no status / backends diff for the whole run) the
// gap between consecutive written frames still stays within one ping interval.
// A heartbeat that only fires on CHANGE would leave a silent stream to the
// fronting proxy's read timeout.
func TestEventsHeartbeatKeepsCadenceUnderDiffSilence(t *testing.T) {
	const ping = 20 * time.Millisecond
	fs := &fakeStatus{snap: statusResponse{AsOf: time.Unix(1, 0), Backends: []backends.BackendStatus{}}}
	frames := hbStreamProbe(t, fs, ping, time.Second, 200*time.Millisecond)

	hb := wireFramesNamed(frames, "hb")
	if len(hb) < 3 {
		t.Fatalf("got %d hb frames over ~10 ping intervals of total diff silence, want >= 3 (a change-gated heartbeat starves)", len(hb))
	}
	// Scheduler jitter under -race is real; the gate is "roughly the interval",
	// not a hard real-time bound: 4x the interval still fails a change-gated or
	// tick-gated variant (tick is 50x ping here), which produces NO gap at all.
	const slack = 4 * ping
	for i := 1; i < len(hb); i++ {
		if gap := hb[i].at.Sub(hb[i-1].at); gap > slack {
			t.Errorf("hb gap %d→%d = %s, want <= %s (ping interval %s)", i-1, i, gap, slack, ping)
		}
	}
}

// TestEventsHeartbeatPayloadContract pins the hb payload: the success stamp
// (last_good_at), the degraded flag and the health class — and NO subs field.
// The connection count is hub-internal bookkeeping; putting it on a frame every
// admin panel receives would make one operator's session visible to the others.
func TestEventsHeartbeatPayloadContract(t *testing.T) {
	fs := &fakeStatus{snap: statusResponse{AsOf: time.Unix(1, 0), Backends: []backends.BackendStatus{}}}
	frames := hbStreamProbe(t, fs, 20*time.Millisecond, time.Second, 150*time.Millisecond)

	hb := wireFramesNamed(frames, "hb")
	if len(hb) == 0 {
		t.Fatal("no hb frame to inspect")
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(hb[0].data), &payload); err != nil {
		t.Fatalf("hb payload is not JSON (%q): %v", hb[0].data, err)
	}
	for _, k := range []string{"last_good_at", "degraded", "health"} {
		if _, ok := payload[k]; !ok {
			t.Errorf("hb payload missing %q, got %v", k, payload)
		}
	}
	if _, leaked := payload["subs"]; leaked {
		t.Errorf("hb payload carries a subs field — connection bookkeeping does not belong on a fanned-out frame: %v", payload)
	}
	if len(payload) != 3 {
		t.Errorf("hb payload has %d fields, want exactly 3 (last_good_at, degraded, health): %v", len(payload), payload)
	}
}

// TestEventsHeartbeatStampDoesNotWalkWithAsOf is the success-stamp gate on the
// wire: as_of is a "when did we last TRY" stamp — the collector advances it
// unconditionally, failed DB reads included — so a heartbeat that echoed as_of
// would report health that was never measured. last_good_at must therefore be
// its own value and must NOT move just because the snapshot's as_of moved.
func TestEventsHeartbeatStampDoesNotWalkWithAsOf(t *testing.T) {
	fs := &fakeStatus{snap: statusResponse{AsOf: time.Unix(1, 0), Backends: []backends.BackendStatus{}}}
	// Advance as_of continuously while the stream runs: with the tick short
	// enough the loop rebuilds every few ms and every rebuild moves as_of.
	stopWalk := make(chan struct{})
	go func() {
		for i := 2; ; i++ {
			select {
			case <-stopWalk:
				return
			default:
			}
			fs.set(statusResponse{AsOf: time.Unix(int64(i), 0), Backends: []backends.BackendStatus{}})
			time.Sleep(5 * time.Millisecond)
		}
	}()
	frames := hbStreamProbe(t, fs, 20*time.Millisecond, 10*time.Millisecond, 200*time.Millisecond)
	close(stopWalk)

	hb := wireFramesNamed(frames, "hb")
	if len(hb) < 2 {
		t.Fatalf("got %d hb frames, want >= 2 to compare stamps", len(hb))
	}
	stamp := func(raw string) any {
		var p map[string]any
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			t.Fatalf("hb payload not JSON (%q): %v", raw, err)
		}
		return p["last_good_at"]
	}
	first, last := stamp(hb[0].data), stamp(hb[len(hb)-1].data)
	if first != last {
		t.Errorf("last_good_at walked with as_of (%v → %v) — the heartbeat is echoing the try-stamp, not a success stamp", first, last)
	}
	// Control: the status frames DID move (otherwise the probe proves nothing).
	st := wireFramesNamed(frames, "status")
	if len(st) < 2 {
		t.Fatalf("control failed: got %d status frames, want >= 2 (as_of was supposed to walk)", len(st))
	}
}

// TestSSEWriterHBFrameFormat pins the heartbeat's exact wire bytes: a NAMED
// event (so the client's parser dispatches it) with a compact JSON payload, and
// a null — never the Unix epoch — while no tick has been measured yet.
func TestSSEWriterHBFrameFormat(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := newSSEWriter(rec)
	measured := livenessStamp{
		lastGoodAt: time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC),
		degraded:   false,
		health:     "ok",
	}
	if err := sw.hb(measured); err != nil {
		t.Fatalf("hb: %v", err)
	}
	want := "event: hb\ndata: {\"last_good_at\":\"2026-07-28T10:00:00Z\",\"degraded\":false,\"health\":\"ok\"}\n\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("hb frame:\n got %q\nwant %q", got, want)
	}

	cold := httptest.NewRecorder()
	if err := newSSEWriter(cold).hb(livenessStamp{degraded: true, health: "unknown"}); err != nil {
		t.Fatalf("hb (cold): %v", err)
	}
	wantCold := "event: hb\ndata: {\"last_good_at\":null,\"degraded\":true,\"health\":\"unknown\"}\n\n"
	if got := cold.Body.String(); got != wantCold {
		t.Errorf("cold hb frame:\n got %q\nwant %q", got, wantCold)
	}
}

// TestSSEHeartbeatOutlivesServerWriteTimeout is the deadline gate (the hb
// sibling of TestSSEWriterOutlivesServerWriteTimeout): the heartbeat must go
// through the sseWriter frame path, which rolls the connection write deadline.
// A keepalive written PAST it — straight onto the ResponseWriter — would still
// satisfy the fronting proxy and let http.Server's ABSOLUTE WriteTimeout kill
// the stream mid-flight.
func TestSSEHeartbeatOutlivesServerWriteTimeout(t *testing.T) {
	orig := sseWriteWindow
	sseWriteWindow = 400 * time.Millisecond
	defer func() { sseWriteWindow = orig }()

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sw := newSSEWriter(w)
		deadline := time.After(900 * time.Millisecond) // 3x the server WriteTimeout
		tk := time.NewTicker(50 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-deadline:
				_ = sw.event("status", "1", []byte(`{"ok":"made-it"}`))
				return
			case <-tk.C:
				// ONLY heartbeats keep this stream alive for the whole run.
				if sw.hb(livenessStamp{health: "ok"}) != nil {
					return
				}
			}
		}
	}))
	srv.Config.WriteTimeout = 300 * time.Millisecond
	srv.Start()
	defer srv.Close()

	resp, err := http.Get(srv.URL) //nolint:noctx // test client on a local httptest server
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body (connection killed by WriteTimeout — hb did not roll the deadline?): %v", err)
	}
	if !strings.Contains(string(body), "made-it") {
		t.Errorf("stream died before the final frame, got %d bytes: %q", len(body), string(body))
	}
}

// TestStatusLivenessStampHoldsOnFailedDBReads is the success-stamp gate at the
// source: as_of advances on every pass, the stamp only on a pass whose DB-backed
// reads ALL answered — the pool ping included.
func TestStatusLivenessStampHoldsOnFailedDBReads(t *testing.T) {
	c := &StatusCollector{}
	dbUp := healthResponse{Status: "ok", Services: map[string]string{"database": "ok"}}
	dbDown := healthResponse{Status: "unhealthy", Services: map[string]string{"database": "error"}}

	// Pass 1 — everything answered: the stamp is this pass's as_of.
	first := &cheapSnapshot{asOf: time.Unix(1000, 0), health: dbUp}
	c.stampLiveness(first, c.dbFails.Load())
	if first.degraded {
		t.Fatal("a pass with no failed read must not be degraded")
	}
	if !first.lastGoodAt.Equal(first.asOf) {
		t.Fatalf("measured pass: last_good_at = %v, want as_of %v", first.lastGoodAt, first.asOf)
	}
	c.cache.Store(first)

	// Pass 2 — a partial read failed (llm_24h): as_of walks on because the
	// collector always sets it; the success stamp must stand still.
	before := c.dbFails.Load()
	c.noteDBFail("status: llm_24h query failed", errors.New("probe: relation missing"))
	second := &cheapSnapshot{asOf: time.Unix(2000, 0), health: dbUp}
	c.stampLiveness(second, before)
	if !second.degraded {
		t.Error("a pass with a failed DB-backed read must be degraded")
	}
	if !second.lastGoodAt.Equal(first.lastGoodAt) {
		t.Errorf("degraded pass: last_good_at = %v, want the previous measured %v", second.lastGoodAt, first.lastGoodAt)
	}
	if second.lastGoodAt.Equal(second.asOf) {
		t.Error("last_good_at walked with as_of — that is the try-stamp, not a success stamp")
	}
	c.cache.Store(second)

	// Pass 3 — the reads answer, but the pool PING did not: still degraded, and
	// the stamp still points at pass 1.
	third := &cheapSnapshot{asOf: time.Unix(3000, 0), health: dbDown}
	c.stampLiveness(third, c.dbFails.Load())
	if !third.degraded {
		t.Error("a failed pool ping must degrade the pass even when every read returned")
	}
	if !third.lastGoodAt.Equal(first.lastGoodAt) {
		t.Errorf("ping-down pass: last_good_at = %v, want %v", third.lastGoodAt, first.lastGoodAt)
	}
	c.cache.Store(third)

	// Pass 4 — recovery: the stamp advances again, so it tracks health rather
	// than freezing permanently after the first blip.
	fourth := &cheapSnapshot{asOf: time.Unix(4000, 0), health: dbUp}
	c.stampLiveness(fourth, c.dbFails.Load())
	if fourth.degraded {
		t.Error("a recovered pass must not stay degraded")
	}
	if !fourth.lastGoodAt.Equal(fourth.asOf) {
		t.Errorf("recovered pass: last_good_at = %v, want as_of %v", fourth.lastGoodAt, fourth.asOf)
	}
}

// touchesPool runs fn on a collector wired to NOTHING and reports whether fn
// reached the database or the assembler. A nil pool / nil config / nil cache is
// dereferenced by every such path, so a recovered panic IS the access counter
// this contract needs — measured on the real call path, not on an injected fake
// that could drift from it.
func touchesPool(fn func()) (touched bool) {
	defer func() {
		if r := recover(); r != nil {
			touched = true
		}
	}()
	fn()
	return false
}

// TestStatusLivenessIsDBFreeAndAssembleFree pins the heartbeat's cost contract:
// liveness() is a pure in-memory read. It fires per connection on the ping timer
// — a cadence unrelated to the tick — so a version that queried, cold-started or
// assembled would put N connections' worth of DB work on a keepalive.
func TestStatusLivenessIsDBFreeAndAssembleFree(t *testing.T) {
	cold := &StatusCollector{} // no pool, no config, no cache
	if touchesPool(func() { _ = cold.liveness() }) {
		t.Fatal("liveness() reached the pool / assemble on a cold collector — it must never leave memory")
	}
	// Control: the detector fires on the shapes liveness must NOT take — the
	// snapshot path (a DB build) and assemble (a nil-snapshot deref). Without
	// this the probe above could pass by accident.
	if !touchesPool(func() { _ = cold.Snapshot(context.Background()) }) {
		t.Error("control: Snapshot(ctx) did not register as an access — the probe cannot detect a violation")
	}
	if !touchesPool(func() { _ = cold.assemble(cold.cache.Load(), nil, nil) }) {
		t.Error("control: assemble() did not register as an access — the probe cannot detect a violation")
	}
	// A cold collector has no measured stand: degraded + unknown, never "ok".
	if got := cold.liveness(); !got.degraded || got.health != "unknown" || !got.lastGoodAt.IsZero() {
		t.Errorf("cold liveness = %+v, want degraded=true health=unknown zero-stamp", got)
	}

	// Warm cache: the stamp is served verbatim from the cached stand — and it is
	// the SUCCESS stamp, not as_of.
	warm := &StatusCollector{}
	warm.cache.Store(&cheapSnapshot{
		asOf:       time.Unix(2000, 0),
		lastGoodAt: time.Unix(1000, 0),
		degraded:   true,
		health:     healthResponse{Status: "degraded"},
	})
	got := warm.liveness()
	if got.lastGoodAt.Equal(time.Unix(2000, 0)) {
		t.Error("liveness() echoed as_of instead of the success stamp")
	}
	if !got.lastGoodAt.Equal(time.Unix(1000, 0)) || !got.degraded || got.health != "degraded" {
		t.Errorf("warm liveness = %+v, want {1000, degraded, \"degraded\"}", got)
	}
}
