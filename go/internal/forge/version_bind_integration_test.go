//go:build integration

// Issue #452 point 5: the forge sync writes a result it computed from one
// version of a block — the push base after a PATCH, the pulled text after a
// direction decision — and a local write can commit between the read and the
// write.
//
// Push. AdvancePushBase records that the forge now holds the version the push
// listed (base_hash, base_fields) and when the block was in sync (synced_at,
// which the candidate filter compares with the block's updated_at). It ran by
// block id alone and set synced_at = now(): a local edit that committed while
// the PATCH was on the wire — or whose transaction began before the advance and
// wrote after it — carried an updated_at below synced_at and was never pushed.
//
// Pull. pullUpdateIssue decided "forge-ahead" on a block read without a lock
// and then overwrote title and content unconditionally: a local edit that
// committed in between was lost, although the 3-way matrix calls that state a
// conflict (or a convergence, when the edit equals the forge).
//
// The windows are forced by the spy forge (the edit runs inside the wire call)
// and by a hold on the applier's pool (the edit runs while the pull's
// transaction waits at its BEGIN); the writes are the production ones
// (UpdateIssueBlock, the pull apply).
//
//	go test -tags=integration ./internal/forge/ -run 'TestPushEdit|TestPullLocalEdit' -count=1 -v
package forge

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// i452Wait bounds every wait of these tests.
const i452Wait = 30 * time.Second

// hookSpy is the push spy with a hook that runs inside the next wire write.
type hookSpy struct {
	*pushSpyForge
	mu   sync.Mutex
	hook func()
}

func (s *hookSpy) runHook() {
	s.mu.Lock()
	h := s.hook
	s.hook = nil
	s.mu.Unlock()
	if h != nil {
		h()
	}
}

func (s *hookSpy) UpdateIssue(ctx context.Context, r RepoRef, n int64, p IssuePatch) error {
	err := s.pushSpyForge.UpdateIssue(ctx, r, n, p)
	s.runHook()
	return err
}

func (s *hookSpy) UpdateComment(ctx context.Context, r RepoRef, id int64, body string) error {
	err := s.pushSpyForge.UpdateComment(ctx, r, id, body)
	s.runHook()
	return err
}

func (s *hookSpy) setHook(h func()) {
	s.mu.Lock()
	s.hook = h
	s.mu.Unlock()
}

// i452Pulled pulls issue #5 (and, with a comment, comment 700 on it) and
// returns the block ids.
func i452Pulled(t *testing.T, pool *pgxpool.Pool, proj store.ProjectRow, withComment bool) (issueID, commentID string) {
	t.Helper()
	ctx := context.Background()
	a := applierWithRegistry(pool)
	iss := IssueRemote{Number: 5, Title: "Crane", Body: "forge body", State: "open", UpdatedAt: time.Unix(1, 0).UTC()}
	if _, err := a.ApplyIssues(ctx, proj, []IssueRemote{iss}); err != nil {
		t.Fatalf("pull-create issue: %v", err)
	}
	_, _, issueID, _ = readMapping(t, pool, proj.ID, "issue", 5)
	if withComment {
		if _, err := a.ApplyComments(ctx, proj, []CommentRemote{{ID: 700, IssueNumber: 5, Body: "forge comment", UpdatedAt: time.Unix(1, 0).UTC()}}); err != nil {
			t.Fatalf("pull-create comment: %v", err)
		}
		_, _, commentID, _ = readMapping(t, pool, proj.ID, "comment", 700)
	}
	return issueID, commentID
}

