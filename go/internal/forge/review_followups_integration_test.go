//go:build integration

// Issue #509, from the bot review of the final head of #501: small findings
// about the pull and the push.
//
// A push advance without a wire write (the listed block projects to its base
// already) found the base moved by a pull between the section's check and
// its lock and recorded the base unknown and flagged the mapping, although
// nothing was sent. It now leaves a moved base alone, and stamps
// metadata.forge_state only while the row holds the listed base.
//
// A fetch older than the observation a flagged mapping records returned
// before it was counted: the WARN and pull_skipped missed it.
//
// A failed sync run's status omitted the counts of the leg that failed: the
// three call sites returned before they added them.
//
//	go test -tags=integration ./internal/forge/ -run 'TestAPushAdvanceWithoutAWireWrite|TestAnOlderFetchOfAFlaggedMapping|TestAFailedSyncRunCarriesTheCounts' -count=1 -v
package forge

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/sealbox"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestAPushAdvanceWithoutAWireWriteLeavesABaseAPullMoved(t *testing.T) {
	e := i491Setup(t, "i509advnowire", "", fakeIssue{Title: "Crane", Body: "forge body"})
	ctx := context.Background()
	// A tags write: the block is listed (updated_at past synced_at) and
	// projects to its base — the push sends nothing and only advances.
	if _, _, err := store.UpdateBlock(ctx, e.pool, e.id(t), store.UpdateBlockData{Tags: []string{"triaged"}}, []string{e.proj.Scope}); err != nil {
		t.Fatal(err)
	}
	held, hold := i452HoldBeginPool(t, e.dsn)
	type out struct {
		res PushResult
		err error
	}
	done := make(chan out, 1)
	go func() {
		r, err := NewPusher(held, e.reg.SnapshotForTenant).PushProject(ctx, e.proj, e.gh.client(), e.gh.repo(), alwaysAllow)
		done <- out{r, err}
	}()
	select {
	case <-hold.entered:
	case o := <-done:
		t.Fatalf("the push finished without its advance: %+v %v", o.res, o.err)
	case <-time.After(i452Wait):
		t.Fatal("the push did not reach its advance")
	}
	// Between the section's check and the advance: a remote close, pulled.
	e.gh.edit(t, 5, func(it *fakeIssue) { it.State = "closed"; it.Body = "closed on the forge" })
	if r := e.pullOn(t, e.pool); r.Applied != 1 {
		t.Fatalf("the pull of the remote close: %+v", r)
	}
	close(hold.release)
	var o out
	select {
	case o = <-done:
	case <-time.After(i452Wait):
		t.Fatal("the push did not finish")
	}
	if o.err != nil || o.res.Pushed != 0 || o.res.Conflicts != 0 {
		t.Errorf("push: %+v %v, want nothing sent and nothing flagged", o.res, o.err)
	}
	if base, conflict := e.mapping(t); conflict || base != e.forgeH(t) {
		t.Errorf("mapping flagged %v, base is the forge's %v — want the pulled base kept", conflict, base == e.forgeH(t))
	}
	if fs := metaString(getBlock(t, e.pool, e.proj.Scope, e.id(t)).Metadata, "forge_state"); fs != "closed" {
		t.Errorf("metadata.forge_state is %q, want closed (the pull's)", fs)
	}
	if n := e.cycles(t, 2); n != 0 {
		t.Errorf("two runs landed %d PATCH(es), want 0", n)
	}
}

func TestAnOlderFetchOfAFlaggedMappingIsCountedAndReported(t *testing.T) {
	logs := i491CaptureLog(t)
	e := i491Setup(t, "i509flagstale", "", fakeIssue{Title: "Crane", Body: "forge body"})
	stale := e.fetch(t)
	// A newer version is pulled (the row records its stamp), then both sides
	// move and a pull flags the mapping.
	e.gh.edit(t, 5, func(it *fakeIssue) { it.Body = "body edited on the forge, second" })
	if r := e.pullOn(t, e.pool); r.Applied != 1 {
		t.Fatalf("pull of the second version: %+v", r)
	}
	e.edit(t, store.IssueUpdate{Title: str("Crane, edited locally")})
	e.gh.edit(t, 5, func(it *fakeIssue) { it.Body = "body edited on the forge, third" })
	if r := e.pullOn(t, e.pool); r.Conflicts != 1 {
		t.Fatalf("the pull of both moves: %+v, want the mapping flagged", r)
	}
	before := logs.count(i491StaleWarn)
	r := e.applyPage(t, stale)
	if r.Applied != 0 || r.Conflicts != 0 || r.Skipped != 1 {
		t.Errorf("the older fetch on the flagged mapping: %+v, want it skipped and counted once, the conflict not counted again", r)
	}
	if n := logs.count(i491StaleWarn) - before; n != 1 {
		t.Errorf("%d WARN line(s) for the skipped older fetch, want one", n)
	}
	if _, conflict := e.mapping(t); !conflict {
		t.Errorf("the flag is gone")
	}
}

