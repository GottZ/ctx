//go:build integration

// Issue #474 (the follow-up list of #509): the comment leg's record of the
// orphan comments it held. It was the list of their ids, written again with
// the cursor after every comment page and carried in every later cursor
// commit: in a page-bounded catch-up — the issue leg stops at its page
// bound, so every orphan is held — 1000 pages of 100 orphans wrote 0.9 MB
// of JSON per run, about 240 MB of WAL and 20–28 s of database time. The
// record is now one timestamp, the newest comment stamp the leg listed: a
// later run knows that a comment at or before it was listed — and held, if
// it was an orphan — by the run before.
//
//	go test -tags=integration ./internal/forge/ -run TestTheOrphanRecordOfABoundedCatchUpStaysSmall -count=1 -v
package forge

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// orphanPages is a forge whose comment listing is pages of 100 comments on
// issues the project does not map, numbered after every mapped one.
type orphanPages struct {
	fakeForge
	pages int
	t0    time.Time
}

func (z *orphanPages) page(n int) CommentPage {
	cs := make([]CommentRemote, 0, 100)
	for k := 0; k < 100; k++ {
		i := (n-1)*100 + k
		cs = append(cs, CommentRemote{ID: int64(5000000 + i), IssueNumber: int64(900000 + i), Body: "c", UpdatedAt: z.t0.Add(time.Duration(i) * time.Second)})
	}
	p := CommentPage{Comments: cs}
	if n < z.pages {
		p.NextURL = fmt.Sprintf("orphans://page/%d", n+1)
	}
	return p
}

func (z *orphanPages) ListCommentsSince(context.Context, RepoRef, time.Time, string) (CommentPage, error) {
	return z.page(1), nil
}

func (z *orphanPages) ListCommentsPage(_ context.Context, u string) (CommentPage, error) {
	var n int
	if _, err := fmt.Sscanf(u, "orphans://page/%d", &n); err != nil {
		return CommentPage{}, err
	}
	return z.page(n), nil
}

func TestTheOrphanRecordOfABoundedCatchUpStaysSmall(t *testing.T) {
	ctx := context.Background()
	e := i491Setup(t, "orec", "", fakeIssue{Title: "Crane", Body: "forge body"})
	m := e.i509Mgr(e.pool)
	z := &orphanPages{pages: 20, t0: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	for run := 1; run <= 2; run++ {
		proj := e.proj
		if err := e.pool.QueryRow(ctx, `SELECT sync_cursor FROM context_projects WHERE id = $1::uuid`, e.proj.ID).Scan(&proj.SyncCursor); err != nil {
			t.Fatal(err)
		}
		// The issue leg stopped at its page bound: every orphan is held.
		if err := m.pullComments(ctx, proj, z, e.gh.repo(), "", time.Time{}, false); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		var raw int
		if err := e.pool.QueryRow(ctx, `SELECT length(sync_cursor::text) FROM context_projects WHERE id = $1::uuid`, e.proj.ID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if raw > 1024 {
			t.Errorf("run %d: the cursor after a bounded catch-up of %d pages of 100 orphans is %d bytes of JSON — the record grows with the orphans", run, z.pages, raw)
		}
		// The comment leg stays held at the earliest orphan.
		if _, since := e.i509Cursor(t, "comments"); !since.Equal(z.t0) {
			t.Errorf("run %d: the comment leg's since %v, want the earliest orphan's %v", run, since, z.t0)
		}
	}
}
