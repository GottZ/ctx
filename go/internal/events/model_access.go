package events

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// channelModelAccessWrite is fired by the migration 165 triggers on
// context_model_keys (revoked_at, expires_at, delete), context_model_consumers
// (active, delete), context_tenants (status, delete) and
// context_tenant_scopes (update, delete): a write that can end the validity
// of a model key. Payload: identity only (table, op, id, and per table
// consumer_id / expires_at / tenant_id / scope) — never a hash or a policy
// value.
const channelModelAccessWrite = "ctx_model_access_write"

// ModelAccessSink receives the ctx_model_access_write signal: the in-flight
// registry of the model-key door (handler.InflightRegistry, W01-8), which
// ends the running /v1 requests of a key that stopped authenticating.
// internal/handler imports this package, so the registry reaches the
// listener through this interface (the scheduler-setter pattern), never
// through an import. Neither method may block: both run on the listener's
// one goroutine, which serves every channel.
type ModelAccessSink interface {
	// ModelAccessNotify takes one payload.
	ModelAccessNotify(payload string)
	// ModelAccessBacklog is called on every (re)connect of the listener: a
	// NOTIFY sent while it was away is lost, so the sink checks everything
	// it holds (design/01 B29).
	ModelAccessBacklog()
}

// ModelAccessHandler forwards ctx_model_access_write to the scheduler's
// sink. The sink is read per notification: the router installs it after
// the scheduler (and with it the listener) has started — until then, and
// without a model API, the channel is inert.
type ModelAccessHandler struct {
	scheduler *Scheduler
}

// HandleNotification is called by pgxlisten for each NOTIFY on
// ctx_model_access_write.
func (h *ModelAccessHandler) HandleNotification(_ context.Context, n *pgconn.Notification, _ *pgx.Conn) error {
	if s := h.scheduler.modelAccessSink(); s != nil {
		s.ModelAccessNotify(n.Payload)
	}
	return nil
}

// HandleBacklog asks the sink for a sweep after every (re)connect.
func (h *ModelAccessHandler) HandleBacklog(context.Context, string, *pgx.Conn) error {
	if s := h.scheduler.modelAccessSink(); s != nil {
		s.ModelAccessBacklog()
	}
	return nil
}

// SetModelAccessSink installs the receiver of ctx_model_access_write (W01-8).
// Unlike the registries of SetModelAliasRegistry it may be called after Run:
// the model-key door is built with the router, after the scheduler started
// (cmd/ctxd/main.go), so the listener reads the sink per notification.
func (s *Scheduler) SetModelAccessSink(sink ModelAccessSink) {
	s.modelAccess.Store(&sink)
}

func (s *Scheduler) modelAccessSink() ModelAccessSink {
	if p := s.modelAccess.Load(); p != nil {
		return *p
	}
	return nil
}
