package handler

// Foreign-load detector (W06-8, design/06 §4.9, §5 B-06-26; A-E5): the proof
// that ctx is the only load source of a GPU engine it fronts.
//
// ctx is the admission authority of each GPU by deployment convention, not by
// port authentication (A-E5): any LAN client that talks to the engine port
// directly runs in the engine's FCFS ahead of ctx's own priority order, and
// every ctx-internal counter still reads green. The detector closes that blind
// spot by READING the engine's own metrics (E-A5-2 forbids a ctx metrics
// export, not reading a foreign one) and comparing them with what ctx itself
// put on the engine:
//
//	(a) per probe round: the requests the engine holds — the larger of its
//	    scheduler's running requests and its in-flight HTTP requests on the
//	    model endpoints — against the dispatcher's held leases on that
//	    origin. Lease and engine lag only in the direction engine <= held (a
//	    lease is taken before the request is sent and released after it
//	    ends), so engine > held for status.foreign_load_hysteresis_ticks
//	    consecutive rounds is load ctx did not admit ⇒ suspected. The HTTP
//	    gauge is what still sees foreign requests waiting in a saturated
//	    engine's queue, where the scheduler gauge reads running = held.
//	(b) per hour: the engine's model-endpoint RESPONSE counter delta against
//	    the LLM-usage rollup's duration_n of every backend row on that origin
//	    (ctx's own logged calls) ⇒ suspected_hourly beyond
//	    max(status.foreign_load_hourly_min_abs, ratio × ctx requests). Both
//	    sides count a call when it ENDS (ctx inserts its row after the call),
//	    so calls in flight across the hour boundary land in the same hour on
//	    both sides — the arrival counter http_requests_total would not.
//
// Activation is data: metadata.foreign_load_probe = "sglang" on a '_global'
// backend row (backends.ForeignLoadProbe). The pull is ASYNC: buildCheap only
// kicks a single-flight round and returns, the status read serves the last
// landed round — a hung engine never carries the status latency (the
// dbStatsBuild posture). The section is server-admin-only: running requests of
// a shared GPU are a live activity oracle over every tenant's load.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/httpx"
	"github.com/jackc/pgx/v5/pgxpool"
)

// foreignLoadProfile is the /metrics vocabulary of one probe kind — data, not
// logic: a second engine (vLLM, llama.cpp) is a second row here plus its kind
// in backends.ForeignLoadProbe, no parser change.
type foreignLoadProfile struct {
	// running is the scheduler gauge of requests the engine runs right now,
	// summed over every label set (one per TP/PP/EP rank on SGLang).
	running string
	// active is the gauge of in-flight HTTP requests by endpoint: one per
	// HTTP request — the unit of a dispatcher lease — from arrival to the
	// response, queued ones included. Optional: an engine build without it
	// is compared on running alone.
	active string
	// responses is the counter of finished HTTP responses by endpoint, the
	// hourly side: it counts a call when it ends, like ctx's llmlog row.
	responses string
	// endpointLabel names the endpoint label of active and responses; only
	// modelEndpoints count — /health, /metrics (this probe's own GET) and the
	// info routes never reach the model and are not in the rollup either.
	// Unknown endpoints are ignored, never counted.
	endpointLabel  string
	modelEndpoints map[string]bool
}

// foreignLoadProfiles is pinned against the live Spark body of 2026-11-03:
// `sglang:num_running_reqs{engine_type="unified",model_name="qwen38-27b",…} 0.0`,
// `sglang:http_requests_active{endpoint="/v1/chat/completions",method="POST"} 0.0`
// and `sglang:http_responses_total{endpoint="/v1/chat/completions",method="POST",status_code="200"}`
// next to /health, /metrics, /model_info, /get_server_info, /server_info,
// /freeze_gc. (sglang:http_requests_total counts on ARRIVAL — 20389 requests
// against 20388 responses with one /metrics GET in flight — and is not read.)
var foreignLoadProfiles = map[string]foreignLoadProfile{
	backends.ForeignLoadProbeSGLang: {
		running:       "sglang:num_running_reqs",
		active:        "sglang:http_requests_active",
		responses:     "sglang:http_responses_total",
		endpointLabel: "endpoint",
		modelEndpoints: map[string]bool{
			"/v1/chat/completions": true,
			"/v1/completions":      true,
			"/v1/embeddings":       true,
			"/v1/rerank":           true,
			"/generate":            true,
			"/encode":              true,
			"/classify":            true,
		},
	},
}

