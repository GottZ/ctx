package store

import (
	"context"
	"encoding/json"
	"fmt"
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
		`INSERT INTO context_settings (key, scope, value, updated_by)
		 VALUES ($1, $2, $3::jsonb, $4::uuid)
		 ON CONFLICT (key, scope) DO UPDATE
		 SET value = EXCLUDED.value,
		     updated_at = now(),
		     updated_by = EXCLUDED.updated_by`,
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
