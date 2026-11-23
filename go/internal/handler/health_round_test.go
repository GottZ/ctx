package handler

// Issue #519, the /health follow-ups of #504: what a probe observes no longer
// depends on how long its caller waits.
//
//   - A probe counts as an answer only once it has read the answer: a backend
//     that sends its 200 headers and then holds its body is unreachable, and
//     a body read that ends early is no answer.
//   - A backend that hangs is found down even when every caller leaves before
//     the budget ends (load-balancer probes with a short timeout), and a
//     backend that answers within the budget is never reported down because
//     its callers were impatient.
//   - The candidates of a role share the budget, so a later candidate is
//     probed with its share instead of with a budget an earlier candidate
//     used up.
//   - A reload in the middle of a probe round logs each change once: what a
//     round found after a newer round applied its findings is dropped.
//
// The tests go through the production handler (HealthHandler.Health) and
// count lines, requests at the stubs and open connections — never the
// process's goroutines or the wall clock.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/pgxdb"
)

// setHealthBudget sets healthProbeBudget for the test.
func setHealthBudget(t *testing.T, d time.Duration) {
	t.Helper()
	prev := healthProbeBudget
	healthProbeBudget = d
	t.Cleanup(func() { healthProbeBudget = prev })
}

// freshHealthNotice gives the test a healthNotice of its own, as a new
// process has: the first failure it observes is a change of state. Rounds of
// the test finish before the old notice comes back.
func freshHealthNotice(t *testing.T) {
	t.Helper()
	prev := healthLines
	healthLines = &healthNotice{}
	t.Cleanup(func() {
		waitHealthRounds(t)
		healthLines = prev
	})
}

// seededBackends is a backend pool whose snapshot is snap.
func seededBackends(snap ...backends.Backend) *backends.Pool {
	bp := backends.NewPool(nil, nil)
	bp.SeedSnapshotForTest(snap)
	return bp
}

// roundHandler serves /health over bp (Dream on) with the database pool db.
func roundHandler(db *pgxpool.Pool, bp *backends.Pool) *HealthHandler {
	st := &swapStore{}
	st.p.Store(healthTestConfig())
	return NewHealthHandler(db, st, bp, nil)
}

// askHealth sends one /health request through h. With leaveAfter > 0 its
// client goes away after that long. It returns the services of the answer,
// nil when the client left before it.
func askHealth(h *HealthHandler, leaveAfter time.Duration) map[string]string {
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()
	if leaveAfter > 0 {
		timer := time.AfterFunc(leaveAfter, cancel)
		defer timer.Stop()
	}
	rec := httptest.NewRecorder()
	h.Health(rec, req.WithContext(ctx))
	if ctx.Err() != nil {
		return nil
	}
	var doc struct{ Services map[string]string }
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		return nil
	}
	return doc.Services
}

