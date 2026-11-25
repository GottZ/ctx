// structlinks.go — deterministic structural link layer (Achse 02, Welle I-A).
//
// context_structural_links (migration 076) carries FACT edges between blocks:
// confidence 1.0 by definition, forge-/system-/manual-derived, STRICTLY
// separate from context_dream_links (discovered, confidence-gated, replace-
// swept). The Dream cycle (dream.WriteLinks replace, dream.CleanupDanglingLinks)
// operates only on context_dream_links and therefore never touches these rows
// (negative gate: structlinks_dream_isolation test in package dream_test).
//
// The comment→issue parent relation is NOT stored here — it lives on
// context_blocks.parent_id (001:39, FK+CASCADE since 076). One fact, one place
// (K3 / masterplan §2): parent_id = the ONE structural parent; this table = all
// further structural classes (references, duplicate-of, …).
//
// Scope isolation is fail-closed and double-lined (§5.2):
//   - WRITE: PutStructuralLink validates source ∈ writableScopes AND target in
//     the SAME scope IN THE SAME Tx (FOR SHARE), and DERIVES the row scope from
//     the source — never trusts a caller-supplied scope. Unknown, invisible and
//     cross-scope targets all collapse to ONE error (ErrLinkScopeViolation →
//     handler 404): no existence oracle for foreign block UUIDs.
//   - READ: the ego-graph batch readers (structuralHopNeighbors/Q1s and
//     inducedStructEdges/Q2s, store/graph.go) filter through the shared
//     visibility.Predicate, so a cross-scope edge that only raw SQL could
//     have injected never yields a foreign title (per-leg red-proven, GB1
//     W2-G1). The former single-block reader StructuralNeighbors was retired
//     in GB6 (E7): unwired since M076, its visibility gate lives on in the
//     batch readers' tests.
package store

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/GottZ/ctx/internal/pgxdb"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrLinkScopeViolation is the UNIFIED error for every structural-link write
// that fails scope/existence validation: source not writable, target unknown,
// target invisible, or target in a foreign scope. Callers (handler) map it to a
// single 404 so a foreign block UUID cannot be probed for existence (§4.3/§5.2).
var ErrLinkScopeViolation = errors.New("store: structural link scope violation")

