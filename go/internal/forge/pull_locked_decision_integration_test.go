//go:build integration

// Issue #491 points 1 and 3: every write of the pull's 3-way decision is
// decided again on the rows its transaction locks.
//
// Point 1. The convergence (block and forge agree, the base is behind) wrote
// base := their hash and synced_at = now() without locking the block. A local
// edit that committed between the pull's read and that write — or whose
// transaction began before the write and wrote after it — was left with
// updated_at < synced_at: no push candidate, never pushed, and nothing said
// so. The convergence now locks the block, records the version both sides
// held as the base, and sets synced_at to the time of that version while the
// block still holds it, -infinity once a local write moved it.
//
// Point 3. A pull that decided "forge-ahead" on its read compared the locked
// block with the base of the page-wide read, not with the mapping row:
// another instance that advanced the base in between turned the decision
// into a conflict the user had to resolve, where the mapping was in fact
// forge-ahead. The pull now locks the mapping row after the block (the order
// of every other writer) and decides on it.
//
// The windows are opened by a hold on the applier's pool (the edit runs
// while the pull's write transaction waits at its BEGIN) and by a
// transaction the test begins before the write and commits after it; the
// writes are the production ones.
//
//	go test -tags=integration ./internal/forge/ -run 'TestPullConvergence|TestPullDecidesOnTheLockedMapping' -count=1 -v
package forge

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// i491Converging sets up the convergence: the block and the forge were
// renamed alike, the base still holds the pulled version.
func i491Converging(t *testing.T, slug string) i491Env {
	t.Helper()
	e := i491Setup(t, slug, "", fakeIssue{Title: "Crane", Body: "forge body"})
	e.edit(t, store.IssueUpdate{Title: str("Crane, renamed")})
	e.gh.edit(t, 5, func(it *fakeIssue) { it.Title = "Crane, renamed" })
	return e
}

// i491PushesTheEdit: the next push PATCHes the local body, the forge holds
// it, and further runs write nothing and flag nothing.
func i491PushesTheEdit(t *testing.T, e i491Env, body string) {
	t.Helper()
	landed := len(e.gh.landed())
	e.pushOn(t, e.pool)
	got := e.gh.landed()
	if len(got) != landed+1 {
		t.Fatalf("the push after the convergence landed %d PATCH(es), want 1 carrying the local edit", len(got)-landed)
	}
	var sent string
	if raw, ok := got[landed].Fields["body"]; !ok || json.Unmarshal(raw, &sent) != nil || sent != body {
		t.Fatalf("the PATCH carries %v, want the body %q", got[landed].Fields, body)
	}
	if _, ok := got[landed].Fields["title"]; ok {
		t.Errorf("the PATCH carries the title again: the base must hold the version both sides agreed on")
	}
	if it := e.gh.issue(t, 5); it.Body != body || it.Title != "Crane, renamed" {
		t.Errorf("the forge holds %q / %q", it.Title, it.Body)
	}
	if n := e.cycles(t, 3); n != 0 {
		t.Errorf("three runs after the push landed %d PATCH(es), want 0", n)
	}
	if base, conflict := e.mapping(t); conflict || base != e.forgeH(t) {
		t.Errorf("after the push: mapping flagged %v, base is the forge's %v", conflict, base == e.forgeH(t))
	}
}

