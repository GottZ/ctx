package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// GlobalScope is the sentinel scope for server-global settings and secrets
// (migration 051). The leading underscore marks the SYSTEM-RESERVED scope
// namespace: api-key-create rejects '_'-prefixed scopes (052), so no tenant
// home_scope can ever collide with the global settings identity.
//
// TODO(multi-tenant): per-tenant settings resolution reads tenant-scope
// overrides on top of GlobalScope; F2 reads scope='_global' exclusively.
const GlobalScope = "_global"

// SettingOverride is one row of the context_settings DB override layer.
// Runtime precedence (once the consumer waves land): request override >
// DB override > env > code default.
type SettingOverride struct {
	Key       string          `json:"key"`
	Scope     string          `json:"scope"`
	Value     json.RawMessage `json:"value"`
	UpdatedAt time.Time       `json:"updated_at"`
	UpdatedBy *string         `json:"updated_by,omitempty"`
}

// LoadSettingOverrides returns all overrides for one scope, ordered by key.
// Read path is pool-based (no actor attribution needed for reads).
func LoadSettingOverrides(ctx context.Context, pool *pgxpool.Pool, scope string) ([]SettingOverride, error) {
	if scope == "" {
		return nil, fmt.Errorf("settings: scope is required")
	}

	rows, err := pool.Query(ctx,
		`SELECT key, scope, value, updated_at, updated_by::text
		 FROM context_settings
		 WHERE scope = $1
		 ORDER BY key`,
		scope)
	if err != nil {
		return nil, fmt.Errorf("settings: load overrides: %w", err)
	}
	defer rows.Close()

	var overrides []SettingOverride
	for rows.Next() {
		var o SettingOverride
		if err := rows.Scan(&o.Key, &o.Scope, &o.Value, &o.UpdatedAt, &o.UpdatedBy); err != nil {
			return nil, fmt.Errorf("settings: scan override: %w", err)
		}
		overrides = append(overrides, o)
	}
	return overrides, rows.Err()
}

// LoadSettingOverridesMulti returns overrides for several scopes in one query,
// ordered by key and then by each scope's position in the input slice — the
// LAST scope listed wins per key. For {GlobalScope, tenant} that means tenant
// beats _global; array_position gives a total order over ANY number of scopes,
// so a future caller resolving across >2 scopes stays deterministic regardless
// of row order. The caller still materializes the precedence in Go (a later
// wave); this ORDER BY is the defensive safeguard, the index-backed read path
// the foundation. No consumer yet (additive next to LoadSettingOverrides, which
// stays the boot-time _global path).
//
// Fail-closed (mirrors rrf.Search empty-scopes, auth-scope §6.2): an empty
// slice or any empty element is rejected — an unscoped scope = ANY('{}') would
// silently match nothing instead of erroring.
func LoadSettingOverridesMulti(ctx context.Context, pool *pgxpool.Pool, scopes []string) ([]SettingOverride, error) {
	if len(scopes) == 0 {
		return nil, fmt.Errorf("settings: at least one scope is required")
	}
	for _, s := range scopes {
		if s == "" {
			return nil, fmt.Errorf("settings: empty scope is not allowed")
		}
	}

	rows, err := pool.Query(ctx,
		`SELECT key, scope, value, updated_at, updated_by::text
		 FROM context_settings
		 WHERE scope = ANY($1::text[])
		 ORDER BY key, array_position($1::text[], scope) DESC`,
		scopes)
	if err != nil {
		return nil, fmt.Errorf("settings: load overrides multi: %w", err)
	}
	defer rows.Close()

	var overrides []SettingOverride
	for rows.Next() {
		var o SettingOverride
		if err := rows.Scan(&o.Key, &o.Scope, &o.Value, &o.UpdatedAt, &o.UpdatedBy); err != nil {
			return nil, fmt.Errorf("settings: scan override: %w", err)
		}
		overrides = append(overrides, o)
	}
	return overrides, rows.Err()
}

