// apply.go — the I-G Pull-APPLY hook (Achse 02, Welle I-G; design/02 §4.5.2,
// §4.5.4, §4.5.7). Applier.ApplyIssues is the IssueApplyFunc the I-F sync shell
// wires (SetApplyIssues): for every fetched forge issue it runs the 3-way
// direction decision over the canonical hash projection (§3.6) and applies the
// PULL side only — push (ctx-ahead) is Welle I-H.
//
// 3-way matrix (§4.5.2), base = mapping.base_hash, ctxH = the hash CtxIssueBase
// returns for the block, forgeH = the hash ForgeIssueBase returns for the fetch:
//
//	no mapping           ⇒ pull-CREATE  (block + mapping + base := forgeH)
//	(=,=)                ⇒ noop         (a flagged mapping: flag cleared, issue #509)
//	(base=ctxH, ≠forgeH) ⇒ pull-UPDATE  (block-update + base := forgeH)
//	(≠ctxH, base=forgeH) ⇒ ctx-ahead    (NOTHING — push is I-H)
//	(≠,≠, ctxH=forgeH)   ⇒ converged     (base := ctxH, NO block write)
//	(≠,≠,≠)              ⇒ CONFLICT      (conflict flag, 0 writes both ways)
//
// The decision is made on a page read without locks and, for every cell that
// writes, made again on the block and the mapping row its transaction locks
// (applyLocked, issues #452 and #491).
//
// Direction is CONTENT-hash only — never a timestamp (W16). Each issue applies in
// its OWN transaction (block + mapping + references atomic), so a mid-page failure
// leaves the earlier issues committed and the fetch cursor UN-advanced (the sync
// shell commits it only after the whole page applies), and the resume re-applies
// the committed ones as idempotent no-ops.
package forge

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/pgxdb"
	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	entityKindIssue      = "issue"
	entityKindComment    = "comment"
	structLinkReferences = "references"
	originForgeSync      = "forge-sync"
)

// SnapshotFunc resolves the type registry for a scope (bound to
// blocktype.Registry.SnapshotForTenant in server.go). A nil return / an
// unresolvable issue workflow drops the apply to the metadata-only fallback
// (§4.5.4): forge_state goes to metadata, workflow_status stays NULL.
type SnapshotFunc func(ctx context.Context, scope string) *blocktype.Set

// Applier owns the Pull-APPLY. It holds the pool (for the batch mapping lookup +
// per-issue transactions) and the registry snapshot function (for the workflow
// status mapping + the structural-link-class gate).
type Applier struct {
	pool     *pgxpool.Pool
	snapshot SnapshotFunc
	clock    func() time.Time
}

// NewApplier builds the Applier. snapshot may resolve to a Set without the issue
// workflow — the apply then runs the §4.5.4 metadata-only fallback.
func NewApplier(pool *pgxpool.Pool, snapshot SnapshotFunc) *Applier {
	return &Applier{pool: pool, snapshot: snapshot, clock: time.Now}
}

// issueTypePolicy is the resolved slice of the issue type config the apply needs.
type issueTypePolicy struct {
	registryOK  bool
	states      []string // the workflow's configured statuses
	terminalSet []string
	forgeMap    map[string]string // forge state → ctx workflow status
	refAllowed  bool              // 'references' ∈ structural_link_classes
}

// resolveIssuePolicy extracts the issue workflow + link-class policy from a Set.
// A missing type / empty workflow ⇒ the metadata-only fallback (registryOK=false,
// workflow_status NULL); references still parse (the v1 seed class is
// deterministic and request-free — blocking it in the registry-less mode would
// silently drop fact edges, §4.5.7).
func resolveIssuePolicy(set *blocktype.Set) issueTypePolicy {
	if set == nil {
		return issueTypePolicy{refAllowed: true}
	}
	pol, ok := set.Resolve(store.IssueTypeName)
	if !ok || len(pol.Workflow.States) == 0 {
		return issueTypePolicy{refAllowed: true}
	}
	refAllowed := false
	for _, c := range pol.StructuralLinkClasses {
		if c == structLinkReferences {
			refAllowed = true
			break
		}
	}
	return issueTypePolicy{
		registryOK:  true,
		states:      pol.Workflow.States,
		terminalSet: pol.Workflow.Terminal,
		forgeMap:    pol.Workflow.ForgeStateMap,
		refAllowed:  refAllowed,
	}
}

// workflowStatusFor maps a forge state to the ctx workflow status via the type's
// forge_state_map (§4.5.4). "" ⇒ NULL: no registry, or an unmapped forge state —
// the metadata-only fallback, never a guessed status.
func (p issueTypePolicy) workflowStatusFor(forgeState string) string {
	if !p.registryOK || p.forgeMap == nil {
		return ""
	}
	return p.forgeMap[forgeState]
}

