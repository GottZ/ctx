package handler

// Server-sent-events surface for GET /api/events (design 04 §3.6, W7 / G34).
//
// Topology. The status collector (W6) builds the snapshot at most once per
// tick; this hub diffs it ONCE per tick and fans the resulting events out to
// every connection — N admin panels cost one build + one diff, not N. Per
// connection only a bounded send mailbox + the initial full state remain. While
// the broadcast loop runs it is the collector's cache refresher, so concurrent
// GET /api/status polls serve that warm cache without their own DB work
// (StatusCollector.broadcasting).
//
// Lifecycle. The loop is connection-driven: the first subscriber starts it, and
// it stops itself one tick after the last subscriber leaves — no SSE clients,
// no O(n) dream-queue scans. A cancelled lifecycle context (server shutdown)
// ends both the loop and every connection handler so http.Server.Shutdown does
// not block on a stream that never goes idle.
//
// Testability mirrors the collector's queueDepthFn seam: the hub talks to a
// statusProvider interface and injectable llmcalls/authenticate funcs, so the
// cap / diff / broadcast / re-auth behaviour is exercised with fakes, no DB.
// The sseWriter is the K8 reuse vehicle for the F6 web-chat token stream (G37).

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/jackc/pgx/v5/pgxpool"
)

// sseWriteWindow is the rolling write-deadline budget per frame. http.Server's
// WriteTimeout (main.go: 120s) is an ABSOLUTE response deadline that would kill
// any long-lived stream at 120s; while the stream writes (diffs + heartbeats) it
// proves liveness, so every frame pushes the deadline one window ahead instead
// — the exact mechanism the query heartbeat uses against the same cap
// (query.go: heartbeatWriteWindow). Package var so tests can shrink it.
var sseWriteWindow = 90 * time.Second

// sseMailbox is the per-connection send buffer. A connection whose mailbox
// overflows cannot keep up with the broadcast; the hub drops it (it reconnects
// and re-fetches the full state) rather than stall the fan-out to everyone else.
const sseMailbox = 16

// statusProvider is the slice of *StatusCollector the SSE layer needs: serve a
// cached snapshot, drive a synchronous per-tick refresh, flip the broadcasting
// flag, and answer the DB-free liveness read behind the heartbeat.
// *StatusCollector satisfies it; tests inject a fake that returns canned
// snapshots without a pool.
//
// liveness is deliberately ctx-LESS: it must not query, cold-start or assemble
// (status.go), because it is called per connection on the ping timer — a
// cadence unrelated to the tick. A method without a context structurally cannot
// carry a DB deadline, which is the cheapest way to keep that property true.
type statusProvider interface {
	Snapshot(ctx context.Context) statusResponse
	refreshForBroadcast(ctx context.Context) statusResponse
	setBroadcasting(on bool)
	liveness() livenessStamp
}

// sseWriter frames server-sent events onto one connection and keeps the rolling
// write deadline ahead of the absolute server WriteTimeout. Every write is
// mutex-serialized: the diff fan-out, the keepalive heartbeat and the re-auth
// teardown can all target the same connection.
type sseWriter struct {
	w  http.ResponseWriter
	rc *http.ResponseController
	mu sync.Mutex
}

// newSSEWriter wraps a connection whose 200 stream header the caller has
// already committed, then probes flushability and arms the first write
// deadline. A non-flushable writer (a middleware ResponseWriter without
// Unwrap()) is logged loudly, not silently: without per-frame flushes the
// stream buffers and a reverse proxy 504s it.
func newSSEWriter(w http.ResponseWriter) *sseWriter {
	sw := &sseWriter{w: w, rc: http.NewResponseController(w)}
	if err := sw.rc.Flush(); err != nil {
		slog.Warn("sse: response writer not flushable, stream will buffer", "error", err)
	}
	_ = sw.rollDeadline()
	return sw
}

// rollDeadline pushes the connection write deadline one window ahead.
func (sw *sseWriter) rollDeadline() error {
	return sw.rc.SetWriteDeadline(time.Now().Add(sseWriteWindow))
}