// SettingRowRef identifies one context_settings row without its value: the
// (key, scope) primary key alone. The deprecation sweep that consumes it
// reports WHERE a superseded row lives, never WHAT it says — half of the 29
// retired keys are secret-class, and a boot log line is the last place a
// resolved api_key may surface (§3.3 log invariant).
type SettingRowRef struct {
	Key   string `json:"key"`
	Scope string `json:"scope"`
}

// SettingRowsForKeys returns every context_settings row on the given keys, in
// EVERY scope, ordered by key then scope. Deliberately scope-blind where the
// rest of this file is scope-addressed: its caller is the A06-A1 boot sweep
// over the retired backend tuple keys, and those rows can sit in tenant scopes
// (the 6 *.api_key keys are tenant-overridable) that no _global-addressed read
// would ever see — a sweep that missed them would tell an operator his
// installation is clean while a tenant override still shadows the pool
// (design/06 §3.3 step 2a, §5.3).
//
// Fail-closed on an empty input like LoadSettingOverridesMulti: key = ANY('{}')
// would silently match nothing and report a clean installation for the wrong
// reason.
func SettingRowsForKeys(ctx context.Context, pool *pgxpool.Pool, keys []string) ([]SettingRowRef, error) {
	if len(keys) == 0 {
		return nil, fmt.Errorf("settings: at least one key is required")
	}
	for _, k := range keys {
		if k == "" {
			return nil, fmt.Errorf("settings: empty key is not allowed")
		}
	}

	rows, err := pool.Query(ctx,
		`SELECT key, scope
		 FROM context_settings
		 WHERE key = ANY($1::text[])
		 ORDER BY key, scope`,
		keys)
	if err != nil {
		return nil, fmt.Errorf("settings: load rows for keys: %w", err)
	}
	defer rows.Close()

	var refs []SettingRowRef
	for rows.Next() {
		var r SettingRowRef
		if err := rows.Scan(&r.Key, &r.Scope); err != nil {
			return nil, fmt.Errorf("settings: scan row ref: %w", err)
		}
		refs = append(refs, r)
	}
	return refs, rows.Err()
}

// AllowSharedSecretsKey is the global-only opt-in flag a tenant's _global
// secret fallback is gated on (03-W5 / D2). It is read DIRECTLY at the TENANT
// scope, NOT through the config snapshot: the registry classifies it
// tenancy:global-only, so the toOverrides gate drops a tenant-scope row from the
// snapshot (a tenant must not be able to self-grant). The operator seeds the
// per-tenant row out of band; this read is the per-tenant truth. Default false =
// strict isolation.
const AllowSharedSecretsKey = "tenant.allow_shared_secrets"

// TenantAllowsSharedSecrets reports whether the tenant has opted INTO the shared
// _global secret fallback (its own tenant-scope AllowSharedSecretsKey row is the
// JSON bool true). Reserved/empty scopes (_global itself never opts in) and a
// missing row are fail-closed false (strict isolation). A read error is the
// caller's to handle — it must NOT be treated as opt-in.
func TenantAllowsSharedSecrets(ctx context.Context, pool *pgxpool.Pool, tenant string) (bool, error) {
	if tenant == "" || strings.HasPrefix(tenant, "_") {
		return false, nil // _global / reserved scope never opts itself in
	}
	var raw json.RawMessage
	err := pool.QueryRow(ctx,
		`SELECT value FROM context_settings WHERE key = $1 AND scope = $2`,
		AllowSharedSecretsKey, tenant).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("secrets: read shared-secrets opt-in for %q: %w", tenant, err)
	}
	return strings.TrimSpace(string(raw)) == "true", nil
}

