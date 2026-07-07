package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Per-category graph HUE override CRUD (093, Web-UX U02-W5, design/02a §A1/§A3).
// A row is the OPTIONAL override of one block CATEGORY's node/cluster hue in one
// scope; the default carrier stays the client-side hash seed (categoryColor).
// Resolution chain (AM-2): tenant-override → global-override → seed. Only the hue
// (HSL degree 0–359) is persisted (02a §A2). Write paths run inside a caller
// transaction with setTxActor (the 093 audit trigger reads ctx.api_key_id, so
// audit attribution can never desync from the mutation) — mirroring the
// settings/disable-profile stores. Reads are pool-based (no actor attribution).

// LoadCategoryHues returns the RESOLVED sparse override map for the given scopes:
// only categories that carry an override in at least one scope appear, and the
// value is the HIGHEST-precedence scope's hue PER CATEGORY. Precedence is the
// scope's position in scopes (LAST wins) — for readScopes()'s {_global, tenant}
// that makes a tenant override beat the _global one for the SAME category, while
// a category only overridden globally still surfaces (per-key precedence, 02a
// §A3). The merge is materialized in Go, NOT trusted to the SQL ORDER BY (the
// settings.go:149 doctrine — the effective hue a tenant renders must be its own,
// never _global's when both exist).
//
// Fail-closed (mirrors LoadSettingOverridesMulti / rrf.Search empty-scopes): an
// empty slice or any empty element is rejected — an unscoped scope = ANY('{}')
// would silently resolve to the empty map instead of erroring.
func LoadCategoryHues(ctx context.Context, pool *pgxpool.Pool, scopes []string) (map[string]int16, error) {
	if len(scopes) == 0 {
		return nil, fmt.Errorf("graph hues: at least one scope is required")
	}
	prio := make(map[string]int, len(scopes))
	for i, s := range scopes {
		if s == "" {
			return nil, fmt.Errorf("graph hues: empty scope is not allowed")
		}
		prio[s] = i // last occurrence wins — highest precedence
	}

	rows, err := pool.Query(ctx,
		`SELECT scope, category, hue
		   FROM context_graph_category_hues
		  WHERE scope = ANY($1::text[])`,
		scopes)
	if err != nil {
		return nil, fmt.Errorf("graph hues: load: %w", err)
	}
	defer rows.Close()

	out := make(map[string]int16)
	best := make(map[string]int) // category → winning scope priority
	for rows.Next() {
		var scope, category string
		var hue int16
		if err := rows.Scan(&scope, &category, &hue); err != nil {
			return nil, fmt.Errorf("graph hues: scan: %w", err)
		}
		p := prio[scope]
		if cur, seen := best[category]; seen && p <= cur {
			continue // a same-or-higher-precedence scope already set this category
		}
		best[category] = p
		out[category] = hue
	}
	return out, rows.Err()
}

// UpsertCategoryHue creates or replaces one override row (one scope, one
// category). Write paths ALWAYS run in an explicit transaction: the 093
// AFTER-ROW audit trigger emits its row atomically with the mutation. by is the
// acting api key id (nullable — nil means an unattributed/system write). scope is
// the caller's writeScope (operator → _global, tenant-admin → own scope), NEVER
// the request body (02a §A5-MT).
func UpsertCategoryHue(ctx context.Context, tx pgx.Tx, scope, category string, hue int16, by *string) error {
	if scope == "" {
		return fmt.Errorf("graph hues: scope is required")
	}
	if category == "" {
		return fmt.Errorf("graph hues: category is required")
	}
	if hue < 0 || hue > 359 {
		return fmt.Errorf("graph hues: hue must be 0..359, got %d", hue)
	}
	if err := setTxActor(ctx, tx, by); err != nil {
		return err
	}
	_, err := tx.Exec(ctx,
		`INSERT INTO context_graph_category_hues (scope, category, hue, updated_by)
		 VALUES ($1, $2, $3, $4::uuid)
		 ON CONFLICT (scope, category) DO UPDATE
		 SET hue = EXCLUDED.hue,
		     updated_at = now(),
		     updated_by = EXCLUDED.updated_by`,
		scope, category, hue, by)
	if err != nil {
		return fmt.Errorf("graph hues: upsert %s@%s: %w", category, scope, err)
	}
	return nil
}

// DeleteCategoryHue removes one override row (revert to seed). Returns
// found=false when no override existed in that scope — the handler answers 404.
// The audit row (action='delete') is written by the 093 trigger; the actor
// reaches it via setTxActor since a DELETE has no column to carry attribution.
func DeleteCategoryHue(ctx context.Context, tx pgx.Tx, scope, category string, by *string) (bool, error) {
	if scope == "" {
		return false, fmt.Errorf("graph hues: scope is required")
	}
	if category == "" {
		return false, fmt.Errorf("graph hues: category is required")
	}
	if err := setTxActor(ctx, tx, by); err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx,
		`DELETE FROM context_graph_category_hues WHERE scope = $1 AND category = $2`,
		scope, category)
	if err != nil {
		return false, fmt.Errorf("graph hues: delete %s@%s: %w", category, scope, err)
	}
	return tag.RowsAffected() > 0, nil
}
