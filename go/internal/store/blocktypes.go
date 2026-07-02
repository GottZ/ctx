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
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: create block type begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	if err := stampBlockTypeTx(ctx, tx, by, requestID); err != nil {
		return nil, err
	}
	bt, err := scanBlockType(tx.QueryRow(ctx,
		`INSERT INTO context_block_types
		    (name, scope, display_name, description, builtin, is_default, config, updated_by)
		 VALUES ($1, $2, $3, $4, false, $5, $6::jsonb, $7::uuid)
		 RETURNING `+blockTypeCols,
		in.Name, in.Scope, in.DisplayName, in.Description, in.IsDefault, string(in.Config), by))
	if err != nil {
		return nil, mapBlockTypeUnique(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("store: create block type commit: %w", err)
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
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: update block type begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	if err := stampBlockTypeTx(ctx, tx, by, requestID); err != nil {
		return nil, err
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

	bt, err := scanBlockType(tx.QueryRow(ctx, fmt.Sprintf(
		`UPDATE context_block_types SET %s
		  WHERE id = $%d::uuid AND scope = ANY($%d::text[])
		  RETURNING `+blockTypeCols,
		strings.Join(set, ", "), idIdx, idIdx+1), args...))
	if err != nil {
		return nil, err
	}
	if bt == nil {
		return nil, nil // not found — no audit row, rollback is a no-op
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("store: update block type commit: %w", err)
	}
	return bt, nil
}

// DeleteBlockType removes one row by id inside an attributed transaction.
// Guards (§5.1(c)), both checked in the SAME transaction as the delete:
// builtin ⇒ ErrBlockTypeBuiltin (always); referencing blocks — counted over
// ALL rows, archived included — ⇒ *BlockTypeInUseError naming the split.
// found=false ⇒ no row with that id in the given scopes.
func DeleteBlockType(ctx context.Context, pool *pgxpool.Pool, id string, scopes []string, by *string, requestID string) (found bool, err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("store: delete block type begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	if err := stampBlockTypeTx(ctx, tx, by, requestID); err != nil {
		return false, err
	}

	var name string
	var builtin bool
	err = tx.QueryRow(ctx,
		`SELECT name, builtin FROM context_block_types
		  WHERE id = $1::uuid AND scope = ANY($2::text[]) FOR UPDATE`,
		id, scopes).Scan(&name, &builtin)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: delete block type lookup: %w", err)
	}
	if builtin {
		return true, ErrBlockTypeBuiltin
	}

	var active, archived int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE NOT is_archived)::int,
		        count(*) FILTER (WHERE is_archived)::int
		   FROM context_blocks WHERE type_name = $1`, name).Scan(&active, &archived); err != nil {
		return false, fmt.Errorf("store: delete block type ref count: %w", err)
	}
	if active > 0 || archived > 0 {
		return true, &BlockTypeInUseError{Active: active, Archived: archived}
	}

	if _, err := tx.Exec(ctx, `DELETE FROM context_block_types WHERE id = $1::uuid`, id); err != nil {
		return false, fmt.Errorf("store: delete block type: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("store: delete block type commit: %w", err)
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
