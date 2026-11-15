// Writer-declared supersedes (issue #197): a store that names `supersedes`
// states that the written block replaces another one. Dream can only GUESS
// that relation (category, title similarity, confidence gates); a writer who
// knows it had no way to say so, and could only archive the old block — which
// loses the history and the pointer to its replacement.
//
// The declaration is a context_dream_links row, relationship 'supersedes',
// pinned, metadata {"origin": "declared", "api_key_id": …}. Pinned rows
// survive dream's replace sweep and are not re-classified by a later dream
// cycle (dream/writelinks.go), the reconcile below and dream.SupersedesMap read
// them on the paths every supersedes link takes, and the metadata keeps a
// declaration machine-readably apart from a dream guess an operator confirmed
// (dream-link-resolve writes only pinned/rationale). The structural link
// table keeps `supersedes` reserved (blocktype/policy.go reservedGraphClasses).
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/pgxdb"
)

// SupersedesChainDepth bounds every walk along supersedes links that a
// declaration runs: the cycle check (upwards from the written block: the
// blocks that supersede it) and the search for the head of a chain. A written
// block with this many superseders stacked above it is refused as a possible
// cycle (fail-closed); a new block, or the head of a chain, has none, so the
// bound never limits how long a chain of corrections can grow.
const SupersedesChainDepth = 64

// DeclaredSupersedesOrigin is the value of metadata.origin on a declared
// supersedes link — the machine-readable mark that tells it apart from a link
// dream wrote (metadata '{}'), whether or not an operator pinned that one.
const DeclaredSupersedesOrigin = "declared"

// SettledSupersedesSQL is the predicate, over a context_dream_links row aliased
// `dl`, of a supersedes link that SETTLES its target: pinned (a writer's
// declaration, or a dream guess an operator confirmed) and at the confidence
// the reconcile needs to make a target a snapshot (supersedesSnapshotConfidence;
// a declaration carries 1). The caller adds relationship = 'supersedes' and the
// source's liveness and scope. One definition for the three readers that need
// it: the query's settled set (dream.SupersedesMap), the already-superseded
// conflict and the head of a chain (below).
const SettledSupersedesSQL = `dl.pinned AND dl.confidence >= 0.7::real`

// declaredSupersedesRationale is the human-readable rationale of a declared
// link, shown next to it in dream-review.
const declaredSupersedesRationale = "declared by the writer of the source block on store"

// ErrSupersedesTarget is the ONE refusal for a `supersedes` id this write
// cannot name: unknown, archived, outside the scope the write resolves to,
// malformed, or the written block itself. Told apart, these would answer
// "does this uuid exist" and "is it yours" for any id a caller tries — the
// parent_id rule (ParentLinkable). The statement sequence up to the refusal
// is the same for all of them (see lockSupersedesPair).
var ErrSupersedesTarget = errors.New("store: supersedes target is not a block of this write's scope")

// Kinds of SupersedesConflictError. They concern blocks in the write's own
// scope only — the key may write, and so read, every block they name.
const (
	// SupersedesCycle — the target already supersedes the written block,
	// directly or through a chain, pinned or guessed (or the written block has
	// SupersedesChainDepth or more superseders above it).
	SupersedesCycle = "cycle"
	// SupersedesSuperseded — the target is already superseded through a
	// pinned link by another live block of its scope; Head names the current
	// head of that chain, the block a declaration should name instead — or is
	// "" when the chain above the target is SupersedesChainDepth links or
	// longer and the walk could not reach its head.
	SupersedesSuperseded = "superseded"
	// SupersedesNotAllowed — the target is a chunk row (issue #191: a chunk
	// belongs to its ingest and never leaves the 'chunk' lifecycle) or of a
	// type that does not take part in the dream loop (dream.linkable=false,
	// or not registered: fail-closed).
	SupersedesNotAllowed = "not_allowed"
)

// SupersedesConflictError is a declaration refused for the state of blocks
// the key may read.
type SupersedesConflictError struct {
	Kind   string
	Target string
	Head   string // SupersedesSuperseded only
}

func (e *SupersedesConflictError) Error() string {
	switch e.Kind {
	case SupersedesSuperseded:
		return fmt.Sprintf("store: supersedes %s: already superseded, current head %s", e.Target, e.Head)
	case SupersedesCycle:
		return fmt.Sprintf("store: supersedes %s: the target already supersedes this block (cycle)", e.Target)
	default:
		return fmt.Sprintf("store: supersedes %s: target cannot be superseded", e.Target)
	}
}

