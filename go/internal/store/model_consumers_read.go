package store

// The read path of the consumer administration (design/05 §3.2, wave W05-02
// folded into W01-5): the consumer list with its status filter, label prefix
// and keyset, one consumer with its live keys, and the audit trail of a
// consumer and its keys. Every filter runs in SQL, never after the page fetch
// (B-26); the status predicates take the clock and the warning window as
// parameters ($now, $warn), never now() in the SQL text, so the Go status
// function of the handler and these predicates can be held against each
// other at an injected time.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/pgxdb"
)

// The consumer statuses of design/05 §3.2, derived from the consumer's active
// flag and its live keys at a point in time.
const (
	ConsumerStatusActive   = "active"   // active, one live key, not expiring
	ConsumerStatusExpiring = "expiring" // active, one live key expiring within warn
	ConsumerStatusRotating = "rotating" // active, two live keys (a rotation's grace)
	ConsumerStatusNoKey    = "no_key"   // active, no live key
	ConsumerStatusInactive = "inactive" // deactivated
)

// ConsumerStatuses lists the five statuses in the order of design/05 §3.2.
func ConsumerStatuses() []string {
	return []string{ConsumerStatusActive, ConsumerStatusExpiring, ConsumerStatusRotating,
		ConsumerStatusNoKey, ConsumerStatusInactive}
}

// consumerStatusSQL is the SQL predicate of each status over the consumer c and
// the per-consumer key aggregate kk (live_n = live keys at $now, warn_hit = a
// live key expires at or before $now + $warn). The handler's consumerStatus is
// the Go form of the same table; the two are held against each other at an
// injected $now (TestConsumerStatusParity). ≥ 2 rather than = 2: I-K2 allows
// two live keys at most, and a third (only psql can make it) still reads as a
// rotation in both forms instead of matching no status.
var consumerStatusSQL = map[string]string{
	ConsumerStatusInactive: `NOT c.active`,
	ConsumerStatusNoKey:    `(c.active AND kk.live_n = 0)`,
	ConsumerStatusRotating: `(c.active AND kk.live_n >= 2)`,
	ConsumerStatusExpiring: `(c.active AND kk.live_n = 1 AND kk.warn_hit)`,
	ConsumerStatusActive:   `(c.active AND kk.live_n = 1 AND NOT kk.warn_hit)`,
}

// ConsumerCursor is the keyset position of a consumer page: the (created_at,
// id) of the last consumer of the previous page.
type ConsumerCursor struct {
	CreatedAt time.Time
	ID        string
}

// ListConsumersOptions narrows ListConsumers. The zero value lists the active
// consumers, all of them, judged at the process clock.
//
//   - Statuses: the statuses to list (ConsumerStatuses); empty = the active
//     consumers, or every consumer with IncludeInactive. The four statuses of
//     an active consumer together are the predicate c.active, and are sent as
//     such.
//   - LabelPrefix: label prefix, case-insensitive (ILIKE with %, _ and \
//     escaped); "" = no filter.
//   - After: the keyset position of the previous page; nil = the first page.
//   - Limit: the most rows to return; 0 = no limit. A page reader passes its
//     page size + 1 to learn whether a next page exists.
//   - Now, Warn: the clock and the expiry warning window of the status
//     predicates and the live-key test; a zero Now is the process clock.
type ListConsumersOptions struct {
	IncludeInactive bool
	Statuses        []string
	LabelPrefix     string
	After           *ConsumerCursor
	Limit           int
	Now             time.Time
	Warn            time.Duration
}

// consumerReadSelect is the shared row of ListConsumers and GetConsumer: the
// consumer, its tenant's slug, its creator, and one LEFT JOIN LATERAL over its
// keys (idx_model_keys_consumer_created) that aggregates the live keys (the
// whitelisted key fields — no hash, no plaintext), their count, the expiry
// warning, the chain length and the latest use, and one over its grants
// (migration 164, the primary key's range) that aggregates the granted alias
// ids in ConsumerGrantIDs' order. $3 is $now, $4 the warning window in
// microseconds.
const consumerReadSelect = `SELECT ` + sqlConsumerCols + `,
	       t.slug, a.label, a.tenant_id::text, COALESCE(a.is_admin, false),
	       kk.live, kk.total, kk.last_used, gg.ids
	  FROM context_model_consumers c
	  JOIN context_tenants t ON t.id = c.tenant_id
	  LEFT JOIN context_api_keys a ON a.id = c.created_by_key
	  LEFT JOIN LATERAL (
	       SELECT COALESCE(json_agg(json_build_object(
	                  'id', k.id, 'consumer_id', k.consumer_id, 'hint', k.key_hint,
	                  'created_at', k.created_at, 'expires_at', k.expires_at,
	                  'revoked_at', k.revoked_at, 'last_used_at', k.last_used_at,
	                  'rotated_from', k.rotated_from)
	                  ORDER BY k.created_at DESC, k.id DESC) FILTER (WHERE ` + sqlKeyLiveAt + `), '[]') AS live,
	              (count(*) FILTER (WHERE ` + sqlKeyLiveAt + `))::int AS live_n,
	              COALESCE(bool_or(` + sqlKeyLiveAt + ` AND k.expires_at <= $3::timestamptz + $4::bigint * interval '1 microsecond'), false) AS warn_hit,
	              count(*)::int AS total,
	              max(k.last_used_at) AS last_used
	         FROM context_model_keys k WHERE k.consumer_id = c.id) kk ON true
	  LEFT JOIN LATERAL (
	       SELECT COALESCE(array_agg(g.alias_id::text ORDER BY g.alias_id), '{}') AS ids
	         FROM context_model_consumer_grants g WHERE g.consumer_id = c.id) gg ON true`

