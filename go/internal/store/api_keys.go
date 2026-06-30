package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultTenantID is the fixed UUID of the single-tenant default tenant that
// the 059 backfill pins to slug 'default' (the value also lives as the DB
// DEFAULT on context_api_keys.tenant_id). It is the canonical home of this
// constant; the handler layer aliases it. The trailing '…0d3fa0' ≈ "default".
const DefaultTenantID = "00000000-0000-0000-0000-0000000d3fa0"

// ApiKey represents a row in context_api_keys (without the plaintext key,
// which is only returned once at creation).
type ApiKey struct {
	ID            string   `json:"id"`
	Label         string   `json:"label"`
	HomeScope     string   `json:"home_scope"`
	AllowedScopes []string `json:"allowed_scopes"`
	// TenantRole is the per-tenant RBAC role (059 column: owner|admin|member,
	// NOT NULL DEFAULT 'member'). Populated by ListApiKeys for the role-badge on
	// the tenant-admin key list (design/05 LÜCKE-1 / TK7a) — an additive read of
	// a column that has existed since 059, no migration. insertApiKeyTx (and thus
	// CreateApiKey) now sets it explicitly and returns it in RETURNING, so a key
	// built on the create path carries its real role ('member' for CreateApiKey).
	TenantRole string     `json:"tenant_role"`
	Active     bool       `json:"active"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

// rowQuerier is the minimal querier satisfied by BOTH *pgxpool.Pool and pgx.Tx
// — each exposes QueryRow with this exact signature. It lets insertApiKeyTx run
// standalone on the pool (CreateApiKey) or compose into a larger bootstrap
// transaction (the upcoming owner-key / quota-gated mint paths) without the
// primitive caring which it received.
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// insertApiKeyTx is the single INSERT path for a new api key: it validates the
// inputs, applies the tenant-aware {shared} default (R-LEAK5), generates and
// hashes the key, and inserts the row with an EXPLICIT tenant_role. q may be a
// *pgxpool.Pool or a pgx.Tx (rowQuerier), so the primitive runs standalone or
// composes into a tenant-bootstrap transaction.
//
// role MUST be a member of the 059 CHECK domain ('owner'|'admin'|'member'); the
// caller passes a fixed store-side string, never raw user input. The INSERT and
// RETURNING now carry tenant_role — additive vs v4.0.1 (whose create path
// omitted the column and returned TenantRole==""); the column has existed since
// 059, so this is SQL-only, NO migration.
func insertApiKeyTx(ctx context.Context, q rowQuerier, label, homeScope string, allowedScopes []string, tenantID, role string) (ApiKey, string, error) {
	if label == "" {
		return ApiKey{}, "", fmt.Errorf("api_keys: label is required")
	}
	if homeScope == "" {
		return ApiKey{}, "", fmt.Errorf("api_keys: home_scope is required")
	}
	if tenantID == "" {
		tenantID = DefaultTenantID
	}
	// Tenant-aware {shared} default (design/01 §3.6 / E3 / R-LEAK5): only the
	// default tenant inherits 'shared' automatically. A foreign tenant's key
	// with no explicit allowed_scopes gets an empty set — never an implicit
	// cross-tenant read into the default tenant's shared blocks. A non-nil empty
	// slice makes the COALESCE below keep '{}' instead of the '{shared}' literal.
	// This decision MUST stay BEFORE the COALESCE: the hard COALESCE '{shared}'
	// fallback only fires for a genuinely nil arg (the default tenant), never
	// softening the foreign-tenant empty set.
	// TENANT-DECISION(shared-scope-owner): shared stays a default-tenant scope
	// (Weg a) — Alt: system-wide scope like _global (Weg b), reversible by
	// rehanging one context_tenant_scopes row.
	if allowedScopes == nil && tenantID != DefaultTenantID {
		allowedScopes = []string{}
	}
	// Scope-format gate (G03): '_'-prefixed scopes are SYSTEM-RESERVED
	// ('_global' = settings identity sentinel, 051). Enforced here as well
	// as in the handler — the store is the last line before the row exists.
	if strings.HasPrefix(homeScope, "_") {
		return ApiKey{}, "", fmt.Errorf("api_keys: scope names starting with '_' are reserved: %s", homeScope)
	}
	for _, s := range allowedScopes {
		if strings.HasPrefix(s, "_") {
			return ApiKey{}, "", fmt.Errorf("api_keys: scope names starting with '_' are reserved: %s", s)
		}
	}

	keyBytes := make([]byte, 32)
	if _, err := rand.Read(keyBytes); err != nil {
		return ApiKey{}, "", fmt.Errorf("api_keys: generate key: %w", err)
	}
	plaintext := hex.EncodeToString(keyBytes)
	h := sha256.Sum256([]byte(plaintext))
	keyHash := hex.EncodeToString(h[:])

	// Migration 014_security_hardening.sql dropped the plaintext api_key
	// column; key_hash is now NOT NULL UNIQUE and the sole identifier for
	// ctx_auth lookups.
	var row ApiKey
	err := q.QueryRow(ctx,
		`INSERT INTO context_api_keys (label, key_hash, home_scope, allowed_scopes, active, tenant_id, tenant_role)
		 VALUES ($1, $2, $3, COALESCE($4::text[], '{shared}'::text[]), true, $5::uuid, $6)
		 RETURNING id, label, home_scope, allowed_scopes, tenant_role, active, last_used_at, created_at`,
		label, keyHash, homeScope, allowedScopes, tenantID, role,
	).Scan(&row.ID, &row.Label, &row.HomeScope, &row.AllowedScopes, &row.TenantRole, &row.Active, &row.LastUsedAt, &row.CreatedAt)
	if err != nil {
		return ApiKey{}, "", fmt.Errorf("api_keys: insert: %w", err)
	}

	return row, plaintext, nil
}

// CreateApiKey generates a new API key, hashes it, and inserts the row.
// Returns the persisted record and the plaintext key (shown once, then only
// the SHA-256 hash is retained server-side).
//
// home_scope is required by API contract (v2.0.0 breaking change): callers
// must pass a non-empty value. The handler enforces this before calling.
// allowedScopes may be nil — see the tenant-aware default in insertApiKeyTx.
//
// tenantID (MT T06, Achse 01-T6) binds the new key to its owning tenant. The
// handler passes the creator's tenant (ar.TenantID); an empty string falls back
// to the default tenant (single-tenant / 059-backfill semantics — never NULL,
// never fail-open, the default tenant IS the incumbent).
//
// The 6-arg signature is UNCHANGED: CreateApiKey is a thin wrapper over
// insertApiKeyTx with role pinned to 'member' (the DB default), so the new keys
// it mints are exactly as before. Owner / quota-gated mints are separate paths
// that pass their own role through the shared primitive.
func CreateApiKey(ctx context.Context, pool *pgxpool.Pool, label, homeScope string, allowedScopes []string, tenantID string) (*ApiKey, string, error) {
	key, plaintext, err := insertApiKeyTx(ctx, pool, label, homeScope, allowedScopes, tenantID, "member")
	if err != nil {
		return nil, "", err
	}
	return &key, plaintext, nil
}

// ListApiKeys returns API key rows ordered by created_at desc, scoped to one
// tenant unless tenantFilter is empty (server-admin → all tenants) and limited
// to active keys unless activeOnly is false.
// Plaintext keys are not returned — callers receive metadata only.
func ListApiKeys(ctx context.Context, pool *pgxpool.Pool, tenantFilter string, activeOnly bool) ([]ApiKey, error) {
	// tenantFilter empty → NULL → no tenant constraint (server-admin, all
	// tenants); set → only that tenant's keys. activeOnly true → only active.
	var tenantArg *string
	if tenantFilter != "" {
		tenantArg = &tenantFilter
	}
	rows, err := pool.Query(ctx,
		`SELECT id, label, home_scope, allowed_scopes, tenant_role, active, last_used_at, created_at
		 FROM context_api_keys
		 WHERE ($1::uuid IS NULL OR tenant_id = $1::uuid)
		   AND (NOT $2::boolean OR active)
		 ORDER BY created_at DESC`,
		tenantArg, activeOnly)
	if err != nil {
		return nil, fmt.Errorf("api_keys: list: %w", err)
	}
	defer rows.Close()

	var keys []ApiKey
	for rows.Next() {
		var k ApiKey
		if err := rows.Scan(&k.ID, &k.Label, &k.HomeScope, &k.AllowedScopes, &k.TenantRole, &k.Active, &k.LastUsedAt, &k.CreatedAt); err != nil {
			return nil, fmt.Errorf("api_keys: scan: %w", err)
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// TenantAPIKeyIDs returns the id of EVERY api key owned by tenantID — active AND
// inactive (a tenant's call history includes calls made by since-revoked keys).
// Used by the per-tenant /api/llmlog filter (T37b, 04-W5 §4.6/§6.4): resolve the
// tenant's keys to a literal uuid[] FIRST, then `api_key_id = ANY($keys)` — not
// `IN (subquery)`, which the planner hash-joins so the apikey index is ignored.
// An empty tenantID yields an empty slice (fail-closed: a caller with no tenant
// gets an empty filter → zero rows, never an unfiltered view).
func TenantAPIKeyIDs(ctx context.Context, pool *pgxpool.Pool, tenantID string) ([]string, error) {
	if tenantID == "" {
		return []string{}, nil
	}
	rows, err := pool.Query(ctx,
		`SELECT id::text FROM context_api_keys WHERE tenant_id = $1::uuid`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("api_keys: tenant key ids: %w", err)
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("api_keys: tenant key id scan: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// DeleteApiKey deactivates an API key by ID (soft delete: sets active=false),
// constrained to the caller's tenant unless the caller is a server-admin.
//
// The tenant constraint lives INSIDE the single UPDATE — atomic and
// TOCTOU-free, no fetch-then-write. A server-admin (isServerAdmin=true) deletes
// any key by id, exactly as before; a non-server-admin deletes only keys whose
// tenant_id matches callerTenant. A miss — wrong tenant, absent, already
// inactive, or a malformed id (22P02) — collapses to found=false with no error,
// so the caller cannot tell a foreign tenant's key from a nonexistent one
// (Leak-Pfad L2, design/05 §5.2). An empty callerTenant for a non-server-admin
// yields a NULL tenant arg, which matches nothing — fail-closed.
func DeleteApiKey(ctx context.Context, pool *pgxpool.Pool, id, callerTenant string, isServerAdmin bool) (bool, error) {
	var tenantArg *string
	if !isServerAdmin && callerTenant != "" {
		tenantArg = &callerTenant
	}
	tag, err := pool.Exec(ctx,
		`UPDATE context_api_keys SET active = false, updated_at = now()
		 WHERE id = $1::uuid AND active = true
		   AND ($2::boolean OR tenant_id = $3::uuid)`,
		id, isServerAdmin, tenantArg,
	)
	if err != nil {
		// A malformed id (invalid uuid text) can't match any key — collapse it
		// to a miss so it's indistinguishable from an absent id (no oracle).
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "22P02" {
			return false, nil
		}
		return false, fmt.Errorf("api_keys: delete: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// ErrLastOwner is returned by UpdateApiKey when a patch would remove the LAST
// active owner of a tenant (demote the sole remaining active owner key off
// 'owner', or deactivate it). The Self-Revoke-Recovery invariant (BE6/D3): a
// tenant ALWAYS keeps >= 1 active owner key, so it can never lock itself out of
// administration. Self-revoke of one's own key stays allowed UNLESS it is the
// last owner. The handler maps this to 409.
var ErrLastOwner = errors.New("store: would remove the last active owner of the tenant")

// ErrOwnerProtected is returned by UpdateApiKey when a caller that may NOT manage
// owner keys (callerMayManageOwner=false — a tenant-admin, not an owner or a
// server-admin) tries to strip owner power from an OWNER key (demote it off
// 'owner', or deactivate it). Owner keys are mutable only by owners and
// server-admins (R-OWNERKEY, design/04 §D-C / AM6(b)): an admin must not be able
// to neutralise the tenant's owner. The handler maps this to 403.
var ErrOwnerProtected = errors.New("store: owner key may only be managed by an owner or server-admin")

// UpdateApiKey mutates the tenant_role and/or active flag of one api key,
// tenant-isolated, owner-invariant-safe, and TOCTOU-free. role / active nil =
// "leave unchanged" (COALESCE in the final UPDATE). It writes ONLY tenant_role +
// active — never is_admin: the server-global admin tier has NO code path here, so
// this action cannot escalate a key to server-admin (design/04 §D-C, AM4).
//
// Authorisation + isolation mirror DeleteApiKey: a non-server-admin reaches only
// keys whose tenant_id == callerTenant; a foreign, absent, or malformed id (22P02)
// resolves to zero rows → (false, nil), indistinguishable from "absent" — no
// existence oracle (Leak-Pfad L2).
//
// Two role-aware guards run under a context_tenants-row FOR UPDATE lock:
//   - (3) Owner-Protection (AM6(b)): a caller without callerMayManageOwner cannot
//     strip owner power from an owner key → ErrOwnerProtected (checked first).
//   - (4) Last-Owner (AM6(a)): a patch that would drop the tenant's active-owner
//     count below one → ErrLastOwner, nothing written.
//
// LOCK TARGET — the SHARED context_tenants row of the target key, NOT a per-key
// lock. This is load-bearing: two concurrent demotions of DIFFERENT owner keys of
// the same tenant touch different api-key rows and would NOT contend on a per-key
// lock (both observe owner-count >= 2, both demote → zero owners = the AM6(a)
// lockout). Serialising on the one tenant row forces the second call to observe
// the first's committed change → it sees the reduced owner set → 409. Every
// UpdateApiKey of a tenant locks the same row first, so there is no lock-ordering
// inversion (no deadlock).
func UpdateApiKey(ctx context.Context, pool *pgxpool.Pool, id, callerTenant string,
	isServerAdmin, callerMayManageOwner bool, role *string, active *bool) (changed bool, err error) {

	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("api_keys: update begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after commit

	// NULL-on-empty inline idiom (ListApiKeys / DeleteApiKey house style): a
	// non-server-admin with an empty callerTenant yields a NULL tenant arg, which
	// matches nothing in the ($isServerAdmin OR tenant_id=$callerTenant) constraint
	// → fail-closed.
	var tenantArg *string
	if !isServerAdmin && callerTenant != "" {
		tenantArg = &callerTenant
	}

	// (1) Resolve the target's tenant + current role/active WITH the tenant
	// constraint. Zero rows — wrong tenant, absent, or a malformed id (22P02) —
	// collapses to (false, nil): no row is touched and the caller cannot tell a
	// foreign key from a nonexistent one (no oracle, mirrors DeleteApiKey:218-242).
	targetTenant, curRole, curActive, found, err := resolveKeyForUpdate(ctx, tx, id, isServerAdmin, tenantArg)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil // foreign / absent / malformed id — no oracle (mirrors DeleteApiKey)
	}

	// (2) Serialisation point: lock the target key's TENANT row (see the lock-target
	// rationale in the doc comment). tenant_id is a NOT NULL FK to context_tenants,
	// so the row is guaranteed to exist.
	var lockedTenant string
	if err = tx.QueryRow(ctx,
		`SELECT id FROM context_tenants WHERE id = $1::uuid FOR UPDATE`, targetTenant).Scan(&lockedTenant); err != nil {
		return false, fmt.Errorf("api_keys: update tenant lock: %w", err)
	}

	// Effective post-patch state of THIS key (nil = unchanged).
	const ownerRole = "owner"
	effRole := curRole
	if role != nil {
		effRole = *role
	}
	effActive := curActive
	if active != nil {
		effActive = *active
	}

	// (3) Owner-Protection (AM6(b)): only an owner / server-admin may strip owner
	// power (demote off 'owner', or deactivate true→false) from an OWNER key. An
	// admin (callerMayManageOwner=false) → 403. Fires only for a same-tenant owner
	// target — a foreign target already collapsed to (false, nil) in (1), before
	// the role was known, so this is no cross-tenant oracle.
	targetIsOwner := curRole == ownerRole
	removesOwnerPower := (role != nil && *role != ownerRole) || (active != nil && !*active && curActive)
	if targetIsOwner && removesOwnerPower && !callerMayManageOwner {
		return false, ErrOwnerProtected
	}

	// (4) Last-Owner (AM6(a)) under the tenant lock: only the transition
	// active-owner → not-active-owner can drop the count. Count the OTHER active
	// owners (id <> target) — directly "does >= 1 owner remain after the patch".
	// Read under the lock, a serialised concurrent demotion of a DIFFERENT owner key
	// is already committed, so its key no longer counts here → the second call sees
	// zero others → ErrLastOwner (the race riegel). Excluding the target by id (vs
	// count-then-minus-one) is robust to a stale self-read.
	wasActiveOwner := curRole == ownerRole && curActive
	willBeActiveOwner := effRole == ownerRole && effActive
	if wasActiveOwner && !willBeActiveOwner {
		var others int
		if err = tx.QueryRow(ctx,
			`SELECT count(*) FROM context_api_keys
			  WHERE tenant_id = $1::uuid AND tenant_role = 'owner' AND active = true AND id <> $2::uuid`,
			targetTenant, id).Scan(&others); err != nil {
			return false, fmt.Errorf("api_keys: update owner count: %w", err)
		}
		if others < 1 {
			return false, ErrLastOwner
		}
	}

	// Apply. Tenant constraint repeated in the WHERE (no cross-tenant patch);
	// COALESCE keeps the existing value for a nil patch field. Writes ONLY
	// tenant_role + active — the SQL names neither is_admin nor any other column.
	tag, err := tx.Exec(ctx,
		`UPDATE context_api_keys
		    SET tenant_role = COALESCE($2::text, tenant_role),
		        active      = COALESCE($3::boolean, active),
		        updated_at  = now()
		  WHERE id = $1::uuid AND ($4::boolean OR tenant_id = $5::uuid)`,
		id, role, active, isServerAdmin, tenantArg,
	)
	if err != nil {
		return false, fmt.Errorf("api_keys: update: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("api_keys: update commit: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// resolveKeyForUpdate fetches the target key's tenant + current role/active under the
// caller's tenant constraint. A foreign, absent, or malformed (22P02) id collapses to
// found=false so the caller cannot distinguish a foreign key from a nonexistent one
// (no oracle, mirrors DeleteApiKey). Split out of UpdateApiKey to keep its cyclomatic
// complexity under the cyclop limit.
func resolveKeyForUpdate(ctx context.Context, q rowQuerier, id string, isServerAdmin bool, tenantArg *string) (targetTenant, role string, active, found bool, err error) {
	err = q.QueryRow(ctx,
		`SELECT tenant_id::text, tenant_role, active
		   FROM context_api_keys
		  WHERE id = $1::uuid AND ($2::boolean OR tenant_id = $3::uuid)`,
		id, isServerAdmin, tenantArg,
	).Scan(&targetTenant, &role, &active)
	if err == nil {
		return targetTenant, role, active, true, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, false, nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "22P02" {
		return "", "", false, false, nil // malformed id → indistinguishable from absent
	}
	return "", "", false, false, fmt.Errorf("api_keys: update resolve: %w", err)
}
