//go:build integration

// A push after a forge stamp reset (issue #509): the reset sets the mapping's
// forge_updated_at to NULL, and the push advance of the next update push
// compares that stamp with the PATCH's answer. A NULL in that comparison made
// the whole predicate NULL, the conflict column NOT NULL rejected it (23502)
// after the PATCH had landed, and the run went into backoff — after a reset
// of every mapping, the first push of each entity.
//
//	go test -tags=integration ./internal/forge/ -run 'TestAPushAfterAStampReset' -count=1 -v
package forge

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/store"
)

func TestAPushAfterAStampResetAdvances(t *testing.T) {
	ctx := context.Background()
	t.Run("issue", func(t *testing.T) {
		e := i509WithSix(t, "srpi", true)
		if _, err := e.resolverOn(e.pool).ResetStamps(ctx, e.proj, e.id(t), store.AuditStamp{}); err != nil {
			t.Fatal(err)
		}
		e.i509MustRun(t)
		if at := e.recordedStamp(t, e.id(t)); at != nil {
			t.Fatalf("precondition: the run after the reset recorded a stamp %v; want NULL", at)
		}
		e.edit(t, store.IssueUpdate{Content: str("body L, a local edit after the stamp reset")})
		res, err := e.pushOnErr(e.pool)
		if err != nil || res.Pushed != 1 {
			t.Fatalf("the push after the reset: %+v, %v — want one write and its advance committed", res, err)
		}
		base, flagged := e.mapping(t)
		if flagged || base != e.forgeH(t) {
			t.Errorf("after the push: flagged %v, base is the forge's %v — want the sent version as the base", flagged, base == e.forgeH(t))
		}
		if at := e.recordedStamp(t, e.id(t)); at == nil || !at.Equal(e.gh.issue(t, 5).Updated) {
			t.Errorf("recorded stamp %v, want the PATCH's answer %v", at, e.gh.issue(t, 5).Updated)
		}
	})
	t.Run("comment", func(t *testing.T) {
		e := i491Setup(t, "srpc", "", fakeIssue{Title: "Crane", Body: "forge body"})
		cid := i509CommentEnv(t, e)
		if _, err := e.resolverOn(e.pool).ResetStamps(ctx, e.proj, cid, store.AuditStamp{}); err != nil {
			t.Fatal(err)
		}
		e.editComment(t, cid, "comment L, a local edit after the stamp reset")
		res, err := e.pushOnErr(e.pool)
		if err != nil || res.Pushed != 1 {
			t.Fatalf("the comment push after the reset: %+v, %v — want one write and its advance committed", res, err)
		}
		_, flagged, _, _ := readMapping(t, e.pool, e.proj.ID, "comment", 700)
		if flagged || e.gh.comment(t, 700).Body != "comment L, a local edit after the stamp reset" {
			t.Errorf("after the comment push: flagged %v, forge comment %q", flagged, e.gh.comment(t, 700).Body)
		}
	})
}
