package store

// The rows of context_gateway_limits (migration 168, wave W06-15,
// design/06 §3.5): the meter's policy as data. The resolution rule and the
// ceiling of a tenant-admin write live in internal/gatewaylimits; this file
// reads all rows for its registry and writes one row per (subject, tier, op)
// for the gateway-limits-* manage actions, stamped for the audit trigger.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/gatewaylimits"
	"github.com/GottZ/ctx/internal/keyquota"
	"github.com/GottZ/ctx/internal/pgxdb"
)

// ErrGatewayLimitSubject: the subject of a write is unknown, malformed or
// outside the caller's access — one error for all three (no oracle; the
// handler answers 404 "model consumer not found" / "tenant not found").
var ErrGatewayLimitSubject = errors.New("store: gateway limits subject not found")

// GatewayLimitKey addresses one row: the subject (kind and the id its kind
// names), the tier of the writer and the op.
type GatewayLimitKey struct {
	Kind       gatewaylimits.SubjectKind
	ConsumerID string
	TenantID   string
	Tier       gatewaylimits.Tier
	Op         keyquota.Op
}

// gatewayLimitCols are the columns every read scans, in scanGatewayLimit's
// order (integers widened to bigint, the cost to float8).
const gatewayLimitCols = `id::text, subject_kind, COALESCE(consumer_id::text, ''), COALESCE(tenant_id::text, ''), tier, op,
	rpm::int8, tpm::int8, max_concurrency::int8, daily_tokens, monthly_tokens, daily_requests::int8,
	monthly_cost_usd::float8, allow_external, allow_unmetered_external, updated_at`

func scanGatewayLimit(row pgx.Row) (gatewaylimits.Row, error) {
	var (
		r          gatewaylimits.Row
		kind, tier string
		op         string
		updated    time.Time
	)
	err := row.Scan(&r.ID, &kind, &r.ConsumerID, &r.TenantID, &tier, &op,
		&r.RPM, &r.TPM, &r.MaxConcurrency, &r.DailyTokens, &r.MonthlyTokens, &r.DailyRequests,
		&r.MonthlyCostUSD, &r.AllowExternal, &r.AllowUnmeteredExternal, &updated)
	if err != nil {
		return gatewaylimits.Row{}, err
	}
	r.Kind, r.Tier, r.Op, r.UpdatedAt = gatewaylimits.SubjectKind(kind), gatewaylimits.Tier(tier), keyquota.Op(op), updated
	return r, nil
}