// linesAt returns the /health records of role at level; backend "" selects
// the role's own lines (no backend attribute), otherwise that backend's.
func linesAt(logs *rejectLogCapture, level slog.Level, role, backend string) []rejectLogRecord {
	var out []rejectLogRecord
	for _, r := range logs.all() {
		if r.level != level || !strings.HasPrefix(r.msg, "health check") || r.attrs["role"] != role {
			continue
		}
		b, has := r.attrs["backend"]
		if (backend == "" && has) || (backend != "" && b != backend) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// wantLines checks that role (backend "" for the role's own lines) logged
// exactly warn WARN and info INFO records and nothing at ERROR.
func wantLines(t *testing.T, logs *rejectLogCapture, role, backend string, warn, info int) {
	t.Helper()
	w, i, e := linesAt(logs, slog.LevelWarn, role, backend), linesAt(logs, slog.LevelInfo, role, backend), linesAt(logs, slog.LevelError, role, backend)
	if len(w) != warn || len(i) != info || len(e) != 0 {
		t.Errorf("role %s backend %q: %d WARN %+v, %d INFO %+v, %d ERROR, want %d WARN, %d INFO, no ERROR",
			role, backend, len(w), w, len(i), i, len(e), warn, info)
	}
}

// bodyHangBackend serves GET / by mode: 200 with a body, 503, or — for
// backendHangs — 200 headers flushed at once and a body that never comes
// until the request ends.
func bodyHangBackend(t *testing.T, mode *atomic.Int32) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode.Load() {
		case backendDown:
			w.WriteHeader(http.StatusServiceUnavailable)
		case backendHangs:
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		default:
			_, _ = io.WriteString(w, "ok")
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestHealthProbeCountsOnlyAnAnswerItRead — a backend that sends its 200
// headers and then holds its body is unreachable (before: the body read ran
// into the budget, its error was dropped and the probe counted as an answer),
// and three body reads that end early do not make a backend that was down
// "answer again" (before: they did, also when the caller left).
func TestHealthProbeCountsOnlyAnAnswerItRead(t *testing.T) {
	setHealthBudget(t, 300*time.Millisecond)
	var mode atomic.Int32
	up := slowBackend(t, 0, nil)
	chat := operatorRow("i519-body", bodyHangBackend(t, &mode), backends.RoleSynthesis)
	h := roundHandler(brokenPool(t), seededBackends(operatorRow("i519-body-e", up, backends.RoleEmbed, backends.RoleDream), chat))
	t.Cleanup(func() { waitHealthRounds(t) })
	for range recoveryAnswers {
		askHealth(h, 0) // a known state: the backend answers, the role is served
	}

	t.Run("headers, then a body that never ends", func(t *testing.T) {
		mode.Store(backendHangs)
		logs := installRejectLogCapture(t)
		got := askHealth(h, 0)
		waitHealthRounds(t)
		if got["chat"] != "error" {
			t.Errorf("services = %v, want chat=error — a probe that never read its answer counted as one", got)
		}
		wantLines(t, logs, backends.RoleSynthesis, chat.Name, 1, 0)
	})

	t.Run("body reads that end early are no answer", func(t *testing.T) {
		mode.Store(backendDown)
		askHealth(h, 0) // known down
		mode.Store(backendHangs)
		logs := installRejectLogCapture(t)
		for range recoveryAnswers {
			askHealth(h, 50*time.Millisecond)
			waitHealthRounds(t)
		}
		wantLines(t, logs, backends.RoleSynthesis, chat.Name, 0, 0)
		wantLines(t, logs, backends.RoleSynthesis, "", 0, 0)
	})
}

// TestHealthFindsAHangingBackendWhenEveryCallerLeavesFirst — every caller
// leaves after 50 ms, long before the budget (a load balancer's probe with a
// short timeout): the hanging backend is still found down, with one WARN for
// it and one for the role it alone serves (before: DEBUG only, forever).
func TestHealthFindsAHangingBackendWhenEveryCallerLeavesFirst(t *testing.T) {
	setHealthBudget(t, 300*time.Millisecond)
	var mode atomic.Int32
	up := slowBackend(t, 0, nil)
	chat := operatorRow("i519-impatient", switchableBackend(t, &mode), backends.RoleSynthesis)
	h := roundHandler(brokenPool(t), seededBackends(operatorRow("i519-impatient-e", up, backends.RoleEmbed, backends.RoleDream), chat))
	t.Cleanup(func() { waitHealthRounds(t) })
	for range recoveryAnswers {
		askHealth(h, 0) // a known state: served
	}
	mode.Store(backendHangs)
	logs := installRejectLogCapture(t)
	for range 6 {
		askHealth(h, 50*time.Millisecond)
	}
	waitHealthRounds(t)
	wantLines(t, logs, backends.RoleSynthesis, chat.Name, 1, 0)
	wantLines(t, logs, backends.RoleSynthesis, "", 1, 0)
}

// TestHealthSlowBackendWithImpatientCallersWritesNothing — the other side of
// the test above: a backend that answers within the budget (after 150 ms)
// while every caller leaves after 30 ms is never reported down. A rule that
// declared a backend down once no probe of it completed for a while would
// write a WARN here.
func TestHealthSlowBackendWithImpatientCallersWritesNothing(t *testing.T) {
	setHealthBudget(t, time.Second)
	up := slowBackend(t, 0, nil)
	chat := operatorRow("i519-slow", slowBackend(t, 150*time.Millisecond, nil), backends.RoleSynthesis)
	h := roundHandler(brokenPool(t), seededBackends(operatorRow("i519-slow-e", up, backends.RoleEmbed, backends.RoleDream), chat))
	t.Cleanup(func() { waitHealthRounds(t) })
	for range recoveryAnswers {
		askHealth(h, 0) // a known state: served
	}
	logs := installRejectLogCapture(t)
	for range 20 {
		askHealth(h, 30*time.Millisecond)
	}
	waitHealthRounds(t)
	if lines := healthLinesAbove(logs); len(lines) != 0 {
		t.Errorf("a backend answering within the budget, callers leaving first: %+v, want no line above DEBUG", lines)
	}
}

// TestHealthLaterCandidateGetsItsShareOfTheBudget — the first chat candidate
// hangs: the second is still requested, answers, and serves the role (before:
// it was "probed" with the budget already spent, logged as failed and never
// requested).
func TestHealthLaterCandidateGetsItsShareOfTheBudget(t *testing.T) {
	setHealthBudget(t, 400*time.Millisecond)
	var second atomic.Int32
	up := slowBackend(t, 0, nil)
	first := operatorRow("i519-share-1", slowBackend(t, time.Hour, nil), backends.RoleSynthesis)
	next := operatorRow("i519-share-2", slowBackend(t, 0, &second), backends.RoleSynthesis)
	h := roundHandler(brokenPool(t), seededBackends(operatorRow("i519-share-e", up, backends.RoleEmbed, backends.RoleDream), first, next))
	t.Cleanup(func() { waitHealthRounds(t) })
	logs := installRejectLogCapture(t)
	got := askHealth(h, 0)
	waitHealthRounds(t)
	if got["chat"] != "ok" {
		t.Errorf("services = %v, want chat=ok — the second candidate answers", got)
	}
	if n := second.Load(); n != 1 {
		t.Errorf("the second candidate saw %d requests, want 1", n)
	}
	wantLines(t, logs, backends.RoleSynthesis, next.Name, 0, 0)
	wantLines(t, logs, backends.RoleSynthesis, first.Name, 1, 0)
}

// gateBackend answers 200 — while hold is set, only once hold is cleared —
// and counts its requests.
func gateBackend(t *testing.T, hold *atomic.Bool, hits *atomic.Int32) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		for hold.Load() {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// reloadFixture is a pool whose chat role moves from x (answering, held at
// its gate on demand) to y (refusing) with a reload.
type reloadFixture struct {
	h      *HealthHandler
	bp     *backends.Pool
	s2     []backends.Backend
	x, y   backends.Backend
	hold   atomic.Bool
	xHits  atomic.Int32
	before int32
}

func newReloadFixture(t *testing.T, id string) *reloadFixture {
	t.Helper()
	f := &reloadFixture{}
	up := slowBackend(t, 0, nil)
	e := operatorRow(id+"-e", up, backends.RoleEmbed, backends.RoleDream)
	f.x = operatorRow(id+"-x", gateBackend(t, &f.hold, &f.xHits), backends.RoleSynthesis)
	f.y = operatorRow(id+"-y", closedPortHost(t), backends.RoleSynthesis)
	f.s2 = []backends.Backend{e, f.y}
	f.bp = seededBackends(e, f.x)
	f.h = roundHandler(brokenPool(t), f.bp)
	t.Cleanup(func() {
		f.hold.Store(false)
		waitHealthRounds(t)
	})
	for range recoveryAnswers {
		askHealth(f.h, 0) // a known state: x serves the role
	}
	f.hold.Store(true)
	f.before = f.xHits.Load()
	return f
}

// TestHealthReloadDuringARoundLogsEachChangeOnce — a round on the old
// snapshot is held at x's gate while the reload moves the chat role to y and
// a round on the new snapshot finds y refusing; the old round ends after
// that, and one more request follows. y's outage and the role's are each one
// WARN (before: the old round forgot y's outage, so the next request logged
// it again).
func TestHealthReloadDuringARoundLogsEachChangeOnce(t *testing.T) {
	f := newReloadFixture(t, "i519-reload")
	logs := installRejectLogCapture(t)
	old := make(chan struct{})
	go func() {
		defer close(old)
		askHealth(f.h, 0)
	}()
	waitUntil(t, 20*time.Second, "the old round at x's gate", func() bool { return f.xHits.Load() > f.before })
	f.bp.SeedSnapshotForTest(f.s2) // the reload
	askHealth(f.h, 0)              // a round on the new snapshot: y refuses
	f.hold.Store(false)
	<-old            // the old round ends after the new one
	askHealth(f.h, 0) // y still refuses
	waitHealthRounds(t)
	wantLines(t, logs, backends.RoleSynthesis, f.y.Name, 1, 0)
	wantLines(t, logs, backends.RoleSynthesis, "", 1, 0)
	wantLines(t, logs, backends.RoleSynthesis, f.x.Name, 0, 0)
}

// TestHealthReloadAmidConcurrentRequestsLogsEachChangeOnce — the stress form:
// 32 requests on the old snapshot held at x's gate, the reload, 32 requests on
// the new one, the gate opens, three more requests. Each change is one line,
// however the requests interleave (before: 3–6 WARN and 3–5 INFO of a role
// per run).
func TestHealthReloadAmidConcurrentRequestsLogsEachChangeOnce(t *testing.T) {
	f := newReloadFixture(t, "i519-storm")
	logs := installRejectLogCapture(t)
	var olds, news sync.WaitGroup
	for range 32 {
		olds.Go(func() { askHealth(f.h, 0) })
	}
	waitUntil(t, 20*time.Second, "a round at x's gate", func() bool { return f.xHits.Load() > f.before })
	time.Sleep(20 * time.Millisecond) // let the other old requests reach the handler
	f.bp.SeedSnapshotForTest(f.s2)
	for range 32 {
		news.Go(func() { askHealth(f.h, 0) })
	}
	news.Wait()
	f.hold.Store(false)
	olds.Wait()
	for range 3 {
		askHealth(f.h, 0)
	}
	waitHealthRounds(t)
	wantLines(t, logs, backends.RoleSynthesis, f.y.Name, 1, 0)
	wantLines(t, logs, backends.RoleSynthesis, "", 1, 0)
	wantLines(t, logs, backends.RoleSynthesis, f.x.Name, 0, 0)
}

// countingHangHost accepts connections and never answers; it counts the
// connections it accepted, those open now and the most open at once.
type countingHangHost struct {
	l                    net.Listener
	accepted, open, peak atomic.Int64
}

func newCountingHangHost(t *testing.T) *countingHangHost {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h := &countingHangHost{l: l}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			h.accepted.Add(1)
			n := h.open.Add(1)
			for p := h.peak.Load(); n > p && !h.peak.CompareAndSwap(p, n); p = h.peak.Load() {
			}
			go func() {
				_, _ = io.Copy(io.Discard, c) // until the peer closes
				h.open.Add(-1)
				_ = c.Close()
			}()
		}
	}()
	t.Cleanup(func() { _ = l.Close() })
	return h
}

// hangingDatabasePool is a pool built the way ctxd builds its pool — the DSN
// from config.Config.DSN, MaxConns 20, the acquire-wait tracer — against db,
// without the boot ping and without idle connections kept open.
func hangingDatabasePool(t *testing.T, db *countingHangHost) *pgxpool.Pool {
	t.Helper()
	host, port, _ := net.SplitHostPort(db.l.Addr().String())
	p, _ := strconv.Atoi(port)
	cfg := &config.Config{Server: config.ServerConfig{DBUser: "u", DBPass: "p", DBHost: host, DBPort: p, DB: "d", DBSSL: "disable"}}
	pc, err := pgxpool.ParseConfig(cfg.DSN())
	if err != nil {
		t.Fatal(err)
	}
	pc.MaxConns, pc.MinConns = 20, 0
	pc.ConnConfig.Tracer = pgxdb.AcquireWaitTracer{}
	pool, err := pgxpool.NewWithConfig(context.Background(), pc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestHealthStormAgainstAHangingDatabase — 200 /health requests over half a
// second against a database that accepts and never answers, each client gone
// after 20 ms: at most one connection attempt is open at the database (before:
// MaxConns, 20), no request line is a WARN with waited_for=db_connection (before:
// one per request), and the outage is one ERROR once the budget ends (before:
// none — every ping was abandoned).
func TestHealthStormAgainstAHangingDatabase(t *testing.T) {
	setHealthBudget(t, 2*time.Second)
	freshHealthNotice(t) // the database is not known down yet
	db := newCountingHangHost(t)
	up := slowBackend(t, 0, nil)
	h := roundHandler(hangingDatabasePool(t, db), seededBackends(operatorRow("i519-db", up, i504Roles...)))
	t.Cleanup(func() { waitHealthRounds(t) }) // after srv.Close: a handler that came late may start a round
	// A client gone after 20 ms may give up before its request reached the
	// server; the requests that did are counted around the logger.
	var arrived, logged atomic.Int64
	logger := Logger(http.HandlerFunc(h.Health))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived.Add(1)
		logger.ServeHTTP(w, r)
		logged.Add(1)
	}))
	t.Cleanup(srv.Close)
	logs := installRejectLogCapture(t)

	var wg sync.WaitGroup
	for i := range 200 {
		wg.Go(func() {
			time.Sleep(time.Duration(i) * 2500 * time.Microsecond)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
			if resp, err := http.DefaultClient.Do(req); err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
		})
	}
	wg.Wait()
	waitHealthRounds(t)
	waitUntil(t, 20*time.Second, "the request lines", func() bool { return logged.Load() == arrived.Load() })
	if n := arrived.Load(); n < 100 {
		t.Fatalf("only %d of 200 requests reached the server — the storm proves nothing", n)
	}

	waited := 0
	for _, r := range logs.withMsg("http request") {
		if r.attrs["waited_for"] == "db_connection" {
			waited++
		}
	}
	errs := 0
	for _, r := range logs.all() {
		if r.msg == "health check: database ping failed" && r.level == slog.LevelError {
			errs++
		}
	}
	t.Logf("200 requests, %d reached the server: %d request lines with waited_for=db_connection, %d database ERROR lines, %d connection attempts accepted by the database, %d open at most",
		arrived.Load(), waited, errs, db.accepted.Load(), db.peak.Load())
	if waited != 0 {
		t.Errorf("%d request lines with waited_for=db_connection, want 0 — one per request is no change of state", waited)
	}
	if errs != 1 {
		t.Errorf("%d database ERROR lines, want 1", errs)
	}
	if p := db.peak.Load(); p > 1 {
		t.Errorf("%d connection attempts open at the database at once, want at most 1", p)
	}
}

// TestHealthDatabaseAttemptEndsAtTheConnectTimeout — the connection attempt a
// /health round left open at a database that never answers ends at the
// default connect timeout (config.DefaultDBConnectTimeout); before, the DSN
// set none and the attempt held its pool slot for good.
func TestHealthDatabaseAttemptEndsAtTheConnectTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the connect timeout")
	}
	// The default is what this test waits for: an operator's PGCONNECT_TIMEOUT
	// in the test process (0 = no timeout) would keep the attempt open.
	t.Setenv("PGCONNECT_TIMEOUT", "")
	setHealthBudget(t, 300*time.Millisecond)
	db := newCountingHangHost(t)
	up := slowBackend(t, 0, nil)
	h := roundHandler(hangingDatabasePool(t, db), seededBackends(operatorRow("i519-db-timeout", up, i504Roles...)))
	t.Cleanup(func() { waitHealthRounds(t) })
	installRejectLogCapture(t)
	if got := askHealth(h, 0); got["database"] != "error" {
		t.Fatalf("services = %v, want database=error", got)
	}
	waitUntil(t, 20*time.Second, "the attempt at the database", func() bool { return db.accepted.Load() == 1 })
	guard := time.Now().Add(defaultDBConnectTimeoutForTest + 5*time.Second)
	for db.open.Load() != 0 && time.Now().Before(guard) {
		time.Sleep(50 * time.Millisecond)
	}
	if n := db.open.Load(); n != 0 {
		t.Errorf("%d connection attempts still open %v after the round, want 0 — the attempt has no connect timeout", n, defaultDBConnectTimeoutForTest+5*time.Second)
	}
}

