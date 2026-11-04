package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/GottZ/ctx/internal/pgxdb"
)

// ModelAlias is one context_model_aliases row (migration 162): the public
// "model" id of the OpenAI-compatible API, the pool role it routes to, the
// sensitivity floor of its chain and its request policy. The store persists
// what it is given; validating the policy (internal/modelalias.ParsePolicy),
// the field tiers and the shadowing floor are the admin action's job.
type ModelAlias struct {
	ID          string
	Scope       string // '_global' or a tenant scope; immutable
	Name        string // the OpenAI model id; immutable
	Kind        string // chat | embedding | rerank; immutable
	Role        string
	Sensitivity string
	Policy      json.RawMessage // a JSON object; nil writes {}
	Description string
	Enabled     bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ModelAliasWrite is the attribution of one alias write: the audit stamp of
// the transaction and, for a write that lowers a sensitivity on purpose, the
// confirm flag the audit row records (ctx.confirm_downgrade, migration 162).
type ModelAliasWrite struct {
	Stamp                       AuditStamp
	ConfirmSensitivityDowngrade bool
}

var (
	// ErrModelAliasImmutable: the write would change name, kind or scope of an
	// alias (or its id, by psql) — refused by the identity trigger of
	// migration 162. The concrete error is a *ModelAliasImmutableError naming
	// the column.
	ErrModelAliasImmutable = errors.New("store: model alias identity is immutable")
	// ErrModelAliasExists: an alias of that name already exists in the scope.
	ErrModelAliasExists = errors.New("store: model alias already exists in the scope")
	// ErrModelAliasScope: the alias's scope lies outside the caller's scopes.
	ErrModelAliasScope = errors.New("store: model alias scope outside the caller's scopes")
)

// ModelAliasImmutableError names the identity column a refused write tried to
// change ("name", "kind" or "scope"). errors.Is(err, ErrModelAliasImmutable).
type ModelAliasImmutableError struct {
	Field string
}

func (e *ModelAliasImmutableError) Error() string {
	return fmt.Sprintf("%v: %s", ErrModelAliasImmutable, e.Field)
}

// Is makes the error match ErrModelAliasImmutable.
func (e *ModelAliasImmutableError) Is(target error) bool { return target == ErrModelAliasImmutable }

// modelAliasIdentityConstraint is the constraint name the identity trigger of
// migration 162 raises its 23514 with (RAISE … USING CONSTRAINT). It — not
// the message text, not the code alone (every CHECK of the table raises
// 23514 too) — tells an identity refusal apart.
const modelAliasIdentityConstraint = "model_aliases_identity"

// modelAliasErr maps the write errors of the table to the store sentinels.
func modelAliasErr(op string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23514" && pgErr.ConstraintName == modelAliasIdentityConstraint {
		return &ModelAliasImmutableError{Field: pgErr.ColumnName}
	}
	if pgxdb.UniqueViolation(err) {
		return ErrModelAliasExists
	}
	return fmt.Errorf("store: %s model alias: %w", op, err)
}

