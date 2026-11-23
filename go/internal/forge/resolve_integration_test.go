//go:build integration

// The resolve surface of a conflict flag (issue #509), against the fake
// forge over the production legs: a sync run (SyncManager with the
// production cursor), the push leg, and the Resolver on the same database.
// Every flag is set by the production pull or push.
package forge

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
)

const (
	i509Local  = "body L, edited locally"
	i509Remote = "body R, edited on the forge"
)

func (e i491Env) resolverOn(pool *pgxpool.Pool) *Resolver {
	return NewResolver(pool, e.reg.SnapshotForTenant)
}

func (e i491Env) resolve(t *testing.T, direction string) ResolveResult {
	t.Helper()
	r, err := e.resolverOn(e.pool).ResolveConflict(context.Background(), e.proj, e.id(t), direction, "", store.AuditStamp{})
	if err != nil {
		t.Fatalf("resolve %s: %v", direction, err)
	}
	return r
}

// i509Diverged is #5 flagged by a sync run over a real divergence: a local
// body edit and a remote one. relisted: one more run before the return, so
// the flagging run's cursor hold is spent — the forge answers 304 to the
// leg's ETag from then on.
func i509Diverged(t *testing.T, slug string, relisted bool) i491Env {
	t.Helper()
	e := i509WithSix(t, slug, true)
	e.edit(t, store.IssueUpdate{Content: str(i509Local)})
	e.gh.edit(t, 5, func(it *fakeIssue) { it.Body = i509Remote })
	if st := e.i509MustRun(t); st.Conflicts != 1 {
		t.Fatalf("the run over both edits: %+v", st)
	}
	if relisted {
		e.i509MustRun(t)
	}
	if _, flagged := e.mapping(t); !flagged {
		t.Fatal("precondition: #5 is not flagged")
	}
	return e
}

