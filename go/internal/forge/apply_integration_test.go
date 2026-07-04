//go:build integration

// I-G Pull-APPLY gates against a real DB (design/02 §4.5.2/§4.5.4/§4.5.7): the
// 5×-direction matrix incl. both creation cases, idempotency (0 writes / 0
// conflicts on a re-run, W16 timestamp-bump probe), the projection golden
// (ctxH == base byte-identical), references edges (+ no phantom), the forge_state
// metadata-only fallback, the both-ahead conflict, and the per-page cursor commit
// (apply error mid-page ⇒ cursor un-advanced).
//
//	go test -tags=integration ./internal/forge/ -run TestApply -count=1 -v
package forge

import (
	"context"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// applierWithRegistry builds an Applier whose snapshot resolves the builtin issue
// workflow (backlog/in-progress/done, forge open→backlog closed→done).
func applierWithRegistry(pool *pgxpool.Pool) *Applier {
	reg := blocktype.NewRegistry().Snapshot()
	return NewApplier(pool, func(context.Context, string) *blocktype.Set { return reg })
}

func readMapping(t *testing.T, pool *pgxpool.Pool, projectID, kind string, forgeID int64) (base string, conflict bool, blockID string, conflictAt *time.Time) {
	t.Helper()
	err := pool.QueryRow(context.Background(),
		`SELECT base_hash, conflict, block_id::text, conflict_at FROM context_project_sync_map
		  WHERE project_id=$1::uuid AND entity_kind=$2 AND forge_id=$3`,
		projectID, kind, forgeID).Scan(&base, &conflict, &blockID, &conflictAt)
	if err != nil {
		t.Fatalf("read mapping (%s #%d): %v", kind, forgeID, err)
	}
	return base, conflict, blockID, conflictAt
}

func getBlock(t *testing.T, pool *pgxpool.Pool, scope, id string) *store.Block {
	t.Helper()
	b, err := store.GetIssue(context.Background(), pool, id, []string{scope}, nil)
	if err != nil {
		t.Fatalf("get block %s: %v", id, err)
	}
	return b
}

// TestApply_PullCreateAndGolden: a forge-only issue ⇒ pull-create (block with the
// "#<nr>" title + mapping + base := forgeH), AND the projection golden — a freshly
// pulled issue has ctxH == base byte-identical (§3.6 negative probe).
func TestApply_PullCreateAndGolden(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	proj := seedIFProject(t, pool, "iggolden")
	a := applierWithRegistry(pool)
	iss := IssueRemote{Number: 7, Title: "Fix the bug", Body: "steps", State: "open",
		Labels: []string{"bug", "p1"}, UpdatedAt: time.Now().UTC()}

	res, err := a.ApplyIssues(context.Background(), proj, []IssueRemote{iss})
	if err != nil || res.Applied != 1 || res.Conflicts != 0 {
		t.Fatalf("pull-create: res=%+v err=%v, want Applied 1 Conflicts 0", res, err)
	}
	base, conflict, blockID, _ := readMapping(t, pool, proj.ID, "issue", 7)
	if conflict {
		t.Fatalf("fresh pull flagged conflict")
	}
	b := getBlock(t, pool, proj.Scope, blockID)
	if b.Title != "#7 Fix the bug" {
		t.Fatalf("block title = %q, want #7 Fix the bug", b.Title)
	}
	if b.WorkflowStatus != "backlog" {
		t.Fatalf("workflow_status = %q, want backlog (open→backlog)", b.WorkflowStatus)
	}
	forgeH, _, _ := ForgeIssueHash(iss)
	if base != forgeH {
		t.Fatalf("base %s != forgeH %s", base, forgeH)
	}
	ctxH := CtxIssueHash(b, []string{"done"}, true)
	if ctxH != base {
		t.Fatalf("GOLDEN violated: ctxH %s != base %s", ctxH, base)
	}
}

// TestApply_Idempotent: a second identical run ⇒ 0 block writes AND 0 conflicts.
// The second run BUMPS updated_at with unchanged content — a timestamp-comparing
// implementation (W16) would re-write; the hash comparison must not.
func TestApply_Idempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	proj := seedIFProject(t, pool, "igidem")
	a := applierWithRegistry(pool)
	iss := IssueRemote{Number: 3, Title: "T", Body: "B", State: "open", UpdatedAt: time.Unix(1000, 0).UTC()}
	if _, err := a.ApplyIssues(context.Background(), proj, []IssueRemote{iss}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	base1, _, blockID, _ := readMapping(t, pool, proj.ID, "issue", 3)
	before := getBlock(t, pool, proj.Scope, blockID)

	iss.UpdatedAt = time.Unix(999999, 0).UTC() // W16: timestamp changes, content does not
	res, err := a.ApplyIssues(context.Background(), proj, []IssueRemote{iss})
	if err != nil || res.Applied != 0 || res.Conflicts != 0 {
		t.Fatalf("second run: res=%+v err=%v, want Applied 0 Conflicts 0 (W16)", res, err)
	}
	after := getBlock(t, pool, proj.Scope, blockID)
	if !before.UpdatedAt.Equal(after.UpdatedAt) {
		t.Fatalf("block was re-written on idempotent run: %v → %v (W16 timestamp trap)", before.UpdatedAt, after.UpdatedAt)
	}
	base2, conflict, _, _ := readMapping(t, pool, proj.ID, "issue", 3)
	if base2 != base1 || conflict {
		t.Fatalf("mapping changed on idempotent run: base %s→%s conflict=%v", base1, base2, conflict)
	}
}

// TestApply_ForgeAhead: base==ctxH, base!=forgeH ⇒ pull-update (block updated,
// base := forgeH).
func TestApply_ForgeAhead(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	proj := seedIFProject(t, pool, "igforge")
	a := applierWithRegistry(pool)
	iss := IssueRemote{Number: 1, Title: "Old", Body: "b", State: "open", UpdatedAt: time.Unix(1, 0).UTC()}
	if _, err := a.ApplyIssues(context.Background(), proj, []IssueRemote{iss}); err != nil {
		t.Fatalf("create: %v", err)
	}
	iss2 := iss
	iss2.Title = "New title"
	iss2.State = "closed"
	iss2.UpdatedAt = time.Unix(2, 0).UTC()
	res, err := a.ApplyIssues(context.Background(), proj, []IssueRemote{iss2})
	if err != nil || res.Applied != 1 {
		t.Fatalf("forge-ahead: res=%+v err=%v, want Applied 1", res, err)
	}
	base, _, blockID, _ := readMapping(t, pool, proj.ID, "issue", 1)
	b := getBlock(t, pool, proj.Scope, blockID)
	if b.Title != "#1 New title" || b.WorkflowStatus != "done" {
		t.Fatalf("block not pulled: title=%q status=%q", b.Title, b.WorkflowStatus)
	}
	forgeH, _, _ := ForgeIssueHash(iss2)
	if base != forgeH {
		t.Fatalf("base not advanced to forgeH: %s != %s", base, forgeH)
	}
}

// TestApply_CtxAhead: base!=ctxH, base==forgeH ⇒ NOTHING (push is I-H). The block
// stays at its local edit; base is untouched.
func TestApply_CtxAhead(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	proj := seedIFProject(t, pool, "igctx")
	a := applierWithRegistry(pool)
	iss := IssueRemote{Number: 1, Title: "T", Body: "orig", State: "open", UpdatedAt: time.Unix(1, 0).UTC()}
	if _, err := a.ApplyIssues(context.Background(), proj, []IssueRemote{iss}); err != nil {
		t.Fatalf("create: %v", err)
	}
	base0, _, blockID, _ := readMapping(t, pool, proj.ID, "issue", 1)
	// Local edit (ctx-ahead): content diverges from base, forge unchanged.
	if _, err := pool.Exec(context.Background(),
		`UPDATE context_blocks SET content='LOCAL EDIT', updated_at=now() WHERE id=$1::uuid`, blockID); err != nil {
		t.Fatalf("local edit: %v", err)
	}
	res, err := a.ApplyIssues(context.Background(), proj, []IssueRemote{iss}) // same forge payload
	if err != nil || res.Applied != 0 || res.Conflicts != 0 {
		t.Fatalf("ctx-ahead: res=%+v err=%v, want 0/0 (push is I-H)", res, err)
	}
	b := getBlock(t, pool, proj.Scope, blockID)
	if b.Content != "LOCAL EDIT" {
		t.Fatalf("ctx-ahead overwrote the local edit: %q", b.Content)
	}
	base1, conflict, _, _ := readMapping(t, pool, proj.ID, "issue", 1)
	if base1 != base0 || conflict {
		t.Fatalf("ctx-ahead touched the mapping: base %s→%s conflict=%v", base0, base1, conflict)
	}
}

// TestApply_Converged: base!=ctxH!=base, ctxH==forgeH ⇒ base := ctxH, NO block
// write (both sides independently reached the same content).
func TestApply_Converged(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	proj := seedIFProject(t, pool, "igconv")
	a := applierWithRegistry(pool)
	iss := IssueRemote{Number: 1, Title: "T", Body: "orig", State: "open", UpdatedAt: time.Unix(1, 0).UTC()}
	if _, err := a.ApplyIssues(context.Background(), proj, []IssueRemote{iss}); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, _, blockID, _ := readMapping(t, pool, proj.ID, "issue", 1)
	if _, err := pool.Exec(context.Background(),
		`UPDATE context_blocks SET content='SAME', updated_at=now() WHERE id=$1::uuid`, blockID); err != nil {
		t.Fatalf("local edit: %v", err)
	}
	before := getBlock(t, pool, proj.Scope, blockID)
	iss2 := iss
	iss2.Body = "SAME" // forge independently converged to the same content
	iss2.UpdatedAt = time.Unix(2, 0).UTC()
	res, err := a.ApplyIssues(context.Background(), proj, []IssueRemote{iss2})
	if err != nil || res.Applied != 0 || res.Conflicts != 0 {
		t.Fatalf("converged: res=%+v err=%v, want 0/0", res, err)
	}
	after := getBlock(t, pool, proj.Scope, blockID)
	if !before.UpdatedAt.Equal(after.UpdatedAt) {
		t.Fatalf("converged wrote the block: %v → %v (should advance base only)", before.UpdatedAt, after.UpdatedAt)
	}
	base, _, _, _ := readMapping(t, pool, proj.ID, "issue", 1)
	ctxH := CtxIssueHash(after, []string{"done"}, true)
	if base != ctxH {
		t.Fatalf("converged did not advance base to ctxH: %s != %s", base, ctxH)
	}
}

// TestApply_Conflict: both-ahead (base!=ctxH!=forgeH!=base) ⇒ conflict persisted,
// block unchanged, 0 writes both ways. A re-run is idempotent (0 new conflicts,
// conflict_at preserved) — RED against last-write-wins.
func TestApply_Conflict(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	proj := seedIFProject(t, pool, "igconflict")
	a := applierWithRegistry(pool)
	iss := IssueRemote{Number: 1, Title: "T", Body: "orig", State: "open", UpdatedAt: time.Unix(1, 0).UTC()}
	if _, err := a.ApplyIssues(context.Background(), proj, []IssueRemote{iss}); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, _, blockID, _ := readMapping(t, pool, proj.ID, "issue", 1)
	if _, err := pool.Exec(context.Background(),
		`UPDATE context_blocks SET content='LOCAL', updated_at=now() WHERE id=$1::uuid`, blockID); err != nil {
		t.Fatalf("local edit: %v", err)
	}
	before := getBlock(t, pool, proj.Scope, blockID)
	iss2 := iss
	iss2.Body = "REMOTE" // forge diverged differently ⇒ both-ahead
	iss2.UpdatedAt = time.Unix(2, 0).UTC()
	res, err := a.ApplyIssues(context.Background(), proj, []IssueRemote{iss2})
	if err != nil || res.Conflicts != 1 || res.Applied != 0 {
		t.Fatalf("conflict: res=%+v err=%v, want Conflicts 1 Applied 0", res, err)
	}
	after := getBlock(t, pool, proj.Scope, blockID)
	if after.Content != "LOCAL" {
		t.Fatalf("conflict overwrote the block (last-write-wins): %q", after.Content)
	}
	_, conflict, _, at1 := readMapping(t, pool, proj.ID, "issue", 1)
	if !conflict || at1 == nil {
		t.Fatalf("conflict not persisted: conflict=%v at=%v", conflict, at1)
	}
	if !before.UpdatedAt.Equal(after.UpdatedAt) {
		t.Fatalf("block updated_at moved on conflict: %v → %v", before.UpdatedAt, after.UpdatedAt)
	}
	// Re-run: idempotent — 0 new conflicts, conflict_at preserved.
	res2, _ := a.ApplyIssues(context.Background(), proj, []IssueRemote{iss2})
	if res2.Conflicts != 0 {
		t.Fatalf("conflict re-run counted %d new conflicts, want 0", res2.Conflicts)
	}
	_, _, _, at2 := readMapping(t, pool, proj.ID, "issue", 1)
	if at2 == nil || !at2.Equal(*at1) {
		t.Fatalf("conflict_at not preserved on re-flag: %v → %v", at1, at2)
	}
}

// TestApply_CtxOnlyMappingUntouched (creation case 2): a local-only mapping
// (forge_id=0, an issue-create not yet pushed) is NOT touched by a pull of a
// DIFFERENT forge issue.
func TestApply_CtxOnlyMappingUntouched(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	proj := seedIFProject(t, pool, "igctxonly")
	a := applierWithRegistry(pool)
	// Seed a local-only block + mapping forge_id=0.
	var localBlock string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO context_blocks (category, title, content, scope, type_name)
		 VALUES ('issue','#L1 local','body',$1,'issue') RETURNING id::text`, proj.Scope).Scan(&localBlock); err != nil {
		t.Fatalf("seed local block: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO context_project_sync_map (project_id, entity_kind, forge_id, block_id, base_hash)
		 VALUES ($1::uuid,'issue',0,$2::uuid,'localbase')`, proj.ID, localBlock); err != nil {
		t.Fatalf("seed local mapping: %v", err)
	}
	// Pull a real forge issue (number 5).
	if _, err := a.ApplyIssues(context.Background(), proj,
		[]IssueRemote{{Number: 5, Title: "remote", State: "open", UpdatedAt: time.Now().UTC()}}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	base, _, blockID, _ := readMapping(t, pool, proj.ID, "issue", 0)
	if base != "localbase" || blockID != localBlock {
		t.Fatalf("local-only mapping mutated: base=%q block=%q", base, blockID)
	}
	_, _, _, _ = readMapping(t, pool, proj.ID, "issue", 5) // #5 pull-created (fatals if absent)
}

// TestApply_References: "#N" bodies ⇒ references edges via the mapping; unknown
// numbers ⇒ NO phantom edge (§4.5.7). RED without the mapping existence check.
func TestApply_References(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	proj := seedIFProject(t, pool, "igrefs")
	a := applierWithRegistry(pool)
	// One page: #1, #2 exist; #3 references #1, #2 and the unknown #999 (+ itself #3).
	page := []IssueRemote{
		{Number: 1, Title: "one", State: "open", UpdatedAt: time.Unix(1, 0).UTC()},
		{Number: 2, Title: "two", State: "open", UpdatedAt: time.Unix(2, 0).UTC()},
		{Number: 3, Title: "three", Body: "relates to #1 and #2, dup of #999, self #3", State: "open", UpdatedAt: time.Unix(3, 0).UTC()},
	}
	if _, err := a.ApplyIssues(context.Background(), proj, page); err != nil {
		t.Fatalf("apply: %v", err)
	}
	_, _, src, _ := readMapping(t, pool, proj.ID, "issue", 3)
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM context_structural_links WHERE source_block_id=$1::uuid AND link_class='references'`,
		src).Scan(&n); err != nil {
		t.Fatalf("count edges: %v", err)
	}
	if n != 2 {
		t.Fatalf("references edges = %d, want 2 (#1,#2; NOT #999 phantom, NOT self #3)", n)
	}
	// Verify the targets are exactly #1 and #2.
	_, _, b1, _ := readMapping(t, pool, proj.ID, "issue", 1)
	_, _, b2, _ := readMapping(t, pool, proj.ID, "issue", 2)
	for _, tgt := range []string{b1, b2} {
		var ok bool
		if err := pool.QueryRow(context.Background(),
			`SELECT EXISTS(SELECT 1 FROM context_structural_links WHERE source_block_id=$1::uuid AND target_block_id=$2::uuid AND link_class='references')`,
			src, tgt).Scan(&ok); err != nil || !ok {
			t.Fatalf("missing references edge %s→%s (ok=%v err=%v)", src, tgt, ok, err)
		}
	}
	// Re-run is idempotent (ON CONFLICT DO NOTHING) — still 2 edges.
	if _, err := a.ApplyIssues(context.Background(), proj, page); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM context_structural_links WHERE source_block_id=$1::uuid`, src).Scan(&n); err != nil {
		t.Fatalf("recount: %v", err)
	}
	if n != 2 {
		t.Fatalf("edges after re-run = %d, want 2 (idempotent)", n)
	}
}

