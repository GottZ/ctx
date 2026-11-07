package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GottZ/ctx/internal/auth"
)

// The in-flight registry of the model-key door (design/01 §4.6 "Widerruf
// wirkt", masterplan K22; wave W01-8): every request the door accepts is
// registered at stage 8 with the cancel function of its context, under its
// model key, consumer, tenant and tenant_scope, and leaves the registry when
// the door's handler returns (a defer: a panic included). A key that stops
// authenticating ends the requests running on it — cancel with
// auth.ErrCredentialRevoked, and the key's hash leaves the door's
// known-good set (W01-6 review N2), so its next request pays the probe token
// again. The routes read the cause (context.Cause) and answer 401
// invalid_api_key, a committed stream its error frame.
//
// Four triggers, each ending what it finds dead:
//
//   - the local revocation (model-key-revoke, model-consumer-delete) right
//     after its commit, synchronously: CancelKey / CancelConsumer, whose
//     count is the action's cancelled_inflight — the requests of THIS
//     process (one process per database is the single-process assumption of
//     the dispatcher and the meter; another ctx process on the same database
//     ends its own through the NOTIFY);
//   - the NOTIFY channel ctx_model_access_write (migration 165:
//     psql and other processes included): the registered keys the payload
//     touches are checked with ctx_model_auth_live, a key's new expires_at
//     re-arms its expiry timer at once;
//   - the listener's backlog after a reconnect (a NOTIFY sent while the
//     listener was away is lost): a sweep at once (design/01 B29);
//   - a periodic sweep every gateway.inflight_sweep_interval: ONE
//     ctx_model_auth_live statement over the keys of all running requests,
//     whatever their number — the second line behind the NOTIFY. A failed
//     sweep ends nothing (fail-open for this line only, WARN throttled).
//
// Expiry needs no query: each key has one timer at the earliest expires_at
// its running requests were authenticated with (or a NOTIFY moved it to);
// at that instant the requests whose expiry has passed end.
//
// The limit of the mechanism (design/01 §4.6 "Grenze"): it checks
// liveness, gates 1-4 of ctx_model_auth. A narrowed policy — models,
// prompt_sensitivity, tenant_scope, admission lane, fair weight — applies
// from the next request; a running one keeps what it was admitted with.
//
// The registry books nothing in the in-flight budget: an entry is a few
// strings and two words, bounded by one per running request (the bytes are
// named in the wave report).

// inflightSweepFloor is the shortest sweep period: the floor V49 enforces on
// gateway.inflight_sweep_interval. A zero interval (a settings snapshot
// without the key, as in test rigs) sweeps at the floor instead of spinning.
const inflightSweepFloor = time.Second

// inflightEntry is one running request. Every field but ended and expires
// is set at registration and never changes; ended and expires are guarded
// by the registry's mutex.
type inflightEntry struct {
	keyID, consumerID, tenantID, scope string
	hash                               string
	cancel                             context.CancelCauseFunc
	seq                                uint64     // registration order: a check ends only entries older than its statement
	expires                            *time.Time // the key's expiry as last known; nil = none
	ended                              bool       // canceled with ErrCredentialRevoked already
}

// inflightKey is the entries of one model key and the key's expiry timer.
type inflightKey struct {
	entries map[*inflightEntry]struct{}
	timer   *time.Timer // nil = no entry carries an expiry
	at      time.Time   // when timer fires
}

// InflightRegistry is the door's registry of running requests; one per
// door, so one per process (ModelDoor.Inflight).
type InflightRegistry struct {
	mu         sync.Mutex
	byKey      map[string]*inflightKey
	byConsumer map[string]map[*inflightEntry]struct{}
	byTenant   map[string]map[*inflightEntry]struct{}
	byScope    map[string]map[*inflightEntry]struct{}
	entries    int
	timers     int
	seq        uint64 // last registration's sequence number

	pending  map[string]struct{} // key ids a NOTIFY touched, for the worker
	sweepDue bool                // the backlog asked for a sweep
	wake     chan struct{}       // cap 1: the worker has work

	// dead is auth.DeadModelKeys over the door's pool (ctx_model_auth_live).
	dead func(ctx context.Context, keyIDs []string) ([]string, error)
	// every is gateway.inflight_sweep_interval, read per period (hot).
	every func() time.Duration
	// forget takes a hash out of the door's known-good set.
	forget func(hash string)
	now    func() time.Time
	log    *slog.Logger

	// touches counts every revocation signal the registry received (a
	// NOTIFY, a backlog, a local cancel). The door reads it before its
	// database check (TouchMark) and hands the mark to register: a signal
	// that arrived while the check ran found no entry to end, so the new
	// entry's key is checked once more.
	touches atomic.Uint64

	cancelled   atomic.Uint64 // requests ended, every trigger
	sweeps      atomic.Uint64 // ctx_model_auth_live statements run
	sweepErrors atomic.Uint64
	lastWarn    atomic.Int64
}