// AllowCrossTenantBlockGrantKey is the global-only opt-in flag a tenant's
// CROSS-TENANT block grant is gated on (07-W6 / design/07 §5.2). EXACT analog of
// AllowSharedSecretsKey: the registry classifies it tenancy:global-only, so the
// toOverrides gate drops a tenant-scope row from the snapshot (a tenant must not
// self-grant). The operator seeds the per-tenant row out of band; this read is the
// per-tenant truth. Default false = no cross-tenant block sharing (intra-tenant
// block grants stay default-allowed — the Enterprise department use-case).
const AllowCrossTenantBlockGrantKey = "tenant.allow_cross_tenant_block_grant"

// TenantAllowsCrossTenantBlockGrant reports whether the tenant has opted INTO
// granting single blocks across the tenant boundary (its own tenant-scope
// AllowCrossTenantBlockGrantKey row is the JSON bool true). Reserved/empty scopes
// (_global never opts in) and a missing row are fail-closed false. A read error is
// the caller's to handle — it must NOT be treated as opt-in (the handler treats
// err as DENY, design/07 §5.2). EXACT analog of TenantAllowsSharedSecrets.
func TenantAllowsCrossTenantBlockGrant(ctx context.Context, pool *pgxpool.Pool, tenant string) (bool, error) {
	if tenant == "" || strings.HasPrefix(tenant, "_") {
		return false, nil // _global / reserved scope never opts itself in
	}
	var raw json.RawMessage
	err := pool.QueryRow(ctx,
		`SELECT value FROM context_settings WHERE key = $1 AND scope = $2`,
		AllowCrossTenantBlockGrantKey, tenant).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("block-grant: read cross-tenant opt-in for %q: %w", tenant, err)
	}
	return strings.TrimSpace(string(raw)) == "true", nil
}

// OptInTenantScopes returns the tenant scopes whose AllowSharedSecretsKey row is
// true — the set a _global secret can be referenced from VIA the fallback. The
// operator's _global-secret DELETE reference scan (§5.7) must check these scopes
// cross-scope, or a tenant secret_ref setting would silently fail-open. The set
// is small (only opted-in tenants); ordered by scope for reproducibility.
func OptInTenantScopes(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	rows, err := pool.Query(ctx,
		`SELECT scope FROM context_settings
		 WHERE key = $1 AND scope <> $2 AND value::text = 'true'
		 ORDER BY scope`,
		AllowSharedSecretsKey, GlobalScope)
	if err != nil {
		return nil, fmt.Errorf("secrets: list shared-secret opt-in scopes: %w", err)
	}
	defer rows.Close()

	var scopes []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("secrets: scan opt-in scope: %w", err)
		}
		scopes = append(scopes, s)
	}
	return scopes, rows.Err()
}

// SettingAudit is one context_settings_audit row of entity_type='setting',
// shaped for the GET /api/settings/{key} history. Values are never sensitive
// for settings rows: the W5 secret_ref gate keeps plaintext out of the table
// by construction.
type SettingAudit struct {
	Action     string          `json:"action"`
	OldValue   json.RawMessage `json:"old_value,omitempty"`
	NewValue   json.RawMessage `json:"new_value,omitempty"`
	ActorLabel *string         `json:"actor_label,omitempty"`
	Via        string          `json:"via,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
}

// ListSettingAudit returns the newest audit rows for one settings key.
func ListSettingAudit(ctx context.Context, pool *pgxpool.Pool, key, scope string, limit int) ([]SettingAudit, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := pool.Query(ctx,
		`SELECT action, old_value, new_value, actor_label, COALESCE(metadata->>'via', ''), created_at
		 FROM context_settings_audit
		 WHERE entity_type = 'setting' AND entity_key = $1 AND scope = $2
		 ORDER BY created_at DESC
		 LIMIT $3`,
		key, scope, limit)
	if err != nil {
		return nil, fmt.Errorf("settings: list audit: %w", err)
	}
	defer rows.Close()

	var out []SettingAudit
	for rows.Next() {
		var a SettingAudit
		if err := rows.Scan(&a.Action, &a.OldValue, &a.NewValue, &a.ActorLabel, &a.Via, &a.CreatedAt); err != nil {
			return nil, fmt.Errorf("settings: scan audit: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// setTxActor stamps the audit actor for the current transaction. The 051
// audit trigger reads ctx.api_key_id via current_setting(); set_config with
// is_local=true is the parameterized equivalent of SET LOCAL (resets at
// COMMIT/ROLLBACK). nil/empty actor leaves the GUC unset — the trigger then
// records api_key_id NULL + metadata.via='sql' (the psql-direct-edit shape).
//
// Centralized inside the write functions (instead of trusting every caller
// to SET LOCAL first) so the audit actor can never desync from the column
// attribution (updated_by/created_by/rotated_by) passed in the same call.
func setTxActor(ctx context.Context, tx pgx.Tx, by *string) error {
	if by == nil || *by == "" {
		return nil
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('ctx.api_key_id', $1, true)`, *by); err != nil {
		return fmt.Errorf("settings: set tx actor: %w", err)
	}
	return nil
}

