// Package store — deterministic structural link layer (Achse 02, Welle I-A).
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
// same Tx (FK race guard). Missing ids are simply absent from the map.
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