// LockSupersedesScope and ShareSupersedesScope are the per-scope supersedes
// lock, a transaction advisory lock in two modes. A store declaring a
// supersedes (UpsertBlockSupersedes, CheckSupersedesDeclarable) takes it
// EXCLUSIVE; a dream link batch (dream.WriteLinks) takes it SHARED. The cycle
// checks of both read links of OTHER blocks — the superseders above the
// written block — which no row lock of theirs covers, so without the lock a
// declaration and a dream guess could each check before the other commits and
// together close a cycle; and a declaration waiting for a link row a dream
// batch holds, while holding that batch's source, would deadlock with the
// batch's next insert. Exclusive against shared means a declaration never
// interleaves with a dream batch or another declaration of its scope, while
// dream batches keep running side by side (dream batches with a supersedes
// guess are ordered among themselves by the second lock,
// LockSupersedesGuesses; the row-lock discipline of
// ReconcileSupersedesTargets is unchanged).
//
// The lock is the FIRST its transaction takes, and the guess lock, where a
// transaction takes one, the second: every holder then waits for either one
// holding no row lock. Transaction-scoped; released at commit or rollback.
func LockSupersedesScope(ctx context.Context, tx pgx.Tx, scope string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('ctx:supersedes:' || $1::text))`, scope); err != nil {
		return fmt.Errorf("store: supersedes scope lock: %w", err)
	}
	return nil
}

// ShareSupersedesScope is LockSupersedesScope in shared mode (same key).
func ShareSupersedesScope(ctx context.Context, tx pgx.Tx, scope string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock_shared(hashtext('ctx:supersedes:' || $1::text))`, scope); err != nil {
		return fmt.Errorf("store: supersedes scope lock (shared): %w", err)
	}
	return nil
}

// LockSupersedesGuesses is the per-scope lock of dream supersedes guesses,
// taken EXCLUSIVE by a dream link batch that carries a supersedes candidate
// (dream.WriteLinks), after ShareSupersedesScope. Two such batches each run
// the cycle walk over committed links only, so side by side each could miss
// the other's uncommitted guess and together close a cycle through declared
// links that run against created_at (A→B and C→D beside declared B→C and D→A).
// The lock orders them: a batch's walk sees every guess an earlier batch of the
// scope committed. A batch without a supersedes candidate does not take it —
// it can only delete or re-classify supersedes links, which closes no cycle.
//
// A key space apart from LockSupersedesScope's (the two-key form of the
// advisory lock functions), so the two locks of one scope never share a key.
func LockSupersedesGuesses(ctx context.Context, tx pgx.Tx, scope string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('ctx:supersedes-guess'), hashtext($1::text))`, scope); err != nil {
		return fmt.Errorf("store: supersedes guess lock: %w", err)
	}
	return nil
}

// SupersedesDeclaration is the `supersedes` of one store.
type SupersedesDeclaration struct {
	// TargetID is the block the write replaces, as the client sent it.
	TargetID string
	// APIKeyID is recorded on the link (metadata.api_key_id).
	APIKeyID string
	// Types is the request's block-type policy snapshot: the target's type
	// must be dream-linkable.
	Types *blocktype.Set
}

// supersedesTarget is the locked target row.
type supersedesTarget struct {
	id       string
	typeName string
	chunk    bool
}

// UpsertBlockSupersedes is UpsertBlock plus a supersedes declaration, in ONE
// transaction: a refused declaration writes nothing, and a failed write leaves
// no link. On success the target is a snapshot with superseded_by = the
// written block.
//
// Rules, kept deliberately small:
//   - No direction rule over created_at: the writer knows which statement
//     stands, so an upsert of an older block may supersede a newer one.
//   - Additive: a declaration never removes another. A block may supersede
//     several (one store each); a repeat declaration is a no-op; a store
//     without the field leaves the block's declarations alone. The way back
//     is the delete-resolve of the link (`ctx dream resolve <source> <target>
//     supersedes delete`, DreamLinkResolve), which reconciles the target.
//
// Lock order: the scope's supersedes lock (LockSupersedesScope, first), the
// written block's row (the upsert identity, FOR UPDATE — the lock
// UpsertBlock's pre-read takes anyway), then the target (FOR NO KEY UPDATE),
// all BEFORE the first write of the transaction. Source before
// target, not id order: a dream batch that already inserted a link touching
// the source holds FOR KEY SHARE on it and then locks its supersedes targets,
// so taking the target first and then waiting for the source would close a
// cycle with it. Every write on context_blocks fires mark_guard_dirty, which
// holds the context_guard_state row until commit, so no row lock may be
// waited for after the upsert (ReconcileSupersedesTargets). A declaration
// holds the scope lock exclusively, so it runs alone against the dream
// batches and the other declarations of its scope: a dream batch writing
// links from the same block, or two stores declaring each other, wait instead
// of deadlocking.
func UpsertBlockSupersedes(ctx context.Context, pool *pgxpool.Pool, category, title, content string, tags []string, metadata map[string]any, scope string, scopeExplicit bool, sens SensitivityWrite, typeName string, decl SupersedesDeclaration) (*Block, error) {
	u := prepareUpsert(category, title, content, tags, metadata, scope, scopeExplicit, sens, typeName)
	var b *Block
	err := pgxdb.Write(ctx, pool, pgxdb.At("store: upsert block declaring supersedes"), func(tx pgx.Tx) error {
		if err := LockSupersedesScope(ctx, tx, scope); err != nil {
			return err
		}
		tgt, _, err := lockSupersedesPair(ctx, tx, category, title, scope, decl.TargetID)
		if err != nil {
			return err
		}
		if err := supersedesTargetAllowed(tgt, decl.Types); err != nil {
			return err
		}
		if b, err = u.exec(ctx, tx); err != nil {
			return err
		}
		if b.ID == tgt.id {
			// A concurrent first create of this identity, committed between
			// the source lock and the upsert, can still land here.
			return ErrSupersedesTarget
		}
		if err := supersedesStateAllows(ctx, tx, b.ID, tgt.id, scope); err != nil {
			return err
		}
		return declareSupersedes(ctx, tx, b.ID, tgt.id, scope, decl.APIKeyID)
	})
	if err != nil {
		return nil, err
	}
	return b, nil
}