func newInflightRegistry(dead func(context.Context, []string) ([]string, error), every func() time.Duration,
	forget func(string), now func() time.Time, log *slog.Logger) *InflightRegistry {
	return &InflightRegistry{
		byKey: map[string]*inflightKey{}, byConsumer: map[string]map[*inflightEntry]struct{}{},
		byTenant: map[string]map[*inflightEntry]struct{}{}, byScope: map[string]map[*inflightEntry]struct{}{},
		pending: map[string]struct{}{}, wake: make(chan struct{}, 1),
		dead: dead, every: every, forget: forget, now: now, log: log,
	}
}

// touchMark is the signal count before the door's database check.
func (r *InflightRegistry) touchMark() uint64 { return r.touches.Load() }

// register files a request the door accepted; the returned function takes
// it out again (idempotent). cancel ends the request's context. mark is
// touchMark from before the request's database check: when a revocation
// signal arrived since, it could not see this entry (the check had not
// returned yet), so the entry's key goes to the worker for one liveness
// check — the window between the check's snapshot and the registration is
// closed without a query per request.
func (r *InflightRegistry) register(ca *auth.ConsumerAuth, hash string, cancel context.CancelCauseFunc, mark uint64) (unregister func()) {
	e := &inflightEntry{keyID: ca.ModelKeyID, consumerID: ca.ConsumerID, tenantID: ca.TenantID, scope: ca.TenantScope,
		hash: hash, cancel: cancel}
	if ca.KeyExpiresAt != nil {
		at := *ca.KeyExpiresAt
		e.expires = &at
	}
	r.mu.Lock()
	k := r.byKey[e.keyID]
	if k == nil {
		k = &inflightKey{entries: map[*inflightEntry]struct{}{}}
		r.byKey[e.keyID] = k
	}
	k.entries[e] = struct{}{}
	addIndex(r.byConsumer, e.consumerID, e)
	addIndex(r.byTenant, e.tenantID, e)
	if e.scope != "" {
		addIndex(r.byScope, e.scope, e)
	}
	r.entries++
	r.seq++
	e.seq = r.seq
	if e.expires != nil && (k.timer == nil || e.expires.Before(k.at)) {
		r.armLocked(e.keyID, k, *e.expires)
	}
	raced := r.touches.Load() != mark
	if raced {
		r.pending[e.keyID] = struct{}{}
	}
	r.mu.Unlock()
	if raced {
		r.signal()
	}
	var once sync.Once
	return func() { once.Do(func() { r.unregister(e) }) }
}

func (r *InflightRegistry) unregister(e *inflightEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if k := r.byKey[e.keyID]; k != nil {
		delete(k.entries, e)
		if len(k.entries) == 0 {
			if k.timer != nil {
				k.timer.Stop()
				r.timers--
			}
			delete(r.byKey, e.keyID)
		}
	}
	dropIndex(r.byConsumer, e.consumerID, e)
	dropIndex(r.byTenant, e.tenantID, e)
	if e.scope != "" {
		dropIndex(r.byScope, e.scope, e)
	}
	r.entries--
}

func addIndex(m map[string]map[*inflightEntry]struct{}, id string, e *inflightEntry) {
	s := m[id]
	if s == nil {
		s = map[*inflightEntry]struct{}{}
		m[id] = s
	}
	s[e] = struct{}{}
}

func dropIndex(m map[string]map[*inflightEntry]struct{}, id string, e *inflightEntry) {
	if s := m[id]; s != nil {
		delete(s, e)
		if len(s) == 0 {
			delete(m, id)
		}
	}
}

// armLocked sets the key's timer to fire at at (one timer per key).
func (r *InflightRegistry) armLocked(keyID string, k *inflightKey, at time.Time) {
	d := max(at.Sub(r.now()), 0)
	if k.timer == nil {
		k.timer = time.AfterFunc(d, func() { r.expire(keyID) })
		r.timers++
	} else {
		k.timer.Reset(d)
	}
	k.at = at
}

// rearmLocked points the key's timer at the earliest expiry of its running
// entries, or stops it when none carries one.
func (r *InflightRegistry) rearmLocked(keyID string, k *inflightKey) {
	var next *time.Time
	for e := range k.entries {
		if !e.ended && e.expires != nil && (next == nil || e.expires.Before(*next)) {
			next = e.expires
		}
	}
	switch {
	case next != nil:
		r.armLocked(keyID, k, *next)
	case k.timer != nil:
		k.timer.Stop()
		k.timer = nil
		r.timers--
	}
}