// keepsStatus answers whether a pull update leaves the block's workflow status
// as it stands (issue #486). The status is finer than the forge state —
// several statuses map to one forge state (backlog and in-progress are both
// open, done and wontfix both closed) — and the projection the 3-way decision
// compares carries the forge state only, so a local move between two statuses
// of one forge state does not show in it. The pull therefore writes the status
// the remote state maps to only where that is news: the remote state differs
// from the state the block projects to (open ↔ closed), or the block carries
// no status of the resolved workflow: none yet, or one a change of the type's
// workflow dropped — no local verb can move the block out of that
// (ValidateTransition refuses a source that is not a configured status) and
// the board does not list it. A dropped status, terminal or not, projects
// the state of the base (ctxIssueBase, issue #491), so such a block reaches
// this write on a remote edit and gets the status the remote state maps to.
// With no workflow resolvable the
// pull writes the status as before #486 (NULL, the metadata-only fallback):
// there is no set to tell a kept status from a stale one, and a kept status
// is read again once a workflow resolves — an unpushed local close would then
// project to "closed" against an open base and forge with nothing to push it.
// cur is the block the pull's transaction locked, so a status moved after the
// unlocked read is the one decided on.
func (p issueTypePolicy) keepsStatus(cur *store.Block, remoteState string) bool {
	if !p.registryOK || !slices.Contains(p.states, cur.WorkflowStatus) {
		return false
	}
	return ctxForgeState(cur, p.terminalSet, true) == remoteState
}

// ctxIssueBase is the block's projection as the 3-way decision and the push
// compare it with baseFields, the mapping's base snapshot: CtxIssueBase under
// the scope's workflow — except for a status the resolved workflow does not
// carry (a change of the type's workflow dropped it). Such a status says
// nothing about the forge state any more: as a member of the old terminal
// set it is no member of the new one and would project "open" against a
// closed base, and the next push of any local edit would reopen the forge
// issue (issue #491). It projects the state of the base instead — the last
// version both sides held — so it never moves the state by itself;
// reopening an issue the forge holds closed takes a local status change
// against that base, to a status of the workflow that is not terminal. The
// same holds the other way: an unpushed local close into a terminal status
// the workflow then drops projects the open base and is not pushed
// (TestDroppedStatusUnpushedLocalCloseIsNotPushed). A
// base without a snapshot (a legacy row, or a base the push recorded as
// unknown) yields to metadata.forge_state, the state the last base write
// left on the block, and a block without either keeps the terminal-set rule.
func (p issueTypePolicy) ctxIssueBase(b *store.Block, baseFields json.RawMessage) ([]byte, string) {
	if !p.registryOK || b.WorkflowStatus == "" || slices.Contains(p.states, b.WorkflowStatus) {
		return CtxIssueBase(b, p.terminalSet, p.registryOK)
	}
	if base, ok := parseIssueProjection(baseFields); ok && base.State != "" {
		return ctxIssueBaseWithState(b, base.State)
	}
	if fs := metaString(b.Metadata, "forge_state"); fs != "" {
		return ctxIssueBaseWithState(b, fs)
	}
	return CtxIssueBase(b, p.terminalSet, p.registryOK)
}

// ApplyIssues is the IssueApplyFunc: one fetched page → the 3-way apply. The
// mapping lookup is ONE batch query for the whole page (no N+1, §6). Every issue
// applies in its own Tx; the first error aborts the page (cursor stays put).
func (a *Applier) ApplyIssues(ctx context.Context, project store.ProjectRow, issues []IssueRemote) (ApplyResult, error) {
	if len(issues) == 0 {
		return ApplyResult{}, nil
	}
	pol := resolveIssuePolicy(a.snapshot(ctx, project.Scope))
	writable := []string{project.Scope}

	ids := make([]int64, 0, len(issues))
	for _, iss := range issues {
		if iss.Number > 0 {
			ids = append(ids, iss.Number)
		}
	}
	maps, err := store.GetSyncMapsByForge(ctx, a.pool, project.ID, entityKindIssue, ids)
	if err != nil {
		return ApplyResult{}, err
	}

	var res ApplyResult
	for _, iss := range issues {
		if iss.Number <= 0 {
			continue // no forge identity — defensive (the client never yields this)
		}
		m, exists := maps[iss.Number]
		r, err := a.applyIssue(ctx, project, pol, writable, iss, m, exists)
		if err != nil {
			return res, err // mid-page failure ⇒ page aborts, cursor un-advanced
		}
		res.Applied += r.Applied
		res.Conflicts += r.Conflicts
		res.Skipped += r.Skipped
		res.Relist = relistAt(res.Relist, r.Relist)
	}
	return res, nil
}