// TestHealthSharedBaseURLKeepsEachRolesShare — y serves embed and chat and
// hangs, z serves chat and answers: whichever role requests y first, chat
// waits for y only within its own share and then requests z, which serves
// it; embed, whose only candidate y is, is judged by its whole budget (the
// review of the linked pull request: chat waited for embed's probe of y and
// had no budget left for z).
func TestHealthSharedBaseURLKeepsEachRolesShare(t *testing.T) {
	setHealthBudget(t, 400*time.Millisecond)
	var zHits atomic.Int32
	y := operatorRow("i519-shared-y", slowBackend(t, time.Hour, nil), backends.RoleEmbed, backends.RoleSynthesis)
	z := operatorRow("i519-shared-z", slowBackend(t, 0, &zHits), backends.RoleSynthesis)
	d := operatorRow("i519-shared-d", slowBackend(t, 0, nil), backends.RoleDream)
	h := roundHandler(brokenPool(t), seededBackends(y, z, d))
	t.Cleanup(func() { waitHealthRounds(t) })
	logs := installRejectLogCapture(t)
	const rounds = 10
	for i := range rounds {
		got := askHealth(h, 0)
		if got["chat"] != "ok" || got["embed"] != "error" {
			t.Errorf("round %d: services %v, want chat ok (z serves it) and embed error (y hangs)", i, got)
		}
	}
	waitHealthRounds(t)
	if n := zHits.Load(); n != rounds {
		t.Errorf("z saw %d requests in %d rounds, want one per round", n, rounds)
	}
	wantLines(t, logs, backends.RoleSynthesis, z.Name, 0, 0)
	if w := linesAt(logs, slog.LevelWarn, backends.RoleEmbed, y.Name); len(w) != 1 {
		t.Errorf("y for embed: %d WARN %+v, want 1", len(w), w)
	}
}