// sqlKeyLiveAt is I-K2's "live" on the key alias k at the parameter $3.
const sqlKeyLiveAt = `(k.revoked_at IS NULL AND (k.expires_at IS NULL OR k.expires_at > $3::timestamptz))`

// listConsumersSQL assembles the statement for one list call. Every caller
// value travels as a bind parameter; the options only pick which constant
// fragments the text carries (no `$n IS NULL OR …`, precedent auditListSQL).
func listConsumersSQL(access ModelAccess, opts ListConsumersOptions) (string, []any, error) {
	admin, tenantArg := access.args()
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	args := []any{admin, tenantArg, now, opts.Warn.Microseconds()}
	var b strings.Builder
	b.WriteString(consumerReadSelect)
	b.WriteString("\n\t WHERE " + sqlListAccess)
	pred, err := consumerStatusPredicate(opts)
	if err != nil {
		return "", nil, err
	}
	if pred != "" {
		b.WriteString("\n\t   AND " + pred)
	}
	if opts.LabelPrefix != "" {
		args = append(args, escapeLike(opts.LabelPrefix)+"%")
		fmt.Fprintf(&b, "\n\t   AND c.label ILIKE $%d", len(args))
	}
	if opts.After != nil {
		args = append(args, opts.After.CreatedAt, opts.After.ID)
		fmt.Fprintf(&b, "\n\t   AND (c.created_at, c.id) < ($%d::timestamptz, $%d::uuid)", len(args)-1, len(args))
	}
	b.WriteString("\n\t ORDER BY c.created_at DESC, c.id DESC")
	if opts.Limit > 0 {
		args = append(args, opts.Limit)
		fmt.Fprintf(&b, "\n\t LIMIT $%d", len(args))
	}
	return b.String(), args, nil
}

// consumerStatusPredicate turns the options into the status part of the WHERE
// clause; "" = no status condition.
func consumerStatusPredicate(opts ListConsumersOptions) (string, error) {
	if len(opts.Statuses) == 0 {
		if opts.IncludeInactive {
			return "", nil
		}
		return "c.active", nil
	}
	want := map[string]bool{}
	for _, s := range opts.Statuses {
		if _, ok := consumerStatusSQL[s]; !ok {
			return "", fmt.Errorf("store: list consumers: unknown status %q", s)
		}
		want[s] = true
	}
	activeAll := want[ConsumerStatusActive] && want[ConsumerStatusExpiring] &&
		want[ConsumerStatusRotating] && want[ConsumerStatusNoKey]
	switch {
	case activeAll && want[ConsumerStatusInactive]:
		return "", nil
	case activeAll:
		return "c.active", nil
	}
	var parts []string
	for _, s := range ConsumerStatuses() {
		if want[s] {
			parts = append(parts, consumerStatusSQL[s])
		}
	}
	return "(" + strings.Join(parts, " OR ") + ")", nil
}

// escapeLike escapes the LIKE metacharacters of a literal prefix (the default
// escape character of ILIKE is the backslash).
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// ListConsumers lists the consumers in the caller's reach, newest first
// (created_at DESC, id DESC), in ONE statement, filtered by status, label
// prefix and keyset in SQL (see ListConsumersOptions). A malformed tenant in
// access reads as an empty list.
func ListConsumers(ctx context.Context, pool *pgxpool.Pool, access ModelAccess, opts ListConsumersOptions) ([]ModelConsumer, error) {
	sql, args, err := listConsumersSQL(access, opts)
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		if pgxdb.MalformedUUID(err) {
			return []ModelConsumer{}, nil
		}
		return nil, fmt.Errorf("model_consumers: list: %w", err)
	}
	defer rows.Close()
	out := []ModelConsumer{}
	for rows.Next() {
		c, err := scanConsumerRead(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		if pgxdb.MalformedUUID(err) {
			return []ModelConsumer{}, nil
		}
		return nil, fmt.Errorf("model_consumers: list: %w", err)
	}
	return out, nil
}

