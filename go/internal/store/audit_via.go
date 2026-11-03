package store

import (
	"context"
	"fmt"

	"github.com/GottZ/ctx/internal/pgxdb"
)

// AuditVia names the channel a write reached context_api_keys or
// context_tenant_quota through — metadata.via of the audit row the migration-160
// triggers write (audit_actor_meta, design/06 §3.3, B-06-14). The set is closed:
// a writer stamps one of the constants below; an unstamped write reads 'api'
// when an actor was stamped and 'sql' when none was, so a psql edit stays
// recognisable and the legitimate boot, provisioning and offboarding paths
// never look like one.
type AuditVia string

const (
	// ViaAPI: a write driven by an authenticated API request (manage actions).
	ViaAPI AuditVia = "api"
	// ViaBootstrap: the first-key mint at boot (BootstrapAdminKey), no actor.
	ViaBootstrap AuditVia = "bootstrap"
	// ViaProvision: the project-provision compound (ProvisionProject).
	ViaProvision AuditVia = "provision"
	// ViaOffboarding: the hard delete of a tenant's keys (PruneTenant).
	ViaOffboarding AuditVia = "offboarding"
	// ViaMigration: the snapshot rows migration 160 backfills (SQL only).
	ViaMigration AuditVia = "migration"
	// ViaSQL: no stamp at all — what the trigger records for a psql edit. No
	// writer sets it; it names the absence of one.
	ViaSQL AuditVia = "sql"
)

// auditVias is the closed set, pinned by TestAuditViaPin.
var auditVias = [...]AuditVia{ViaAPI, ViaBootstrap, ViaProvision, ViaOffboarding, ViaMigration, ViaSQL}

// valid reports whether v is one of the pinned channels.
func (v AuditVia) valid() bool {
	for _, x := range auditVias {
		if v == x {
			return true
		}
	}
	return false
}

// AuditStamp is the attribution one write transaction hands the audit triggers:
// the acting api key (ctx.api_key_id), the request id (ctx.request_id) and the
// channel (ctx.audit_via). An empty field stamps nothing: the trigger then
// records api_key_id NULL, no request_id, and derives via from whether an
// actor was stamped (the psql shape).
type AuditStamp struct {
	ActorKeyID string
	RequestID  string
	Via        AuditVia
}

// APIStamp is the stamp of a write driven by an authenticated API request.
func APIStamp(actorKeyID, requestID string) AuditStamp {
	return AuditStamp{ActorKeyID: actorKeyID, RequestID: requestID, Via: ViaAPI}
}

// stampAudit applies s to the transaction q runs in, the way setTxActor and
// SetTxRequestID do (set_config with is_local=true, reset at COMMIT/ROLLBACK),
// in one round trip; only the non-empty fields are set, so it never clears a
// stamp the caller's transaction already carries. q must be a pgx.Tx for the
// stamp to reach the write: on a pool the statement is its own transaction
// and the settings are gone before the next one. An unknown via is refused —
// a typo must not reach the audit trail as a channel.
func stampAudit(ctx context.Context, q pgxdb.Rower, s AuditStamp) error {
	if s.Via != "" && !s.Via.valid() {
		return fmt.Errorf("store: audit stamp: unknown via %q", s.Via)
	}
	if s.ActorKeyID == "" && s.RequestID == "" && s.Via == "" {
		return nil
	}
	var a, r, v *string
	if err := q.QueryRow(ctx,
		`SELECT CASE WHEN $1::text <> '' THEN set_config('ctx.api_key_id', $1::text, true) END,
		        CASE WHEN $2::text <> '' THEN set_config('ctx.request_id', $2::text, true) END,
		        CASE WHEN $3::text <> '' THEN set_config('ctx.audit_via', $3::text, true) END`,
		s.ActorKeyID, s.RequestID, string(s.Via)).Scan(&a, &r, &v); err != nil {
		return fmt.Errorf("store: audit stamp: %w", err)
	}
	return nil
}
