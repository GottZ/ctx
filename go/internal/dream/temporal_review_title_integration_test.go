//go:build integration

// Issue #427, the title: the dream's temporal review sends the block's title
// to the LLM as well as its content, so its findings land only while the
// block still holds both (store.PopulateTemporalReview). The rig and its
// helpers are those of temporal_stamp_integration_test.go.
//
// Run with:
//
//	go test -tags=integration ./internal/dream/ -run TestTemporalReviewTitle -count=1 -v
package dream_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// i427TitleDate is a review that reads the date off the title it was sent,
// the way the LLM reads a date the title states.
var i427TitleDateRE = regexp.MustCompile(`Title: Review on (\d{4}-\d{2}-\d{2})`)

func i427TitleDate(userPrompt string) string {
	m := i427TitleDateRE.FindStringSubmatch(userPrompt)
	if m == nil {
		return `{"dates":[]}`
	}
	return i427Dates(m[1])(userPrompt)
}

// TestTemporalReviewTitleReplacedDuringReview: the review reads the title as
// well as the content. A title replaced during the review must not leave a
// date read from the old title, and the block stays due, so the next pick
// reviews the title it holds. Without a finding nothing is written, and the
// stamp's comparison alone keeps the block due.
func TestTemporalReviewTitleReplacedDuringReview(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer func(string) string
	}{
		{"with a finding from the title", i427TitleDate},
		{"without a finding", i427Dates()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newI427Rig(t)
			b := rig.block(t, "Review on 2026-03-15", "Notes of the review, nothing dated in here.")

			entered, release := rig.review.hold(tc.answer)
			done := rig.cycle()
			if p := i427Entered(t, entered, done); !strings.Contains(p, "Title: Review on 2026-03-15") {
				t.Fatalf("the review was sent %q, want the picked title", p)
			}
			newTitle := "Review on 2026-07-20"
			rig.update(t, rig.pool, b.ID, store.UpdateBlockData{Title: &newTitle})
			close(release)
			i427Await(t, done, "first cycle")

			if n := rig.rowsAt(t, b.ID, "2026-03-15"); n != 0 {
				t.Errorf("a date read from the replaced title landed: %d rows", n)
			}
			if !rig.due(t, b.ID) {
				t.Errorf("the block is marked reviewed although its title was replaced during the review")
			}

			rig.embed(t, b.ID)
			rig.endClaim(t, b.ID)
			prompts := rig.review.record(i427TitleDate)
			i427Await(t, rig.cycle(), "second cycle")
			if len(*prompts) != 1 || !strings.Contains((*prompts)[0], "Title: "+newTitle) {
				t.Errorf("the next cycle sent %d review(s) %q, want one of the current title", len(*prompts), *prompts)
			}
			if n := rig.rowsAt(t, b.ID, "2026-07-20"); n != 8 {
				t.Errorf("the current title's date has %d rows, want 8", n)
			}
			if n := rig.rowsAt(t, b.ID, "2026-03-15"); n != 0 {
				t.Errorf("the replaced title's date has %d rows, want 0", n)
			}
			if rig.due(t, b.ID) {
				t.Errorf("the block is still due after the review of its current title")
			}
		})
	}
}

// TestTemporalReviewTitleReplacedAfterTheMergeGuard is the window the merge's
// guard leaves by design (it reads the row without a lock, store.
// writeTemporalGuarded): a title write that commits after the guard and
// before the merge's COMMIT does not conflict with the merge, whose rows hold
// the block only FOR KEY SHARE, and a title write runs no temporal afterwork
// that would replace them. The findings of the old title land — and the stamp,
// which compares after the merge committed, leaves the block due, so the next
// review runs on the current title; here it finds that title's date, and its
// merge replaces the rows. A next review that finds no date writes nothing and
// leaves them, as after any title change: that is the missing temporal
// afterwork of a title write, not this window. The merge is held at its
// COMMIT (testdb.CommitHold), so the order does not depend on timing.
func TestTemporalReviewTitleReplacedAfterTheMergeGuard(t *testing.T) {
	rig := newI427Rig(t)
	b := rig.block(t, "Review on 2026-04-14", "Notes of the review, nothing dated in here.")

	holdPool, hold := testdb.HoldCommitPool(t, rig.dsn, func(sql string) bool {
		return strings.Contains(sql, "DELETE FROM context_temporal")
	})
	rig.review.record(i427TitleDate)
	done := rig.cycleOn(holdPool)
	hold.Wait(t)

	newTitle := "Review on 2026-07-28"
	rig.update(t, rig.pool, b.ID, store.UpdateBlockData{Title: &newTitle})
	hold.Release()
	i427Await(t, done, "first cycle")

	if n := rig.rowsAt(t, b.ID, "2026-04-14"); n != 8 {
		t.Fatalf("the old title's finding has %d rows, want 8 (it committed before the title write was seen)", n)
	}
	if !rig.due(t, b.ID) {
		t.Errorf("the block is marked reviewed although its title was replaced before the stamp")
	}

	rig.embed(t, b.ID)
	rig.endClaim(t, b.ID)
	i427Await(t, rig.cycle(), "second cycle")
	if n := rig.rowsAt(t, b.ID, "2026-04-14"); n != 0 {
		t.Errorf("the old title's finding outlived the next review: %d rows", n)
	}
	if n := rig.rowsAt(t, b.ID, "2026-07-28"); n != 8 {
		t.Errorf("the current title's finding has %d rows, want 8", n)
	}
	if rig.due(t, b.ID) {
		t.Errorf("the block is still due after the review of its current title")
	}
}
