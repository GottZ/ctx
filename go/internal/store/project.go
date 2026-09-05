// Project registry store layer (workflow W4, design/03-workflow-api-cli.md §3.1;
// masterplan K1 = migration 079, K14 = PruneTenant owns the drain). One row =
// one project = one repo corpus; scope is the data discriminator (Modell C).
//
// The compound create (CreateProject) is the load-bearing piece: it assigns the
// project's tenant scope AND inserts the register row in ONE transaction, so a
// failure AFTER the scope assign rolls the scope back too (atomicity gate,
// §7-W4). Quota is caller-supplied fail-closed (maxScopes threaded through
// AssignTenantScopeTx, identical to handleScopeCreate). Idempotency is keyed on
// (tenant_id, identity): a re-init of the same identity returns the existing row
// and touches NO scope (created=false) — no duplicate, no orphan scope.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/GottZ/ctx/internal/pgxdb"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ProjectRow is one context_projects row (the /api/project wire shape; forge/
// sync_cursor/metadata stay raw JSON — the API shows what the row stores).
//
// TokenSecret is the NAME of a context_secrets row (never the PAT) — it is
// deliberately json:"-" so no list/get response can leak even the ref name via
// the wire shape (the API exposes only token_set=bool via forge-sync-status,
// design/02 §5.4). The remaining sync-state columns (SyncEnabled/PushEnabled/
// LastError/BackoffUntil) are the Achse-02 I-F contract (migration 080).
type ProjectRow struct {
	ID               string          `json:"id"`
	TenantID         string          `json:"tenant_id"`
	Scope            string          `json:"scope"`
	Identity         string          `json:"identity"`
	DisplayName      string          `json:"display_name"`
	Forge            json.RawMessage `json:"forge"`
	WebhookSecretRef *string         `json:"webhook_secret_ref"`
	TokenSecret      *string         `json:"-"` // secret NAME only, never on the wire (§5.4)
	SyncStatus       string          `json:"sync_status"`
	SyncEnabled      bool            `json:"sync_enabled"`
	PushEnabled      bool            `json:"push_enabled"`
	LastSyncAt       *time.Time      `json:"last_sync_at"`
	LastError        *string         `json:"last_error,omitempty"`
	BackoffUntil     *time.Time      `json:"backoff_until,omitempty"`
	SyncCursor       json.RawMessage `json:"sync_cursor"`
	CreatedAt        time.Time       `json:"created_at"`
	CreatedBy        *string         `json:"created_by,omitempty"`
	Metadata         json.RawMessage `json:"metadata"`
}

// Sentinel errors the handler maps onto HTTP statuses. Scope-lifecycle errors
// (ErrScopeExists/ErrScopeQuotaExceeded/ErrTenantNotFound) are REUSED from the
// tenant layer — CreateProject composes AssignTenantScopeTx and surfaces them
// unchanged, so the handler maps ONE error vocabulary (409/429/404).
var (
	// ErrProjectExists is unused as an idempotency signal (the tenant+identity
	// re-init returns created=false, not an error); it exists for a future
	// hard-conflict path and mirrors the store's other 23505 sentinels.
	ErrProjectExists = errors.New("project already exists")
)

// scanProject reads one context_projects row. tenant_id/created_by are cast to
// text so the Go side keeps string UUIDs (consistent with the tenant layer).
// pgx.ErrNoRows collapses to (nil, nil) — the no-oracle 404 contract.
func scanProject(row pgx.Row) (*ProjectRow, error) {
	p := &ProjectRow{}
	err := row.Scan(&p.ID, &p.TenantID, &p.Scope, &p.Identity, &p.DisplayName,
		&p.Forge, &p.WebhookSecretRef, &p.TokenSecret, &p.SyncStatus, &p.SyncEnabled,
		&p.PushEnabled, &p.LastSyncAt, &p.LastError, &p.BackoffUntil, &p.SyncCursor,
		&p.CreatedAt, &p.CreatedBy, &p.Metadata)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: scan project: %w", err)
	}
	return p, nil
}

// projectSelect builds the RETURNING/SELECT column list with tenant_id/created_by
// rendered as text (the ::text casts scanProject expects).
const projectSelect = `id, tenant_id::text, scope, identity, display_name, forge, webhook_secret_ref, token_secret, sync_status, sync_enabled, push_enabled, last_sync_at, last_error, backoff_until, sync_cursor, created_at, created_by::text, metadata`