// StructuralLink is one deterministic edge (conf 1.0 by definition — no
// confidence field). Scope is DERIVED from the source on write, never taken
// from the caller.
type StructuralLink struct {
	SourceID  string         `json:"source_id"`
	TargetID  string         `json:"target_id"`
	LinkClass string         `json:"link_class"`
	Scope     string         `json:"scope"`
	Origin    string         `json:"origin"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// blockScopes fetches id→scope for the given block ids with FOR SHARE, so the
// rows cannot be deleted between validation and the dependent write inside the
// same Tx (FK race guard), and their scope cannot move either: a scope move
// (UpdateBlock) takes the row FOR NO KEY UPDATE — a scope is no key column —
// which waits for FOR SHARE but not for FOR KEY SHARE, and its link sweep then
// finds the committed link (issue #503). Missing ids are simply absent from
// the map.
func blockScopes(ctx context.Context, tx pgx.Tx, ids []string) (map[string]string, error) {
	rows, err := tx.Query(ctx,
		`SELECT id::text, scope FROM context_blocks WHERE id = ANY($1::uuid[]) FOR SHARE`,
		ids)
	if err != nil {
		return nil, fmt.Errorf("structural link: lock blocks: %w", err)
	}
	defer rows.Close()
	out := make(map[string]string, len(ids))
	for rows.Next() {
		var id, scope string
		if err := rows.Scan(&id, &scope); err != nil {
			return nil, fmt.Errorf("structural link: scan block scope: %w", err)
		}
		out[id] = scope
	}
	return out, rows.Err()
}

// lockLinkTargetsBelowSQL locks the link targets of a writer that takes a
// block for writing and links it to them (LockPulledBlock, issue #503) whose
// id lies below that block's ($2): FOR SHARE — the lock blockScopes takes,
// so PutStructuralLink finds them held — in ascending id order (ORDER BY id
// puts the sort under the row locks, so the rows are taken in that order).
const lockLinkTargetsBelowSQL = `SELECT 1 FROM context_blocks WHERE id = ANY($1::uuid[]) AND id < $2::uuid ORDER BY id FOR SHARE`

// lockLinkTargetsBelow runs lockLinkTargetsBelowSQL over targets (no
// statement for none) below the block pivot.
func lockLinkTargetsBelow(ctx context.Context, tx pgx.Tx, targets []string, pivot string) error {
	if len(targets) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, lockLinkTargetsBelowSQL, targets, pivot)
	if err != nil {
		return fmt.Errorf("structural link: lock targets: %w", err)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("structural link: lock targets: %w", err)
	}
	return nil
}

// scopeWritable reports whether scope is in the caller's writable set.
func scopeWritable(scope string, allowedWriteScopes []string) bool {
	for _, s := range allowedWriteScopes {
		if s == scope {
			return true
		}
	}
	return false
}

// PutStructuralLink inserts a structural edge, validating IN THE SAME Tx
// (fail-closed): both blocks exist, source.scope == target.scope, and that
// scope is in allowedWriteScopes. The row scope is DERIVED from the source. Any
// violation ⇒ ErrLinkScopeViolation (unified — no existence oracle). Idempotent:
// INSERT ON CONFLICT DO NOTHING, so re-putting the same (source,target,class)
// is a no-op; a DIFFERENT class on the same pair inserts a second row (PK
// carries link_class).
func PutStructuralLink(ctx context.Context, tx pgx.Tx, l StructuralLink, allowedWriteScopes []string) error {
	if l.LinkClass == "" {
		return fmt.Errorf("structural link: empty link_class")
	}
	if l.SourceID == l.TargetID {
		// Self-loops are meaningless AND blocked by the table CHECK; reject
		// early with the unified error (a self-referential UUID reveals nothing).
		return ErrLinkScopeViolation
	}
	scopes, err := blockScopes(ctx, tx, []string{l.SourceID, l.TargetID})
	if err != nil {
		return err
	}
	srcScope, srcOK := scopes[l.SourceID]
	tgtScope, tgtOK := scopes[l.TargetID]
	// Unknown source/target, cross-scope, or source not writable ⇒ ONE error.
	if !srcOK || !tgtOK || srcScope != tgtScope || !scopeWritable(srcScope, allowedWriteScopes) {
		return ErrLinkScopeViolation
	}

	origin := l.Origin
	if origin == "" {
		origin = "manual"
	}
	meta := l.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO context_structural_links
		   (source_block_id, target_block_id, link_class, scope, origin, metadata)
		 VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6::jsonb)
		 ON CONFLICT (source_block_id, target_block_id, link_class) DO NOTHING`,
		l.SourceID, l.TargetID, l.LinkClass, srcScope, origin, meta)
	if err != nil {
		return fmt.Errorf("structural link: insert: %w", err)
	}
	return nil
}

// DeleteStructuralLink removes one edge after validating the source block is in
// allowedWriteScopes (unknown/foreign source ⇒ ErrLinkScopeViolation, no
// oracle). Removing a non-existent edge of a writable source is a no-op.
func DeleteStructuralLink(ctx context.Context, tx pgx.Tx, sourceID, targetID, class string, allowedWriteScopes []string) error {
	if class == "" {
		return fmt.Errorf("structural link: empty link_class")
	}
	scopes, err := blockScopes(ctx, tx, []string{sourceID})
	if err != nil {
		return err
	}
	srcScope, ok := scopes[sourceID]
	if !ok || !scopeWritable(srcScope, allowedWriteScopes) {
		return ErrLinkScopeViolation
	}
	_, err = tx.Exec(ctx,
		`DELETE FROM context_structural_links
		 WHERE source_block_id = $1::uuid AND target_block_id = $2::uuid AND link_class = $3`,
		sourceID, targetID, class)
	if err != nil {
		return fmt.Errorf("structural link: delete: %w", err)
	}
	return nil
}