// foreignLoadBodyCap bounds the /metrics body the probe reads into memory: a
// memory guard against a misbehaving endpoint, not a policy — the live SGLang
// body is ~78 KB, so 4 MiB is ~50× headroom and is never reached by a sane
// engine. A body past it is a probe error, never a truncated parse.
const foreignLoadBodyCap = 4 << 20

// foreignLoadClients is the probe's own client pair from the wire clients'
// constructor (httpx.NewClients, the llm/embed/rerank posture): the SAME
// transport and dial path the backend's inference calls take, keyed on the
// same EgressGuarded predicate. Marked rows are '_global' by construction,
// so in practice this is the pooled, unguarded LAN client. No client timeout
// — every probe carries its own ctx deadline (status.foreign_load_timeout).
var foreignLoadClients = httpx.NewClients()

// foreignLoadRow is one /api/status foreign_load entry (server-admin only).
// Nullable fields are null until measured or when the current round could not
// measure them — never a fabricated 0 that would read as "clean".
type foreignLoadRow struct {
	// Target is the normalized origin (scheme://host:port) — the dispatcher's
	// key for the same engine.
	Target string `json:"target"`
	Probe  string `json:"probe"`
	// Backends names every backend row on this origin: the rows whose rollup
	// lines form ctx's side of the hourly comparison.
	Backends []string `json:"backends"`
	// Running is the scheduler gauge, Active the in-flight HTTP requests on
	// the model endpoints (null when the engine does not expose them); the
	// comparison uses the larger of the two.
	Running *int64 `json:"running"`
	Active  *int64 `json:"active"`
	// Held is the dispatcher's held leases on the origin, null when the target
	// is pass-through (no slots / dispatcher disabled) or no dispatcher is
	// wired: then only the hourly comparison applies.
	Held *int `json:"held"`
	// Delta is max(running, active) − held.
	Delta     *int64     `json:"delta"`
	Suspected bool       `json:"suspected"`
	Since     *time.Time `json:"since"`
	// SuspectedHourly is the verdict of the last evaluated hour (Hourly).
	SuspectedHourly bool             `json:"suspected_hourly"`
	Hourly          *foreignLoadHour `json:"hourly"`
	// ProbeError is the failure class of the last round ("timeout",
	// "unreachable", "http status 401", "metric family absent: …",
	// "api_key_ref unresolved", …) — a closed vocabulary, never request
	// material or a secret; null after a successful round.
	ProbeError *string    `json:"probe_error"`
	ProbedAt   *time.Time `json:"probed_at"`
}

// foreignLoadHour is the last hour the hourly comparison could evaluate.
type foreignLoadHour struct {
	Hour           time.Time `json:"hour"`
	EngineRequests int64     `json:"engine_requests"`
	CtxRequests    int64     `json:"ctx_requests"`
}

// foreignLoadTarget is one probed origin, derived from the pool snapshot at
// the tick.
type foreignLoadTarget struct {
	origin string
	kind   string
	// names: every backend row name on this origin, any scope, any state.
	names []string
	// rollupKeys: the rollup backend_name values of those rows — the row name
	// and, for rows a writer logged with backend_name NULL, the raw base_url
	// (the rollup keys on COALESCE(backend_name, host)).
	rollupKeys []string
	bearer     string
	// unresolvedRef: the marked row names an api_key_ref whose secret did not
	// resolve at the last pool reload — probing without it would only earn a
	// 401 that hides the real cause.
	unresolvedRef bool
	guarded       bool
}

