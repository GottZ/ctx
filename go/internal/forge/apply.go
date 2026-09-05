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
//	(=,=)                ⇒ noop
//	(base=ctxH, ≠forgeH) ⇒ pull-UPDATE  (block-update + base := forgeH)
//	(≠ctxH, base=forgeH) ⇒ ctx-ahead    (NOTHING — push is I-H)
//	(≠,≠, ctxH=forgeH)   ⇒ converged     (base := ctxH, NO block write)
//	(≠,≠,≠)              ⇒ CONFLICT      (conflict flag, 0 writes both ways)
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
	"regexp"
	"strconv"
	"time"

	"github.com/GottZ/ctx/internal/blocktype"
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
	ctxFields, ctxH := CtxIssueBase(block, pol.terminalSet, pol.registryOK)
	base := m.BaseHash

	switch {
	case base == ctxH && base == forgeH:
		return ApplyResult{}, nil // (=,=): noop
	case base == ctxH && base != forgeH:
		return a.pullUpdateIssue(ctx, project, pol, writable, iss, m.BlockID, content, forgeH, forgeFields, &fUpdated)
	case base != ctxH && base == forgeH:
		return ApplyResult{}, nil // ctx-ahead: push is I-H, I-G does nothing
	case ctxH == forgeH:
		return ApplyResult{}, a.txBaseUpdate(ctx, m.BlockID, ctxH, ctxFields, &fUpdated) // converged
	default:
		if m.Conflict {
			return ApplyResult{}, nil // already flagged — idempotent, 0 new conflicts
		}
		return a.flagConflict(ctx, m.BlockID)
	}
}