// event writes one named event frame (id optional) carrying pre-marshalled
// compact JSON. json.Marshal never emits a raw newline (control bytes are
// escaped), so a single data: line is always a valid SSE frame. Rolls the
// deadline, writes, flushes.
func (sw *sseWriter) event(name, id string, data []byte) error {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	_ = sw.rollDeadline()
	var b strings.Builder
	if id != "" {
		b.WriteString("id: ")
		b.WriteString(id)
		b.WriteByte('\n')
	}
	b.WriteString("event: ")
	b.WriteString(name)
	b.WriteString("\ndata: ")
	b.Write(data)
	b.WriteString("\n\n")
	if _, err := io.WriteString(sw.w, b.String()); err != nil {
		return err
	}
	return sw.rc.Flush()
}

// ping writes an SSE comment keepalive (": ping"). The client ignores it; it
// resets the fronting proxy's read timeout and the connection write deadline.
// Thin wrapper over the generic comment() (sse.go) — same line, one writer.
//
// /api/events no longer uses it: an ignored keepalive proves liveness to the
// SOCKET and to the proxy, and nothing at all to the client, so the telemetry
// stream sends hb() instead (S3). It remains the keepalive of the domain-event
// stream (project_events.go), whose frames are ids-only refetch signals with no
// server-health payload to carry.
func (sw *sseWriter) ping() error {
	return sw.comment("ping")
}

// hbEvent is the /api/events keepalive payload (RC-1 wave S3). The keepalive is
// a REAL named event, not an SSE comment: eventsource parsers drop comment lines
// natively (the repo's own client does — sse.svelte.ts), so a ": ping" keepalive
// fires nothing client-side and leaves the page unable to tell "the pipe is
// warm" from "the server is answering".
//
// Carrying the SUCCESS stamp turns that same frame into the signal a client
// watchdog can act on (wave S4): last_good_at is the newest tick whose DB reads
// ANSWERED, so a stream that keeps arriving while the stamp freezes reads
// "transport fine, server blind" — the state as_of can never express, because
// as_of advances on failed reads too.
//
// No subs field, deliberately: connection bookkeeping is hub-internal, and a
// frame fanned to every open admin panel is the wrong place to disclose how many
// other sessions are watching.
type hbEvent struct {
	// LastGoodAt is null until the first measured tick — a zero time would
	// marshal as the Unix epoch and read like a real, very old measurement.
	LastGoodAt *time.Time `json:"last_good_at"`
	Degraded   bool       `json:"degraded"`
	Health     string     `json:"health"`
}

func hbEventOf(l livenessStamp) hbEvent {
	return hbEvent{LastGoodAt: timePtr(l.lastGoodAt), Degraded: l.degraded, Health: l.health}
}

// hb writes the keepalive through the NORMAL frame path, so it rolls the write
// deadline exactly like a diff frame: a keepalive written past sseWriter would
// keep the fronting proxy happy and still let http.Server's ABSOLUTE WriteTimeout
// kill the stream at 120s (sseWriteWindow).
func (sw *sseWriter) hb(l livenessStamp) error {
	data, err := json.Marshal(hbEventOf(l))
	if err != nil {
		return err
	}
	return sw.event("hb", "", data)
}

// statusEvent is the per-tick status payload: the full /api/status shape MINUS
// backends (a separate event, so the two diff independently) and minus the
// llmcall stream. The client merges it onto its held status object — one render
// path shared with the GET /api/status poll fallback.
type statusEvent struct {
	AsOf           time.Time       `json:"as_of"`
	Health         healthResponse  `json:"health"`
	Dream          dreamStatus     `json:"dream"`
	LLM24h         []llm24hRow     `json:"llm_24h"`
	LLM24hComplete bool            `json:"llm_24h_complete"`
	// Profiles is the disable-profile registry line (U01-W7), replacing the
	// retired gaming field. The SSE stream is server-admin-only, so it always
	// carries the array (never nil) — a plain slice, unlike the pointer on the
	// dual-purpose statusResponse. ORDER BY name upstream ⇒ diffKey-stable.
	Profiles       []statusProfile `json:"profiles"`
	Activity       *activityStatus `json:"activity"`
	// Dispatch is the full server-admin registry section (MW12). The SSE stream
	// is server-admin-only (RequireAdmin, server.go), so the full view rides it.
	// Accepted per-tick diff behaviour (design/05 §4.5 lens-2): the section
	// carries monotone since-boot counters + volatile inflight/waitQ, so under
	// load it diffs every tick — the broadcast loop already refreshes at that
	// cadence; no diff-exception logic (more code without a measured problem).
	Dispatch *dispatchStatus `json:"dispatch"`
	// GuardReview is the needs_review queue section (guard W2, RC-1 wave S2).
	// It is the one section BOTH pull paths carry (status.go assemble +
	// status_tenant.go SnapshotForTenant), so the push signal owes it too —
	// without it a growing review queue only became visible on the next poll,
	// on a stream whose whole point is that it does not need one.
	// Pointer + omitempty, deliberately: three states must stay
	// DISTINGUISHABLE on the wire — section absent (no fresh generation, the
	// server cannot say), section with zero counts (the queue is genuinely
	// empty), section with an older built_at (counts are real but aging). A
	// value type or a missing omitempty would collapse the first into the
	// second and render "0 open" for "I do not know".
	GuardReview *guardReviewStatus `json:"guard_review,omitempty"`
}