// foreignLoadTargets derives the probed origins from one pool snapshot: every
// enabled, not profile-disabled row whose ForeignLoadProbe resolves (shared
// rows only), grouped by normalized origin. A row parked by an active disable
// profile (the eject case) pauses its probe: the GPU is lent out on purpose.
func foreignLoadTargets(rows []backends.Backend, disabledBy map[string]string) []foreignLoadTarget {
	byOrigin := map[string]*foreignLoadTarget{}
	for i := range rows {
		b := &rows[i]
		kind := b.ForeignLoadProbe()
		if kind == "" || !b.Enabled || disabledBy[b.ID] != "" {
			continue
		}
		origin, err := dispatch.NormalizeOrigin(b.Host)
		if err != nil {
			continue
		}
		t := byOrigin[origin]
		if t == nil {
			t = &foreignLoadTarget{origin: origin, kind: kind, guarded: b.EgressGuarded()}
			byOrigin[origin] = t
		}
		// The first keyed marked row (pool order: scope, name) with a resolved
		// secret supplies the Bearer; none at all ⇒ no Authorization header.
		if b.APIKeyRef != "" && t.bearer == "" {
			t.unresolvedRef = b.APIKey == ""
			t.bearer = b.APIKey
		}
	}
	if len(byOrigin) == 0 {
		return nil
	}
	for i := range rows {
		origin, err := dispatch.NormalizeOrigin(rows[i].Host)
		if t := byOrigin[origin]; err == nil && t != nil {
			t.names = append(t.names, rows[i].Name)
			t.rollupKeys = append(t.rollupKeys, rows[i].Name, rows[i].Host)
		}
	}
	out := make([]foreignLoadTarget, 0, len(byOrigin))
	for _, t := range byOrigin {
		t.names = sortedUnique(t.names)
		t.rollupKeys = sortedUnique(t.rollupKeys)
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].origin < out[j].origin })
	return out
}

func sortedUnique(in []string) []string {
	sort.Strings(in)
	out := in[:0]
	for i, s := range in {
		if i == 0 || s != in[i-1] {
			out = append(out, s)
		}
	}
	return out
}

// foreignLoadSettings is the hot policy of one round (A-E2: all four values
// are registry keys, status.foreign_load_*).
type foreignLoadSettings struct {
	timeout    time.Duration
	hysteresis int
	minAbs     int64
	minRatio   float64
	// grace is derived, not configured: how late the first sample of an hour
	// may come and still stand for the hour boundary, and how long after the
	// boundary the hourly rollup read waits. The wait is for llmlog inserts
	// whose transaction began before the boundary (created_at = transaction
	// start) and commits after it. One tick plus one probe timeout is what a
	// continuously ticking collector needs to land a round after the
	// boundary.
	grace time.Duration
}

func foreignLoadSettingsOf(cfg *config.Config) foreignLoadSettings {
	tick := cfg.Events.TickInterval
	if tick <= 0 {
		tick = 5 * time.Second // cheapNow's fallback for the same key
	}
	hyst := cfg.Status.ForeignLoadHysteresisTicks
	if hyst < 1 {
		hyst = 1 // V37 refuses 0 at boot and on settings writes; a literal test config is the only source
	}
	return foreignLoadSettings{
		timeout:    cfg.Status.ForeignLoadTimeout,
		hysteresis: hyst,
		minAbs:     int64(cfg.Status.ForeignLoadHourlyMinAbs),
		minRatio:   cfg.Status.ForeignLoadHourlyMinRatio,
		grace:      tick + cfg.Status.ForeignLoadTimeout,
	}
}

// foreignLoadReading is one parsed /metrics body.
type foreignLoadReading struct {
	running   int64
	active    int64
	hasActive bool
	responses int64
}

// foreignLoadProbe is the outcome of one GET: a reading, or errText (the
// status-field class) plus the raw cause for the log line.
type foreignLoadProbe struct {
	reading foreignLoadReading
	errText string
	cause   error
}

// foreignLoadMark is the counter sample that stands for an hour boundary.
type foreignLoadMark struct {
	hour      time.Time
	responses int64
	ok        bool
}

// foreignLoadState is the per-origin detector memory between rounds.
type foreignLoadState struct {
	target          foreignLoadTarget
	running         *int64
	held            *int
	delta           *int64
	suspected       bool
	since           *time.Time
	excess, clean   int // consecutive rounds with engine > held / engine <= held
	probeErr        string
	probedAt        *time.Time
	mark            foreignLoadMark
	active          *int64
	pending         *foreignLoadHour // an hour whose engine delta is known, ctx side still to read
	hourly          *foreignLoadHour
	suspectedHourly bool
	hourlyReadFail  bool // the last due rollup read failed (WARN once, not per round)
}

