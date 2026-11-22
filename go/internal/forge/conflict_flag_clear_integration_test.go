//go:build integration

// Issue #509: a conflict flag goes once a pull finds block, base and forge
// in agreement. Before, the noop cell of the matrix wrote nothing, so a
// mapping flagged while both sides moved stayed flagged after they came
// level: the push lists no flagged mapping, and a later local edit was never
// pushed. A pull that finds the block and the forge at one version on the
// rows it locks now clears the flag and records that version as the base —
// also when the base was already that version. The push advance clears no
// flag: it does not know what the forge holds since its PATCH landed. A flag
// over a real divergence of both sides stays.
//
//	go test -tags=integration ./internal/forge/ -run 'TestAPullClearsAFlag|TestAPushAdvanceKeepsAFlag|TestAFlagOverARealDivergenceStays' -count=1 -v
package forge

import (
	"testing"

	"github.com/GottZ/ctx/internal/store"
)

// i509FlaggedLevel flags the mapping while block, base and forge end on one
// version: a pull of one version of a second waits at its write while
// another instance pulls the other version of that second (the same-stamp
// rule flags, TestPullOfASameSecondFetch). It returns the body all three
// hold.
func i509FlaggedLevel(t *testing.T, slug string) (i491Env, string) {
	t.Helper()
	const first, second = "body edited on the forge, first", "body edited on the forge, second"
	e := i491Setup(t, slug, "", fakeIssue{Title: "Crane", Body: "forge body"})
	since := e.gh.issue(t, 5).Updated
	e.gh.edit(t, 5, func(it *fakeIssue) { it.Body = first })
	pageA := e.i491Page(t, since, "")
	res := e.pullHeldOnPage(t, pageA.Issues, func() {
		e.gh.editInSecond(t, 5, func(it *fakeIssue) { it.Body = second })
		if r := e.applyPage(t, e.i491Page(t, since, "").Issues); r.Applied != 1 {
			t.Fatalf("B's pull of the second version: %+v", r)
		}
	})
	if _, conflict := e.mapping(t); res.Conflicts != 1 || !conflict || e.body(t) != second {
		t.Fatalf("precondition: %+v, flagged %v, block %q — want the mapping flagged over the second version", res, conflict, e.body(t))
	}
	return e, second
}

func TestAPullClearsAFlagWhenBlockBaseAndForgeAgree(t *testing.T) {
	e, body := i509FlaggedLevel(t, "i509clearlevel")
	if r := e.pullOn(t, e.pool); r.Applied != 0 || r.Conflicts != 0 {
		t.Errorf("the pull of the version all three hold: %+v, want nothing written to the block and nothing flagged", r)
	}
	if base, conflict := e.mapping(t); conflict || base != e.forgeH(t) || e.body(t) != body {
		t.Fatalf("flagged %v, base is the forge's %v, block %q — want the flag gone", conflict, base == e.forgeH(t), e.body(t))
	}
	// The flag's time goes with it: a later flag records its own.
	if _, _, _, at := readMapping(t, e.pool, e.proj.ID, "issue", 5); at != nil {
		t.Errorf("conflict_at is %v after the flag went, want none", at)
	}
	// The mapping is a push candidate again: a local edit goes out.
	e.edit(t, store.IssueUpdate{Title: str("Crane, edited locally")})
	if n := e.cycles(t, 2); n != 1 {
		t.Errorf("two runs after a local edit landed %d PATCH(es), want 1", n)
	}
	if it := e.gh.issue(t, 5); it.Title != "Crane, edited locally" {
		t.Errorf("the forge holds the title %q, want the local edit", it.Title)
	}
}

