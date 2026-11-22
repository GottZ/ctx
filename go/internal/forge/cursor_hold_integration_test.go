//go:build integration

// Issue #509, the second review of the push's forge stamp: a flag set across
// a push's advance, and an unordered version, behind the production cursor.
//
// The push advance cleared a flag a pull had set meanwhile when its block
// still held the version the PATCH sent. Another instance's sync run that
// listed between the PATCH's landing and the advance had seen a remote edit
// (block, base and forge all apart), flagged the mapping and saved since and
// the listing's ETag past that edit; the advance cleared the flag, every
// later run got a 304 or a since beyond the edit, and a later local edit was
// pushed over the remote one without a trace. The advance keeps the flag
// now, and the run that flags an entity anew — or leaves a version it cannot
// order — saves its cursor no further than that entity's stamp and without
// an ETag, so the next run lists the entity again and the pull decides on
// it: a pull-update clears the flag where the block did not move. A mapping
// flagged already holds no cursor back: the run after the one that flagged
// writes the cursor as before.
//
// Every run here is a sync run as ctxd starts it, from the project row the
// database holds with its saved cursor (since and ETag), against the fake
// forge, with a second issue changed later on the same listing.
//
//	go test -tags=integration ./internal/forge/ -run 'TestAFlagSetAcrossAPushAdvance|TestAnUnorderedVersionIsListedAgain|TestTheCursorHold|TestACommentFlaggedAnew' -count=1 -v
package forge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
)

// i509Mgr is a sync manager over pool with both pull legs on the production
// Applier and the fake forge's client.
func (e i491Env) i509Mgr(pool *pgxpool.Pool) *SyncManager {
	ap := e.applier(pool)
	m := mgr(pool, okTenant, okPolicy, e.gh.client(), ap.ApplyIssues)
	m.SetApplyComments(ap.ApplyComments)
	return m
}

// i509RunOn runs one sync of the project on m as ctxd starts it: from the
// project row the database holds, its saved fetch cursors included. It
// never calls Fatal (it may run inside a seam of the fake, off the test's
// goroutine); a run that ended with an error returns it with its state.
func (e i491Env) i509RunOn(m *SyncManager) (SyncStatus, error) {
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `UPDATE context_projects SET forge = $2::jsonb WHERE id = $1::uuid`,
		e.proj.ID, `{"kind":"github","owner":"o","repo":"r","api_base":"`+e.gh.srv.URL+`"}`); err != nil {
		return SyncStatus{}, err
	}
	proj, err := store.GetProjectByID(ctx, e.pool, e.proj.ID)
	if err != nil {
		return SyncStatus{}, fmt.Errorf("project row: %w", err)
	}
	if proj == nil {
		return SyncStatus{}, errors.New("project row: none")
	}
	if _, err := m.StartSync(ctx, *proj, false); err != nil {
		return SyncStatus{}, err
	}
	for deadline := time.Now().Add(i452Wait); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if st := m.Status(e.proj.ID); !st.Running {
			if st.LastError != "" {
				return st, errors.New(st.LastError)
			}
			return st, nil
		}
	}
	return SyncStatus{}, errors.New("the sync run did not finish")
}

// i509Run is i509RunOn on a fresh manager over pool.
func (e i491Env) i509Run(pool *pgxpool.Pool) (SyncStatus, error) {
	return e.i509RunOn(e.i509Mgr(pool))
}

// i509MustRun is i509Run on the environment's pool from the test goroutine.
func (e i491Env) i509MustRun(t *testing.T) SyncStatus {
	t.Helper()
	st, err := e.i509Run(e.pool)
	if err != nil {
		t.Fatalf("sync run: %v (%+v)", err, st)
	}
	return st
}

// i509Cursor is the saved fetch cursor of a leg ("issues" or "comments"),
// read as the next run reads it.
func (e i491Env) i509Cursor(t *testing.T, leg string) (etag string, since time.Time) {
	t.Helper()
	proj, err := store.GetProjectByID(context.Background(), e.pool, e.proj.ID)
	if err != nil || proj == nil {
		t.Fatalf("project row: %v", err)
	}
	if leg == "comments" {
		return readCommentCursor(proj.SyncCursor)
	}
	etag, since, _ = readCursor(proj.SyncCursor)
	return etag, since
}