// GetProjectByID returns one project by id, UNSCOPED — the caller applies
// visibility (scope-read for member GET, tenant-ownership for admin PATCH/
// DELETE). An unknown OR malformed id both collapse to (nil, nil): a raw 22P02
// on a non-UUID path parameter must not become a 500 existence side-channel
// (the same no-oracle contract as GetTenant).
func GetProjectByID(ctx context.Context, pool *pgxpool.Pool, id string) (*ProjectRow, error) {
	if id == "" {
		return nil, nil
	}
	p, err := scanProject(pool.QueryRow(ctx,
		`SELECT `+projectSelect+` FROM context_projects WHERE id = $1::uuid`, id))
	if err != nil {
		if pgxdb.MalformedUUID(err) {
			return nil, nil // malformed id → 404, not 500 (no oracle)
		}
		return nil, err
	}
	return p, nil
}

// ListProjects returns the projects whose scope is in the caller's read set,
// newest first. An optional identity filter (empty = no filter) narrows to a
// single project for the `GET /api/project?identity=` init probe. The scope
// intersection IS the tenant-isolation boundary — a foreign project is invisible
// because its scope is not in ReadScopes (never a caller-chosen scope).
func ListProjects(ctx context.Context, pool *pgxpool.Pool, scopes []string, identity string) ([]ProjectRow, error) {
	rows, err := pool.Query(ctx,
		`SELECT `+projectSelect+` FROM context_projects
		  WHERE scope = ANY($1::text[])
		    AND ($2 = '' OR identity = $2)
		  ORDER BY created_at DESC`, scopes, identity)
	if err != nil {
		return nil, fmt.Errorf("store: list projects: %w", err)
	}
	defer rows.Close()

	out := make([]ProjectRow, 0)
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list projects rows: %w", err)
	}
	return out, nil
}

// CreateProjectParams is the compound-create input. ScopeName is the FULLY
// prefixed scope ('<slug>:<name>', built + length-checked by the handler exactly
// like handleScopeCreate). MaxScopes is the fail-closed quota the handler loaded
// for the binding tenant (nil/-1 = cap-free); it is threaded into
// AssignTenantScopeTx so project-create can NOT bypass the scope quota.
// CreatedBy is the acting api_key id (” → NULL).
type CreateProjectParams struct {
	TenantID    string
	ScopeName   string
	MaxScopes   *int
	Identity    string
	DisplayName string
	Forge       json.RawMessage
	CreatedBy   string
}

// CreateProject is the atomic compound create (§7-W4 atomicity gate). ONE tx:
//
//  1. idempotency: SELECT the (tenant_id, identity) project FOR UPDATE. If it
//     exists, return it with created=false — NO scope is assigned (a re-init
//     leaves no orphan scope) and the caller returns an idempotent 200.
//  2. AssignTenantScopeTx(tx, tenant, scope, maxScopes): registers the scope
//     under the SAME lock+quota+error contract as handleScopeCreate. A quota
//     overrun → ErrScopeQuotaExceeded; a scope-name collision → ErrScopeExists.
//  3. INSERT the register row, tenant_id = the SAME binding tenant the scope was
//     just assigned to (the §3.1 invariant tenant_id = tenant_of(scope), derived
//     in-tx, never from the request).
//
// Because 2 and 3 share the transaction, a failure at 3 (e.g. a created_by that
// no longer references a live key → 23503) rolls the scope from 2 back too — the
// atomicity gate goes RED for any two-call (pool-scope-assign then pool-insert)
// implementation, where the scope would survive the failed insert.
func CreateProject(ctx context.Context, pool *pgxpool.Pool, p CreateProjectParams) (row *ProjectRow, created bool, err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("store: create project begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 1: idempotency on (tenant_id, identity). FOR UPDATE serialises a concurrent
	// re-init of the same identity so two callers can not both pass the check and
	// both assign a scope.
	existing, err := scanProject(tx.QueryRow(ctx,
		`SELECT `+projectSelect+` FROM context_projects
		  WHERE tenant_id = $1::uuid AND identity = $2 FOR UPDATE`, p.TenantID, p.Identity))
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		if err := tx.Commit(ctx); err != nil {
			return nil, false, fmt.Errorf("store: create project idempotent commit: %w", err)
		}
		return existing, false, nil
	}

	// 2: assign the scope (fail-closed quota threaded through). Errors are the
	// tenant-layer sentinels, surfaced unchanged.
	if _, err = AssignTenantScopeTx(ctx, tx, p.TenantID, p.ScopeName, p.MaxScopes); err != nil {
		return nil, false, err
	}

	// 3: insert the register row IN THE SAME TX. created_by '' → NULL (NULLIF +
	// ::uuid); a non-live created_by → 23503 rolls back the scope from step 2.
	var createdBy any
	if p.CreatedBy != "" {
		createdBy = p.CreatedBy
	}
	forge := p.Forge
	if len(forge) == 0 {
		forge = json.RawMessage(`{}`)
	}
	inserted, err := scanProject(tx.QueryRow(ctx,
		`INSERT INTO context_projects (tenant_id, scope, identity, display_name, forge, created_by, created_by_principal)
		 VALUES ($1::uuid, $2, $3, $4, $5::jsonb, $6::uuid,
		         (SELECT k.principal_id FROM context_api_keys k WHERE k.id = $6::uuid))
		 RETURNING `+projectSelect,
		p.TenantID, p.ScopeName, p.Identity, p.DisplayName, forge, createdBy))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			switch pgErr.Code {
			case "23505":
				// uq_projects_scope / uq_projects_identity: the scope from step 2 is
				// fresh, so this is an identity race lost after the FOR UPDATE window.
				return nil, false, ErrProjectExists
			case "23503":
				// created_by → context_api_keys FK: the acting key vanished mid-create.
				return nil, false, fmt.Errorf("store: create project insert fk: %w", err)
			}
		}
		return nil, false, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, false, fmt.Errorf("store: create project commit: %w", err)
	}
	return inserted, true, nil
}