// foreignLoadDetector holds the per-origin state, the published rows and the
// single-flight flag of the async probe round.
type foreignLoadDetector struct {
	dispatch DispatchSource
	// hourlySum reads ctx's own logged calls of one hour from the rollup; nil
	// (no pool) ⇒ the hourly comparison never runs.
	hourlySum func(ctx context.Context, hour time.Time, rollupKeys []string) (int64, error)
	now       func() time.Time
	logger    *slog.Logger // nil ⇒ slog.Default() at call time

	inflight atomic.Bool
	mu       sync.Mutex
	states   map[string]*foreignLoadState
	order    []string
	rows     atomic.Pointer[[]foreignLoadRow]
}

func newForeignLoadDetector(pool *pgxpool.Pool, src DispatchSource) *foreignLoadDetector {
	d := &foreignLoadDetector{dispatch: src, now: time.Now, states: map[string]*foreignLoadState{}}
	if pool != nil {
		d.hourlySum = func(ctx context.Context, hour time.Time, keys []string) (int64, error) {
			return queryForeignLoadHourly(ctx, pool, hour, keys)
		}
	}
	return d
}

func (d *foreignLoadDetector) log() *slog.Logger {
	if d.logger != nil {
		return d.logger
	}
	return slog.Default()
}

// kickForeignLoad is buildCheap's hook: derive the marked targets from the
// pool snapshot and start a round. Never blocks on I/O.
func (c *StatusCollector) kickForeignLoad(cfg *config.Config) {
	if c.foreignLoad == nil || c.backendPool == nil {
		return
	}
	c.foreignLoad.kick(foreignLoadTargets(c.backendPool.Snapshot(), c.backendPool.DisabledBy()), foreignLoadSettingsOf(cfg))
}

// kick syncs the target set (a newly marked origin shows up at once, with
// null readings; an unmarked one disappears) and starts ONE async round
// unless one is still in flight — a hung engine therefore costs at most one
// goroutine for at most one timeout, however often the status ticks.
func (d *foreignLoadDetector) kick(targets []foreignLoadTarget, set foreignLoadSettings) {
	d.mu.Lock()
	d.syncLocked(targets)
	d.mu.Unlock()
	if len(targets) == 0 || !d.inflight.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer d.inflight.Store(false)
		d.round(context.Background(), targets, set)
	}()
}

func (d *foreignLoadDetector) syncLocked(targets []foreignLoadTarget) {
	keep := make(map[string]bool, len(targets))
	d.order = d.order[:0]
	for _, t := range targets {
		keep[t.origin] = true
		d.order = append(d.order, t.origin)
		if st := d.states[t.origin]; st != nil {
			st.target = t
		} else {
			d.states[t.origin] = &foreignLoadState{target: t}
		}
	}
	for origin := range d.states {
		if !keep[origin] {
			delete(d.states, origin)
		}
	}
	d.publishLocked()
}

// round is one probe pass over every target: the dispatcher's held view
// before and after, and in between — all in parallel — the GETs and the
// hourly rollup reads that are due; then one state update under the lock.
func (d *foreignLoadDetector) round(ctx context.Context, targets []foreignLoadTarget, set foreignLoadSettings) {
	now := d.now()
	due := d.dueHours(now, set)
	reads := make([]foreignLoadRead, len(due))

	before, wired := d.heldView()
	probes := make([]foreignLoadProbe, len(targets))
	var wg sync.WaitGroup
	for i := range targets {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			probes[i] = fetchForeignLoad(ctx, targets[i], set.timeout)
		}(i)
	}
	for i := range due {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			qctx, cancel := context.WithTimeout(ctx, set.timeout)
			defer cancel()
			reads[i] = due[i]
			reads[i].n, reads[i].err = d.hourlySum(qctx, due[i].hour, due[i].keys)
		}(i)
	}
	wg.Wait()
	after, _ := d.heldView()

	log := d.log()
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, r := range reads {
		if st := d.states[r.origin]; st != nil {
			st.settleRead(r, set, log)
		}
	}
	for i, t := range targets {
		st := d.states[t.origin]
		if st == nil {
			continue // unmarked while the round ran
		}
		held, known := mergeHeld(before, after, wired, t.origin)
		st.observe(now, probes[i], held, known, set, log)
	}
	d.publishLocked()
}

