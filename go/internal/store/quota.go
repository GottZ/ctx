package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Per-tenant quota CRUD (MT T36b, 04-W4, context_tenant_quota / 063). The
// QuotaAccountant (internal/backends/quota.go) READS this table directly on its
// cached refresh path; this is the management write/read surface behind the
// tenant-quota-set/get manage-actions + the `ctx quota` CLI. The 063 NOTIFY
// trigger fires on the settings channel after a write — the accountant is also
// refreshed synchronously by the handler so a set takes effect at once.

// GetTenantQuota loads one tenant scope's quota policy. found=false (nil policy,
// nil error) when no row exists — the caller renders "unlimited / fail-open",
// never an error.
func GetTenantQuota(ctx context.Context, pool *pgxpool.Pool, scope string) (*backends.TenantQuota, bool, error) {
	var q backends.TenantQuota
	err := pool.QueryRow(ctx, `
		SELECT daily_cost_usd, monthly_cost_usd, daily_calls, on_exceed, enabled
		FROM context_tenant_quota
		WHERE scope = $1`, scope).Scan(
		&q.DailyCostUSD, &q.MonthlyCostUSD, &q.DailyCalls, &q.OnExceed, &q.Enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("store: get tenant quota: %w", err)
	}
	return &q, true, nil
}

// UpsertTenantQuota writes (insert-or-replace) one tenant scope's quota policy,
// keyed on the UNIQUE(scope) constraint. by is the acting key id for the
// updated_by provenance column (nil/"" → NULL; the FK tolerates it). The DB
// CHECK on on_exceed ('block'|'external_off') is the last line — the handler
// validates first for a clean 422.
func UpsertTenantQuota(ctx context.Context, pool *pgxpool.Pool, scope string, q backends.TenantQuota, by *string) error {
	_, err := pool.Exec(ctx, `
		INSERT INTO context_tenant_quota
		    (scope, daily_cost_usd, monthly_cost_usd, daily_calls, on_exceed, enabled, updated_by, updated_by_principal)
		VALUES ($1,$2,$3,$4,$5,$6,$7::uuid,
		        (SELECT k.principal_id FROM context_api_keys k WHERE k.id = $7::uuid))
		ON CONFLICT (scope) DO UPDATE SET
		    daily_cost_usd  = EXCLUDED.daily_cost_usd,
		    monthly_cost_usd = EXCLUDED.monthly_cost_usd,
		    daily_calls     = EXCLUDED.daily_calls,
		    on_exceed       = EXCLUDED.on_exceed,
		    enabled         = EXCLUDED.enabled,
		    updated_by      = EXCLUDED.updated_by,
		    updated_by_principal = EXCLUDED.updated_by_principal,
		    updated_at      = now()`,
		scope, q.DailyCostUSD, q.MonthlyCostUSD, q.DailyCalls, q.OnExceed, q.Enabled, by)
	if err != nil {
		return fmt.Errorf("store: upsert tenant quota: %w", err)
	}
	return nil
}