// PutBlockParent sets context_blocks.parent_id (the ONE structural parent, K3)
// enforcing the comment-scope invariant (§5.2): child and parent must share a
// scope, and that scope must be in allowedWriteScopes. This is the parent_id
// write path; I-D2's InsertCommentBlock composes it (scope := parent scope).
// Unknown/foreign child or parent ⇒ ErrLinkScopeViolation (no oracle).
func PutBlockParent(ctx context.Context, tx pgx.Tx, childID, parentID string, allowedWriteScopes []string) error {
	if childID == parentID {
		return ErrLinkScopeViolation
	}
	scopes, err := blockScopes(ctx, tx, []string{childID, parentID})
	if err != nil {
		return err
	}
	childScope, cOK := scopes[childID]
	parentScope, pOK := scopes[parentID]
	if !cOK || !pOK || childScope != parentScope || !scopeWritable(childScope, allowedWriteScopes) {
		return ErrLinkScopeViolation
	}
	_, err = tx.Exec(ctx,
		`UPDATE context_blocks SET parent_id = $1::uuid, updated_at = now() WHERE id = $2::uuid`,
		parentID, childID)
	if err != nil {
		return fmt.Errorf("set block parent: %w", err)
	}
	return nil
}

// ParentLinkable reports whether parentID may become the parent of the block a
// write addresses — the row its upsert key (childCategory, childTitle,
// childScope) resolves to. nil = linkable. Unknown id, archived block, foreign
// scope, a malformed uuid and the write's OWN identity ALL collapse into
// ErrLinkScopeViolation — the same uniform verdict PutBlockParent gives, so the
// difference cannot be used to probe a foreign block id for existence
// (§4.3/§5.2).
//
// The identity comparison is what makes the self-parent case a refusal BEFORE
// the write rather than after it: a client that names the very block its
// category+title address would otherwise get its content (and its claimed type)
// upserted and THEN a refusal from PutBlockParent's childID == parentID line —
// an orphan of exactly the kind the claim gate exists to prevent. Same scope
// plus same category plus same title is the same row, because that triple is the
// partial unique key of the live corpus.
//
// It is the pre-check of handler /api/store, before the write's transaction
// takes its locks (context_store.go). Since issue #211 that transaction checks
// the parent again, locked (lockBatchParent), and links it in the same
// transaction as the write (LinkBlockParent), so a parent that changes in
// between refuses the write instead of leaving a block without its parent.
//
// It grants nothing and replaces nothing: PutBlockParent re-validates inside the
// transaction that performs the write, and its verdict is the authoritative one.
// The extra condition here is is_archived, which PutBlockParent does not read —
// deliberately stricter, because an archived parent is exactly the parent a
// caller must not hang new blocks under, and InsertCommentBlock excludes it too.
func ParentLinkable(ctx context.Context, q pgxdb.Rower, parentID, childScope, childCategory, childTitle string) error {
	return parentLinkable(ctx, q, "", parentID, childScope, childCategory, childTitle)
}

// lockBatchParent is ParentLinkable inside a write's own transaction, the
// parent row locked FOR SHARE by the same read — the lock PutBlockParent takes
// on it (blockScopes) — so it can neither be archived, deleted nor moved to
// another scope before the link the write makes commits (issue #211: a block
// batch, and /api/store with a parent, which writes block and link in ONE
// transaction since then).
func lockBatchParent(ctx context.Context, tx pgx.Tx, parentID, childScope, childCategory, childTitle string) error {
	return parentLinkable(ctx, tx, " FOR SHARE", parentID, childScope, childCategory, childTitle)
}

