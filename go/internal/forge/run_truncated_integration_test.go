//go:build integration

// Issue #474, the deadline's bookkeeping: a run that its deadline or a leg's
// page bound cut is not a failure at the forge. Before, both went through
// handleWireError: each bumped backoff_n and pushed backoff_until out (1, 2,
// 4, 8, 16 min …) although every such run moved the cursor on — a first sync
// that never fits one run stood at backoff_n 6 when it was done. A deadline
// during the token read wrote nothing to the project row, and a run whose
// work was all done when the deadline passed is a success.
//
// The rule (SyncManager.truncated): a run a bound cut leaves the bound in
// last_error (sync_status error); where it moved on — a fetch cursor past
// where it started, an entity applied or pushed — backoff_n goes to 0 and
// backoff_until is cleared, as for a success; where it did not, the backoff
// grows as for a wire error, so a forge that answers nothing is not run
// against back to back.
//
//	go test -tags=integration ./internal/forge/ -run 'TestATruncatedRun|TestADeadlineThatGetsNothing|TestADeadlineThatEndsTheTokenReadIsRecorded|TestARunWhoseWorkWasDone' -count=1 -v
package forge

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
)

// i474Backoff reads the project's backoff state as a status read serves it.
func (e i491Env) i474Backoff(t *testing.T) (n int, until *time.Time, lastErr string) {
	t.Helper()
	proj, err := store.GetProjectByID(context.Background(), e.pool, e.proj.ID)
	if err != nil || proj == nil {
		t.Fatalf("project row: %v", err)
	}
	_, _, n = readCursor(proj.SyncCursor)
	if proj.LastError != nil {
		lastErr = *proj.LastError
	}
	return n, proj.BackoffUntil, lastErr
}

func i474Catalog(n int) []fakeIssue {
	t0 := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)
	var issues []fakeIssue
	for i := 1; i <= n; i++ {
		issues = append(issues, fakeIssue{Number: int64(i), Updated: t0.Add(time.Duration(i) * time.Second), Title: fmt.Sprintf("T%d", i), Body: "b"})
	}
	return issues
}