func TestPullConvergenceKeepsALocalEditMadeDuringIt(t *testing.T) {
	// The edit commits between the pull's read and its write.
	t.Run("edit committed between the read and the write", func(t *testing.T) {
		e := i491Converging(t, "i491convread")
		const body = "local body written during the pull"
		e.pullHeldOn(t, func() { e.edit(t, store.IssueUpdate{Content: str(body)}) })
		i491PushesTheEdit(t, e, body)
	})

	// The edit's transaction began before the version the pull read was
	// written (so its updated_at is older than that version's) and commits
	// during the pull: no timestamp shows it, only the locked block does.
	t.Run("edit begun before the version the pull read, committed during the pull", func(t *testing.T) {
		e := i491Setup(t, "i491convbegun", "", fakeIssue{Title: "Crane", Body: "forge body"})
		w := e.begun(t)
		e.edit(t, store.IssueUpdate{Title: str("Crane, renamed")})
		e.gh.edit(t, 5, func(it *fakeIssue) { it.Title = "Crane, renamed" })
		const body = "body of a write begun before the rename"
		e.pullHeldOn(t, func() {
			e.editIn(t, w, store.IssueUpdate{Content: str(body)})
			if err := w.Commit(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
		i491PushesTheEdit(t, e, body)
	})

	// The edit's transaction began after that version and before the
	// convergence, and writes once the convergence committed: its updated_at
	// (its transaction start) lies below the convergence's now().
	t.Run("edit begun before the convergence, written after it", func(t *testing.T) {
		e := i491Converging(t, "i491convafter")
		w := e.begun(t)
		if r := e.pullOn(t, e.pool); r.Applied != 0 || r.Conflicts != 0 {
			t.Fatalf("converging pull: %+v, want nothing applied and no conflict", r)
		}
		if base, conflict := e.mapping(t); conflict || base != e.forgeH(t) {
			t.Fatalf("precondition: the pull did not converge (flagged %v)", conflict)
		}
		const body = "body of a write begun before the convergence"
		e.editIn(t, w, store.IssueUpdate{Content: str(body)})
		if err := w.Commit(context.Background()); err != nil {
			t.Fatal(err)
		}
		i491PushesTheEdit(t, e, body)
	})

	// Another instance flags the mapping while this pull waits: it pulled a
	// remote edit made after this pull fetched, and the local edit made
	// during the wait — both sides moved. The convergence this pull read must
	// not clear that flag and let the next push send the local edit over the
	// remote one.
	t.Run("conflict flagged by another instance during the pull", func(t *testing.T) {
		e := i491Converging(t, "i491convflag")
		e.pullHeldOn(t, func() {
			e.edit(t, store.IssueUpdate{Content: str("body edited locally during the pull")})
			e.gh.edit(t, 5, func(it *fakeIssue) { it.Body = "body edited on the forge after the fetch" })
			if r := e.pullOn(t, e.pool); r.Conflicts != 1 {
				t.Fatalf("other instance's pull: %+v, want the conflict flagged", r)
			}
		})
		if _, conflict := e.mapping(t); !conflict {
			t.Errorf("the converging pull cleared the flag another instance set during it")
		}
		landed := len(e.gh.landed())
		e.pushOn(t, e.pool)
		if n := len(e.gh.landed()) - landed; n != 0 {
			t.Errorf("the push sent %d PATCH(es) over the flagged mapping", n)
		}
		if it := e.gh.issue(t, 5); it.Body != "body edited on the forge after the fetch" {
			t.Errorf("the forge holds %q, want the remote edit kept", it.Body)
		}
	})

	// A comment converges through the same write.
	t.Run("comment, edit committed between the read and the write", func(t *testing.T) {
		pool, dsn := testdb.SetupTestDBWithDSN(t)
		ctx := context.Background()
		proj := enablePush(seedIFProject(t, pool, "i491convcomment"))
		_, commentID := i452Pulled(t, pool, proj, true)
		i452Edit(t, ctx, pool, proj.Scope, commentID, store.IssueUpdate{Content: str("comment v2")})

		held, hold := i452HoldBeginPool(t, dsn)
		done := make(chan error, 1)
		go func() {
			_, err := applierWithRegistry(held).ApplyComments(ctx, proj, []CommentRemote{{ID: 700, IssueNumber: 5, Body: "comment v2", UpdatedAt: time.Unix(2, 0).UTC()}})
			done <- err
		}()
		select {
		case <-hold.entered:
		case err := <-done:
			t.Fatalf("the converging pull finished without a write transaction: %v", err)
		case <-time.After(i452Wait):
			t.Fatalf("the pull did not reach its write within %s", i452Wait)
		}
		i452Edit(t, ctx, pool, proj.Scope, commentID, store.IssueUpdate{Content: str("comment v3, written during the pull")})
		close(hold.release)
		if err := <-done; err != nil {
			t.Fatalf("pull: %v", err)
		}
		spy := newPushSpy()
		res, err := pusherWithRegistry(pool).PushProject(ctx, proj, spy, RepoRef{Owner: "o", Repo: "r"}, alwaysAllow)
		if err != nil || res.Pushed != 1 || len(spy.commentPatches) != 1 || spy.commentPatches[0].Body != "comment v3, written during the pull" {
			t.Fatalf("push after the convergence: pushed=%d comment patches=%+v err=%v, want the body written during the pull", res.Pushed, spy.commentPatches, err)
		}
		i452NoCandidates(t, pool, proj)
	})
}

// pullHeldOn runs one pull on a pool whose write transaction waits at its
// BEGIN; between the read and the write it calls between.
func (e i491Env) pullHeldOn(t *testing.T, between func()) ApplyResult {
	t.Helper()
	return e.pullHeldOnPage(t, e.fetch(t), between)
}

// pullHeldOnPage is pullHeldOn for a page fetched earlier.
func (e i491Env) pullHeldOnPage(t *testing.T, issues []IssueRemote, between func()) ApplyResult {
	t.Helper()
	held, hold := i452HoldBeginPool(t, e.dsn)
	type out struct {
		res ApplyResult
		err error
	}
	done := make(chan out, 1)
	go func() {
		r, err := e.applier(held).ApplyIssues(context.Background(), e.proj, issues)
		done <- out{r, err}
	}()
	select {
	case <-hold.entered:
	case o := <-done:
		t.Fatalf("the pull finished without a write transaction: %+v %v", o.res, o.err)
	case <-time.After(i452Wait):
		t.Fatalf("the pull did not reach its write within %s", i452Wait)
	}
	between()
	close(hold.release)
	select {
	case o := <-done:
		if o.err != nil {
			t.Fatalf("pull: %v", o.err)
		}
		return o.res
	case <-time.After(i452Wait):
		t.Fatalf("the pull did not finish within %s", i452Wait)
	}
	return ApplyResult{}
}

// TestPullDecidesOnTheLockedMapping: the pull read the block at the base and
// the forge ahead of it; before its write, a local edit commits and another
// instance pushes it (PATCH, then the advance moves the base onto the edit).
// The mapping row the pull locks holds the edit as its base and the block
// equals it: the forge is ahead of that base, not in conflict with it. The
// pull writes what it fetched, and the next pull — the forge holds the edit
// the other instance sent — brings the block level with the forge, without a
// conflict and without a wire write.
func TestPullDecidesOnTheLockedMapping(t *testing.T) {
	e := i491Setup(t, "i491lockedmap", "", fakeIssue{Title: "Crane", Body: "forge body"})
	e.gh.edit(t, 5, func(it *fakeIssue) { it.Body = "body edited on the forge" })

	res := e.pullHeldOn(t, func() {
		e.edit(t, store.IssueUpdate{Title: str("Crane, edited locally")})
		if r := e.pushOn(t, e.pool); r.Pushed != 1 || r.Conflicts != 0 {
			t.Fatalf("other instance's push: %+v, want one push", r)
		}
	})
	if res.Conflicts != 0 {
		t.Errorf("the pull flagged %d conflict(s): it compared with the base of its read, not the mapping row it locked", res.Conflicts)
	}
	e.pullOn(t, e.pool)
	if _, conflict := e.mapping(t); conflict {
		t.Fatalf("the mapping is flagged after the next pull")
	}
	b := getBlock(t, e.pool, e.proj.Scope, e.id(t))
	if it := e.gh.issue(t, 5); b.Title != "#5 "+it.Title || b.Content != it.Body || it.Title != "Crane, edited locally" || it.Body != "body edited on the forge" {
		t.Errorf("block %q / %q, forge %q / %q — want both at the local title and the forge body", b.Title, b.Content, it.Title, it.Body)
	}
	if n := e.cycles(t, 3); n != 0 {
		t.Errorf("three runs landed %d PATCH(es), want 0", n)
	}
	if base, conflict := e.mapping(t); conflict || base != e.forgeH(t) {
		t.Errorf("mapping flagged %v, base is the forge's %v", conflict, base == e.forgeH(t))
	}
}

// TestPullOfAnOlderFetchLeavesANewerPull: this pull fetched the issue and
// read the block at the base; before it writes, the forge changes again and
// another instance pulls that newer version (block and base at it). This
// pull's fetch is older than what the mapping now records: it must not
// write it, or the block falls back to the older version — and with the
// since cursor past the newer one, no later pull would bring it back.
func TestPullOfAnOlderFetchLeavesANewerPull(t *testing.T) {
	e := i491Setup(t, "i491olderfetch", "", fakeIssue{Title: "Crane", Body: "forge body"})
	e.gh.edit(t, 5, func(it *fakeIssue) { it.Body = "body edited on the forge, first" })
	res := e.pullHeldOn(t, func() {
		e.gh.edit(t, 5, func(it *fakeIssue) { it.Body = "body edited on the forge, second" })
		if r := e.pullOn(t, e.pool); r.Applied != 1 {
			t.Fatalf("other instance's pull of the newer version: %+v", r)
		}
	})
	if res.Applied != 0 || res.Conflicts != 0 {
		t.Errorf("the pull of the older fetch: %+v, want nothing written", res)
	}
	if b := getBlock(t, e.pool, e.proj.Scope, e.id(t)); b.Content != "body edited on the forge, second" {
		t.Errorf("the block holds %q, want the newer version kept", b.Content)
	}
	if base, conflict := e.mapping(t); conflict || base != e.forgeH(t) {
		t.Errorf("mapping flagged %v, base is the forge's %v", conflict, base == e.forgeH(t))
	}
}
