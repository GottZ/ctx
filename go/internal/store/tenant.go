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

// ErrNoScopes is the fail-closed sentinel: a read path resolved an EMPTY scope
// set. It is an error, never silently "all scopes". Callers gate on it via
// RequireScopes (design/01 §5.4).
var ErrNoScopes = errors.New("store: no scopes resolved (fail-closed)")

// RequireScopes enforces fail-closed: an empty scope slice is an error, NOT
// "all scopes". It mirrors the guard that today lives only in rrf.Search
// (search.go:65-67) so the later fail-closed-hardening wave (T07) can spread it
// to SearchBlocks + the manage reads as a per-path second line of defence behind
// the auth choke point (design/01 §5.0/§5.4). T04 only DEFINES the helper; the
// wiring into the read call-sites is T07.
func RequireScopes(scopes []string) error {
	if len(scopes) == 0 {
		return ErrNoScopes
	}
	return nil
}

// TenantScopes returns every scope OWNED by the given tenant (Modell C: a lookup
// on context_tenant_scopes WHERE tenant_id = $1). This is the per-TENANT scope
// set — distinct from a single KEY's read_scopes (built in ctx_auth, migration
// 060): it is the data foundation ("which scopes belong to whom") the visibility
// axis consumes, plus the cross-tenant isolation guarantee that a tenant id NEVER
// resolves to a foreign scope (the PK on context_tenant_scopes.scope ⇒ one scope
// = one tenant, no multi-mapping). It does NOT change how visibility works today
// (design/01 §4.2): SearchBlocks/ctx_rrf already gate on `scope = ANY(...)`; this
// supplies the per-tenant list those paths consume.
//
// An unknown or empty tenant yields an empty slice (NOT an error) — callers MUST
// pass the result through RequireScopes (or rely on the auth choke point) so that
// empty fails CLOSED rather than collapsing to "all scopes" via the PG
// `scope = ANY('{}')` artefact (design/01 §5.0/§5.2). An empty tenantID short-
// circuits before the query (an empty string is not a valid UUID and would raise
// 22P02; semantically it is "no tenant" → no scopes). The result is ORDER BY
// scope for a deterministic, testable order (TenantScopes is a set; the wire-
// ordered read_scopes[0]=home invariant belongs to ctx_auth, not here).
func TenantScopes(ctx context.Context, pool *pgxpool.Pool, tenantID string) ([]string, error) {
	if tenantID == "" {
		return nil, nil
	}

	rows, err := pool.Query(ctx,
		`SELECT scope FROM context_tenant_scopes WHERE tenant_id = $1::uuid ORDER BY scope`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("store: tenant scopes query: %w", err)
	}
	defer rows.Close()

	var scopes []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("store: tenant scopes scan: %w", err)
		}
		scopes = append(scopes, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: tenant scopes rows: %w", err)
	}
	return scopes, nil
}

// Tenant is a row in context_tenants — the owner/management register (059).
type Tenant struct {
	ID          string    `json:"id"`
	Slug        string    `json:"slug"`
	DisplayName string    `json:"display_name"`
	Status      string    `json:"status"` // active | suspended | offboarding (059 CHECK)
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// ErrTenantNotFound is returned by GetTenant/UpdateTenant when no tenant matches
// the id — the 404 path (no existence oracle beyond the admin-gated action).
var ErrTenantNotFound = errors.New("store: tenant not found")

// ErrTenantSlugExists is returned by CreateTenant on a UNIQUE(slug) violation
// (incl. a second 'default') — the 409 path. Rejecting '_'-prefixed slugs (the
// 400 path, reservedSlug) is the caller's job; this enforces only schema gates.
var ErrTenantSlugExists = errors.New("store: tenant slug already exists")

const tenantCols = `id::text, slug, display_name, status, created_at, updated_at`

func scanTenant(row pgx.Row) (*Tenant, error) {
	var t Tenant
	if err := row.Scan(&t.ID, &t.Slug, &t.DisplayName, &t.Status, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return nil, err
	}
	return &t, nil
}

// CreateTenant inserts a new tenant (status defaults to 'active' per 059). A
// duplicate slug — including a second 'default' — raises 23505 and is returned
// as the typed ErrTenantSlugExists (→ 409). Slug-namespace validation
// (reservedSlug, → 400) is the caller's job; this enforces only the schema gates.
func CreateTenant(ctx context.Context, pool *pgxpool.Pool, slug, displayName string) (*Tenant, error) {
	t, err := scanTenant(pool.QueryRow(ctx,
		`INSERT INTO context_tenants (slug, display_name)
		 VALUES ($1, $2)
		 RETURNING `+tenantCols, slug, displayName))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, ErrTenantSlugExists
		}
		return nil, fmt.Errorf("store: create tenant: %w", err)
	}
	return t, nil
}