func TestATruncatedRunThatMovesOnGrowsNoBackoff(t *testing.T) {
	cases := []struct {
		name  string
		setup func(m *SyncManager)
		bound string
	}{
		// A first sync that never fits the deadline: each page's apply takes
		// 300 ms, the deadline is 1 s.
		{"deadline", func(m *SyncManager) {
			orig := m.applyIssues
			m.applyIssues = func(ctx context.Context, p store.ProjectRow, is []IssueRemote) (ApplyResult, error) {
				select {
				case <-time.After(300 * time.Millisecond):
				case <-ctx.Done():
				}
				return orig(ctx, p, is)
			}
			m.SetRunTimeout(time.Second)
		}, "project.sync.run_timeout"},
		// A leg that reads 3 pages per run (the bound of 1000, scaled down).
		{"page bound", func(m *SyncManager) { m.pageBound = 3 }, "pages in one run"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := i491Env{i486Env: i486Setup(t, "rtr"+c.name[:2], "")}
			e.gh = newFakeGitHub(t, 1, quotedNext, i474Catalog(14)...)
			runs := 0
			for ; runs < 12; runs++ {
				m := e.i509Mgr(e.pool)
				c.setup(m)
				st, err := e.i509RunOn(m)
				n, until, lastErr := e.i474Backoff(t)
				t.Logf("run %d: applied %d err %v | backoff_n %d backoff_until %v", runs+1, st.Applied, err, n, until)
				if err == nil {
					break
				}
				if !strings.Contains(lastErr, c.bound) || st.BackoffSet {
					t.Errorf("run %d: last_error %q, backoff set %v — want the bound named and no backoff", runs+1, lastErr, st.BackoffSet)
				}
				if n != 0 || until != nil {
					t.Errorf("run %d moved the cursor on and left backoff_n %d, backoff_until %v — want 0 and none", runs+1, n, until)
				}
			}
			var mapped int
			if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM context_project_sync_map WHERE project_id = $1::uuid`, e.proj.ID).Scan(&mapped); err != nil {
				t.Fatal(err)
			}
			if runs == 12 || mapped != 14 {
				t.Errorf("after %d runs %d of 14 issues mapped — want the first sync done", runs+1, mapped)
			}
		})
	}
}

func TestADeadlineThatGetsNothingGrowsTheBackoff(t *testing.T) {
	e := i491Env{i486Env: i486Setup(t, "rtn", "")}
	e.gh = newFakeGitHub(t, 1, quotedNext, i474Catalog(3)...)
	release := make(chan struct{})
	defer close(release)
	e.gh.holdPage(1, release) // the forge answers no page
	for run := 1; run <= 2; run++ {
		m := e.i509Mgr(e.pool)
		m.SetRunTimeout(300 * time.Millisecond)
		st, err := e.i509RunOn(m)
		n, until, lastErr := e.i474Backoff(t)
		if err == nil || !strings.Contains(lastErr, "project.sync.run_timeout") || !st.BackoffSet || n != run || until == nil {
			t.Errorf("run %d that got no page: err %v, last_error %q, backoff set %v, backoff_n %d, until %v — want the deadline named and the backoff grown to %d",
				run, err, lastErr, st.BackoffSet, n, until, run)
		}
	}
}

// TestADeadlineThatEndsTheTokenReadIsRecorded: before, a deadline during
// the token read only finished the run — the project row kept no error and
// no backoff, while every other deadline wrote both.
func TestADeadlineThatEndsTheTokenReadIsRecorded(t *testing.T) {
	e := i491Setup(t, "rtk", "", fakeIssue{Title: "Crane", Body: "forge body"})
	m := e.i509Mgr(e.pool)
	e.i474Token(t, m)
	m.SetRunTimeout(time.Nanosecond)
	if _, err := e.i509RunOn(m); err == nil {
		t.Fatal("the run did not end at its deadline")
	}
	n, until, lastErr := e.i474Backoff(t)
	if !strings.Contains(lastErr, "project.sync.run_timeout") || n != 1 || until == nil {
		t.Errorf("the project row after a deadline in the token read: last_error %q, backoff_n %d, until %v — want the deadline named and the backoff grown (nothing moved)", lastErr, n, until)
	}
}

// TestARunWhoseWorkWasDoneIsASuccess: the last write of the push — a
// comment PATCH — answers only after the deadline passed. The write is let
// finish by design (Pusher.section) and the pass had nothing left: the
// deadline cut nothing, so the run is a success (no error, no backoff).
// Green before the rule too: it pins that the rule leaves such a run alone.
func TestARunWhoseWorkWasDoneIsASuccess(t *testing.T) {
	const deadline = 1500 * time.Millisecond
	e := i491Setup(t, "rtw5", "", fakeIssue{Title: "Crane", Body: "forge body"})
	e.gh.addComment(900, 5, "c900")
	m := e.i509Mgr(e.pool)
	e.i474Token(t, m)
	m.SetPush(NewPusher(e.pool, e.reg.SnapshotForTenant).PushProject)
	if _, err := e.i509RunOn(m); err != nil {
		t.Fatalf("the run that pulls #5 and its comment: %v", err)
	}
	_, _, bid, _ := readMapping(t, e.pool, e.proj.ID, "comment", 900)
	if _, err := e.pool.Exec(context.Background(), `UPDATE context_blocks SET content = 'c900, edited here', updated_at = now() WHERE id = $1::uuid`, bid); err != nil {
		t.Fatal(err)
	}
	m = e.i509Mgr(e.pool)
	e.i474Token(t, m)
	m.SetPush(NewPusher(e.pool, e.reg.SnapshotForTenant).PushProject)
	m.SetRunTimeout(deadline)
	start := time.Now()
	e.gh.commentSeam(func() { time.Sleep(time.Until(start.Add(deadline + 300*time.Millisecond))) })
	st, err := e.i509RunOn(m)
	if st.Pushed != 1 || e.gh.comment(t, 900).Body != "c900, edited here" {
		t.Fatalf("the run: %+v %v — want the comment PATCH landed", st, err)
	}
	n, until, lastErr := e.i474Backoff(t)
	if err != nil || lastErr != "" || n != 0 || until != nil {
		t.Errorf("a run whose last write answered after the deadline: err %v, last_error %q, backoff_n %d, until %v — want a success", err, lastErr, n, until)
	}
}