// i509WithSix is the #491 environment with a second issue, #6, on the
// listing; one stored run pulls #6 and saves the issue leg's cursor (with
// the listing's ETag when etags).
func i509WithSix(t *testing.T, slug string, etags bool) i491Env {
	t.Helper()
	e := i491Setup(t, slug, "", fakeIssue{Title: "Crane", Body: "forge body"})
	e.gh.addIssue(fakeIssue{Number: 6, Updated: e.gh.issue(t, 5).Updated, Title: "Heron", Body: "six"})
	if etags {
		e.gh.setETags()
	}
	if st := e.i509MustRun(t); st.Applied != 1 {
		t.Fatalf("the run that pulls #6: %+v", st)
	}
	return e
}

// i509WantPulled asserts that #5's block holds body, unflagged, its base the
// forge's version.
func (e i491Env) i509WantPulled(t *testing.T, body, step string) {
	t.Helper()
	if base, flagged := e.mapping(t); flagged || base != e.forgeH(t) || e.body(t) != body {
		t.Errorf("%s: block %q, flagged %v, base is the forge's %v — want the remote edit pulled and the flag gone", step, e.body(t), flagged, base == e.forgeH(t))
	}
}

// TestAFlagSetAcrossAPushAdvanceIsResolvedByTheNextRun: A pushes a title
// edit; a remote body edit lands before or after A's PATCH, and another
// instance's sync run lists between the PATCH's landing and A's advance,
// with #6 changed later on the same listing. That run flags #5 (block, base
// and forge all apart). The advance keeps the flag, the flagging run saved
// its cursor at #5's stamp without an ETag, and the next run pulls the
// remote body and clears the flag.
func TestAFlagSetAcrossAPushAdvanceIsResolvedByTheNextRun(t *testing.T) {
	const remote = "body edited on the forge"
	for _, tc := range []struct {
		name           string
		etags, earlier bool
	}{
		{"a remote edit after the PATCH landed", true, false},
		{"a remote edit before the PATCH", true, true},
		{"a remote edit after the PATCH landed, a forge without ETags", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := i509WithSix(t, "i509f1", tc.etags)
			other := e.otherInstance(t)
			e.edit(t, store.IssueUpdate{Title: str("Crane, edited locally")})
			remoteEdit := func() { e.gh.edit(t, 5, func(it *fakeIssue) { it.Body = remote }) }
			var before func()
			if tc.earlier {
				before = remoteEdit
			}
			var mid SyncStatus
			var midErr error
			var at5 time.Time
			e.gh.seam(before, func() {
				if !tc.earlier {
					remoteEdit()
				}
				at5 = e.gh.issue(t, 5).Updated
				e.gh.edit(t, 6, func(it *fakeIssue) { it.Body = "six, edited on the forge later" })
				mid, midErr = e.i509Run(other)
			})
			if r := e.pushOn(t, e.pool); r.Pushed != 1 {
				t.Fatalf("A's push: %+v", r)
			}
			if midErr != nil || mid.Conflicts != 1 {
				t.Fatalf("precondition: the other instance's run %+v (%v), want #5 flagged", mid, midErr)
			}
			if _, flagged := e.mapping(t); !flagged {
				t.Fatalf("the push advance cleared the flag the other instance's run set; the forge's remote edit is unpulled")
			}
			if etag, since := e.i509Cursor(t, "issues"); etag != "" || !since.Equal(at5) {
				t.Errorf("the flagging run saved since %v and ETag %q, want since at #5's stamp %v and no ETag", since, etag, at5)
			}
			if st := e.i509MustRun(t); st.Applied != 1 {
				t.Errorf("the next run: %+v, want #5's remote edit pulled", st)
			}
			e.i509WantPulled(t, remote, "after the next run")
		})
	}
}

