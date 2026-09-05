// Block-type registry CRUD (WF T10, design/01 §5.1(c)/§5.4/§7-T10) against
// context_block_types (migration 072). Every MUTATION runs in ONE explicit
// transaction stamped with setTxActor + SetTxRequestID — the 072 audit
// trigger (audit_block_type_write) reads those GUCs; a plain pool.Exec would
// record via='sql' with api_key_id NULL on exactly the writes that switch
// visibility policy (provenance loss, §3.2 R1). Config validation does NOT
// live here: blocktype.DecodePolicy is the single authority and runs in the
// handler BEFORE these writes (422 with field path); this layer only owns
// row identity, the builtin/in-use delete guards and the audit attribution.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/pgxdb"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// BlockTypeRow is one context_block_types row (wire shape of the manage
// type-* family; config stays raw JSON — the decoded Policy is a Go-internal
// resolution artifact, the API always shows what the row stores).
type BlockTypeRow struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Scope       string          `json:"scope"`
	DisplayName string          `json:"display_name"`
	Description string          `json:"description"`
	Builtin     bool            `json:"builtin"`
	IsDefault   bool            `json:"is_default"`
	Config      json.RawMessage `json:"config"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
	UpdatedBy   *string         `json:"updated_by,omitempty"`
}

// Sentinel errors the handler maps onto HTTP statuses.
var (
	// ErrBlockTypeExists — UNIQUE(name, scope) hit on create (409).
	ErrBlockTypeExists = errors.New("block type already exists")
	// ErrBlockTypeDefaultExists — the partial unique index uq_block_types_default
	// rejected a second is_default row per scope (409).
	ErrBlockTypeDefaultExists = errors.New("scope already has a default block type")
	// ErrBlockTypeBuiltin — delete guard: builtin rows are undeletable, always
	// (§5.1(c); their CONFIG is editable, that is the point of the registry).
	ErrBlockTypeBuiltin = errors.New("builtin block types cannot be deleted")
)

// BlockTypeInUseError — delete guard: blocks still reference the type. The
// count spans ALL rows (active + archived, §5.1(c) R1): an archived reference
// surviving the delete would resurface as an inexplicable orphan on unarchive.
type BlockTypeInUseError struct {
	Active   int
	Archived int
}

func (e *BlockTypeInUseError) Error() string {
	return fmt.Sprintf("block type is referenced by %d active and %d archived blocks", e.Active, e.Archived)
}

// RetrievalExcludedTypePredicate is the shared SQL fragment both embed-backfill
// pick queries (query.go Pfad A `backfillPending`, scheduler.go Pfad B
// `backfillOneEmbedding` — peek AND pick) AND-in, so a block whose TYPE carries
// retrieval policy 'excluded' never consumes an embed slot. Such a vector has
// NO consumer at all: every ranked path filters against the registry's
// visible-type allowlist (`ctx_rrf` p_types_visible, fed by
// blocktype.Set.VisibleTypes — 'excluded' is the one policy that is not in it),
// so the embedding is written once and then read by nobody.
//
// Vorfall 2026-07-25 (the wave trigger, three independent sightings): 33 Hermes
// "Compaction source" parts (type=checkpoint, ~36.5 kB each) saturated the CPU
// embed backend for hours, and the digest rebuild rewrites topic-map-private
// (type=system-meta, 73.7 kB ≈ ~32k real tokens) on EVERY boot → ClearEmbedding
// → ~60 min of CPU embed for a dead vector, recurring.
//
// SEMANTICS, deliberately conservative in the fail-safe direction — skipping an
// embedding is the destructive move (an unembedded block is FTS-only), so the
// predicate skips a block only when NO reader in the system could ever rank it:
//
//   - The '_global' row is the base every tenant inherits; only `= 'excluded'`
//     skips. Absent `retrieval.policy` decodes to full-pass (blocktype.DecodePolicy),
//     and 'damped'/'aggregate-to-parent' are visible types (Set.VisibleTypes) —
//     all of them stay embeddable.
//   - Tenant overlay: the retrieval exclusion in the search path IS reader-aware
//     ("Overlay gewinnt", design/01 §5.4 / D6, pinned by rrf T12) — a tenant may
//     lift a _global exclusion to full-pass, and then blocks of that type DO have
//     a consumer. The backfill has no reader identity (both picks are scope-free,
//     they embed whatever is pending), so the reader-aware rule is mirrored as the
//     UNION over all possible readers: an inner NOT EXISTS keeps the type
//     embeddable as soon as ANY non-_global scope overrides it to a non-excluded
//     policy. Live 2026-07-25 there are exactly 7 rows, all scope='_global'
//     (checkpoint + system-meta excluded), so the inner clause is empty in
//     practice today — it exists so a later tenant override cannot silently
//     starve that tenant's own retrieval.
//   - A block whose type has NO registry row at all stays embeddable (NOT EXISTS
//     over the _global row): the registry, not its absence, narrows visibility.
//
// Defined ONCE (design/04 §3.3 "je einmal definiert in Go-Konstanten") and
// interpolated into the call sites, exactly like its EmbedFailureExcludedPredicate
// sibling; it carries no bind parameters and references context_blocks.type_name,
// so it composes into any query whose outer FROM is the bare (unaliased)
// context_blocks table.
//
// NOT covered on purpose (own waves): the /status `embed_backlog` metric
// (status_db.go) still counts these blocks, so a corpus with parked
// excluded-type blocks shows a permanent backlog floor; and the re-embed
// migration worker (embed_cutover.go) keeps its own predicate.
const RetrievalExcludedTypePredicate = `
	AND NOT EXISTS (
		SELECT 1 FROM context_block_types g
		WHERE g.scope = '_global'
		  AND g.name = context_blocks.type_name
		  AND g.config->'retrieval'->>'policy' = 'excluded'
		  AND NOT EXISTS (
			SELECT 1 FROM context_block_types o
			WHERE o.name = g.name
			  AND o.scope <> '_global'
			  AND o.config->'retrieval'->>'policy' IS DISTINCT FROM 'excluded'
		  )
	)`

const blockTypeCols = `id, name, scope, display_name, description, builtin, is_default, config, created_at, updated_at, updated_by`

func scanBlockType(row pgx.Row) (*BlockTypeRow, error) {
	bt := &BlockTypeRow{}
	err := row.Scan(&bt.ID, &bt.Name, &bt.Scope, &bt.DisplayName, &bt.Description,
		&bt.Builtin, &bt.IsDefault, &bt.Config, &bt.CreatedAt, &bt.UpdatedAt, &bt.UpdatedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: scan block type: %w", err)
	}
	return bt, nil
}

// ListBlockTypes returns the registry rows visible to the caller's namespace
// set (K-T1: the tierOpen list/get gate admits, the HANDLER scopes — it
// passes ['_global'] ∪ own tenant namespace, never a caller-chosen scope).
func ListBlockTypes(ctx context.Context, pool *pgxpool.Pool, scopes []string) ([]BlockTypeRow, error) {
	rows, err := pool.Query(ctx,
		`SELECT `+blockTypeCols+` FROM context_block_types
		  WHERE scope = ANY($1::text[])
		  ORDER BY scope, name`, scopes)
	if err != nil {
		return nil, fmt.Errorf("store: list block types: %w", err)
	}
	defer rows.Close()

	out := make([]BlockTypeRow, 0)
	for rows.Next() {
		bt := BlockTypeRow{}
		if err := rows.Scan(&bt.ID, &bt.Name, &bt.Scope, &bt.DisplayName, &bt.Description,
			&bt.Builtin, &bt.IsDefault, &bt.Config, &bt.CreatedAt, &bt.UpdatedAt, &bt.UpdatedBy); err != nil {
			return nil, fmt.Errorf("store: list block types scan: %w", err)
		}
		out = append(out, bt)
	}
	return out, rows.Err()
}

// GetBlockType resolves one row by UUID or by name, constrained to the
// caller-visible namespace set (same 404-no-oracle shape as the T24 key
// paths: a foreign-scope row reads as "not found"). A name present in
// several visible scopes prefers the non-'_global' row (tier-2 shadowing
// order, inert while only _global rows exist).
func GetBlockType(ctx context.Context, pool *pgxpool.Pool, idOrName string, scopes []string) (*BlockTypeRow, error) {
	if IsFullUUID(idOrName) {
		return scanBlockType(pool.QueryRow(ctx,
			`SELECT `+blockTypeCols+` FROM context_block_types
			  WHERE id = $1::uuid AND scope = ANY($2::text[])`, idOrName, scopes))
	}
	return scanBlockType(pool.QueryRow(ctx,
		`SELECT `+blockTypeCols+` FROM context_block_types
		  WHERE name = $1 AND scope = ANY($2::text[])
		  ORDER BY (scope = '_global') LIMIT 1`, idOrName, scopes))
}

// BlockTypeWrite carries one create/update mutation. Config is the RAW JSONB
// envelope — already validated through blocktype.DecodePolicy by the caller.
type BlockTypeWrite struct {
	Name        string
	Scope       string
	DisplayName string
	Description string
	IsDefault   bool
	Config      json.RawMessage
}

// CreateBlockType inserts a non-builtin registry row inside an attributed
// transaction (audit via='api'). Unique violations map onto the sentinel
// errors above.
func CreateBlockType(ctx context.Context, pool *pgxpool.Pool, in BlockTypeWrite, by *string, requestID string) (*BlockTypeRow, error) {
	var bt *BlockTypeRow
	if err := pgxdb.Write(ctx, pool,
		pgxdb.Stages{Begin: "store: create block type begin", Commit: "store: create block type commit"},
		func(tx pgx.Tx) error {
			if err := stampBlockTypeTx(ctx, tx, by, requestID); err != nil {
				return err
			}
			var err error
			bt, err = scanBlockType(tx.QueryRow(ctx,
				`INSERT INTO context_block_types
				    (name, scope, display_name, description, builtin, is_default, config, updated_by)
				 VALUES ($1, $2, $3, $4, false, $5, $6::jsonb, $7::uuid)
				 RETURNING `+blockTypeCols,
				in.Name, in.Scope, in.DisplayName, in.Description, in.IsDefault, string(in.Config), by))
			if err != nil {
				return mapBlockTypeUnique(err)
			}
			return nil
		}); err != nil {
		return nil, err
	}
	return bt, nil
}

// BlockTypeUpdate carries the PATCH fields of type-update. nil = keep.
// Name/scope/builtin/is_default are deliberately NOT patchable in tier 1:
// name+scope are the row identity (builtin: immutable by doc §3.2), and an
// is_default swap is a two-row transaction that ships with a real consumer.
type BlockTypeUpdate struct {
	DisplayName *string
	Description *string
	Config      json.RawMessage // nil = keep
}

// UpdateBlockType patches one row by id inside an attributed transaction.
// nil result = no row with that id in the given scopes (404-no-oracle).
func UpdateBlockType(ctx context.Context, pool *pgxpool.Pool, id string, scopes []string, in BlockTypeUpdate, by *string, requestID string) (*BlockTypeRow, error) {
	var bt *BlockTypeRow
	if err := pgxdb.Write(ctx, pool,
		pgxdb.Stages{Begin: "store: update block type begin", Commit: "store: update block type commit"},
		func(tx pgx.Tx) error {
			if err := stampBlockTypeTx(ctx, tx, by, requestID); err != nil {
				return err
			}

			set := []string{"updated_at = now()", "updated_by = $1::uuid"}
			args := []any{by}
			idx := 2
			if in.DisplayName != nil {
				set = append(set, fmt.Sprintf("display_name = $%d", idx))
				args = append(args, *in.DisplayName)
				idx++
			}
			if in.Description != nil {
				set = append(set, fmt.Sprintf("description = $%d", idx))
				args = append(args, *in.Description)
				idx++
			}
			if in.Config != nil {
				set = append(set, fmt.Sprintf("config = $%d::jsonb", idx))
				args = append(args, string(in.Config))
				idx++
			}
			args = append(args, id)
			idIdx := idx
			args = append(args, scopes)

			var err error
			bt, err = scanBlockType(tx.QueryRow(ctx, fmt.Sprintf(
				`UPDATE context_block_types SET %s
				  WHERE id = $%d::uuid AND scope = ANY($%d::text[])
				  RETURNING `+blockTypeCols,
				strings.Join(set, ", "), idIdx, idIdx+1), args...))
			if err != nil {
				return err
			}
			if bt == nil {
				return pgxdb.ErrRollback // not found — no audit row, rollback is a no-op
			}
			return nil
		}); err != nil {
		if errors.Is(err, pgxdb.ErrRollback) {
			return nil, nil
		}
		return nil, err
	}
	return bt, nil
}

// DeleteBlockType removes one row by id inside an attributed transaction.
// Guards (§5.1(c)), both checked in the SAME transaction as the delete:
// builtin ⇒ ErrBlockTypeBuiltin (always); referencing blocks — counted over
// ALL rows, archived included — ⇒ *BlockTypeInUseError naming the split.
// found=false ⇒ no row with that id in the given scopes.
func DeleteBlockType(ctx context.Context, pool *pgxpool.Pool, id string, scopes []string, by *string, requestID string) (found bool, err error) {
	if err = pgxdb.Write(ctx, pool,
		pgxdb.Stages{Begin: "store: delete block type begin", Commit: "store: delete block type commit"},
		func(tx pgx.Tx) error {
			if err := stampBlockTypeTx(ctx, tx, by, requestID); err != nil {
				return err
			}

			var name string
			var builtin bool
			err := tx.QueryRow(ctx,
				`SELECT name, builtin FROM context_block_types
				  WHERE id = $1::uuid AND scope = ANY($2::text[]) FOR UPDATE`,
				id, scopes).Scan(&name, &builtin)
			if errors.Is(err, pgx.ErrNoRows) {
				// (false, nil) before the commit in the straight-line form —
				// pgxdb.ErrRollback keeps that a rollback instead of a commit.
				return pgxdb.ErrRollback
			}
			if err != nil {
				return fmt.Errorf("store: delete block type lookup: %w", err)
			}
			if builtin {
				found = true
				return ErrBlockTypeBuiltin
			}

			var active, archived int
			if err := tx.QueryRow(ctx,
				`SELECT count(*) FILTER (WHERE NOT is_archived)::int,
				        count(*) FILTER (WHERE is_archived)::int
				   FROM context_blocks WHERE type_name = $1`, name).Scan(&active, &archived); err != nil {
				return fmt.Errorf("store: delete block type ref count: %w", err)
			}
			if active > 0 || archived > 0 {
				found = true
				return &BlockTypeInUseError{Active: active, Archived: archived}
			}

			if _, err := tx.Exec(ctx, `DELETE FROM context_block_types WHERE id = $1::uuid`, id); err != nil {
				return fmt.Errorf("store: delete block type: %w", err)
			}
			return nil
		}); err != nil {
		if errors.Is(err, pgxdb.ErrRollback) {
			return false, nil
		}
		return found, err
	}
	return true, nil
}

// stampBlockTypeTx applies the audit attribution GUCs (settings.go:249-270
// pattern; third user after settings and backends).
func stampBlockTypeTx(ctx context.Context, tx pgx.Tx, by *string, requestID string) error {
	if err := setTxActor(ctx, tx, by); err != nil {
		return err
	}
	return SetTxRequestID(ctx, tx, requestID)
}

// mapBlockTypeUnique translates the two 23505 shapes onto sentinel errors.
func mapBlockTypeUnique(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		if pgErr.ConstraintName == "uq_block_types_default" {
			return ErrBlockTypeDefaultExists
		}
		return ErrBlockTypeExists
	}
	return err
}
