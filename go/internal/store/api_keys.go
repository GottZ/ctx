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

	"github.com/GottZ/ctx/internal/pgxdb"
	"github.com/jackc/pgx/v5"
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
	TenantRole string `json:"tenant_role"`
	// WriteScopes is the explicit per-key write-scope set (078, E4b): scopes the
	// key may WRITE blocks to beyond its home_scope. Empty for every existing key
	// (column DEFAULT '{}'). The invariant write_scopes ⊆ allowed_scopes ∪
	// {home_scope} is enforced at mint/update (validateWriteScopes) AND at the
	// read-time write gate (handler.writableBlockScopes) — double, fail-closed.
	WriteScopes []string   `json:"write_scopes"`
	Active      bool       `json:"active"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

// ErrWriteScopeNotAllowed is returned by the mint/update paths when a requested
// write_scope is not a subset of allowed_scopes ∪ {home_scope} (078, E4b invariant,
// enforcement path (a)). A write_scope with no matching read right would be a
// blind-writer; the handler maps this to 400. This is the Go-side gate (no DB CHECK,
// v2.0.0 line) — the read-time writableBlockScopes intersection is the second,
// independent enforcement (path (b)) that also neutralises entries left stale by a
// later allowed_scopes shrink.
var ErrWriteScopeNotAllowed = errors.New("store: write_scope not in allowed_scopes ∪ home_scope")

// validateWriteScopes enforces write_scopes ⊆ allowed_scopes ∪ {home_scope} and the
// '_'-reserved namespace, the mint/update-time half of the 078 double invariant. A
// nil writeScopes (unchanged / none) passes trivially. It returns the FIRST
// offending scope wrapped so the caller can surface a precise 400, or nil when every
// element is a subset of the readable set.
func validateWriteScopes(homeScope string, allowedScopes, writeScopes []string) error {
	if len(writeScopes) == 0 {
		return nil
	}
	readable := make(map[string]struct{}, len(allowedScopes)+1)
	readable[homeScope] = struct{}{}
	for _, s := range allowedScopes {
		readable[s] = struct{}{}
	}
	for _, ws := range writeScopes {
		if strings.HasPrefix(ws, "_") {
			return fmt.Errorf("api_keys: scope names starting with '_' are reserved: %s", ws)
		}
		if _, ok := readable[ws]; !ok {
			return fmt.Errorf("%w: %s", ErrWriteScopeNotAllowed, ws)
		}
	}
	return nil
}

// insertApiKeyTx is the single INSERT path for a new api key: it validates the
// inputs, applies the tenant-aware {shared} default (R-LEAK5), generates and
// hashes the key, and inserts the row with an EXPLICIT tenant_role. q may be a
// *pgxpool.Pool or a pgx.Tx (both are a pgxdb.Rower), so the primitive runs
// standalone or composes into a tenant-bootstrap transaction.
//
// role MUST be a member of the 059 CHECK domain ('owner'|'admin'|'member'); the
// caller passes a fixed store-side string, never raw user input. The INSERT and
// RETURNING now carry tenant_role — additive vs v4.0.1 (whose create path
// omitted the column and returned TenantRole==""); the column has existed since
// 059, so this is SQL-only, NO migration.
func insertApiKeyTx(ctx context.Context, q pgxdb.Rower, label, homeScope string, allowedScopes, writeScopes []string, tenantID, role string) (ApiKey, string, error) {
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
	// write_scopes ⊆ allowed_scopes ∪ {home_scope} (078, E4b enforcement path (a)).
	// Runs BEFORE key generation / INSERT (mirrors the reserved-scope gates above),
	// so a nil-querier unit test early-returns and the store is the last line before
	// a blind-writer row could exist. The intersection at writableBlockScopes is the
	// independent second half (path (b)).
	if err := validateWriteScopes(homeScope, allowedScopes, writeScopes); err != nil {
		return ApiKey{}, "", err
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
	// write_scopes (078): a nil arg persists '{}' (COALESCE) — the pausability
	// default. A non-nil set was already validated ⊆ allowed_scopes ∪ {home_scope}
	// by validateWriteScopes above (defense-in-depth, last line before the row).
	// principal_id (094/F4): every mint creates a FRESH principal named after
	// the key label — the same 1-key-1-principal semantics as the 094 backfill.
	// One atomic statement (data-modifying CTE): a failed key INSERT rolls the
	// principal back with it, no rights-less orphan row. Linking a new key to
	// an EXISTING principal (second key of a person) is a later, explicit flow
	// (Achse 05 key selector / 04 SSO onboarding) — never an implicit default.
	var row ApiKey
	err := q.QueryRow(ctx,
		`WITH p AS (
		     INSERT INTO context_principals (display_name) VALUES ($1) RETURNING id
		 )
		 INSERT INTO context_api_keys (label, key_hash, home_scope, allowed_scopes, write_scopes, active, tenant_id, tenant_role, principal_id)
		 SELECT $1, $2, $3, COALESCE($4::text[], '{shared}'::text[]), COALESCE($5::text[], '{}'::text[]), true, $6::uuid, $7, p.id FROM p
		 RETURNING id, label, home_scope, allowed_scopes, write_scopes, tenant_role, active, last_used_at, created_at`,
		label, keyHash, homeScope, allowedScopes, writeScopes, tenantID, role,
	).Scan(&row.ID, &row.Label, &row.HomeScope, &row.AllowedScopes, &row.WriteScopes, &row.TenantRole, &row.Active, &row.LastUsedAt, &row.CreatedAt)
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
	key, plaintext, err := insertApiKeyTx(ctx, pool, label, homeScope, allowedScopes, nil, tenantID, "member")
	if err != nil {
		return nil, "", err
	}
	return &key, plaintext, nil
}

// ErrKeyQuotaExceeded is returned by MintKeyWithQuota when the tenant already
// owns >= max_keys ACTIVE keys — the 429 path. The count and the insert run
// inside one transaction under a context_tenants-row FOR UPDATE lock, so the cap
// is race-tight (no TOCTOU); a nil or negative maxKeys means unlimited (the cap is
// skipped). The count is ACTIVE-ONLY by design (S3b, design/04 §D-B): a
// soft-deleted (active=false) key must NOT permanently consume a max_keys slot, so
// a create→revoke→create rotation reclaims its budget. The handler maps this to 429.
var ErrKeyQuotaExceeded = errors.New("store: tenant key quota exceeded")

// MintKeyWithQuota mints a new api key bound to tenantID, race-gated and
// quota-capped, in ONE transaction. It is the store mechanic behind the BE6
// self-service api-key-create action (called with role="member") — the cap it
// enforces is what makes max_keys a HARD structural limit, unlike the fail-open,
// synthesis-only QuotaAccountant gate (design/04 §Quota).
//
// Race gate (identical to AssignTenantScope): `SELECT id FROM context_tenants
// WHERE id=$1 FOR UPDATE` locks the OWNING tenant's row, serialising ALL concurrent
// mints of THIS tenant. The count→insert then runs under that lock, so two parallel
// mints can NOT both observe count < max_keys and both commit (no TOCTOU). The lock
// doubles as the existence check: no row (or a malformed id → 22P02) collapses to
// ErrTenantNotFound, reusing pgxdb.AbsentOrMalformed's no-oracle 404 contract.
//
// Quota: maxKeys == nil OR *maxKeys < 0 means UNLIMITED (cap skipped). Otherwise
// count(*) of the tenant's ACTIVE keys is read under the lock; cnt >= *maxKeys →
// ErrKeyQuotaExceeded. The count is `AND active = true` on purpose (S3b): a revoked
// key never permanently burns a slot. insertApiKeyTx runs on the SAME tx, so the
// count and the insert are one atomic unit. role MUST be a member of the 059 CHECK
// domain; self-service passes a fixed "member" (owner is the MintOwnerKey path).
func MintKeyWithQuota(ctx context.Context, pool *pgxpool.Pool, label, homeScope string, allowedScopes, writeScopes []string, tenantID, role string, maxKeys *int) (ApiKey, string, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return ApiKey{}, "", fmt.Errorf("api_keys: mint begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Lock the owning tenant row: per-tenant serialisation + existence check in one
	// step. No row (or a malformed id → 22P02) → ErrTenantNotFound (404, no oracle).
	var lockedID string
	if err = tx.QueryRow(ctx,
		`SELECT id FROM context_tenants WHERE id = $1::uuid FOR UPDATE`, tenantID).Scan(&lockedID); err != nil {
		if pgxdb.AbsentOrMalformed(err) {
			return ApiKey{}, "", ErrTenantNotFound
		}
		return ApiKey{}, "", fmt.Errorf("api_keys: mint tenant lock: %w", err)
	}

	// Quota cap (nil / negative = unlimited). ACTIVE-only count under the lock so it
	// observes any competing mint's committed insert — race-tight — AND so revoked
	// keys do not permanently consume the budget (S3b).
	if maxKeys != nil && *maxKeys >= 0 {
		var cnt int
		if err = tx.QueryRow(ctx,
			`SELECT count(*) FROM context_api_keys WHERE tenant_id = $1::uuid AND active = true`, tenantID).Scan(&cnt); err != nil {
			return ApiKey{}, "", fmt.Errorf("api_keys: mint count: %w", err)
		}
		if cnt >= *maxKeys {
			return ApiKey{}, "", ErrKeyQuotaExceeded
		}
	}

	key, plaintext, err := insertApiKeyTx(ctx, tx, label, homeScope, allowedScopes, writeScopes, tenantID, role)
	if err != nil {
		return ApiKey{}, "", err
	}
	if err = tx.Commit(ctx); err != nil {
		return ApiKey{}, "", fmt.Errorf("api_keys: mint commit: %w", err)
	}
	return key, plaintext, nil
}

// MintOwnerKey mints an OWNER key bound to tenantID with NO quota check and NO
// transaction of its own — the single sanctioned path to role='owner'. q may be a
// *pgxpool.Pool OR a pgx.Tx (both are a pgxdb.Rower), so the server-admin
// tenant-create bootstrap can compose it INTO its own CreateTenant transaction:
// the tenant row and its first owner key then commit together or not at all.
//
// It deliberately bypasses MintKeyWithQuota's cap: the bootstrap owner key is the
// tenant's FIRST key and would otherwise wedge on a max_keys=0 default. This is the
// ONLY path that mints 'owner' on create — api-key-create is hard-'member'
// (MintKeyWithQuota), so an owner role can arise only here or via the owner-gated
// api-key-update (design/04 §D-B point 4, closing AM3). No cap, no tenant-row lock:
// the caller owns the transaction scope.
func MintOwnerKey(ctx context.Context, q pgxdb.Rower, label, homeScope string, allowedScopes []string, tenantID string) (ApiKey, string, error) {
	// Owner keys mint with empty write_scopes (nil): an owner's home_scope IS the
	// tenant's first scope and covers the bootstrap corpus; explicit write_scopes
	// are a later, per-key concern via api-key-update.
	return insertApiKeyTx(ctx, q, label, homeScope, allowedScopes, nil, tenantID, "owner")
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
		`SELECT id, label, home_scope, allowed_scopes, write_scopes, tenant_role, active, last_used_at, created_at
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
		if err := rows.Scan(&k.ID, &k.Label, &k.HomeScope, &k.AllowedScopes, &k.WriteScopes, &k.TenantRole, &k.Active, &k.LastUsedAt, &k.CreatedAt); err != nil {
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

// DeleteApiKey soft-deletes (active=false) one api key by ID — the REAL revoke
// path behind api-key-delete — tenant-isolated, owner-invariant-safe, and
// TOCTOU-free. It MIRRORS UpdateApiKey with the patch FIXED to active=false: the
// same two role-aware guards run under a context_tenants-row FOR UPDATE lock
// before the soft-delete. They MUST live here too, because api-key-delete is
// tierTenantAdmin (owner AND admin pass) and revoke is the path an attacker
// actually reaches — the riegel at api-key-update alone would sit on the stilled
// path (design/04 §D-D, AM6, Review-Major):
//   - (3) Owner-Protection (AM6(b)): an admin (callerMayManageOwner=false) cannot
//     revoke an OWNER key → ErrOwnerProtected (the handler maps it to 403).
//   - (4) Last-Owner (AM6(a)): revoking the last ACTIVE owner → ErrLastOwner (409),
//     nothing written — the tenant always keeps >= 1 active owner (Recovery, D3).
//
// No-oracle (Leak-Pfad L2) preserved: a foreign / absent / malformed (22P02) id
// resolves to zero rows → (false, nil), indistinguishable from "absent", BEFORE
// the role is known — so the owner-protection 403 fires only for a SAME-tenant
// owner target, never as a cross-tenant existence oracle. Re-deleting an already
// inactive key is a no-op miss: the patch removes no owner power (curActive=false),
// so NEITHER guard fires, and the final UPDATE's `active = true` predicate matches
// nothing → false (idempotency preserved). The lock target is the SHARED
// context_tenants row (not a per-key lock), so two concurrent revokes of DIFFERENT
// owner keys serialise → the second sees the committed revoke and gets ErrLastOwner
// (a per-key lock would let both through → zero owners). An empty callerTenant for a
// non-server-admin yields a NULL tenant arg → matches nothing (fail-closed).
func DeleteApiKey(ctx context.Context, pool *pgxpool.Pool, id, callerTenant string,
	isServerAdmin, callerMayManageOwner bool) (bool, error) {

	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("api_keys: delete begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after commit

	// NULL-on-empty inline idiom (ListApiKeys / UpdateApiKey house style): a
	// non-server-admin with an empty callerTenant yields a NULL tenant arg →
	// fail-closed in the ($isServerAdmin OR tenant_id=$callerTenant) constraint.
	var tenantArg *string
	if !isServerAdmin && callerTenant != "" {
		tenantArg = &callerTenant
	}

	// (1) Resolve the target's tenant + current role/active WITH the tenant
	// constraint (the shared helper with UpdateApiKey). Zero rows — wrong tenant,
	// absent, or a malformed id (22P02) — collapses to (false, nil): no row is
	// touched and the caller cannot tell a foreign key from a nonexistent one.
	rk, found, err := resolveKeyForUpdate(ctx, tx, id, isServerAdmin, tenantArg)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil // foreign / absent / malformed id — no oracle (L2)
	}
	targetTenant, curRole, curActive := rk.tenant, rk.role, rk.active

	// (2) Serialisation point: lock the target key's TENANT row (see UpdateApiKey's
	// lock-target rationale). tenant_id is a NOT NULL FK, so the row is guaranteed.
	var lockedTenant string
	if err = tx.QueryRow(ctx,
		`SELECT id FROM context_tenants WHERE id = $1::uuid FOR UPDATE`, targetTenant).Scan(&lockedTenant); err != nil {
		return false, fmt.Errorf("api_keys: delete tenant lock: %w", err)
	}

	const ownerRole = "owner"
	// A revoke removes owner power only when the target is currently active (active
	// true→false); re-revoking an already-inactive key removes nothing → no guard.
	removesOwnerPower := curActive

	// (3) Owner-Protection (AM6(b)): only an owner / server-admin may revoke an
	// OWNER key. An admin (callerMayManageOwner=false) → ErrOwnerProtected. Fires
	// only for a same-tenant owner target — a foreign target already collapsed to
	// (false, nil) in (1), before the role was known (no cross-tenant oracle).
	if curRole == ownerRole && removesOwnerPower && !callerMayManageOwner {
		return false, ErrOwnerProtected
	}

	// (4) Last-Owner (AM6(a)) under the lock: revoking an ACTIVE owner key may drop
	// the active-owner count below one. Count the OTHER active owners (id <> target)
	// — "does >= 1 owner remain after the revoke". Read under the lock, a serialised
	// concurrent revoke of a DIFFERENT owner key is already committed → the second
	// call sees zero others → ErrLastOwner (the race riegel).
	if curRole == ownerRole && curActive {
		var others int
		if err = tx.QueryRow(ctx,
			`SELECT count(*) FROM context_api_keys
			  WHERE tenant_id = $1::uuid AND tenant_role = 'owner' AND active = true AND id <> $2::uuid`,
			targetTenant, id).Scan(&others); err != nil {
			return false, fmt.Errorf("api_keys: delete owner count: %w", err)
		}
		if others < 1 {
			return false, ErrLastOwner
		}
	}

	// (5) Soft-delete. Tenant constraint repeated in the WHERE (no cross-tenant
	// revoke); `active = true` keeps re-deletes idempotent (already-inactive → 0
	// rows → false). The 22P02 case is already absorbed by resolveKeyForUpdate, so
	// this Exec runs only on a well-formed, resolved id.
	tag, err := tx.Exec(ctx,
		`UPDATE context_api_keys SET active = false, updated_at = now()
		  WHERE id = $1::uuid AND active = true
		    AND ($2::boolean OR tenant_id = $3::uuid)`,
		id, isServerAdmin, tenantArg,
	)
	if err != nil {
		return false, fmt.Errorf("api_keys: delete: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("api_keys: delete commit: %w", err)
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
// On success the final UPDATE RETURNs the patched row, so the caller gets it back
// as (updated, true, nil) and need not re-read for its {success, key} response. A
// no-op miss (foreign / absent / malformed id, or a guard rollback) is (ApiKey{},
// false, <err-or-nil>) — the empty ApiKey carries no oracle.
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
	isServerAdmin, callerMayManageOwner bool, role *string, active *bool, writeScopes *[]string) (updated ApiKey, changed bool, err error) {

	tx, err := pool.Begin(ctx)
	if err != nil {
		return ApiKey{}, false, fmt.Errorf("api_keys: update begin: %w", err)
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
	rk, found, err := resolveKeyForUpdate(ctx, tx, id, isServerAdmin, tenantArg)
	if err != nil {
		return ApiKey{}, false, err
	}
	if !found {
		return ApiKey{}, false, nil // foreign / absent / malformed id — no oracle (mirrors DeleteApiKey)
	}
	targetTenant := rk.tenant

	// (2a) write_scopes ⊆ allowed_scopes ∪ {home_scope} (078, E4b, enforcement path
	// (a) on update). Validated against the key's EXISTING read set — home_scope and
	// allowed_scopes are NOT mutable via this action, so the resolved row is the
	// authority. A write_scope with no read right is a blind-writer → rejected; the
	// '_'-reserved namespace is rejected too (same rule as home/allowed). nil = leave
	// unchanged → passes trivially. Checked under the tenant lock, before the write.
	if writeScopes != nil {
		if verr := validateWriteScopes(rk.homeScope, rk.allowedScopes, *writeScopes); verr != nil {
			return ApiKey{}, false, verr
		}
	}

	// (2) Serialisation point: lock the target key's TENANT row (see the lock-target
	// rationale in the doc comment). tenant_id is a NOT NULL FK to context_tenants,
	// so the row is guaranteed to exist.
	var lockedTenant string
	if err = tx.QueryRow(ctx,
		`SELECT id FROM context_tenants WHERE id = $1::uuid FOR UPDATE`, targetTenant).Scan(&lockedTenant); err != nil {
		return ApiKey{}, false, fmt.Errorf("api_keys: update tenant lock: %w", err)
	}

	// (3)+(4) AM6 owner guards under the tenant lock: Owner-Protection (an admin may
	// not strip owner power) and Last-Owner (never drop the tenant's active-owner count
	// below one). Extracted to keep this function under the cyclop ceiling.
	if err = enforceOwnerInvariantsTx(ctx, tx, rk, id, role, active, callerMayManageOwner); err != nil {
		return ApiKey{}, false, err
	}

	// Apply. Tenant constraint repeated in the WHERE (no cross-tenant patch);
	// COALESCE keeps the existing value for a nil patch field. Writes ONLY
	// tenant_role + active — the SQL names neither is_admin nor any other column.
	// RETURNING hands the patched row straight back so the handler need not re-read
	// it for the {success, key} response. The row was resolved (found) and its
	// tenant locked above, so this UPDATE matches exactly one row; an ErrNoRows
	// (the row vanished post-resolve — impossible under the lock) degrades to the
	// same no-oracle miss as !found: (ApiKey{}, false, nil).
	// write_scopes patch: nil → COALESCE keeps the existing value (unchanged); a
	// non-nil slice (incl. empty, to CLEAR the set) replaces it. pgx encodes a nil
	// *[]string as NULL and a non-nil *[]string as the array, so COALESCE($6,…)
	// distinguishes "leave" from "set/clear" cleanly.
	var wsArg any
	if writeScopes != nil {
		wsArg = *writeScopes
	}
	var row ApiKey
	err = tx.QueryRow(ctx,
		`UPDATE context_api_keys
		    SET tenant_role  = COALESCE($2::text, tenant_role),
		        active       = COALESCE($3::boolean, active),
		        write_scopes = COALESCE($6::text[], write_scopes),
		        updated_at   = now()
		  WHERE id = $1::uuid AND ($4::boolean OR tenant_id = $5::uuid)
		  RETURNING id, label, home_scope, allowed_scopes, write_scopes, tenant_role, active, last_used_at, created_at`,
		id, role, active, isServerAdmin, tenantArg, wsArg,
	).Scan(&row.ID, &row.Label, &row.HomeScope, &row.AllowedScopes, &row.WriteScopes, &row.TenantRole, &row.Active, &row.LastUsedAt, &row.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ApiKey{}, false, nil
		}
		return ApiKey{}, false, fmt.Errorf("api_keys: update: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return ApiKey{}, false, fmt.Errorf("api_keys: update commit: %w", err)
	}
	return row, true, nil
}

// enforceOwnerInvariantsTx runs the AM6 owner guards for an api-key patch under the
// already-held tenant lock: (3) Owner-Protection (only an owner / server-admin may
// strip owner power from an OWNER key) and (4) Last-Owner (a patch must never drop the
// tenant's active-owner count below one). rk is the resolved target; role/active are
// the patch (nil = unchanged). Returns ErrOwnerProtected / ErrLastOwner, a wrapped
// query error, or nil when the patch is owner-safe. Extracted from UpdateApiKey to
// keep it under the cyclop ceiling; the owner-count read runs under the caller's lock,
// so the last-owner race riegel is preserved (a serialised concurrent demotion is
// already committed and no longer counts).
func enforceOwnerInvariantsTx(ctx context.Context, tx pgx.Tx, rk resolvedKey, id string,
	role *string, active *bool, callerMayManageOwner bool) error {
	const ownerRole = "owner"
	effRole := rk.role
	if role != nil {
		effRole = *role
	}
	effActive := rk.active
	if active != nil {
		effActive = *active
	}
	// (3) Owner-Protection (AM6(b)): fires only for a same-tenant owner target — a
	// foreign target already collapsed to !found before the role was known (no oracle).
	targetIsOwner := rk.role == ownerRole
	removesOwnerPower := (role != nil && *role != ownerRole) || (active != nil && !*active && rk.active)
	if targetIsOwner && removesOwnerPower && !callerMayManageOwner {
		return ErrOwnerProtected
	}
	// (4) Last-Owner (AM6(a)): only the active-owner → not-active-owner transition can
	// drop the count. Count the OTHER active owners (id <> target) — "does >= 1 owner
	// remain after the patch". Excluding the target by id is robust to a stale self-read.
	wasActiveOwner := rk.role == ownerRole && rk.active
	willBeActiveOwner := effRole == ownerRole && effActive
	if wasActiveOwner && !willBeActiveOwner {
		var others int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM context_api_keys
			  WHERE tenant_id = $1::uuid AND tenant_role = 'owner' AND active = true AND id <> $2::uuid`,
			rk.tenant, id).Scan(&others); err != nil {
			return fmt.Errorf("api_keys: update owner count: %w", err)
		}
		if others < 1 {
			return ErrLastOwner
		}
	}
	return nil
}

