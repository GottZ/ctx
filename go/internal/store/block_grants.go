package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// GrantedBlockIDs resolves the set of block IDs row-level-granted TO a tenant
// (Strategy A, design/07 §4.1): one lookup per request, the result flows as a
// bound uuid[] into the visibility OR-arm. An empty/whitespace tenantID or no
// grants returns a NON-NIL empty slice, so the caller binds '{}'::uuid[] and the
// OR-arm `id = ANY('{}')` is a deterministic FALSE — a byte-identical no-op to
// the scope-only state. Index-backed by idx_block_grants_grantee (067).
//
// TENANT-DECISION(block-grant-resolution): Strategy A (resolved []string bound
// param) for the Go ID/abruf paths in T40a — Alternative B (correlated subquery
// `id IN (SELECT block_id FROM context_block_grants WHERE grantee_tenant=$T)`),
// umentscheidbar weil die VisibilityPredicate-Signatur einen uuid[]-Param traegt
// und der OR-Arm hinter dem Switch-Point gekapselt ist; ctx_rrf (T40b) ist HART
// auf A festgelegt. design/07 §4.1.
//
// TENANT-DECISION(block-grant-empty-scope): konservativ — RequireScopes bleibt aktiv
//   (leeres readScopes wird abgelehnt, auch mit Grants). Alternative: Grant-only-Sicht
//   (RequireScopesOrGrants relaxen) — design/07 §5.3.6 Default-Empfehlung; umentscheidbar
//   weil es ein einzelner Guard am Eingang ist. Gewaehlt: fail-closed + minimal, gueltige
//   Grantees tragen ihren home_scope ohnehin. design/07 §5.3.6 / G5.
func GrantedBlockIDs(ctx context.Context, pool *pgxpool.Pool, tenantID string) ([]string, error) {
	if strings.TrimSpace(tenantID) == "" {
		return []string{}, nil
	}
	rows, err := pool.Query(ctx,
		`SELECT block_id::text FROM context_block_grants WHERE grantee_tenant = $1::uuid`,
		tenantID)
	if err != nil {
		return nil, fmt.Errorf("store: granted block ids: %w", err)
	}
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan granted block id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// BlockGrant is a row in context_block_grants (067): one row-level READ grant —
// grantee_tenant gains read access to a SINGLE block_id that lives in another
// scope/tenant. The write-side management (T43) is the create/list/revoke trio
// below; the read-side OR-arm (T40a/T40b) consumes only the resolved id list via
// GrantedBlockIDs. design/07 §3.1 / §4.7.
type BlockGrant struct {
	ID            string    `json:"id"`
	BlockID       string    `json:"block_id"`
	GranteeTenant string    `json:"grantee_tenant"`
	GrantedBy     *string   `json:"granted_by"` // api_key_id of the granting admin (FK ON DELETE SET NULL → nullable)
	CreatedAt     time.Time `json:"created_at"`
}

// ErrBlockGrantUnknownTarget is the 23503/22P02 path: the block_id is not a
// registered block, OR the grantee_tenant is not a registered tenant, OR either
// id is malformed. ONE typed error — the action does not disclose WHICH half was
// unknown (no block/tenant existence oracle past the admin gate) → 400.
var ErrBlockGrantUnknownTarget = errors.New("store: unknown block or grantee tenant")

// ErrBlockGrantNotFound is the revoke-miss path (0 rows, or a malformed id) → 404.
var ErrBlockGrantNotFound = errors.New("store: block grant not found")

// ErrBlockNotFound is the scope-lookup miss (an absent OR malformed block id).
var ErrBlockNotFound = errors.New("store: block not found")

const blockGrantCols = `id::text, block_id::text, grantee_tenant::text, granted_by::text, created_at`

func scanBlockGrant(row pgx.Row) (*BlockGrant, error) {
	var g BlockGrant
	if err := row.Scan(&g.ID, &g.BlockID, &g.GranteeTenant, &g.GrantedBy, &g.CreatedAt); err != nil {
		return nil, err
	}
	return &g, nil
}

// BlockScope returns the scope of a block by id WITHOUT any visibility gate — the
// ownership-resolution primitive for block-grant-create/revoke (design/07 §5.1):
// the handler then checks block.scope ∈ TenantScopes(caller). An absent OR
// malformed id collapses to ErrBlockNotFound, which the handler maps to the SAME
// 403 as a foreign block — no existence oracle past the admin gate.
func BlockScope(ctx context.Context, pool *pgxpool.Pool, blockID string) (string, error) {
	if strings.TrimSpace(blockID) == "" {
		return "", ErrBlockNotFound
	}
	var scope string
	err := pool.QueryRow(ctx, `SELECT scope FROM context_blocks WHERE id = $1::uuid`, blockID).Scan(&scope)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrBlockNotFound
	}
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "22P02" {
			return "", ErrBlockNotFound // malformed id → same miss as absent (no oracle)
		}
		return "", fmt.Errorf("store: block scope: %w", err)
	}
	return scope, nil
}

