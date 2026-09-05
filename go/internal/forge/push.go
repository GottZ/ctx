// push.go — the I-H PUSH pass (Achse 02, Welle I-H; design/02 §4.5.2,
// §5.6, §6.1). It is the mirror of the I-G Pull-APPLY: I-G reconciles forge→ctx
// and leaves the ctx-ahead branch untouched (apply.go: "push is I-H"); this pass
// drains exactly those ctx-ahead entities back onto the forge.
//
// A mapping reaches the push because, after the pull leg ran, its block still
// projects to a hash != base_hash and it is NOT conflict-flagged — i.e. the pull
// already set base := forgeH (or would have conflicted) for anything the forge
// changed, so a surviving base != ctxH is, by construction, ctx-ahead
// (base == forgeH). The push therefore needs NO extra fetch to decide direction.
//
// Cases (§4.5.2):
//   - forge_id == 0  ⇒ CREATE: POST the issue, write forge_id, rename
//     "#L<seq>"→"#<nr>" + cascade comment titles + base := ctxH (one Tx).
//   - forge_id  > 0  ⇒ UPDATE: a FIELD-DIFF PATCH (only diverged fields; a status
//     flip never carries the body). A truncated body is HARD-excluded — if the
//     (truncated) body is what diverged, the entity is flagged conflict, never
//     written (data-loss guard: the 50 KB cap would overwrite a 64 KB forge body).
//   - comments push AFTER their issue (CreateComment needs the forge number).
//
// GATES (§5.6, §6.1): push_enabled=false ⇒ 0 wire writes; conflict ⇒ 0 wire
// writes; every content-POST passes the token-scoped throttle (allow()) — an
// empty bucket STOPS the pass (batch-wise, the rest drain next run).
package forge

