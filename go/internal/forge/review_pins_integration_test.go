//go:build integration

// Issue #491 (review of the first round, F7): behaviours of the pull and the
// push that no test held, so a mutation of them went unnoticed. Each test
// drives the production legs against the fake forge or holds a production
// statement; none matches on the SQL text of the lock it pins.
//
//	go test -tags=integration ./internal/forge/ -run 'TestForgeStateStampOfAnUnchangedState|TestPushAdvanceOffItsBaseStampsNoForgeState|TestPullConvergenceReadDecidesOnTheLockedRows|TestPullWaitsForAMappingFlagInFlight|TestPushAdvanceKeepsAShareLockerWaiting' -count=1 -v
package forge

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// TestForgeStateStampOfAnUnchangedStateWritesNothing: the push advance
// stamps metadata.forge_state with the state of the version it records
// (store.StampForgeState). When that is the state the block already holds,
// the row is not rewritten — no new row version, and no write notification
// to the listeners of ctx_block_write and ctx_project_write. A stamp of every
// advance would rewrite the block on each push and fire both channels for a
// change that is none.
func TestForgeStateStampOfAnUnchangedStateWritesNothing(t *testing.T) {
	e := i491Setup(t, "i491stampeq", "", fakeIssue{Title: "Crane", Body: "forge body"})
	ctx := context.Background()
	e.edit(t, store.IssueUpdate{Title: str("Crane, A")})
	xmin := func() string {
		t.Helper()
		var x string
		if err := e.pool.QueryRow(ctx, `SELECT xmin::text FROM context_blocks WHERE id = $1::uuid`, e.id(t)).Scan(&x); err != nil {
			t.Fatal(err)
		}
		return x
	}
	lc := e.i491Observer(t)
	for _, ch := range []string{"ctx_block_write", "ctx_project_write"} {
		if _, err := lc.Exec(ctx, `LISTEN `+ch); err != nil {
			t.Fatal(err)
		}
	}
	before := xmin()
	if r := e.pushOn(t, e.pool); r.Pushed != 1 || r.Conflicts != 0 {
		t.Fatalf("push of the title: %+v", r)
	}
	if after := xmin(); after != before {
		t.Errorf("the advance rewrote the block (xmin %s → %s) although its forge state did not change", before, after)
	}
	wctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if n, err := lc.WaitForNotification(wctx); err == nil {
		t.Errorf("the push fired %s for the block: %.120s", n.Channel, n.Payload)
	}
	if fs := metaString(getBlock(t, e.pool, e.proj.Scope, e.id(t)).Metadata, "forge_state"); fs != "open" {
		t.Errorf("metadata.forge_state is %q, want open", fs)
	}
}

// TestPushAdvanceOffItsBaseStampsNoForgeState: the push closes the issue;
// before its advance another instance pulls twice — it converges on the
// close the PATCH sent, then pulls a remote reopen. The advance finds a base
// that is neither the listed one nor the sent one, records the base unknown
// and flags the mapping; it must not stamp the state it sent (closed) over
// the state the pull wrote (open), which is the forge's.
func TestPushAdvanceOffItsBaseStampsNoForgeState(t *testing.T) {
	e := i491Setup(t, "i491stampmoved", "", fakeIssue{Title: "Crane", Body: "forge body"})
	e.move(t, e.id(t), "done")
	var converged, reopened ApplyResult
	e.gh.seam(nil, func() {
		converged = e.pullOn(t, e.pool)
		e.gh.edit(t, 5, func(it *fakeIssue) { it.State = "open"; it.Body = "reopened on the forge" })
		reopened = e.pullOn(t, e.pool)
	})
	r := e.pushOn(t, e.pool)
	if converged.Applied != 0 || converged.Conflicts != 0 || reopened.Applied != 1 {
		t.Fatalf("precondition: the other instance's pulls: %+v then %+v, want the convergence and the reopen applied", converged, reopened)
	}
	if r.Pushed != 1 || r.Conflicts != 1 {
		t.Errorf("push: %+v, want one push and the conflict of the moved base", r)
	}
	if fs := metaString(getBlock(t, e.pool, e.proj.Scope, e.id(t)).Metadata, "forge_state"); fs != "open" {
		t.Errorf("metadata.forge_state is %q after the advance off its base, want open (the state the pull of the reopen wrote)", fs)
	}
}