// i452Edit is a local edit through the issue verb, in tx or on its own.
func i452Edit(t *testing.T, ctx context.Context, q interface {
	Begin(context.Context) (pgx.Tx, error)
}, scope, id string, u store.IssueUpdate) {
	t.Helper()
	tx, err := q.Begin(ctx)
	if err != nil {
		t.Errorf("begin edit: %v", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := store.UpdateIssueBlock(ctx, tx, id, u, nil, []string{scope}); err != nil {
		t.Errorf("edit: %v", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		t.Errorf("commit edit: %v", err)
	}
}

// i452NoCandidates: after a push that wrote what the block holds, no mapping
// of the project is a candidate any more (the advance set synced_at to the
// version's time, not -infinity).
func i452NoCandidates(t *testing.T, pool *pgxpool.Pool, proj store.ProjectRow) {
	t.Helper()
	ctx := context.Background()
	issues, err := store.ListIssuePushCandidates(ctx, pool, proj.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	comments, err := store.ListCommentPushCandidates(ctx, pool, proj.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 0 || len(comments) != 0 {
		t.Errorf("after the push wrote the block's version, %d issue and %d comment mapping(s) are still push candidates", len(issues), len(comments))
	}
}

// TestPushEditDuringThePatchIsPushedNext: a local edit commits while the push's
// PATCH is on the wire. The advance must leave the mapping a candidate, so
// the next run pushes the edit.
func TestPushEditDuringThePatchIsPushedNext(t *testing.T) {
	str := func(s string) *string { return &s }
	t.Run("issue", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		ctx := context.Background()
		proj := enablePush(seedIFProject(t, pool, "i452pushissue"))
		issueID, _ := i452Pulled(t, pool, proj, false)
		i452Edit(t, ctx, pool, proj.Scope, issueID, store.IssueUpdate{Title: str("Crane, renamed")})

		spy := &hookSpy{pushSpyForge: newPushSpy()}
		spy.setHook(func() {
			i452Edit(t, ctx, pool, proj.Scope, issueID, store.IssueUpdate{Content: str("body edited during the patch")})
		})
		p := pusherWithRegistry(pool)
		if res, err := p.PushProject(ctx, proj, spy, RepoRef{Owner: "o", Repo: "r"}, alwaysAllow); err != nil || res.Pushed != 1 {
			t.Fatalf("first push: pushed=%d err=%v, want 1", res.Pushed, err)
		}
		res, err := p.PushProject(ctx, proj, spy, RepoRef{Owner: "o", Repo: "r"}, alwaysAllow)
		if err != nil {
			t.Fatalf("second push: %v", err)
		}
		if res.Pushed != 1 || len(spy.patches) != 2 || spy.patches[1].Patch.Body == nil ||
			*spy.patches[1].Patch.Body != "body edited during the patch" {
			t.Fatalf("second push: pushed=%d patches=%+v, want a PATCH of the body edited during the first", res.Pushed, spy.patches)
		}
		if spy.patches[1].Patch.Title != nil {
			t.Errorf("the second PATCH carries the title again (%q): the base must hold what the first PATCH wrote", *spy.patches[1].Patch.Title)
		}
		i452NoCandidates(t, pool, proj)
	})
	t.Run("comment", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		ctx := context.Background()
		proj := enablePush(seedIFProject(t, pool, "i452pushcomment"))
		_, commentID := i452Pulled(t, pool, proj, true)
		i452Edit(t, ctx, pool, proj.Scope, commentID, store.IssueUpdate{Content: str("comment v2")})

		spy := &hookSpy{pushSpyForge: newPushSpy()}
		spy.setHook(func() {
			i452Edit(t, ctx, pool, proj.Scope, commentID, store.IssueUpdate{Content: str("comment v3, edited during the patch")})
		})
		p := pusherWithRegistry(pool)
		if res, err := p.PushProject(ctx, proj, spy, RepoRef{Owner: "o", Repo: "r"}, alwaysAllow); err != nil || res.Pushed != 1 {
			t.Fatalf("first push: pushed=%d err=%v, want 1", res.Pushed, err)
		}
		res, err := p.PushProject(ctx, proj, spy, RepoRef{Owner: "o", Repo: "r"}, alwaysAllow)
		if err != nil {
			t.Fatalf("second push: %v", err)
		}
		if res.Pushed != 1 || len(spy.commentPatches) != 2 || spy.commentPatches[1].Body != "comment v3, edited during the patch" {
			t.Fatalf("second push: pushed=%d comment patches=%+v, want the body edited during the first", res.Pushed, spy.commentPatches)
		}
		i452NoCandidates(t, pool, proj)
	})
}

// TestPushEditBegunBeforeTheAdvance: the edit's transaction begins before the
// push advances the base and writes after it, so its updated_at (the
// transaction start) is earlier than the advance's now(). The mapping must
// still be a candidate: synced_at is the time of the version the forge holds.
func TestPushEditBegunBeforeTheAdvance(t *testing.T) {
	str := func(s string) *string { return &s }
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	proj := enablePush(seedIFProject(t, pool, "i452pushbegun"))
	issueID, _ := i452Pulled(t, pool, proj, false)
	i452Edit(t, ctx, pool, proj.Scope, issueID, store.IssueUpdate{Title: str("Crane, renamed")})

	w, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Rollback(ctx) }()
	if _, err := w.Exec(ctx, `SELECT 1`); err != nil {
		t.Fatal(err)
	}

	spy := newPushSpy()
	p := pusherWithRegistry(pool)
	if res, err := p.PushProject(ctx, proj, spy, RepoRef{Owner: "o", Repo: "r"}, alwaysAllow); err != nil || res.Pushed != 1 {
		t.Fatalf("first push: pushed=%d err=%v, want 1", res.Pushed, err)
	}
	if _, err := store.UpdateIssueBlock(ctx, w, issueID, store.IssueUpdate{Content: str("body of a write begun before the advance")}, nil, []string{proj.Scope}); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if err := w.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	res, err := p.PushProject(ctx, proj, spy, RepoRef{Owner: "o", Repo: "r"}, alwaysAllow)
	if err != nil {
		t.Fatalf("second push: %v", err)
	}
	if res.Pushed != 1 || len(spy.patches) != 2 || spy.patches[1].Patch.Body == nil ||
		*spy.patches[1].Patch.Body != "body of a write begun before the advance" {
		t.Fatalf("second push: pushed=%d patches=%+v, want a PATCH of the body", res.Pushed, spy.patches)
	}
	i452NoCandidates(t, pool, proj)
}

// TestPushEditInFlightDuringTheAdvance: an edit whose transaction began before
// the issue was even pulled — so before the mapping's last synced_at and the
// version the push lists — holds the block row across the advance and commits
// after it. Its updated_at is earlier than every time the mapping carries, so
// no timestamp shows it: the advance has to wait for it, see that the block no
// longer holds the version the PATCH wrote, and leave the row a candidate
// whatever updated_at the edit stored.
func TestPushEditInFlightDuringTheAdvance(t *testing.T) {
	str := func(s string) *string { return &s }
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	proj := enablePush(seedIFProject(t, pool, "i452pushflight"))

	w, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Rollback(ctx) }()
	var wPID uint32
	if err := w.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&wPID); err != nil {
		t.Fatal(err)
	}
	issueID, _ := i452Pulled(t, pool, proj, false)
	i452Edit(t, ctx, pool, proj.Scope, issueID, store.IssueUpdate{Title: str("Crane, renamed")})

	spy := &hookSpy{pushSpyForge: newPushSpy()}
	spy.setHook(func() {
		if _, err := store.UpdateIssueBlock(ctx, w, issueID, store.IssueUpdate{Content: str("body of a write in flight")}, nil, []string{proj.Scope}); err != nil {
			t.Errorf("in-flight edit: %v", err)
		}
	})
	p := pusherWithRegistry(pool)
	done := make(chan struct{})
	var res PushResult
	var perr error
	go func() {
		defer close(done)
		res, perr = p.PushProject(ctx, proj, spy, RepoRef{Owner: "o", Repo: "r"}, alwaysAllow)
	}()
	testdb.WaitBlockedBy(t, pool, wPID, done, "the push's advance", "")
	if err := w.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(i452Wait):
		t.Fatalf("the first push did not finish within %s", i452Wait)
	}
	if perr != nil || res.Pushed != 1 {
		t.Fatalf("first push: pushed=%d err=%v, want 1", res.Pushed, perr)
	}

	res2, err := p.PushProject(ctx, proj, spy, RepoRef{Owner: "o", Repo: "r"}, alwaysAllow)
	if err != nil {
		t.Fatalf("second push: %v", err)
	}
	if res2.Pushed != 1 || len(spy.patches) != 2 || spy.patches[1].Patch.Body == nil ||
		*spy.patches[1].Patch.Body != "body of a write in flight" {
		t.Fatalf("second push: pushed=%d patches=%+v, want a PATCH of the body written in flight", res2.Pushed, spy.patches)
	}
	i452NoCandidates(t, pool, proj)
}

