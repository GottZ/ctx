//go:build integration

// Issue #486 point 1: a forge-ahead pull wrote the workflow status its forge
// state maps to on every update. The status is finer than the forge state —
// backlog and in-progress are both "open", done and wontfix both "closed" —
// and the projection the 3-way decision compares carries the forge state
// only, so a local move between two statuses of one forge state leaves the
// block "at the base": the next remote edit of the title or the body pulled
// the issue and wrote the mapped status back over the local one, without a
// conflict.
//
// The pull now writes the mapped status where the forge state changed (open
// ↔ closed) or the block has none yet, and leaves it otherwise. The local
// moves run through store.UpdateIssueBlock with the registry snapshot, the
// write the issue_state tool and the REST status route take
// (handler.issueUpdateCore); the registry is the production one
// (Registry.SnapshotForTenant, the snapshot ctxd hands the Applier), with a
// tenant's own issue type written through store.CreateBlockType.
//
//	go test -tags=integration ./internal/forge/ -run 'TestPull.*Status' -count=1 -v
package forge

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// i486TenantWorkflow is a tenant's own issue type: several open and several
// terminal statuses, the forge mapping onto one of each. Admissible as an
// overlay of the builtin (overview and digest stay off, the workflow section
// is freely overlayable — blocktype.NarrowingViolation).
const i486TenantWorkflow = `{"v":1,"overview":{"include":false},"digest":{"include":false},
	"workflow":{"states":["triage","backlog","in-progress","review","done","wontfix","duplicate"],
	            "initial":"triage","terminal":["done","wontfix","duplicate"],
	            "forge_state_map":{"open":"triage","closed":"done"}},
	"structural_link_classes":["references","duplicate-of"]}`

// i486WorkflowWithoutInProgress is a later version of a tenant's issue type
// that no longer has the builtin's in-progress.
const i486WorkflowWithoutInProgress = `{"v":1,"overview":{"include":false},"digest":{"include":false},
	"workflow":{"states":["triage","review","done","wontfix"],"initial":"triage","terminal":["done","wontfix"],
	            "forge_state_map":{"open":"triage","closed":"done"}},
	"structural_link_classes":["references","duplicate-of"]}`

// i486NoWorkflow is a tenant's own issue type without a workflow: the pull
// resolves no status mapping for it (registryOK=false, the §4.5.4 fallback).
const i486NoWorkflow = `{"v":1,"overview":{"include":false},"digest":{"include":false}}`

type i486Env struct {
	pool *pgxpool.Pool
	dsn  string
	proj store.ProjectRow
	reg  *blocktype.Registry
}

func i486Setup(t *testing.T, slug, tenantType string) i486Env {
	t.Helper()
	ctx := context.Background()
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	proj := seedIFProject(t, pool, slug)
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	env := i486Env{pool: pool, dsn: dsn, proj: proj, reg: reg}
	if tenantType != "" {
		env.setTenantType(t, tenantType)
	}
	return env
}

// setTenantType writes (or rewrites) the tenant's own issue type and drops
// the cached generation, as the NOTIFY dispatch does.
func (e i486Env) setTenantType(t *testing.T, config string) {
	t.Helper()
	ctx := context.Background()
	if cur, err := store.GetBlockType(ctx, e.pool, store.IssueTypeName, []string{e.proj.Scope}); err == nil && cur != nil && cur.Scope == e.proj.Scope {
		if _, err := store.UpdateBlockType(ctx, e.pool, cur.ID, []string{e.proj.Scope}, store.BlockTypeUpdate{Config: json.RawMessage(config)}, nil, "i486"); err != nil {
			t.Fatalf("update tenant issue type: %v", err)
		}
	} else if _, err := store.CreateBlockType(ctx, e.pool, store.BlockTypeWrite{
		Name: store.IssueTypeName, Scope: e.proj.Scope, DisplayName: "issue", Config: json.RawMessage(config),
	}, nil, "i486"); err != nil {
		t.Fatalf("create tenant issue type: %v", err)
	}
	e.reg.InvalidateTenant(e.proj.Scope)
}

