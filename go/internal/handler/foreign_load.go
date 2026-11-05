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
//	(a) per probe round: what the engine holds against what ctx holds on
//	    that origin, each gauge in its own unit (detector pairing, A03-W8):
//	    the scheduler's running requests (engine sequences) against the
//	    dispatcher's HeldWeight (Σw of the held leases — an external lease
//	    of weight w is w sequences: chat n, embedding inputs, rerank
//	    documents), and the in-flight HTTP requests on the model endpoints
//	    against Held (one HTTP request per lease). Lease and engine lag only
//	    in the direction engine <= held (a lease is taken before the request
//	    is sent and released after it ends), so running > HeldWeight or
//	    active > Held for status.foreign_load_hysteresis_ticks consecutive
//	    rounds is load ctx did not admit ⇒ suspected; without the HTTP gauge
//	    running alone is compared with HeldWeight. The HTTP gauge is what
//	    still sees foreign requests waiting in a saturated engine's queue,
//	    where the scheduler gauge reads running = HeldWeight.
//	(b) per hour: the engine's model-endpoint RESPONSE counter delta against
//	    the LLM-usage rollup's duration_n of every backend row on that origin
//	    (ctx's own logged calls) ⇒ suspected_hourly beyond
//	    max(status.foreign_load_hourly_min_abs, ratio × ctx requests). Both
//	    sides count a call when it ENDS (ctx inserts its row after the call),
//	    so calls in flight across the hour boundary land in the same hour on
//	    both sides — the arrival counter http_requests_total would not.
//
// Activation is data: metadata.foreign_load_probe = "sglang" on a '_global'
// backend row (backends.ForeignLoadProbe). The detector runs on its OWN
// cadence (RunForeignLoad, status.foreign_load_interval, W06-8b), not on
// status reads: it is A-E5's standard observation — ctx decides the priority
// order, so it has to watch for load past that order whether or not anyone
// has the dashboard open. Each tick kicks a single-flight round and returns;
// the status read serves the last landed round, so a hung engine never
// carries the status latency (the dbStatsBuild posture). The section is
// server-admin-only: running requests of a shared GPU are a live activity
// oracle over every tenant's load.

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
	// wired: then only the hourly comparison applies. HeldWeight is the sum
	// of their weights (engine sequences), null in the same cases; running
	// pairs with HeldWeight, active with Held.
	Held       *int `json:"held"`
	HeldWeight *int `json:"held_weight"`
	// Delta is max(running − held_weight, active − held), running −
	// held_weight without the active gauge.
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
	// start) and commits after it. The ticker wakes ON every boundary
	// (foreignLoadWait), so the boundary round normally lands within one GET
	// of it; when that kick is a no-op because a round is still in flight,
	// the next regular round starts within one interval and lands within one
	// probe timeout — interval + timeout is the bound.
	grace time.Duration
}

func foreignLoadSettingsOf(cfg *config.Config) foreignLoadSettings {
	hyst := cfg.Status.ForeignLoadHysteresisTicks
	if hyst < 1 {
		hyst = 1 // V37 refuses 0 at boot and on settings writes; a literal test config is the only source
	}
	return foreignLoadSettings{
		timeout:    cfg.Status.ForeignLoadTimeout,
		hysteresis: hyst,
		minAbs:     int64(cfg.Status.ForeignLoadHourlyMinAbs),
		minRatio:   cfg.Status.ForeignLoadHourlyMinRatio,
		grace:      foreignLoadIntervalOf(cfg) + cfg.Status.ForeignLoadTimeout,
	}
}

// foreignLoadIntervalOf is the ticker's period, status.foreign_load_interval.
// V37 refuses 0 at boot and on settings writes and V17 any negative, so the
// clamp to V37's 1 s floor only catches a literal test config, where a
// non-positive wait would spin the ticker.
func foreignLoadIntervalOf(cfg *config.Config) time.Duration {
	if iv := cfg.Status.ForeignLoadInterval; iv > 0 {
		return iv
	}
	return time.Second
}

// foreignLoadWait is the ticker's sleep before its next round: one interval,
// cut short at the next UTC hour boundary (the hourly check's own buckets).
// The cut lands the first round of every hour ON the boundary, so the
// boundary sample of check (b) stands for the boundary to within one GET
// instead of up to one interval after it, and the hourly precision no longer
// depends on the interval: with a plain period, an interval of 36 s already
// lets the boundary jitter reach a 1 % tolerance at any request rate.
func foreignLoadWait(now time.Time, interval time.Duration) time.Duration {
	if untilHour := now.UTC().Truncate(time.Hour).Add(time.Hour).Sub(now); untilHour < interval {
		return untilHour
	}
	return interval
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
	heldWeight      *int
	delta           *int64
	suspected       bool
	since           *time.Time
	excess, clean   int // consecutive rounds with / without an engine excess over the held counts
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
	// after is the ticker's sleep (RunForeignLoad): time.After in production,
	// a stepped channel in the ticker gates.
	after  func(time.Duration) <-chan time.Time
	logger *slog.Logger // nil ⇒ slog.Default() at call time

	inflight atomic.Bool
	mu       sync.Mutex
	states   map[string]*foreignLoadState
	order    []string
	rows     atomic.Pointer[[]foreignLoadRow]
}