// TestAnUnorderedVersionIsListedAgainBehindANewerIssue: a remote edit in the
// second of A's PATCH; a sync run read #5's mapping before A's advance and
// writes after it, so it cannot order the two versions of that second and
// flags the mapping. #6, changed a second later on the same listing, moved
// the newest stamp of the page past #5's: the run saves since at #5's stamp
// (not #6's) and no ETag, and the next run pulls the edit.
func TestAnUnorderedVersionIsListedAgainBehindANewerIssue(t *testing.T) {
	const remote = "body edited on the forge in the second of the PATCH"
	e := i509WithSix(t, "i509unord", true)
	e.edit(t, store.IssueUpdate{Title: str("Crane, edited locally")})
	held, hold := i452HoldBeginPool(t, e.dsn)
	m := mgr(e.pool, okTenant, okPolicy, e.gh.client(), e.applier(held).ApplyIssues)
	var at5 time.Time
	e.gh.seam(nil, func() {
		e.gh.editInSecond(t, 5, func(it *fakeIssue) { it.Body = remote })
		at5 = e.gh.issue(t, 5).Updated
		e.gh.edit(t, 6, func(it *fakeIssue) { it.Body = "six, edited on the forge a second later" })
		go func() { _, _ = e.i509RunOn(m) }()
		select {
		case <-hold.entered:
		case <-time.After(i452Wait):
			t.Error("the held run did not reach its write")
		}
	})
	if r := e.pushOn(t, e.pool); r.Pushed != 1 {
		t.Fatalf("A's push: %+v", r)
	}
	close(hold.release)
	if st := waitDone(t, m, e.proj.ID); st.Conflicts != 1 {
		t.Fatalf("precondition: the run across the advance %+v, want the two versions of one second flagged", st)
	}
	if etag, since := e.i509Cursor(t, "issues"); etag != "" || !since.Equal(at5) {
		t.Errorf("the run saved since %v and ETag %q, want since at #5's stamp %v and no ETag", since, etag, at5)
	}
	if st := e.i509MustRun(t); st.Applied != 1 {
		t.Errorf("the next run: %+v, want the remote edit pulled", st)
	}
	e.i509WantPulled(t, remote, "after the next run")
}

// TestTheCursorHoldLastsOneRun: a flag over a real divergence of both sides
// stays, and only the run that sets it holds the cursor back. The second
// run lists #5 and #6 again (the cost of the hold: the entities from #5's
// stamp on, where the cursor without the hold asks from #6's), finds #5
// flagged already and saves the cursor as before — since at the newest
// stamp, the listing's ETag. From there the runs go as they go without a
// hold: the third asks from #6's stamp (a listing of its own, answered in
// full), the fourth is a 304.
func TestTheCursorHoldLastsOneRun(t *testing.T) {
	e := i509WithSix(t, "i509holdonce", true)
	e.edit(t, store.IssueUpdate{Title: str("Crane, edited locally")})
	e.gh.edit(t, 5, func(it *fakeIssue) { it.Body = "body edited on the forge" })
	at5 := e.gh.issue(t, 5).Updated
	e.gh.edit(t, 6, func(it *fakeIssue) { it.Body = "six, edited on the forge later" })
	at6 := e.gh.issue(t, 6).Updated

	st1 := e.i509MustRun(t)
	etag1, since1 := e.i509Cursor(t, "issues")
	if st1.Conflicts != 1 || etag1 != "" || !since1.Equal(at5) {
		t.Errorf("run 1: %+v, cursor since %v ETag %q — want #5 flagged, since at its stamp %v, no ETag", st1, since1, etag1, at5)
	}
	st2 := e.i509MustRun(t)
	etag2, since2 := e.i509Cursor(t, "issues")
	if st2.Fetched != 2 || st2.Conflicts != 0 || etag2 == "" || !since2.Equal(at6) {
		t.Errorf("run 2: %+v, cursor since %v ETag %q — want #5 and #6 listed again, the flag not counted again, since at #6's stamp %v and the listing's ETag", st2, since2, etag2, at6)
	}
	if st3 := e.i509MustRun(t); st3.Fetched != 1 {
		t.Errorf("run 3: %+v, want #6 alone listed (since at its stamp)", st3)
	}
	if st4 := e.i509MustRun(t); st4.Fetched != 0 {
		t.Errorf("run 4: %+v, want a 304", st4)
	}
	if _, flagged := e.mapping(t); !flagged || e.gh.issue(t, 5).Body != "body edited on the forge" {
		t.Errorf("flagged %v, forge body %q — want the flag over the divergence kept", flagged, e.gh.issue(t, 5).Body)
	}
	t.Logf("hold cost: run 2 fetched %d issue(s) on %d page(s); the cursor without the hold lists #6 alone", st2.Fetched, st2.Pages)
}