func (a *Applier) pullCreateIssue(ctx context.Context, project store.ProjectRow, pol issueTypePolicy,
	writable []string, iss IssueRemote, content store.ForgeIssueContent, forgeH string, forgeFields json.RawMessage, fUpdated *time.Time) (ApplyResult, error) {

	tx, err := a.pool.Begin(ctx)
	if err != nil {
		return ApplyResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	b, err := store.PullCreateIssueBlock(ctx, tx, project.Scope, content)
	if err != nil {
		return ApplyResult{}, err
	}
	if err := store.InsertSyncMap(ctx, tx, store.SyncMap{
		ProjectID: project.ID, EntityKind: entityKindIssue, ForgeID: iss.Number,
		BlockID: b.ID, BaseHash: forgeH, BaseFields: forgeFields, ForgeUpdatedAt: fUpdated,
	}); err != nil {
		return ApplyResult{}, err
	}
	if err := a.applyReferences(ctx, tx, project, pol, writable, b.ID, iss.Number, iss.Body); err != nil {
		return ApplyResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ApplyResult{}, err
	}
	return ApplyResult{Applied: 1}, nil
}

func (a *Applier) pullUpdateIssue(ctx context.Context, project store.ProjectRow, pol issueTypePolicy,
	writable []string, iss IssueRemote, blockID string, content store.ForgeIssueContent, forgeH string, forgeFields json.RawMessage, fUpdated *time.Time) (ApplyResult, error) {

	tx, err := a.pool.Begin(ctx)
	if err != nil {
		return ApplyResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := store.PullUpdateIssueBlock(ctx, tx, blockID, content); err != nil {
		return ApplyResult{}, err
	}
	if err := store.UpdateSyncMapBase(ctx, tx, blockID, forgeH, forgeFields, fUpdated); err != nil {
		return ApplyResult{}, err
	}
	if err := a.applyReferences(ctx, tx, project, pol, writable, blockID, iss.Number, iss.Body); err != nil {
		return ApplyResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ApplyResult{}, err
	}
	return ApplyResult{Applied: 1}, nil
}

// txBaseUpdate rewrites base_hash + the base-field snapshot alone (convergence)
// — 0 block writes.
func (a *Applier) txBaseUpdate(ctx context.Context, blockID, baseHash string, baseFields json.RawMessage, fUpdated *time.Time) error {
	tx, err := a.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := store.UpdateSyncMapBase(ctx, tx, blockID, baseHash, baseFields, fUpdated); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (a *Applier) flagConflict(ctx context.Context, blockID string) (ApplyResult, error) {
	tx, err := a.pool.Begin(ctx)
	if err != nil {
		return ApplyResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := store.FlagSyncMapConflict(ctx, tx, blockID, a.clock()); err != nil {
		return ApplyResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ApplyResult{}, err
	}
	return ApplyResult{Conflicts: 1}, nil
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
			continue // parent issue not pulled yet — added on a later run
		}
		m, exists := cmaps[c.ID]
		r, err := a.applyComment(ctx, project, writable, c, parent.BlockID, m, exists)
		if err != nil {
			return res, err
		}
		res.Applied += r.Applied
		res.Conflicts += r.Conflicts
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
	ctxFields, ctxH := CtxCommentBase(block)
	base := m.BaseHash
	switch {
	case base == ctxH && base == forgeH:
		return ApplyResult{}, nil
	case base == ctxH && base != forgeH:
		return a.pullUpdateComment(ctx, m.BlockID, cappedBody, forgeH, forgeFields, &fUpdated)
	case base != ctxH && base == forgeH:
		return ApplyResult{}, nil // ctx-ahead (I-H)
	case ctxH == forgeH:
		return ApplyResult{}, a.txBaseUpdate(ctx, m.BlockID, ctxH, ctxFields, &fUpdated)
	default:
		if m.Conflict {
			return ApplyResult{}, nil
		}
		return a.flagConflict(ctx, m.BlockID)
	}
}

func (a *Applier) pullCreateComment(ctx context.Context, project store.ProjectRow, c CommentRemote,
	parentBlockID, cappedBody, forgeH string, forgeFields json.RawMessage, fUpdated *time.Time, writable []string) (ApplyResult, error) {

	tx, err := a.pool.Begin(ctx)
	if err != nil {
		return ApplyResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	b, err := store.InsertCommentBlock(ctx, tx, parentBlockID, store.CommentFields{
		Content:  cappedBody,
		Metadata: map[string]any{"forge_comment_id": c.ID},
	}, writable)
	if err != nil {
		return ApplyResult{}, err
	}
	if err := store.InsertSyncMap(ctx, tx, store.SyncMap{
		ProjectID: project.ID, EntityKind: entityKindComment, ForgeID: c.ID,
		BlockID: b.ID, BaseHash: forgeH, BaseFields: forgeFields, ForgeUpdatedAt: fUpdated,
	}); err != nil {
		return ApplyResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ApplyResult{}, err
	}
	return ApplyResult{Applied: 1}, nil
}

func (a *Applier) pullUpdateComment(ctx context.Context, blockID, body, forgeH string, forgeFields json.RawMessage, fUpdated *time.Time) (ApplyResult, error) {
	tx, err := a.pool.Begin(ctx)
	if err != nil {
		return ApplyResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := store.PullUpdateCommentBlock(ctx, tx, blockID, body); err != nil {
		return ApplyResult{}, err
	}
	if err := store.UpdateSyncMapBase(ctx, tx, blockID, forgeH, forgeFields, fUpdated); err != nil {
		return ApplyResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
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

// applyReferences materialises the 'references' fact edges from a body (§4.5.7):
// each "#<nr>" resolves to a target block via the mapping — a number with NO
// mapping yields NO edge (no phantom links; the next run that pulls the target
// adds the edge idempotently via the PK). Self-references are dropped and the
// structural-link-class gate is honoured. 0 forge requests (pure local parsing).
// The target lookup rides the pool (the targets are committed blocks of other
// issues); the edge write rides the caller's Tx (atomic with the source block).
func (a *Applier) applyReferences(ctx context.Context, tx pgx.Tx, project store.ProjectRow,
	pol issueTypePolicy, writable []string, sourceID string, sourceNum int64, body string) error {

	if !pol.refAllowed {
		return nil
	}
	refs := parseIssueRefs(body)
	targetNums := make([]int64, 0, len(refs))
	for n := range refs {
		if n != sourceNum { // drop a self-reference (PutStructuralLink rejects self-loops)
			targetNums = append(targetNums, n)
		}
	}
	if len(targetNums) == 0 {
		return nil
	}
	tmaps, err := store.GetSyncMapsByForge(ctx, a.pool, project.ID, entityKindIssue, targetNums)
	if err != nil {
		return err
	}
	for _, n := range targetNums {
		tm, ok := tmaps[n]
		if !ok {
			continue // unknown number ⇒ NO phantom edge (§4.5.7)
		}
		err := store.PutStructuralLink(ctx, tx, store.StructuralLink{
			SourceID: sourceID, TargetID: tm.BlockID,
			LinkClass: structLinkReferences, Origin: originForgeSync,
		}, writable)
		if err != nil && !errors.Is(err, store.ErrLinkScopeViolation) {
			return err // a scope violation is defensively skipped (same-scope by construction)
		}
	}
	return nil
}