// SetTxRequestID optionally stamps the request id for audit metadata
// (metadata.request_id in context_settings_audit). Call once per write
// transaction before the mutation; resets at COMMIT/ROLLBACK.
func SetTxRequestID(ctx context.Context, tx pgx.Tx, requestID string) error {
	if requestID == "" {
		return nil
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('ctx.request_id', $1, true)`, requestID); err != nil {
		return fmt.Errorf("settings: set tx request id: %w", err)
	}
	return nil
}

// UpsertSetting creates or replaces one override row. Write paths ALWAYS run
// in an explicit transaction: the 051 AFTER-ROW triggers emit the audit row
// and NOTIFY atomically with the mutation. by is the acting api key id
// (nullable — nil means an unattributed/system write).
func UpsertSetting(ctx context.Context, tx pgx.Tx, key, scope string, value json.RawMessage, by *string) error {
	if key == "" {
		return fmt.Errorf("settings: key is required")
	}
	if scope == "" {
		return fmt.Errorf("settings: scope is required")
	}
	if len(value) == 0 || !json.Valid(value) {
		return fmt.Errorf("settings: value must be valid JSON")
	}

	if err := setTxActor(ctx, tx, by); err != nil {
		return err
	}

	_, err := tx.Exec(ctx,
		`INSERT INTO context_settings (key, scope, value, updated_by, updated_by_principal)
		 VALUES ($1, $2, $3::jsonb, $4::uuid,
		         (SELECT k.principal_id FROM context_api_keys k WHERE k.id = $4::uuid))
		 ON CONFLICT (key, scope) DO UPDATE
		 SET value = EXCLUDED.value,
		     updated_at = now(),
		     updated_by = EXCLUDED.updated_by,
		     updated_by_principal = EXCLUDED.updated_by_principal`,
		key, scope, string(value), by)
	if err != nil {
		return fmt.Errorf("settings: upsert %s: %w", key, err)
	}
	return nil
}

// DeleteSetting removes one override row (revert to env/default). Returns
// found=false when no override existed. The audit row (action='unset') is
// written by the 051 trigger; the actor reaches it via setTxActor since a
// DELETE has no column to carry attribution.
func DeleteSetting(ctx context.Context, tx pgx.Tx, key, scope string, by *string) (bool, error) {
	if key == "" {
		return false, fmt.Errorf("settings: key is required")
	}
	if scope == "" {
		return false, fmt.Errorf("settings: scope is required")
	}

	if err := setTxActor(ctx, tx, by); err != nil {
		return false, err
	}

	tag, err := tx.Exec(ctx,
		`DELETE FROM context_settings WHERE key = $1 AND scope = $2`,
		key, scope)
	if err != nil {
		return false, fmt.Errorf("settings: delete %s: %w", key, err)
	}
	return tag.RowsAffected() > 0, nil
}