func (e i491Env) auditRows(t *testing.T, action string) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM context_settings_audit WHERE entity_type = 'forge_sync' AND action = $1 AND entity_key = $2`,
		action, e.id(t)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The remote direction holds the issue leg's cursor: after the run that
// relisted the flagged entity, the leg's ETag answers 304 and since lies
// past #5, so without the hold the next run would not list it and the block
// kept the local edit with the base at it — block and forge apart, no flag.
func TestResolveRemoteListsTheEntityAgainAndPullsTheForge(t *testing.T) {
	for _, relisted := range []bool{false, true} {
		name := map[bool]string{false: "right after the flag", true: "after the relisting run"}[relisted]
		t.Run(name, func(t *testing.T) {
			e := i509Diverged(t, "rsr", relisted)
			r := e.resolve(t, ResolveRemote)
			if !r.Changed || r.Leg != "issues" || r.HeldSince == nil {
				t.Fatalf("resolve remote: %+v", r)
			}
			if base, flagged := e.mapping(t); flagged || base == e.forgeH(t) {
				t.Fatalf("after the resolution: flagged %v, base is the forge's %v — want the flag gone and the base at the block", flagged, base == e.forgeH(t))
			}
			st := e.i509MustRun(t)
			e.i509WantPulled(t, i509Remote, "the run after the remote resolution")
			if st.Applied != 1 {
				t.Errorf("the run after the remote resolution applied %d, want the pull of #5", st.Applied)
			}
			if n := e.cycles(t, 2); n != 0 {
				t.Errorf("%d PATCH(es) after the remote resolution, want none", n)
			}
			if got := e.auditRows(t, "resolve-remote"); got != 1 {
				t.Errorf("%d audit rows, want 1", got)
			}
		})
	}
}

// The local direction diffs the block against the forge version the flag
// recorded: a run that lists that version again reads ctx-ahead and leaves
// it to the push, which sends the block's body over the forge's.
func TestResolveLocalPushesTheBlockOverTheForge(t *testing.T) {
	for _, relisted := range []bool{false, true} {
		name := map[bool]string{false: "right after the flag", true: "after the relisting run"}[relisted]
		t.Run(name, func(t *testing.T) {
			e := i509Diverged(t, "rsl", relisted)
			r := e.resolve(t, ResolveLocal)
			if !r.Changed || r.Basis != "forge_seen" {
				t.Fatalf("resolve local: %+v", r)
			}
			if st := e.i509MustRun(t); st.Conflicts != 0 {
				t.Fatalf("the run after the local resolution flagged again: %+v", st)
			}
			if _, flagged := e.mapping(t); flagged {
				t.Fatal("flagged again after the local resolution")
			}
			before := len(e.gh.landed())
			if pr := e.pushOn(t, e.pool); pr.Pushed != 1 {
				t.Fatalf("push after the local resolution: %+v", pr)
			}
			sent := e.gh.landed()[before:]
			if len(sent) != 1 || e.gh.issue(t, 5).Body != i509Local {
				t.Fatalf("the forge holds %q after %d PATCH(es), want the local body", e.gh.issue(t, 5).Body, len(sent))
			}
			if base, flagged := e.mapping(t); flagged || base != e.forgeH(t) || e.body(t) != i509Local {
				t.Errorf("after the push: flagged %v, base is the forge's %v, block %q", flagged, base == e.forgeH(t), e.body(t))
			}
			if n := e.cycles(t, 2); n != 0 {
				t.Errorf("%d more PATCH(es), want none", n)
			}
		})
	}
}

// A remote edit newer than the version the local resolution kept against
// is a new conflict: the user resolved against R, not against R2.
func TestResolveLocalDoesNotCoverALaterRemoteEdit(t *testing.T) {
	e := i509Diverged(t, "rsn", true)
	e.resolve(t, ResolveLocal)
	e.gh.edit(t, 5, func(it *fakeIssue) { it.Body = "body R2, edited on the forge after the resolution" })
	if st := e.i509MustRun(t); st.Conflicts != 1 {
		t.Fatalf("the run over the later remote edit: %+v, want a new flag", st)
	}
	if n := len(e.gh.landed()); e.pushOn(t, e.pool).Pushed != 0 || len(e.gh.landed()) != n {
		t.Error("the push sent the local version over a remote edit nobody resolved")
	}
}

// i509FlagKeptByTheRecord is the push's first limit: A's PATCH of L1 landed,
// a local edit L2 followed and B's pull flagged the mapping (block L2, forge
// L1) before A's advance, which records L1 as the base and keeps the flag.
// The advance drops the forge snapshot the flag recorded.
func i509FlagKeptByTheRecord(t *testing.T, slug string) i491Env {
	t.Helper()
	e := i509WithSix(t, slug, true)
	e.edit(t, store.IssueUpdate{Content: str("body L1, pushed")})
	e.gh.seam(nil, func() {
		e.edit(t, store.IssueUpdate{Content: str("body L2, edited while the PATCH was on the wire")})
		if r := e.pullOn(t, e.pool); r.Conflicts != 1 {
			t.Errorf("B's pull in the window: %+v, want a flag", r)
		}
	})
	if pr := e.pushOn(t, e.pool); pr.Pushed != 1 {
		t.Fatalf("A's push: %+v", pr)
	}
	if _, flagged := e.mapping(t); !flagged {
		t.Fatal("precondition: the flag is gone after A's record")
	}
	cs, _, err := e.resolverOn(e.pool).ListConflicts(context.Background(), e.proj, "", 0)
	if err != nil || len(cs) != 1 || cs[0].Forge.SeenCurrent || cs[0].Cell != "forge_unknown" {
		t.Fatalf("the listing of the flag A's record kept: %+v %v — want one row whose forge side is unknown", cs, err)
	}
	return e
}

func TestResolveAFlagThePushRecordKept(t *testing.T) {
	t.Run("local: the push sends the local edit against the base the forge holds", func(t *testing.T) {
		e := i509FlagKeptByTheRecord(t, "rkl")
		if r := e.resolve(t, ResolveLocal); r.Basis != "base" {
			t.Fatalf("resolve local: %+v, want the row's base as the basis", r)
		}
		e.i509MustRun(t)
		if pr := e.pushOn(t, e.pool); pr.Pushed != 1 {
			t.Fatalf("push after the local resolution: %+v", pr)
		}
		if got := e.gh.issue(t, 5).Body; got != "body L2, edited while the PATCH was on the wire" {
			t.Errorf("the forge holds %q, want L2", got)
		}
		if base, flagged := e.mapping(t); flagged || base != e.forgeH(t) {
			t.Errorf("after the push: flagged %v, base is the forge's %v", flagged, base == e.forgeH(t))
		}
	})
	t.Run("remote: the next run pulls the forge's L1", func(t *testing.T) {
		e := i509FlagKeptByTheRecord(t, "rkr")
		e.resolve(t, ResolveRemote)
		e.i509MustRun(t)
		e.i509WantPulled(t, "body L1, pushed", "the run after the remote resolution")
	})
}

// A comment flag resolves the same way, on the comment leg.
func TestResolveACommentFlag(t *testing.T) {
	setup := func(t *testing.T, slug string) (i491Env, string) {
		e := i491Setup(t, slug, "", fakeIssue{Title: "Crane", Body: "forge body"})
		cid := i509CommentEnv(t, e)
		e.gh.setETags()
		e.i509MustRun(t)
		e.editComment(t, cid, "comment L, edited locally")
		e.gh.editComment(t, 700, "comment R, edited on the forge")
		if st := e.i509MustRun(t); st.Conflicts != 1 {
			t.Fatalf("the run over both comment edits: %+v", st)
		}
		e.i509MustRun(t)
		return e, cid
	}
	resolve := func(t *testing.T, e i491Env, cid, dir string) ResolveResult {
		r, err := e.resolverOn(e.pool).ResolveConflict(context.Background(), e.proj, cid, dir, "", store.AuditStamp{})
		if err != nil || !r.Changed {
			t.Fatalf("resolve %s: %+v %v", dir, r, err)
		}
		return r
	}
	t.Run("local", func(t *testing.T) {
		e, cid := setup(t, "rcl")
		resolve(t, e, cid, ResolveLocal)
		e.i509MustRun(t)
		if pr := e.pushOn(t, e.pool); pr.Pushed != 1 || e.gh.comment(t, 700).Body != "comment L, edited locally" {
			t.Errorf("push after the local resolution: %+v, forge comment %q", pr, e.gh.comment(t, 700).Body)
		}
	})
	t.Run("remote", func(t *testing.T) {
		e, cid := setup(t, "rcr")
		if r := resolve(t, e, cid, ResolveRemote); r.Leg != "comments" {
			t.Fatalf("resolve remote held leg %q, want comments", r.Leg)
		}
		e.i509MustRun(t)
		_, flagged, _, _ := readMapping(t, e.pool, e.proj.ID, "comment", 700)
		if got := getBlock(t, e.pool, e.proj.Scope, cid).Content; flagged || got != "comment R, edited on the forge" {
			t.Errorf("after the run: comment block %q, flagged %v — want the forge's comment pulled", got, flagged)
		}
	})
}

// A repeated resolution writes nothing; a resolution decided on a listing
// is refused once the flag it names is gone or another.
func TestResolveIsIdempotentAndHonoursTheListedFlag(t *testing.T) {
	e := i509Diverged(t, "rsi", true)
	ctx := context.Background()
	cs, _, err := e.resolverOn(e.pool).ListConflicts(ctx, e.proj, "", 0)
	if err != nil || len(cs) != 1 {
		t.Fatalf("listing: %+v %v", cs, err)
	}
	c := cs[0]
	if c.Cell != "diverged" || !c.Forge.SeenCurrent || c.Forge.Seen == nil || c.Base.Hash == c.Block.Hash || c.Forge.Seen.Hash != e.forgeH(t) {
		t.Fatalf("the listed row: %+v", c)
	}
	if _, err := e.resolverOn(e.pool).ResolveConflict(ctx, e.proj, e.id(t), ResolveRemote, "0123456789abcdef", store.AuditStamp{}); !errors.Is(err, ErrFlagChanged) {
		t.Fatalf("a resolution naming another flag: %v, want ErrFlagChanged", err)
	}
	if _, flagged := e.mapping(t); !flagged || e.auditRows(t, "resolve-remote") != 0 {
		t.Fatal("the refused resolution wrote")
	}
	if r, err := e.resolverOn(e.pool).ResolveConflict(ctx, e.proj, e.id(t), ResolveRemote, c.Flag, store.AuditStamp{}); err != nil || !r.Changed {
		t.Fatalf("the resolution naming the listed flag: %+v %v", r, err)
	}
	if r, err := e.resolverOn(e.pool).ResolveConflict(ctx, e.proj, e.id(t), ResolveRemote, c.Flag, store.AuditStamp{}); err != nil || r.Changed {
		t.Fatalf("the repeated resolution: %+v %v, want unchanged", r, err)
	}
	if got := e.auditRows(t, "resolve-remote"); got != 1 {
		t.Errorf("%d audit rows after a repeat, want 1", got)
	}
	if _, err := e.resolverOn(e.pool).ResolveConflict(ctx, e.proj, e.id(t), "both", "", store.AuditStamp{}); !errors.Is(err, ErrResolveDirection) {
		t.Errorf("an unknown direction: %v", err)
	}
	other := e.proj
	other.ID = "00000000-0000-7000-8000-000000000001"
	if _, err := e.resolverOn(e.pool).ResolveConflict(ctx, other, e.id(t), ResolveLocal, "", store.AuditStamp{}); !errors.Is(err, ErrConflictNotFound) {
		t.Errorf("a resolution through another project: %v, want ErrConflictNotFound", err)
	}
}

// The listing pages in block id order.
func TestListConflictsPages(t *testing.T) {
	e := i509Diverged(t, "rsp", true)
	e.gh.edit(t, 6, func(it *fakeIssue) { it.Body = "six, edited on the forge" })
	_, _, six, _ := readMapping(t, e.pool, e.proj.ID, "issue", 6)
	tx, err := e.pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateIssueBlock(context.Background(), tx, six, store.IssueUpdate{Content: str("six, edited locally")}, e.set(), []string{e.proj.Scope}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := e.i509MustRun(t); st.Conflicts != 1 {
		t.Fatalf("the run that flags #6: %+v", st)
	}
	r := e.resolverOn(e.pool)
	p1, next, err := r.ListConflicts(context.Background(), e.proj, "", 1)
	if err != nil || len(p1) != 1 || next == "" {
		t.Fatalf("page 1: %+v next %q %v", p1, next, err)
	}
	p2, next2, err := r.ListConflicts(context.Background(), e.proj, next, 1)
	if err != nil || len(p2) != 1 || p2[0].BlockID == p1[0].BlockID || p2[0].BlockID < p1[0].BlockID {
		t.Fatalf("page 2: %+v next %q %v", p2, next2, err)
	}
	if p3, _, err := r.ListConflicts(context.Background(), e.proj, p2[0].BlockID, 1); err != nil || len(p3) != 0 {
		t.Fatalf("page 3: %+v %v", p3, err)
	}
}

// i509WaitLockWaiter waits until a backend of the test database waits on a
// heavyweight lock in a statement containing what (a condition polled
// against pg_stat_activity, not a sleep).
func (e i491Env) i509WaitLockWaiter(t *testing.T, what string) {
	t.Helper()
	for deadline := time.Now().Add(i452Wait); time.Now().Before(deadline); time.Sleep(2 * time.Millisecond) {
		var n int
		if err := e.pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_stat_activity
			  WHERE datname = current_database() AND wait_event_type = 'Lock' AND query LIKE '%' || $1 || '%'`, what).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
	}
	t.Fatalf("no backend waits on a lock in a statement with %q", what)
}