// CheckSupersedesDeclarable runs every gate UpsertBlockSupersedes runs before
// and after its upsert, without writing: the staged MCP store checks it, so a
// card never promises a declaration the confirm would refuse. It grants
// nothing — the confirm runs UpsertBlockSupersedes, which decides again.
// When the write's identity has no row yet, the state gates run against the
// target alone (a block that does not exist supersedes nothing, so it can
// close no cycle).
func CheckSupersedesDeclarable(ctx context.Context, pool *pgxpool.Pool, category, title, scope string, decl SupersedesDeclaration) error {
	err := pgxdb.Write(ctx, pool, pgxdb.At("store: check supersedes"), func(tx pgx.Tx) error {
		if err := LockSupersedesScope(ctx, tx, scope); err != nil {
			return err
		}
		tgt, sourceID, err := lockSupersedesPair(ctx, tx, category, title, scope, decl.TargetID)
		if err != nil {
			return err
		}
		if err := supersedesTargetAllowed(tgt, decl.Types); err != nil {
			return err
		}
		if err := supersedesStateAllows(ctx, tx, sourceID, tgt.id, scope); err != nil {
			return err
		}
		return pgxdb.ErrRollback
	})
	if errors.Is(err, pgxdb.ErrRollback) {
		return nil
	}
	return err
}

// lockSupersedesPair locks the write's own identity row (if it exists) and
// then the target, and returns the target and the identity row's id ("" when
// the write will insert). Every id this write may not name
// answers ErrSupersedesTarget:
//
//   - a malformed id before any statement — it names no block;
//   - unknown, archived and foreign-scope ids after the SAME two statements
//     (identity lock, target lock), because the target lock carries scope and
//     archive in its WHERE: a row outside the write scope is never locked, so
//     a key cannot learn from a lock wait that a foreign block exists;
//   - the write's own identity after the same two statements as well.
func lockSupersedesPair(ctx context.Context, tx pgx.Tx, category, title, scope, targetID string) (supersedesTarget, string, error) {
	if !IsFullUUID(targetID) {
		return supersedesTarget{}, "", ErrSupersedesTarget
	}
	parsed, err := uuid.Parse(targetID)
	if err != nil {
		return supersedesTarget{}, "", ErrSupersedesTarget
	}
	tgt := supersedesTarget{id: parsed.String()}

	var sourceID string
	if err := tx.QueryRow(ctx,
		`SELECT id::text FROM context_blocks
		  WHERE category = $1 AND title = $2 AND scope = $3 AND NOT is_archived
		  FOR UPDATE`,
		category, title, scope).Scan(&sourceID); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return supersedesTarget{}, "", fmt.Errorf("store: supersedes: lock source: %w", err)
	}

	err = tx.QueryRow(ctx,
		`SELECT type_name, lifecycle_state = 'chunk' OR (source_id IS NOT NULL AND chunk_index IS NOT NULL)
		   FROM context_blocks
		  WHERE id = $1::uuid AND scope = $2 AND NOT is_archived
		  FOR NO KEY UPDATE`,
		tgt.id, scope).Scan(&tgt.typeName, &tgt.chunk)
	if errors.Is(err, pgx.ErrNoRows) {
		return supersedesTarget{}, "", ErrSupersedesTarget
	}
	if err != nil {
		return supersedesTarget{}, "", fmt.Errorf("store: supersedes: lock target: %w", err)
	}
	if tgt.id == sourceID {
		return supersedesTarget{}, "", ErrSupersedesTarget
	}
	return tgt, sourceID, nil
}

