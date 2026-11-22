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
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/GottZ/ctx/internal/pgxdb"
	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pushBatchLimit bounds the candidates enumerated per run (§6, 10k+ issues/repo).
// The throttle caps the actual wire rate; this caps the DB enumeration so one run
// is bounded work. Remaining candidates drain on the next run.
const pushBatchLimit = 500

// PushResult is what the push pass reports back to the sync run.
type PushResult struct {
	Pushed    int  // entities written to the forge (create + update + comment)
	Conflicts int  // mappings flagged: a truncated body that diverged (never written), or a base another instance moved during the push
	Skipped   int  // entities left to the next run: another instance's push held them, wrote them since the listing, or a pull flagged them since (a DEBUG line names which, issue #491)
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

	ctxFields, ctxH := pol.ctxIssueBase(&c.Block, c.BaseFields)
	projected := func(b *store.Block) string {
		_, h := pol.ctxIssueBase(b, c.BaseFields)
		return h
	}
	rec := pushRecord{blockID: c.Block.ID, listed: c.BaseHash, ctxH: ctxH, ctxFields: ctxFields, projected: projected}
	if ctxH == c.BaseHash {
		// In sync (a write that left the projection alone, or an edit undone
		// since a PATCH): 0 wire, but the row was listed, so it leaves the
		// candidates through the bound advance (issue #452) — otherwise it
		// stays listed for good and, past pushBatchLimit, keeps later
		// candidates out of the listing.
		skip, err := p.section(ctx, c.Block.ID, c.BaseHash, nil, rec.advance(ctx, false))
		noteSkip(res, c.Block.ID, skip)
		rec.count(res, err)
		return true, err
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
		skip, err := p.section(ctx, c.Block.ID, c.BaseHash, nil, rec.advance(ctx, false))
		noteSkip(res, c.Block.ID, skip)
		rec.count(res, err)
		return true, err
	}
	cont, err := p.wireSection(ctx, c.Block.ID, c.BaseHash, allow, res, func() error {
		var err error
		rec.stamp, err = client.UpdateIssue(ctx, repo, c.ForgeID, patch)
		return err
	}, rec.advance(ctx, true))
	rec.count(res, err)
	return cont, err
}

// errThrottled leaves a push section without its record when the token
// bucket ran dry inside it.
var errThrottled = errors.New("forge: push throttled")

// pushUnlockGrace bounds the statement that gives a section's push lock back.
// It runs on a context detached from the caller's (a canceled run must still
// give the lock back) and is the budget of one statement on an open
// connection; a connection that does not answer in it is closed instead.
const pushUnlockGrace = 5 * time.Second

// section runs one write of the push about an entity — the wire write and
// the advance that records it, or the advance alone — under the entity's
// push lock (store.TryLockPushEntity, issue #491): a session-level advisory
// lock on one pool connection the section holds until it ends. An entity
// another instance is writing now, wrote since this push listed it or
// flagged since (its pull saw both sides move), is left to the next run:
// nothing runs, nothing is written, the pass goes on with the next
// candidate, and skip says why.
//
// wire (nil for the advance alone) runs with no transaction open on the
// section's connection, so the forge's answer time holds no snapshot and no
// relation lock in the database; record runs afterwards in one short
// transaction on that connection. The connection stays out of the pool
// across the wire write — one per running push leg, the legs are bounded by
// project.sync.max_concurrent (V68 caps it against the pool size). The lock
// is given back on every way out; a connection whose unlock is not proven
// (the lock statement failed with an unknown outcome, the unlock failed or
// found no lock) is closed, and the server releases a closed session's
// advisory locks — a pooled connection never keeps an entity locked.
func (p *Pusher) section(ctx context.Context, blockID, listedBase string, wire func() error, record func(tx pgx.Tx) error) (store.PushSkip, error) {
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return "", fmt.Errorf("forge: push section: acquire a connection: %w", err)
	}
	locked, skip, err := store.TryLockPushEntity(ctx, conn, blockID, listedBase)
	switch {
	case locked:
		defer unlockPushEntity(ctx, conn, blockID)
	case err != nil:
		closePushConn(ctx, conn)
		return "", err
	default:
		conn.Release()
		return skip, nil
	}
	if err != nil || skip != "" {
		return skip, err
	}
	if wire != nil {
		if err := wire(); err != nil {
			return "", err
		}
	}
	return "", pgxdb.Write(ctx, conn.Conn(), pgxdb.Stages{Begin: "forge: push record begin", Commit: "forge: push record commit"}, record)
}

// noteSkip counts an entity a section left to the next run, in the
// result the sync run reports (push_skipped), and names the reason at DEBUG.
func noteSkip(res *PushResult, blockID string, skip store.PushSkip) {
	if skip == "" {
		return
	}
	res.Skipped++
	slog.Debug("forge: push left an entity to the next run", "block", blockID, "reason", string(skip))
}