// LoadGatewayLimits reads every row of context_gateway_limits — the Loader
// of the registry (gatewaylimits.Registry, boot, after each manage write and
// on the ctx_settings_write NOTIFY). The table holds at most a few rows per
// consumer, tenant and op: one full read per reload.
func LoadGatewayLimits(ctx context.Context, q pgxdb.Querier) ([]gatewaylimits.Row, error) {
	rows, err := q.Query(ctx, `SELECT `+gatewayLimitCols+` FROM context_gateway_limits ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: load gateway limits: %w", err)
	}
	defer rows.Close()
	var out []gatewaylimits.Row
	for rows.Next() {
		r, err := scanGatewayLimit(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan gateway limits: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: load gateway limits: %w", err)
	}
	return out, nil
}

// GatewayLimitSubjectTenant resolves key's subject within access: the
// canonical consumer id (consumer rows) and the tenant the subject belongs
// to (a consumer's tenant, the tenant itself; "" for the instance), or
// ErrGatewayLimitSubject for an unknown, malformed or foreign subject and
// for the server-tier-only kinds without server-admin access — one error
// for all (no oracle).
func GatewayLimitSubjectTenant(ctx context.Context, q pgxdb.Rower, access ModelAccess, key GatewayLimitKey) (consumer, tenant string, err error) {
	srv, accessTenant := access.args()
	switch key.Kind {
	case gatewaylimits.SubjectConsumer:
		id, ok := CanonicalUUID(key.ConsumerID)
		if !ok {
			return "", "", ErrGatewayLimitSubject
		}
		err = q.QueryRow(ctx, `SELECT c.id::text, c.tenant_id::text FROM context_model_consumers c
			WHERE c.id = $1::uuid AND ($2::boolean OR c.tenant_id = $3::uuid)`, id, srv, accessTenant).Scan(&consumer, &tenant)
	case gatewaylimits.SubjectTenant, gatewaylimits.SubjectTenantEnvelope:
		if key.Kind == gatewaylimits.SubjectTenantEnvelope && !access.ServerAdmin {
			return "", "", ErrGatewayLimitSubject
		}
		id, ok := CanonicalUUID(key.TenantID)
		if !ok {
			return "", "", ErrGatewayLimitSubject
		}
		err = q.QueryRow(ctx, `SELECT t.id::text FROM context_tenants t
			WHERE t.id = $1::uuid AND ($2::boolean OR t.id = $3::uuid)`, id, srv, accessTenant).Scan(&tenant)
	case gatewaylimits.SubjectInstance:
		if !access.ServerAdmin {
			return "", "", ErrGatewayLimitSubject
		}
		return "", "", nil
	default:
		return "", "", ErrGatewayLimitSubject
	}
	if errors.Is(err, pgx.ErrNoRows) || pgxdb.AbsentOrMalformed(err) {
		return "", "", ErrGatewayLimitSubject
	}
	if err != nil {
		return "", "", fmt.Errorf("store: gateway limits subject: %w", err)
	}
	return consumer, tenant, nil
}

// rowIDs are the (consumer_id, tenant_id) a row of kind carries: a consumer
// row names only its consumer (ck_gl_subject), a tenant or envelope row only
// its tenant, the instance neither.
func rowIDs(kind gatewaylimits.SubjectKind, consumer, tenant string) (*string, *string) {
	switch kind {
	case gatewaylimits.SubjectConsumer:
		return nullable(consumer), nil
	case gatewaylimits.SubjectInstance:
		return nil, nil
	}
	return nil, nullable(tenant)
}

// nullable binds "" as NULL.
func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// UpsertGatewayLimit replaces the row of key with v (a full replacement: a
// nil dimension stores NULL) in one transaction stamped with stamp
// (updated_by, updated_by_principal from the stamp's key), after checking
// in the same transaction that the subject is within access: a consumer of
// access.TenantID (or any for a server-admin), the tenant access.TenantID
// (or any), the instance and the tenant envelope for a server-admin only —
// else ErrGatewayLimitSubject. It returns the stored row. The values are
// not validated here (gatewaylimits.ValidateValues, the handler); the
// table's CHECKs are the last line.
func UpsertGatewayLimit(ctx context.Context, pool *pgxpool.Pool, access ModelAccess, key GatewayLimitKey, v gatewaylimits.Values, stamp AuditStamp) (gatewaylimits.Row, error) {
	var out gatewaylimits.Row
	err := pgxdb.Write(ctx, pool, pgxdb.Stages{Begin: "gateway_limits: upsert begin", Commit: "gateway_limits: upsert commit"},
		func(tx pgx.Tx) error {
			if err := stampAudit(ctx, tx, stamp); err != nil {
				return err
			}
			consumer, tenant, err := GatewayLimitSubjectTenant(ctx, tx, access, key)
			if err != nil {
				return err
			}
			cid, tid := rowIDs(key.Kind, consumer, tenant)
			out, err = scanGatewayLimit(tx.QueryRow(ctx, `
				INSERT INTO context_gateway_limits AS l (subject_kind, consumer_id, tenant_id, tier, op,
				    rpm, tpm, max_concurrency, daily_tokens, monthly_tokens, daily_requests, monthly_cost_usd,
				    allow_external, allow_unmetered_external, updated_by, updated_by_principal)
				VALUES ($1, $2::uuid, $3::uuid, $4, $5, $6, $7, $8, $9, $10, $11, $12::float8::numeric, $13, $14,
				    $15::uuid, (SELECT k.principal_id FROM context_api_keys k WHERE k.id = $15::uuid))
				ON CONFLICT (subject_kind, consumer_id, tenant_id, tier, op) DO UPDATE SET
				    rpm = EXCLUDED.rpm, tpm = EXCLUDED.tpm, max_concurrency = EXCLUDED.max_concurrency,
				    daily_tokens = EXCLUDED.daily_tokens, monthly_tokens = EXCLUDED.monthly_tokens,
				    daily_requests = EXCLUDED.daily_requests, monthly_cost_usd = EXCLUDED.monthly_cost_usd,
				    allow_external = EXCLUDED.allow_external, allow_unmetered_external = EXCLUDED.allow_unmetered_external,
				    updated_by = EXCLUDED.updated_by, updated_by_principal = EXCLUDED.updated_by_principal, updated_at = now()
				RETURNING `+gatewayLimitCols,
				string(key.Kind), cid, tid, string(key.Tier), string(key.Op),
				v.RPM, v.TPM, v.MaxConcurrency, v.DailyTokens, v.MonthlyTokens, v.DailyRequests, v.MonthlyCostUSD,
				v.AllowExternal, v.AllowUnmeteredExternal, nullable(stamp.ActorKeyID)))
			return err
		})
	if err != nil {
		return gatewaylimits.Row{}, err
	}
	return out, nil
}

// DeleteGatewayLimit deletes the row of key under the same access rule,
// stamped; it reports whether a row was deleted (false for a subject within
// access without that row).
func DeleteGatewayLimit(ctx context.Context, pool *pgxpool.Pool, access ModelAccess, key GatewayLimitKey, stamp AuditStamp) (bool, error) {
	var deleted bool
	err := pgxdb.Write(ctx, pool, pgxdb.Stages{Begin: "gateway_limits: delete begin", Commit: "gateway_limits: delete commit"},
		func(tx pgx.Tx) error {
			if err := stampAudit(ctx, tx, stamp); err != nil {
				return err
			}
			consumer, tenant, err := GatewayLimitSubjectTenant(ctx, tx, access, key)
			if err != nil {
				return err
			}
			cid, tid := rowIDs(key.Kind, consumer, tenant)
			tag, err := tx.Exec(ctx, `DELETE FROM context_gateway_limits
				WHERE subject_kind = $1 AND consumer_id IS NOT DISTINCT FROM $2::uuid AND tenant_id IS NOT DISTINCT FROM $3::uuid
				  AND tier = $4 AND op = $5`,
				string(key.Kind), cid, tid, string(key.Tier), string(key.Op))
			deleted = tag.RowsAffected() > 0
			return err
		})
	return deleted, err
}

// pruneGatewayLimits deletes the limits rows of a tenant and of its
// consumers inside the prune's stamped transaction, BEFORE the consumers:
// the CASCADE from a deleted consumer would write audit rows that no longer
// find the consumer's tenant, and the CASCADE from the tenant row (deleted
// outside the stamped transaction) rows without via='offboarding'. On a
// chain below 168 (the migration tests of 160 … 167 prune on their own
// chain) there is no table and nothing to delete: one catalog lookup per
// prune, a rare server-admin operation.
func pruneGatewayLimits(ctx context.Context, tx pgx.Tx, tenantID string) error {
	var present bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass('context_gateway_limits') IS NOT NULL`).Scan(&present); err != nil {
		return fmt.Errorf("store: prune tenant: gateway limits: %w", err)
	}
	if !present {
		return nil
	}
	if _, err := tx.Exec(ctx, `DELETE FROM context_gateway_limits
		WHERE tenant_id = $1::uuid
		   OR consumer_id IN (SELECT c.id FROM context_model_consumers c WHERE c.tenant_id = $1::uuid)`, tenantID); err != nil {
		return fmt.Errorf("store: prune tenant: gateway limits: %w", err)
	}
	return nil
}