// supersedesTargetAllowed refuses targets whose policy keeps them out of the
// supersedes state: chunk rows (#191) and types that are not dream-linkable —
// dream never links such a target, so the reconcile could never have made it
// a snapshot. An unregistered type is refused (fail-closed); a nil set is a
// wiring bug and refused too.
func supersedesTargetAllowed(tgt supersedesTarget, types *blocktype.Set) error {
	if tgt.chunk {
		return &SupersedesConflictError{Kind: SupersedesNotAllowed, Target: tgt.id}
	}
	if types == nil {
		return &SupersedesConflictError{Kind: SupersedesNotAllowed, Target: tgt.id}
	}
	if p, known := types.Resolve(tgt.typeName); !known || !p.Dream.Linkable {
		return &SupersedesConflictError{Kind: SupersedesNotAllowed, Target: tgt.id}
	}
	return nil
}

// supersedesStateAllows is the state half of the gates: the target is not
// already settled by ANOTHER live block of its scope (SettledSupersedesSQL:
// a declaration, or a dream guess an operator confirmed), and declaring would
// close no cycle. sourceID "" (no row yet) skips the cycle walk — nothing
// supersedes a block that does not exist.
//
// A target that only a dream GUESS supersedes (unpinned) is not a conflict:
// the writer's statement outranks the guess, and declareSupersedes moves the
// target's pointer to the written block.
func supersedesStateAllows(ctx context.Context, tx pgx.Tx, sourceID, targetID, scope string) error {
	var other string
	err := tx.QueryRow(ctx,
		`SELECT dl.source_block_id::text
		   FROM context_dream_links dl
		   JOIN context_blocks s ON s.id = dl.source_block_id
		  WHERE dl.target_block_id = $1::uuid
		    AND dl.relationship = 'supersedes'
		    AND `+SettledSupersedesSQL+`
		    AND dl.source_block_id IS DISTINCT FROM $2::uuid
		    AND NOT s.is_archived
		    AND s.scope = $3
		  ORDER BY dl.created_at, dl.source_block_id
		  LIMIT 1`,
		targetID, nullableUUID(sourceID), scope).Scan(&other)
	switch {
	case err == nil:
		head, herr := supersedesHead(ctx, tx, other, scope)
		if herr != nil {
			return herr
		}
		return &SupersedesConflictError{Kind: SupersedesSuperseded, Target: targetID, Head: head}
	case !errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("store: supersedes: other superseder: %w", err)
	}
	if sourceID == "" {
		return nil
	}

	// Cycle: does the target (directly or through a chain) supersede the
	// source already? Every supersedes link counts, pinned or not: a dream
	// guess that the target supersedes the source refuses the declaration as
	// a cycle too — the remedy is to delete that guess (`ctx dream resolve
	// <target> <source> supersedes delete`) and declare again.
	cycle, err := SupersedesWouldCycle(ctx, tx, sourceID, targetID)
	if err != nil {
		return err
	}
	if cycle {
		return &SupersedesConflictError{Kind: SupersedesCycle, Target: targetID}
	}
	return nil
}