// i452BeginHold holds the first transaction its pool begins, before the
// BEGIN goes out, until release is closed: the statements before it (the
// mapping lookup, the unlocked read of the block) have run, the transaction
// that writes has not started.
type i452BeginHold struct {
	once    sync.Once
	used    bool
	mu      sync.Mutex
	entered chan struct{}
	release chan struct{}
}

func (h *i452BeginHold) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(data.SQL)), "begin") {
		return ctx
	}
	h.mu.Lock()
	first := !h.used
	h.used = true
	h.mu.Unlock()
	if first {
		h.once.Do(func() { close(h.entered) })
		select {
		case <-h.release:
		case <-time.After(i452Wait):
		}
	}
	return ctx
}

func (h *i452BeginHold) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func i452HoldBeginPool(t *testing.T, dsn string) (*pgxpool.Pool, *i452BeginHold) {
	t.Helper()
	h := &i452BeginHold{entered: make(chan struct{}), release: make(chan struct{})}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Tracer = h
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		select {
		case <-h.release:
		default:
			close(h.release)
		}
		pool.Close()
	})
	return pool, h
}

// TestPullLocalEditBetweenTheReadAndTheWrite: the forge changed the issue (or
// the comment), the pull read the block as unchanged since the base and
// decided "forge-ahead", and a local edit commits before the pull writes.
// The local edit must survive: the mapping becomes a conflict (both sides
// moved) — or converges, when the edit is what the forge holds.
func TestPullLocalEditBetweenTheReadAndTheWrite(t *testing.T) {
	str := func(s string) *string { return &s }
	for _, tc := range []struct {
		name         string
		comment      bool
		localBody    string
		forgeBody    string
		wantConflict bool
	}{
		{"issue, edit differs from the forge", false, "local body edited before the pull wrote", "forge body, edited on the forge", true},
		{"issue, edit equals the forge", false, "forge body, edited on the forge", "forge body, edited on the forge", false},
		{"comment, edit differs from the forge", true, "local comment edited before the pull wrote", "forge comment, edited on the forge", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, dsn := testdb.SetupTestDBWithDSN(t)
			ctx := context.Background()
			proj := seedIFProject(t, pool, "i452pull")
			issueID, commentID := i452Pulled(t, pool, proj, tc.comment)
			id, kind, forgeID := issueID, "issue", int64(5)
			if tc.comment {
				id, kind, forgeID = commentID, "comment", 700
			}
			baseBefore, _, _, _ := readMapping(t, pool, proj.ID, kind, forgeID)

			held, hold := i452HoldBeginPool(t, dsn)
			a := applierWithRegistry(held)
			done := make(chan error, 1)
			go func() {
				var err error
				if tc.comment {
					_, err = a.ApplyComments(ctx, proj, []CommentRemote{{ID: 700, IssueNumber: 5, Body: tc.forgeBody, UpdatedAt: time.Unix(2, 0).UTC()}})
				} else {
					_, err = a.ApplyIssues(ctx, proj, []IssueRemote{{Number: 5, Title: "Crane", Body: tc.forgeBody, State: "open", UpdatedAt: time.Unix(2, 0).UTC()}})
				}
				done <- err
			}()
			select {
			case <-hold.entered:
			case err := <-done:
				t.Fatalf("the pull finished without a write transaction: %v", err)
			case <-time.After(i452Wait):
				t.Fatalf("the pull did not reach its write within %s", i452Wait)
			}
			i452Edit(t, ctx, pool, proj.Scope, id, store.IssueUpdate{Content: str(tc.localBody)})
			close(hold.release)
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("pull: %v", err)
				}
			case <-time.After(i452Wait):
				t.Fatalf("the pull did not finish within %s", i452Wait)
			}

			b := getBlock(t, pool, proj.Scope, id)
			if b.Content != tc.localBody {
				t.Errorf("the block holds %q after the pull, want the local edit %q kept", b.Content, tc.localBody)
			}
			base, conflict, _, _ := readMapping(t, pool, proj.ID, kind, forgeID)
			if conflict != tc.wantConflict {
				t.Errorf("mapping conflict=%v, want %v", conflict, tc.wantConflict)
			}
			if !tc.wantConflict {
				// Converged: base := the hash both sides hold.
				var ctxH string
				if tc.comment {
					_, ctxH = CtxCommentBase(b)
				} else {
					_, ctxH = CtxIssueBase(b, []string{"done"}, true)
				}
				if base != ctxH || base == baseBefore {
					t.Errorf("converged mapping base = %s, want the hash both sides hold (%s)", base, ctxH)
				}
			}
		})
	}
}

