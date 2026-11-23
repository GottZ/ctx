package handler

// Issue #504, review of the linked pull request, and issue #519: what the
// probes of /health cost does not grow with its requests, and their budget
// ends before the compose healthcheck gives up.
//
// A /health that kept one probe per request past its client let every
// anonymous request hold a probe of each hanging backend for the whole
// budget: 2,000 requests in a second whose clients left after 20 ms held
// about 10,000 goroutines and 2,000 connections to the backend for five
// seconds. #504 ended the probes with their request — and with it, a backend
// that hung while every caller left first was never found down (#519). Now
// the probes run in rounds (healthRound) that every request arriving
// meanwhile shares: a storm holds one probe per backend, which ends with the
// round's budget, whatever the clients do. And a budget equal to the compose
// healthcheck's timeout (5 s) let Docker give up first while a chat backend
// hung, so the container went unhealthy over a backend, not over its
// database.

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
)

// healthServer serves /health over snap (Dream on, the database refusing).
func healthServer(t *testing.T, snap []backends.Backend) *HealthHandler {
	t.Helper()
	return roundHandler(brokenPool(t), seededBackends(snap...))
}

// TestHealthStormHoldsOneProbePerBackend — 200 /health requests at once while
// the chat backend hangs, each client gone after 100 ms: the backend saw one
// connection — the round the requests shared — and none is left once the
// round's budget ended.
func TestHealthStormHoldsOneProbePerBackend(t *testing.T) {
	const requests = 200
	setHealthBudget(t, time.Second)
	hang := newCountingHangHost(t)
	up := slowBackend(t, 0, nil)
	h := healthServer(t, []backends.Backend{
		operatorRow("i504-storm-e", up, backends.RoleEmbed, backends.RoleDream),
		operatorRow("i504-storm-c", "http://"+hang.l.Addr().String(), backends.RoleSynthesis),
	})
	t.Cleanup(func() { waitHealthRounds(t) }) // after srv.Close: a handler that came late may start a round
	srv := httptest.NewServer(http.HandlerFunc(h.Health))
	t.Cleanup(srv.Close)
	installRejectLogCapture(t)

	tr := &http.Transport{MaxIdleConnsPerHost: requests}
	t.Cleanup(tr.CloseIdleConnections)
	cl := &http.Client{Transport: tr}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range requests {
		wg.Go(func() {
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
			if resp, err := cl.Do(req); err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
		})
	}
	close(start)
	wg.Wait()
	waitHealthRounds(t)
	waitUntil(t, 20*time.Second, "the round's connection to close", func() bool { return hang.open.Load() == 0 })
	t.Logf("%d requests whose clients left after 100 ms: %d connections at the hanging backend, at most %d open at once",
		requests, hang.accepted.Load(), hang.peak.Load())
	if n := hang.accepted.Load(); n != 1 {
		t.Errorf("the hanging backend saw %d connections for %d requests within one budget, want 1 — the requests did not share their round", n, requests)
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

// switchableBackend serves GET / by mode: 200, 503, no answer until the
// request ends, or 200 after 100 ms.
func switchableBackend(t *testing.T, mode *atomic.Int32) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode.Load() {
		case backendDown:
			w.WriteHeader(http.StatusServiceUnavailable)
		case backendHangs:
			<-r.Context().Done()
		case backendSlow:
			select {
			case <-time.After(100 * time.Millisecond):
				w.WriteHeader(http.StatusOK)
			case <-r.Context().Done():
			}
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
	backendSlow
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
// the budget ends (`ctx statusline` gives up after 500 ms) gets its answer
// at once, and what its round finds is recorded as if it had waited: an
// outage already known stays known while the backend hangs, and an answer
// count in progress goes on while the backend answers after the clients
// left.
func TestHealthClientThatLeavesFirstChangesNothing(t *testing.T) {
	const leaveAfter = 20 * time.Millisecond
	setHealthBudget(t, 300*time.Millisecond)
	var mode atomic.Int32
	up := slowBackend(t, 0, nil)
	snap := []backends.Backend{
		operatorRow("i504-early-e", up, backends.RoleEmbed, backends.RoleDream),
		operatorRow("i504-early", switchableBackend(t, &mode), backends.RoleSynthesis),
	}
	h := healthServer(t, snap)
	t.Cleanup(func() { waitHealthRounds(t) })
	early := func(t *testing.T) {
		t.Helper()
		for range 3 {
			start := time.Now()
			if got := askHealth(h, leaveAfter); got != nil && mode.Load() == backendHangs {
				t.Errorf("a client that left after %v got %v while the backend hangs", leaveAfter, got)
			}
			if took := time.Since(start); took > leaveAfter+time.Second {
				t.Errorf("/health answered %v after its client left at %v — the request waited for its round", took, leaveAfter)
			}
		}
		waitHealthRounds(t)
	}

	healthCalls(t, snap, 3) // a known state: the backend answers, the role is served
	t.Run("a known outage stays known while the backend hangs", func(t *testing.T) {
		mode.Store(backendDown)
		logs := healthCalls(t, snap, 1)
		wantOneLine(t, logs, backends.RoleSynthesis, "row-i504-early", slog.LevelWarn, "backend ping failed", 0)
		logs = installRejectLogCapture(t)
		mode.Store(backendHangs)
		early(t)
		mode.Store(backendDown)
		healthCalls(t, snap, 1)
		if lines := healthRoleLines(logs, slog.LevelWarn, backends.RoleSynthesis, "row-i504-early"); len(lines) != 0 {
			t.Errorf("the outage was forgotten while clients left early: %+v", lines)
		}
	})
	t.Run("an answer count goes on while the clients leave", func(t *testing.T) {
		mode.Store(backendDown)
		healthCalls(t, snap, 1) // down (known or new)
		mode.Store(backendUp)
		healthCalls(t, snap, 2) // two answers in a row, not yet back
		mode.Store(backendSlow)
		logs := installRejectLogCapture(t)
		early(t) // the round's answer comes after the first client left: the third answer
		wantOneLine(t, logs, backends.RoleSynthesis, "row-i504-early", slog.LevelInfo, "answers again", 0)
	})
}