func (e i486Env) set() *blocktype.Set {
	return e.reg.SnapshotForTenant(context.Background(), e.proj.Scope)
}

func (e i486Env) applier(pool *pgxpool.Pool) *Applier {
	return NewApplier(pool, e.reg.SnapshotForTenant)
}

func (e i486Env) pusher() *Pusher { return NewPusher(e.pool, e.reg.SnapshotForTenant) }

// pull applies one fetched page of issue #5.
func (e i486Env) pull(t *testing.T, iss IssueRemote) ApplyResult {
	t.Helper()
	res, err := e.applier(e.pool).ApplyIssues(context.Background(), e.proj, []IssueRemote{iss})
	if err != nil {
		t.Fatalf("pull %+v: %v", iss, err)
	}
	return res
}

// move is a local status move through the issue verb's write.
func (e i486Env) move(t *testing.T, id, status string) {
	t.Helper()
	ctx := context.Background()
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := store.UpdateIssueBlock(ctx, tx, id, store.IssueUpdate{Status: &status}, e.set(), []string{e.proj.Scope}); err != nil {
		t.Fatalf("local move to %q: %v", status, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// inSync: the mapping holds the remote's hash, is not flagged, the block
// projects onto it under the scope's registry, and nothing is a push
// candidate (the kept status does not make the issue ctx-ahead).
func (e i486Env) inSync(t *testing.T, id string, remote IssueRemote) {
	t.Helper()
	ctx := context.Background()
	base, conflict, _, _ := readMapping(t, e.pool, e.proj.ID, "issue", 5)
	_, forgeH, _, _ := ForgeIssueBase(remote)
	if conflict || base != forgeH {
		t.Errorf("mapping conflict=%v, base is the remote's hash: %v", conflict, base == forgeH)
	}
	pol := resolveIssuePolicy(e.set())
	if _, ctxH := CtxIssueBase(getBlock(t, e.pool, e.proj.Scope, id), pol.terminalSet, pol.registryOK); ctxH != base {
		t.Errorf("the block does not project onto the base after the pull")
	}
	if cands, err := store.ListIssuePushCandidates(ctx, e.pool, e.proj.ID, 10); err != nil || len(cands) != 0 {
		t.Errorf("push candidates after the pull: %d (err %v), want 0", len(cands), err)
	}
}

func i486Remote(state, title, body string, at int64) IssueRemote {
	return IssueRemote{Number: 5, Title: title, Body: body, State: state, UpdatedAt: time.Unix(at, 0).UTC()}
}

// TestPullKeepsALocalStatusOfTheSameForgeState: the block's status was moved
// locally, the forge edited the issue, the pull updates the block. A move
// within one forge state survives the pull; a forge state change writes the
// status the new state maps to, as before.
func TestPullKeepsALocalStatusOfTheSameForgeState(t *testing.T) {
	for _, tc := range []struct {
		name       string
		tenantType string
		pulled     string   // forge state of the pull-create
		created    string   // status the pull-create writes
		moves      []string // local moves, in order
		remote     IssueRemote
		want       string
	}{
		{"builtin, backlog to in-progress, remote body edit", "", "open", "backlog",
			[]string{"in-progress"}, i486Remote("open", "Crane", "body edited on the forge", 2), "in-progress"},
		{"tenant, triage to review, remote title edit", i486TenantWorkflow, "open", "triage",
			[]string{"backlog", "review"}, i486Remote("open", "Crane, renamed on the forge", "forge body", 2), "review"},
		{"tenant, done to wontfix, remote body edit of the closed issue", i486TenantWorkflow, "closed", "done",
			[]string{"wontfix"}, i486Remote("closed", "Crane", "body edited on the forge", 2), "wontfix"},
		{"tenant, review, the forge closes the issue", i486TenantWorkflow, "open", "triage",
			[]string{"review"}, i486Remote("closed", "Crane", "forge body", 2), "done"},
		{"tenant, wontfix, the forge reopens the issue", i486TenantWorkflow, "closed", "done",
			[]string{"wontfix"}, i486Remote("open", "Crane", "reopened on the forge", 2), "triage"},
		{"builtin, in-progress, the forge closes the issue", "", "open", "backlog",
			[]string{"in-progress"}, i486Remote("closed", "Crane", "forge body", 2), "done"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := i486Setup(t, "i486keep", tc.tenantType)
			if r := e.pull(t, i486Remote(tc.pulled, "Crane", "forge body", 1)); r.Applied != 1 {
				t.Fatalf("pull-create: %+v", r)
			}
			_, _, id, _ := readMapping(t, e.pool, e.proj.ID, "issue", 5)
			if got := getBlock(t, e.pool, e.proj.Scope, id).WorkflowStatus; got != tc.created {
				t.Fatalf("pull-create wrote status %q, want %q (the mapped status)", got, tc.created)
			}
			for _, s := range tc.moves {
				e.move(t, id, s)
			}

			if r := e.pull(t, tc.remote); r.Applied != 1 || r.Conflicts != 0 {
				t.Fatalf("pull-update: %+v, want one applied, no conflict", r)
			}
			b := getBlock(t, e.pool, e.proj.Scope, id)
			if b.WorkflowStatus != tc.want {
				t.Errorf("status after the pull = %q, want %q", b.WorkflowStatus, tc.want)
			}
			if b.Content != tc.remote.Body || b.Title != "#5 "+tc.remote.Title {
				t.Errorf("the pull did not write the remote edit: title %q body %q", b.Title, b.Content)
			}
			e.inSync(t, id, tc.remote)
			if r := e.pull(t, tc.remote); r.Applied != 0 || r.Conflicts != 0 {
				t.Errorf("a second identical pull: %+v, want a no-op", r)
			}
		})
	}
}

// TestPullKeepsATerminalStatusThePushClosedWith: a local close with a status
// the forge mapping does not produce (wontfix) is pushed — the forge only
// learns "closed" — and the forge then edits the closed issue's body. The
// pull compares the remote state with the state the block projects to, not
// with the forge state metadata of the last pull (still "open" here), and
// keeps wontfix.
func TestPullKeepsATerminalStatusThePushClosedWith(t *testing.T) {
	ctx := context.Background()
	e := i486Setup(t, "i486pushed", i486TenantWorkflow)
	e.proj = enablePush(e.proj)
	e.pull(t, i486Remote("open", "Crane", "forge body", 1))
	_, _, id, _ := readMapping(t, e.pool, e.proj.ID, "issue", 5)
	e.move(t, id, "wontfix")

	spy := newPushSpy()
	if res, err := e.pusher().PushProject(ctx, e.proj, spy, RepoRef{Owner: "o", Repo: "r"}, alwaysAllow); err != nil || res.Pushed != 1 ||
		len(spy.patches) != 1 || spy.patches[0].Patch.State == nil || *spy.patches[0].Patch.State != "closed" {
		t.Fatalf("push: pushed=%d patches=%+v err=%v, want one PATCH of state closed", res.Pushed, spy.patches, err)
	}

	remote := i486Remote("closed", "Crane", "body edited on the forge after the close", 2)
	if r := e.pull(t, remote); r.Applied != 1 || r.Conflicts != 0 {
		t.Fatalf("pull-update: %+v, want one applied, no conflict", r)
	}
	if b := getBlock(t, e.pool, e.proj.Scope, id); b.WorkflowStatus != "wontfix" || b.Content != remote.Body {
		t.Errorf("after the pull: status %q body %q, want wontfix and the remote body", b.WorkflowStatus, b.Content)
	}
	e.inSync(t, id, remote)
}

// TestPullWithoutAWorkflowStatusMapping: the tenant redefined its issue type
// without a workflow, so the pull resolves no status (registryOK=false). An
// edit of the same forge state leaves the status the block carries; a forge
// state change writes NULL, as before — the status it held belongs to the
// other forge state, and a non-terminal status on a closed issue would make
// the block project to "open" once a workflow resolves again and push a
// reopen.
func TestPullWithoutAWorkflowStatusMapping(t *testing.T) {
	for _, tc := range []struct {
		name   string
		remote IssueRemote
		want   string
	}{
		{"same forge state, remote body edit", i486Remote("open", "Crane", "body edited on the forge", 2), "in-progress"},
		{"the forge closes the issue", i486Remote("closed", "Crane", "forge body", 2), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := i486Setup(t, "i486nowf", "")
			e.pull(t, i486Remote("open", "Crane", "forge body", 1))
			_, _, id, _ := readMapping(t, e.pool, e.proj.ID, "issue", 5)
			e.move(t, id, "in-progress")
			e.setTenantType(t, i486NoWorkflow)
			if pol := resolveIssuePolicy(e.set()); pol.registryOK {
				t.Fatalf("precondition: the tenant's issue type still resolves a workflow")
			}

			if r := e.pull(t, tc.remote); r.Applied != 1 || r.Conflicts != 0 {
				t.Fatalf("pull-update: %+v, want one applied, no conflict", r)
			}
			b := getBlock(t, e.pool, e.proj.Scope, id)
			if b.WorkflowStatus != tc.want {
				t.Errorf("status after the pull = %q, want %q", b.WorkflowStatus, tc.want)
			}
			if fs, _ := b.Metadata["forge_state"].(string); fs != tc.remote.State {
				t.Errorf("metadata.forge_state = %q, want %q", fs, tc.remote.State)
			}
			e.inSync(t, id, tc.remote)

			// The workflow resolves again: the block still projects onto the
			// base, so neither side is ahead.
			e.setTenantType(t, i486TenantWorkflow)
			e.inSync(t, id, tc.remote)
		})
	}
}

// TestPullWritesTheMappedStatusIntoABlockWithoutOne: a block pulled while no
// workflow resolved carries no status; once one resolves, the next pull
// update writes the status its forge state maps to (unchanged behaviour —
// there is no local status to keep).
func TestPullWritesTheMappedStatusIntoABlockWithoutOne(t *testing.T) {
	e := i486Setup(t, "i486null", i486NoWorkflow)
	e.pull(t, i486Remote("open", "Crane", "forge body", 1))
	_, _, id, _ := readMapping(t, e.pool, e.proj.ID, "issue", 5)
	if got := getBlock(t, e.pool, e.proj.Scope, id).WorkflowStatus; got != "" {
		t.Fatalf("precondition: pull-create without a workflow wrote status %q", got)
	}
	e.setTenantType(t, i486TenantWorkflow)

	remote := i486Remote("open", "Crane", "body edited on the forge", 2)
	if r := e.pull(t, remote); r.Applied != 1 || r.Conflicts != 0 {
		t.Fatalf("pull-update: %+v, want one applied, no conflict", r)
	}
	if got := getBlock(t, e.pool, e.proj.Scope, id).WorkflowStatus; got != "triage" {
		t.Errorf("status after the pull = %q, want triage (the mapped status)", got)
	}
	e.inSync(t, id, remote)
}

// TestPullHealsAStatusTheWorkflowNoLongerHas: the tenant changed its issue
// type's workflow and dropped the status the block carries. No local verb can
// move the block any more (ValidateTransition refuses a source that is not a
// configured status), and the board does not list it; the next pull update
// writes the status the forge state maps to, as it did before, instead of
// keeping the stranded one.
func TestPullHealsAStatusTheWorkflowNoLongerHas(t *testing.T) {
	ctx := context.Background()
	e := i486Setup(t, "i486strand", "")
	e.pull(t, i486Remote("open", "Crane", "forge body", 1))
	_, _, id, _ := readMapping(t, e.pool, e.proj.ID, "issue", 5)
	e.move(t, id, "in-progress")
	e.setTenantType(t, i486WorkflowWithoutInProgress)

	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	review := "review"
	if _, err := store.UpdateIssueBlock(ctx, tx, id, store.IssueUpdate{Status: &review}, e.set(), []string{e.proj.Scope}); !errors.Is(err, blocktype.ErrInvalidTransition) {
		t.Fatalf("precondition: a local move out of the dropped status answered %v, want ErrInvalidTransition", err)
	}
	_ = tx.Rollback(ctx)

	remote := i486Remote("open", "Crane", "body edited on the forge", 2)
	if r := e.pull(t, remote); r.Applied != 1 || r.Conflicts != 0 {
		t.Fatalf("pull-update: %+v, want one applied, no conflict", r)
	}
	if got := getBlock(t, e.pool, e.proj.Scope, id).WorkflowStatus; got != "triage" {
		t.Errorf("status after the pull = %q, want triage (the mapped status of the workflow that resolves now)", got)
	}
	e.inSync(t, id, remote)
}

// pullHeld runs one pull of remote on a pool of the scope's registry
// whose write transaction waits at its BEGIN; during the hold, between the
// read and the write, it calls between.
func (e i486Env) pullHeld(t *testing.T, remote IssueRemote, between func()) ApplyResult {
	t.Helper()
	ctx := context.Background()
	held, hold := i452HoldBeginPool(t, e.dsn)
	type out struct {
		res ApplyResult
		err error
	}
	done := make(chan out, 1)
	go func() {
		r, err := e.applier(held).ApplyIssues(ctx, e.proj, []IssueRemote{remote})
		done <- out{r, err}
	}()
	select {
	case <-hold.entered:
	case o := <-done:
		t.Fatalf("the pull finished without a write transaction: %+v %v", o.res, o.err)
	case <-time.After(i452Wait):
		t.Fatalf("the pull did not reach its write within %s", i452Wait)
	}
	between()
	close(hold.release)
	select {
	case o := <-done:
		if o.err != nil {
			t.Fatalf("pull: %v", o.err)
		}
		return o.res
	case <-time.After(i452Wait):
		t.Fatalf("the pull did not finish within %s", i452Wait)
	}
	return ApplyResult{}
}

// TestPullKeepsAStatusMovedDuringThePull: the pull read the block, decided
// "forge-ahead", and a local status move within the forge state commits
// before the pull's transaction locks the block. The move leaves the
// projection alone, so the pull still writes the remote edit — and decides
// on the block it locked, not the one it read: a block read without a status
// and given one during the pull keeps it too.
func TestPullKeepsAStatusMovedDuringThePull(t *testing.T) {
	for _, tc := range []struct {
		name       string
		createType string // the tenant's issue type at the pull-create ("" = builtin)
		pullType   string // the tenant's issue type from then on ("" = unchanged)
		move       string
	}{
		{"a status set before", "", "", "in-progress"},
		{"no status before (pulled without a workflow)", i486NoWorkflow, i486TenantWorkflow, "review"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := i486Setup(t, "i486during", tc.createType)
			e.pull(t, i486Remote("open", "Crane", "forge body", 1))
			_, _, id, _ := readMapping(t, e.pool, e.proj.ID, "issue", 5)
			if tc.pullType != "" {
				e.setTenantType(t, tc.pullType)
			}

			remote := i486Remote("open", "Crane", "body edited on the forge", 2)
			res := e.pullHeld(t, remote, func() { e.move(t, id, tc.move) })

			if res.Applied != 1 || res.Conflicts != 0 {
				t.Fatalf("pull: %+v, want one applied, no conflict", res)
			}
			b := getBlock(t, e.pool, e.proj.Scope, id)
			if b.WorkflowStatus != tc.move || b.Content != remote.Body {
				t.Errorf("after the pull: status %q body %q, want %q and the remote body", b.WorkflowStatus, b.Content, tc.move)
			}
			e.inSync(t, id, remote)
		})
	}
}