// resolveKeyForUpdate fetches the target key's tenant + current role/active under the
// caller's tenant constraint. A foreign, absent, or malformed (22P02) id collapses to
// found=false so the caller cannot distinguish a foreign key from a nonexistent one
// (no oracle, mirrors DeleteApiKey). Split out of UpdateApiKey to keep its cyclomatic
// complexity under the cyclop limit.
func resolveKeyForUpdate(ctx context.Context, q pgxdb.Rower, id string, isServerAdmin bool, tenantArg *string) (r resolvedKey, found bool, err error) {
	err = q.QueryRow(ctx,
		`SELECT tenant_id::text, tenant_role, active, home_scope, allowed_scopes
		   FROM context_api_keys
		  WHERE id = $1::uuid AND ($2::boolean OR tenant_id = $3::uuid)`,
		id, isServerAdmin, tenantArg,
	).Scan(&r.tenant, &r.role, &r.active, &r.homeScope, &r.allowedScopes)
	if err == nil {
		return r, true, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return resolvedKey{}, false, nil
	}
	if pgxdb.MalformedUUID(err) {
		return resolvedKey{}, false, nil // malformed id → indistinguishable from absent
	}
	return resolvedKey{}, false, fmt.Errorf("api_keys: update resolve: %w", err)
}

// resolvedKey carries the fields resolveKeyForUpdate reads under the caller's
// tenant constraint. home_scope + allowed_scopes were added for the 078 write_scopes
// validation (write_scopes ⊆ allowed_scopes ∪ {home_scope}); DeleteApiKey ignores them.
type resolvedKey struct {
	tenant        string
	role          string
	active        bool
	homeScope     string
	allowedScopes []string
}