// A resolution and a pull of the same entity serialise on the block row: a
// pull whose read converged on the flagged row (the block adopted the
// forge's version) and a resolution in either direction, in both orders.
func TestResolveAgainstAConcurrentPull(t *testing.T) {
	isLockSyncMap := func(sql string) bool {
		return strings.Contains(sql, "FROM context_project_sync_map") && strings.Contains(sql, "WHERE block_id = $1::uuid") &&
			strings.Contains(sql, "FOR NO KEY UPDATE") && !strings.Contains(sql, "OF m")
	}
	isResolveMapLock := func(sql string) bool { return strings.Contains(sql, "FOR NO KEY UPDATE OF m") }
	for _, dir := range []string{ResolveLocal, ResolveRemote} {
		t.Run(dir+": the resolution waits for the pull", func(t *testing.T) {
			e := i509Diverged(t, "rcp", true)
			e.edit(t, store.IssueUpdate{Content: str(i509Remote)})
			held, hold := i491HoldStatementPool(t, e.dsn, isLockSyncMap)
			page := e.fetch(t)
			done := make(chan ApplyResult, 1)
			go func() {
				r, err := e.applier(held).ApplyIssues(context.Background(), e.proj, page)
				if err != nil {
					t.Errorf("the pull: %v", err)
				}
				done <- r
			}()
			pzWait(t, hold.entered, "the pull at its mapping lock")
			got := make(chan ResolveResult, 1)
			go func() {
				r, err := e.resolverOn(e.otherInstance(t)).ResolveConflict(context.Background(), e.proj, e.id(t), dir, "", store.AuditStamp{})
				if err != nil {
					t.Errorf("the resolution: %v", err)
				}
				got <- r
			}()
			e.i509WaitLockWaiter(t, "FROM context_blocks")
			close(hold.release)
			<-done
			r := <-got
			if r.Changed || e.auditRows(t, "resolve-"+dir) != 0 {
				t.Errorf("the resolution after the pull's convergence: %+v — want nothing to resolve", r)
			}
			if base, flagged := e.mapping(t); flagged || base != e.forgeH(t) {
				t.Errorf("after both: flagged %v, base is the forge's %v", flagged, base == e.forgeH(t))
			}
			if proj, _ := store.GetProjectByID(context.Background(), e.pool, e.proj.ID); readHold(proj.SyncCursor, "issues") != "" {
				t.Error("a resolution that changed nothing left a cursor hold")
			}
		})
		t.Run(dir+": the pull waits for the resolution", func(t *testing.T) {
			e := i509Diverged(t, "rcq", true)
			e.edit(t, store.IssueUpdate{Content: str(i509Remote)})
			held, hold := i491HoldStatementPool(t, e.dsn, isResolveMapLock)
			got := make(chan ResolveResult, 1)
			go func() {
				r, err := e.resolverOn(held).ResolveConflict(context.Background(), e.proj, e.id(t), dir, "", store.AuditStamp{})
				if err != nil {
					t.Errorf("the resolution: %v", err)
				}
				got <- r
			}()
			pzWait(t, hold.entered, "the resolution at its mapping lock")
			page := e.fetch(t)
			done := make(chan ApplyResult, 1)
			go func() {
				r, err := e.applier(e.otherInstance(t)).ApplyIssues(context.Background(), e.proj, page)
				if err != nil {
					t.Errorf("the pull: %v", err)
				}
				done <- r
			}()
			e.i509WaitLockWaiter(t, "FROM context_blocks")
			close(hold.release)
			r := <-got
			pr := <-done
			if !r.Changed || e.auditRows(t, "resolve-"+dir) != 1 {
				t.Errorf("the resolution before the pull: %+v", r)
			}
			if base, flagged := e.mapping(t); flagged || base != e.forgeH(t) || pr.Conflicts != 0 {
				t.Errorf("after both: flagged %v, base is the forge's %v, pull %+v", flagged, base == e.forgeH(t), pr)
			}
		})
	}
}