// SupersedesWouldCycle reports whether a supersedes link source → target
// would close a cycle: whether the target already supersedes the source,
// directly or through a chain of supersedes links of any kind. The walk goes
// UP from the source — the blocks that supersede it, and theirs — so its
// length is the number of blocks above the source: a new block, or the head
// of a chain, walks nothing, and a correction of a long chain is as cheap as
// the first. A source with SupersedesChainDepth or more superseders above it
// is reported as a cycle (fail-closed). One walk for both writers of
// supersedes links: a declaration (supersedesStateAllows) and a dream guess
// (dream.WriteLinks); both run it under LockSupersedesScope.
func SupersedesWouldCycle(ctx context.Context, q pgxdb.Rower, sourceID, targetID string) (bool, error) {
	var cycle, tooDeep bool
	if err := q.QueryRow(ctx,
		`WITH RECURSIVE up(id, depth) AS (
		     SELECT dl.source_block_id, 1
		       FROM context_dream_links dl
		      WHERE dl.target_block_id = $1::uuid AND dl.relationship = 'supersedes'
		   UNION
		     SELECT dl.source_block_id, u.depth + 1
		       FROM up u
		       JOIN context_dream_links dl
		         ON dl.target_block_id = u.id AND dl.relationship = 'supersedes'
		      WHERE u.depth < $3
		 )
		 SELECT COALESCE(bool_or(id = $2::uuid), false), COALESCE(max(depth), 0) >= $3 FROM up`,
		sourceID, targetID, SupersedesChainDepth).Scan(&cycle, &tooDeep); err != nil {
		return false, fmt.Errorf("store: supersedes: cycle walk: %w", err)
	}
	return cycle || tooDeep, nil
}

// supersedesHead follows settling supersedes links from blockID to the block
// no live settling link of the same scope supersedes — the current head of the
// chain. The walk stays in scope, so the head is a block of the write scope,
// which the key may read by construction (writableBlockScopes ⊆ read scopes).
func supersedesHead(ctx context.Context, tx pgx.Tx, blockID, scope string) (string, error) {
	var head string
	var depth int
	if err := tx.QueryRow(ctx,
		`WITH RECURSIVE chain(id, depth) AS (
		     SELECT $1::uuid, 0
		   UNION ALL
		     SELECT nxt.id, c.depth + 1
		       FROM chain c
		       CROSS JOIN LATERAL (
		         SELECT dl.source_block_id AS id
		           FROM context_dream_links dl
		           JOIN context_blocks s ON s.id = dl.source_block_id
		          WHERE dl.target_block_id = c.id
		            AND dl.relationship = 'supersedes'
		            AND `+SettledSupersedesSQL+`
		            AND NOT s.is_archived
		            AND s.scope = $2
		          ORDER BY dl.created_at, dl.source_block_id
		          LIMIT 1
		       ) nxt
		      WHERE c.depth < $3
		 )
		 SELECT id::text, depth FROM chain ORDER BY depth DESC LIMIT 1`,
		blockID, scope, SupersedesChainDepth).Scan(&head, &depth); err != nil {
		return "", fmt.Errorf("store: supersedes: chain head: %w", err)
	}
	if depth >= SupersedesChainDepth {
		// The walk stopped at the bound, so the block it reached need not be
		// the head (or the chain loops): name none rather than send the caller
		// to a block that answers the same conflict again.
		return "", nil
	}
	return head, nil
}

// declareSupersedes writes the pinned link and settles the target: its pointer
// moves to the written block (a dream guess that pointed elsewhere is
// outranked), and the reconcile — the one every supersedes path runs — keeps
// the state machine's invariants. The target row is already locked
// (lockSupersedesPair); the reconcile's own lock pass re-asserts it.
func declareSupersedes(ctx context.Context, tx pgx.Tx, sourceID, targetID, scope, apiKeyID string) error {
	md, err := json.Marshal(map[string]string{"origin": DeclaredSupersedesOrigin, "api_key_id": apiKeyID})
	if err != nil {
		return fmt.Errorf("store: supersedes: metadata: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO context_dream_links
		     (source_block_id, target_block_id, relationship, confidence, raw_confidence, scope, metadata, pinned, rationale)
		 VALUES ($1::uuid, $2::uuid, 'supersedes', 1, 1, $3, $4::jsonb, true, $5)
		 ON CONFLICT (source_block_id, target_block_id) DO UPDATE SET
		     relationship = 'supersedes',
		     confidence = 1,
		     raw_confidence = 1,
		     scope = EXCLUDED.scope,
		     metadata = EXCLUDED.metadata,
		     pinned = true,
		     rationale = EXCLUDED.rationale`,
		sourceID, targetID, scope, md, declaredSupersedesRationale); err != nil {
		return fmt.Errorf("store: supersedes: write link: %w", err)
	}
	if err := markSupersededSnapshot(ctx, tx, targetID, sourceID); err != nil {
		return err
	}
	if _, err := ReconcileSupersedesTargets(ctx, tx, []string{targetID}, sourceID); err != nil {
		return fmt.Errorf("store: supersedes: %w", err)
	}
	return nil
}

// nullableUUID maps "" to NULL for a ::uuid parameter.
func nullableUUID(id string) any {
	if id == "" {
		return nil
	}
	return id
}