// stampModelAliasWrite stamps the audit attribution of w onto tx. The confirm
// flag is set OR cleared on every write: it belongs to the one write that
// asked for it, not to a later write of the same transaction (unlike the
// actor stamp, which stampAudit never clears).
func stampModelAliasWrite(ctx context.Context, tx pgx.Tx, w ModelAliasWrite) error {
	if err := stampAudit(ctx, tx, w.Stamp); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('ctx.confirm_downgrade', CASE WHEN $1::boolean THEN 'true' ELSE '' END, true)`,
		w.ConfirmSensitivityDowngrade); err != nil {
		return fmt.Errorf("store: model alias confirm flag: %w", err)
	}
	return nil
}

// modelAliasID parses id; a malformed id names no row. Deciding that here,
// before the statement, keeps every 22P02 of the statement itself (a policy
// that is no JSON) an error instead of a "not found" over an aborted
// transaction. The canonical spelling goes to the database.
func modelAliasID(id string) (string, bool) {
	u, err := uuid.Parse(id)
	if err != nil {
		return "", false
	}
	return u.String(), true
}

const modelAliasColumns = `id::text, scope, name, kind, role, sensitivity, policy, description, enabled, created_at, updated_at`

func scanModelAlias(row pgx.Row) (ModelAlias, error) {
	var a ModelAlias
	var policy []byte
	err := row.Scan(&a.ID, &a.Scope, &a.Name, &a.Kind, &a.Role, &a.Sensitivity, &policy,
		&a.Description, &a.Enabled, &a.CreatedAt, &a.UpdatedAt)
	a.Policy = policy
	return a, err
}

func modelAliasPolicy(p json.RawMessage) json.RawMessage {
	if len(p) == 0 {
		return json.RawMessage(`{}`)
	}
	return p
}

// CreateModelAlias inserts a in tx and returns the stored row. scopes is the
// caller's permitted scope set (nil = server-admin, every scope;
// []string{home} = tenant-admin): an alias outside it is refused IN THE
// STATEMENT with ErrModelAliasScope, nothing written. A duplicate (scope,
// name) is ErrModelAliasExists.
func CreateModelAlias(ctx context.Context, tx pgx.Tx, a ModelAlias, scopes []string, w ModelAliasWrite) (ModelAlias, error) {
	if err := stampModelAliasWrite(ctx, tx, w); err != nil {
		return ModelAlias{}, err
	}
	out, err := scanModelAlias(tx.QueryRow(ctx, `
		INSERT INTO context_model_aliases (scope, name, kind, role, sensitivity, policy, description, enabled)
		SELECT $1::varchar, $2::text, $3::text, $4::text, $5::text, $6::jsonb, $7::text, $8::boolean
		 WHERE ($9::text[] IS NULL OR $1::varchar = ANY($9))
		RETURNING `+modelAliasColumns,
		a.Scope, a.Name, a.Kind, a.Role, a.Sensitivity, modelAliasPolicy(a.Policy), a.Description, a.Enabled, scopes))
	if errors.Is(err, pgx.ErrNoRows) {
		return ModelAlias{}, ErrModelAliasScope
	}
	if err != nil {
		return ModelAlias{}, modelAliasErr("create", err)
	}
	return out, nil
}

// UpdateModelAlias writes the full state of a (addressed by a.ID) in tx and
// returns the stored row. found=false when the row does not exist, the id is
// malformed, or its scope lies outside scopes (nil = every scope) — one
// answer for all three, no existence oracle. name, kind and scope are written
// too, so the identity trigger is the one authority: a change of any of them
// is a *ModelAliasImmutableError (errors.Is ErrModelAliasImmutable).
func UpdateModelAlias(ctx context.Context, tx pgx.Tx, a ModelAlias, scopes []string, w ModelAliasWrite) (ModelAlias, bool, error) {
	id, ok := modelAliasID(a.ID)
	if !ok {
		return ModelAlias{}, false, nil
	}
	if err := stampModelAliasWrite(ctx, tx, w); err != nil {
		return ModelAlias{}, false, err
	}
	out, err := scanModelAlias(tx.QueryRow(ctx, `
		UPDATE context_model_aliases
		   SET scope = $2, name = $3, kind = $4, role = $5, sensitivity = $6, policy = $7,
		       description = $8, enabled = $9, updated_at = now()
		 WHERE id = $1::uuid AND ($10::text[] IS NULL OR scope = ANY($10))
		RETURNING `+modelAliasColumns,
		id, a.Scope, a.Name, a.Kind, a.Role, a.Sensitivity, modelAliasPolicy(a.Policy), a.Description, a.Enabled, scopes))
	if errors.Is(err, pgx.ErrNoRows) {
		return ModelAlias{}, false, nil
	}
	if err != nil {
		return ModelAlias{}, false, modelAliasErr("update", err)
	}
	return out, true, nil
}

// DeleteModelAlias removes the alias id in tx and returns the deleted row;
// found=false as in UpdateModelAlias.
func DeleteModelAlias(ctx context.Context, tx pgx.Tx, id string, scopes []string, w ModelAliasWrite) (ModelAlias, bool, error) {
	id, ok := modelAliasID(id)
	if !ok {
		return ModelAlias{}, false, nil
	}
	if err := stampModelAliasWrite(ctx, tx, w); err != nil {
		return ModelAlias{}, false, err
	}
	out, err := scanModelAlias(tx.QueryRow(ctx, `
		DELETE FROM context_model_aliases
		 WHERE id = $1::uuid AND ($2::text[] IS NULL OR scope = ANY($2))
		RETURNING `+modelAliasColumns, id, scopes))
	if errors.Is(err, pgx.ErrNoRows) {
		return ModelAlias{}, false, nil
	}
	if err != nil {
		return ModelAlias{}, false, modelAliasErr("delete", err)
	}
	return out, true, nil
}

// GetModelAlias reads the alias id under the scope predicate; found=false as
// in UpdateModelAlias.
func GetModelAlias(ctx context.Context, q pgxdb.Rower, id string, scopes []string) (ModelAlias, bool, error) {
	id, ok := modelAliasID(id)
	if !ok {
		return ModelAlias{}, false, nil
	}
	out, err := scanModelAlias(q.QueryRow(ctx, `
		SELECT `+modelAliasColumns+` FROM context_model_aliases
		 WHERE id = $1::uuid AND ($2::text[] IS NULL OR scope = ANY($2))`, id, scopes))
	if errors.Is(err, pgx.ErrNoRows) {
		return ModelAlias{}, false, nil
	}
	if err != nil {
		return ModelAlias{}, false, fmt.Errorf("store: get model alias: %w", err)
	}
	return out, true, nil
}

// ListModelAliases reads every alias under the scope predicate (nil = every
// scope), ordered by scope and name in byte order.
func ListModelAliases(ctx context.Context, q pgxdb.Querier, scopes []string) ([]ModelAlias, error) {
	rows, err := q.Query(ctx, `
		SELECT `+modelAliasColumns+` FROM context_model_aliases
		 WHERE ($1::text[] IS NULL OR scope = ANY($1))
		 ORDER BY scope COLLATE "C", name COLLATE "C"`, scopes)
	if err != nil {
		return nil, fmt.Errorf("store: list model aliases: %w", err)
	}
	defer rows.Close()
	var out []ModelAlias
	for rows.Next() {
		a, err := scanModelAlias(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list model aliases: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list model aliases: %w", err)
	}
	return out, nil
}

// LockModelAliasName serializes, until tx ends, every alias write the admin
// action makes under the alias name `name` — in any scope. The shadowing floor
// (design/05 V-02-c: a tenant alias never sits below the sensitivity of the
// _global alias of the same name) compares rows of two scopes; a row lock
// cannot hold that, because the row to compare against may not exist yet (a
// _global alias created while a tenant creates its shadow). A
// transaction-scoped advisory lock on the name can: each writer takes it
// before it reads the other side, so the second writer reads what the first
// committed. psql writes bypass it — psql is outside the write rules.
func LockModelAliasName(ctx context.Context, tx pgx.Tx, name string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('model_alias:' || $1::text))`, name); err != nil {
		return fmt.Errorf("store: model alias name lock: %w", err)
	}
	return nil
}