// A resolution and a push of the same entity exclude each other through
// the entity's push lock: a resolution while the push's PATCH landed and
// its advance has not run is refused (ErrPushInFlight) and writes nothing;
// a push section that starts while a resolution holds the lock leaves the
// entity to the next run, which pushes the resolved local version.
func TestResolveAgainstAConcurrentPush(t *testing.T) {
	t.Run("the push holds the entity", func(t *testing.T) {
		e := i509WithSix(t, "rpp", true)
		e.edit(t, store.IssueUpdate{Content: str("body L1, pushed")})
		isAdvance := func(sql string) bool {
			return strings.Contains(sql, "COALESCE(workflow_status, ''), updated_at") && strings.Contains(sql, "FOR NO KEY UPDATE")
		}
		held, hold := i491HoldStatementPool(t, e.dsn, isAdvance)
		e.gh.seam(nil, func() {
			e.edit(t, store.IssueUpdate{Content: str("body L2")})
			e.pullOn(t, e.pool)
		})
		done := make(chan PushResult, 1)
		go func() {
			r, err := NewPusher(held, e.reg.SnapshotForTenant).PushProject(context.Background(), e.proj, e.gh.client(), e.gh.repo(), alwaysAllow)
			if err != nil {
				t.Errorf("push: %v", err)
			}
			done <- r
		}()
		pzWait(t, hold.entered, "the push's advance")
		if _, flagged := e.mapping(t); !flagged {
			t.Fatal("precondition: B's pull did not flag")
		}
		for _, dir := range []string{ResolveLocal, ResolveRemote} {
			if _, err := e.resolverOn(e.pool).ResolveConflict(context.Background(), e.proj, e.id(t), dir, "", store.AuditStamp{}); !errors.Is(err, ErrPushInFlight) {
				t.Errorf("resolve %s while the push holds the entity: %v, want ErrPushInFlight", dir, err)
			}
		}
		if _, flagged := e.mapping(t); !flagged || e.auditRows(t, "resolve-local")+e.auditRows(t, "resolve-remote") != 0 {
			t.Error("a refused resolution wrote")
		}
		close(hold.release)
		<-done
		if r := e.resolve(t, ResolveLocal); !r.Changed {
			t.Fatalf("the resolution after the advance: %+v", r)
		}
	})
	t.Run("the resolution holds the entity", func(t *testing.T) {
		e := i509WithSix(t, "rpq", true)
		e.edit(t, store.IssueUpdate{Content: str(i509Local)})
		held, hold := i491HoldStatementPool(t, e.dsn, i491AtThePushLock)
		done := make(chan PushResult, 1)
		go func() {
			r, err := NewPusher(held, e.reg.SnapshotForTenant).PushProject(context.Background(), e.proj, e.gh.client(), e.gh.repo(), alwaysAllow)
			if err != nil {
				t.Errorf("push: %v", err)
			}
			done <- r
		}()
		pzWait(t, hold.entered, "the push at the entity's lock (listed before the flag)")
		e.gh.edit(t, 5, func(it *fakeIssue) { it.Body = i509Remote })
		if r := e.pullOn(t, e.pool); r.Conflicts != 1 {
			t.Fatalf("the pull that flags: %+v", r)
		}
		heldR, holdR := i491HoldStatementPool(t, e.dsn, func(sql string) bool { return strings.Contains(sql, "FOR NO KEY UPDATE OF m") })
		got := make(chan ResolveResult, 1)
		go func() {
			r, err := e.resolverOn(heldR).ResolveConflict(context.Background(), e.proj, e.id(t), ResolveLocal, "", store.AuditStamp{})
			if err != nil {
				t.Errorf("the resolution: %v", err)
			}
			got <- r
		}()
		pzWait(t, holdR.entered, "the resolution at its mapping lock")
		close(hold.release)
		pr := <-done
		if pr.Pushed != 0 || pr.Skipped != 1 {
			t.Errorf("the push section while the resolution holds the entity: %+v, want it left to the next run", pr)
		}
		close(holdR.release)
		if r := <-got; !r.Changed {
			t.Fatalf("the resolution: %+v", r)
		}
		if pr := e.pushOn(t, e.pool); pr.Pushed != 1 || e.gh.issue(t, 5).Body != i509Local {
			t.Errorf("the next push: %+v, forge %q", pr, e.gh.issue(t, 5).Body)
		}
	})
}

// Keeping the local version of an issue whose body was truncated needs the
// forge version the flag recorded: a flag the push's data-loss guard set
// has none, and the push never sends a truncated body.
func TestResolveLocalOnATruncatedIssueWithoutASnapshotIsRefused(t *testing.T) {
	big := strings.Repeat("x", store.MaxForgeBodyBytes+100)
	e := i491Setup(t, "rtr", "", fakeIssue{Title: "Crane", Body: big})
	e.edit(t, store.IssueUpdate{Content: str(strings.Repeat("y", store.MaxForgeBodyBytes-10))})
	if pr := e.pushOn(t, e.pool); pr.Conflicts != 1 {
		t.Fatalf("the push over a truncated body: %+v, want the data-loss guard's flag", pr)
	}
	if _, err := e.resolverOn(e.pool).ResolveConflict(context.Background(), e.proj, e.id(t), ResolveLocal, "", store.AuditStamp{}); !errors.Is(err, ErrLocalTruncated) {
		t.Fatalf("resolve local: %v, want ErrLocalTruncated", err)
	}
	if r := e.resolve(t, ResolveRemote); !r.Changed {
		t.Fatalf("resolve remote: %+v", r)
	}
}