// UpdateProject patches the mutable fields ONLY (display_name, forge). A nil
// argument leaves the column unchanged (COALESCE). scope / tenant_id /
// webhook_secret_ref are NOT reachable here by construction — the handler rejects
// those keys 422 before ever calling this, and this statement does not name them.
// Returns the updated row, or (nil, nil) if the id vanished (404).
func UpdateProject(ctx context.Context, pool *pgxpool.Pool, id string, displayName *string, forge json.RawMessage) (*ProjectRow, error) {
	var forgeArg any
	if len(forge) > 0 {
		forgeArg = string(forge)
	}
	var dnArg any
	if displayName != nil {
		dnArg = *displayName
	}
	p, err := scanProject(pool.QueryRow(ctx,
		`UPDATE context_projects
		    SET display_name = COALESCE($2, display_name),
		        forge        = COALESCE($3::jsonb, forge)
		  WHERE id = $1::uuid
		 RETURNING `+projectSelect, id, dnArg, forgeArg))
	if err != nil {
		return nil, err
	}
	return p, nil
}

// DeleteProject removes the register row (context_project_sync_runs +
// context_webhook_events CASCADE) AND drains the project's own scoped credentials
// (K14 secret-drain, design/03 §5.6 + design/02 §5.2). The project's blocks AND
// its tenant scope survive — scope teardown is a tenant-lifecycle concern
// (design/03 §4.2). Returns whether a register row was deleted.
//
// Secret-drain (W13 project-delete leg — the tenant-prune leg is PruneTenant):
// context_secrets rows carry the scope only as a plain column (no FK, no cascade,
// 051), so a naked register-delete would ORPHAN the sealed webhook HMAC secret
// ('webhook.github.<id>') AND any forge PAT ('forge.token.<id>') in a scope that
// now has no project referencing them — a live credential surviving its project.
// Both are project-id-keyed by construction, so the drain is deterministic. Done
// in ONE tx with the register-delete: a failure rolls the whole delete back.
func DeleteProject(ctx context.Context, pool *pgxpool.Pool, id string) (bool, error) {
	if id == "" {
		return false, nil
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("store: delete project begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Resolve the scope the credentials live in (the project's own scope). A
	// vanished/malformed id ⇒ nothing to delete (idempotent, no oracle).
	var scope string
	err = tx.QueryRow(ctx, `SELECT scope FROM context_projects WHERE id = $1::uuid`, id).Scan(&scope)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, tx.Commit(ctx)
	}
	if err != nil {
		if pgxdb.MalformedUUID(err) {
			return false, tx.Commit(ctx) // malformed id → no row
		}
		return false, fmt.Errorf("store: delete project scope load: %w", err)
	}

	// Drain the project-scoped credentials by their deterministic names BEFORE
	// the register-delete (order is cosmetic — different table, no FK — but keeps
	// the drain readable next to the row it belongs to).
	if _, err := tx.Exec(ctx,
		`DELETE FROM context_secrets WHERE scope = $1 AND name = ANY($2::text[])`,
		scope, []string{WebhookSecretName(id), "forge.token." + id}); err != nil {
		return false, fmt.Errorf("store: delete project secrets: %w", err)
	}

	tag, err := tx.Exec(ctx, `DELETE FROM context_projects WHERE id = $1::uuid`, id)
	if err != nil {
		return false, fmt.Errorf("store: delete project: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("store: delete project commit: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}