// ListTenants returns all tenants, newest first.
func ListTenants(ctx context.Context, pool *pgxpool.Pool) ([]Tenant, error) {
	rows, err := pool.Query(ctx, `SELECT `+tenantCols+` FROM context_tenants ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: list tenants: %w", err)
	}
	defer rows.Close()

	var tenants []Tenant
	for rows.Next() {
		t, err := scanTenant(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan tenant: %w", err)
		}
		tenants = append(tenants, *t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list tenants rows: %w", err)
	}
	return tenants, nil
}

// tenantNotFound maps BOTH "no such tenant" cases to ErrTenantNotFound: a
// well-formed-but-absent id (pgx.ErrNoRows) AND a MALFORMED id (22P02 from the
// ::uuid cast on a non-UUID string). Both must surface as the SAME 404 — a raw
// 22P02 → 500 would distinguish "malformed" from "well-formed-but-absent", a
// (weak) existence side-channel and an inconsistent contract.
func tenantNotFound(err error) bool {
	if errors.Is(err, pgx.ErrNoRows) {
		return true
	}
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}

// GetTenant returns one tenant by id, or ErrTenantNotFound. An empty, absent, OR
// malformed id all collapse to ErrTenantNotFound (404, no oracle). The empty
// short-circuit avoids a needless round-trip + the ''::uuid 22P02; a non-empty
// malformed id is caught by tenantNotFound after the cast.
func GetTenant(ctx context.Context, pool *pgxpool.Pool, id string) (*Tenant, error) {
	if id == "" {
		return nil, ErrTenantNotFound
	}
	t, err := scanTenant(pool.QueryRow(ctx,
		`SELECT `+tenantCols+` FROM context_tenants WHERE id = $1::uuid`, id))
	if err != nil {
		if tenantNotFound(err) {
			return nil, ErrTenantNotFound
		}
		return nil, fmt.Errorf("store: get tenant: %w", err)
	}
	return t, nil
}

// UpdateTenant patches status and/or display_name (empty string = leave
// unchanged, COALESCE(NULLIF(...))) and returns the updated row, or
// ErrTenantNotFound. The 059 CHECK is the backstop for an invalid status (the
// handler validates the status domain first → 400). Status='suspended' takes
// effect at the next auth via the 060 ctx_auth status gate; the per-turn cut for
// already-running sessions is E6 (T05c), not this function.
func UpdateTenant(ctx context.Context, pool *pgxpool.Pool, id, status, displayName string) (*Tenant, error) {
	if id == "" {
		return nil, ErrTenantNotFound
	}
	t, err := scanTenant(pool.QueryRow(ctx,
		`UPDATE context_tenants
		    SET status       = COALESCE(NULLIF($2,''), status),
		        display_name = COALESCE(NULLIF($3,''), display_name),
		        updated_at   = now()
		  WHERE id = $1::uuid
		 RETURNING `+tenantCols, id, status, displayName))
	if err != nil {
		if tenantNotFound(err) {
			return nil, ErrTenantNotFound
		}
		return nil, fmt.Errorf("store: update tenant: %w", err)
	}
	return t, nil
}