// TestAPullClearsAFlagThePushRecordKept: a local edit lands while the PATCH
// is on the wire, another instance's pull flags the mapping (block at the
// edit, forge at the sent version, base at the listed one), and the advance
// records the sent version as the base and keeps the flag — the block does
// not hold what it sent. The edit is undone: block, base and forge agree,
// and the next pull clears the flag.
func TestAPullClearsAFlagThePushRecordKept(t *testing.T) {
	e := i491Setup(t, "i509clearkept", "", fakeIssue{Title: "Crane", Body: "forge body"})
	e.edit(t, store.IssueUpdate{Title: str("Crane, A")})
	other := e.otherInstance(t)
	e.gh.seam(nil, func() {
		e.edit(t, store.IssueUpdate{Title: str("Crane, edited during the PATCH")})
		if r := e.pullOn(t, other); r.Conflicts != 1 {
			t.Errorf("other instance's pull during the PATCH: %+v, want the mapping flagged", r)
		}
	})
	if r := e.pushOn(t, e.pool); r.Pushed != 1 {
		t.Fatalf("push: %+v", r)
	}
	if _, conflict := e.mapping(t); !conflict {
		t.Fatalf("precondition: the advance cleared the flag although the block does not hold the version it sent")
	}
	e.edit(t, store.IssueUpdate{Title: str("Crane, A")})
	e.pullOn(t, e.pool)
	if base, conflict := e.mapping(t); conflict || base != e.forgeH(t) {
		t.Fatalf("after the edit was undone: flagged %v, base is the forge's %v — want the flag gone", conflict, base == e.forgeH(t))
	}
	if n := e.cycles(t, 2); n != 0 {
		t.Errorf("two runs landed %d PATCH(es), want 0", n)
	}
}

// TestAPushAdvanceKeepsAFlagTheNextPullClears: the same, but the edit is
// undone while the PATCH is still on the wire: the advance finds the block
// at the version it sent and records it as the base, and it keeps the flag
// the other instance's pull set — the advance does not know what the forge
// holds since its PATCH landed (a remote edit after it is what such a flag
// can stand for, TestAFlagSetAcrossAPushAdvanceIsResolvedByTheNextRun). The
// next pull finds block, base and forge at the sent version and clears it.
func TestAPushAdvanceKeepsAFlagTheNextPullClears(t *testing.T) {
	e := i491Setup(t, "i509clearadv", "", fakeIssue{Title: "Crane", Body: "forge body"})
	e.edit(t, store.IssueUpdate{Title: str("Crane, A")})
	other := e.otherInstance(t)
	e.gh.seam(nil, func() {
		e.edit(t, store.IssueUpdate{Title: str("Crane, edited during the PATCH")})
		if r := e.pullOn(t, other); r.Conflicts != 1 {
			t.Errorf("other instance's pull during the PATCH: %+v, want the mapping flagged", r)
		}
		e.edit(t, store.IssueUpdate{Title: str("Crane, A")})
	})
	if r := e.pushOn(t, e.pool); r.Pushed != 1 || r.Conflicts != 0 {
		t.Fatalf("push: %+v", r)
	}
	if base, conflict := e.mapping(t); !conflict || base != e.forgeH(t) {
		t.Fatalf("after the advance: flagged %v, base is the forge's %v — want the sent version recorded and the flag kept", conflict, base == e.forgeH(t))
	}
	if r := e.pullOn(t, e.pool); r.Applied != 0 || r.Conflicts != 0 {
		t.Errorf("the pull after the advance: %+v, want nothing written to the block and nothing flagged", r)
	}
	if base, conflict := e.mapping(t); conflict || base != e.forgeH(t) {
		t.Fatalf("after the next pull: flagged %v, base is the forge's %v — want the flag gone", conflict, base == e.forgeH(t))
	}
	if n := e.cycles(t, 2); n != 0 {
		t.Errorf("two runs landed %d PATCH(es), want 0", n)
	}
}

// TestAFlagOverARealDivergenceStays: both sides moved apart after the flag
// — a local edit and a remote one. The pull writes nothing, the flag stays,
// and the push sends nothing.
func TestAFlagOverARealDivergenceStays(t *testing.T) {
	e, _ := i509FlaggedLevel(t, "i509keepdiv")
	e.edit(t, store.IssueUpdate{Title: str("Crane, edited locally")})
	e.gh.edit(t, 5, func(it *fakeIssue) { it.Body = "body edited on the forge, third" })
	if r := e.pullOn(t, e.pool); r.Applied != 0 || r.Conflicts != 0 {
		t.Errorf("the pull over the flagged divergence: %+v, want nothing written and the conflict not counted again", r)
	}
	if n := e.cycles(t, 2); n != 0 {
		t.Errorf("two runs landed %d PATCH(es) over the flagged mapping, want 0", n)
	}
	b := getBlock(t, e.pool, e.proj.Scope, e.id(t))
	if _, conflict := e.mapping(t); !conflict || b.Title != "#5 Crane, edited locally" || e.gh.issue(t, 5).Body != "body edited on the forge, third" {
		t.Errorf("flagged %v, block %q, forge %q — want the flag and both sides kept", conflict, b.Title, e.gh.issue(t, 5).Body)
	}
}