// TestPullLocalEditInFlightAcrossThePull: the local edit holds the block row
// (written, not committed) when the pull reaches its write. The pull has to
// wait for it and project what it committed, not write over it once the row
// is free.
func TestPullLocalEditInFlightAcrossThePull(t *testing.T) {
	str := func(s string) *string { return &s }
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	proj := seedIFProject(t, pool, "i452pullflight")
	issueID, _ := i452Pulled(t, pool, proj, false)

	w, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Rollback(ctx) }()
	var wPID uint32
	if err := w.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&wPID); err != nil {
		t.Fatal(err)
	}
	const local = "local body written while the pull ran"
	if _, err := store.UpdateIssueBlock(ctx, w, issueID, store.IssueUpdate{Content: str(local)}, nil, []string{proj.Scope}); err != nil {
		t.Fatalf("in-flight edit: %v", err)
	}

	a := applierWithRegistry(pool)
	done := make(chan struct{})
	var perr error
	go func() {
		defer close(done)
		_, perr = a.ApplyIssues(ctx, proj, []IssueRemote{{Number: 5, Title: "Crane", Body: "forge body, edited on the forge", State: "open", UpdatedAt: time.Unix(2, 0).UTC()}})
	}()
	testdb.WaitBlockedBy(t, pool, wPID, done, "the pull", "")
	if err := w.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(i452Wait):
		t.Fatalf("the pull did not finish within %s", i452Wait)
	}
	if perr != nil {
		t.Fatalf("pull: %v", perr)
	}
	if b := getBlock(t, pool, proj.Scope, issueID); b.Content != local {
		t.Errorf("the block holds %q after the pull, want the local edit kept", b.Content)
	}
	if _, conflict, _, _ := readMapping(t, pool, proj.ID, "issue", 5); !conflict {
		t.Errorf("mapping conflict=false, want true: both sides moved")
	}
}

