// Package usageevents records the usage events of the model API's consumers
// (wave W06-18, design/06 §4.10 row consumer_usage, §5 B-06-21): first_use,
// soft_limit and hard_limit as audit rows of entity_type 'consumer_usage' in
// context_settings_audit, written asynchronously and deduplicated, and the
// peer_bound notice of the federation's TOFU binding (cluster C3, F-W12).
//
// Contract (RC-D S1 — the Recorder is a fail-closed stub: Note drops,
// Flush answers ErrNotImplemented):
//
//   - One row per event key: first_use once per model key (the key's
//     consumer is entity_key, the key in metadata.model_key_id); soft_limit
//     and hard_limit once per consumer × op × dimension × level × window
//     (metadata.window_start = the UTC start of the day or month; the level
//     is part of the key so that the consumer's own limit and, later in the
//     same window, its tenant's envelope are two exceedances — S1 decision
//     after the sub-agent's finding). The key
//     never contains anything the client controls (no Ctx-Hop, no claimed
//     chain — B-06-21).
//   - Dedupe in two stages: an in-memory set (bounded to the current and
//     the previous month's windows, swept at the month change) answers
//     repeats without a statement; the INSERT itself is
//     INSERT … SELECT … WHERE NOT EXISTS (same entity_key, action and
//     metadata.dedupe) through idx_settings_audit_key, so a restart within
//     the window writes no second row for first_use (soft/hard duplicates
//     after a restart within one window are possible only when two replicas
//     race, design/06 §4.10).
//   - Row shape: entity_type 'consumer_usage', entity_key = consumer id,
//     action = the kind, scope '_global', metadata {dedupe, tenant_id,
//     consumer_id, op, dimension, level, window_start, used, limit,
//     model_key_id, via: 'gateway'}; api_key_id NULL, actor_label NULL —
//     /api/audit's tenant filter (metadata.tenant_id) and actor redaction
//     apply unchanged. Envelope events (level tenant/instance) carry no
//     used/limit values (B6 doctrine: not the consumer's own limit).
//   - peer_bound writes NO row: the TOFU UPDATE of
//     context_model_consumers.peer_instance_id is audited by the
//     model_consumer trigger of migration 161 in the binding's own
//     transaction (one row, new_value.peer_instance_id = the peer's instance
//     id) — design/06 §4.10 "kein zweiter Write"; GET /api/audit labels that
//     row action "peer_bound" for a server-admin (handler/audit.go). Note
//     counts the notice (Stats.PeerBound) and logs it once at INFO.
//   - Note never blocks and never fails the request: a full queue drops
//     (Stats.Dropped), a failed INSERT is counted (Stats.Failed).
package usageevents

import (
	"context"
	"errors"
	"time"

	"github.com/GottZ/ctx/internal/pgxdb"
)

// Kind is the event's action in the audit row.
type Kind string

// The event kinds (design/06 §4.10; peer_bound per RC-D-INDEX §4).
const (
	KindFirstUse  Kind = "first_use"
	KindSoftLimit Kind = "soft_limit"
	KindHardLimit Kind = "hard_limit"
	KindPeerBound Kind = "peer_bound"
)

// EntityType is the audit entity of the usage events (already in the
// /api/audit whitelist, internal/store/audit.go).
const EntityType = "consumer_usage"

// Via is the metadata.via of every row this package writes (design/06
// §4.10). S2 adds it to store.AuditVia as AuditViaGateway (pin 6 → 7).
const Via = "gateway"

// QueueCap bounds the events waiting for the writer (named constant like
// llmlog's maxQueuedInserts; a full queue drops and counts).
const QueueCap = 1024

// ErrNotImplemented is the answer of the RC-D S1 stubs.
var ErrNotImplemented = errors.New("usageevents: not implemented (RC-D S1 stub)")

// Event is one usage event.
type Event struct {
	Kind       Kind
	ConsumerID string
	TenantID   string
	// Op is chat, embed or rerank; "" for peer_bound.
	Op string
	// Dimension is daily_tokens, monthly_tokens or daily_requests
	// (soft_limit, hard_limit); "" otherwise.
	Dimension string
	// Level is consumer, tenant or instance (keyquota.BudgetLevel).
	Level string
	// WindowStart is the UTC start of the dimension's window.
	WindowStart time.Time
	// Used and Limit are the consumer level's figures (0 for envelopes).
	Used, Limit int64
	// ModelKeyID is the key of a first_use.
	ModelKeyID string
	// PeerInstanceID is the bound peer's instance id (peer_bound).
	PeerInstanceID string
}

// Sink is the hook the gateway (meter adapter, access-log hook) and the
// federation's binding path (cluster C3, after a TOFU UPDATE that touched
// one row) call. Note must not block.
type Sink interface {
	Note(e Event)
}

// Stats are the recorder's counters (status section gateway.events).
type Stats struct {
	Noted, Deduped, Written, Dropped, Failed, PeerBound int64
}

// Recorder is the Sink of production.
type Recorder struct {
	db  pgxdb.Execer
	now func() time.Time
}

// NewRecorder returns a recorder writing through db; now is its clock (nil =
// time.Now).
func NewRecorder(db pgxdb.Execer, now func() time.Time) *Recorder {
	if now == nil {
		now = time.Now
	}
	return &Recorder{db: db, now: now}
}

// Note queues e (S1 stub: drops it).
func (r *Recorder) Note(e Event) { _ = e }

// Flush writes every queued event and returns when the queue is empty or ctx
// ends (shutdown, tests). S1 stub.
func (r *Recorder) Flush(ctx context.Context) error {
	_ = ctx
	return ErrNotImplemented
}

// Stats returns the counters. S1 stub: zero.
func (r *Recorder) Stats() Stats { return Stats{} }

// DedupeKey is the event's dedupe key (metadata.dedupe); "" for an event
// that is never written (peer_bound, an incomplete event). S1 stub: "".
func DedupeKey(e Event) string {
	_ = e
	return ""
}
