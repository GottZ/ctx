//go:build integration

// Issue #503: two ctxd instances pull the same project at the same time, and
// two of its issues reference each other ("see #6" in #5, "see #5" in #6).
// Each pull locks the block it writes FOR NO KEY UPDATE; the reference write
// reads its target FOR SHARE, which keeps the target's scope from moving
// under the link. A pull that locked its target only after its own block met
// the other pull doing the same the other way round, and PostgreSQL ended one
// of them with 40P01. The pull now locks the targets of its references and
// its own block in ascending id order before it writes, so of two such pulls
// one waits before it holds a row the other one needs.
//
// The window is opened by a hold on each pull's pool at its first statement
// that takes a FOR SHARE lock — before the fix the reference lock after the
// block lock, after it the first target lock in id order — until both pulls
// are there; then both go on.
//
//	go test -tags=integration ./internal/forge/ -run TestPullsOfIssuesThatReferenceEachOther -count=1 -v
package forge

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/GottZ/ctx/internal/store"
)

// i503RefEnv pulls #5 ("see #6") and #6 ("see #5") from the fake forge,
// twice, so both blocks and both mappings exist.
func i503RefEnv(t *testing.T, slug string) i491Env {
	t.Helper()
	e := i491Env{i486Env: i486Setup(t, slug, "")}
	e.proj = enablePush(e.proj)
	t0 := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)
	e.gh = newFakeGitHub(t, 100, quotedNext,
		fakeIssue{Number: 5, Updated: t0, Title: "five", Body: "see #6"},
		fakeIssue{Number: 6, Updated: t0, Title: "six", Body: "see #5"})
	e.pullOn(t, e.pool)
	return e
}