func newForeignLoadDetector(pool *pgxpool.Pool, src DispatchSource) *foreignLoadDetector {
	d := &foreignLoadDetector{dispatch: src, now: time.Now, after: time.After, states: map[string]*foreignLoadState{}}
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

// RunForeignLoad is the detector's own cadence (W06-8b) and its ONLY round
// source: one kick per status.foreign_load_interval, plus one on every UTC
// hour boundary (foreignLoadWait), whether or not anyone reads /api/status.
// Before it, rounds followed the status collector, whose refresh is
// read-driven — headless there were no rounds, no WARN and practically never
// an hourly comparison, which made A-E5's standard observation depend on an
// open dashboard. Unlike the read-driven sources beside it (the dream-queue
// scan, the db section), the detector raises WARNs and gates a rollout
// (A03-W12 checks suspected = false), so it ticks on its own; buildCheap no
// longer kicks rounds at all, so the round cadence — and with it the
// hysteresis window (rounds × interval) and grace — is the same with or
// without an observer.
//
// The interval is read per tick (hot: a change applies from the next wait
// on). cmd/ctxd starts it once per process with the server lifecycle
// context, like the scheduler arms and the SSE hub; it returns when ctx ends,
// and the round in flight then is cancelled with it and records nothing
// (round). Without a probed row a tick is the pool snapshot scan plus the
// wait's timer: no GET, no round goroutine, no allocation in the kick itself.
// Blocks until ctx ends.
func (c *StatusCollector) RunForeignLoad(ctx context.Context) {
	d := c.foreignLoad
	if d == nil || c.backendPool == nil {
		return
	}
	slog.Info("status: foreign load ticker started")
	for {
		cfg := c.cfg.Snapshot() //nolint:forbidigo // MT 06 BLIND: the detector's cadence and policy (status.foreign_load_*) are global-only server telemetry, no tenant dimension.
		c.kickForeignLoad(ctx, cfg)
		select {
		case <-ctx.Done():
			return
		case <-d.after(foreignLoadWait(d.now(), foreignLoadIntervalOf(cfg))):
		}
	}
}

// kickForeignLoad is one tick of RunForeignLoad: derive the marked targets
// from the pool snapshot and start a round. Never blocks on I/O.
func (c *StatusCollector) kickForeignLoad(ctx context.Context, cfg *config.Config) {
	if c.foreignLoad == nil || c.backendPool == nil {
		return
	}
	c.foreignLoad.kick(ctx, foreignLoadTargets(c.backendPool.Snapshot(), c.backendPool.DisabledBy()), foreignLoadSettingsOf(cfg))
}

// kick syncs the target set (a newly marked origin shows up at once, with
// null readings; an unmarked one disappears) and starts ONE async round
// unless one is still in flight — a hung engine therefore costs at most one
// goroutine for at most one timeout, however short the interval. The round
// runs under ctx (the server lifecycle): after the stop no round starts.
func (d *foreignLoadDetector) kick(ctx context.Context, targets []foreignLoadTarget, set foreignLoadSettings) {
	if ctx.Err() != nil {
		return
	}
	d.mu.Lock()
	d.syncLocked(targets)
	d.mu.Unlock()
	if len(targets) == 0 || !d.inflight.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer d.inflight.Store(false)
		d.round(ctx, targets, set)
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
// A round whose ctx ended meanwhile (shutdown) records nothing: its GETs and
// reads were cut off, not answered, and a stop is no engine failure to WARN
// about or to break a hysteresis streak with.
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
	if ctx.Err() != nil {
		return
	}
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
		held, weight, known := mergeHeld(before, after, wired, t.origin)
		st.observe(now, probes[i], held, weight, known, set, log)
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
	s.settleHour(r.hour, r.n, set, log)
}

func (d *foreignLoadDetector) heldView() (dispatch.Snapshot, bool) {
	if d.dispatch == nil {
		return dispatch.Snapshot{}, false
	}
	return d.dispatch.Snapshot(), true
}

// heldOf reads the held leases of origin and the sum of their weights
// (HeldWeight) from one dispatcher snapshot. known is false for a
// pass-through target (dispatcher disabled, or the origin has no slots): its
// leases are not counted in Held, so engine > held would be ctx's own
// traffic. An origin absent from an ENABLED dispatcher's snapshot never saw
// an Acquire since boot (targets are created lazily) — 0 leases, and that 0
// is a fact; with the dispatcher disabled every target is pass-through,
// absent or not.
func heldOf(snap dispatch.Snapshot, origin string) (held, weight int, known bool) {
	for i := range snap.Targets {
		if ts := &snap.Targets[i]; ts.Origin == origin {
			return ts.Held, ts.HeldWeight, snap.Enabled && ts.Slots > 0
		}
	}
	return 0, 0, snap.Enabled
}

// mergeHeld takes the larger held count and the larger held weight of the
// two snapshots around the GETs, each on its own: a lease taken or released
// while the GET was in flight then counts in ctx's favour, which keeps the
// lag in the engine <= held direction the comparison relies on.
func mergeHeld(before, after dispatch.Snapshot, wired bool, origin string) (held, weight int, known bool) {
	if !wired {
		return 0, 0, false
	}
	hb, wb, kb := heldOf(before, origin)
	ha, wa, ka := heldOf(after, origin)
	return max(hb, ha), max(wb, wa), kb && ka
}

// observe folds one probe round into the state.
func (s *foreignLoadState) observe(now time.Time, p foreignLoadProbe, held, heldWeight int, heldKnown bool, set foreignLoadSettings, log *slog.Logger) {
	stamp := now
	s.probedAt = &stamp
	if p.errText != "" {
		if s.probeErr == "" {
			log.Warn("status: foreign load probe failed", "target", s.target.origin, "error", p.errText, "cause", p.cause)
		}
		s.probeErr = p.errText
		s.running, s.active, s.held, s.heldWeight, s.delta = nil, nil, nil, nil, nil
		s.excess, s.clean = 0, 0 // an unmeasured round breaks "consecutive"
		return
	}
	if s.probeErr != "" {
		log.Info("status: foreign load probe recovered", "target", s.target.origin)
		s.probeErr = ""
	}
	running := p.reading.running
	s.running = &running
	s.active = nil
	if p.reading.hasActive {
		active := p.reading.active
		s.active = &active
	}
	s.observeHour(now, p.reading.responses, set)
	if !heldKnown {
		s.held, s.heldWeight, s.delta = nil, nil, nil
		s.excess, s.clean = 0, 0
		if s.suspected {
			log.Warn("status: foreign load cleared", "target", s.target.origin, "reason", "held is no longer comparable (pass-through target)")
			s.suspected, s.since = false, nil
		}
		return
	}
	s.compare(now, held, heldWeight, set, log)
}

// compare is check (a) with the symmetric hysteresis: N consecutive rounds
// with an engine excess enter suspected, N consecutive rounds without one
// leave it — a bursty foreign client must not toggle a WARN pair per tick.
// The excess of a round is the detector pairing (A03-W8, DECISIONS A-E5 no.
// 3): running − heldWeight (sequences against the leases' summed weights)
// and, when the engine exposes the gauge, active − held (HTTP requests
// against leases); an excess is either one above 0, delta the larger of the
// two — so a weighted lease is not foreign load, while a foreign request
// (one more HTTP request) or a foreign sequence still is.
func (s *foreignLoadState) compare(now time.Time, held, heldWeight int, set foreignLoadSettings, log *slog.Logger) {
	h, hw := held, heldWeight
	delta := *s.running - int64(heldWeight)
	if s.active != nil {
		delta = max(delta, *s.active-int64(held))
	}
	s.held, s.heldWeight, s.delta = &h, &hw, &delta
	if delta > 0 {
		s.excess, s.clean = s.excess+1, 0
	} else {
		s.excess, s.clean = 0, s.clean+1
	}
	switch {
	case !s.suspected && s.excess >= set.hysteresis:
		since := now
		s.suspected, s.since = true, &since
		log.Warn("status: foreign load suspected", "target", s.target.origin, "running", *s.running, "active", optionalGauge(s.active),
			"held", held, "held_weight", heldWeight, "delta", delta, "rounds", s.excess)
	case s.suspected && s.clean >= set.hysteresis:
		log.Warn("status: foreign load cleared", "target", s.target.origin, "running", *s.running, "active", optionalGauge(s.active),
			"held", held, "held_weight", heldWeight, "suspected_since", *s.since)
		s.suspected, s.since = false, nil
	}
}

// optionalGauge is a nullable gauge as a log value: the number, or nil when
// the engine does not expose it (never a pointer address).
func optionalGauge(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

// observeHour records the response-counter sample that stands for the current hour's
// start boundary: the first successful reading of the hour, if it came within
// grace of the boundary. When the previous hour's start sample exists too,
// that hour's engine delta is known and the hour is pending until the rollup
// side is read (dueHours, settleRead). A counter that went backwards is an engine
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
// duration_n over the origin's rows, the engine side its response delta. It
// settles only the hour the rollup read was for: when the pending hour moved
// on meanwhile (a newer boundary replaced it), the read is discarded and the
// newer hour stays pending for its own read — never one hour's engine delta
// against another hour's rollup. Only an engine EXCESS counts — a deficit
// means ctx logged attempts the engine never counted (refused before the
// HTTP layer), not foreign load.
func (s *foreignLoadState) settleHour(hour time.Time, ctxRequests int64, set foreignLoadSettings, log *slog.Logger) {
	p := s.pending
	if p == nil || !p.Hour.Equal(hour) {
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
			Running: st.running, Active: st.active, Held: st.held, HeldWeight: st.heldWeight, Delta: st.delta,
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
// counter but has not served a request yet reads 0. Values are taken as the
// engine reports them: a sample beyond ±2^63 (never sent by a sane engine)
// converts to an undefined int64 and is not guarded against. running and responses are
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