// TestHealthCallerThatLeavesReturnsBeforeItsRound — a client that leaves
// gets no answer and its request returns while its round still runs: the
// request does not wait for the round (review of the linked pull request).
func TestHealthCallerThatLeavesReturnsBeforeItsRound(t *testing.T) {
	setHealthBudget(t, 2*time.Second)
	up := slowBackend(t, 0, nil)
	chat := operatorRow("i519-leaves", slowBackend(t, time.Hour, nil), backends.RoleSynthesis)
	h := roundHandler(brokenPool(t), seededBackends(operatorRow("i519-leaves-e", up, backends.RoleEmbed, backends.RoleDream), chat))
	t.Cleanup(func() { waitHealthRounds(t) })
	installRejectLogCapture(t)
	got := askHealth(h, 50*time.Millisecond)
	healthLines.mu.Lock()
	inFlight := len(healthLines.rounds)
	healthLines.mu.Unlock()
	if got != nil || inFlight != 1 {
		t.Errorf("a client gone after 50 ms: answer %v, %d rounds in flight when its request returned, want no answer and its round still running", got, inFlight)
	}
}

// TestHealthSharedBaseURLSlowWithinTheLongerShare — y answers after 150 ms
// and serves embed (its only candidate, the whole 400 ms) and chat (the
// first of three, a third of it): when chat's shorter share ended its probe
// of y, embed probes y once more with the rest of its own and finds it up;
// y is never logged down for embed, and chat is served either way.
func TestHealthSharedBaseURLSlowWithinTheLongerShare(t *testing.T) {
	setHealthBudget(t, 400*time.Millisecond)
	y := operatorRow("i519-slowshare-y", slowBackend(t, 150*time.Millisecond, nil), backends.RoleEmbed, backends.RoleSynthesis)
	z1 := operatorRow("i519-slowshare-z1", slowBackend(t, 0, nil), backends.RoleSynthesis)
	z2 := operatorRow("i519-slowshare-z2", slowBackend(t, 0, nil), backends.RoleSynthesis)
	d := operatorRow("i519-slowshare-d", slowBackend(t, 0, nil), backends.RoleDream)
	h := roundHandler(brokenPool(t), seededBackends(y, z1, z2, d))
	t.Cleanup(func() { waitHealthRounds(t) })
	logs := installRejectLogCapture(t)
	for i := range 10 {
		if got := askHealth(h, 0); got["embed"] != "ok" || got["chat"] != "ok" {
			t.Errorf("round %d: services %v, want embed ok (y answers within embed's budget) and chat ok", i, got)
		}
	}
	waitHealthRounds(t)
	wantLines(t, logs, backends.RoleEmbed, y.Name, 0, 0)
}
