//go:build integration

// I-H push gates against a real DB with a spy Forge (design/02 §4.5.2/§5.6/§6.1):
// the push_enabled + conflict wire-write gates, the field-diff PATCH, the
// truncated-body data-loss guard (status flip ⇒ PATCH without body; body edit ⇒
// 0 wire + conflict), the "#L<seq>"→"#<nr>" rename + comment-title cascade + base
// rewrite (idempotent), the issue-before-comments ordering, and the token-scoped
// throttle stop. The 3-way idempotency ANCHOR to I-G (a 2nd run = 0 writes) is
// proven by re-running the push.
//
//	go test -tags=integration ./internal/forge/ -run TestPush -count=1 -v
package forge

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ── spy Forge (records push wire calls; pull methods no-op via fakeForge) ──────

type recordedPatch struct {
	Number int64
	Patch  IssuePatch
}
type recordedComment struct {
	Issue int64
	Body  string
}

type pushSpyForge struct {
	fakeForge
	mu             sync.Mutex
	creates        []IssueCreate
	patches        []recordedPatch
	comments       []recordedComment
	commentPatches []recordedComment
	nextIssue      int64
	nextComment    int64
	wire           int
}

func newPushSpy() *pushSpyForge { return &pushSpyForge{nextIssue: 41, nextComment: 900} }

func (s *pushSpyForge) CreateIssue(_ context.Context, _ RepoRef, in IssueCreate) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wire++
	s.nextIssue++
	s.creates = append(s.creates, in)
	return s.nextIssue, nil
}
func (s *pushSpyForge) UpdateIssue(_ context.Context, _ RepoRef, n int64, p IssuePatch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wire++
	s.patches = append(s.patches, recordedPatch{n, p})
	return nil
}
func (s *pushSpyForge) CreateComment(_ context.Context, _ RepoRef, issue int64, body string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wire++
	s.nextComment++
	s.comments = append(s.comments, recordedComment{issue, body})
	return s.nextComment, nil
}
func (s *pushSpyForge) UpdateComment(_ context.Context, _ RepoRef, id int64, body string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wire++
	s.commentPatches = append(s.commentPatches, recordedComment{id, body})
	return nil
}

func (s *pushSpyForge) wireCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wire
}

// ── helpers ───────────────────────────────────────────────────────────────────

func pusherWithRegistry(pool *pgxpool.Pool) *Pusher {
	reg := blocktype.NewRegistry().Snapshot()
	return NewPusher(pool, func(context.Context, string) *blocktype.Set { return reg })
}

var alwaysAllow = func() bool { return true }

func seedLocalIssue(t *testing.T, pool *pgxpool.Pool, proj store.ProjectRow, seq int, human, body string) string {
	t.Helper()
	ctx := context.Background()
	var id string
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_blocks (category, title, content, scope, type_name, type_source, workflow_status)
		 VALUES ('issue', $1, $2, $3, 'issue', 'manual', 'backlog') RETURNING id::text`,
		fmt.Sprintf("#L%d %s", seq, human), body, proj.Scope).Scan(&id); err != nil {
		t.Fatalf("seed local issue: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_project_sync_map (project_id, entity_kind, forge_id, block_id, base_hash)
		 VALUES ($1::uuid,'issue',0,$2::uuid,'draftbase')`, proj.ID, id); err != nil {
		t.Fatalf("seed local issue mapping: %v", err)
	}
	return id
}

func seedLocalComment(t *testing.T, pool *pgxpool.Pool, proj store.ProjectRow, issueID, title, body string) string {
	t.Helper()
	ctx := context.Background()
	var id string
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_blocks (category, title, content, scope, type_name, type_source, parent_id)
		 VALUES ('comment', $1, $2, $3, 'comment', 'manual', $4::uuid) RETURNING id::text`,
		title, body, proj.Scope, issueID).Scan(&id); err != nil {
		t.Fatalf("seed local comment: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_project_sync_map (project_id, entity_kind, forge_id, block_id, base_hash)
		 VALUES ($1::uuid,'comment',0,$2::uuid,'draftbase')`, proj.ID, id); err != nil {
		t.Fatalf("seed local comment mapping: %v", err)
	}
	return id
}

func blockTitle(t *testing.T, pool *pgxpool.Pool, id string) string {
	t.Helper()
	var title string
	if err := pool.QueryRow(context.Background(), `SELECT title FROM context_blocks WHERE id=$1::uuid`, id).Scan(&title); err != nil {
		t.Fatalf("read title: %v", err)
	}
	return title
}