// TestPushInSyncRowLeavesTheCandidates: a mapping whose block projects to the
// base again — after a write that does not touch the projection (tags), or
// after an edit made during a PATCH was undone before the next run — is
// listed (updated_at > synced_at, or synced_at = -infinity) and skipped as in
// sync. The skip must advance synced_at through the bound advance, or the row
// is a candidate for good and, past the batch limit, keeps later candidates
// out of the listing.
func TestPushInSyncRowLeavesTheCandidates(t *testing.T) {
	str := func(s string) *string { return &s }
	t.Run("edit during the patch undone before the next run", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		ctx := context.Background()
		proj := enablePush(seedIFProject(t, pool, "i452insyncundo"))
		issueID, _ := i452Pulled(t, pool, proj, false)
		i452Edit(t, ctx, pool, proj.Scope, issueID, store.IssueUpdate{Title: str("Crane, renamed")})
		spy := &hookSpy{pushSpyForge: newPushSpy()}
		spy.setHook(func() {
			i452Edit(t, ctx, pool, proj.Scope, issueID, store.IssueUpdate{Content: str("edited during the patch")})
		})
		p := pusherWithRegistry(pool)
		if res, err := p.PushProject(ctx, proj, spy, RepoRef{Owner: "o", Repo: "r"}, alwaysAllow); err != nil || res.Pushed != 1 {
			t.Fatalf("first push: pushed=%d err=%v, want 1", res.Pushed, err)
		}
		i452Edit(t, ctx, pool, proj.Scope, issueID, store.IssueUpdate{Content: str("forge body")})
		for run := 2; run <= 3; run++ {
			res, err := p.PushProject(ctx, proj, spy, RepoRef{Owner: "o", Repo: "r"}, alwaysAllow)
			if err != nil || res.Pushed != 0 || spy.wireCalls() != 1 {
				t.Fatalf("run %d: pushed=%d wire=%d err=%v, want 0 new wire writes (in sync)", run, res.Pushed, spy.wireCalls(), err)
			}
			i452NoCandidates(t, pool, proj)
		}
	})
	t.Run("tags only, issue and comment", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		ctx := context.Background()
		proj := enablePush(seedIFProject(t, pool, "i452insynctags"))
		issueID, commentID := i452Pulled(t, pool, proj, true)
		i452Edit(t, ctx, pool, proj.Scope, issueID, store.IssueUpdate{Tags: []string{"field-work"}})
		i452Edit(t, ctx, pool, proj.Scope, commentID, store.IssueUpdate{Tags: []string{"field-work"}})
		issues, _ := store.ListIssuePushCandidates(ctx, pool, proj.ID, 10)
		comments, _ := store.ListCommentPushCandidates(ctx, pool, proj.ID, 10)
		if len(issues) != 1 || len(comments) != 1 {
			t.Fatalf("precondition: %d issue / %d comment candidates after the tags writes, want 1/1", len(issues), len(comments))
		}
		spy := newPushSpy()
		res, err := pusherWithRegistry(pool).PushProject(ctx, proj, spy, RepoRef{Owner: "o", Repo: "r"}, alwaysAllow)
		if err != nil || res.Pushed != 0 || spy.wireCalls() != 0 {
			t.Fatalf("push: pushed=%d wire=%d err=%v, want 0/0", res.Pushed, spy.wireCalls(), err)
		}
		i452NoCandidates(t, pool, proj)
	})
}