// TestPullConvergenceReadDecidesOnTheLockedRowsOnceAPushMovedTheBase: the
// pull read a convergence (block and forge renamed alike); before it writes,
// a local edit and another instance's push of it move the base. The read's
// convergence was made against a base that is gone, so the pull decides on
// the locked rows: the push recorded the stamp of its PATCH (issue #509),
// and the fetch predates it — nothing is written, the skip is counted (its
// content differs from the pushed base). Before #509 the row kept the
// pulled version's stamp, the matrix read forge-ahead and wrote the fetch
// over the pushed edit, a step back the next pull healed. The next run
// brings the block level with the forge without a wire write. Recording the
// read's convergence over the pushed base instead would make the next push
// send the pushed edit a second time.
func TestPullConvergenceReadDecidesOnTheLockedRowsOnceAPushMovedTheBase(t *testing.T) {
	e := i491Converging(t, "i491convpushed")
	const body = "body edited locally"
	res := e.pullHeldOnPage(t, e.fetch(t), func() {
		e.edit(t, store.IssueUpdate{Content: str(body)})
		if r := e.pushOn(t, e.pool); r.Pushed != 1 || r.Conflicts != 0 {
			t.Fatalf("other instance's push: %+v", r)
		}
	})
	if res.Applied != 0 || res.Conflicts != 0 || res.Skipped != 1 {
		t.Errorf("the converging pull after the push: %+v, want nothing written (a fetch older than the push's stamp) and the skip counted", res)
	}
	if n := e.cycles(t, 3); n != 0 {
		t.Errorf("three runs landed %d PATCH(es), want 0 (the forge holds the pushed edit)", n)
	}
	b := getBlock(t, e.pool, e.proj.Scope, e.id(t))
	if it := e.gh.issue(t, 5); b.Content != body || it.Body != body || b.Title != "#5 Crane, renamed" {
		t.Errorf("block %q / %q, forge %q — want both at the renamed title and the local body", b.Title, b.Content, it.Body)
	}
	if base, conflict := e.mapping(t); conflict || base != e.forgeH(t) {
		t.Errorf("mapping flagged %v, base is the forge's %v", conflict, base == e.forgeH(t))
	}
}

// TestPullWaitsForAMappingFlagInFlight: the pull locks the mapping row after
// the block row (store.LockSyncMap). The push flags a mapping without the
// block row (FlagPushConflict, a truncated body edited locally); while that
// flag is written and not committed, a pull of a remote edit decides
// "conflict". It must wait for the flag and find it set — a conflict already
// counted — not read the row as it was before the flag and count the same
// conflict a second time.
func TestPullWaitsForAMappingFlagInFlight(t *testing.T) {
	big := strings.Repeat("x", store.MaxForgeBodyBytes+100)
	e := i491Setup(t, "i491flagflight", "", fakeIssue{Title: "Crane", Body: big})
	ctx := context.Background()
	e.edit(t, store.IssueUpdate{Content: str("body edited locally")})
	e.gh.edit(t, 5, func(it *fakeIssue) { it.Body = "y" + big })

	pushPool, hold := testdb.HoldCommitPool(t, e.dsn, func(sql string) bool { return strings.Contains(sql, "SET conflict = true") })
	type pushOut struct {
		res PushResult
		err error
	}
	pushDone := make(chan pushOut, 1)
	go func() {
		r, err := e.pushOnErr(pushPool)
		pushDone <- pushOut{r, err}
	}()
	pid := hold.Wait(t)
	type pullOut struct {
		res ApplyResult
		err error
	}
	pullDone := make(chan pullOut, 1)
	waiting := make(chan struct{})
	page := e.fetch(t)
	go func() {
		defer close(waiting)
		r, err := e.applier(e.pool).ApplyIssues(ctx, e.proj, page)
		pullDone <- pullOut{r, err}
	}()
	testdb.WaitBlockedBy(t, e.pool, pid, waiting, "the pull", "")
	hold.Release()
	p := <-pushDone
	q := <-pullDone
	if p.err != nil || p.res.Conflicts != 1 || p.res.Pushed != 0 {
		t.Errorf("push: %+v %v, want the data-loss conflict and nothing pushed", p.res, p.err)
	}
	if q.err != nil || q.res.Conflicts != 0 || q.res.Applied != 0 {
		t.Errorf("pull: %+v %v, want the flag found set: nothing applied, no conflict counted again", q.res, q.err)
	}
	if _, conflict := e.mapping(t); !conflict {
		t.Errorf("the mapping is not flagged")
	}
	if n := len(e.gh.landed()); n != 0 {
		t.Errorf("%d PATCH(es) landed", n)
	}
}

