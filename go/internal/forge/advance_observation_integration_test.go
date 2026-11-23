//go:build integration

// Issue #509, the follow-up list of 2026-11-23: two things a push advance
// lost about the forge.
//
//   - A newer, divergent forge snapshot. A push sends B from base A; right
//     after the PATCH lands, a remote edit C follows and another instance's
//     pull flags the mapping with the snapshot C (its stamp later than the
//     PATCH's answer). The advance wrote base B and dropped the snapshot:
//     the conflict list showed no forge side, a local resolution kept base B,
//     and the next fetch of C — the stamp the row records, the base unmoved
//     — pulled C over the block although the local version was chosen. The
//     advance now keeps an observation younger than its PATCH's answer, and
//     the local resolution pushes B over C.
//   - No cursor hold when the advance leaves the base unknown. A pull of
//     another instance moves the base to a third version while the PATCH is
//     on the wire (block and forge agree on it); a sync run of another
//     instance lists the entity between the PATCH's landing and the advance,
//     finds nothing to do and saves since past it, with the listing's ETag.
//     The advance then records the base unknown and flags the mapping, and
//     no later run listed the entity again: block and forge agreed, the flag
//     stayed, and a later local edit was never pushed. The advance now holds
//     the leg at the stamp of the forge's newest version it knows, so the
//     next run lists the entity and decides on it — here a convergence that
//     clears the flag.
//
//	go test -tags=integration ./internal/forge/ -run 'TestAPushAdvanceKeepsANewerForgeSnapshot|TestAnAdvanceThatLeavesTheBaseUnknownHoldsTheLeg' -count=1 -v
package forge

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/store"
)

func TestAPushAdvanceKeepsANewerForgeSnapshot(t *testing.T) {
	ctx := context.Background()
	const local, remote = "local body, pushed", "remote body, a second after the PATCH"
	e := i491Setup(t, "aks", "", fakeIssue{Title: "Crane", Body: "forge body"})
	other := e.otherInstance(t)
	e.edit(t, store.IssueUpdate{Content: str(local)})
	e.gh.seam(nil, func() {
		// The PATCH landed. A remote edit follows, and another instance's
		// pull flags #5 over it (block, base and forge all apart).
		e.gh.edit(t, 5, func(it *fakeIssue) { it.Body = remote })
		r, err := e.applier(other).ApplyIssues(ctx, e.proj, e.fetch(t))
		if err != nil || r.Conflicts != 1 {
			t.Errorf("the other instance's pull of the remote edit: %+v %v, want a new flag", r, err)
		}
	})
	if r := e.pushOn(t, e.pool); r.Pushed != 1 {
		t.Fatalf("the push: %+v", r)
	}
	if _, flagged := e.mapping(t); !flagged {
		t.Fatal("precondition: the flag over the remote edit does not stand after the advance")
	}
	cs, _, err := e.resolverOn(e.pool).ListConflicts(ctx, e.proj, "", 0)
	if err != nil || len(cs) != 1 {
		t.Fatalf("conflict list: %+v %v", cs, err)
	}
	if c := cs[0]; !c.Forge.SeenCurrent || c.Forge.Seen == nil || c.Forge.Seen.Hash != e.forgeH(t) {
		t.Errorf("the conflict list after the advance: forge side current %v, seen %+v — want the remote edit the flagging pull saw", c.Forge.SeenCurrent, c.Forge.Seen)
	}
	res, err := e.resolverOn(e.pool).ResolveConflict(ctx, e.proj, e.id(t), ResolveLocal, "", store.AuditStamp{})
	if err != nil || !res.Changed {
		t.Fatalf("resolve local: %+v %v", res, err)
	}
	t.Logf("local resolution: basis %q", res.Basis)
	e.cycles(t, 2)
	if got, forge := e.body(t), e.gh.issue(t, 5).Body; got != local || forge != local {
		t.Errorf("after the local resolution and two runs: block %q, forge %q — want the local version on both sides", got, forge)
	}
}

