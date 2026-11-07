package store

// The rows of context_gateway_limits (migration 168, wave W06-15,
// design/06 §3.5): the meter's policy as data. The resolution rule and the
// ceiling of a tenant-admin write live in internal/gatewaylimits; this file
// reads all rows for its registry and writes one row per (subject, tier, op)
// for the gateway-limits-* manage actions, stamped for the audit trigger.

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/gatewaylimits"
	"github.com/GottZ/ctx/internal/keyquota"
	"github.com/GottZ/ctx/internal/pgxdb"
)

// ErrGatewayLimitsNotImplemented is the answer of the RC-D S1 stubs below.
var ErrGatewayLimitsNotImplemented = errors.New("store: gateway limits not implemented (RC-D S1 stub)")

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

// LoadGatewayLimits reads every row of context_gateway_limits — the Loader
// of the registry (gatewaylimits.Registry, boot, after each manage write and
// on the ctx_settings_write NOTIFY).
//
// RC-D S1 stub.
func LoadGatewayLimits(ctx context.Context, q pgxdb.Querier) ([]gatewaylimits.Row, error) {
	_, _ = ctx, q
	return nil, ErrGatewayLimitsNotImplemented
}

// UpsertGatewayLimit replaces the row of key with v (a full replacement: a
// nil dimension stores NULL) in one transaction stamped with stamp
// (updated_by, updated_by_principal from the stamp's key), after checking in
// the same statement that the subject is within access: a consumer of
// access.TenantID (or any for a server-admin), the tenant access.TenantID
// (or any), the instance and the tenant envelope for a server-admin only —
// else ErrGatewayLimitSubject. It returns the stored row.
//
// RC-D S1 stub.
func UpsertGatewayLimit(ctx context.Context, pool *pgxpool.Pool, access ModelAccess, key GatewayLimitKey, v gatewaylimits.Values, stamp AuditStamp) (gatewaylimits.Row, error) {
	_, _, _, _, _, _ = ctx, pool, access, key, v, stamp
	return gatewaylimits.Row{}, ErrGatewayLimitsNotImplemented
}

// DeleteGatewayLimit deletes the row of key under the same access rule,
// stamped; it reports whether a row was deleted (false for a subject within
// access without that row).
//
// RC-D S1 stub.
func DeleteGatewayLimit(ctx context.Context, pool *pgxpool.Pool, access ModelAccess, key GatewayLimitKey, stamp AuditStamp) (bool, error) {
	_, _, _, _, _ = ctx, pool, access, key, stamp
	return false, ErrGatewayLimitsNotImplemented
}
