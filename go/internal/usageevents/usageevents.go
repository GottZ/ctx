// Package usageevents records the usage events of the model API's consumers
// (wave W06-18, design/06 §4.10 row consumer_usage, §5 B-06-21): first_use,
// soft_limit and hard_limit as audit rows of entity_type 'consumer_usage' in
// context_settings_audit, written asynchronously and deduplicated, and the
// peer_bound notice of the federation's TOFU binding (cluster C3, F-W12).
//
// Contract:
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
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
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
// §4.10); store.AuditViaGateway names the same channel (this package stays a
// leaf over pgxdb and does not import store).
const Via = "gateway"

// QueueCap bounds the events waiting for the writer (named constant like
// llmlog's maxQueuedInserts; a full queue drops and counts).
const QueueCap = 1024

// Write-path bounds. Hardcoded on purpose, not settings: they bound the
// process's own bookkeeping, not anybody's policy (the llmlog precedent).
const (
	// maxConcurrentInserts caps the INSERTs one recorder runs at once. The
	// events are rare by construction — one row per key and window, the
	// in-memory set answers every repeat — so two slots only bind while the
	// database is slow, and then they keep a burst of first notes from
	// taking ctxd's shared 20-connection pool.
	maxConcurrentInserts = 2
	// insertTimeout bounds one INSERT; the clock starts when the event
	// leaves the queue, not at Note.
	insertTimeout = 5 * time.Second
	// lossWarnInterval throttles the loss WARN (full queue, failed INSERT,
	// unkeyable event) to one line per interval and recorder; each line
	// carries the totals, so the throttle hides nothing.
	lossWarnInterval = time.Minute
	// flushPoll is how often Flush looks at the pending count.
	flushPoll = 5 * time.Millisecond
	// keySep joins the parts of a dedupe key; a part containing it makes the
	// event unkeyable, so two different tuples never share a key.
	keySep = "|"
)

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

// levelConsumer is the only level whose rows carry used/limit (B6 doctrine:
// an envelope is not the consumer's own limit).
const levelConsumer = "consumer"

// Sink is the hook the gateway (meter adapter, access-log hook) and the
// federation's binding path (cluster C3, after a TOFU UPDATE that touched
// one row) call. Note must not block.
type Sink interface {
	Note(e Event)
}

// Stats are the recorder's counters (status section gateway.events),
// monotone since the recorder was made.
type Stats struct {
	// Noted counts the events queued for the writer.
	Noted int64
	// Deduped counts the repeats answered without a row: by the in-memory
	// set at Note, or by the INSERT's NOT EXISTS.
	Deduped int64
	// Written counts the rows inserted.
	Written int64
	// Dropped counts the events a full queue refused (QueueCap).
	Dropped int64
	// Failed counts the failed INSERTs (database error, insertTimeout) and
	// the events DedupeKey cannot key (incomplete — a caller bug).
	Failed int64
	// PeerBound counts the peer_bound notices (they write no row).
	PeerBound int64
}

// queued is one event waiting for the writer with its key and the month
// bucket the in-memory set holds it in (-1 = not held).
type queued struct {
	e     Event
	key   string
	month int
}

// Recorder is the Sink of production: Note queues, at most
// maxConcurrentInserts goroutines (started on demand, gone when the queue is
// empty) write, Flush waits for the queue to drain.
type Recorder struct {
	db  pgxdb.Execer
	now func() time.Time

	mu      sync.Mutex
	queue   []queued
	running int
	// seen is the in-memory stage of the dedupe: month index → keys. It
	// holds only the current and the previous month (swept at the month
	// change, swept records the month of the last sweep).
	seen  map[int]map[string]struct{}
	swept int

	pending  atomic.Int64 // events queued or being written
	lastWarn atomic.Int64 // unix nanos of the last loss WARN; 0 = none yet

	noted, deduped, written, dropped, failed, peerBound atomic.Int64
}

// NewRecorder returns a recorder writing through db; now is its clock (nil =
// time.Now).
func NewRecorder(db pgxdb.Execer, now func() time.Time) *Recorder {
	if now == nil {
		now = time.Now
	}
	return &Recorder{db: db, now: now, seen: map[int]map[string]struct{}{}}
}