// foreignLoadRead is one due hourly rollup read and its outcome.
type foreignLoadRead struct {
	origin string
	hour   time.Time
	keys   []string
	n      int64
	err    error
}

// dueHours lists every pending hour whose end boundary lies at least one
// grace back. Pending hours come from EARLIER rounds only (observe sets them
// after this list is taken), so a read never happens in the round that
// sampled the boundary.
func (d *foreignLoadDetector) dueHours(now time.Time, set foreignLoadSettings) []foreignLoadRead {
	if d.hourlySum == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var due []foreignLoadRead
	for origin, st := range d.states {
		if p := st.pending; p != nil && now.Sub(p.Hour.Add(time.Hour)) >= set.grace {
			due = append(due, foreignLoadRead{origin: origin, hour: p.Hour, keys: st.target.rollupKeys})
		}
	}
	return due
}

// settleRead folds one hourly rollup read into the state: a failure keeps
// the hour pending for the next round (the next boundary replaces it) and
// WARNs on the state change only — a database that stays down must not log
// once per round.
func (s *foreignLoadState) settleRead(r foreignLoadRead, set foreignLoadSettings, log *slog.Logger) {
	if r.err != nil {
		if !s.hourlyReadFail {
			log.Warn("status: foreign load hourly rollup read failed", "target", r.origin, "hour", r.hour, "error", r.err)
		}
		s.hourlyReadFail = true
		return
	}
	if s.hourlyReadFail {
		log.Info("status: foreign load hourly rollup read recovered", "target", r.origin)
		s.hourlyReadFail = false
	}
	s.settleHour(r.n, set, log)
}

func (d *foreignLoadDetector) heldView() (dispatch.Snapshot, bool) {
	if d.dispatch == nil {
		return dispatch.Snapshot{}, false
	}
	return d.dispatch.Snapshot(), true
}

// heldOf reads the held leases of origin from one dispatcher snapshot. known
// is false for a pass-through target (dispatcher disabled, or the origin has
// no slots): its leases are not counted in Held, so engine > held would be
// ctx's own traffic. An origin absent from an ENABLED dispatcher's snapshot
// never saw an Acquire since boot (targets are created lazily) — 0 leases,
// and that 0 is a fact; with the dispatcher disabled every target is
// pass-through, absent or not.
func heldOf(snap dispatch.Snapshot, origin string) (int, bool) {
	for i := range snap.Targets {
		if snap.Targets[i].Origin == origin {
			return snap.Targets[i].Held, snap.Enabled && snap.Targets[i].Slots > 0
		}
	}
	return 0, snap.Enabled
}

// mergeHeld takes the larger held count of the two snapshots around the GETs:
// a lease taken or released while the GET was in flight then counts in ctx's
// favour, which keeps the lag in the engine <= held direction the comparison
// relies on.
func mergeHeld(before, after dispatch.Snapshot, wired bool, origin string) (int, bool) {
	if !wired {
		return 0, false
	}
	hb, kb := heldOf(before, origin)
	ha, ka := heldOf(after, origin)
	return max(hb, ha), kb && ka
}

// observe folds one probe round into the state.
func (s *foreignLoadState) observe(now time.Time, p foreignLoadProbe, held int, heldKnown bool, set foreignLoadSettings, log *slog.Logger) {
	stamp := now
	s.probedAt = &stamp
	if p.errText != "" {
		if s.probeErr == "" {
			log.Warn("status: foreign load probe failed", "target", s.target.origin, "error", p.errText, "cause", p.cause)
		}
		s.probeErr = p.errText
		s.running, s.active, s.held, s.delta = nil, nil, nil, nil
		s.excess, s.clean = 0, 0 // an unmeasured round breaks "consecutive"
		return
	}
	if s.probeErr != "" {
		log.Info("status: foreign load probe recovered", "target", s.target.origin)
		s.probeErr = ""
	}
	running := p.reading.running
	s.running = &running
	engine := running
	s.active = nil
	if p.reading.hasActive {
		active := p.reading.active
		s.active = &active
		engine = max(engine, active)
	}
	s.observeHour(now, p.reading.responses, set)
	if !heldKnown {
		s.held, s.delta = nil, nil
		s.excess, s.clean = 0, 0
		if s.suspected {
			log.Warn("status: foreign load cleared", "target", s.target.origin, "reason", "held is no longer comparable (pass-through target)")
			s.suspected, s.since = false, nil
		}
		return
	}
	s.compare(now, engine, held, set, log)
}