func statusEventOf(s statusResponse) statusEvent {
	return statusEvent{
		AsOf:           s.AsOf,
		Health:         s.Health,
		Dream:          s.Dream,
		LLM24h:         s.LLM24h,
		LLM24hComplete: s.LLM24hComplete,
		Profiles:       derefProfiles(s.Profiles),
		Activity:       s.Activity,
		Dispatch:       s.Dispatch,
		GuardReview:    s.GuardReview,
	}
}

// derefProfiles flattens the statusResponse's *[]statusProfile (present on the
// server-admin path, nil on the per-tenant path) to the plain slice the SSE
// frame carries. The SSE stream is server-admin-only, so in practice the
// pointer is always non-nil here; nil degrades to an empty slice for a stable
// wire shape.
func derefProfiles(p *[]statusProfile) []statusProfile {
	if p == nil {
		return []statusProfile{}
	}
	return *p
}

// diffKey marshals the status event with the two as_of-CLASS stamps zeroed:
// as_of advances every tick, and guard_review.built_at advances with every
// generation the collector builds (status_guard.go) — either one left in would
// defeat the diff and fire a status frame every cycle even when nothing
// meaningful changed. The remaining fields only move on real events (dream-queue
// counts, llm-24h aggregate, flagged-block counts, health class).
//
// The guard section is zeroed on a COPY. The value receiver copies the frame,
// not the struct behind its pointer — and that struct is the SHARED per-tick
// generation every reader holds, so zeroing in place would blank built_at for
// the pull paths and for every other connection too.
func (e statusEvent) diffKey() []byte {
	e.AsOf = time.Time{}
	if e.GuardReview != nil {
		gr := *e.GuardReview
		gr.BuiltAt = nil
		e.GuardReview = &gr
	}
	b, _ := json.Marshal(e)
	return b
}

// backendsDiffKey marshals the backend list with the live-ticking fields
// zeroed: cooldown_remaining_s counts down on every read (pool.go) and last_ok
// is a health-probe timestamp — including them would fire a backends event
// every tick during any cooldown. A real state change (effective_state,
// enabled, roles, priority, trust, error class, fails) still diffs. The sent
// payload carries the live values; only the diff comparison ignores them.
func backendsDiffKey(bs []backends.BackendStatus) []byte {
	cp := make([]backends.BackendStatus, len(bs))
	copy(cp, bs)
	for i := range cp {
		cp[i].CooldownRemaining = 0
		cp[i].LastOK = ""
	}
	b, _ := json.Marshal(cp)
	return b
}

// sseFrame is one fanned-out event: a name, an optional id, and pre-marshalled
// data ready to write verbatim.
type sseFrame struct {
	name string
	id   string
	data []byte
}

// sseSub is one subscriber's mailbox. The broadcast loop fans frames into ch
// (buffered, non-blocking); the connection handler drains it. done is closed by
// the hub when it drops the sub (mailbox overflow) so the handler tears down.
type sseSub struct {
	ch   chan sseFrame
	done chan struct{}
}