// CreateBlockGrant inserts one row-level read grant (block_id → grantee_tenant).
// grantedBy is the granting admin key's id, recorded for provenance (empty →
// NULL). It is IDEMPOTENT: ON CONFLICT (block_id, grantee_tenant) DO NOTHING, and
// a duplicate returns the EXISTING row (no 409 — re-granting is a harmless no-op,
// design/07 §4.7). 23503 (block/grantee FK) or 22P02 (malformed id) →
// ErrBlockGrantUnknownTarget. The OWNERSHIP + cross-tenant opt-in gates live in
// the handler (design/07 §5.1/§5.2) — this enforces only the schema gates.
func CreateBlockGrant(ctx context.Context, pool *pgxpool.Pool, blockID, granteeTenant, grantedBy string) (*BlockGrant, error) {
	var granter any // NULL-safe: an empty granter id must not break the ::uuid cast
	if grantedBy != "" {
		granter = grantedBy
	}
	g, err := scanBlockGrant(pool.QueryRow(ctx,
		`INSERT INTO context_block_grants (block_id, grantee_tenant, granted_by)
		 VALUES ($1::uuid, $2::uuid, $3::uuid)
		 ON CONFLICT (block_id, grantee_tenant) DO NOTHING
		 RETURNING `+blockGrantCols, blockID, granteeTenant, granter))
	if errors.Is(err, pgx.ErrNoRows) {
		// ON CONFLICT DO NOTHING returned no row → the grant already exists. Return
		// it so a re-grant is an idempotent success, not a spurious error.
		return getBlockGrant(ctx, pool, blockID, granteeTenant)
	}
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && (pgErr.Code == "23503" || pgErr.Code == "22P02") {
			return nil, ErrBlockGrantUnknownTarget
		}
		return nil, fmt.Errorf("store: create block grant: %w", err)
	}
	return g, nil
}

// getBlockGrant fetches the existing grant for the idempotent create path.
func getBlockGrant(ctx context.Context, pool *pgxpool.Pool, blockID, granteeTenant string) (*BlockGrant, error) {
	g, err := scanBlockGrant(pool.QueryRow(ctx,
		`SELECT `+blockGrantCols+` FROM context_block_grants WHERE block_id = $1::uuid AND grantee_tenant = $2::uuid`,
		blockID, granteeTenant))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrBlockGrantNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get block grant: %w", err)
	}
	return g, nil
}

// ListBlockGrants returns the grants MADE BY a granting tenant — the grants on
// blocks whose scope is OWNED by grantingTenant (Modell C: context_tenant_scopes).
// This is the owner-side "what have I shared, and to whom" view; it is the only
// list a caller is entitled to (a non-owner gets no global grant-topology oracle).
// An optional blockID narrows to one block. An empty/unknown granting tenant
// yields an empty slice (no rows). Index-backed: ts.tenant_id (PK on
// context_tenant_scopes.scope side via tenant_id index), b.scope (idx_context_scope),
// bg.block_id (uq_block_grant). design/07 §4.7 / §3.1.
func ListBlockGrants(ctx context.Context, pool *pgxpool.Pool, grantingTenant, blockID string) ([]BlockGrant, error) {
	if strings.TrimSpace(grantingTenant) == "" {
		return []BlockGrant{}, nil
	}
	var filter any // NULL = no block filter (list all of the tenant's shared blocks)
	if strings.TrimSpace(blockID) != "" {
		filter = blockID
	}
	rows, err := pool.Query(ctx,
		`SELECT `+selectQualifiedBlockGrantCols+`
		   FROM context_block_grants bg
		   JOIN context_blocks b ON b.id = bg.block_id
		   JOIN context_tenant_scopes ts ON ts.scope = b.scope
		  WHERE ts.tenant_id = $1::uuid
		    AND ($2::uuid IS NULL OR bg.block_id = $2::uuid)
		  ORDER BY bg.created_at DESC`,
		grantingTenant, filter)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "22P02" {
			return nil, ErrBlockGrantUnknownTarget // malformed tenant/block filter → 400, not 500
		}
		return nil, fmt.Errorf("store: list block grants: %w", err)
	}
	defer rows.Close()

	grants := make([]BlockGrant, 0)
	for rows.Next() {
		g, err := scanBlockGrant(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan block grant: %w", err)
		}
		grants = append(grants, *g)
	}
	return grants, rows.Err()
}

// selectQualifiedBlockGrantCols is blockGrantCols qualified with the bg alias for
// the join in ListBlockGrants (the bare blockGrantCols would be ambiguous against
// context_blocks.id / context_tenant_scopes columns).
const selectQualifiedBlockGrantCols = `bg.id::text, bg.block_id::text, bg.grantee_tenant::text, bg.granted_by::text, bg.created_at`

// RevokeBlockGrant deletes one grant by (block_id, grantee_tenant). 0 rows
// (absent) OR a malformed id → ErrBlockGrantNotFound (404, no oracle). Takes
// effect immediately — the grantee's next auth/query re-resolves GrantedBlockIDs
// and the block vanishes from every read path (design/07 §5.6). The OWNERSHIP gate
// (you may only revoke a grant on a block you own) lives in the handler.
func RevokeBlockGrant(ctx context.Context, pool *pgxpool.Pool, blockID, granteeTenant string) error {
	if strings.TrimSpace(blockID) == "" || strings.TrimSpace(granteeTenant) == "" {
		return ErrBlockGrantNotFound
	}
	tag, err := pool.Exec(ctx,
		`DELETE FROM context_block_grants WHERE block_id = $1::uuid AND grantee_tenant = $2::uuid`,
		blockID, granteeTenant)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "22P02" {
			return ErrBlockGrantNotFound // malformed id → same 404 as absent (no oracle)
		}
		return fmt.Errorf("store: revoke block grant: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrBlockGrantNotFound
	}
	return nil
}
