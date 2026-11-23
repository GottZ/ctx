//go:build integration

// Issue #474: a sync run's deadline (project.sync.run_timeout) may pass at
// any point of the run, and wherever it passes the run leaves the sync
// whole. The test holds the run at each of twelve points — a held issue
// page, mid-apply of an issue page, after the last issue apply, between the
// pull legs, mid-apply of a comment page, before the push, inside the first
// and the second push section before its PATCH, the PATCH on the wire, the
// PATCH landed with its answer late, the PATCH landed and the forge edited
// after it, and the PATCH landed while another instance syncs the project —
// until the deadline passed, with local and remote edits of issues and
// comments pending on both sides. For each point: no advisory lock, no
// session idle in a transaction and no sync slot remain; the run's error,
// when it ended with one, names the deadline; and after two quiet runs no
// issue or comment differs between its block and the forge without a
// conflict flag.
//
//	go test -tags=integration ./internal/forge/ -run TestEveryDeadlineHoldPointLeavesTheSyncWhole -count=1 -v
package forge

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/sealbox"
	"github.com/GottZ/ctx/internal/store"
)

// i474mRaw is the project's saved sync cursor.
func (e i491Env) i474mRaw(t *testing.T) string {
	t.Helper()
	var raw string
	if err := e.pool.QueryRow(context.Background(), `SELECT COALESCE(sync_cursor, '{}'::jsonb)::text FROM context_projects WHERE id = $1::uuid`, e.proj.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

// i474mLocks counts the advisory locks of the test database and its
// sessions idle in a transaction.
func (e i491Env) i474mLocks(t *testing.T) (adv, idle int) {
	t.Helper()
	ctx := context.Background()
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`).Scan(&adv); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND state LIKE 'idle in transaction%'`).Scan(&idle); err != nil {
		t.Fatal(err)
	}
	return adv, idle
}

// i474mBox seals the project's token once and returns the box, for the
// managers of later runs.
func (e i491Env) i474mBox(t *testing.T) func() (*sealbox.Box, error) {
	t.Helper()
	m := e.i509Mgr(e.pool)
	e.i474Token(t, m)
	return m.openBox
}

// i474mMgr is a manager over pool with both pull legs, the production push
// and one sync slot.
func (e i491Env) i474mMgr(pool *pgxpool.Pool, box func() (*sealbox.Box, error)) *SyncManager {
	m := e.i509Mgr(pool)
	m.openBox = box
	m.throttle = NewThrottle()
	m.SetPush(NewPusher(pool, e.reg.SnapshotForTenant).PushProject)
	m.SetMaxConcurrent(1)
	return m
}

// i474mBlock is the block forge entity n of kind is mapped to, and whether
// its mapping is flagged; "" and nil when it is not mapped.
func (e i491Env) i474mBlock(t *testing.T, kind string, n int64) (id string, flagged bool, b *store.Block) {
	t.Helper()
	_, fl, id, _ := readMapping(t, e.pool, e.proj.ID, kind, n)
	if id == "" {
		return "", false, nil
	}
	return id, fl, getBlock(t, e.pool, e.proj.Scope, id)
}

// i474mEditIssue is a local edit of issue n's body through the issue verb.
func (e i491Env) i474mEditIssue(t *testing.T, n int64, body string) {
	t.Helper()
	ctx := context.Background()
	id, _, _ := e.i474mBlock(t, "issue", n)
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := store.UpdateIssueBlock(ctx, tx, id, store.IssueUpdate{Content: &body}, e.set(), []string{e.proj.Scope}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// i474mEditComment is a local edit of comment id's block.
func (e i491Env) i474mEditComment(t *testing.T, id int64, body string) {
	t.Helper()
	bid, _, _ := e.i474mBlock(t, "comment", id)
	if _, err := e.pool.Exec(context.Background(), `UPDATE context_blocks SET content = $2, updated_at = now() WHERE id = $1::uuid`, bid, body); err != nil {
		t.Fatal(err)
	}
}

// i474mDiverge lists every issue and comment whose block differs from the
// forge without a flag, and every one left unmapped.
func (e i491Env) i474mDiverge(t *testing.T) []string {
	t.Helper()
	var bad []string
	for n := int64(5); n <= 9; n++ {
		_, fl, b := e.i474mBlock(t, "issue", n)
		fi := e.gh.issue(t, n)
		if b == nil {
			bad = append(bad, fmt.Sprintf("#%d unmapped", n))
			continue
		}
		if !fl && (b.Content != fi.Body || b.Title != fmt.Sprintf("#%d %s", n, fi.Title)) {
			bad = append(bad, fmt.Sprintf("#%d block (%q,%q) forge (%q,%q)", n, b.Title, b.Content, fi.Title, fi.Body))
		}
	}
	for _, c := range []int64{900, 901, 902} {
		_, fl, b := e.i474mBlock(t, "comment", c)
		fc := e.gh.comment(t, c)
		if b == nil {
			bad = append(bad, fmt.Sprintf("c%d unmapped", c))
			continue
		}
		if !fl && b.Content != fc.Body {
			bad = append(bad, fmt.Sprintf("c%d block %q forge %q", c, b.Content, fc.Body))
		}
	}
	return bad
}

// i474mSetup builds a push-enabled project with a token against a fake
// forge holding issues #5..#9 and comments 900, 901 (on #5) and 902 (on
// #6), listed pageSize per page, and syncs it once.
func i474mSetup(t *testing.T, slug string, pageSize int) (i491Env, func() (*sealbox.Box, error)) {
	t.Helper()
	e := i491Env{i486Env: i486Setup(t, slug, "")}
	e.proj = enablePush(e.proj)
	t0 := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)
	var issues []fakeIssue
	for n := int64(5); n <= 9; n++ {
		issues = append(issues, fakeIssue{Number: n, Updated: t0.Add(time.Duration(n-5) * time.Second), Title: fmt.Sprintf("T%d", n), Body: "forge body"})
	}
	e.gh = newFakeGitHub(t, pageSize, quotedNext, issues...)
	e.gh.addComment(900, 5, "c900")
	e.gh.addComment(901, 5, "c901")
	e.gh.addComment(902, 6, "c902")
	box := e.i474mBox(t)
	if st, err := e.i509RunOn(e.i474mMgr(e.pool, box)); err != nil {
		t.Fatalf("first run: %+v %v", st, err)
	}
	if bad := e.i474mDiverge(t); len(bad) > 0 {
		t.Fatalf("after the first run: %v", bad)
	}
	return e, box
}

// i474mHooks is the state a hold point shares with the test: the run's
// context (for holds inside the fake forge), whether the hold point was
// reached, a counter of push sections, and — for a hold the forge keeps
// itself — a check run after the run.
type i474mHooks struct {
	runCtx    atomic.Value
	reached   atomic.Bool
	allowN    atomic.Int64
	reachedIf func() bool
}

// wait holds until ctx's deadline passed and marks the hold point reached.
func (h *i474mHooks) wait(ctx context.Context) {
	<-ctx.Done()
	h.reached.Store(true)
	time.Sleep(20 * time.Millisecond)
}

// waitRun is wait on the run's context, once the push stored it.
func (h *i474mHooks) waitRun() {
	if c, ok := h.runCtx.Load().(context.Context); ok {
		h.wait(c)
	}
}

func TestEveryDeadlineHoldPointLeavesTheSyncWhole(t *testing.T) {
	type holdPoint struct {
		name     string
		pageSize int
		build    func(t *testing.T, e i491Env, m *SyncManager, h *i474mHooks, release chan struct{})
	}
	// storeRunCtx wraps m's push so the fake forge's seams can wait on the
	// run's context.
	storeRunCtx := func(m *SyncManager, h *i474mHooks) {
		orig := m.push
		m.push = func(ctx context.Context, p store.ProjectRow, c Forge, r RepoRef, allow func() bool) (PushResult, error) {
			h.runCtx.Store(ctx)
			return orig(ctx, p, c, r, allow)
		}
	}
	// remoteEdit5 edits issue #5's title on the forge from inside a seam
	// (the fake's mutex is not held there).
	remoteEdit5 := func(e i491Env) {
		e.gh.mu.Lock()
		defer e.gh.mu.Unlock()
		for i := range e.gh.issues {
			if e.gh.issues[i].Number == 5 {
				e.gh.issues[i].Title = "R5T"
				e.gh.issues[i].Updated = e.gh.nextUpdated()
			}
		}
	}
	points := []holdPoint{
		{"issue-page2-held", 1, func(_ *testing.T, e i491Env, _ *SyncManager, h *i474mHooks, rel chan struct{}) {
			// The forge holds the issue listing's page 2 until the run is
			// over; the hold point is reached once the run asked for it.
			e.gh.holdPage(2, rel)
			served := e.gh.pagesServed.Load()
			h.reachedIf = func() bool { return e.gh.pagesServed.Load()-served >= 2 }
		}},
		{"issue-apply-mid-page", 2, func(_ *testing.T, _ i491Env, m *SyncManager, h *i474mHooks, _ chan struct{}) {
			orig := m.applyIssues
			m.applyIssues = func(ctx context.Context, p store.ProjectRow, is []IssueRemote) (ApplyResult, error) {
				if len(is) < 2 || h.reached.Load() {
					return orig(ctx, p, is)
				}
				r1, err := orig(ctx, p, is[:1])
				if err != nil {
					return r1, err
				}
				h.wait(ctx)
				r2, err := orig(ctx, p, is[1:])
				r1.Applied += r2.Applied
				r1.Conflicts += r2.Conflicts
				r1.Relist = relistAt(r1.Relist, r2.Relist)
				return r1, err
			}
		}},
		{"issue-apply-done-after-deadline", 1, func(_ *testing.T, _ i491Env, m *SyncManager, h *i474mHooks, _ chan struct{}) {
			orig := m.applyIssues
			var n atomic.Int64
			m.applyIssues = func(ctx context.Context, p store.ProjectRow, is []IssueRemote) (ApplyResult, error) {
				r, err := orig(ctx, p, is)
				if n.Add(1) == 2 {
					h.wait(ctx)
				}
				return r, err
			}
		}},
		{"between-legs", 1, func(_ *testing.T, _ i491Env, m *SyncManager, h *i474mHooks, _ chan struct{}) {
			orig := m.applyComments
			m.applyComments = func(ctx context.Context, p store.ProjectRow, cs []CommentRemote) (ApplyResult, error) {
				if !h.reached.Load() {
					h.wait(ctx)
				}
				return orig(ctx, p, cs)
			}
		}},
		{"comment-apply-mid", 1, func(_ *testing.T, _ i491Env, m *SyncManager, h *i474mHooks, _ chan struct{}) {
			orig := m.applyComments
			m.applyComments = func(ctx context.Context, p store.ProjectRow, cs []CommentRemote) (ApplyResult, error) {
				if len(cs) < 2 || h.reached.Load() {
					return orig(ctx, p, cs)
				}
				r1, err := orig(ctx, p, cs[:1])
				if err != nil {
					return r1, err
				}
				h.wait(ctx)
				r2, err := orig(ctx, p, cs[1:])
				r1.Applied += r2.Applied
				r1.orphans = append(r1.orphans, r2.orphans...)
				return r1, err
			}
		}},
		{"before-push", 1, func(_ *testing.T, _ i491Env, m *SyncManager, h *i474mHooks, _ chan struct{}) {
			orig := m.push
			m.push = func(ctx context.Context, p store.ProjectRow, c Forge, r RepoRef, allow func() bool) (PushResult, error) {
				h.wait(ctx)
				return orig(ctx, p, c, r, allow)
			}
		}},
		{"section-locked-before-patch", 1, func(_ *testing.T, _ i491Env, m *SyncManager, h *i474mHooks, _ chan struct{}) {
			orig := m.push
			m.push = func(ctx context.Context, p store.ProjectRow, c Forge, r RepoRef, allow func() bool) (PushResult, error) {
				return orig(ctx, p, c, r, func() bool { h.wait(ctx); return allow() })
			}
		}},
		{"second-section-before-patch", 1, func(_ *testing.T, _ i491Env, m *SyncManager, h *i474mHooks, _ chan struct{}) {
			orig := m.push
			m.push = func(ctx context.Context, p store.ProjectRow, c Forge, r RepoRef, allow func() bool) (PushResult, error) {
				return orig(ctx, p, c, r, func() bool {
					if h.allowN.Add(1) == 2 {
						h.wait(ctx)
					}
					return allow()
				})
			}
		}},
		{"patch-on-wire", 1, func(_ *testing.T, e i491Env, m *SyncManager, h *i474mHooks, _ chan struct{}) {
			storeRunCtx(m, h)
			e.gh.seam(h.waitRun, nil)
		}},
		{"patch-landed-answer-late", 1, func(_ *testing.T, e i491Env, m *SyncManager, h *i474mHooks, _ chan struct{}) {
			storeRunCtx(m, h)
			e.gh.seam(nil, h.waitRun)
		}},
		{"patch-landed-remote-edit", 1, func(_ *testing.T, e i491Env, m *SyncManager, h *i474mHooks, _ chan struct{}) {
			storeRunCtx(m, h)
			e.gh.seam(nil, func() {
				remoteEdit5(e)
				h.waitRun()
			})
		}},
		{"patch-landed-other-run", 1, func(t *testing.T, e i491Env, m *SyncManager, h *i474mHooks, _ chan struct{}) {
			storeRunCtx(m, h)
			other := e.otherInstance(t)
			e.gh.seam(nil, func() {
				remoteEdit5(e)
				om := e.i474mMgr(other, m.openBox)
				om.push = nil
				st, err := e.i509RunOn(om)
				t.Logf("  other instance's run inside the PATCH window: applied %d conflicts %d err %v", st.Applied, st.Conflicts, err)
				h.waitRun()
			})
		}},
	}
	for i, p := range points {
		t.Run(p.name, func(t *testing.T) {
			e, box := i474mSetup(t, fmt.Sprintf("i474m%d", i), p.pageSize)
			e.i474mEditIssue(t, 5, "L5")
			e.i474mEditIssue(t, 6, "L6")
			e.gh.edit(t, 7, func(it *fakeIssue) { it.Body = "R7" })
			e.gh.edit(t, 8, func(it *fakeIssue) { it.Title = "R8T" })
			e.gh.edit(t, 9, func(it *fakeIssue) { it.Body = "R9" })
			e.i474mEditComment(t, 900, "L900")
			e.gh.editComment(t, 901, "R901")
			e.gh.editComment(t, 902, "R902")
			cursorBefore := e.i474mRaw(t)

			m := e.i474mMgr(e.pool, box)
			m.SetRunTimeout(2500 * time.Millisecond)
			h := &i474mHooks{}
			rel := make(chan struct{})
			p.build(t, e, m, h, rel)
			patchesBefore := len(e.gh.landed())
			st, err := e.i509RunOn(m)
			if h.reachedIf != nil && h.reachedIf() {
				h.reached.Store(true)
			}
			close(rel)
			adv, idle := e.i474mLocks(t)
			t.Logf("deadline run: reached=%v err=%v applied=%d pushed=%d conflicts=%d sem=%d | advisory=%d idle-in-tx=%d | patches=%d",
				h.reached.Load(), err, st.Applied, st.Pushed, st.Conflicts, len(m.sem), adv, idle, len(e.gh.landed())-patchesBefore)
			t.Logf("cursor before: %s", cursorBefore)
			t.Logf("cursor after : %s", e.i474mRaw(t))
			if !h.reached.Load() {
				t.Errorf("hold point not reached (the deadline came elsewhere): err %v", err)
			}
			if adv != 0 || idle != 0 || len(m.sem) != 0 {
				t.Errorf("LEFT LOCKED after the deadline: advisory %d idle-in-tx %d sem %d", adv, idle, len(m.sem))
			}
			if err != nil && !strings.Contains(err.Error(), "deadline") {
				t.Errorf("the run ended at the deadline with an error that does not name it: %v", err)
			}
			if bad := e.i474mDiverge(t); len(bad) > 0 {
				t.Logf("right after the deadline run (the quiet runs settle it): %v", bad)
			}
			for q := 0; q < 2; q++ {
				st, err := e.i509RunOn(e.i474mMgr(e.pool, box))
				t.Logf("quiet run %d: applied %d pushed %d conflicts %d err %v", q, st.Applied, st.Pushed, st.Conflicts, err)
			}
			byNumber := map[int64]int{}
			for _, pt := range e.gh.landed()[patchesBefore:] {
				byNumber[pt.Number]++
			}
			t.Logf("issue PATCHes from the deadline run on, by number: %v", byNumber)
			if bad := e.i474mDiverge(t); len(bad) > 0 {
				t.Errorf("SILENT DIVERGENCE after two quiet runs: %v", bad)
			}
			for n := int64(5); n <= 9; n++ {
				if _, fl, _ := e.i474mBlock(t, "issue", n); fl {
					t.Logf("  #%d flagged after the quiet runs", n)
				}
			}
		})
	}
}