// TestAnUnknownBaseKeepsTheForgeVersionAPullAppliedAfterThePATCH: while the
// PATCH's answer is on its way, another instance converges on the sent
// version, pulls a remote edit R over it (a version later than the PATCH)
// and a local edit E follows. The advance finds the base at R — neither the
// listed nor the sent version — and records it unknown; R, a pull's
// observation younger than the PATCH's answer, becomes the flag's forge
// side. The held listing finds R at the stamp the row records (a flagged
// conflict with that stamp ends on the read), so without it no snapshot
// was ever recorded; keeping the local version now diffs E against R.
func TestAnUnknownBaseKeepsTheForgeVersionAPullAppliedAfterThePATCH(t *testing.T) {
	ctx := context.Background()
	const remote, local = "remote body after the PATCH", "local body after the pull of the remote edit"
	e := i491Setup(t, "aur", "", fakeIssue{Title: "Crane", Body: "forge body"})
	other := e.otherInstance(t)
	e.edit(t, store.IssueUpdate{Title: str("Crane, A")})
	e.gh.seam(nil, func() {
		if r, err := e.applier(other).ApplyIssues(ctx, e.proj, e.fetch(t)); err != nil || r.Conflicts != 0 {
			t.Errorf("the other instance's convergence on the sent version: %+v %v", r, err)
		}
		e.gh.edit(t, 5, func(it *fakeIssue) { it.Body = remote })
		if r, err := e.applier(other).ApplyIssues(ctx, e.proj, e.fetch(t)); err != nil || r.Applied != 1 {
			t.Errorf("the other instance's pull of the remote edit: %+v %v", r, err)
		}
		e.edit(t, store.IssueUpdate{Content: str(local)})
	})
	if r := e.pushOn(t, e.pool); r.Pushed != 1 || r.Conflicts != 1 {
		t.Fatalf("the push: %+v — want the PATCH and the advance's flag over the unknown base", r)
	}
	cs, _, err := e.resolverOn(e.pool).ListConflicts(ctx, e.proj, "", 0)
	if err != nil || len(cs) != 1 {
		t.Fatalf("conflict list: %+v %v", cs, err)
	}
	if c := cs[0]; c.Base.Hash != store.UnknownBaseHash || !c.Forge.SeenCurrent || c.Forge.Seen == nil || c.Forge.Seen.Hash != e.forgeH(t) {
		t.Errorf("the conflict list after the advance: base %q, forge side current %v, seen %+v — want the unknown base and the remote edit as the forge side", c.Base.Hash, c.Forge.SeenCurrent, c.Forge.Seen)
	}
	e.i509MustRun(t) // the held listing
	res, err := e.resolverOn(e.pool).ResolveConflict(ctx, e.proj, e.id(t), ResolveLocal, "", store.AuditStamp{})
	if err != nil || !res.Changed || res.Basis != "forge_seen" {
		t.Fatalf("resolve local: %+v %v — want the remote edit as the basis", res, err)
	}
	n := len(e.gh.landed())
	e.pushOn(t, e.pool)
	if got := e.gh.landed(); len(got) != n+1 || len(got[n].Fields) != 1 || got[n].Fields["body"] == nil {
		t.Errorf("the push after the local resolution: %+v — want one PATCH of the body only (the title the forge holds already)", got[n:])
	}
	if forge := e.gh.issue(t, 5); forge.Body != local || forge.Title != "Crane, A" {
		t.Errorf("the forge after the local resolution: %+v", forge)
	}
}

func TestAnAdvanceThatLeavesTheBaseUnknownHoldsTheLeg(t *testing.T) {
	e := i509WithSix(t, "auh", true)
	other := e.otherInstance(t)
	e.edit(t, store.IssueUpdate{Title: str("Crane, A")})
	e.i509ThirdVersion(t, other, func() {
		// The PATCH landed. #6 changes later, and another instance's sync run
		// lists #5 (nothing to do: block, base and forge at the third
		// version) and saves since past it, with the listing's ETag.
		e.gh.edit(t, 6, func(it *fakeIssue) { it.Body = "six, edited later" })
		if st, err := e.i509Run(other); err != nil || st.Conflicts != 0 {
			t.Errorf("the other instance's run inside the window: %+v %v", st, err)
		}
	})
	if r := e.pushOn(t, e.pool); r.Pushed != 1 || r.Conflicts != 1 {
		t.Fatalf("the push: %+v — want the PATCH and the advance's flag over the unknown base", r)
	}
	if base, flagged := e.mapping(t); !flagged || base != store.UnknownBaseHash {
		t.Fatalf("precondition: after the advance base %q, flagged %v — want unknown and flagged", base, flagged)
	}
	// The hold write set the hold and dropped the advance's pending mark.
	var pending bool
	if err := e.pool.QueryRow(context.Background(), `SELECT metadata ? 'hold_pending' FROM context_project_sync_map WHERE block_id = $1::uuid`, e.id(t)).Scan(&pending); err != nil || pending {
		t.Errorf("the pending mark after the hold write: %v %v — want it dropped", pending, err)
	}
	for run := 1; run <= 2; run++ {
		st := e.i509MustRun(t)
		t.Logf("run %d: fetched %d", run, st.Fetched)
	}
	if base, flagged := e.mapping(t); flagged || base != e.forgeH(t) {
		t.Errorf("STALE FLAG: two runs after the advance, base is the forge's %v, flagged %v — block and forge agree, and no run listed #5 again", base == e.forgeH(t), flagged)
	}
}