// expire is a key's timer: the entries whose expiry has passed end, the
// timer moves to the next expiry.
func (r *InflightRegistry) expire(keyID string) {
	r.mu.Lock()
	k := r.byKey[keyID]
	if k == nil {
		r.mu.Unlock()
		return
	}
	now := r.now()
	var ends []*inflightEntry
	for e := range k.entries {
		if !e.ended && e.expires != nil && !e.expires.After(now) {
			ends = append(ends, r.endLocked(e))
		}
	}
	r.rearmLocked(keyID, k)
	r.mu.Unlock()
	r.finish(ends)
}

// endLocked marks e ended; finish cancels it outside the mutex.
func (r *InflightRegistry) endLocked(e *inflightEntry) *inflightEntry {
	e.ended = true
	return e
}

// finish cancels the request of every entry and forgets its hash.
func (r *InflightRegistry) finish(ends []*inflightEntry) {
	for _, e := range ends {
		e.cancel(auth.ErrCredentialRevoked)
		r.forget(e.hash)
	}
	r.cancelled.Add(uint64(len(ends)))
}

// CancelKey ends every running request of the model key in this process —
// the local revocation, called right after its commit (model-key-revoke).
// It returns how many requests were running on the key: each of them is
// ended now, or was already ending for the same reason.
func (r *InflightRegistry) CancelKey(keyID string) int {
	r.touches.Add(1)
	r.mu.Lock()
	k := r.byKey[keyID]
	if k == nil {
		r.mu.Unlock()
		return 0
	}
	var ends []*inflightEntry
	for e := range k.entries {
		if !e.ended {
			ends = append(ends, r.endLocked(e))
		}
	}
	n := len(k.entries)
	r.rearmLocked(keyID, k)
	r.mu.Unlock()
	r.finish(ends)
	return n
}

// CancelConsumer is CancelKey for every key of a consumer
// (model-consumer-delete: the deactivation revokes all of its keys).
func (r *InflightRegistry) CancelConsumer(consumerID string) int {
	r.touches.Add(1)
	r.mu.Lock()
	set := r.byConsumer[consumerID]
	var ends []*inflightEntry
	keys := map[string]struct{}{}
	for e := range set {
		if !e.ended {
			ends = append(ends, r.endLocked(e))
		}
		keys[e.keyID] = struct{}{}
	}
	n := len(set)
	for id := range keys {
		if k := r.byKey[id]; k != nil {
			r.rearmLocked(id, k)
		}
	}
	r.mu.Unlock()
	r.finish(ends)
	return n
}

// accessPayload is the NOTIFY payload of migration 165. expires_at is raw:
// absent (a delete, another table) and null (no expiry) differ.
type accessPayload struct {
	Table     string          `json:"table"`
	Op        string          `json:"op"`
	ID        string          `json:"id"`
	Scope     string          `json:"scope"`
	ExpiresAt json.RawMessage `json:"expires_at"`
}

// ModelAccessNotify takes one ctx_model_access_write payload (the
// listener's goroutine; it never blocks and never reads the database): the
// registered keys it touches go to the worker for a liveness check, a key's
// new expires_at re-arms its timer at once. A payload it cannot read asks
// for a sweep (fail-closed: the check is never skipped).
func (r *InflightRegistry) ModelAccessNotify(payload string) {
	r.touches.Add(1)
	var p accessPayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		r.warn("model auth: unreadable ctx_model_access_write payload; sweeping instead", "error", err)
		r.ModelAccessBacklog()
		return
	}
	r.mu.Lock()
	var touched map[*inflightEntry]struct{}
	switch p.Table {
	case "context_model_keys":
		if k := r.byKey[p.ID]; k != nil {
			touched = k.entries
			if p.Op == "UPDATE" && p.ExpiresAt != nil {
				r.moveExpiryLocked(p.ID, k, p.ExpiresAt)
			}
		}
	case "context_model_consumers":
		touched = r.byConsumer[p.ID]
	case "context_tenants":
		touched = r.byTenant[p.ID]
	case "context_tenant_scopes":
		touched = r.byScope[p.Scope]
	default:
		r.mu.Unlock()
		r.warn("model auth: ctx_model_access_write payload of an unknown table; sweeping instead", "table", p.Table)
		r.ModelAccessBacklog()
		return
	}
	n := 0
	for e := range touched {
		if !e.ended {
			r.pending[e.keyID] = struct{}{}
			n++
		}
	}
	r.mu.Unlock()
	if n > 0 {
		r.signal()
	}
}