import (
	"context"
	"time"

	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pushBatchLimit bounds the candidates enumerated per run (§6, 10k+ issues/repo).
// The throttle caps the actual wire rate; this caps the DB enumeration so one run
// is bounded work. Remaining candidates drain on the next run.
const pushBatchLimit = 500

// PushResult is what the push pass reports back to the sync run.
type PushResult struct {
	Pushed    int  // entities written to the forge (create + update + comment)
	Conflicts int  // truncated-body divergences flagged, never written
	Throttled bool // the token bucket ran dry — the pass stopped early
}

// Pusher owns the I-H push pass. It holds the pool (candidate enumeration +
// finalise/advance writes) and the registry snapshot (for the ctx state
// derivation, mirror of the Applier).
type Pusher struct {
	pool     *pgxpool.Pool
	snapshot SnapshotFunc
	clock    func() time.Time
}

// NewPusher builds the push pass. snapshot resolves the issue workflow per scope
// (a nil/registry-less resolve degrades the state derivation to metadata, exactly
// like the pull — §4.5.4).
func NewPusher(pool *pgxpool.Pool, snapshot SnapshotFunc) *Pusher {
	return &Pusher{pool: pool, snapshot: snapshot, clock: time.Now}
}

// PushProject drains ctx-ahead entities for one project. allow() gates each
// content-POST (the token bucket, bound by the caller to the credential key).
// push_enabled=false ⇒ 0 wire writes (the fail-closed §5.6 gate). client carries
// the push methods; repo is the resolved owner/repo.
func (p *Pusher) PushProject(ctx context.Context, project store.ProjectRow, client Forge, repo RepoRef, allow func() bool) (PushResult, error) {
	var res PushResult
	if !project.PushEnabled {
		return res, nil // §5.6: pull-only until an explicit tenant-admin toggle
	}
	pol := resolveIssuePolicy(p.snapshot(ctx, project.Scope))

	// ── issue leg (before comments so a freshly created number is visible) ──
	issues, err := store.ListIssuePushCandidates(ctx, p.pool, project.ID, pushBatchLimit)
	if err != nil {
		return res, err
	}
	for i := range issues {
		cont, err := p.pushIssue(ctx, project, client, repo, pol, allow, &issues[i], &res)
		if err != nil {
			return res, err
		}
		if !cont {
			return res, nil // throttled — stop, next run continues
		}
	}

	// ── comment leg ──
	comments, err := store.ListCommentPushCandidates(ctx, p.pool, project.ID, pushBatchLimit)
	if err != nil {
		return res, err
	}
	for i := range comments {
		cont, err := p.pushComment(ctx, client, repo, allow, &comments[i], &res)
		if err != nil {
			return res, err
		}
		if !cont {
			return res, nil
		}
	}
	return res, nil
}

// pushIssue pushes one candidate issue. Returns cont=false when the throttle ran
// dry (the caller stops the pass).
func (p *Pusher) pushIssue(ctx context.Context, project store.ProjectRow, client Forge, repo RepoRef,
	pol issueTypePolicy, allow func() bool, c *store.IssuePushCandidate, res *PushResult) (bool, error) {

	ctxFields, ctxH := CtxIssueBase(&c.Block, pol.terminalSet, pol.registryOK)
	if ctxH == c.BaseHash {
		return true, nil // in-sync (updated_at bumped, content unchanged) — 0 wire
	}
	ctxProj, _ := parseIssueProjection(ctxFields)
	truncated := metaBool(c.Block.Metadata, "truncated")

	if c.ForgeID == 0 {
		return p.createIssue(ctx, project, client, repo, allow, c, ctxProj, ctxH, ctxFields, res)
	}

	base, baseOK := parseIssueProjection(c.BaseFields)
	patch, conflict := diffIssue(base, baseOK, ctxProj, truncated)
	if conflict {
		if err := store.FlagPushConflict(ctx, p.pool, c.Block.ID, p.clock()); err != nil {
			return false, err
		}
		res.Conflicts++
		return true, nil // 0 wire writes
	}
	if patch.empty() {
		// Only a never-pushed field (milestone) diverged: advance base, 0 wire.
		if err := store.AdvancePushBase(ctx, p.pool, c.Block.ID, ctxH, ctxFields); err != nil {
			return false, err
		}
		return true, nil
	}
	if !allow() {
		res.Throttled = true
		return false, nil
	}
	if err := client.UpdateIssue(ctx, repo, c.ForgeID, patch); err != nil {
		return false, err
	}
	if err := store.AdvancePushBase(ctx, p.pool, c.Block.ID, ctxH, ctxFields); err != nil {
		return false, err
	}
	res.Pushed++
	return true, nil
}

// createIssue POSTs a new forge issue from a local draft, then finalises the
// mapping/rename/cascade. A ctx state of "closed" is reconciled with a follow-up
// PATCH so forge == ctx before the base is written (both content-POSTs throttled).
func (p *Pusher) createIssue(ctx context.Context, project store.ProjectRow, client Forge, repo RepoRef,
	allow func() bool, c *store.IssuePushCandidate, ctxProj issueProjection, ctxH string, ctxFields []byte, res *PushResult) (bool, error) {

	if !allow() {
		res.Throttled = true
		return false, nil
	}
	number, err := client.CreateIssue(ctx, repo, IssueCreate{
		Title: ctxProj.Title, Body: ctxProj.Body, Labels: ctxProj.Labels, Assignees: ctxProj.Assignees,
	})
	if err != nil {
		return false, err
	}
	// New issues are created open; reconcile a closed ctx state so forge == ctx.
	if ctxProj.State == "closed" {
		if !allow() {
			// Rare: created open but bucket dry before the close. Finalise at the
			// created (open) state — the next run's field-diff pushes the close.
			openFields, openH := issueBaseWithState(ctxProj, "open")
			if err := store.FinalizePushCreateIssue(ctx, p.pool, c.Block.ID, number, openH, openFields); err != nil {
				return false, err
			}
			res.Pushed++
			res.Throttled = true
			return false, nil
		}
		st := "closed"
		if err := client.UpdateIssue(ctx, repo, number, IssuePatch{State: &st}); err != nil {
			return false, err
		}
	}
	if err := store.FinalizePushCreateIssue(ctx, p.pool, c.Block.ID, number, ctxH, ctxFields); err != nil {
		return false, err
	}
	res.Pushed++
	return true, nil
}

// pushComment pushes one candidate comment (create or body-update).
func (p *Pusher) pushComment(ctx context.Context, client Forge, repo RepoRef,
	allow func() bool, c *store.CommentPushCandidate, res *PushResult) (bool, error) {

	block := &store.Block{Content: c.Content}
	ctxFields, ctxH := CtxCommentBase(block)
	if ctxH == c.BaseHash {
		return true, nil // in-sync — 0 wire
	}
	body, truncated := store.CapForgeBody(c.Content)

	if c.ForgeID == 0 {
		if c.ParentForgeID == 0 {
			return true, nil // parent issue not pushed yet — this comment waits
		}
		if !allow() {
			res.Throttled = true
			return false, nil
		}
		id, err := client.CreateComment(ctx, repo, c.ParentForgeID, body)
		if err != nil {
			return false, err
		}
		if err := store.FinalizePushCreateComment(ctx, p.pool, c.BlockID, id, ctxH, ctxFields); err != nil {
			return false, err
		}
		res.Pushed++
		return true, nil
	}

	// forge_id>0: the comment projection is {body}, so ctxH != base ⇒ the body
	// diverged. A truncated body can not be safely pushed ⇒ conflict (data-loss
	// guard, mirror of the issue rule).
	if truncated {
		if err := store.FlagPushConflict(ctx, p.pool, c.BlockID, p.clock()); err != nil {
			return false, err
		}
		res.Conflicts++
		return true, nil
	}
	if !allow() {
		res.Throttled = true
		return false, nil
	}
	if err := client.UpdateComment(ctx, repo, c.ForgeID, body); err != nil {
		return false, err
	}
	if err := store.AdvancePushBase(ctx, p.pool, c.BlockID, ctxH, ctxFields); err != nil {
		return false, err
	}
	res.Pushed++
	return true, nil
}

// diffIssue builds the field-diff PATCH (§4.5.2). base is the last-synced
// projection (baseOK=false on a legacy row with no snapshot). truncated HARD-
// excludes the body: a truncated body that diverged ⇒ conflict (not a write); a
// truncated body unchanged ⇒ simply never in the PATCH (a status flip still
// pushes). labels/assignees are already sorted (issueProjectionJSON), so the
// slice compare is order-stable.
func diffIssue(base issueProjection, baseOK bool, ctx issueProjection, truncated bool) (IssuePatch, bool) {
	if !baseOK {
		// No snapshot to diff against: push every field EXCEPT a body we cannot
		// prove unchanged (truncated ⇒ leave body out; never a silent overwrite).
		p := IssuePatch{Title: &ctx.Title, State: &ctx.State}
		l := ctx.Labels
		a := ctx.Assignees
		p.Labels = &l
		p.Assignees = &a
		if !truncated {
			p.Body = &ctx.Body
		}
		return p, false
	}
	var p IssuePatch
	if ctx.Title != base.Title {
		p.Title = &ctx.Title
	}
	if ctx.State != base.State {
		p.State = &ctx.State
	}
	if !eqStrings(ctx.Labels, base.Labels) {
		l := ctx.Labels
		p.Labels = &l
	}
	if !eqStrings(ctx.Assignees, base.Assignees) {
		a := ctx.Assignees
		p.Assignees = &a
	}
	if ctx.Body != base.Body {
		if truncated {
			return IssuePatch{}, true // truncated + body edit ⇒ conflict, 0 wire
		}
		p.Body = &ctx.Body
	}
	return p, false
}

// issueBaseWithState re-projects a ctx issue projection with a forced state (used
// only for the rare create-then-throttled-before-close path).
func issueBaseWithState(p issueProjection, state string) ([]byte, string) {
	p.State = state
	return issueProjectionJSON(p)
}

func eqStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func metaBool(meta map[string]any, key string) bool {
	if meta == nil {
		return false
	}
	b, _ := meta[key].(bool)
	return b
}