// TestApply_FallbackNoRegistry: registry unresolvable ⇒ workflow_status stays
// NULL, forge state goes to metadata.forge_state only (§4.5.4), golden still
// holds (ctxH == base).
func TestApply_FallbackNoRegistry(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	proj := seedIFProject(t, pool, "igfallback")
	a := NewApplier(pool, func(context.Context, string) *blocktype.Set { return nil }) // registry unresolvable
	iss := IssueRemote{Number: 9, Title: "T", Body: "B", State: "closed", UpdatedAt: time.Now().UTC()}
	if _, err := a.ApplyIssues(context.Background(), proj, []IssueRemote{iss}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	base, _, blockID, _ := readMapping(t, pool, proj.ID, "issue", 9)
	b := getBlock(t, pool, proj.Scope, blockID)
	if b.WorkflowStatus != "" {
		t.Fatalf("workflow_status = %q, want NULL/empty (no registry)", b.WorkflowStatus)
	}
	if fs, _ := b.Metadata["forge_state"].(string); fs != "closed" {
		t.Fatalf("metadata.forge_state = %q, want closed", fs)
	}
	if ctxH := CtxIssueHash(b, nil, false); ctxH != base {
		t.Fatalf("fallback golden violated: ctxH %s != base %s", ctxH, base)
	}
}

// TestApply_CursorAdvancesOnSuccess: after a successful page apply the issue
// cursor since advances to the page's max updated_at.
func TestApply_CursorAdvancesOnSuccess(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	proj := seedIFProject(t, pool, "igcursor")
	t1 := time.Unix(1000, 0).UTC()
	fake := &fakeForge{pages: []IssuePage{{ETag: "e1", Issues: []IssueRemote{{Number: 1, Title: "a", State: "open", UpdatedAt: t1}}}}}
	m := mgr(pool, okTenant, okPolicy, fake,
		func(context.Context, store.ProjectRow, []IssueRemote) (ApplyResult, error) { return ApplyResult{Applied: 1}, nil })
	if _, err := m.StartSync(context.Background(), proj, false); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitDone(t, m, proj.ID)
	got, _ := store.GetProjectByID(context.Background(), pool, proj.ID)
	etag, since, _ := readCursor(got.SyncCursor)
	if !since.Equal(t1) {
		t.Fatalf("issue cursor since = %v, want %v", since, t1)
	}
	if etag != "e1" {
		t.Fatalf("issue cursor etag = %q, want e1", etag)
	}
}

// TestApply_CursorNotAdvancedOnApplyError: an apply error mid-page ⇒ the fetch
// cursor is NOT advanced (resume re-fetches without skipping). RED against a
// commit-before-apply implementation.
func TestApply_CursorNotAdvancedOnApplyError(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	proj := seedIFProject(t, pool, "igcursorerr")
	fake := &fakeForge{pages: []IssuePage{{ETag: "e1", Issues: []IssueRemote{
		{Number: 1, Title: "a", State: "open", UpdatedAt: time.Unix(1000, 0).UTC()},
		{Number: 2, Title: "b", State: "open", UpdatedAt: time.Unix(2000, 0).UTC()},
	}}}}
	m := mgr(pool, okTenant, okPolicy, fake,
		func(context.Context, store.ProjectRow, []IssueRemote) (ApplyResult, error) {
			return ApplyResult{}, context.DeadlineExceeded // apply fails
		})
	if _, err := m.StartSync(context.Background(), proj, false); err != nil {
		t.Fatalf("start: %v", err)
	}
	st := waitDone(t, m, proj.ID)
	if !st.Aborted {
		t.Fatalf("apply error should abort the run")
	}
	got, _ := store.GetProjectByID(context.Background(), pool, proj.ID)
	_, since, _ := readCursor(got.SyncCursor)
	if !since.IsZero() {
		t.Fatalf("cursor advanced to %v despite apply error (skip risk)", since)
	}
}
