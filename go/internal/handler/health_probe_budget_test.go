package handler

// Issue #504, review of the linked pull request: the probes of /health end
// with the request that asked for them, and their budget ends before the
// compose healthcheck gives up.
//
// A /health that kept its probes past its client let every anonymous request
// hold a probe of each hanging backend for the whole budget: 2,000 requests in
// a second whose clients left after 20 ms held about 10,000 goroutines and
// 2,000 connections to the backend for five seconds. And a budget equal to the
// compose healthcheck's timeout (5 s) let Docker give up first while a chat
// backend hung, so the container went unhealthy over a backend, not over its
// database. A probe that a client abandoned is neither an outage nor a
// recovery: it logs nothing above DEBUG and changes no state.

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/jackc/pgx/v5/pgxpool"
)

// hangingHost accepts connections and never answers; open counts the
// connections that are open.
type hangingHost struct {
	l    net.Listener
	open atomic.Int64
}

func newHangingHost(t *testing.T) *hangingHost {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h := &hangingHost{l: l}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			h.open.Add(1)
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

func (h *hangingHost) url() string { return "http://" + h.l.Addr().String() }

// healthServer serves /health over snap (Dream on, the database refusing).
func healthServer(t *testing.T, snap []backends.Backend) *HealthHandler {
	t.Helper()
	st := &swapStore{}
	st.p.Store(healthTestConfig())
	bp := backends.NewPool(nil, nil)
	bp.SeedSnapshotForTest(snap)
	return NewHealthHandler(brokenPool(t), st, bp, nil)
}

// TestHealthProbesEndWithTheirClient — 200 /health requests at once while the
// chat backend hangs, each client gone after 100 ms: within half a second
// after the last client left, the process is back at the goroutines it had
// before and no connection to the backend is open.
func TestHealthProbesEndWithTheirClient(t *testing.T) {
	const (
		requests = 200
		giveUp   = 100 * time.Millisecond
		settle   = 500 * time.Millisecond
		// slack covers the goroutines of the servers' connections that are
		// still closing and those of the package's other tests: the count is
		// process-wide, and the CI runner's two cores were one goroutine over
		// a slack of 20. Probes that outlive their requests keep about five
		// goroutines per request (1,014 for these 200), so half a goroutine
		// per request still tells the two apart; the connections open at the
		// hanging backend are the exact measure.
		slack = requests / 2
	)
	// The probes' transport (http.DefaultTransport) keeps up to two idle
	// connections per host, also one it finished dialing for a request that
	// was cancelled meanwhile: bounded, no probe. They are closed before
	// counting; a probe in flight keeps its connection busy, not idle.
	idle := func() { http.DefaultTransport.(*http.Transport).CloseIdleConnections() }
	hang := newHangingHost(t)
	up := slowBackend(t, 0, nil)
	h := healthServer(t, []backends.Backend{
		operatorRow("i504-storm-e", up, backends.RoleEmbed, backends.RoleDream),
		operatorRow("i504-storm-c", hang.url(), backends.RoleSynthesis),
	})
	srv := httptest.NewServer(http.HandlerFunc(h.Health))
	t.Cleanup(srv.Close)
	installRejectLogCapture(t)
	time.Sleep(50 * time.Millisecond)
	idle()
	base := runtime.NumGoroutine()

	tr := &http.Transport{MaxIdleConnsPerHost: requests}
	t.Cleanup(tr.CloseIdleConnections)
	cl := &http.Client{Transport: tr}
	var wg sync.WaitGroup
	for range requests {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), giveUp)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
			if resp, err := cl.Do(req); err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
		})
	}
	wg.Wait()
	deadline := time.Now().Add(settle)
	idle()
	goroutines, open := runtime.NumGoroutine(), hang.open.Load()
	for (goroutines > base+slack || open != 0) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		idle()
		goroutines, open = runtime.NumGoroutine(), hang.open.Load()
	}
	t.Logf("%v after the last of %d clients left: %d goroutines (%d before), %d connections open at the hanging backend",
		settle, requests, goroutines, base, open)
	if goroutines > base+slack || open != 0 {
		t.Errorf("%v after the clients left: %d goroutines (%d before) and %d connections to the hanging backend, want at most %d and 0 — the probes outlived their requests",
			settle, goroutines, base, open, base+slack)
	}
}

