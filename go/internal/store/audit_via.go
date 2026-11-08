package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/GottZ/ctx/internal/pgxdb"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// AuditVia names the channel a write reached context_api_keys or
// context_tenant_quota through — metadata.via of the audit row the migration-160
// triggers write (audit_actor_meta, design/06 §3.3, B-06-14) — or, for
// AuditViaGateway, the channel of the rows the gateway writes from Go. The set is closed:
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
	// AuditViaGateway: the consumer_usage rows of the model API's usage
	// events (W06-18, design/06 §4.10). Not a trigger stamp: the
	// usageevents recorder writes the rows itself and sets metadata.via to
	// usageevents.Via, the same literal (that package is a leaf over pgxdb
	// and does not import store).
	AuditViaGateway AuditVia = "gateway"
)

// auditVias is the closed set, pinned by TestAuditViaPin.
var auditVias = [...]AuditVia{ViaAPI, ViaBootstrap, ViaProvision, ViaOffboarding, ViaMigration, ViaSQL, AuditViaGateway}

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

// errStampOutsideTx refuses a stamp that could not reach the write it is meant
// for (see stampAudit).
var errStampOutsideTx = errors.New("store: audit stamp outside a transaction")

// stampAudit applies s to the transaction q runs in, the way setTxActor and
// SetTxRequestID do (set_config with is_local=true, reset at COMMIT/ROLLBACK),
// in one round trip; only the non-empty fields are set, so it never clears a
// stamp the caller's transaction already carries. A non-empty stamp needs q
// to be a pgx.Tx: on a pool the set_config statement is its own transaction
// and the settings are gone before the write, which would then read
// via='sql' without an actor although the caller stamped it — so that case
// is refused with errStampOutsideTx before anything is written. An empty
// stamp is a no-op on any handle. An unknown via is refused — a typo must not
// reach the audit trail as a channel. The actor is cast to uuid as it is set,
// so a malformed actor fails here (SQLSTATE 22P02) rather than at the next
// audited write, and ctx.api_key_id always holds the canonical spelling.
func stampAudit(ctx context.Context, q pgxdb.Rower, s AuditStamp) error {
	if s.Via != "" && !s.Via.valid() {
		return fmt.Errorf("store: audit stamp: unknown via %q", s.Via)
	}
	if s.ActorKeyID == "" && s.RequestID == "" && s.Via == "" {
		return nil
	}
	if _, inTx := q.(pgx.Tx); !inTx {
		return errStampOutsideTx
	}
	var a, r, v *string
	if err := q.QueryRow(ctx,
		`SELECT CASE WHEN $1::text <> '' THEN set_config('ctx.api_key_id', ($1::text)::uuid::text, true) END,
		        CASE WHEN $2::text <> '' THEN set_config('ctx.request_id', $2::text, true) END,
		        CASE WHEN $3::text <> '' THEN set_config('ctx.audit_via', $3::text, true) END`,
		s.ActorKeyID, s.RequestID, string(s.Via)).Scan(&a, &r, &v); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "22P02" {
			return fmt.Errorf("store: audit stamp: actor key id: %w", err)
		}
		return fmt.Errorf("store: audit stamp: %w", err)
	}
	return nil
}