func (a *Applier) applyIssue(ctx context.Context, project store.ProjectRow, pol issueTypePolicy,
	writable []string, iss IssueRemote, m store.SyncMap, exists bool) (ApplyResult, error) {

	forgeFields, forgeH, cappedBody, truncated := ForgeIssueBase(iss)
	fUpdated := iss.UpdatedAt
	content := store.ForgeIssueContent{
		Number: iss.Number, Title: iss.Title, Body: cappedBody, Truncated: truncated,
		ForgeState: iss.State, Labels: iss.Labels, Assignees: iss.Assignees,
		Milestone: iss.Milestone, WorkflowStatus: pol.workflowStatusFor(iss.State),
	}

	if !exists {
		return a.pullCreateIssue(ctx, project, pol, writable, iss, content, forgeH, forgeFields, &fUpdated)
	}

	block, err := store.GetIssue(ctx, a.pool, m.BlockID, writable, nil)
	if errors.Is(err, store.ErrIssueNotFound) {
		return ApplyResult{}, nil // block archived/vanished — skip (no re-materialise)
	}
	if err != nil {
		return ApplyResult{}, err
	}
	projectBlock := func(b *store.Block, baseFields json.RawMessage) string {
		_, h := pol.ctxIssueBase(b, baseFields)
		return h
	}
	return a.applyLocked(ctx, writable, m, decide(m.BaseHash, projectBlock(block, m.BaseFields), forgeH), pulled{
		forgeH: forgeH, forgeFields: forgeFields, fUpdated: &fUpdated, project: projectBlock,
		targets: func(ctx context.Context) ([]string, error) {
			return a.referenceTargets(ctx, project, pol, iss.Number, iss.Body)
		},
		// The workflow status is written only where the forge state changed
		// or the block has none of the resolved workflow (keepsStatus, issue
		// #486), decided on the block the transaction locked.
		write: func(tx pgx.Tx, cur *store.Block, targets []string) (*store.Block, error) {
			content.KeepWorkflowStatus = pol.keepsStatus(cur, iss.State)
			b, err := store.PullUpdateIssueBlock(ctx, tx, m.BlockID, content)
			if err != nil {
				return nil, err
			}
			return b, applyReferences(ctx, tx, writable, m.BlockID, targets)
		},
	})
}

// fetchOrder places this pull's fetch against the observation of the forge
// entity the locked mapping row records: its forge_updated_at, the forge's
// own updated_at of the version a pull last applied or converged on, or of
// the write a push last recorded (the PATCH's answer, never older than the
// row's, issue #509). It orders two observations of the forge on the
// forge's clock and is no direction input (W16: ctx-ahead or forge-ahead
// stays a matter of content hashes). Issue #491.
type fetchOrder int

const (
	fetchNewer     fetchOrder = iota // later than the recorded observation, or none to compare with
	fetchSameStamp                   // the same updated_at: the forge's clock cannot order the two
	fetchOlder                       // earlier than the recorded observation
)

// orderFetch compares at the database's precision (timestamptz keeps
// microseconds). GitHub's updated_at has whole seconds, so two versions
// written within one second carry the same stamp.
func orderFetch(locked store.SyncMap, fetched *time.Time) fetchOrder {
	if locked.ForgeUpdatedAt == nil || fetched == nil {
		return fetchNewer
	}
	f, r := fetched.Truncate(time.Microsecond), locked.ForgeUpdatedAt.Truncate(time.Microsecond)
	switch {
	case f.Before(r):
		return fetchOlder
	case f.Equal(r):
		return fetchSameStamp
	}
	return fetchNewer
}

// olderOrUnordered decides a fetch the locked mapping row's observation does
// not show to be newer (issue #491). It answers handled=false when the matrix
// decides on the locked rows, and otherwise the conflict count of what it did.
//
//   - Older than the recorded observation: nothing is written, whether or not
//     the base moved since the read. Another pull applied a newer version (it
//     may even have committed between this pull's fetch and its read of the
//     mapping), or a push recorded its write, which the fetch predates
//     (issue #509); writing the older one would set the block back, and the
//     cursor of the instance that pulled the newer version may answer 304
//     from then on, so no later pull would correct it. The block holds a
//     version at least as new as the fetch, and a later forge change carries
//     a stamp at or after the recorded one, which the since cursor of every
//     instance lists again (the cursor never passes the newest stamp of a
//     page it applied).
//   - The same stamp, after another instance moved the base since the read:
//     the other pull's version and this one cannot be ordered. When they are
//     the same version there is nothing to do; otherwise the mapping is
//     flagged and nothing is written either way — the conservative answer
//     the base write before issue #491 gave, because writing the fetch loses
//     the other version when it was the newer one (and, with a 304 after it,
//     for good), and keeping the row loses this one in the other order. A
//     push that recorded its write since the read moved the base as well:
//     a remote edit in the second of its PATCH carries the stamp it recorded
//     and is flagged here, and the next pull pulls it.
//   - The same stamp without a moved base: the next version of one second,
//     pulled run after run, and the matrix decides — also when another
//     instance pulled the other version of that second between this pull's
//     fetch and its read of the mapping, which reads the same (a known
//     limit, pinned by TestPullOfASameSecondFetch).
func olderOrUnordered(locked, read store.SyncMap, forgeH string, fetched *time.Time) (handled, conflict, stale bool) {
	switch orderFetch(locked, fetched) {
	case fetchOlder:
		return true, false, forgeH != locked.BaseHash
	case fetchSameStamp:
		if locked.BaseHash == read.BaseHash {
			return false, false, false
		}
		return true, forgeH != locked.BaseHash && !locked.Conflict, false
	}
	return false, false, false
}

