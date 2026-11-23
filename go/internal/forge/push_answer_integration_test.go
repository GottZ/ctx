//go:build integration

// Two small points of the push (issue #509): an answer without a readable
// updated_at is logged, and a write whose record fails to commit after the
// PATCH landed is counted as pushed.
package forge

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/store"
)

const i509NoStampWarn = "forge: the forge's answer to a push write carried no readable updated_at"

func TestAPushAnswerWithoutAStampIsLogged(t *testing.T) {
	for _, a := range []fakeStampAnswer{fakeStampAbsent, fakeStampUnreadable, fakeStampServed} {
		t.Run(map[fakeStampAnswer]string{fakeStampAbsent: "absent", fakeStampUnreadable: "unreadable", fakeStampServed: "served"}[a], func(t *testing.T) {
			logs := i491CaptureLog(t)
			e := i491Setup(t, "pas", "", fakeIssue{Title: "Crane", Body: "forge body"})
			e.gh.setStampAnswer(a)
			e.edit(t, store.IssueUpdate{Content: str("body edited locally")})
			if pr := e.pushOn(t, e.pool); pr.Pushed != 1 {
				t.Fatalf("push: %+v", pr)
			}
			want := 1
			if a == fakeStampServed {
				want = 0
			}
			if got := logs.count(i509NoStampWarn); got != want {
				t.Errorf("%d log line(s) for the answer, want %d", got, want)
			}
		})
	}
}

// A record whose COMMIT fails after its PATCH landed: the forge was written,
// the push counts it (the run's pushed), and returns the error.
func TestAPushWhoseRecordFailsToCommitCountsTheWrite(t *testing.T) {
	e := i491Setup(t, "prc", "", fakeIssue{Title: "Crane", Body: "forge body"})
	ctx := context.Background()
	for _, q := range []string{
		`CREATE FUNCTION i509_refuse_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'i509: the commit is refused'; END $$`,
		`CREATE CONSTRAINT TRIGGER i509_refuse_commit AFTER UPDATE ON context_project_sync_map
		   DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION i509_refuse_commit()`,
	} {
		if _, err := e.pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	e.edit(t, store.IssueUpdate{Content: str("body edited locally")})
	res, err := NewPusher(e.pool, e.reg.SnapshotForTenant).PushProject(ctx, e.proj, e.gh.client(), e.gh.repo(), alwaysAllow)
	if err == nil {
		t.Fatal("the push returned no error although its record's COMMIT failed")
	}
	if len(e.gh.landed()) != 1 || e.gh.issue(t, 5).Body != "body edited locally" {
		t.Fatalf("precondition: the PATCH did not land (%d)", len(e.gh.landed()))
	}
	if res.Pushed != 1 {
		t.Errorf("pushed %d after a PATCH that landed and a record that failed, want 1", res.Pushed)
	}
}
