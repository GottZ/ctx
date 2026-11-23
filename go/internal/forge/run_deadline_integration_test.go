//go:build integration

// Issue #474: a sync run has a deadline of its own (project.sync.run_timeout).
// Before, a run ran under a context without one: a forge that held a page
// back — or answered every page just under the client's 30 s timeout — held
// one of the process's sync slots as long as it liked, up to 1000 pages per
// leg, and every other project that wanted to sync got ErrSyncSaturated
// meanwhile. Now the run's reads, applies and push sections stop at the
// deadline; its bookkeeping — the cursor commit, the error stamp, the run
// row, the slot — runs outside it, so the run leaves the cursor of its last
// applied page, a last_error that names the deadline, and a free slot, and
// the next run goes on from there. A push write on the wire when the
// deadline passes runs to its answer and its advance commits.
//
//	go test -tags=integration ./internal/forge/ -run 'TestASyncRunEndsAtItsDeadline|TestAPushWriteOnTheWireAtTheDeadline' -count=1 -v
package forge

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/sealbox"
	"github.com/GottZ/ctx/internal/store"
)

// i474Token seals a token for the project, so a sync run's push leg runs
// (it runs only with a resolved token), and wires the box into m.
func (e *i491Env) i474Token(t *testing.T, m *SyncManager) {
	t.Helper()
	ctx := context.Background()
	box, err := sealbox.New(hex.EncodeToString([]byte(strings.Repeat("k", 32))), "")
	if err != nil {
		t.Fatal(err)
	}
	const secret = "forge_token"
	nonce, ct, err := box.Seal(secret, e.proj.Scope, []byte("token"))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := store.PutSecret(ctx, tx, secret, e.proj.Scope, nonce, ct, 1, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.SetProjectToken(ctx, tx, e.proj.ID, secret); err != nil {
		t.Fatal(err)
	}
	// No API action writes push_enabled; the row is what a run reads.
	if _, err := tx.Exec(ctx, `UPDATE context_projects SET push_enabled = true WHERE id = $1::uuid`, e.proj.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	m.openBox = func() (*sealbox.Box, error) { return box, nil }
	m.throttle = NewThrottle()
}

func TestASyncRunEndsAtItsDeadlineAndTheNextRunGoesOn(t *testing.T) {
	const deadline = 1500 * time.Millisecond
	ctx := context.Background()
	e := i491Env{i486Env: i486Setup(t, "rdl", "")}
	t0 := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)
	var issues []fakeIssue
	for i, n := 0, int64(4); n <= 9; i, n = i+1, n+1 {
		issues = append(issues, fakeIssue{Number: n, Updated: t0.Add(time.Duration(i) * time.Second), Title: fmt.Sprintf("issue %d", n), Body: "forge body"})
	}
	e.gh = newFakeGitHub(t, 1, quotedNext, issues...) // a page per issue
	release := make(chan struct{})
	e.gh.holdPage(3, release) // the forge does not answer page 3 (#6)
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()

	m := e.i509Mgr(e.pool)
	m.SetMaxConcurrent(1)
	m.SetRunTimeout(deadline)
	other := seedIFProject(t, e.pool, "rdlother")
	type ran struct {
		st  SyncStatus
		err error
	}
	done := make(chan ran, 1)
	go func() {
		st, err := e.i509RunOn(m)
		done <- ran{st, err}
	}()
	for e.gh.pagesServed.Load() < 3 {
		time.Sleep(5 * time.Millisecond)
	}
	// What another project sees while the forge holds page 3.
	if _, err := m.StartSync(ctx, other, false); !errors.Is(err, ErrSyncSaturated) {
		t.Errorf("another project's start while the run waits on page 3: %v, want %v", err, ErrSyncSaturated)
	}
	r := <-done
	st, err := r.st, r.err
	if err != nil && strings.Contains(err.Error(), "did not finish") {
		_, otherErr := m.StartSync(ctx, other, false)
		close(release)
		released = true
		t.Fatalf("the run held the forge's unanswered page 3, and with it the process's only sync slot, for %s — %s past its deadline of %s; another project's start then: %v",
			i452Wait, i452Wait-deadline, deadline, otherErr)
	}
	if !strings.Contains(st.LastError, "deadline of 1.5s") || !st.Aborted {
		t.Errorf("the run that met its deadline: last_error %q, aborted %v — want the deadline named", st.LastError, st.Aborted)
	}
	if n := len(m.sem); n != 0 {
		t.Errorf("the run that met its deadline still holds %d sync slot(s)", n)
	}
	// The cursor of the last applied page (#5, page 2) is saved: the run's
	// bookkeeping runs outside its deadline.
	if _, since := e.i509Cursor(t, "issues"); !since.Equal(t0.Add(time.Second)) {
		t.Errorf("the saved since after the deadline: %v, want #5's stamp %v (page 2, the last applied)", since, t0.Add(time.Second))
	}
	proj, err := store.GetProjectByID(ctx, e.pool, e.proj.ID)
	if err != nil || proj == nil {
		t.Fatalf("project row: %v", err)
	}
	if proj.LastError == nil || !strings.Contains(*proj.LastError, "project.sync.run_timeout") || proj.SyncStatus != "error" {
		t.Errorf("the project row after the deadline: status %q, last_error %v — want error and the deadline named", proj.SyncStatus, proj.LastError)
	}
	var run *store.SyncRunRow
	for i := 0; i < 400; i++ {
		if run, err = store.LatestSyncRun(ctx, e.pool, e.proj.ID); err != nil || (run != nil && run.Status != "running") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err != nil || run == nil || run.Status != "error" || run.Error == nil || !strings.Contains(*run.Error, "deadline") {
		t.Errorf("the run row: %+v %v — want an error run naming the deadline", run, err)
	}
	for _, n := range []int64{4, 5} {
		if _, _, id, _ := readMapping(t, e.pool, e.proj.ID, "issue", n); id == "" {
			t.Errorf("#%d, on a page applied before the deadline, is not mapped", n)
		}
	}

	// The next run goes on from the saved cursor: it lists #5..#9, not #4.
	close(release)
	released = true
	e.gh.pagesServed.Store(0)
	next := e.i509Mgr(e.pool)
	next.SetMaxConcurrent(1)
	st, err = e.i509RunOn(next)
	if err != nil || st.Fetched != 5 || st.Applied != 4 {
		t.Errorf("the run after the deadline: %+v %v — want #5..#9 listed (5), #6..#9 applied (4)", st, err)
	}
}

// TestTheCursorOfAPageAppliedAtTheDeadlineIsSaved: the apply of page 2 (#5)
// commits and returns only after the run's deadline passed. Its cursor is
// the run's bookkeeping and is saved; the run ends there.
func TestTheCursorOfAPageAppliedAtTheDeadlineIsSaved(t *testing.T) {
	const deadline = time.Second
	e := i491Env{i486Env: i486Setup(t, "rdc", "")}
	t0 := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)
	e.gh = newFakeGitHub(t, 1, quotedNext,
		fakeIssue{Number: 4, Updated: t0, Title: "issue 4", Body: "forge body"},
		fakeIssue{Number: 5, Updated: t0.Add(time.Second), Title: "issue 5", Body: "forge body"},
		fakeIssue{Number: 6, Updated: t0.Add(2 * time.Second), Title: "issue 6", Body: "forge body"})
	ap := e.applier(e.pool)
	start := time.Now()
	m := mgr(e.pool, okTenant, okPolicy, e.gh.client(), func(ctx context.Context, p store.ProjectRow, issues []IssueRemote) (ApplyResult, error) {
		res, err := ap.ApplyIssues(ctx, p, issues)
		if len(issues) == 1 && issues[0].Number == 5 {
			time.Sleep(time.Until(start.Add(deadline + 300*time.Millisecond)))
		}
		return res, err
	})
	m.SetRunTimeout(deadline)
	st, err := e.i509RunOn(m)
	if err == nil || !strings.Contains(st.LastError, "deadline") {
		t.Fatalf("the run: %+v %v — want it ended at its deadline", st, err)
	}
	if _, since := e.i509Cursor(t, "issues"); !since.Equal(t0.Add(time.Second)) {
		t.Errorf("the saved since: %v, want #5's stamp %v — the page applied at the deadline lost its cursor", since, t0.Add(time.Second))
	}
}

func TestAPushWriteOnTheWireAtTheDeadlineIsRecorded(t *testing.T) {
	const deadline = 2 * time.Second
	e := i491Setup(t, "rdp", "", fakeIssue{Title: "Crane", Body: "forge body"})
	e.edit(t, store.IssueUpdate{Content: str("local edit, pushed")})
	m := e.i509Mgr(e.pool)
	e.i474Token(t, m)
	m.SetPush(NewPusher(e.pool, e.reg.SnapshotForTenant).PushProject)
	m.SetRunTimeout(deadline)
	start := time.Now()
	// The PATCH lands; its answer leaves only once the run's deadline passed.
	e.gh.seam(nil, func() { time.Sleep(time.Until(start.Add(deadline + 300*time.Millisecond))) })
	st, err := e.i509RunOn(m)
	t.Logf("the run: %+v %v", st, err)
	if got := len(e.gh.landed()); got != 1 {
		t.Fatalf("PATCHes landed: %d, want 1", got)
	}
	if st.Pushed != 1 {
		t.Errorf("the run counts %d pushes, want the PATCH that landed", st.Pushed)
	}
	if base, flagged := e.mapping(t); flagged || base != e.forgeH(t) {
		t.Errorf("after a PATCH whose answer came after the deadline: flagged %v, base is the forge's %v — want the advance recorded", flagged, base == e.forgeH(t))
	}
	// The advance recorded the write: the next run sends nothing again.
	if _, err := e.i509RunOn(m); err != nil && !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("the next run: %v", err)
	}
	if got := len(e.gh.landed()); got != 1 {
		t.Errorf("PATCHes landed after the next run: %d — the advance of the first was lost and the edit went out again", got)
	}
}

// TestADeadlineThatEndsTheTokenReadIsNamed: a deadline that passes before
// the run read its sealed token names the deadline, not a token failure.
func TestADeadlineThatEndsTheTokenReadIsNamed(t *testing.T) {
	e := i491Setup(t, "rdt", "", fakeIssue{Title: "Crane", Body: "forge body"})
	m := e.i509Mgr(e.pool)
	e.i474Token(t, m)
	m.SetRunTimeout(time.Nanosecond)
	st, err := e.i509RunOn(m)
	if err == nil || !strings.Contains(st.LastError, "deadline") {
		t.Errorf("the run: last_error %q (%v) — want the deadline named", st.LastError, err)
	}
}
