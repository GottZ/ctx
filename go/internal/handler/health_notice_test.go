package handler

// Issue #504: /health logs what it probes once per change of state, not once
// per request. Before, every request logged a WARN per probed role while no
// backend of the role was enabled (three on a new installation with Dream on;
// the compose healthcheck asks every 30 s, about 8,640 lines a day) and a WARN
// per role and unreachable backend. Now: one WARN when a role stops being
// served (naming the reason and the remedy), DEBUG for every further request
// in the same state, one INFO when it is served again — and the same for a
// backend that stops answering. The state is per role (and per role and
// backend), process-wide, and a change of state logs exactly once however many
// requests observe it at the same time.
//
// The tests reach a known state first (every role served) instead of
// resetting the process-wide state: whatever an earlier test left, the first
// call of a step below is a change of state.

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
)

// i504Roles are the roles HealthStatus probes with Dream on.
var i504Roles = []string{backends.RoleEmbed, backends.RoleSynthesis, backends.RoleDream}

// healthRoleLines returns the captured /health records of role at level
// (every record of the call whose message starts with "health check" and
// whose role attribute is role); backend narrows them to that backend's
// lines when it is not empty.
func healthRoleLines(logs *rejectLogCapture, level slog.Level, role, backend string) []rejectLogRecord {
	var out []rejectLogRecord
	for _, r := range logs.all() {
		if r.level != level || !strings.HasPrefix(r.msg, "health check") || r.attrs["role"] != role {
			continue
		}
		if backend != "" && r.attrs["backend"] != backend {
			continue
		}
		out = append(out, r)
	}
	return out
}

// healthCalls runs n HealthStatus calls over snap (Dream on) with a fresh log
// capture and returns it.
func healthCalls(t *testing.T, snap []backends.Backend, n int) *rejectLogCapture {
	t.Helper()
	pool := brokenPool(t)
	logs := installRejectLogCapture(t)
	for range n {
		HealthStatus(context.Background(), pool, snap, true, "ok")
	}
	return logs
}

// flakyBackend serves GET / with 200, or 503 while down is set.
func flakyBackend(t *testing.T, down *atomic.Bool) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if down.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// wantOneLine checks that role logged exactly one record at level (whose
// message contains want) and nothing at the other levels an operator reads,
// and debug records at DEBUG.
func wantOneLine(t *testing.T, logs *rejectLogCapture, role, backend string, level slog.Level, want string, debug int) {
	t.Helper()
	for _, l := range []slog.Level{slog.LevelError, slog.LevelWarn, slog.LevelInfo} {
		recs := healthRoleLines(logs, l, role, backend)
		switch {
		case l == level && (len(recs) != 1 || !strings.Contains(recs[0].msg, want)):
			t.Errorf("role %s %s: %d %v lines %+v, want exactly one saying %q", role, backend, len(recs), l, recs, want)
		case l != level && len(recs) != 0:
			t.Errorf("role %s %s: %d %v lines %+v, want none", role, backend, len(recs), l, recs)
		}
	}
	if got := len(healthRoleLines(logs, slog.LevelDebug, role, backend)); got != debug {
		t.Errorf("role %s %s: %d DEBUG lines, want %d (one per further request in the same state)", role, backend, got, debug)
	}
}

// TestHealthLogsARoleWithoutAnEnabledBackendOncePerChange — outage, a change
// of its reason, recovery, second outage: WARN, WARN, INFO, WARN per role,
// each once, DEBUG for every further request. The second outage is the lesson
// of #498: a state that stays behind silences the next outage.
func TestHealthLogsARoleWithoutAnEnabledBackendOncePerChange(t *testing.T) {
	up := slowBackend(t, 0, nil)
	serving := []backends.Backend{operatorRow("i504-role-up", up, i504Roles...)}
	off := operatorRow("i504-role-off", up, i504Roles...)
	off.Enabled = false
	tenantOnly := operatorRow("i504-role-tenant", up, i504Roles...)
	tenantOnly.Scope = "i504-tenant" // /health never probes a tenant row

	healthCalls(t, serving, 1) // a known state: every role is served
	const n = 10
	for _, step := range []struct {
		name  string
		snap  []backends.Backend
		level slog.Level
		want  string
	}{
		{"no backend of the role", nil, slog.LevelWarn, "ctx backends seed"},
		{"rows of the role, none enabled", []backends.Backend{off}, slog.LevelWarn, "disabled"},
		{"a backend of the role is enabled again", serving, slog.LevelInfo, "enabled again"},
		{"second outage: only a tenant row carries the role", []backends.Backend{tenantOnly}, slog.LevelWarn, "ctx backends seed"},
	} {
		t.Run(step.name, func(t *testing.T) {
			logs := healthCalls(t, step.snap, n)
			debug := n - 1
			if step.level == slog.LevelInfo {
				debug = 0 // a served role logs nothing per request
			}
			for _, role := range i504Roles {
				wantOneLine(t, logs, role, "", step.level, step.want, debug)
			}
		})
	}
}