// ── T37d · per-tenant SSE migration map (04-W5 push; INTERIM = polling) ──────
//
// Decision (2026-06-20): live per-tenant SSE is the long-term goal, but the
// interim tenant-admin telemetry path is POLLING — a tenant-admin already reads
// its OWN view via GET /api/status (T37c, SnapshotForTenant) + GET /api/llmlog
// (T37b, api_key_id filter), both shipped and per-tenant-scoped. /api/events
// stays SERVER-admin-only (server.go) so there is NO push leak (K-T1: the pull
// is per-tenant, the push is not opened). Polling is the deliberate interim, not
// a gap. This block is the spec the SSE rework follows; every touch-point below
// carries a `// T37d:` anchor pointing back to the numbered step here.
//
// What the push path must grow to admit tenant-admins:
//
//  1. server.go gate — RequireAdmin → RequireAdminOrTenantAdmin (mirror the
//     /api/status mount). The gate only ADMITS; the hub MUST then scope every
//     frame, exactly as the pull pairs the looser gate with an in-handler filter.
//  2. subscribe() — capture the subscriber identity (scope + role + the tenant's
//     api_key_id set) onto sseSub at subscribe time. A server-admin sub is tagged
//     "global" and keeps today's full fan-out unchanged.
//  3. broadcast() — fan a frame ONLY to subs it belongs to. Today one frame goes
//     to everyone; per-tenant means a frame is keyed by scope and broadcast()
//     matches that key against sseSub.scope (global subs match all).
//  4. runLoop() diff — today ONE global diff (status + backends + EVERY tenant's
//     llmcalls). Per-tenant needs either (a) a per-scope diff built from
//     SnapshotForTenant(scope) + scope-visible backends + scope-filtered
//     llmcalls, or (b) one global build filtered per-sub at fan-out. (a) reuses
//     the T37c rollup cache and the egress predicate already lives in
//     SnapshotForTenant; (b) is cheaper but re-implements that predicate inside
//     the hub. fetchLLMCalls must additionally gain the api_key_id filter
//     llmlog.go already uses (server-admin = all rows, tenant = ANY(its keys)).
//  5. HandleEvents initial state — serve SnapshotForTenant(scope) instead of the
//     global Snapshot for a tenant-admin sub (the same reduction as /api/status).
//  6. HandleEvents re-auth — today `!res.IsAdmin` ends the stream. Per-tenant
//     must end it on `!(server-admin || tenant-admin-of-own-tenant)` AND on a
//     tenant_id/role CHANGE mid-stream: a key re-pointed to another tenant must
//     not keep streaming the old tenant's telemetry. Mirror the
//     RequireAdminOrTenantAdmin admit test plus a stored-identity compare.
//
// Reusable blocks already in place: SnapshotForTenant (status_tenant.go) and the
// llmlog api_key_id filter (llmlog.go) — the per-tenant DATA shaping exists; only
// the PUSH plumbing (subscribe-tag → broadcast-route → re-auth) is new. Open
// design choice is (4a) vs (4b); see ctx 019ee181 §FOLGESESSION-ANKER.
//
// sseHub multiplexes one broadcast loop over many connections.
type sseHub struct {
	status statusProvider
	cfg    ConfigStore
	life   context.Context
	// Injectable seams (mirrors StatusCollector.queueDepth) — bound to the real
	// pool / auth in NewEventsHandler, faked in tests.
	llmcalls     func(ctx context.Context, cursor llmCursor, limit int) ([]llmlogEntry, llmCursor)
	authenticate func(ctx context.Context, key string, isSession bool) (*auth.AuthResult, error)

	mu      sync.Mutex
	subs    map[*sseSub]struct{}
	running bool
}

// subscribe registers a connection and starts the broadcast loop if idle.
// Returns ok=false when the connection cap (events.max_connections) is reached
// — the caller answers 429 and the client degrades to polling.
func (h *sseHub) subscribe() (*sseSub, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	max := h.cfg.Snapshot().Events.MaxConnections //nolint:forbidigo // MT 06 BLIND: events.max_connections is a server-global SSE hub cap, not tenant-scoped.
	if max <= 0 {
		max = 8
	}
	if len(h.subs) >= max {
		return nil, false
	}
	// T37d (anchor 2): tag this sub with the subscriber's scope/role + api_key_id
	// set here; a server-admin sub stays "global". See the migration map above.
	s := &sseSub{ch: make(chan sseFrame, sseMailbox), done: make(chan struct{})}
	h.subs[s] = struct{}{}
	if !h.running {
		h.running = true
		go h.runLoop()
	}
	return s, true
}