// unlockPushEntity gives a section's push lock back and returns conn to the
// pool, or closes conn when the release is not proven.
func unlockPushEntity(ctx context.Context, conn *pgxpool.Conn, blockID string) {
	uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pushUnlockGrace)
	defer cancel()
	released, err := store.UnlockPushEntity(uctx, conn, blockID)
	if err != nil || !released {
		slog.Warn("forge: push lock not released on its connection — closing the connection, the server releases the lock with the session",
			"block", blockID, "released", released, "error", err)
		closePushConn(ctx, conn)
		return
	}
	conn.Release()
}

// closePushConn closes conn and hands it back; the pool destroys a closed
// connection instead of pooling it.
func closePushConn(ctx context.Context, conn *pgxpool.Conn) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pushUnlockGrace)
	defer cancel()
	_ = conn.Conn().Close(cctx)
	conn.Release()
}

// wireSection is section with a content write: the throttle is asked once
// the section holds the entity (a busy entity costs no token), then send
// writes the forge and record advances the base. It answers cont=false when
// the bucket ran dry (the caller stops the pass).
func (p *Pusher) wireSection(ctx context.Context, blockID, listedBase string, allow func() bool, res *PushResult,
	send func() error, record func(tx pgx.Tx) error) (bool, error) {

	sent := false
	skip, err := p.section(ctx, blockID, listedBase, func() error {
		if !allow() {
			return errThrottled
		}
		if err := send(); err != nil {
			return err
		}
		sent = true
		return nil
	}, record)
	noteSkip(res, blockID, skip)
	switch {
	case errors.Is(err, errThrottled):
		res.Throttled = true
		return false, nil
	case err != nil:
		return false, err
	}
	if sent {
		res.Pushed++
	}
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
	number, stamp, err := client.CreateIssue(ctx, repo, IssueCreate{
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
			if err := store.FinalizePushCreateIssue(ctx, p.pool, c.Block.ID, number, openH, openFields, stamp); err != nil {
				return false, err
			}
			res.Pushed++
			res.Throttled = true
			return false, nil
		}
		st := "closed"
		closed, err := client.UpdateIssue(ctx, repo, number, IssuePatch{State: &st})
		if err != nil {
			return false, err
		}
		if closed != nil {
			stamp = closed // the version the close left is the one finalised
		}
	}
	if err := store.FinalizePushCreateIssue(ctx, p.pool, c.Block.ID, number, ctxH, ctxFields, stamp); err != nil {
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
	projected := func(b *store.Block) string {
		_, h := CtxCommentBase(b)
		return h
	}
	rec := pushRecord{blockID: c.BlockID, listed: c.BaseHash, ctxH: ctxH, ctxFields: ctxFields, projected: projected}
	if ctxH == c.BaseHash {
		// In sync — 0 wire; the row leaves the candidates as in pushIssue.
		skip, err := p.section(ctx, c.BlockID, c.BaseHash, nil, rec.advance(ctx, false))
		noteSkip(res, c.BlockID, skip)
		rec.count(res, err)
		return true, err
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
		id, created, err := client.CreateComment(ctx, repo, c.ParentForgeID, body)
		if err != nil {
			return false, err
		}
		if err := store.FinalizePushCreateComment(ctx, p.pool, c.BlockID, id, ctxH, ctxFields, created); err != nil {
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
	cont, err := p.wireSection(ctx, c.BlockID, c.BaseHash, allow, res, func() error {
		var err error
		rec.stamp, err = client.UpdateComment(ctx, repo, c.ForgeID, body)
		return err
	}, rec.advance(ctx, true))
	rec.count(res, err)
	return cont, err
}

// pushRecord is the record of one candidate's push section: the advance
// that writes the pushed (or in-sync) version as the mapping's base through
// store.AdvancePushBase. listed is the base the listing read; when another
// instance's pull moved the base since (the section keeps other pushes out,
// not pulls) to a version other than the one this push sent, the advance
// flags the mapping instead of writing either version (issue #486) and the
// pass counts the conflict; a pull that converged on the sent version
// leaves nothing to decide (issue #491). stamp is the forge's updated_at
// the PATCH answered, which the wire write sets (nil: none, and an advance
// without a wire write has none) — the mapping's forge stamp with the base
// (issue #509).
//
// moved keeps what the advance found, and count adds the conflict once the
// section's transaction committed (issue #509): counted inside it, a COMMIT
// that failed counted a conflict that was rolled back, and a failed leg's
// counts reach the run's status.
type pushRecord struct {
	blockID, listed, ctxH string
	ctxFields             []byte
	stamp                 *time.Time
	projected             store.PushProjection
	moved                 bool
}

// advance is the section's record; sent says that a PATCH carried ctxH
// (store.AdvancePushBase).
func (r *pushRecord) advance(ctx context.Context, sent bool) func(tx pgx.Tx) error {
	return func(tx pgx.Tx) error {
		var err error
		r.moved, err = store.AdvancePushBase(ctx, tx, r.blockID, r.listed, r.ctxH, r.ctxFields, r.stamp, sent, r.projected)
		return err
	}
}

// count adds the advance's conflict to res once its section returned
// without an error — the record committed.
func (r *pushRecord) count(res *PushResult, err error) {
	if err == nil && r.moved {
		res.Conflicts++
	}
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