// i503Linked answers whether the structural reference source → target exists.
func i503Linked(t *testing.T, e i491Env, source, target string) bool {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM context_structural_links
		  WHERE source_block_id = $1::uuid AND target_block_id = $2::uuid AND link_class = 'references'`,
		source, target).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

func TestPullsOfIssuesThatReferenceEachOtherDoNotDeadlock(t *testing.T) {
	e := i503RefEnv(t, "i503xref")
	ctx := context.Background()
	_, _, id5, _ := readMapping(t, e.pool, e.proj.ID, "issue", 5)
	_, _, id6, _ := readMapping(t, e.pool, e.proj.ID, "issue", 6)

	e.gh.edit(t, 5, func(it *fakeIssue) { it.Body = "see #6, edited on the forge" })
	e.gh.edit(t, 6, func(it *fakeIssue) { it.Body = "see #5, edited on the forge" })
	var i5, i6 []IssueRemote
	for _, it := range e.fetch(t) {
		switch it.Number {
		case 5:
			i5 = append(i5, it)
		case 6:
			i6 = append(i6, it)
		}
	}
	atShareLock := func(sql string) bool { return strings.Contains(sql, "FOR SHARE") }
	pa, ha := i491HoldStatementPool(t, e.dsn, atShareLock)
	pb, hb := i491HoldStatementPool(t, e.dsn, atShareLock)
	errs := make(chan error, 2)
	go func() { _, err := e.applier(pa).ApplyIssues(ctx, e.proj, i5); errs <- err }()
	go func() { _, err := e.applier(pb).ApplyIssues(ctx, e.proj, i6); errs <- err }()
	for _, h := range []*i491StatementHold{ha, hb} {
		select {
		case <-h.entered:
		case <-time.After(i452Wait):
			t.Fatal("a pull did not reach its first FOR SHARE lock")
		}
	}
	close(ha.release)
	close(hb.release)
	for i := 0; i < 2; i++ {
		select {
		case err := <-errs:
			var pg *pgconn.PgError
			switch {
			case errors.As(err, &pg) && pg.Code == "40P01":
				t.Errorf("the two pulls deadlocked (40P01): %v", err)
			case err != nil:
				t.Errorf("pull: %v", err)
			}
		case <-time.After(i452Wait):
			t.Fatal("the two pulls did not finish: a wait cycle")
		}
	}
	if t.Failed() {
		return
	}
	for n, id := range map[int64]string{5: id5, 6: id6} {
		if b := getBlock(t, e.pool, e.proj.Scope, id); !strings.Contains(b.Content, "edited on the forge") {
			t.Errorf("#%d holds %q, want the remote edit", n, b.Content)
		}
	}
	if !i503Linked(t, e, id5, id6) || !i503Linked(t, e, id6, id5) {
		t.Errorf("references 5→6 %v, 6→5 %v — want both", i503Linked(t, e, id5, id6), i503Linked(t, e, id6, id5))
	}
}

// i503LockWaitSeen reports whether another backend of the database waits on
// a heavyweight lock while pending() holds; false as soon as pending() no
// longer holds without such a wait. It observes the wait itself instead of
// sleeping for it.
func i503LockWaitSeen(t *testing.T, e i491Env, pending func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(i452Wait)
	for time.Now().Before(deadline) {
		var waiting int
		if err := e.pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_stat_activity
			  WHERE datname = current_database() AND pid <> pg_backend_pid() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatalf("lock wait probe: %v", err)
		}
		if waiting > 0 {
			return true
		}
		if !pending() {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// TestPullOfAReferenceTargetMovedOutOfScopeWritesNoLink: the reference
// target is still locked FOR SHARE, which keeps its scope from moving under
// the link. A scope move (UpdateBlock with a scope) takes the row FOR NO KEY
// UPDATE — a scope is no key column, so FOR KEY SHARE would not hold it up.
// Either the pull waits for the move and finds the target in the other scope
// (no link), or the move waits for the pull and its sweep then deletes the
// link the pull committed (no link either). A cross-scope link would stay in
// both orders if the target were locked FOR KEY SHARE.
func TestPullOfAReferenceTargetMovedOutOfScopeWritesNoLink(t *testing.T) {
	isCommit := func(sql string) bool { return strings.EqualFold(strings.TrimSpace(sql), "commit") }
	// The move's first link sweep statement: its UPDATE of the block ran.
	isSweep := func(sql string) bool { return strings.Contains(sql, "context_dream_links l SET scope") }
	for _, tc := range []struct {
		name      string
		moveFirst bool
	}{{"the move holds the target first", true}, {"the pull holds the target first", false}} {
		t.Run(tc.name, func(t *testing.T) {
			e := i503RefEnv(t, "i503moved")
			ctx := context.Background()
			_, _, id5, _ := readMapping(t, e.pool, e.proj.ID, "issue", 5)
			_, _, id6, _ := readMapping(t, e.pool, e.proj.ID, "issue", 6)
			other := e.proj.Scope + "-elsewhere"
			e.gh.edit(t, 5, func(it *fakeIssue) { it.Body = "see #6, edited on the forge" })
			var page []IssueRemote
			for _, it := range e.fetch(t) {
				if it.Number == 5 {
					page = append(page, it)
				}
			}
			movePool, moveHold := i491HoldStatementPool(t, e.dsn, isSweep)
			pullPool, pullHold := i491HoldStatementPool(t, e.dsn, isCommit)
			move := make(chan error, 1)
			pull := make(chan error, 1)
			startMove := func() {
				go func() {
					_, _, err := store.UpdateBlock(ctx, movePool, id6, store.UpdateBlockData{Scope: &other}, []string{e.proj.Scope, other})
					move <- err
				}()
			}
			startPull := func() {
				go func() { _, err := e.applier(pullPool).ApplyIssues(ctx, e.proj, page); pull <- err }()
			}
			pending := func(c chan error) func() bool { return func() bool { return len(c) == 0 } }
			if tc.moveFirst {
				// The move holds #6 (its UPDATE ran) and waits before its sweep.
				close(pullHold.release)
				startMove()
				<-moveHold.entered
				startPull()
				if !i503LockWaitSeen(t, e, pending(pull)) {
					t.Fatal("the pull did not wait for the move that holds its reference target")
				}
				close(moveHold.release)
			} else {
				// The pull wrote its block and the link and waits before its commit.
				close(moveHold.release)
				startPull()
				<-pullHold.entered
				startMove()
				if !i503LockWaitSeen(t, e, pending(move)) {
					t.Fatal("the scope move did not wait for the pull that holds the target FOR SHARE")
				}
				close(pullHold.release)
			}
			for _, c := range []chan error{move, pull} {
				select {
				case err := <-c:
					if err != nil {
						t.Fatalf("%v", err)
					}
				case <-time.After(i452Wait):
					t.Fatal("the move or the pull did not finish")
				}
			}
			if b := getBlock(t, e.pool, e.proj.Scope, id5); !strings.Contains(b.Content, "edited on the forge") {
				t.Errorf("#5 holds %q, want the remote edit", b.Content)
			}
			if i503Linked(t, e, id5, id6) {
				t.Errorf("a link from #5 to #6 stays although the move put #6 into another scope")
			}
		})
	}
}