// Note queues e. It never blocks and never fails the caller: a repeat is
// answered from the in-memory set, a full queue drops and counts, an
// unkeyable event counts as failed; peer_bound only counts and logs.
func (r *Recorder) Note(e Event) {
	if e.Kind == KindPeerBound {
		r.peerBound.Add(1)
		slog.Info("usageevents: peer bound", "consumer_id", e.ConsumerID,
			"tenant_id", e.TenantID, "peer_instance_id", e.PeerInstanceID)
		return
	}
	key := DedupeKey(e)
	if key == "" {
		r.failed.Add(1)
		r.warnLoss("usageevents: event without a dedupe key, not recorded", e, nil)
		return
	}
	cur := monthIndex(r.now())
	month := bucketOf(e, cur)

	r.mu.Lock()
	r.sweepLocked(cur)
	if r.heldLocked(month, key) {
		r.mu.Unlock()
		r.deduped.Add(1)
		return
	}
	if len(r.queue) >= QueueCap {
		r.mu.Unlock()
		// Not held: a later note of the same key tries again.
		r.dropped.Add(1)
		r.warnLoss("usageevents: queue full, event dropped", e, nil)
		return
	}
	r.holdLocked(month, key)
	r.pending.Add(1)
	r.queue = append(r.queue, queued{e: e, key: key, month: month})
	spawn := r.running < maxConcurrentInserts
	if spawn {
		r.running++
	}
	r.mu.Unlock()
	r.noted.Add(1)
	if spawn {
		go r.drain()
	}
}

// monthIndex numbers t's UTC month (year × 12 + month − 1).
func monthIndex(t time.Time) int {
	u := t.UTC()
	return u.Year()*12 + int(u.Month()) - 1
}

// bucketOf is the month whose in-memory set holds e's key: the window's
// month for soft_limit/hard_limit, the noting month for first_use. A window
// older than the previous month is not held (-1) — the INSERT's NOT EXISTS
// alone answers it, so the set stays bounded.
func bucketOf(e Event, cur int) int {
	m := cur
	if e.Kind != KindFirstUse {
		m = monthIndex(e.WindowStart)
	}
	if m < cur-1 {
		return -1
	}
	return m
}

// sweepLocked drops the months before the previous one at a month change.
func (r *Recorder) sweepLocked(cur int) {
	if cur == r.swept {
		return
	}
	for m := range r.seen {
		if m < cur-1 {
			delete(r.seen, m)
		}
	}
	r.swept = cur
}

func (r *Recorder) heldLocked(month int, key string) bool {
	_, ok := r.seen[month][key]
	return ok
}

func (r *Recorder) holdLocked(month int, key string) {
	if month < 0 {
		return
	}
	set := r.seen[month]
	if set == nil {
		set = map[string]struct{}{}
		r.seen[month] = set
	}
	set[key] = struct{}{}
}

// release forgets a key whose INSERT failed, so a later note retries it.
func (r *Recorder) release(q queued) {
	r.mu.Lock()
	delete(r.seen[q.month], q.key)
	r.mu.Unlock()
}

// drain writes queued events until the queue is empty, then exits. The
// running count drops under the lock that guards the queue, so an event
// queued after the last pop always finds a live drain or a free slot.
func (r *Recorder) drain() {
	for {
		r.mu.Lock()
		if len(r.queue) == 0 {
			r.running--
			r.queue = nil // release the backing array of a drained burst
			r.mu.Unlock()
			return
		}
		q := r.queue[0]
		r.queue[0] = queued{}
		r.queue = r.queue[1:]
		r.mu.Unlock()

		wrote, err := r.insert(q)
		switch {
		case err != nil:
			r.failed.Add(1)
			r.release(q)
			slog.Debug("usageevents: insert failed", "kind", q.e.Kind, "consumer_id", q.e.ConsumerID, "error", err)
			r.warnLoss("usageevents: insert failed, event lost", q.e, err)
		case wrote:
			r.written.Add(1)
		default:
			r.deduped.Add(1)
		}
		r.pending.Add(-1)
	}
}

// insertSQL writes one consumer_usage row unless a row with the same
// entity_key, action and metadata.dedupe exists (idx_settings_audit_key
// leads on entity_key). api_key_id, actor_label, old_value and new_value
// stay NULL.
const insertSQL = `INSERT INTO context_settings_audit (entity_type, entity_key, scope, action, metadata)
SELECT $1, $2, '_global', $3, $4::jsonb
 WHERE NOT EXISTS (
       SELECT 1 FROM context_settings_audit
        WHERE entity_key = $2 AND action = $3 AND entity_type = $1
          AND metadata->>'dedupe' = $5)`

