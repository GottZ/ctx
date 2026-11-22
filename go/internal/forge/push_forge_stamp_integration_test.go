//go:build integration

// Issue #509: the push records the forge's stamp of its write. GitHub
// answers a PATCH (and a POST) with the entity it left, updated_at included;
// the client read nothing of it, and the advance left the mapping's
// forge_updated_at at the stamp of the version a pull last saw. With the
// same stamp and an unmoved base the 3-way matrix then decided about a fetch
// older than this push: a spurious conflict after a later local edit, a local
// revert to the older version lost without a trace, or a step back of the
// block that the next pull healed. The advance now records the stamp the
// answer carries (never one older than the row records), so such a fetch is
// older than the row's observation and writes nothing. An answer without a
// readable updated_at keeps the row's stamp and is no error of the push: the
// write landed.
//
//	go test -tags=integration ./internal/forge/ -run 'TestPushRecordsTheForgeStamp|TestAFetchOlderThanAnOwnPush|TestASameSecondRemoteEditAfterAPush' -count=1 -v
package forge

import (
	"context"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
)

// i509CommentEnv adds comment 700 on #5 to the fake forge and pulls it; it
// returns the comment's block id.
func i509CommentEnv(t *testing.T, e i491Env) string {
	t.Helper()
	e.gh.addComment(700, 5, "forge comment")
	if r := e.pullComments(t, e.fetchComments(t)); r.Applied != 1 {
		t.Fatalf("pull-create of comment 700: %+v", r)
	}
	_, _, id, _ := readMapping(t, e.pool, e.proj.ID, "comment", 700)
	return id
}

// fetchComments lists the forge's comments through the production client.
func (e i491Env) fetchComments(t *testing.T) []CommentRemote {
	t.Helper()
	page, err := e.gh.client().ListCommentsSince(context.Background(), e.gh.repo(), time.Time{}, "")
	if err != nil {
		t.Fatalf("fetch comments: %v", err)
	}
	return page.Comments
}

// pullComments applies a fetched comment page on the environment's pool.
func (e i491Env) pullComments(t *testing.T, comments []CommentRemote) ApplyResult {
	t.Helper()
	r, err := e.applier(e.pool).ApplyComments(context.Background(), e.proj, comments)
	if err != nil {
		t.Fatalf("comment pull: %v", err)
	}
	return r
}