// staleSeenKey carries, in a sync run's context, the entities whose skipped
// older fetch the run reported already (withStaleSeen).
type staleSeenKey struct{}

type staleSeen struct {
	mu     sync.Mutex
	blocks map[string]bool
}

// withStaleSeen gives a sync run the set behind firstStale, so a skipped
// older fetch is reported once per entity and run however often the entity
// is listed (a write between two pages lists it again).
func withStaleSeen(ctx context.Context) context.Context {
	return context.WithValue(ctx, staleSeenKey{}, &staleSeen{blocks: map[string]bool{}})
}

// firstStale answers whether this is the first skipped older fetch of
// blockID in the run; without a run set (a direct ApplyIssues call) every
// one is the first.
func firstStale(ctx context.Context, blockID string) bool {
	s, _ := ctx.Value(staleSeenKey{}).(*staleSeen)
	if s == nil {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.blocks[blockID] {
		return false
	}
	s.blocks[blockID] = true
	return true
}

// reportStale makes a skipped older fetch whose content differs from the
// locked row's base visible (issue #491): the forge carries something the
// block does not, and this pull does not write it. Once, it is the ordinary
// trace of a race — another instance pulled a newer version or pushed a
// write the fetch predates, and the next pull lists the forge's version.
// Run after run for the same block, the forge's
// updated_at went back behind the stamp the mapping records (a clock set
// back, a restore of the forge from a backup, a manual change): every pull
// skips the entity, and a later local edit is pushed over the forge's
// version. docs/architecture.md names the way out (the mapping's
// forge_updated_at set to NULL).
func reportStale(ctx context.Context, res *ApplyResult, blockID string, fetched, recorded *time.Time) {
	if !firstStale(ctx, blockID) {
		return
	}
	res.Skipped = 1
	slog.Warn("forge: a pull skipped a fetch older than the forge version its mapping records, and the forge's content differs from the recorded base — not written; once is a race with another instance's pull or push, every run means the forge's updated_at went back (clock, restore)",
		"block", blockID, "fetched_updated_at", fetched, "recorded_updated_at", recorded)
}

// decision is one cell of the 3-way matrix (see the package doc).
type decision int

const (
	decNoop     decision = iota // (=,=)
	decPull                     // forge-ahead: pull-UPDATE
	decCtxAhead                 // the push's (I-H)
	decConverge                 // block and forge agree, the base is behind
	decConflict                 // both moved apart
)

// decide is the 3-way matrix: base = the mapping's base_hash, ctxH = the
// block's projection, forgeH = the fetched entity's.
func decide(base, ctxH, forgeH string) decision {
	switch {
	case base == ctxH && base == forgeH:
		return decNoop
	case base == ctxH:
		return decPull
	case base == forgeH:
		return decCtxAhead
	case ctxH == forgeH:
		return decConverge
	default:
		return decConflict
	}
}

// pulled is one fetched entity the locked apply decides about: the forge
// side's projection, the block's projection against a base snapshot
// (issueTypePolicy.ctxIssueBase or CtxCommentBase), the blocks the
// pull-UPDATE links the block to (targets, nil for none — resolved before
// the transaction, which locks those below the block's id before the block,
// issue #503) and the pull-UPDATE's block write.
type pulled struct {
	forgeH      string
	forgeFields json.RawMessage
	fUpdated    *time.Time
	project     func(b *store.Block, baseFields json.RawMessage) string
	targets     func(ctx context.Context) ([]string, error)
	write       func(tx pgx.Tx, cur *store.Block, targets []string) (*store.Block, error)
}

// applyLocked carries out the matrix decision the read path made on a page
// read without locks (read), in one transaction. A decision that writes
// nothing ends on the read (endsOnRead): noop on an unflagged mapping,
// ctx-ahead (the push's), and a conflict the mapping already carries
// (idempotent, 0 new conflicts). Every other one is
// decided again on the rows the transaction locks — the targets of its
// references whose id lies below the block's, in ascending order, then the
// block (LockPulledBlock, issue #503), then its mapping row (LockSyncMap),
// the order of every writer of both — because a local write can move the block and another ctxd
// instance the mapping between the read and the write (issues #452, #491):
//
//   - The mapping still holds the base the read decided on. The read's
//     convergence stands whatever the block holds now: the read saw block
//     and forge at one version, so it is the base both sides last agreed
//     on; a local write since is ahead of it and goes out with the next push
//     (synced_at -infinity, UpdateSyncMapBase) — unless another instance
//     flagged the mapping since the read: its pull saw the forge move past
//     what this pull fetched, and the push must not send the local write
//     over that remote edit, so the matrix decides on the locked rows. Any
//     other decision is taken again on the locked block — forge-ahead stays
//     forge-ahead only while the block still projects to the base; a block a
//     local write moved is a convergence when it now equals the forge, a
//     conflict otherwise (the read path's own matrix, issue #452).
//   - The mapping's base moved (another instance pushed and advanced it, or
//     pulled): the read's decision was made against a base that is gone, and
//     the matrix runs on the locked block and the locked row alone — unless
//     the row's observation of the forge does not show this fetch to be
//     newer (olderOrUnordered): an older fetch writes nothing, one with the
//     same stamp as a version another instance pulled meanwhile is flagged
//     unless it is that version. Where
//     another instance pushed a local edit, the block equals the new base and
//     the forge is ahead of it — a pull, not the conflict the read's base
//     would make of it; what the pull fetched before that push landed comes
//     back with the next pull, the forge holding the pushed edit.
//
// A fetch older than the row's observation writes nothing in either case: a
// pull of another instance may have applied the newer version between this
// pull's fetch and its read.
//
// A flag goes once block and forge agree on the locked rows (issue #509):
// the convergence clears it as it always did, and so does the noop cell —
// block, base and forge at one version — which before wrote nothing, so a
// mapping flagged while both sides moved stayed flagged after they came
// level, out of the push's listing for good. It records that version as
// the base (synced_at the block's updated_at) like a convergence.
//
// A flag this pull sets anew, and a version it cannot order, report the
// fetch's stamp (ApplyResult.Relist): the leg saves its cursor no further
// than it and no ETag, so the next run lists the entity again and decides
// on it (issue #509) — the flag may stand for a remote edit no later run
// would list otherwise. A conflict the mapping carries already reports
// nothing.
//
// A block that is gone, archived or out of scope, or that lost its mapping,
// is skipped, as the read does.
func (a *Applier) applyLocked(ctx context.Context, writable []string, m store.SyncMap, read decision, p pulled) (ApplyResult, error) {
	if res, done := endsOnRead(ctx, read, m, p); done {
		return res, nil
	}
	var targets []string
	if p.targets != nil {
		var err error
		if targets, err = p.targets(ctx); err != nil {
			return ApplyResult{}, err
		}
	}
	var res ApplyResult
	if err := pgxdb.Write(ctx, a.pool, pgxdb.Stages{}, func(tx pgx.Tx) error {
		res = ApplyResult{}
		cur, err := store.LockPulledBlock(ctx, tx, m.BlockID, writable, targets)
		if errors.Is(err, store.ErrIssueNotFound) {
			return nil // archived/vanished since the read — skip, as the read does
		}
		if err != nil {
			return err
		}
		locked, ok, err := store.LockSyncMap(ctx, tx, m.BlockID)
		if err != nil || !ok {
			return err
		}
		if handled, conflict, stale := olderOrUnordered(locked, m, p.forgeH, p.fUpdated); handled {
			if stale {
				reportStale(ctx, &res, m.BlockID, p.fUpdated, locked.ForgeUpdatedAt)
			} else if p.forgeH != locked.BaseHash {
				res.Relist = stampOf(p.fUpdated) // the same stamp as another version: listed again next run (issue #509)
			}
			if !conflict {
				return nil
			}
			res.Conflicts = 1
			return store.FlagSyncMapConflict(ctx, tx, m.BlockID, a.clock(), p.seen())
		}
		ctxH := p.project(cur, locked.BaseFields)
		d := decide(locked.BaseHash, ctxH, p.forgeH)
		if read == decConverge && locked.BaseHash == m.BaseHash && locked.Conflict == m.Conflict {
			d = decConverge
		}
		switch d {
		case decPull:
			b, err := p.write(tx, cur, targets)
			if err != nil {
				return err
			}
			res.Applied = 1
			return store.UpdateSyncMapBase(ctx, tx, m.BlockID, p.forgeH, p.forgeFields, p.fUpdated, &b.UpdatedAt)
		case decNoop, decConverge:
			if d == decNoop && !locked.Conflict && !locked.PullGate {
				return nil // in sync and unflagged: nothing to record
			}
			var heldAt *time.Time
			if ctxH == p.forgeH {
				heldAt = &cur.UpdatedAt
			}
			if err := store.UpdateSyncMapBase(ctx, tx, m.BlockID, p.forgeH, p.forgeFields, p.fUpdated, heldAt); err != nil {
				return err
			}
			// The forge state the pull read, as a pull-update writes it
			// (issue #491); a comment's snapshot carries none.
			return store.StampForgeState(ctx, tx, m.BlockID, p.forgeFields)
		}
		return a.flagOrObserve(ctx, tx, m.BlockID, locked, d, p, &res)
	}); err != nil {
		return ApplyResult{}, err
	}
	return res, nil
}

// flagOrObserve carries out the two cells of the locked decision that write
// no block: a conflict and ctx-ahead (applyLocked). A conflict on an
// unflagged row flags it anew with the fetched version and reports the
// fetch's stamp (Relist). A conflict on a flagged row whose fetch is newer
// than the row's observation records it (issue #509), so an older fetch
// another instance decides on later is skipped instead of clearing the flag
// over this one. ctx-ahead with such a fetch (a remote revert to the base,
// or a change outside the projection) records its stamp and reports it
// (issue #509): a push whose PATCH is on the wire records the sent version
// as the base after this, and a revert to the old base that landed after
// the PATCH would otherwise never be listed again.
func (a *Applier) flagOrObserve(ctx context.Context, tx pgx.Tx, blockID string, locked store.SyncMap, d decision, p pulled, res *ApplyResult) error {
	newer := orderFetch(locked, p.fUpdated) == fetchNewer
	switch {
	case d == decConflict && !locked.Conflict:
		res.Conflicts = 1
		res.Relist = stampOf(p.fUpdated) // flagged anew: listed again next run (issue #509)
		return store.FlagSyncMapConflict(ctx, tx, blockID, a.clock(), p.seen())
	case d == decConflict && newer:
		return store.RecordForgeObservation(ctx, tx, blockID, p.fUpdated, p.seen())
	case d == decCtxAhead && (newer || locked.PullGate):
		if locked.PullGate {
			// The forge holds the base a remote resolution wrote: the
			// block's edit since is the only change, and the push may
			// send it (issue #509).
			if _, err := store.OpenPullGate(ctx, tx, blockID); err != nil {
				return err
			}
		}
		if !newer {
			return nil
		}
		res.Relist = stampOf(p.fUpdated)
		var seen *store.ForgeSeen
		if locked.Conflict {
			seen = p.seen()
		}
		return store.RecordForgeObservation(ctx, tx, blockID, p.fUpdated, seen)
	}
	return nil
}

// seen is the fetched version as a flag keeps it (store.ForgeSeen, issue
// #509): the forge side the resolve surface lists and a resolution that
// keeps the local version diffs against.
func (p pulled) seen() *store.ForgeSeen {
	return &store.ForgeSeen{Fields: p.forgeFields, Hash: p.forgeH, At: p.fUpdated}
}

// stampOf is a fetch's forge stamp, zero for none.
func stampOf(fetched *time.Time) time.Time {
	if fetched == nil {
		return time.Time{}
	}
	return *fetched
}

// endsOnRead answers whether the read's decision is carried out without a
// transaction (done), and what it counts: ctx-ahead (the push's), a noop
// on an unflagged mapping, and a conflict the mapping carries already
// (idempotent, 0 new conflicts). A noop on a flagged mapping goes on to the
// locked rows, which clear the flag (issue #509). So do a ctx-ahead and a
// conflict on a flagged mapping whose fetch is newer than the row's
// observation (issue #509): the locked rows record it — the flagged row
// its stamp and the forge version, so a convergence another instance read
// before that version can no longer clear the flag over it; the ctx-ahead
// row its stamp, and the leg lists the entity again (a remote revert to
// the base inside a push's window). Before, both ended here and held no
// cursor, and each of the two orders lost a remote edit without a flag.
//
// A fetch older than the observation the flagged row records is reported
// (reportStale: pull_skipped and the WARN) before the conflict ends here —
// before, it returned uncounted (issue #509). A conflict decision means the
// fetch differs from the base, so such a skip always carries other content.
func endsOnRead(ctx context.Context, read decision, m store.SyncMap, p pulled) (ApplyResult, bool) {
	var res ApplyResult
	if m.PullGate && orderFetch(m, p.fUpdated) != fetchOlder {
		// A remote resolution keeps the push away until a pull decides
		// over the entity (issue #509): this fetch does, on the locked
		// rows — a noop or ctx-ahead there ends the gate.
		return res, false
	}
	switch {
	case read == decNoop && !m.Conflict:
		return res, true
	case read == decCtxAhead:
		return res, orderFetch(m, p.fUpdated) != fetchNewer
	case read == decConflict && m.Conflict:
		switch orderFetch(m, p.fUpdated) {
		case fetchOlder:
			reportStale(ctx, &res, m.BlockID, p.fUpdated, m.ForgeUpdatedAt)
			return res, true
		case fetchSameStamp:
			return res, true
		}
	}
	return res, false
}

func (a *Applier) pullCreateIssue(ctx context.Context, project store.ProjectRow, pol issueTypePolicy,
	writable []string, iss IssueRemote, content store.ForgeIssueContent, forgeH string, forgeFields json.RawMessage, fUpdated *time.Time) (ApplyResult, error) {

	targets, err := a.referenceTargets(ctx, project, pol, iss.Number, iss.Body)
	if err != nil {
		return ApplyResult{}, err
	}
	if err := pgxdb.Write(ctx, a.pool, pgxdb.Stages{}, func(tx pgx.Tx) error {
		// The new block is no other transaction's to lock or to wait for
		// (its id is unknown until the commit), so the reference write may
		// take the targets after it (issue #503).
		b, err := store.PullCreateIssueBlock(ctx, tx, project.Scope, content)
		if err != nil {
			return err
		}
		if err := store.InsertSyncMap(ctx, tx, store.SyncMap{
			ProjectID: project.ID, EntityKind: entityKindIssue, ForgeID: iss.Number,
			BlockID: b.ID, BaseHash: forgeH, BaseFields: forgeFields, ForgeUpdatedAt: fUpdated,
		}); err != nil {
			return err
		}
		return applyReferences(ctx, tx, writable, b.ID, targets)
	}); err != nil {
		return ApplyResult{}, err
	}
	return ApplyResult{Applied: 1}, nil
}

// ApplyComments is the CommentApplyFunc: one fetched page of comments → the
// 3-way apply over the {body} projection (§3.6). A comment attaches to its parent
// issue via the issue mapping (project, 'issue', issue_number); a comment whose
// parent is NOT yet pulled is SKIPPED (the next run, after the issue exists, adds
// it). Two batch lookups per page (comment mappings + parent issue mappings) — no
// N+1. Each comment applies in its own Tx.
func (a *Applier) ApplyComments(ctx context.Context, project store.ProjectRow, comments []CommentRemote) (ApplyResult, error) {
	if len(comments) == 0 {
		return ApplyResult{}, nil
	}
	writable := []string{project.Scope}
	cIDs := make([]int64, 0, len(comments))
	issueNums := make([]int64, 0, len(comments))
	for _, c := range comments {
		if c.ID > 0 {
			cIDs = append(cIDs, c.ID)
		}
		if c.IssueNumber > 0 {
			issueNums = append(issueNums, c.IssueNumber)
		}
	}
	cmaps, err := store.GetSyncMapsByForge(ctx, a.pool, project.ID, entityKindComment, cIDs)
	if err != nil {
		return ApplyResult{}, err
	}
	imaps, err := store.GetSyncMapsByForge(ctx, a.pool, project.ID, entityKindIssue, issueNums)
	if err != nil {
		return ApplyResult{}, err
	}

	var res ApplyResult
	for _, c := range comments {
		if c.ID <= 0 {
			continue
		}
		parent, hasParent := imaps[c.IssueNumber]
		if !hasParent {
			// Parent issue not pulled (yet): the comment leg decides whether
			// a later run lists it again (holdOrphans, issue #509).
			res.orphans = append(res.orphans, orphanComment{id: c.ID, parent: c.IssueNumber, at: c.UpdatedAt})
			continue
		}
		m, exists := cmaps[c.ID]
		r, err := a.applyComment(ctx, project, writable, c, parent.BlockID, m, exists)
		if err != nil {
			return res, err
		}
		res.Applied += r.Applied
		res.Conflicts += r.Conflicts
		res.Skipped += r.Skipped
		res.Relist = relistAt(res.Relist, r.Relist)
	}
	return res, nil
}

func (a *Applier) applyComment(ctx context.Context, project store.ProjectRow, writable []string,
	c CommentRemote, parentBlockID string, m store.SyncMap, exists bool) (ApplyResult, error) {

	forgeFields, forgeH, cappedBody, _ := ForgeCommentBase(c.Body)
	fUpdated := c.UpdatedAt

	if !exists {
		return a.pullCreateComment(ctx, project, c, parentBlockID, cappedBody, forgeH, forgeFields, &fUpdated, writable)
	}
	block, err := store.GetIssue(ctx, a.pool, m.BlockID, writable, nil)
	if errors.Is(err, store.ErrIssueNotFound) {
		return ApplyResult{}, nil
	}
	if err != nil {
		return ApplyResult{}, err
	}
	projectBlock := func(b *store.Block, _ json.RawMessage) string {
		_, h := CtxCommentBase(b)
		return h
	}
	return a.applyLocked(ctx, writable, m, decide(m.BaseHash, projectBlock(block, m.BaseFields), forgeH), pulled{
		forgeH: forgeH, forgeFields: forgeFields, fUpdated: &fUpdated, project: projectBlock,
		write: func(tx pgx.Tx, _ *store.Block, _ []string) (*store.Block, error) {
			return store.PullUpdateCommentBlock(ctx, tx, m.BlockID, cappedBody)
		},
	})
}

func (a *Applier) pullCreateComment(ctx context.Context, project store.ProjectRow, c CommentRemote,
	parentBlockID, cappedBody, forgeH string, forgeFields json.RawMessage, fUpdated *time.Time, writable []string) (ApplyResult, error) {

	// This is the FOURTH store.InsertCommentBlock site (T02-11 Übergabe 4) and
	// the only one outside the handler claim gates — deliberately, and it needs
	// no gate of its own: parentBlockID cannot be empty here. ApplyComments
	// reaches this function only through a mapped parent issue (`if !hasParent {
	// continue }`), and what it passes is context_project_sync_map.block_id, a
	// column declared `UUID NOT NULL REFERENCES context_blocks(id)`
	// (113_baseline.sql) — a mapping row that exists names a block that exists.
	// The claim gates decide what a CLIENT may assert about a block it writes;
	// this arm asserts nothing on a client's behalf, and its type is the SQL
	// literal inside InsertCommentBlock, not a name out of a request. Running
	// parentRequiredReject here would be a branch whose condition is constant.
	// The floor under all four sites is the primitive itself: an empty parent is
	// ErrCommentParentRequired before any write. Pinned by
	// TestApply_CommentParentByConstruction (apply_comment_parent_integration_test.go).
	if err := pgxdb.Write(ctx, a.pool, pgxdb.Stages{}, func(tx pgx.Tx) error {
		b, err := store.InsertCommentBlock(ctx, tx, parentBlockID, store.CommentFields{
			Content:  cappedBody,
			Metadata: map[string]any{"forge_comment_id": c.ID},
		}, writable)
		if err != nil {
			return err
		}
		return store.InsertSyncMap(ctx, tx, store.SyncMap{
			ProjectID: project.ID, EntityKind: entityKindComment, ForgeID: c.ID,
			BlockID: b.ID, BaseHash: forgeH, BaseFields: forgeFields, ForgeUpdatedAt: fUpdated,
		})
	}); err != nil {
		return ApplyResult{}, err
	}
	return ApplyResult{Applied: 1}, nil
}

// issueRefRe extracts a "#<nr>" reference token with a boundary before the '#'
// (so 'word#12', an HTML entity '&#12', and '##12' do not match) and a word
// boundary after the digits (§4.5.7 body parsing).
var issueRefRe = regexp.MustCompile(`(?:^|[^0-9A-Za-z_&#])#(\d{1,18})\b`)

// parseIssueRefs returns the distinct forge issue numbers referenced in body.
func parseIssueRefs(body string) map[int64]bool {
	out := map[int64]bool{}
	for _, m := range issueRefRe.FindAllStringSubmatch(body, -1) {
		if n, err := strconv.ParseInt(m[1], 10, 64); err == nil && n > 0 {
			out[n] = true
		}
	}
	return out
}

// referenceTargets resolves the 'references' fact edges of a body (§4.5.7)
// to their target blocks: each "#<nr>" resolves via the mapping — a number
// with NO mapping yields NO edge (no phantom links; a later pull-update of
// the source adds it, idempotently via the PK — a pull of the target does
// not). Self-references are dropped and the structural-link-class gate is
// honoured. 0 forge requests (pure local parsing). The ids come back sorted
// and distinct.
//
// It runs on the pool before the pull's write transaction begins (the
// targets are committed blocks of other issues): the transaction locks
// those below the source block's id before the block and links exactly
// these (issue #503) — a target mapped meanwhile gets its edge from a later
// pull-update of the source, as an unmapped one does. Resolving them inside the transaction also
// needed a second pool connection while the first held the block.
func (a *Applier) referenceTargets(ctx context.Context, project store.ProjectRow,
	pol issueTypePolicy, sourceNum int64, body string) ([]string, error) {

	if !pol.refAllowed {
		return nil, nil
	}
	refs := parseIssueRefs(body)
	targetNums := make([]int64, 0, len(refs))
	for n := range refs {
		if n != sourceNum { // drop a self-reference (PutStructuralLink rejects self-loops)
			targetNums = append(targetNums, n)
		}
	}
	if len(targetNums) == 0 {
		return nil, nil
	}
	tmaps, err := store.GetSyncMapsByForge(ctx, a.pool, project.ID, entityKindIssue, targetNums)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(tmaps))
	for _, n := range targetNums {
		if tm, ok := tmaps[n]; ok { // unknown number ⇒ NO phantom edge (§4.5.7)
			ids = append(ids, tm.BlockID)
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids), nil
}

// applyReferences writes the 'references' edges from sourceID to targets
// (referenceTargets) in the caller's Tx, atomic with the source block.
// PutStructuralLink's validation locks each target FOR SHARE: a pull-update
// holds those below its block's id already (LockPulledBlock) and takes
// those above it here, after its block, in ascending order (targets are
// sorted); a pull-create's block is new, no other transaction's to wait
// for.
func applyReferences(ctx context.Context, tx pgx.Tx, writable []string, sourceID string, targets []string) error {
	for _, id := range targets {
		err := store.PutStructuralLink(ctx, tx, store.StructuralLink{
			SourceID: sourceID, TargetID: id,
			LinkClass: structLinkReferences, Origin: originForgeSync,
		}, writable)
		if err != nil && !errors.Is(err, store.ErrLinkScopeViolation) {
			return err // a scope violation is defensively skipped (same-scope by construction)
		}
	}
	return nil
}