// parentLinkable is ParentLinkable's check, its read ending in lockClause.
// The child's scope is part of the read's WHERE, not a comparison after it: a
// block of another scope is no row here, so a locking read never takes — or
// waits for — a lock on a block the caller may not see, and the time of the
// answer says no more than its text does (review of #211).
func parentLinkable(ctx context.Context, q pgxdb.Rower, lockClause, parentID, childScope, childCategory, childTitle string) error {
	if !IsFullUUID(parentID) {
		return ErrLinkScopeViolation
	}
	var parentCategory, parentTitle string
	err := q.QueryRow(ctx,
		`SELECT category, title FROM context_blocks WHERE id = $1::uuid AND scope = $2 AND NOT is_archived`+lockClause,
		parentID, childScope).Scan(&parentCategory, &parentTitle)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrLinkScopeViolation
	}
	if err != nil {
		return fmt.Errorf("structural link: parent lookup: %w", err)
	}
	if parentCategory == childCategory && parentTitle == childTitle {
		return ErrLinkScopeViolation
	}
	return nil
}

// parentCycleSQL answers whether the block $2 is $1 or one of $1's ancestors
// through parent_id — hanging $2 under $1 would then close a cycle — and names
// the scopes of $1 and its ancestors. UNION (not UNION ALL) drops a row
// already walked, so an existing cycle above $1 ends the walk instead of
// looping; each step is a primary-key probe, so the cost is the depth of $1's
// chain. It runs in the linking transaction and sees the links the
// transaction set before it (the earlier items of a batch).
const parentCycleSQL = `WITH RECURSIVE up(id, scope, parent_id) AS (
		SELECT id, scope, parent_id FROM context_blocks WHERE id = $1::uuid
		UNION
		SELECT b.id, b.scope, b.parent_id FROM context_blocks b JOIN up ON b.id = up.parent_id)
	SELECT EXISTS (SELECT 1 FROM up WHERE id = $2::uuid) OR $1::uuid = $2::uuid,
	       ARRAY(SELECT DISTINCT scope FROM up ORDER BY 1)`

// LinkBlockParent hangs childID under parentID in the caller's transaction,
// the parent check of /api/store and of a block batch's parents (issue #211):
// a parent that would close a cycle — the child itself, or a block below the
// child — is refused with the one ErrLinkScopeViolation every other unlinkable
// parent answers (no oracle), then PutBlockParent re-asserts the scope
// invariant (child and parent in scope) and writes the link. parent_id is the
// ONE structural parent (K3): a tree, which ParentLinkable already kept free
// of the one-block cycle (a block as its own parent) and a two-step write
// could close with two (issue #211).
//
// The walk is serialized against the other parent writes of every scope it
// passes through (issue #547): held names the scopes whose supersedes locks
// the transaction holds — every parent write takes the lock of its child's
// scope, a scope move the lock of the scope it moves from. A chain that
// crosses into a scope the transaction does not hold (a scope move keeps
// parent_id) could be changed there by a write the walk cannot see — two such
// writes, each in its own scope, closed a cycle together — so LinkBlockParent
// then asks for the transaction to run again with those locks taken up front
// (scopesToLockError, writeSupersedesSourceScopes) and decides nothing on
// what it read. A chain within the held scopes adds no statement.
func LinkBlockParent(ctx context.Context, tx pgx.Tx, childID, parentID, scope string, held []string) error {
	parsed, err := uuid.Parse(parentID)
	if err != nil || !IsFullUUID(parentID) {
		return ErrLinkScopeViolation
	}
	// PutBlockParent matches the ids against the canonical lowercase form the
	// database returns (blockScopes): a parent named in capitals is the same
	// block and links as such.
	parentID = parsed.String()
	var cycle bool
	var chain []string
	if err = tx.QueryRow(ctx, parentCycleSQL, parentID, childID).Scan(&cycle, &chain); err != nil {
		return fmt.Errorf("structural link: parent cycle check: %w", err)
	}
	var missing []string
	for _, sc := range chain {
		if !slices.Contains(held, sc) {
			missing = append(missing, sc)
		}
	}
	if len(missing) > 0 {
		return &scopesToLockError{scopes: missing}
	}
	if cycle {
		return ErrLinkScopeViolation
	}
	return PutBlockParent(ctx, tx, childID, parentID, []string{scope})
}