// TestHealthLogsAnUnreachableBackendOncePerChange — a backend that stops
// answering, answers again and stops again: WARN, INFO, WARN per role it
// serves. A backend disabled while it is down and enabled again while still
// down is a new outage (the state of a row that is no candidate is
// forgotten). A probe its caller abandoned observes nothing about the
// backend: no line above DEBUG and no change of state.
func TestHealthLogsAnUnreachableBackendOncePerChange(t *testing.T) {
	var down atomic.Bool
	row := operatorRow("i504-flaky", flakyBackend(t, &down), i504Roles...)
	snap := []backends.Backend{row}
	disabled := row
	disabled.Enabled = false
	const n = 10

	healthCalls(t, snap, 1) // a known state: the backend answers
	for _, step := range []struct {
		name  string
		down  bool
		level slog.Level
		want  string
		debug int
	}{
		{"the backend stops answering", true, slog.LevelWarn, "backend ping failed", n - 1},
		{"the backend answers again", false, slog.LevelInfo, "answers again", 0},
		{"second outage", true, slog.LevelWarn, "backend ping failed", n - 1},
	} {
		t.Run(step.name, func(t *testing.T) {
			down.Store(step.down)
			logs := healthCalls(t, snap, n)
			for _, role := range i504Roles {
				wantOneLine(t, logs, role, row.Name, step.level, step.want, step.debug)
			}
		})
	}

	t.Run("disabled while down and enabled again while still down", func(t *testing.T) {
		healthCalls(t, []backends.Backend{disabled}, 2)
		logs := healthCalls(t, snap, n)
		for _, role := range i504Roles {
			wantOneLine(t, logs, role, row.Name, slog.LevelWarn, "backend ping failed", n-1)
		}
	})

	t.Run("a probe its caller abandoned changes nothing", func(t *testing.T) {
		down.Store(false)
		healthCalls(t, snap, 1) // answers again: INFO, the state is "up"
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		pool := brokenPool(t)
		logs := installRejectLogCapture(t)
		for range 3 {
			HealthStatus(ctx, pool, snap, true, "ok")
		}
		HealthStatus(context.Background(), pool, snap, true, "ok") // still up: no recovery line
		for _, role := range i504Roles {
			for _, l := range []slog.Level{slog.LevelError, slog.LevelWarn, slog.LevelInfo} {
				if recs := healthRoleLines(logs, l, role, row.Name); len(recs) != 0 {
					t.Errorf("role %s: %v lines %+v after abandoned probes, want none", role, l, recs)
				}
			}
		}
	})
}

// TestHealthForgetsTheDreamRoleWhileDreamIsOff — the dream role is not probed
// while dream.enabled is off; its state does not outlive that, so switching
// Dream on again over a pool still without a dream backend is a WARN again.
func TestHealthForgetsTheDreamRoleWhileDreamIsOff(t *testing.T) {
	up := slowBackend(t, 0, nil)
	healthCalls(t, []backends.Backend{operatorRow("i504-dream-up", up, i504Roles...)}, 1)
	embedOnly := []backends.Backend{operatorRow("i504-dream-embed", up, backends.RoleEmbed, backends.RoleSynthesis)}
	healthCalls(t, embedOnly, 2) // dream: WARN, DEBUG

	pool := brokenPool(t)
	for range 2 {
		HealthStatus(context.Background(), pool, embedOnly, false, "ok")
	}
	logs := healthCalls(t, embedOnly, 3)
	wantOneLine(t, logs, backends.RoleDream, "", slog.LevelWarn, "ctx backends seed", 2)
}