// unsubscribe removes a connection. The loop notices the empty set at its next
// tick and stops itself (bounded by one tick) — no signal needed here.
func (h *sseHub) unsubscribe(s *sseSub) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.subs, s)
}

func (h *sseHub) subCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// llmcallCoalesceThreshold is the per-tick row count above which the collapsed
// telemetry frame degrades to a content-free refetch signal. Read per tick (the
// key is hot) and floored at the default, exactly the way ProjectHub reads
// project.events.coalesce_threshold — one coalescing doctrine, two hubs.
func (h *sseHub) llmcallCoalesceThreshold() int {
	n := h.cfg.Snapshot().Events.LLMCallCoalesceThreshold //nolint:forbidigo // MT 06 BLIND: process-global coalescing knob of the server-admin telemetry hub, shared across all connections.
	if n <= 0 {
		n = 20
	}
	return n
}

// broadcast fans one frame to every subscriber without blocking. A full mailbox
// means the client cannot keep up: drop it (close done + remove) so the slow
// connection cannot stall the fan-out to the rest. Deleting during range is
// safe in Go.
func (h *sseHub) broadcast(f sseFrame) {
	h.mu.Lock()
	defer h.mu.Unlock()
	// T37d (anchor 3): per-tenant fan-out filters here — a frame keyed by scope
	// reaches only subs whose scope matches (global subs match all).
	for s := range h.subs {
		select {
		case s.ch <- f:
		default:
			close(s.done)
			delete(h.subs, s)
		}
	}
}

// runLoop is the single broadcast loop. One tick: refresh the collector cache,
// diff status + backends against the last sent state and broadcast only the
// changed ones, then collapse the new llmlog rows since the cursor into ONE
// llmcalls event. Stops on lifecycle cancel (shutdown) or one tick after the
// last subscriber leaves.
//
// The frame count per tick is bounded by the EVENT KINDS (<= 3), never by the
// row count: the pre-S0 per-row fan-out pushed llmlog.max_limit (200) frames
// into a 16-deep mailbox, so any burst above ~14 rows dropped every open
// connection at once.
func (h *sseHub) runLoop() {
	h.status.setBroadcasting(true)
	defer h.status.setBroadcasting(false)
	defer func() {
		h.mu.Lock()
		h.running = false
		h.mu.Unlock()
	}()

	cfg := h.cfg.Snapshot() //nolint:forbidigo // MT 06 BLIND: SSE broadcast-loop cadence (events.tick_interval) is server-global, shared across all connections.
	tick := cfg.Events.TickInterval
	if tick <= 0 {
		tick = 5 * time.Second
	}
	t := time.NewTicker(tick)
	defer t.Stop()

	var lastStatus, lastBackends []byte
	// New llmlog rows are streamed forward only — the table fetches its history
	// over HTTP, so the cursor starts at "now" and never replays the backlog.
	llmCur := newLLMCursor(time.Now())

	for {
		select {
		case <-h.life.Done():
			return
		case <-t.C:
			if h.subCount() == 0 {
				return // last subscriber left — stop (no idle queue scans)
			}

			// T37d (anchor 4): today ONE global diff fans to all. Per-tenant builds
			// a per-scope diff (SnapshotForTenant + scope-filtered llmcalls) OR
			// filters this global build per-sub at fan-out. See the migration map.
			opCtx, cancel := context.WithTimeout(h.life, 10*time.Second)
			snap := h.status.refreshForBroadcast(opCtx)

			se := statusEventOf(snap)
			if key := se.diffKey(); !bytes.Equal(key, lastStatus) {
				lastStatus = key
				if data, err := json.Marshal(se); err == nil {
					h.broadcast(sseFrame{name: "status", id: strconv.FormatInt(snap.AsOf.UnixMilli(), 10), data: data})
				}
			}
			if key := backendsDiffKey(snap.Backends); !bytes.Equal(key, lastBackends) {
				lastBackends = key
				if data, err := json.Marshal(snap.Backends); err == nil {
					h.broadcast(sseFrame{name: "backends", data: data})
				}
			}

			rows, next := h.llmcalls(opCtx, llmCur, cfg.LLMLog.MaxLimit)
			cancel()
			llmCur = next
			if len(rows) > 0 {
				if data, err := json.Marshal(llmcallsFrameOf(rows, h.llmcallCoalesceThreshold())); err == nil {
					h.broadcast(sseFrame{name: "llmcalls", data: data})
				}
			}
		}
	}
}