// i509Pages is a project whose forge lists two issues per page: #4 to #9,
// pulled by one stored run.
func i509Pages(t *testing.T, slug string) i491Env {
	t.Helper()
	e := i491Env{i486Env: i486Setup(t, slug, "")}
	e.proj = enablePush(e.proj)
	t0 := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)
	var issues []fakeIssue
	for n := int64(4); n <= 9; n++ {
		issues = append(issues, fakeIssue{Number: n, Updated: t0, Title: fmt.Sprintf("issue %d", n), Body: "forge body"})
	}
	e.gh = newFakeGitHub(t, 2, quotedNext, issues...)
	if st := e.i509MustRun(t); st.Applied != 6 {
		t.Fatalf("the run that pulls #4..#9: %+v", st)
	}
	return e
}

// i509EditBoth edits issue n's title locally and its body on the forge.
func (e i491Env) i509EditBoth(t *testing.T, n int64) {
	t.Helper()
	_, _, id, _ := readMapping(t, e.pool, e.proj.ID, "issue", n)
	tx, err := e.pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := store.UpdateIssueBlock(context.Background(), tx, id, store.IssueUpdate{Title: str(fmt.Sprintf("issue %d, edited locally", n))}, e.set(), []string{e.proj.Scope}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.gh.edit(t, n, func(it *fakeIssue) { it.Body = "body edited on the forge" })
}

// TestTheCursorHoldAcrossPagesAndTheBound: the listing runs over three pages
// of two — [#4 #6] [#5 #7] [#8 #9] by the order of their remote edits — and
// #5 and #8 are flagged anew on pages 2 and 3, after page 1's cursor was
// written. The hold is the earliest such stamp and lasts to the end of the
// leg; under the page bound the next run reads on from it.
func TestTheCursorHoldAcrossPagesAndTheBound(t *testing.T) {
	edits := func(t *testing.T, e i491Env) map[int64]time.Time {
		t.Helper()
		e.gh.edit(t, 4, func(it *fakeIssue) { it.Body = "four, edited on the forge" })
		e.gh.edit(t, 6, func(it *fakeIssue) { it.Body = "six, edited on the forge" })
		e.i509EditBoth(t, 5)
		e.gh.edit(t, 7, func(it *fakeIssue) { it.Body = "seven, edited on the forge" })
		e.i509EditBoth(t, 8)
		e.gh.edit(t, 9, func(it *fakeIssue) { it.Body = "nine, edited on the forge" })
		at := map[int64]time.Time{}
		for n := int64(4); n <= 9; n++ {
			at[n] = e.gh.issue(t, n).Updated
		}
		return at
	}
	t.Run("the whole listing in one run", func(t *testing.T) {
		e := i509Pages(t, "i509holdpages")
		at := edits(t, e)
		st1 := e.i509MustRun(t)
		if etag, since := e.i509Cursor(t, "issues"); st1.Conflicts != 2 || etag != "" || !since.Equal(at[5]) {
			t.Errorf("run 1: %+v, cursor since %v ETag %q — want #5 and #8 flagged and since at #5's stamp %v (page 2, the earliest)", st1, since, etag, at[5])
		}
		st2 := e.i509MustRun(t)
		if _, since := e.i509Cursor(t, "issues"); st2.Fetched != 4 || st2.Conflicts != 0 || !since.Equal(at[9]) {
			t.Errorf("run 2: %+v, cursor since %v — want #5 #7 #8 #9 listed again and since at the newest stamp %v", st2, since, at[9])
		}
	})
	t.Run("the page bound", func(t *testing.T) {
		e := i509Pages(t, "i509holdbound")
		at := edits(t, e)
		m := e.i509Mgr(e.pool)
		m.pageBound = 2
		if _, err := e.i509RunOn(m); err == nil || !strings.Contains(err.Error(), "pages in one run") {
			t.Fatalf("run 1 ended without the page bound: %v", err)
		}
		if etag, since := e.i509Cursor(t, "issues"); etag != "" || !since.Equal(at[5]) {
			t.Errorf("run 1 (pages 1 and 2 of 3): cursor since %v ETag %q, want since at #5's stamp %v", since, etag, at[5])
		}
		st2, err := e.i509RunOn(m)
		if err != nil {
			t.Fatalf("run 2: %v (%+v)", err, st2)
		}
		if _, since := e.i509Cursor(t, "issues"); st2.Fetched != 4 || st2.Conflicts != 1 || !since.Equal(at[8]) {
			t.Errorf("run 2: %+v, cursor since %v — want #5 #7 #8 #9 read, #8 flagged anew, since at #8's stamp %v", st2, since, at[8])
		}
		_, _, id9, _ := readMapping(t, e.pool, e.proj.ID, "issue", 9)
		if b := getBlock(t, e.pool, e.proj.Scope, id9); b.Content != "nine, edited on the forge" {
			t.Errorf("#9 holds %q, want its remote edit pulled past the bound", b.Content)
		}
		st3, err := e.i509RunOn(m)
		if _, since := e.i509Cursor(t, "issues"); err != nil || st3.Conflicts != 0 || !since.Equal(at[9]) {
			t.Errorf("run 3: %+v %v, cursor since %v — want since at the newest stamp %v", st3, err, since, at[9])
		}
	})
}

// TestACommentFlaggedAnewIsListedAgain: the comment leg holds its cursor the
// same way. Comment 700 is edited on both sides (flagged), comment 701 is
// written later on the same listing; the run saves the comment cursor at
// 700's stamp without an ETag. The local edit is then undone: the next run
// lists 700 again and pulls the remote edit (block at the base, forge
// ahead), and the run after it saves the cursor as before.
func TestACommentFlaggedAnewIsListedAgain(t *testing.T) {
	const remote = "comment edited on the forge"
	e := i491Setup(t, "i509chold", "", fakeIssue{Title: "Crane", Body: "forge body"})
	cid := i509CommentEnv(t, e)
	e.gh.setETags()
	e.i509MustRun(t)
	if etag, _ := e.i509Cursor(t, "comments"); etag == "" {
		t.Fatalf("precondition: the comment leg saved no ETag")
	}
	e.editComment(t, cid, "comment edited locally")
	at700 := e.gh.editComment(t, 700, remote)
	at701 := e.gh.addComment(701, 5, "a newer comment")

	if st := e.i509MustRun(t); st.Conflicts != 1 || st.Applied != 1 {
		t.Fatalf("run 1: %+v, want comment 700 flagged and 701 pulled", st)
	}
	if etag, since := e.i509Cursor(t, "comments"); etag != "" || !since.Equal(at700) {
		t.Errorf("run 1: comment cursor since %v ETag %q, want since at 700's stamp %v and no ETag", since, etag, at700)
	}
	e.editComment(t, cid, "forge comment")
	if st := e.i509MustRun(t); st.Applied != 1 {
		t.Errorf("run 2: %+v, want comment 700's remote edit pulled", st)
	}
	_, flagged, _, _ := readMapping(t, e.pool, e.proj.ID, "comment", 700)
	if b := getBlock(t, e.pool, e.proj.Scope, cid); flagged || b.Content != remote {
		t.Errorf("comment 700: block %q, flagged %v — want the remote edit pulled and the flag gone", b.Content, flagged)
	}
	if etag, since := e.i509Cursor(t, "comments"); etag == "" || !since.Equal(at701) {
		t.Errorf("run 2: comment cursor since %v ETag %q, want since at 701's stamp %v and the listing's ETag", since, etag, at701)
	}
}
