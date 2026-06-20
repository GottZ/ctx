package settings

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
)

// TenantOverlay returns the config.Store overlay (06-C2): it resolves a tenant's
// effective config as the _global base plus that tenant's context_settings rows,
// precedence tenant > _global > env > default — the second overlay pass on the
// same config.Build machine, fed by the MT3-W3 loadTenantOverrideRows. This is
// the consumer that wave; the returned value is injected into config.Store by a
// later wave (06-C3). Until then config.Store.overlay is nil and SnapshotFor*
// return the base unchanged, so the server is functionally identical
// (pausability invariant).
//
// Contract (config.TenantOverlay, store.go). The Store has already rejected an
// empty or reserved ('_'-prefixed) scope before calling the overlay, so
// tenantScope is a real home_scope. It returns:
//   - (base, nil)  when the tenant has no own override rows — it inherits the
//     base unchanged. The Store caches the base pointer cheaply, so the common
//     "tenant without own settings" case adds one pointer, not a full config
//     generation (the §10.2 footprint guard at N tenants).
//   - (cfg, nil)   a freshly built tenant generation when own rows exist.
//   - (nil, err)   on a load failure — the Store falls back to base WITHOUT
//     caching, so the next access retries (fail-safe, self-healing).
func TenantOverlay(pool *pgxpool.Pool) config.TenantOverlay {
	return func(ctx context.Context, base *config.Config, tenantScope string) (*config.Config, error) {
		rows, err := loadTenantOverrideRows(ctx, pool, tenantScope)
		if err != nil {
			return nil, err
		}
		if !hasScope(rows, tenantScope) {
			return base, nil
		}
		cfg, issues := BuildFromRows(ctx, pool, rows, []string{store.GlobalScope, tenantScope})
		logTenantIssues(tenantScope, issues)
		return cfg, nil
	}
}

// hasScope reports whether any row carries scope. The two-scope load always
// returns the _global base rows, so this keys on the TENANT scope to decide
// "this tenant has its own settings" vs "inherit the base" — keying on row
// presence instead would build a redundant generation for every tenant that
// merely inherits _global.
func hasScope(rows []store.SettingOverride, scope string) bool {
	for _, r := range rows {
		if r.Scope == scope {
			return true
		}
	}
	return false
}

// logTenantIssues surfaces overlay build findings with the tenant scope attached
// so a per-tenant misconfig is visible (W11). Messages are value-free by the
// toOverrides / §3.3 contract — they name keys and reasons, never values.
func logTenantIssues(tenantScope string, issues []config.Issue) {
	for _, is := range issues {
		if is.Severity == config.SeverityError {
			slog.Error("settings: tenant overlay error", "tenant", tenantScope, "field", is.Field, "msg", is.Msg)
			continue
		}
		slog.Warn("settings: tenant overlay issue", "tenant", tenantScope, "field", is.Field, "msg", is.Msg)
	}
}