// llmcallsFrame is the collapsed telemetry payload of ONE tick. Below the
// coalesce threshold it carries the rows themselves; above it Rows/Kind/Cursor
// swap roles — the rows are dropped and only the count plus the tick's newest
// position remain, so a burst costs a fixed-size frame instead of an unbounded
// push. Same wire idea as the domain hub's issues-bulk frame (project_hub.go).
type llmcallsFrame struct {
	Rows   []llmlogEntry `json:"rows,omitempty"`
	Count  int           `json:"count"`
	Kind   string        `json:"kind,omitempty"`
	Cursor string        `json:"cursor,omitempty"`
}

// llmcallsFrameOf collapses one tick's rows into that payload. rows arrive
// oldest-first (fetchLLMCalls orders (created_at, id) ASC), so the LAST row is
// the tick's newest and its cursor is the position the client refetches from
// over GET /api/llmlog (capped, per-tenant filtered) — the same tuple the loop
// itself resumes at, rendered once by llmCursor.String().
func llmcallsFrameOf(rows []llmlogEntry, threshold int) llmcallsFrame {
	if len(rows) > threshold {
		return llmcallsFrame{
			Count:  len(rows),
			Kind:   "llmcalls-bulk",
			Cursor: llmCursorOf(rows[len(rows)-1]).String(),
		}
	}
	return llmcallsFrame{Rows: rows, Count: len(rows)}
}

// llmCursorZeroID is the lowest uuid there is — the id half of a cursor that has
// not seen a row yet. (t, zero) sorts before every real row carrying t, so a
// stream starting at "now" cannot lose a row written in that very instant.
const llmCursorZeroID = "00000000-0000-0000-0000-000000000000"

// llmCursor is the telemetry stream position: the TUPLE (created_at, id).
//
// created_at alone is not a total order on context_llm_log. Its PK is
// (id, created_at) with gen_random_uuid() (migrations/025_llm_log.sql:9,24) —
// there is no monotonic secondary key, and two rows written in the same
// microsecond tie. Before S14 the loop carried max(created_at) and asked for
// `created_at > $1`: when the LIMIT cut fell between two tied rows, the cursor
// advanced past BOTH and the second one was never delivered on any later tick
// (design 05 §4.6, F4b).
type llmCursor struct {
	CreatedAt time.Time
	ID        string
}

// newLLMCursor is the stream-start position at a wall-clock instant.
func newLLMCursor(at time.Time) llmCursor {
	return llmCursor{CreatedAt: at.UTC(), ID: llmCursorZeroID}
}

// llmCursorOf is the position OF a fetched row.
func llmCursorOf(e llmlogEntry) llmCursor {
	return llmCursor{CreatedAt: e.CreatedAt.UTC(), ID: e.ID}
}

// String renders the position for the wire: "<RFC3339Nano>|<uuid>".
func (c llmCursor) String() string {
	return c.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + c.ID
}

// before reports whether c sorts strictly before o under the SAME order the
// query applies. It is the Go-side mirror of the row comparison, so a scan
// error mid-page can never roll the cursor backwards and replay rows.
// Comparing the id halves as text matches PostgreSQL's uuid byte order: pgx
// renders uuids canonically (fixed width, lower-case hex, dashes at fixed
// positions), and over that alphabet lexicographic order IS byte order.
func (c llmCursor) before(o llmCursor) bool {
	if !c.CreatedAt.Equal(o.CreatedAt) {
		return c.CreatedAt.Before(o.CreatedAt)
	}
	return c.ID < o.ID
}

