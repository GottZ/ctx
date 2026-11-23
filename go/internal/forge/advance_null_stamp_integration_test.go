//go:build integration

// F4 of PR #529's review (a hypothesis): an advance that leaves the base
// unknown holds the entity's leg at the later of the row's forge stamp and
// its PATCH's answer stamp — at the zero time when neither has one, which
// lists the leg from its start. Executed: the only order found that reaches
// it is a stamp reset while the PATCH is on the wire, after a pull moved the
// base, with a forge answer that carries no updated_at (GitHub's always
// does). The stamp reset holds the same leg at the zero time itself — that
// is its form of "list the whole leg" (store.HoldFetchCursor at the zero
// time, Resolver.ResetStamps) — so the advance's hold adds no listing: the
// next run lists the leg once and converges. A hold later than the zero
// time is not known to be at or before the entity's forge stamp (neither
// version's stamp is known), so the full listing is the one that cannot
// miss it. Green before and after; it pins the order.
//
//	go test -tags=integration ./internal/forge/ -run TestAnAdvanceWithoutAnyStampHoldsAsTheStampResetDoes -count=1 -v
package forge

import (
	"context"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
)

func TestAnAdvanceWithoutAnyStampHoldsAsTheStampResetDoes(t *testing.T) {
	ctx := context.Background()
	e := i509WithSix(t, "ans", true)
	other := e.otherInstance(t)
	e.gh.setStampAnswer(fakeStampAbsent)
	e.edit(t, store.IssueUpdate{Title: str("Crane, A")})
	e.i509ThirdVersion(t, other, func() {
		// The pull converged on the third version and recorded its stamp;
		// a stamp reset of #5 follows while the PATCH's answer is on its way.
		if _, err := e.resolverOn(other).ResetStamps(ctx, e.proj, e.id(t), store.AuditStamp{}); err != nil {
			t.Errorf("stamp reset: %v", err)
		}
	})
	if r := e.pushOn(t, e.pool); r.Pushed != 1 || r.Conflicts != 1 {
		t.Fatalf("the push: %+v — want the PATCH and the advance's flag over the unknown base", r)
	}
	if st := e.recordedStamp(t, e.id(t)); st != nil {
		t.Fatalf("precondition: the row's forge stamp is %v, want none", st)
	}
	proj, err := store.GetProjectByID(ctx, e.pool, e.proj.ID)
	if err != nil || proj == nil {
		t.Fatalf("project row: %v", err)
	}
	c := decodeCursor(proj.SyncCursor)
	if h := c.Holds["issues"]; h == nil || !h.At.IsZero() {
		t.Fatalf("the issue leg's hold: %+v — want the zero time (the stamp reset's and the advance's)", h)
	}
	if _, since := e.i509Cursor(t, "issues"); !since.Equal(time.Time{}) {
		t.Errorf("the run after them starts the leg at %v, want its start", since)
	}
	st := e.i509MustRun(t)
	if st.Fetched != 2 {
		t.Errorf("the held run fetched %d issues, want the whole leg (2)", st.Fetched)
	}
	if base, flagged := e.mapping(t); flagged || base != e.forgeH(t) {
		t.Errorf("after the held run: base is the forge's %v, flagged %v — want converged", base == e.forgeH(t), flagged)
	}
	if st := e.i509MustRun(t); st.Fetched >= 2 {
		t.Errorf("the run after it fetched %d issues — the leg was listed whole again", st.Fetched)
	}
}