// compare is check (a) with the symmetric hysteresis: N consecutive rounds of
// engine > held enter suspected, N consecutive rounds of engine <= held
// leave it — a bursty foreign client must not toggle a WARN pair per tick.
// engine is max(running, active) of the round.
func (s *foreignLoadState) compare(now time.Time, engine int64, held int, set foreignLoadSettings, log *slog.Logger) {
	h := held
	delta := engine - int64(held)
	s.held, s.delta = &h, &delta
	if engine > int64(held) {
		s.excess, s.clean = s.excess+1, 0
	} else {
		s.excess, s.clean = 0, s.clean+1
	}
	switch {
	case !s.suspected && s.excess >= set.hysteresis:
		since := now
		s.suspected, s.since = true, &since
		log.Warn("status: foreign load suspected", "target", s.target.origin, "engine", engine, "held", held,
			"delta", delta, "rounds", s.excess)
	case s.suspected && s.clean >= set.hysteresis:
		log.Warn("status: foreign load cleared", "target", s.target.origin, "engine", engine, "held", held,
			"suspected_since", *s.since)
		s.suspected, s.since = false, nil
	}
}

// observeHour records the response-counter sample that stands for the current hour's
// start boundary: the first successful reading of the hour, if it came within
// grace of the boundary. When the previous hour's start sample exists too,
// that hour's engine delta is known and the hour is pending until the rollup
// side is read (readDueHours). A counter that went backwards is an engine
// restart: that hour is not comparable and is skipped.
func (s *foreignLoadState) observeHour(now time.Time, responses int64, set foreignLoadSettings) {
	hour := now.UTC().Truncate(time.Hour)
	if s.mark.ok && !s.mark.hour.Before(hour) {
		return
	}
	if now.Sub(hour) > set.grace {
		return
	}
	if prev := hour.Add(-time.Hour); s.mark.ok && s.mark.hour.Equal(prev) && responses >= s.mark.responses {
		s.pending = &foreignLoadHour{Hour: prev, EngineRequests: responses - s.mark.responses}
	}
	s.mark = foreignLoadMark{hour: hour, responses: responses, ok: true}
}

// settleHour is check (b) for the pending hour: ctxRequests is the rollup's
// duration_n over the origin's rows, the engine side its response delta. Only an engine EXCESS counts — a deficit
// means ctx logged attempts the engine never counted (refused before the
// HTTP layer), not foreign load.
func (s *foreignLoadState) settleHour(ctxRequests int64, set foreignLoadSettings, log *slog.Logger) {
	p := s.pending
	if p == nil {
		return
	}
	s.pending = nil
	tolerance := math.Max(float64(set.minAbs), set.minRatio*float64(ctxRequests))
	excess := p.EngineRequests - ctxRequests
	suspected := float64(excess) > tolerance
	s.hourly = &foreignLoadHour{Hour: p.Hour, EngineRequests: p.EngineRequests, CtxRequests: ctxRequests}
	if suspected != s.suspectedHourly {
		msg := "status: foreign load hourly mismatch"
		if !suspected {
			msg = "status: foreign load hourly mismatch cleared"
		}
		log.Warn(msg, "target", s.target.origin, "hour", p.Hour, "engine_requests", p.EngineRequests,
			"ctx_requests", ctxRequests, "tolerance", tolerance)
	}
	s.suspectedHourly = suspected
}

