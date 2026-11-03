package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/GottZ/ctx/internal/pgxdb"
)

// auditEntity is the policy of one entity type on the audit read surface
// (GET /api/audit, design/06 §4.10).
type auditEntity struct {
	// serverAdminOnly: a reader that is not a server-admin gets an empty list
	// without a query, whatever metadata.tenant_id the rows carry.
	serverAdminOnly bool
}

// auditEntities is the closed set of entity types the audit read surface
// serves — data, not a switch. api_key and tenant_quota are written by the
// 160 triggers; gateway_limits (M-06-5), the three model_* types (01's
// trigger), consumer_usage (the gateway's Go writer) and instance (04's
// trigger) read as empty lists until their writers land. setting, secret and
// backend stay on their own surfaces (GET /api/settings/{key} and the
// admin-only manage actions) and, like every other type, answer
// ErrAuditEntity here.
var auditEntities = map[string]auditEntity{
	"api_key":        {},
	"tenant_quota":   {},
	"gateway_limits": {},
	"model_consumer": {},
	"model_key":      {},
	"model_grant":    {},
	"consumer_usage": {},
	"instance":       {serverAdminOnly: true},
}

// AuditEntities lists the served entity types, sorted.
func AuditEntities() []string {
	out := make([]string, 0, len(auditEntities))
	for e := range auditEntities {
		out = append(out, e)
	}
	sort.Strings(out)
	return out
}

// ErrAuditEntity: the entity type is not served by ListAudit — unknown, or
// read on its own surface (setting, secret, backend).
var ErrAuditEntity = errors.New("store: audit entity not served")

// AuditQuery selects audit rows of one entity type, newest first.
//
// The reader is part of the query, and its zero value reads nothing: a
// server-admin sets AllTenants; every other reader sets TenantID and sees only
// rows whose metadata.tenant_id is that tenant. AllTenants=false with an empty
// TenantID yields an empty list without a query (fail-closed) — forgetting the
// reader can never widen the view.
type AuditQuery struct {
	Entity     string    // one of AuditEntities()
	EntityKey  string    // "" = every key of the entity
	Since      time.Time // zero = no bound; otherwise rows strictly newer
	Limit      int       // ≥ 1; the caller clamps it (handler: audit.max_limit)
	AllTenants bool      // server-admin reader: no tenant predicate
	TenantID   string    // the reader's tenant when !AllTenants
}

// AuditRow is one context_settings_audit row as stored. The actor fields
// (APIKeyID, ActorLabel, metadata.actor_*) are unredacted: redaction for
// readers below server-admin is the handler's job (B-06-23).
type AuditRow struct {
	ID         string
	EntityKey  string
	Scope      string
	Action     string
	OldValue   json.RawMessage // nil when NULL
	NewValue   json.RawMessage // nil when NULL
	APIKeyID   *string
	ActorLabel *string
	Metadata   json.RawMessage
	CreatedAt  time.Time
}

// The statement is assembled from these constant fragments; caller values only
// ever travel as bind parameters. A tenant reader's statement carries the
// tenant predicate as SQL text — the role picks the text, never a NULL-able
// parameter (no `$n IS NULL OR …`), so a generic plan cannot fold the filter
// away and the server-admin statement has no tenant predicate to bypass.
const (
	auditSelect = `SELECT id::text, entity_key, scope, action, old_value, new_value,
       api_key_id::text, actor_label, metadata, created_at
  FROM context_settings_audit a
 WHERE entity_type = $1`
	auditKeyPredicate    = "\n   AND entity_key = $%d"
	auditSincePredicate  = "\n   AND created_at > $%d"
	auditTenantPredicate = "\n   AND metadata->>'tenant_id' = $%d"
	// created_at alone does not order a transaction's rows (now() is the
	// transaction start); id is uuidv7 and breaks the tie in write order.
	// Qualified: a bare "id" in ORDER BY would name the id::text output
	// column and sort the text form.
	auditOrderLimit = "\n ORDER BY a.created_at DESC, a.id DESC\n LIMIT $%d"
)

// auditListSQL builds the statement for one query. tenant is the canonical
// reader tenant, used only when !aq.AllTenants.
func auditListSQL(aq AuditQuery, tenant string) (string, []any) {
	var b strings.Builder
	b.WriteString(auditSelect)
	args := []any{aq.Entity}
	if aq.EntityKey != "" {
		args = append(args, aq.EntityKey)
		fmt.Fprintf(&b, auditKeyPredicate, len(args))
	}
	if !aq.Since.IsZero() {
		args = append(args, aq.Since)
		fmt.Fprintf(&b, auditSincePredicate, len(args))
	}
	if !aq.AllTenants {
		args = append(args, tenant)
		fmt.Fprintf(&b, auditTenantPredicate, len(args))
	}
	args = append(args, aq.Limit)
	fmt.Fprintf(&b, auditOrderLimit, len(args))
	return b.String(), args
}

// ListAudit returns the audit rows of one entity type for one reader, newest
// first (created_at DESC, id DESC). The entity range rides
// idx_settings_audit_entity (entity_type, created_at DESC); with an EntityKey
// the planner may take idx_settings_audit_key instead.
//
// A tenant reader's view is the intersection of the query with its tenant:
// a foreign or unknown EntityKey reads as the same empty list (no oracle,
// B-06-3); rows without metadata.tenant_id (a scope no tenant owns, psql rows)
// are server-admin-only.
func ListAudit(ctx context.Context, q pgxdb.Querier, aq AuditQuery) ([]AuditRow, error) {
	spec, ok := auditEntities[aq.Entity]
	if !ok {
		return nil, ErrAuditEntity
	}
	if aq.Limit < 1 {
		return nil, fmt.Errorf("store: audit: limit %d below 1", aq.Limit)
	}
	tenant := ""
	if !aq.AllTenants {
		if aq.TenantID == "" || spec.serverAdminOnly {
			return []AuditRow{}, nil
		}
		u, err := uuid.Parse(aq.TenantID)
		if err != nil {
			return nil, fmt.Errorf("store: audit: reader tenant is not a uuid: %w", err)
		}
		// metadata.tenant_id is written from a uuid column, i.e. in this form.
		tenant = u.String()
	}
	sql, args := auditListSQL(aq, tenant)
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("store: audit: list: %w", err)
	}
	defer rows.Close()
	out := []AuditRow{}
	for rows.Next() {
		var r AuditRow
		if err := rows.Scan(&r.ID, &r.EntityKey, &r.Scope, &r.Action, &r.OldValue, &r.NewValue,
			&r.APIKeyID, &r.ActorLabel, &r.Metadata, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: audit: scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: audit: rows: %w", err)
	}
	return out, nil
}