// composeHealthcheckTimeout reads the timeout of the ctx service's
// healthcheck (`/ctx -health`) from the repository's docker-compose.yml.
func composeHealthcheckTimeout(t *testing.T) time.Duration {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "..", "docker-compose.yml"))
	if err != nil {
		t.Fatalf("docker-compose.yml: %v", err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	inProbe := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "test:") && strings.Contains(line, `"/ctx", "-health"`):
			inProbe = true
		case inProbe && strings.HasPrefix(line, "timeout:"):
			d, err := time.ParseDuration(strings.TrimSpace(strings.TrimPrefix(line, "timeout:")))
			if err != nil {
				t.Fatalf("healthcheck timeout %q: %v", line, err)
			}
			return d
		case inProbe && !strings.Contains(line, ":"):
			inProbe = false
		}
	}
	t.Fatal("docker-compose.yml: no timeout for the /ctx -health healthcheck")
	return 0
}

// TestHealthAnswersBeforeTheComposeHealthcheckGivesUp — the compose
// healthcheck against a chat backend that hangs: its clock starts before its
// request (here 50 ms), and /health still answers within it, so Docker reads
// the health document — whose database verdict alone decides — instead of a
// timeout. The probe that ran out of its budget found a backend that did not
// answer in time: that is an outage, logged as one.
func TestHealthAnswersBeforeTheComposeHealthcheckGivesUp(t *testing.T) {
	const clockLead = 50 * time.Millisecond
	timeout := composeHealthcheckTimeout(t)
	up := slowBackend(t, 0, nil)
	hanging := slowBackend(t, time.Hour, nil)
	// A known state first: the backend answers.
	healthCalls(t, []backends.Backend{operatorRow("i504-clock", up, backends.RoleSynthesis)}, 3)
	h := healthServer(t, []backends.Backend{
		operatorRow("i504-clock-e", up, backends.RoleEmbed, backends.RoleDream),
		operatorRow("i504-clock", hanging, backends.RoleSynthesis),
	})
	srv := httptest.NewServer(http.HandlerFunc(h.Health))
	t.Cleanup(srv.Close)
	logs := installRejectLogCapture(t)

	start := time.Now()
	time.Sleep(clockLead)
	cl := &http.Client{Timeout: timeout - clockLead}
	resp, err := cl.Get(srv.URL)
	if err != nil {
		t.Fatalf("the healthcheck's %v ran out after %v: %v — Docker reads a timeout, not the health document", timeout, time.Since(start), err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var doc struct{ Services map[string]string }
	if err := json.Unmarshal(body, &doc); err != nil || doc.Services["chat"] != "error" {
		t.Fatalf("health document %s (%v), want one with chat=error", body, err)
	}
	t.Logf("the health document arrived %v into the healthcheck's %v clock", time.Since(start).Round(time.Millisecond), timeout)
	wantOneLine(t, logs, backends.RoleSynthesis, "row-i504-clock", slog.LevelWarn, "backend ping failed", 0)
}

// switchableBackend serves GET / by mode: 200, 503, or no answer until the
// request ends.
func switchableBackend(t *testing.T, mode *atomic.Int32) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode.Load() {
		case backendDown:
			w.WriteHeader(http.StatusServiceUnavailable)
		case backendHangs:
			<-r.Context().Done()
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

const (
	backendUp int32 = iota
	backendDown
	backendHangs
)

// healthLinesAbove returns the /health records at INFO and above.
func healthLinesAbove(logs *rejectLogCapture) []rejectLogRecord {
	var out []rejectLogRecord
	for _, r := range logs.all() {
		if r.level >= slog.LevelInfo && strings.HasPrefix(r.msg, "health check") {
			out = append(out, r)
		}
	}
	return out
}

// TestHealthClientThatLeavesFirstChangesNothing — a client that leaves before
// the budget ends (`ctx statusline` gives up after 500 ms) ends the probes of
// its request at once. What they did not finish is neither an outage nor a
// recovery: no WARN, no INFO, and an outage already known stays known.
func TestHealthClientThatLeavesFirstChangesNothing(t *testing.T) {
	const leaveAfter = 50 * time.Millisecond
	var mode atomic.Int32
	up := slowBackend(t, 0, nil)
	snap := []backends.Backend{
		operatorRow("i504-early-e", up, backends.RoleEmbed, backends.RoleDream),
		operatorRow("i504-early", switchableBackend(t, &mode), backends.RoleSynthesis),
	}
	h := healthServer(t, snap)
	early := func(t *testing.T) {
		t.Helper()
		logs := installRejectLogCapture(t)
		for range 3 {
			req := httptest.NewRequest(http.MethodGet, "/health", nil)
			ctx, cancel := context.WithCancel(req.Context())
			time.AfterFunc(leaveAfter, cancel)
			start := time.Now()
			h.Health(httptest.NewRecorder(), req.WithContext(ctx))
			cancel()
			if took := time.Since(start); took > leaveAfter+time.Second {
				t.Errorf("/health answered %v after its client left at %v — its probes outlived the request", took, leaveAfter)
			}
		}
		if lines := healthLinesAbove(logs); len(lines) != 0 {
			t.Errorf("abandoned probes wrote %+v, want nothing above DEBUG", lines)
		}
	}

	healthCalls(t, snap, 3) // a known state: the backend answers, the role is served
	t.Run("an answering backend that hangs while the client leaves", func(t *testing.T) {
		mode.Store(backendHangs)
		early(t)
	})
	t.Run("a known outage stays known", func(t *testing.T) {
		mode.Store(backendDown)
		logs := healthCalls(t, snap, 1)
		wantOneLine(t, logs, backends.RoleSynthesis, "row-i504-early", slog.LevelWarn, "backend ping failed", 0)
		mode.Store(backendHangs)
		early(t)
		mode.Store(backendDown)
		logs = healthCalls(t, snap, 1)
		if lines := healthRoleLines(logs, slog.LevelWarn, backends.RoleSynthesis, "row-i504-early"); len(lines) != 0 {
			t.Errorf("the outage was forgotten while clients left early: %+v", lines)
		}
		if lines := healthRoleLines(logs, slog.LevelDebug, backends.RoleSynthesis, "row-i504-early"); len(lines) != 1 {
			t.Errorf("%d DEBUG lines for the backend still down, want 1", len(lines))
		}
	})
	t.Run("an answer count in progress survives a client that leaves", func(t *testing.T) {
		mode.Store(backendDown)
		healthCalls(t, snap, 1) // down (known or new)
		mode.Store(backendUp)
		healthCalls(t, snap, 2) // two answers in a row, not yet back
		mode.Store(backendHangs)
		early(t)
		mode.Store(backendUp)
		logs := healthCalls(t, snap, 1)
		wantOneLine(t, logs, backends.RoleSynthesis, "row-i504-early", slog.LevelInfo, "answers again", 0)
	})
	t.Run("a database that hangs while the client leaves", func(t *testing.T) {
		mode.Store(backendUp)
		healthCalls(t, snap, 3) // the backend answers again (the INFO comes here)
		db := newHangingHost(t)
		pool, err := pgxpool.New(context.Background(), "postgres://test:test@"+db.l.Addr().String()+"/test?sslmode=disable")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		st := &swapStore{}
		st.p.Store(healthTestConfig())
		bp := backends.NewPool(nil, nil)
		bp.SeedSnapshotForTest(snap)
		h = NewHealthHandler(pool, st, bp, nil)
		early(t)
	})
}