// TestHealthOneLinePerChangeUnderConcurrency — many /health requests at the
// same time observe one change of state: exactly one line per role.
func TestHealthOneLinePerChangeUnderConcurrency(t *testing.T) {
	var down atomic.Bool
	row := operatorRow("i504-conc", flakyBackend(t, &down), i504Roles...)
	serving := []backends.Backend{row}
	pool := brokenPool(t)
	burst := func(t *testing.T, snap []backends.Backend) *rejectLogCapture {
		t.Helper()
		logs := installRejectLogCapture(t)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for range 32 {
			wg.Go(func() {
				<-start
				HealthStatus(context.Background(), pool, snap, true, "ok")
			})
		}
		close(start)
		wg.Wait()
		return logs
	}
	healthCalls(t, serving, 1) // a known state

	for _, step := range []struct {
		name    string
		snap    []backends.Backend
		down    bool
		backend string
		level   slog.Level
		want    string
	}{
		{"the roles lose their backend", nil, false, "", slog.LevelWarn, "ctx backends seed"},
		{"the roles get it back", serving, false, "", slog.LevelInfo, "enabled again"},
		{"the backend stops answering", serving, true, row.Name, slog.LevelWarn, "backend ping failed"},
		{"the backend answers again", serving, false, row.Name, slog.LevelInfo, "answers again"},
	} {
		t.Run(step.name, func(t *testing.T) {
			down.Store(step.down)
			logs := burst(t, step.snap)
			debug := 31
			if step.level == slog.LevelInfo {
				debug = 0
			}
			for _, role := range i504Roles {
				wantOneLine(t, logs, role, step.backend, step.level, step.want, debug)
			}
		})
	}
}

// TestHealthForgetsABackendTheWalkDidNotReach — a role stops at its first
// reachable candidate, so a fallback behind an answering primary is not
// observed: its state is forgotten, not kept. Primary and fallback go down
// (WARN for each), the primary comes back (INFO; the fallback is not
// reached), the primary goes down again: the walk reaches the fallback,
// still down, and that is a WARN again — kept, the stale state would have
// made it DEBUG (review of issue #504).
func TestHealthForgetsABackendTheWalkDidNotReach(t *testing.T) {
	var primaryDown, fallbackDown atomic.Bool
	primary := operatorRow("i504-primary", flakyBackend(t, &primaryDown), i504Roles...)
	fallback := operatorRow("i504-fallback", flakyBackend(t, &fallbackDown), i504Roles...)
	snap := []backends.Backend{primary, fallback}
	healthCalls(t, snap, 1) // a known state: the primary answers

	primaryDown.Store(true)
	fallbackDown.Store(true)
	logs := healthCalls(t, snap, 2)
	for _, role := range i504Roles {
		wantOneLine(t, logs, role, primary.Name, slog.LevelWarn, "backend ping failed", 1)
		wantOneLine(t, logs, role, fallback.Name, slog.LevelWarn, "backend ping failed", 1)
	}
	primaryDown.Store(false)
	logs = healthCalls(t, snap, 2)
	for _, role := range i504Roles {
		wantOneLine(t, logs, role, primary.Name, slog.LevelInfo, "answers again", 0)
		if recs := healthRoleLines(logs, slog.LevelDebug, role, fallback.Name); len(recs) != 0 {
			t.Errorf("role %s: the fallback was probed behind an answering primary: %+v", role, recs)
		}
	}
	primaryDown.Store(true)
	logs = healthCalls(t, snap, 2)
	for _, role := range i504Roles {
		wantOneLine(t, logs, role, primary.Name, slog.LevelWarn, "backend ping failed", 1)
		wantOneLine(t, logs, role, fallback.Name, slog.LevelWarn, "backend ping failed", 1)
	}
}

// TestHealthObservesABackendThatHangsPastItsClient — a backend that accepts
// the connection and never answers, and a /health client that gives up
// before the probe's budget ends (the compose healthcheck's clock starts
// before its request): the probe runs to its budget all the same and the
// outage is a WARN (review of issue #504; before, the client's leaving cut
// the probe and the outage stayed at DEBUG).
func TestHealthObservesABackendThatHangsPastItsClient(t *testing.T) {
	prev := healthProbeBudget
	healthProbeBudget = 300 * time.Millisecond
	t.Cleanup(func() { healthProbeBudget = prev })

	up := slowBackend(t, 0, nil)
	hanging := slowBackend(t, time.Minute, nil)
	healthCalls(t, []backends.Backend{operatorRow("i504-hang", up, i504Roles...)}, 1) // a known state: it answers

	st := &swapStore{}
	st.p.Store(healthTestConfig())
	bp := backends.NewPool(nil, nil)
	bp.SeedSnapshotForTest([]backends.Backend{operatorRow("i504-hang", hanging, i504Roles...)})
	h := NewHealthHandler(brokenPool(t), st, bp, nil)
	logs := installRejectLogCapture(t)
	for range 2 {
		req := httptest.NewRequest(http.MethodGet, "/health", nil)
		ctx, cancel := context.WithCancel(req.Context())
		time.AfterFunc(50*time.Millisecond, cancel) // the client leaves before the budget ends
		h.Health(httptest.NewRecorder(), req.WithContext(ctx))
		cancel()
	}
	for _, role := range i504Roles {
		wantOneLine(t, logs, role, "row-i504-hang", slog.LevelWarn, "backend ping failed", 1)
	}
}
