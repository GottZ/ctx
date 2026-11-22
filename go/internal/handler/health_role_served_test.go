package handler

// Issue #504, review of the linked pull request: a role can stop being served
// without a line. Backend X is known down (its WARN came once), backend Y
// answers and serves the role; the operator disables Y: the role answers
// error from then on, and the log had only X's DEBUG (before the pull request
// it had a WARN per request). Besides the state per backend, each role with
// candidates now has a state of its own, served or not: one WARN when none of
// its candidates answers any more, naming how many there are, DEBUG while that
// lasts, and one INFO when it is served again (after three calls in a row that
// found an answering candidate, like a backend). A role without an enabled
// backend keeps the one line that names that reason — no second WARN.

import (
	"log/slog"
	"net"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
)

// roleServedLines returns the records of role at level that carry no backend
// and are not about the role's rows (no enabled backend, enabled again): the
// lines of the role's served state.
func roleServedLines(logs *rejectLogCapture, level slog.Level, role string) []rejectLogRecord {
	var out []rejectLogRecord
	for _, r := range healthRoleLines(logs, level, role, "") {
		if _, ok := r.attrs["backend"]; ok || strings.Contains(r.msg, "enabled") || strings.Contains(r.msg, "no backend of this role —") ||
			strings.Contains(r.msg, "disabled") {
			continue
		}
		out = append(out, r)
	}
	return out
}

// closedURL returns the URL of a port nothing listens on.
func closedURL(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	u := "http://" + l.Addr().String()
	_ = l.Close()
	return u
}

func TestHealthLogsARoleThatNoBackendServes(t *testing.T) {
	up := slowBackend(t, 0, nil)
	e := operatorRow("i504-served-e", up, backends.RoleEmbed, backends.RoleDream)
	x := operatorRow("i504-served-x", closedURL(t), backends.RoleSynthesis)
	y := operatorRow("i504-served-y", up, backends.RoleSynthesis)
	yOff := y
	yOff.Enabled = false
	xOff := x
	xOff.Enabled = false
	const role = backends.RoleSynthesis

	healthCalls(t, []backends.Backend{e, x, y}, recoveryAnswers) // a known state: X down, Y serves the role

	t.Run("the operator disables the backend that serves the role", func(t *testing.T) {
		logs := healthCalls(t, []backends.Backend{e, x, yOff}, 3)
		warns := roleServedLines(logs, slog.LevelWarn, role)
		if len(warns) != 1 || !strings.Contains(warns[0].msg, "no backend of this role answers") {
			t.Fatalf("role %s: WARN lines %+v, want exactly one saying no backend of the role answers", role, warns)
		}
		if got, _ := warns[0].attrs["candidates"].(int64); got != 1 {
			t.Errorf("the WARN names %v candidates, want 1 (X)", warns[0].attrs["candidates"])
		}
		if got := len(roleServedLines(logs, slog.LevelDebug, role)); got != 2 {
			t.Errorf("%d DEBUG lines for the unserved role, want 2 (one per further call)", got)
		}
		if got := len(roleServedLines(logs, slog.LevelInfo, role)); got != 0 {
			t.Errorf("%d INFO lines, want none", got)
		}
		if lines := healthRoleLines(logs, slog.LevelWarn, role, x.Name); len(lines) != 0 {
			t.Errorf("X was known down, its line repeats: %+v", lines)
		}
	})

	t.Run("the backend is enabled again", func(t *testing.T) {
		logs := healthCalls(t, []backends.Backend{e, x, y}, recoveryAnswers-1)
		if got := roleServedLines(logs, slog.LevelInfo, role); len(got) != 0 {
			t.Errorf("served again after %d calls: %+v, want the INFO only after %d", recoveryAnswers-1, got, recoveryAnswers)
		}
		logs = healthCalls(t, []backends.Backend{e, x, y}, 2)
		infos := roleServedLines(logs, slog.LevelInfo, role)
		if len(infos) != 1 || !strings.Contains(infos[0].msg, "served again") {
			t.Errorf("INFO lines %+v, want exactly one saying the role is served again", infos)
		}
		if got := roleServedLines(logs, slog.LevelWarn, role); len(got) != 0 {
			t.Errorf("WARN lines %+v, want none", got)
		}
	})

	t.Run("every backend of the role disabled: one WARN, not two", func(t *testing.T) {
		logs := healthCalls(t, []backends.Backend{e, xOff, yOff}, 2)
		wantOneLine(t, logs, role, "", slog.LevelWarn, "disabled", 1)
	})

	t.Run("a candidate again, and it does not answer: the role is unserved", func(t *testing.T) {
		logs := healthCalls(t, []backends.Backend{e, x, yOff}, 1)
		warns := roleServedLines(logs, slog.LevelWarn, role)
		if len(warns) != 1 {
			t.Errorf("WARN lines %+v, want exactly one for the unserved role", warns)
		}
	})
}