// publishLocked swaps in a fresh row slice; a published slice is never
// mutated again, so assemble can hand it out without copying. No target ⇒ nil
// ⇒ the status section is absent ("nothing measured" never reads as clean).
func (d *foreignLoadDetector) publishLocked() {
	if len(d.order) == 0 {
		d.rows.Store(nil)
		return
	}
	rows := make([]foreignLoadRow, 0, len(d.order))
	for _, origin := range d.order {
		st := d.states[origin]
		row := foreignLoadRow{
			Target: origin, Probe: st.target.kind, Backends: append([]string{}, st.target.names...),
			Running: st.running, Active: st.active, Held: st.held, Delta: st.delta,
			Suspected: st.suspected, Since: st.since,
			SuspectedHourly: st.suspectedHourly, ProbedAt: st.probedAt,
		}
		if st.hourly != nil {
			h := *st.hourly
			row.Hourly = &h
		}
		if st.probeErr != "" {
			e := st.probeErr
			row.ProbeError = &e
		}
		rows = append(rows, row)
	}
	d.rows.Store(&rows)
}

// section is the assemble read: nil (section absent) without a detector or
// without a probed target.
func (d *foreignLoadDetector) section() *[]foreignLoadRow {
	if d == nil {
		return nil
	}
	p := d.rows.Load()
	if p == nil {
		return nil
	}
	rows := *p
	return &rows
}

// fetchForeignLoad is one GET <origin>/metrics under the probe timeout.
func fetchForeignLoad(ctx context.Context, t foreignLoadTarget, timeout time.Duration) foreignLoadProbe {
	prof, ok := foreignLoadProfiles[t.kind]
	if !ok {
		return foreignLoadProbe{errText: "unknown probe kind"}
	}
	if t.unresolvedRef {
		return foreignLoadProbe{errText: "api_key_ref unresolved"}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.origin+"/metrics", nil)
	if err != nil {
		return foreignLoadProbe{errText: "bad origin", cause: err}
	}
	if t.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+t.bearer)
	}
	resp, err := foreignLoadClients.For(t.guarded).Do(req)
	if err != nil {
		return foreignLoadProbe{errText: transportClass(ctx, err), cause: err}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return foreignLoadProbe{errText: "http status " + strconv.Itoa(resp.StatusCode)}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, foreignLoadBodyCap+1))
	if err != nil {
		return foreignLoadProbe{errText: transportClass(ctx, err), cause: err}
	}
	if len(body) > foreignLoadBodyCap {
		return foreignLoadProbe{errText: fmt.Sprintf("metrics body over %d bytes", foreignLoadBodyCap)}
	}
	reading, err := parseForeignLoadMetrics(body, prof)
	if err != nil {
		return foreignLoadProbe{errText: err.Error()}
	}
	return foreignLoadProbe{reading: reading}
}

func transportClass(ctx context.Context, err error) string {
	var ne net.Error
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) ||
		(errors.As(err, &ne) && ne.Timeout()) {
		return "timeout"
	}
	return "unreachable"
}