// insert writes q's row; wrote is false when NOT EXISTS found the key.
func (r *Recorder) insert(q queued) (wrote bool, err error) {
	meta, err := json.Marshal(rowMetadata(q.e, q.key))
	if err != nil {
		return false, fmt.Errorf("usageevents: metadata: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), insertTimeout)
	defer cancel()
	tag, err := r.db.Exec(ctx, insertSQL, EntityType, q.e.ConsumerID, string(q.e.Kind), string(meta), q.key)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// rowMetadata is the row's metadata (package doc: row shape). Empty fields
// are left out; used/limit only on the consumer level of a limit event.
func rowMetadata(e Event, key string) map[string]any {
	m := map[string]any{"dedupe": key, "via": Via, "consumer_id": e.ConsumerID}
	for k, v := range map[string]string{
		"tenant_id": e.TenantID, "op": e.Op, "model_key_id": e.ModelKeyID,
	} {
		if v != "" {
			m[k] = v
		}
	}
	if e.Kind == KindFirstUse {
		return m
	}
	m["dimension"] = e.Dimension
	m["level"] = e.Level
	m["window_start"] = e.WindowStart.UTC().Format(time.RFC3339)
	if e.Level == levelConsumer {
		m["used"] = e.Used
		m["limit"] = e.Limit
	}
	return m
}

// warnLoss logs a lost event at WARN at most once per lossWarnInterval with
// the recorder's loss totals; the events in between are still counted.
func (r *Recorder) warnLoss(msg string, e Event, err error) {
	now := time.Now().UnixNano()
	last := r.lastWarn.Load()
	if last != 0 && now-last < int64(lossWarnInterval) {
		return
	}
	if !r.lastWarn.CompareAndSwap(last, now) {
		return
	}
	attrs := []any{"kind", e.Kind, "consumer_id", e.ConsumerID,
		"dropped_total", r.dropped.Load(), "failed_total", r.failed.Load()}
	if err != nil {
		attrs = append(attrs, "error", err)
	}
	slog.Warn(msg, attrs...)
}

// Flush waits until every event Note queued has been written (or counted as
// deduped or failed) and returns, or returns ctx's error with the pending
// count when ctx ends first (shutdown, tests). It says nothing about events
// noted after it returned.
func (r *Recorder) Flush(ctx context.Context) error {
	tick := time.NewTicker(flushPoll)
	defer tick.Stop()
	for r.pending.Load() > 0 {
		select {
		case <-ctx.Done():
			return fmt.Errorf("usageevents: flush with %d event(s) pending: %w", r.pending.Load(), ctx.Err())
		case <-tick.C:
		}
	}
	return nil
}

// Stats returns the counters.
func (r *Recorder) Stats() Stats {
	return Stats{
		Noted: r.noted.Load(), Deduped: r.deduped.Load(), Written: r.written.Load(),
		Dropped: r.dropped.Load(), Failed: r.failed.Load(), PeerBound: r.peerBound.Load(),
	}
}

// DedupeKey is the event's dedupe key (metadata.dedupe); "" for an event
// that is never written (peer_bound, an incomplete event, a part containing
// the separator). first_use: the model key alone; soft_limit/hard_limit:
// consumer × op × dimension × level × window start (RFC 3339, UTC) — never
// the figures, never anything the client controls (B-06-21).
func DedupeKey(e Event) string {
	var parts []string
	switch e.Kind {
	case KindFirstUse:
		if e.ConsumerID == "" {
			return ""
		}
		parts = []string{string(e.Kind), e.ModelKeyID}
	case KindSoftLimit, KindHardLimit:
		if e.ConsumerID == "" || e.WindowStart.IsZero() {
			return ""
		}
		parts = []string{string(e.Kind), e.ConsumerID, e.Op, e.Dimension, e.Level,
			e.WindowStart.UTC().Format(time.RFC3339)}
	default:
		return ""
	}
	for _, p := range parts {
		if p == "" || strings.Contains(p, keySep) {
			return ""
		}
	}
	return strings.Join(parts, keySep)
}