// TestPushAdvanceKeepsAShareLockerWaiting: the advance locks the block row
// FOR NO KEY UPDATE before anything else, the lock its later forge-state
// stamp (an UPDATE) needs — so it never has to upgrade a lock it shares with
// another transaction. A transaction that locks the row FOR SHARE (as a
// structural link write locks its target) waits for the advance instead of
// sharing the row with it. Held: the advance after its block lock, at its
// mapping statement.
func TestPushAdvanceKeepsAShareLockerWaiting(t *testing.T) {
	e := i491Setup(t, "i491sharewait", "", fakeIssue{Title: "Crane", Body: "forge body"})
	ctx := context.Background()
	e.edit(t, store.IssueUpdate{Title: str("Crane, A")})
	held, hold := i491HoldStatementPool(t, e.dsn, func(sql string) bool {
		return strings.Contains(sql, "UPDATE context_project_sync_map") && strings.Contains(sql, "synced_at = CASE WHEN")
	})
	type out struct {
		res PushResult
		err error
	}
	done := make(chan out, 1)
	go func() {
		r, err := e.pushOnErr(held)
		done <- out{r, err}
	}()
	select {
	case <-hold.entered:
	case o := <-done:
		t.Fatalf("the push finished without reaching its advance: %+v %v", o.res, o.err)
	case <-time.After(i452Wait):
		t.Fatalf("the push did not reach its advance within %s", i452Wait)
	}
	obs := e.i491Observer(t)
	if _, err := obs.Exec(ctx, `SET lock_timeout = '500ms'`); err != nil {
		t.Fatal(err)
	}
	_, err := obs.Exec(ctx, `SELECT 1 FROM context_blocks WHERE id = $1::uuid FOR SHARE`, e.id(t))
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Errorf("a FOR SHARE lock of the block during the advance: %v, want it to wait (SQLSTATE 55P03 at the lock timeout)", err)
	}
	close(hold.release)
	select {
	case o := <-done:
		if o.err != nil || o.res.Pushed != 1 || o.res.Conflicts != 0 {
			t.Errorf("push: %+v %v, want one push", o.res, o.err)
		}
	case <-time.After(i452Wait):
		t.Fatalf("the push did not finish within %s", i452Wait)
	}
}

// TestPushLockIsHeldThroughTheRecord (second review, P2): the section gives
// its push lock back only after the record transaction ended. While the
// advance runs — held at its mapping statement — another instance's try of
// the entity's lock fails; given back before the record, that instance could
// list the entity at the old base, PATCH and record over the advance.
func TestPushLockIsHeldThroughTheRecord(t *testing.T) {
	e := i491Setup(t, "i491lockrecord", "", fakeIssue{Title: "Crane", Body: "forge body"})
	ctx := context.Background()
	e.edit(t, store.IssueUpdate{Title: str("Crane, A")})
	held, hold := i491HoldStatementPool(t, e.dsn, func(sql string) bool {
		return strings.Contains(sql, "UPDATE context_project_sync_map") && strings.Contains(sql, "synced_at = CASE WHEN")
	})
	type out struct {
		res PushResult
		err error
	}
	done := make(chan out, 1)
	go func() {
		r, err := e.pushOnErr(held)
		done <- out{r, err}
	}()
	select {
	case <-hold.entered:
	case o := <-done:
		t.Fatalf("the push finished without reaching its record: %+v %v", o.res, o.err)
	case <-time.After(i452Wait):
		t.Fatalf("the push did not reach its record within %s", i452Wait)
	}
	other, err := pgxpool.New(ctx, e.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	conn, err := other.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	base, _ := e.mapping(t)
	locked, skip, err := store.TryLockPushEntity(ctx, conn, e.id(t), base)
	if locked {
		_, _ = store.UnlockPushEntity(ctx, conn, e.id(t))
	}
	conn.Release()
	if err != nil || locked || skip != store.PushSkipInFlight {
		t.Errorf("another instance's try of the push lock during the record: locked %v, skip %q, %v — want it refused (in flight)", locked, skip, err)
	}
	close(hold.release)
	select {
	case o := <-done:
		if o.err != nil || o.res.Pushed != 1 || o.res.Conflicts != 0 {
			t.Errorf("push: %+v %v, want one push", o.res, o.err)
		}
	case <-time.After(i452Wait):
		t.Fatalf("the push did not finish within %s", i452Wait)
	}
}