// parseForeignLoadMetrics reads the families of prof out of a Prometheus
// text body. Contract: only lines whose metric name equals one of the family
// names are parsed; every other line — other families, comments, garbage, a
// malformed sample — is skipped. Values are summed over every label set (the
// running gauge) or over the model-endpoint label sets (active, responses);
// NaN/Inf samples are skipped. A family counts as present once a
// "# TYPE <name>" line or a sample names it, so an engine that declared its
// counter but has not served a request yet reads 0. running and responses are
// required — an engine that does not speak this vocabulary is an error naming
// the missing family; active is optional (hasActive).
func parseForeignLoadMetrics(body []byte, prof foreignLoadProfile) (foreignLoadReading, error) {
	sums := map[string]float64{}
	seen := map[string]bool{}
	wanted := map[string]bool{prof.running: true, prof.active: true, prof.responses: true}
	for len(body) > 0 {
		var line []byte
		if i := bytes.IndexByte(body, '\n'); i >= 0 {
			line, body = body[:i], body[i+1:]
		} else {
			line, body = body, nil
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		if line[0] == '#' {
			if f := bytes.Fields(line); len(f) >= 3 && string(f[1]) == "TYPE" && wanted[string(f[2])] {
				seen[string(f[2])] = true
			}
			continue
		}
		end := bytes.IndexAny(line, "{ \t")
		if end <= 0 || !wanted[string(line[:end])] {
			continue
		}
		name := string(line[:end])
		labels, value, ok := parseMetricSample(line[end:])
		if !ok {
			continue
		}
		seen[name] = true
		if name != prof.running {
			if ep, ok := metricLabel(labels, prof.endpointLabel); !ok || !prof.modelEndpoints[ep] {
				continue
			}
		}
		sums[name] += value
	}
	var missing []string
	for _, f := range []string{prof.running, prof.responses} {
		if !seen[f] {
			missing = append(missing, f)
		}
	}
	if len(missing) > 0 {
		return foreignLoadReading{}, fmt.Errorf("metric family absent: %s", strings.Join(missing, ", "))
	}
	return foreignLoadReading{
		running:   int64(math.Round(sums[prof.running])),
		active:    int64(math.Round(sums[prof.active])),
		hasActive: seen[prof.active],
		responses: int64(math.Round(sums[prof.responses])),
	}, nil
}

// parseMetricSample splits the part of a sample line after the metric name
// into its label set (without braces) and value; a trailing timestamp is
// ignored.
func parseMetricSample(rest []byte) ([]byte, float64, bool) {
	var labels []byte
	if rest[0] == '{' {
		end := labelSetEnd(rest)
		if end < 0 {
			return nil, 0, false
		}
		labels, rest = rest[1:end], rest[end+1:]
	}
	f := bytes.Fields(rest)
	if len(f) == 0 {
		return nil, 0, false
	}
	v, err := strconv.ParseFloat(string(f[0]), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return nil, 0, false
	}
	return labels, v, true
}

// labelSetEnd returns the index of the '}' closing the label set that starts
// at b[0] == '{', honouring quoted values and their escapes; -1 if unclosed.
func labelSetEnd(b []byte) int {
	inQuote := false
	for i := 1; i < len(b); i++ {
		switch c := b[i]; {
		case inQuote && c == '\\':
			i++
		case c == '"':
			inQuote = !inQuote
		case !inQuote && c == '}':
			return i
		}
	}
	return -1
}

// metricLabel returns the unescaped value of key in a label set
// (k="v",k2="v2"; escapes \\ \" \n); ok=false when absent or malformed.
func metricLabel(labels []byte, key string) (string, bool) {
	for {
		labels = bytes.TrimLeft(labels, " \t,")
		if len(labels) == 0 {
			return "", false
		}
		eq := bytes.IndexByte(labels, '=')
		if eq < 0 {
			return "", false
		}
		k := string(bytes.TrimSpace(labels[:eq]))
		rest := bytes.TrimLeft(labels[eq+1:], " \t")
		if len(rest) == 0 || rest[0] != '"' {
			return "", false
		}
		val, n, ok := unquoteLabelValue(rest)
		if !ok {
			return "", false
		}
		if k == key {
			return val, true
		}
		labels = rest[n:]
	}
}

// unquoteLabelValue reads one quoted label value at b[0] == '"' and returns
// it unescaped plus the number of bytes consumed (closing quote included).
func unquoteLabelValue(b []byte) (string, int, bool) {
	var val []byte
	for i := 1; i < len(b); i++ {
		switch c := b[i]; {
		case c == '\\' && i+1 < len(b):
			i++
			if b[i] == 'n' {
				val = append(val, '\n')
			} else {
				val = append(val, b[i])
			}
		case c == '"':
			return string(val), i + 1, true
		default:
			val = append(val, c)
		}
	}
	return "", 0, false
}

// queryForeignLoadHourly is ctx's side of the hourly comparison: the rollup's
// duration_n — one per wire call that reached a backend (K9 rejection rows
// carry no duration and are excluded) — of one hour bucket over the origin's
// backend_name values. The rollup stores those cut to 100 characters
// (migration 159), so the keys are cut the same way IN SQL — one definition of
// the cut, Postgres' own left(). One bucket over idx_llm_usage_bucket (or the
// unique index, also bucket_start-led): a handful of rows per hour, not a scan.
func queryForeignLoadHourly(ctx context.Context, pool *pgxpool.Pool, hour time.Time, rollupKeys []string) (int64, error) {
	var n int64
	err := pool.QueryRow(ctx, `
		SELECT COALESCE(sum(duration_n), 0)::bigint
		  FROM context_llm_usage_hourly
		 WHERE bucket_start = $1
		   AND backend_name IN (SELECT left(k, 100) FROM unnest($2::text[]) AS k)`,
		hour, rollupKeys).Scan(&n)
	return n, err
}
