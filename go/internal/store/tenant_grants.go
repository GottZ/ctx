package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TenantGrant is a row in context_tenant_grants (migration 061): one cross-tenant
// READ grant — grantee_tenant gets read access to granted_scope, a scope owned by
// ANOTHER tenant (the friend-tenant channel). It is consumed by ctx_auth (060),
// which unions a key's tenant's grants into read_scopes at auth time, so a
// create/delete takes effect at the grantee's NEXT auth (no running-session
// mutation, design/02 §V4). It NEVER affects the write side (the home_scope write
// gate, context_store.go:99, is unchanged) — read-only sharing.
type TenantGrant struct {
	ID            string    `json:"id"`
	GranteeTenant string    `json:"grantee_tenant"`
	GrantedScope  string    `json:"granted_scope"`
	CreatedAt     time.Time `json:"created_at"`
	CreatedBy     *string   `json:"created_by"` // api_key_id of the admin who created it (nullable — FK ON DELETE SET NULL)
}

// ErrGrantExists is the 23505 path: a (grantee_tenant, granted_scope) pair is
// already granted (uq_tenant_grant) → 409.
var ErrGrantExists = errors.New("store: tenant grant already exists")

// ErrGrantUnknownTarget is the 23503/22P02 path: the grantee tenant is not
// registered (FK → context_tenants), OR the scope is not a registered ownable
// scope (FK → context_tenant_scopes — which by construction excludes the
// '_'-system namespace, so a '_'-scope grant fails closed here even if the
// handler check is bypassed), OR the grantee id is malformed. ONE typed error:
// the action does not disclose WHICH half was unknown (no tenant/scope existence
// oracle past the admin gate) → 400.
var ErrGrantUnknownTarget = errors.New("store: unknown grantee tenant or scope")

// ErrGrantNotFound is the delete-miss path (0 rows, or a malformed id) → 404.
var ErrGrantNotFound = errors.New("store: tenant grant not found")

const tenantGrantCols = `id::text, grantee_tenant::text, granted_scope, created_at, created_by::text`

func scanTenantGrant(row pgx.Row) (*TenantGrant, error) {
	var g TenantGrant
	if err := row.Scan(&g.ID, &g.GranteeTenant, &g.GrantedScope, &g.CreatedAt, &g.CreatedBy); err != nil {
		return nil, err
	}
	return &g, nil
}

// CreateTenantGrant inserts one cross-tenant read grant (the friend-tenant
// action). createdBy is the admin key's id, recorded for provenance (empty →
// NULL). Typed errors: 23505 → ErrGrantExists (409); 23503 (grantee/scope FK) or
// 22P02 (malformed grantee id) → ErrGrantUnknownTarget (400). The granted_scope
// FK to context_tenant_scopes is the fail-closed backstop against granting an
// unregistered — or '_'-system — scope even if the handler's reserved check is
// bypassed.
func CreateTenantGrant(ctx context.Context, pool *pgxpool.Pool, granteeTenant, grantedScope, createdBy string) (*TenantGrant, error) {
	var creator any // NULL-safe: an empty creator id must not break the ::uuid cast
	if createdBy != "" {
		creator = createdBy
	}
	g, err := scanTenantGrant(pool.QueryRow(ctx,
		`INSERT INTO context_tenant_grants (grantee_tenant, granted_scope, created_by)
		 VALUES ($1::uuid, $2, $3::uuid)
		 RETURNING `+tenantGrantCols, granteeTenant, grantedScope, creator))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			switch pgErr.Code {
			case "23505": // uq_tenant_grant
				return nil, ErrGrantExists
			case "23503", "22P02": // FK violation (grantee/scope) or malformed grantee uuid
				return nil, ErrGrantUnknownTarget
			}
		}
		return nil, fmt.Errorf("store: create tenant grant: %w", err)
	}
	return g, nil
}

// ListTenantGrants returns grants newest-first. A non-empty granteeFilter (a
// tenant id) narrows to that grantee's grants ("what can this tenant read across
// the boundary?"); empty lists all (the admin topology view). A malformed filter
// → ErrGrantUnknownTarget (400), not a 500.
func ListTenantGrants(ctx context.Context, pool *pgxpool.Pool, granteeFilter string) ([]TenantGrant, error) {
	var (
		rows pgx.Rows
		err  error
	)
	if granteeFilter != "" {
		rows, err = pool.Query(ctx,
			`SELECT `+tenantGrantCols+` FROM context_tenant_grants WHERE grantee_tenant = $1::uuid ORDER BY created_at DESC`, granteeFilter)
	} else {
		rows, err = pool.Query(ctx,
			`SELECT `+tenantGrantCols+` FROM context_tenant_grants ORDER BY created_at DESC`)
	}
	if err != nil {
		return nil, grantListErr(err)
	}
	defer rows.Close()

	var grants []TenantGrant
	for rows.Next() {
		g, err := scanTenantGrant(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan tenant grant: %w", err)
		}
		grants = append(grants, *g)
	}
	if err := rows.Err(); err != nil {
		return nil, grantListErr(err)
	}
	return grants, nil
}

// grantListErr maps a malformed-uuid filter (22P02, which pgx may surface at
// Query or at rows.Err depending on protocol timing) to ErrGrantUnknownTarget.
func grantListErr(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "22P02" {
		return ErrGrantUnknownTarget
	}
	return fmt.Errorf("store: list tenant grants: %w", err)
}

// DeleteTenantGrant removes one grant by id. 0 rows (absent) OR a malformed id →
// ErrGrantNotFound (404, no oracle — same as the tenant CRUD). The grantee's read
// access to the scope disappears at its next auth (ctx_auth re-resolves).
func DeleteTenantGrant(ctx context.Context, pool *pgxpool.Pool, id string) error {
	if id == "" {
		return ErrGrantNotFound
	}
	tag, err := pool.Exec(ctx, `DELETE FROM context_tenant_grants WHERE id = $1::uuid`, id)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "22P02" {
			return ErrGrantNotFound // malformed id → same 404 as absent (no oracle)
		}
		return fmt.Errorf("store: delete tenant grant: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrGrantNotFound
	}
	return nil
}