// i509TokenProject seals a token for the project, so a sync run reaches its
// push leg.
func i509TokenProject(t *testing.T, pool *pgxpool.Pool, proj store.ProjectRow) (store.ProjectRow, *sealbox.Box) {
	t.Helper()
	ctx := context.Background()
	box, err := sealbox.New(hex.EncodeToString([]byte(strings.Repeat("k", 32))), "")
	if err != nil {
		t.Fatal(err)
	}
	const secret = "forge_token"
	nonce, ct, err := box.Seal(secret, proj.Scope, []byte("token"))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := store.PutSecret(ctx, tx, secret, proj.Scope, nonce, ct, 1, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.SetProjectToken(ctx, tx, proj.ID, secret); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	name := secret
	proj.TokenSecret = &name
	return proj, box
}

// i509FailedRun waits for the run's row and returns its stats; the run must
// have ended as an error.
func i509FailedRun(t *testing.T, pool *pgxpool.Pool, projectID string) map[string]int {
	t.Helper()
	var run *store.SyncRunRow
	var err error
	for i := 0; i < 400; i++ {
		if run, err = store.LatestSyncRun(context.Background(), pool, projectID); err != nil || (run != nil && run.Status != "running") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err != nil || run == nil || run.Status != "error" {
		t.Fatalf("latest run: %+v %v, want a failed run", run, err)
	}
	var stats map[string]int
	if err := json.Unmarshal(run.Stats, &stats); err != nil {
		t.Fatal(err)
	}
	return stats
}

func TestAFailedSyncRunCarriesTheCountsOfTheLegThatFailed(t *testing.T) {
	failed := errors.New("the leg failed after it wrote")
	issuePage := []IssuePage{{Issues: []IssueRemote{{Number: 5, Title: "Crane", State: "open", UpdatedAt: time.Unix(10, 0).UTC()}}}}
	okIssues := func(context.Context, store.ProjectRow, []IssueRemote) (ApplyResult, error) { return ApplyResult{}, nil }

	t.Run("the issue leg", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		proj := seedIFProject(t, pool, "i509failissue")
		m := mgr(pool, okTenant, okPolicy, &fakeForge{pages: issuePage}, func(context.Context, store.ProjectRow, []IssueRemote) (ApplyResult, error) {
			return ApplyResult{Applied: 1, Conflicts: 1, Skipped: 1}, failed
		})
		if _, err := m.StartSync(context.Background(), proj, false); err != nil {
			t.Fatal(err)
		}
		st := waitDone(t, m, proj.ID)
		stats := i509FailedRun(t, pool, proj.ID)
		if st.PullSkipped != 1 || st.Applied != 1 || st.Conflicts != 1 || stats["pull_skipped"] != 1 || stats["applied"] != 1 {
			t.Errorf("failed run: state %+v, stats %v — want the leg's counts (applied 1, conflicts 1, pull_skipped 1)", st, stats)
		}
	})
	t.Run("the comment leg", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		proj := seedIFProject(t, pool, "i509failcomment")
		m := mgr(pool, okTenant, okPolicy, &fakeForge{}, okIssues)
		m.SetApplyComments(func(context.Context, store.ProjectRow, []CommentRemote) (ApplyResult, error) {
			return ApplyResult{Applied: 1, Skipped: 1}, failed
		})
		if _, err := m.StartSync(context.Background(), proj, false); err != nil {
			t.Fatal(err)
		}
		st := waitDone(t, m, proj.ID)
		stats := i509FailedRun(t, pool, proj.ID)
		if st.PullSkipped != 1 || st.Applied != 1 || stats["pull_skipped"] != 1 {
			t.Errorf("failed run: state %+v, stats %v — want the comment leg's counts (applied 1, pull_skipped 1)", st, stats)
		}
	})
	t.Run("the push leg", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		proj, box := i509TokenProject(t, pool, enablePush(seedIFProject(t, pool, "i509failpush")))
		m := mgr(pool, okTenant, okPolicy, &fakeForge{}, okIssues)
		m.openBox = func() (*sealbox.Box, error) { return box, nil }
		m.throttle = NewThrottle()
		m.SetPush(func(context.Context, store.ProjectRow, Forge, RepoRef, func() bool) (PushResult, error) {
			return PushResult{Pushed: 1, Skipped: 2, Conflicts: 1}, failed
		})
		if _, err := m.StartSync(context.Background(), proj, false); err != nil {
			t.Fatal(err)
		}
		st := waitDone(t, m, proj.ID)
		stats := i509FailedRun(t, pool, proj.ID)
		if st.PushSkipped != 2 || st.Pushed != 1 || st.Conflicts != 1 || stats["push_skipped"] != 2 || stats["pushed"] != 1 {
			t.Errorf("failed run: state %+v, stats %v — want the push leg's counts (pushed 1, push_skipped 2, conflicts 1)", st, stats)
		}
	})
}