func mapByBlock(t *testing.T, pool *pgxpool.Pool, id string) (forgeID int64, base string, conflict bool) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT forge_id, base_hash, conflict FROM context_project_sync_map WHERE block_id=$1::uuid`, id).
		Scan(&forgeID, &base, &conflict); err != nil {
		t.Fatalf("read mapping by block: %v", err)
	}
	return forgeID, base, conflict
}

func enablePush(p store.ProjectRow) store.ProjectRow { p.PushEnabled = true; return p }

// ── gates ──────────────────────────────────────────────────────────────────────

// TestPush_DisabledGate (§5.6): push_enabled=false ⇒ 0 wire writes, even with a
// live ctx-ahead candidate. RED without the gate.
func TestPush_DisabledGate(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	proj := seedIFProject(t, pool, "ihdisabled") // push_enabled defaults false
	seedLocalIssue(t, pool, proj, 1, "Fix", "body")
	spy := newPushSpy()
	res, err := pusherWithRegistry(pool).PushProject(context.Background(), proj, spy, RepoRef{Owner: "o", Repo: "r"}, alwaysAllow)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if spy.wireCalls() != 0 || res.Pushed != 0 {
		t.Fatalf("push_enabled=false made wire=%d pushed=%d, want 0/0", spy.wireCalls(), res.Pushed)
	}
}

// TestPush_ConflictGate (§4.5.2): a conflict-flagged mapping ⇒ 0 wire writes
// (conflict is the user's domain). RED if the candidate query dropped `NOT conflict`.
func TestPush_ConflictGate(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	proj := enablePush(seedIFProject(t, pool, "ihconflict"))
	id := seedLocalIssue(t, pool, proj, 1, "Fix", "body")
	if _, err := pool.Exec(context.Background(),
		`UPDATE context_project_sync_map SET conflict=true, conflict_at=now() WHERE block_id=$1::uuid`, id); err != nil {
		t.Fatalf("flag conflict: %v", err)
	}
	spy := newPushSpy()
	res, err := pusherWithRegistry(pool).PushProject(context.Background(), proj, spy, RepoRef{Owner: "o", Repo: "r"}, alwaysAllow)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if spy.wireCalls() != 0 || res.Pushed != 0 {
		t.Fatalf("conflict mapping made wire=%d pushed=%d, want 0/0", spy.wireCalls(), res.Pushed)
	}
}

// TestPush_CreateRenameCascadeOrderIdempotent (§4.5.2 line 416): a local issue +
// its comment ⇒ CreateIssue first, "#L1"→"#42" rename + comment-title cascade +
// base := ctxH, THEN CreateComment on #42. A second run is 0 wire (idempotent —
// no double-rename, no base drift).
func TestPush_CreateRenameCascadeOrderIdempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	proj := enablePush(seedIFProject(t, pool, "ihcreate"))
	issueID := seedLocalIssue(t, pool, proj, 1, "Fix the bug", "steps")
	commentID := seedLocalComment(t, pool, proj, issueID, "#L1.cL1 alice", "a comment")

	p := pusherWithRegistry(pool)
	spy := newPushSpy()
	res, err := p.PushProject(context.Background(), proj, spy, RepoRef{Owner: "o", Repo: "r"}, alwaysAllow)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if res.Pushed != 2 {
		t.Fatalf("pushed=%d, want 2 (issue + comment)", res.Pushed)
	}
	// Order + payloads.
	if len(spy.creates) != 1 || spy.creates[0].Title != "Fix the bug" {
		t.Fatalf("CreateIssue = %+v, want 1 with prefix-free title", spy.creates)
	}
	if len(spy.comments) != 1 || spy.comments[0].Issue != 42 || spy.comments[0].Body != "a comment" {
		t.Fatalf("CreateComment = %+v, want 1 on issue #42 (issue-first ordering)", spy.comments)
	}
	// Rename + cascade.
	if got := blockTitle(t, pool, issueID); got != "#42 Fix the bug" {
		t.Fatalf("issue title = %q, want #42 Fix the bug", got)
	}
	if got := blockTitle(t, pool, commentID); got != "#42.cL1 alice" {
		t.Fatalf("comment title = %q, want #42.cL1 alice (cascade)", got)
	}
	// Mapping: forge_id set, base := ctxH.
	fid, base, _ := mapByBlock(t, pool, issueID)
	if fid != 42 {
		t.Fatalf("issue forge_id = %d, want 42", fid)
	}
	b := getBlock(t, pool, proj.Scope, issueID)
	_, ctxH := CtxIssueBase(b, []string{"done"}, true)
	if base != ctxH {
		t.Fatalf("issue base %s != ctxH %s after push", base, ctxH)
	}
	cfid, _, _ := mapByBlock(t, pool, commentID)
	if cfid != 901 {
		t.Fatalf("comment forge_id = %d, want 901", cfid)
	}

	// Second run: idempotent — 0 wire, title unchanged, base unchanged.
	res2, err := p.PushProject(context.Background(), proj, spy, RepoRef{Owner: "o", Repo: "r"}, alwaysAllow)
	if err != nil {
		t.Fatalf("push 2: %v", err)
	}
	if res2.Pushed != 0 || spy.wireCalls() != 2 {
		t.Fatalf("2nd run pushed=%d wire=%d, want 0 new / wire still 2 (idempotent)", res2.Pushed, spy.wireCalls())
	}
	if got := blockTitle(t, pool, issueID); got != "#42 Fix the bug" {
		t.Fatalf("2nd run re-renamed the title: %q (double-rename bug)", got)
	}
	_, base2, _ := mapByBlock(t, pool, issueID)
	if base2 != base {
		t.Fatalf("2nd run drifted the base: %s → %s", base, base2)
	}
}

// TestPush_UpdateFieldDiff (§4.5.2): a ctx-ahead pulled issue (title + status
// edited, body untouched) ⇒ a PATCH carrying ONLY title+state, no body.
func TestPush_UpdateFieldDiff(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	proj := enablePush(seedIFProject(t, pool, "ihupdate"))
	a := applierWithRegistry(pool)
	iss := IssueRemote{Number: 5, Title: "Old", Body: "b", State: "open", Labels: []string{"bug"}, UpdatedAt: time.Unix(1, 0).UTC()}
	if _, err := a.ApplyIssues(context.Background(), proj, []IssueRemote{iss}); err != nil {
		t.Fatalf("pull-create: %v", err)
	}
	_, _, blockID, _ := readMapping(t, pool, proj.ID, "issue", 5)
	// Local ctx-ahead edit: title + status change, body untouched.
	if _, err := pool.Exec(context.Background(),
		`UPDATE context_blocks SET title='#5 New', workflow_status='done', updated_at=now()+interval '1 second' WHERE id=$1::uuid`, blockID); err != nil {
		t.Fatalf("local edit: %v", err)
	}
	spy := newPushSpy()
	res, err := pusherWithRegistry(pool).PushProject(context.Background(), proj, spy, RepoRef{Owner: "o", Repo: "r"}, alwaysAllow)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if res.Pushed != 1 || len(spy.patches) != 1 {
		t.Fatalf("pushed=%d patches=%d, want 1/1", res.Pushed, len(spy.patches))
	}
	p := spy.patches[0].Patch
	if spy.patches[0].Number != 5 {
		t.Fatalf("patched issue %d, want 5", spy.patches[0].Number)
	}
	if p.Title == nil || *p.Title != "New" || p.State == nil || *p.State != "closed" {
		t.Fatalf("field-diff patch = %+v, want title=New state=closed", p)
	}
	if p.Body != nil {
		t.Fatalf("field-diff pushed the body (%q) though it did not change", *p.Body)
	}
	// base advanced ⇒ 2nd run is 0 wire.
	res2, _ := pusherWithRegistry(pool).PushProject(context.Background(), proj, spy, RepoRef{Owner: "o", Repo: "r"}, alwaysAllow)
	if res2.Pushed != 0 {
		t.Fatalf("2nd run pushed=%d, want 0 (base advanced)", res2.Pushed)
	}
}

// TestPush_TruncatedStatusChangeNoBody (§4.5.2): a truncated issue whose STATUS
// flips (body unchanged) ⇒ PATCH with state, WITHOUT the body field. RED without
// the truncated exclusion (it would try to push a body — or here, since the body
// did not change, the danger is the general rule; the paired body-edit test is
// the harder RED).
func TestPush_TruncatedStatusChangeNoBody(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	proj := enablePush(seedIFProject(t, pool, "ihtruncstat"))
	a := applierWithRegistry(pool)
	big := strings.Repeat("x", store.MaxForgeBodyBytes+2048) // > cap ⇒ truncated
	iss := IssueRemote{Number: 5, Title: "T", Body: big, State: "open", UpdatedAt: time.Unix(1, 0).UTC()}
	if _, err := a.ApplyIssues(context.Background(), proj, []IssueRemote{iss}); err != nil {
		t.Fatalf("pull-create: %v", err)
	}
	_, _, blockID, _ := readMapping(t, pool, proj.ID, "issue", 5)
	b := getBlock(t, pool, proj.Scope, blockID)
	if trunc, _ := b.Metadata["truncated"].(bool); !trunc {
		t.Fatalf("setup: block not flagged truncated")
	}
	// Status flip ONLY (content = the stored capped body, untouched).
	if _, err := pool.Exec(context.Background(),
		`UPDATE context_blocks SET workflow_status='done', updated_at=now()+interval '1 second' WHERE id=$1::uuid`, blockID); err != nil {
		t.Fatalf("status flip: %v", err)
	}
	spy := newPushSpy()
	res, err := pusherWithRegistry(pool).PushProject(context.Background(), proj, spy, RepoRef{Owner: "o", Repo: "r"}, alwaysAllow)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if res.Pushed != 1 || len(spy.patches) != 1 {
		t.Fatalf("pushed=%d patches=%d, want 1/1", res.Pushed, len(spy.patches))
	}
	p := spy.patches[0].Patch
	if p.State == nil || *p.State != "closed" {
		t.Fatalf("patch state = %v, want closed", p.State)
	}
	if p.Body != nil {
		t.Fatalf("truncated issue pushed its body on a status flip (data-loss guard breached)")
	}
	if res.Conflicts != 0 {
		t.Fatalf("status-only flip on a truncated issue wrongly conflicted: %d", res.Conflicts)
	}
}

// TestPush_TruncatedBodyEditConflict (§4.5.2, the hard RED): a truncated issue
// whose BODY is edited locally ⇒ 0 wire writes + conflict (pushing the 50 KB cap
// would overwrite up to ~15 KB of forge body). RED without the truncated logic
// (it would PATCH the truncated body).
func TestPush_TruncatedBodyEditConflict(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	proj := enablePush(seedIFProject(t, pool, "ihtruncbody"))
	a := applierWithRegistry(pool)
	big := strings.Repeat("y", store.MaxForgeBodyBytes+4096)
	iss := IssueRemote{Number: 7, Title: "T", Body: big, State: "open", UpdatedAt: time.Unix(1, 0).UTC()}
	if _, err := a.ApplyIssues(context.Background(), proj, []IssueRemote{iss}); err != nil {
		t.Fatalf("pull-create: %v", err)
	}
	_, _, blockID, _ := readMapping(t, pool, proj.ID, "issue", 7)
	// Local body edit on a truncated block.
	if _, err := pool.Exec(context.Background(),
		`UPDATE context_blocks SET content='locally edited body', updated_at=now()+interval '1 second' WHERE id=$1::uuid`, blockID); err != nil {
		t.Fatalf("body edit: %v", err)
	}
	spy := newPushSpy()
	res, err := pusherWithRegistry(pool).PushProject(context.Background(), proj, spy, RepoRef{Owner: "o", Repo: "r"}, alwaysAllow)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if spy.wireCalls() != 0 {
		t.Fatalf("truncated body edit made %d wire writes, want 0", spy.wireCalls())
	}
	if res.Conflicts != 1 {
		t.Fatalf("truncated body edit conflicts=%d, want 1", res.Conflicts)
	}
	if _, _, conflict := mapByBlock(t, pool, blockID); !conflict {
		t.Fatalf("mapping not flagged conflict after truncated body edit")
	}
}

// TestPush_ThrottleStops (§6.1): when the token bucket runs dry the pass STOPS
// (batch-wise) — the remaining candidates drain on the next run. Here allow()
// yields exactly one token, so one of two local issues is pushed.
func TestPush_ThrottleStops(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	proj := enablePush(seedIFProject(t, pool, "ihthrottle"))
	seedLocalIssue(t, pool, proj, 1, "one", "b1")
	seedLocalIssue(t, pool, proj, 2, "two", "b2")
	n := 0
	allowOnce := func() bool { n++; return n <= 1 }
	spy := newPushSpy()
	res, err := pusherWithRegistry(pool).PushProject(context.Background(), proj, spy, RepoRef{Owner: "o", Repo: "r"}, allowOnce)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if !res.Throttled || res.Pushed != 1 || spy.wireCalls() != 1 {
		t.Fatalf("throttle stop: throttled=%v pushed=%d wire=%d, want true/1/1", res.Throttled, res.Pushed, spy.wireCalls())
	}
}