// llmcallCursorSQL is the SSE telemetry page, keyset-paginated on the tuple
// (created_at, id). It is a named const so the EXPLAIN gate plans the SHIPPED
// statement instead of a copy of it.
//
// Why `created_at >= $1` is spelled out although the row comparison implies it:
// it is the qual TimescaleDB prunes chunks on. Measured on a 7-chunk / 60k-row
// fixture, the row comparison ALONE plans a ChunkAppend across EVERY chunk
// (18 shared buffers, 1.59 ms planning); with the redundant leading qual the
// plan touches the single qualifying chunk (6 buffers, 0.17 ms). At 1M+ rows
// spanning years of 7-day chunks that is the difference between a per-tick
// sweep over the whole hypertable and one range scan. Being implied by the row
// comparison, the qual cannot change the result set — only the plan.
//
// `id::text AS id_text` is deliberate, not cosmetic: without the alias the bare
// `id` in ORDER BY binds to the OUTPUT column (the text cast) instead of the
// uuid column, and the sort order would stop matching the WHERE comparison.
const llmcallCursorSQL = `
	SELECT id::text AS id_text, created_at, pipeline, model,
	       COALESCE(backend_name, host) AS backend,
	       duration_ms, error, prompt_tokens, completion_tokens, cost_usd
	FROM context_llm_log
	WHERE created_at >= $1 AND (created_at, id) > ($1, $2::uuid)
	ORDER BY created_at ASC, id ASC
	LIMIT $3`

// fetchLLMCalls returns the telemetry rows past cursor (oldest first) and the
// advanced cursor. It reuses llmlogEntry + normalizeLLMError so the SSE stream
// and GET /api/llmlog share one row shape and one error-normalization (class +
// ≤256-char detail) — and the same body-free SELECT list: llmlogEntry has no
// request_*/response_content fields, so a body leak is structurally impossible
// regardless of the query.
func fetchLLMCalls(ctx context.Context, pool *pgxpool.Pool, cursor llmCursor, limit int) ([]llmlogEntry, llmCursor) {
	if limit <= 0 {
		limit = 200
	}
	if cursor.ID == "" {
		cursor.ID = llmCursorZeroID
	}
	// T37d (anchor 4): the per-tenant variant adds the `api_key_id = ANY($keys)`
	// filter llmlog.go already uses — server-admin = all rows, tenant = own keys.
	rows, err := pool.Query(ctx, llmcallCursorSQL, cursor.CreatedAt, cursor.ID, limit)
	if err != nil {
		slog.Warn("sse: llmcall fetch failed", "error", err)
		return nil, cursor
	}
	defer rows.Close()

	out := []llmlogEntry{}
	next := cursor
	for rows.Next() {
		var e llmlogEntry
		var rawErr *string
		if err := rows.Scan(&e.ID, &e.CreatedAt, &e.Pipeline, &e.Model, &e.Backend,
			&e.DurationMs, &rawErr, &e.PromptTokens, &e.CompletionTokens, &e.CostUSD); err != nil {
			slog.Warn("sse: llmcall scan failed", "error", err)
			return out, next
		}
		e.Error = normalizeLLMError(rawErr)
		if pos := llmCursorOf(e); next.before(pos) {
			next = pos
		}
		out = append(out, e)
	}
	if rows.Err() != nil {
		slog.Warn("sse: llmcall rows error", "error", rows.Err())
	}
	return out, next
}

// EventsHandler serves GET /api/events over the shared hub.
type EventsHandler struct {
	hub *sseHub
}

// NewEventsHandler wires the SSE handler. life is the server lifecycle context
// (cancelled on shutdown) so streams end before http.Server.Shutdown waits on
// them; pool drives the in-stream re-auth + llmcall cursor; collector + cfg
// feed the broadcast.
func NewEventsHandler(life context.Context, pool *pgxpool.Pool, collector *StatusCollector, cfg ConfigStore) *EventsHandler {
	return &EventsHandler{hub: &sseHub{
		status: collector,
		cfg:    cfg,
		life:   life,
		llmcalls: func(ctx context.Context, cursor llmCursor, limit int) ([]llmlogEntry, llmCursor) {
			return fetchLLMCalls(ctx, pool, cursor, limit)
		},
		authenticate: func(ctx context.Context, key string, isSession bool) (*auth.AuthResult, error) {
			// resolveRequestCredential, not auth.Authenticate: the stream may
			// be carried by an opaque ctxt_ token (S3) — SanitizeKey would
			// destroy its prefix on the re-auth tick and kill the stream
			// (design 03 §4, RVW-Vollst-F2) — or by a ctx_session cookie
			// (R2), whose revocation must end the stream the same way. The
			// csrf secret is irrelevant on this GET-only stream.
			ar, _, err := resolveRequestCredential(ctx, pool, key, isSession)
			return ar, err
		},
		subs: map[*sseSub]struct{}{},
	}}
}