// TestPushInSyncAdvanceSeesARealEdit: the in-sync skip advances through the
// bound advance, so a real edit that commits between the listing and that
// advance keeps the row a candidate, and the next run pushes it.
func TestPushInSyncAdvanceSeesARealEdit(t *testing.T) {
	str := func(s string) *string { return &s }
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	proj := enablePush(seedIFProject(t, pool, "i452insyncreal"))
	issueID, _ := i452Pulled(t, pool, proj, false)
	i452Edit(t, ctx, pool, proj.Scope, issueID, store.IssueUpdate{Tags: []string{"field-work"}})

	held, hold := i452HoldBeginPool(t, dsn)
	spy := newPushSpy()
	done := make(chan error, 1)
	go func() {
		_, err := pusherWithRegistry(held).PushProject(ctx, proj, spy, RepoRef{Owner: "o", Repo: "r"}, alwaysAllow)
		done <- err
	}()
	select {
	case <-hold.entered:
	case err := <-done:
		t.Fatalf("the push skipped the in-sync row without an advance (err %v)", err)
	case <-time.After(i452Wait):
		t.Fatalf("the push did not reach its advance within %s", i452Wait)
	}
	i452Edit(t, ctx, pool, proj.Scope, issueID, store.IssueUpdate{Content: str("body edited before the advance")})
	close(hold.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("first push: %v", err)
		}
	case <-time.After(i452Wait):
		t.Fatalf("the first push did not finish within %s", i452Wait)
	}
	res, err := pusherWithRegistry(pool).PushProject(ctx, proj, spy, RepoRef{Owner: "o", Repo: "r"}, alwaysAllow)
	if err != nil || res.Pushed != 1 || len(spy.patches) != 1 || spy.patches[0].Patch.Body == nil ||
		*spy.patches[0].Patch.Body != "body edited before the advance" {
		t.Fatalf("second push: pushed=%d patches=%+v err=%v, want a PATCH of the edit", res.Pushed, spy.patches, err)
	}
	i452NoCandidates(t, pool, proj)
}
