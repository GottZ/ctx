//go:build integration

// Issue #509 (T2 of PR #529): the hold a push advance asks for, where it
// leaves the base unknown, survives a hold write that fails or never runs.
// The advance commits first; the hold takes the project row in a
// transaction of its own after it (the project row after the block and the
// mapping row would reverse a resolution's order). When that second
// transaction failed — or the process ended between the two — the mapping
// stood flagged over an unknown base with no hold, the push lists no flagged
// mapping, and nothing set the hold again: another instance that had saved
// its cursor past the entity never listed it again. The advance now marks
// the hold as pending on the mapping row it writes (metadata.hold_pending),
// in its own transaction; the hold write drops the mark, and the next run of
// any instance sets every pending hold before it reads its cursor.
//
//	go test -tags=integration ./internal/forge/ -run TestAHoldThePushCouldNotWriteIsSetByTheNextRun -count=1 -v
package forge

import (
	"context"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/store"
)

// i474FailNextHold makes the next UPDATE of the project row that changes its
// holds fail (a trigger; nextval is not rolled back with the transaction,
// so it fires once).
func (e i491Env) i474FailNextHold(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for _, q := range []string{
		`CREATE SEQUENCE IF NOT EXISTS zz_hold_fault`,
		`CREATE OR REPLACE FUNCTION zz_hold_fault() RETURNS trigger LANGUAGE plpgsql AS $$
		 BEGIN
		   IF NEW.sync_cursor->'holds' IS DISTINCT FROM OLD.sync_cursor->'holds' AND nextval('zz_hold_fault') = 1 THEN
		     RAISE EXCEPTION 'injected: the hold write fails';
		   END IF;
		   RETURN NEW;
		 END $$`,
		`CREATE TRIGGER zz_hold_fault BEFORE UPDATE ON context_projects FOR EACH ROW EXECUTE FUNCTION zz_hold_fault()`,
	} {
		if _, err := e.pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAHoldThePushCouldNotWriteIsSetByTheNextRun(t *testing.T) {
	ctx := context.Background()
	e := i509WithSix(t, "hps", true)
	other := e.otherInstance(t)
	e.edit(t, store.IssueUpdate{Title: str("Crane, A")})
	e.i509ThirdVersion(t, other, func() {
		// The PATCH landed. #6 changes later, and another instance's sync run
		// lists #5 (nothing to do) and saves since past it, with the ETag.
		e.gh.edit(t, 6, func(it *fakeIssue) { it.Body = "six, edited later" })
		if st, err := e.i509Run(other); err != nil || st.Conflicts != 0 {
			t.Errorf("the other instance's run inside the window: %+v %v", st, err)
		}
		e.i474FailNextHold(t)
	})
	r, err := NewPusher(e.pool, e.reg.SnapshotForTenant).PushProject(ctx, e.proj, e.gh.client(), e.gh.repo(), alwaysAllow)
	if err == nil || !strings.Contains(err.Error(), "injected") || r.Pushed != 1 {
		t.Fatalf("the push: %+v %v — want the PATCH and a failed hold write", r, err)
	}
	if base, flagged := e.mapping(t); !flagged || base != store.UnknownBaseHash {
		t.Fatalf("precondition: after the advance base %q, flagged %v — want unknown and flagged", base, flagged)
	}
	if _, since := e.i509Cursor(t, "issues"); !since.After(e.gh.issue(t, 5).Updated) {
		t.Fatalf("precondition: the saved since %v is not past #5's stamp", since)
	}
	// The next run of another instance (not the one whose hold failed).
	for run := 1; run <= 2; run++ {
		st, err := e.i509Run(other)
		if err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		t.Logf("run %d: fetched %d", run, st.Fetched)
	}
	if base, flagged := e.mapping(t); flagged || base != e.forgeH(t) {
		t.Errorf("STALE FLAG: two runs after an advance whose hold write failed, base is the forge's %v, flagged %v — block and forge agree, and no run listed #5 again", base == e.forgeH(t), flagged)
	}
	var pending bool
	if err := e.pool.QueryRow(ctx, `SELECT metadata ? 'hold_pending' FROM context_project_sync_map WHERE block_id = $1::uuid`, e.id(t)).Scan(&pending); err != nil || pending {
		t.Errorf("the pending mark after the runs: %v %v — want it gone", pending, err)
	}
}

// TestALocalResolutionSetsAHoldThePushLeftPending: the hold write failed,
// and a local resolution comes before any run. It drops the pending mark
// with the flag, so it sets the hold itself (under the project row, which
// a resolution now takes first in both directions).
func TestALocalResolutionSetsAHoldThePushLeftPending(t *testing.T) {
	ctx := context.Background()
	e := i509WithSix(t, "hpl", true)
	other := e.otherInstance(t)
	e.edit(t, store.IssueUpdate{Title: str("Crane, A")})
	e.i509ThirdVersion(t, other, func() { e.i474FailNextHold(t) })
	if _, err := NewPusher(e.pool, e.reg.SnapshotForTenant).PushProject(ctx, e.proj, e.gh.client(), e.gh.repo(), alwaysAllow); err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("the push: %v — want a failed hold write", err)
	}
	res, err := e.resolverOn(e.pool).ResolveConflict(ctx, e.proj, e.id(t), ResolveLocal, "", store.AuditStamp{})
	if err != nil || !res.Changed {
		t.Fatalf("resolve local: %+v %v", res, err)
	}
	proj, err := store.GetProjectByID(ctx, e.pool, e.proj.ID)
	if err != nil || proj == nil {
		t.Fatalf("project row: %v", err)
	}
	if readHold(proj.SyncCursor, "issues") == "" {
		t.Errorf("after the local resolution the issue leg holds nothing — the hold the push left pending is lost (cursor %s)", proj.SyncCursor)
	}
	var pending bool
	if err := e.pool.QueryRow(ctx, `SELECT metadata ? 'hold_pending' FROM context_project_sync_map WHERE block_id = $1::uuid`, e.id(t)).Scan(&pending); err != nil || pending {
		t.Errorf("the pending mark after the resolution: %v %v — want it gone", pending, err)
	}
}