// editComment is a local edit of a comment block through the issue verb.
func (e i491Env) editComment(t *testing.T, id, body string) {
	t.Helper()
	ctx := context.Background()
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := store.UpdateIssueBlock(ctx, tx, id, store.IssueUpdate{Content: &body}, e.set(), []string{e.proj.Scope}); err != nil {
		t.Fatalf("local comment edit: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// i509SameStamp answers whether the mapping's stamp is at.
func i509SameStamp(got *time.Time, at time.Time) bool { return got != nil && got.Equal(at) }

func TestPushRecordsTheForgeStampOfItsWrite(t *testing.T) {
	t.Run("an issue PATCH", func(t *testing.T) {
		e := i491Setup(t, "i509stamp", "", fakeIssue{Title: "Crane", Body: "forge body"})
		e.edit(t, store.IssueUpdate{Title: str("Crane, edited locally")})
		if r := e.pushOn(t, e.pool); r.Pushed != 1 {
			t.Fatalf("push: %+v", r)
		}
		if got, want := e.recordedStamp(t, e.id(t)), e.gh.issue(t, 5).Updated; !i509SameStamp(got, want) {
			t.Errorf("the mapping records the stamp %v, want %v, the one the PATCH answered", got, want)
		}
	})
	t.Run("a comment PATCH", func(t *testing.T) {
		e := i491Setup(t, "i509cstamp", "", fakeIssue{Title: "Crane", Body: "forge body"})
		cid := i509CommentEnv(t, e)
		e.editComment(t, cid, "comment edited locally")
		if r := e.pushOn(t, e.pool); r.Pushed != 1 {
			t.Fatalf("push: %+v", r)
		}
		c := e.gh.comment(t, 700)
		if c.Body != "comment edited locally" {
			t.Fatalf("the forge holds %q, want the local edit", c.Body)
		}
		if got := e.recordedStamp(t, cid); !i509SameStamp(got, c.Updated) {
			t.Errorf("the comment mapping records the stamp %v, want %v, the one the PATCH answered", got, c.Updated)
		}
	})
	for _, tc := range []struct {
		name   string
		answer fakeStampAnswer
	}{{"an answer without updated_at", fakeStampAbsent}, {"an answer with an unreadable updated_at", fakeStampUnreadable}} {
		t.Run(tc.name, func(t *testing.T) {
			e := i491Setup(t, "i509nostamp", "", fakeIssue{Title: "Crane", Body: "forge body"})
			before := e.recordedStamp(t, e.id(t))
			e.gh.setStampAnswer(tc.answer)
			e.edit(t, store.IssueUpdate{Title: str("Crane, edited locally")})
			if r := e.pushOn(t, e.pool); r.Pushed != 1 || r.Conflicts != 0 {
				t.Fatalf("push: %+v, want the write pushed and recorded", r)
			}
			if got := e.recordedStamp(t, e.id(t)); before == nil || !i509SameStamp(got, *before) {
				t.Errorf("the mapping records %v, want the stamp it held (%v)", got, before)
			}
			if base, conflict := e.mapping(t); conflict || base != e.forgeH(t) {
				t.Errorf("mapping flagged %v, base is the forge's %v", conflict, base == e.forgeH(t))
			}
		})
	}
}

// TestPushCreateRecordsTheForgeStamp: a push-create records the stamp of the
// create's answer — for an issue created closed, the stamp of the PATCH
// that closed it, the version the finalize records.
func TestPushCreateRecordsTheForgeStamp(t *testing.T) {
	t.Run("an issue", func(t *testing.T) {
		e := i491Setup(t, "i509cri", "", fakeIssue{Title: "Crane", Body: "forge body"})
		id := seedLocalIssue(t, e.pool, e.proj, 1, "Fresh", "draft body")
		if r := e.pushOn(t, e.pool); r.Pushed != 1 {
			t.Fatalf("push: %+v", r)
		}
		n, _, _ := mapByBlock(t, e.pool, id)
		if got, want := e.recordedStamp(t, id), e.gh.issue(t, n).Updated; !i509SameStamp(got, want) {
			t.Errorf("the mapping of the created #%d records %v, want %v", n, got, want)
		}
	})
	t.Run("an issue created closed", func(t *testing.T) {
		e := i491Setup(t, "i509cric", "", fakeIssue{Title: "Crane", Body: "forge body"})
		id := seedLocalIssue(t, e.pool, e.proj, 1, "Fresh", "draft body")
		e.move(t, id, "done")
		if r := e.pushOn(t, e.pool); r.Pushed != 1 {
			t.Fatalf("push: %+v", r)
		}
		n, _, _ := mapByBlock(t, e.pool, id)
		it := e.gh.issue(t, n)
		if it.State != "closed" {
			t.Fatalf("the forge holds #%d %s, want it closed", n, it.State)
		}
		if got := e.recordedStamp(t, id); !i509SameStamp(got, it.Updated) {
			t.Errorf("the mapping of the created #%d records %v, want %v, the close's", n, got, it.Updated)
		}
	})
	t.Run("a comment", func(t *testing.T) {
		e := i491Setup(t, "i509crc", "", fakeIssue{Title: "Crane", Body: "forge body"})
		cid := seedLocalComment(t, e.pool, e.proj, e.id(t), "#5.cL1", "local comment")
		if r := e.pushOn(t, e.pool); r.Pushed != 1 {
			t.Fatalf("push: %+v", r)
		}
		n, _, _ := mapByBlock(t, e.pool, cid)
		if got, want := e.recordedStamp(t, cid), e.gh.comment(t, n).Updated; !i509SameStamp(got, want) {
			t.Errorf("the mapping of the created comment %d records %v, want %v", n, got, want)
		}
	})
}

// TestAFetchOlderThanAnOwnPushWritesNothing: another instance fetched the
// issue before this instance's push landed and applies the fetch after the
// push's advance (the probes of the review of #501).
func TestAFetchOlderThanAnOwnPushWritesNothing(t *testing.T) {
	t.Run("a later local edit is pushed, not flagged", func(t *testing.T) {
		e := i491Setup(t, "i509stalefc", "", fakeIssue{Title: "Crane", Body: "forge body"})
		e.edit(t, store.IssueUpdate{Title: str("Crane 0")})
		stale := e.fetch(t)
		if r := e.pushOn(t, e.pool); r.Pushed != 1 {
			t.Fatalf("push: %+v", r)
		}
		e.edit(t, store.IssueUpdate{Title: str("Crane 0 late")})
		if r := e.applyPage(t, stale); r.Applied != 0 || r.Conflicts != 0 {
			t.Errorf("the older fetch: %+v, want nothing written and nothing flagged", r)
		}
		if n := e.cycles(t, 3); n != 1 {
			t.Errorf("three runs landed %d PATCH(es), want the late edit pushed once", n)
		}
		b := getBlock(t, e.pool, e.proj.Scope, e.id(t))
		if base, conflict := e.mapping(t); conflict || base != e.forgeH(t) || b.Title != "#5 Crane 0 late" || e.gh.issue(t, 5).Title != "Crane 0 late" {
			t.Errorf("flagged %v, base is the forge's %v, block %q, forge %q", conflict, base == e.forgeH(t), b.Title, e.gh.issue(t, 5).Title)
		}
	})
	t.Run("the block is not set back", func(t *testing.T) {
		e := i491Setup(t, "i509stalesw", "", fakeIssue{Title: "Crane", Body: "forge body"})
		e.edit(t, store.IssueUpdate{Title: str("Crane 0")})
		stale := e.fetch(t)
		if r := e.pushOn(t, e.pool); r.Pushed != 1 {
			t.Fatalf("push: %+v", r)
		}
		if r := e.applyPage(t, stale); r.Applied != 0 || r.Conflicts != 0 {
			t.Errorf("the older fetch: %+v, want nothing written", r)
		}
		if b := getBlock(t, e.pool, e.proj.Scope, e.id(t)); b.Title != "#5 Crane 0" {
			t.Errorf("the block holds %q, want the pushed version kept", b.Title)
		}
	})
	t.Run("a local revert to the older version is pushed", func(t *testing.T) {
		e := i491Setup(t, "i509staleaba", "", fakeIssue{Title: "Crane", Body: "forge body"})
		stale := e.fetch(t)
		e.edit(t, store.IssueUpdate{Title: str("Crane X")})
		if r := e.pushOn(t, e.pool); r.Pushed != 1 {
			t.Fatalf("push: %+v", r)
		}
		e.edit(t, store.IssueUpdate{Title: str("Crane")})
		if r := e.applyPage(t, stale); r.Applied != 0 || r.Conflicts != 0 {
			t.Errorf("the older fetch: %+v, want nothing written", r)
		}
		if r := e.pushOn(t, e.pool); r.Pushed != 1 {
			t.Errorf("push of the revert: %+v, want it pushed", r)
		}
		b := getBlock(t, e.pool, e.proj.Scope, e.id(t))
		if base, conflict := e.mapping(t); conflict || base != e.forgeH(t) || b.Title != "#5 Crane" || e.gh.issue(t, 5).Title != "Crane" {
			t.Errorf("flagged %v, base is the forge's %v, block %q, forge %q — want the revert on both sides", conflict, base == e.forgeH(t), b.Title, e.gh.issue(t, 5).Title)
		}
	})
	t.Run("a comment", func(t *testing.T) {
		e := i491Setup(t, "i509stalec", "", fakeIssue{Title: "Crane", Body: "forge body"})
		cid := i509CommentEnv(t, e)
		stale := e.fetchComments(t)
		e.editComment(t, cid, "comment edited locally")
		if r := e.pushOn(t, e.pool); r.Pushed != 1 {
			t.Fatalf("push: %+v", r)
		}
		if r := e.pullComments(t, stale); r.Applied != 0 || r.Conflicts != 0 {
			t.Errorf("the older comment fetch: %+v, want nothing written", r)
		}
		if b := getBlock(t, e.pool, e.proj.Scope, cid); b.Content != "comment edited locally" {
			t.Errorf("the comment block holds %q, want the pushed edit kept", b.Content)
		}
	})
}

// TestASameSecondRemoteEditAfterAPushIsPulled: the recorded stamp does not
// lock out a remote edit that lands in the second of the PATCH (GitHub's
// updated_at has whole seconds). The next pull fetches it at the stamp the
// row records; nothing moved the base since its read, so the matrix decides
// and pulls it. A pull that read the mapping before the advance and writes
// after it cannot order the two versions of that second and flags the
// mapping; the next pull, with block and base at the pushed version, pulls
// the remote edit and clears the flag.
func TestASameSecondRemoteEditAfterAPushIsPulled(t *testing.T) {
	const remote = "body edited on the forge in the second of the PATCH"
	t.Run("the next pull", func(t *testing.T) {
		e := i491Setup(t, "i509samesec", "", fakeIssue{Title: "Crane", Body: "forge body"})
		e.edit(t, store.IssueUpdate{Title: str("Crane, edited locally")})
		if r := e.pushOn(t, e.pool); r.Pushed != 1 {
			t.Fatalf("push: %+v", r)
		}
		e.gh.editInSecond(t, 5, func(it *fakeIssue) { it.Body = remote })
		if r := e.pullOn(t, e.pool); r.Applied != 1 || r.Conflicts != 0 {
			t.Errorf("the next pull: %+v, want the remote edit applied", r)
		}
		if base, conflict := e.mapping(t); conflict || base != e.forgeH(t) || e.body(t) != remote {
			t.Errorf("flagged %v, base is the forge's %v, block %q", conflict, base == e.forgeH(t), e.body(t))
		}
	})
	t.Run("a pull that read the mapping before the advance", func(t *testing.T) {
		e := i491Setup(t, "i509samesecwin", "", fakeIssue{Title: "Crane", Body: "forge body"})
		e.edit(t, store.IssueUpdate{Title: str("Crane, edited locally")})
		held, hold := i452HoldBeginPool(t, e.dsn)
		done := make(chan ApplyResult, 1)
		e.gh.seam(nil, func() {
			e.gh.editInSecond(t, 5, func(it *fakeIssue) { it.Body = remote })
			page := e.fetch(t)
			go func() {
				r, err := e.applier(held).ApplyIssues(context.Background(), e.proj, page)
				if err != nil {
					t.Errorf("held pull: %v", err)
				}
				done <- r
			}()
			select {
			case <-hold.entered:
			case <-time.After(i452Wait):
				t.Error("the held pull did not reach its write")
			}
		})
		if r := e.pushOn(t, e.pool); r.Pushed != 1 {
			t.Fatalf("push: %+v", r)
		}
		close(hold.release)
		var held1 ApplyResult
		select {
		case held1 = <-done:
		case <-time.After(i452Wait):
			t.Fatal("the held pull did not finish")
		}
		if held1.Applied != 0 || held1.Conflicts != 1 {
			t.Errorf("the pull across the advance: %+v, want the two versions of one second flagged", held1)
		}
		if r := e.pullOn(t, e.pool); r.Applied != 1 || r.Conflicts != 0 {
			t.Errorf("the next pull: %+v, want the remote edit applied", r)
		}
		if base, conflict := e.mapping(t); conflict || base != e.forgeH(t) || e.body(t) != remote {
			t.Errorf("flagged %v, base is the forge's %v, block %q", conflict, base == e.forgeH(t), e.body(t))
		}
		if n := e.cycles(t, 2); n != 0 {
			t.Errorf("two runs landed %d PATCH(es), want 0", n)
		}
	})
}