// moveExpiryLocked sets the key's expiry from the payload on every entry
// and re-arms the timer (an expiry already past fires at once).
func (r *InflightRegistry) moveExpiryLocked(keyID string, k *inflightKey, raw json.RawMessage) {
	var at *time.Time
	if string(raw) != "null" {
		t, err := time.Parse(time.RFC3339Nano, unquote(raw))
		if err != nil {
			r.warn("model auth: unreadable expires_at in ctx_model_access_write; the key's timer stays", "error", err)
			return
		}
		at = &t
	}
	for e := range k.entries {
		e.expires = at
	}
	r.rearmLocked(keyID, k)
}

func unquote(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return string(raw)
	}
	return s
}

// ModelAccessBacklog asks for a sweep at once — the listener reconnected,
// and a NOTIFY sent while it was away is lost (design/01 B29). It never
// blocks.
func (r *InflightRegistry) ModelAccessBacklog() {
	r.touches.Add(1)
	r.mu.Lock()
	r.sweepDue = true
	r.mu.Unlock()
	r.signal()
}

func (r *InflightRegistry) signal() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Run is the registry's worker until ctx ends: the checks the NOTIFYs and
// the backlog ask for, and the periodic sweep every
// gateway.inflight_sweep_interval (read per period: a change applies from
// the next one).
func (r *InflightRegistry) Run(ctx context.Context) {
	t := time.NewTimer(r.period())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
			r.check(ctx, false)
		case <-t.C:
			r.check(ctx, true)
			t.Reset(r.period())
		}
	}
}

func (r *InflightRegistry) period() time.Duration {
	return max(r.every(), inflightSweepFloor)
}

// check runs ONE ctx_model_auth_live statement: over every registered key
// (all: the period, or a sweep the backlog asked for) or over the keys the
// NOTIFYs touched, and ends the requests of the dead ones that were
// registered before the statement — a request registered after it passed
// its own database check later (a tenant suspended and resumed in between
// must not end it). A failed statement ends nothing; the keys it was asked
// about are asked again with the next check.
func (r *InflightRegistry) check(ctx context.Context, all bool) {
	r.mu.Lock()
	all = all || r.sweepDue
	r.sweepDue = false
	upTo := r.seq // an entry registered after this passed its own check later
	var ids []string
	if all {
		ids = slices.Collect(maps.Keys(r.byKey))
		clear(r.pending)
	} else {
		ids = slices.Collect(maps.Keys(r.pending))
		clear(r.pending)
	}
	r.mu.Unlock()
	if len(ids) == 0 {
		return
	}
	dctx, cancel := context.WithTimeout(ctx, modelAuthDBDeadline)
	dead, err := r.dead(dctx, ids)
	cancel()
	r.sweeps.Add(1)
	if err != nil {
		r.sweepErrors.Add(1)
		if !all {
			r.mu.Lock()
			for _, id := range ids {
				r.pending[id] = struct{}{}
			}
			r.mu.Unlock()
		}
		if ctx.Err() == nil {
			r.warn("model auth: in-flight liveness check failed; running requests stay until the next check", "keys", len(ids), "error", err)
		}
		return
	}
	var ends []*inflightEntry
	r.mu.Lock()
	for _, id := range dead {
		k := r.byKey[id]
		if k == nil {
			continue
		}
		for e := range k.entries {
			if !e.ended && e.seq <= upTo {
				ends = append(ends, r.endLocked(e))
			}
		}
		r.rearmLocked(id, k)
	}
	r.mu.Unlock()
	r.finish(ends)
}

// warn logs at most once per modelAuthLogEvery.
func (r *InflightRegistry) warn(msg string, args ...any) {
	now := time.Now().UnixNano()
	last := r.lastWarn.Load()
	if now-last < int64(modelAuthLogEvery) || !r.lastWarn.CompareAndSwap(last, now) {
		return
	}
	r.log.Warn(msg+" (throttled to one line per second)", args...)
}

// inflightStats are the registry's counters, part of ModelAuthStats.
type inflightStats struct {
	running, keys, timers          int
	cancelled, sweeps, sweepErrors uint64
}

func (r *InflightRegistry) stats() inflightStats {
	r.mu.Lock()
	s := inflightStats{running: r.entries, keys: len(r.byKey), timers: r.timers}
	r.mu.Unlock()
	s.cancelled, s.sweeps, s.sweepErrors = r.cancelled.Load(), r.sweeps.Load(), r.sweepErrors.Load()
	return s
}