// HandleEvents serves the admin SSE stream. Admin-gated upstream (RequireAdmin)
// — same payload as /api/status. Flow: cap-check → commit stream header →
// initial full status + backends → fan-out diffs / hb heartbeats / periodic re-auth
// until the client disconnects, the server shuts down, the hub drops a slow
// consumer, or the key is revoked.
func (h *EventsHandler) HandleEvents(w http.ResponseWriter, r *http.Request) {
	sub, ok := h.hub.subscribe()
	if !ok {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"success": false, "error": "too many SSE connections",
		})
		return
	}
	defer h.hub.unsubscribe(sub)

	// Cache-Control: no-store is global (SecurityHeaders); X-Accel-Buffering
	// turns OFF nginx response buffering for this stream specifically.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	sw := newSSEWriter(w)

	cfg := h.hub.cfg.Snapshot() //nolint:forbidigo // MT 06 BLIND: per-connection SSE tick cadence (events.tick_interval) is server-global telemetry, not tenant-scoped.
	tick := cfg.Events.TickInterval
	if tick <= 0 {
		tick = 5 * time.Second
	}
	ping := cfg.Events.PingInterval
	if ping <= 0 {
		ping = 25 * time.Second
	}

	// Initial full state: a complete status + backends event before any diffs
	// (design §3.6), served from the collector cache the loop keeps warm.
	// T37d (anchor 5): a tenant-admin sub serves SnapshotForTenant(scope) here
	// instead of the global Snapshot — same reduction as /api/status (T37c).
	snap := h.hub.status.Snapshot(r.Context())
	if data, err := json.Marshal(statusEventOf(snap)); err == nil {
		if sw.event("status", strconv.FormatInt(snap.AsOf.UnixMilli(), 10), data) != nil {
			return
		}
	}
	if data, err := json.Marshal(snap.Backends); err == nil {
		if sw.event("backends", "", data) != nil {
			return
		}
	}

	key, keyIsSession := requestCredential(r)
	pingT := time.NewTicker(ping)
	defer pingT.Stop()
	// Re-auth every 12th base tick (= 60s at the 5s default). The system is
	// per-request auth with no cache, so a long-lived stream is the one path
	// that could keep pushing admin telemetry to a key revoked via
	// api-key-delete (R3); this re-validates and ends the stream on revocation.
	reauthT := time.NewTicker(12 * tick)
	defer reauthT.Stop()

	for {
		select {
		case <-r.Context().Done():
			return // client disconnected
		case <-h.hub.life.Done():
			return // server shutting down — return so srv.Shutdown unblocks
		case <-sub.done:
			return // hub dropped us (mailbox overflow)
		case f := <-sub.ch:
			if sw.event(f.name, f.id, f.data) != nil {
				return
			}
		case <-pingT.C:
			// The keepalive is an OBSERVABLE hb event carrying the success stamp,
			// not a ": ping" comment the client's parser drops (S3). liveness() is
			// an in-memory cache read, so N connections firing on their own timers
			// cost N marshals and zero DB work.
			if sw.hb(h.hub.status.liveness()) != nil {
				return
			}
		case <-reauthT.C:
			// T37d (anchor 6): per-tenant re-auth ends the stream on
			// !(server-admin || tenant-admin-of-own-tenant) AND on a tenant_id/role
			// change mid-stream (a re-pointed key must not keep the old telemetry).
			res, err := h.hub.authenticate(r.Context(), key, keyIsSession)
			if err != nil || res == nil || !res.IsValid || !res.IsAdmin {
				_ = sw.event("error", "", []byte(`{"error":"session revoked"}`))
				return
			}
		}
	}
}