// GlobalModelAliasSensitivity reads the sensitivity of the _global alias
// name — the floor a tenant alias of that name may not go below.
// found=false when there is none.
func GlobalModelAliasSensitivity(ctx context.Context, q pgxdb.Rower, name string) (string, bool, error) {
	var sens string
	err := q.QueryRow(ctx, `
		SELECT sensitivity FROM context_model_aliases
		 WHERE scope = '_global' AND name = $1`, name).Scan(&sens)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: model alias floor: %w", err)
	}
	return sens, true, nil
}

// ModelAliasShadow is a tenant alias that shadows a _global name.
type ModelAliasShadow struct {
	Scope       string
	Sensitivity string
}

// ModelAliasShadows lists the tenant aliases named name (every scope but
// _global), ordered by scope in byte order — the rows a _global alias of
// that name is the floor of.
func ModelAliasShadows(ctx context.Context, q pgxdb.Querier, name string) ([]ModelAliasShadow, error) {
	rows, err := q.Query(ctx, `
		SELECT scope, sensitivity FROM context_model_aliases
		 WHERE name = $1 AND scope <> '_global'
		 ORDER BY scope COLLATE "C"`, name)
	if err != nil {
		return nil, fmt.Errorf("store: model alias shadows: %w", err)
	}
	defer rows.Close()
	var out []ModelAliasShadow
	for rows.Next() {
		var s ModelAliasShadow
		if err := rows.Scan(&s.Scope, &s.Sensitivity); err != nil {
			return nil, fmt.Errorf("store: model alias shadows: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: model alias shadows: %w", err)
	}
	return out, nil
}