// GetConsumer reads one consumer in the caller's reach with its live keys at
// now (a zero now is the process clock), its chain length and latest use.
// Unknown, malformed and another tenant's id are the same
// ErrModelConsumerNotFound (no oracle).
func GetConsumer(ctx context.Context, pool *pgxpool.Pool, consumerID string, access ModelAccess, now time.Time) (ModelConsumer, error) {
	admin, tenantArg := access.args()
	if now.IsZero() {
		now = time.Now()
	}
	rows, err := pool.Query(ctx, consumerReadSelect+"\n\t WHERE c.id = $5::uuid AND "+sqlListAccess,
		admin, tenantArg, now, int64(0), consumerID)
	if err != nil {
		if pgxdb.MalformedUUID(err) {
			return ModelConsumer{}, ErrModelConsumerNotFound
		}
		return ModelConsumer{}, fmt.Errorf("model_consumers: get: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil && !pgxdb.MalformedUUID(err) {
			return ModelConsumer{}, fmt.Errorf("model_consumers: get: %w", err)
		}
		return ModelConsumer{}, ErrModelConsumerNotFound
	}
	return scanConsumerRead(rows)
}

// scanConsumerRead scans one row of consumerReadSelect.
func scanConsumerRead(rows pgx.Rows) (ModelConsumer, error) {
	var (
		c    ModelConsumer
		live []byte
	)
	dest := append(consumerDest(&c), &c.TenantSlug, &c.CreatedByLabel, &c.CreatedByTenantID,
		&c.CreatedByServerAdmin, &live, &c.KeysTotal, &c.LastUsedAt, &c.Models)
	if err := rows.Scan(dest...); err != nil {
		return ModelConsumer{}, fmt.Errorf("model_consumers: read scan: %w", err)
	}
	if err := json.Unmarshal(live, &c.Keys); err != nil {
		return ModelConsumer{}, fmt.Errorf("model_consumers: read keys: %w", err)
	}
	return c, nil
}

// consumerAuditEntities are the entity types of a consumer's audit trail:
// policy changes (the consumer is the entity), credential events (a key is),
// and the limits of design/06 keyed by the consumer id (V-06-e). The peer
// register of design/04 Rev. 0 (federation_peer) is gone (masterplan §8
// design/05 (3)).
var consumerAuditEntities = []string{"model_consumer", "model_key", "gateway_limits"}

// ConsumerAuditRow is one row of a consumer's audit trail: an AuditRow and its
// entity type, unredacted (the handler redacts, as for GET /api/audit).
type ConsumerAuditRow struct {
	EntityType string
	AuditRow
}

// ConsumerAuditTrail reads the newest audit rows whose entity_key is one of ids
// (a consumer and the keys of its chain) among consumerAuditEntities — newest
// first (created_at DESC, id DESC), at most limit (≥ 1), on
// idx_settings_audit_key. The reader is part of the query as in ListAudit: a
// server-admin (allTenants) reads every row, any other reader only rows whose
// metadata.tenant_id is tenantID (as SQL text, never a NULL-able parameter);
// an empty tenantID without allTenants reads nothing, without a query.
func ConsumerAuditTrail(ctx context.Context, pool *pgxpool.Pool, ids []string, allTenants bool, tenantID string, limit int) ([]ConsumerAuditRow, error) {
	if limit < 1 {
		return nil, fmt.Errorf("store: consumer audit trail: limit %d below 1", limit)
	}
	if len(ids) == 0 || (!allTenants && tenantID == "") {
		return []ConsumerAuditRow{}, nil
	}
	sql := `SELECT a.id::text, a.entity_type, a.entity_key, a.scope, a.action, a.old_value, a.new_value,
	       a.api_key_id::text, a.actor_label, a.metadata, a.created_at
	  FROM context_settings_audit a
	 WHERE a.entity_key = ANY($1::text[]) AND a.entity_type = ANY($2::text[])`
	args := []any{ids, consumerAuditEntities}
	if !allTenants {
		args = append(args, tenantID)
		sql += "\n\t   AND a.metadata->>'tenant_id' = $3"
	}
	args = append(args, limit)
	sql += fmt.Sprintf("\n\t ORDER BY a.created_at DESC, a.id DESC\n\t LIMIT $%d", len(args))
	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("model_consumers: audit trail: %w", err)
	}
	defer rows.Close()
	out := []ConsumerAuditRow{}
	for rows.Next() {
		var r ConsumerAuditRow
		if err := rows.Scan(&r.ID, &r.EntityType, &r.EntityKey, &r.Scope, &r.Action, &r.OldValue, &r.NewValue,
			&r.APIKeyID, &r.ActorLabel, &r.Metadata, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("model_consumers: audit trail scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("model_consumers: audit trail: %w", err)
	}
	return out, nil
}
