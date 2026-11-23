package handler

// Issue #504, review of the linked pull request: a backend that flaps — every
// second probe fails — wrote a WARN and an INFO in turn, twice the lines the
// per-request WARN wrote before. The WARN of an outage still comes with its
// first failed probe; the INFO that the backend answers again comes only after
// three probes in a row that it answered, and a failure in between starts the
// count anew. The first state a process observes is no change: it logs
// nothing.

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
)

// recoveryAnswers is the number of probes in a row a backend that was down
// has to answer before the INFO says it answers again.
const recoveryAnswers = 3

// backendLines returns the /health records of backend for role at INFO and
// above, as "LEVEL message".
func backendLines(logs *rejectLogCapture, role, backend string) []string {
	var out []string
	for _, r := range logs.all() {
		if r.level >= slog.LevelInfo && strings.HasPrefix(r.msg, "health check") && r.attrs["role"] == role && r.attrs["backend"] == backend {
			out = append(out, r.level.String()+" "+r.msg)
		}
	}
	return out
}

// TestHealthBackendAnswersAgainAfterThreeProbesInARow — outage, two answers,
// a failure, three answers, a second outage: WARN at the first failure, no
// INFO after two answers, the INFO with the third answer in a row, WARN again.
func TestHealthBackendAnswersAgainAfterThreeProbesInARow(t *testing.T) {
	var down atomic.Bool
	up := slowBackend(t, 0, nil)
	row := operatorRow("i504-rec", flakyBackend(t, &down), backends.RoleSynthesis)
	snap := []backends.Backend{operatorRow("i504-rec-e", up, backends.RoleEmbed, backends.RoleDream), row}
	healthCalls(t, snap, recoveryAnswers) // a known state: the backend answers
	pool := brokenPool(t)
	step := func(t *testing.T, isDown bool, calls int, want ...string) {
		t.Helper()
		down.Store(isDown)
		logs := installRejectLogCapture(t)
		for range calls {
			HealthStatus(context.Background(), pool, seededBackends(snap...), true, "ok")
		}
		got := backendLines(logs, backends.RoleSynthesis, row.Name)
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("%d calls with the backend down=%v: lines %q, want %q", calls, isDown, got, want)
		}
	}
	const warn, info = "WARN health check: backend ping failed", "INFO health check: backend answers again"
	step(t, true, 2, warn)
	step(t, false, recoveryAnswers-1)
	step(t, true, 1) // the count starts anew; still down, no second WARN
	step(t, false, recoveryAnswers-1)
	step(t, false, 1, info)
	step(t, false, 5)
	step(t, true, 1, warn)
}

// TestHealthFlappingBackendLogsOneWarn — every second probe of the chat
// backend fails: 60 calls write one WARN for the backend and one for its
// role, which it alone serves, and no INFO (before: 30 WARN and 29 INFO).
func TestHealthFlappingBackendLogsOneWarn(t *testing.T) {
	var n atomic.Int64
	flap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if n.Add(1)%2 == 0 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(flap.Close)
	up := slowBackend(t, 0, nil)
	e := operatorRow("i504-flap-e", up, backends.RoleEmbed, backends.RoleDream)
	healthCalls(t, []backends.Backend{e, operatorRow("i504-flap-base", up, backends.RoleSynthesis)}, recoveryAnswers) // the role is served
	row := operatorRow("i504-flap", flap.URL, backends.RoleSynthesis)
	logs := healthCalls(t, []backends.Backend{e, row}, 60)
	count := func(backend string) map[slog.Level]int {
		m := map[slog.Level]int{}
		for _, r := range logs.all() {
			b, _ := r.attrs["backend"].(string)
			if strings.HasPrefix(r.msg, "health check") && r.attrs["role"] == backends.RoleSynthesis && b == backend {
				m[r.level]++
			}
		}
		return m
	}
	b, role := count(row.Name), count("")
	t.Logf("60 calls with a backend that fails every second probe: backend %v, role %v", b, role)
	if b[slog.LevelWarn] != 1 || b[slog.LevelInfo] != 0 {
		t.Errorf("backend: %d WARN, %d INFO, want 1 and 0", b[slog.LevelWarn], b[slog.LevelInfo])
	}
	if role[slog.LevelWarn] != 1 || role[slog.LevelInfo] != 0 {
		t.Errorf("role: %d WARN, %d INFO, want 1 and 0", role[slog.LevelWarn], role[slog.LevelInfo])
	}
}

// TestHealthRecoveryOneLineUnderConcurrency — 64 calls at the same time for
// each change: exactly one WARN for the backend and one for the role it alone
// serves when it stops answering, exactly one INFO each when it answers
// again. A burst shares its probe round (issue #519), so the recovery, which
// wants three answers in a row, gets three bursts.
func TestHealthRecoveryOneLineUnderConcurrency(t *testing.T) {
	var down atomic.Bool
	up := slowBackend(t, 0, nil)
	row := operatorRow("i504-c64", flakyBackend(t, &down), backends.RoleSynthesis)
	snap := []backends.Backend{operatorRow("i504-c64-e", up, backends.RoleEmbed, backends.RoleDream), row}
	healthCalls(t, snap, recoveryAnswers) // a known state
	pool := brokenPool(t)
	bp := seededBackends(snap...)
	bursts := func(t *testing.T, isDown bool, n int) (backend, role map[slog.Level]int) {
		t.Helper()
		down.Store(isDown)
		logs := installRejectLogCapture(t)
		for range n {
			var wg sync.WaitGroup
			start := make(chan struct{})
			for range 64 {
				wg.Go(func() {
					<-start
					HealthStatus(context.Background(), pool, bp, true, "ok")
				})
			}
			close(start)
			wg.Wait()
		}
		backend, role = map[slog.Level]int{}, map[slog.Level]int{}
		for _, r := range logs.all() {
			if !strings.HasPrefix(r.msg, "health check") || r.attrs["role"] != backends.RoleSynthesis || r.level < slog.LevelInfo {
				continue
			}
			if r.attrs["backend"] == row.Name {
				backend[r.level]++
			} else if _, ok := r.attrs["backend"]; !ok {
				role[r.level]++
			}
		}
		return backend, role
	}
	for _, s := range []struct {
		name   string
		down   bool
		bursts int
		level  slog.Level
	}{
		{"outage", true, 1, slog.LevelWarn},
		{"recovery", false, recoveryAnswers, slog.LevelInfo},
		{"second outage", true, 1, slog.LevelWarn},
	} {
		if b, r := bursts(t, s.down, s.bursts); len(b) != 1 || b[s.level] != 1 || len(r) != 1 || r[s.level] != 1 {
			t.Errorf("%s: backend %v, role %v, want exactly one %v for each", s.name, b, r, s.level)
		}
	}
}

// TestHealthFirstObservationIsNoChange — a fresh process (a new healthNotice):
// the first call that finds every role served by an answering backend logs
// nothing above DEBUG for any role — no "enabled again", no "answers again",
// no "served again".
func TestHealthFirstObservationIsNoChange(t *testing.T) {
	prev := healthLines
	healthLines = &healthNotice{}
	t.Cleanup(func() { healthLines = prev })
	up := slowBackend(t, 0, nil)
	// As many calls as a recovery takes: a first state read as "unserved" or
	// "down" would log its all-clear within them.
	logs := healthCalls(t, []backends.Backend{operatorRow("i504-first", up, i504Roles...)}, recoveryAnswers)
	for _, r := range healthLinesAbove(logs) {
		if _, ok := r.attrs["role"]; ok {
			t.Errorf("first observation of a served role wrote %s %s", r.level, r.text())
		}
	}
}
