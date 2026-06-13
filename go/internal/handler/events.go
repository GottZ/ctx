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
// any long-lived stream at 120s; while the stream writes (diffs + pings) it
// proves liveness, so every frame pushes the deadline one window ahead instead
// — the exact mechanism the query heartbeat uses against the same cap
// (query.go: heartbeatWriteWindow). Package var so tests can shrink it.
var sseWriteWindow = 90 * time.Second

// sseMailbox is the per-connection send buffer. A connection whose mailbox
// overflows cannot keep up with the broadcast; the hub drops it (it reconnects
// and re-fetches the full state) rather than stall the fan-out to everyone else.
const sseMailbox = 16

// statusProvider is the slice of *StatusCollector the SSE layer needs: serve a
// cached snapshot, drive a synchronous per-tick refresh, and flip the
// broadcasting flag. *StatusCollector satisfies it; tests inject a fake that
// returns canned snapshots without a pool.
type statusProvider interface {
	Snapshot(ctx context.Context) statusResponse
	refreshForBroadcast(ctx context.Context) statusResponse
	setBroadcasting(on bool)
}

// sseWriter frames server-sent events onto one connection and keeps the rolling
// write deadline ahead of the absolute server WriteTimeout. Every write is
// mutex-serialized: the diff fan-out, the keepalive ping and the re-auth
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
func (sw *sseWriter) ping() error {
	return sw.comment("ping")
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
	Gaming         gamingStatus    `json:"gaming"`
	Activity       *activityStatus `json:"activity"`
}

func statusEventOf(s statusResponse) statusEvent {
	return statusEvent{
		AsOf:           s.AsOf,
		Health:         s.Health,
		Dream:          s.Dream,
		LLM24h:         s.LLM24h,
		LLM24hComplete: s.LLM24hComplete,
		Gaming:         s.Gaming,
		Activity:       s.Activity,
	}
}

// diffKey marshals the status event with as_of zeroed: as_of advances every
// tick and would otherwise defeat the diff (status would fire every cycle even
// when nothing meaningful changed). The remaining fields only move on real
// events (dream-queue counts, llm-24h aggregate, gaming, health class).
func (e statusEvent) diffKey() []byte {
	e.AsOf = time.Time{}
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

// sseHub multiplexes one broadcast loop over many connections.
type sseHub struct {
	status statusProvider
	cfg    ConfigStore
	life   context.Context
	// Injectable seams (mirrors StatusCollector.queueDepth) — bound to the real
	// pool / auth in NewEventsHandler, faked in tests.
	llmcalls     func(ctx context.Context, cursor time.Time, limit int) ([]llmlogEntry, time.Time)
	authenticate func(ctx context.Context, key string) (*auth.AuthResult, error)

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
	max := h.cfg.Snapshot().Events.MaxConnections
	if max <= 0 {
		max = 8
	}
	if len(h.subs) >= max {
		return nil, false
	}
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

// broadcast fans one frame to every subscriber without blocking. A full mailbox
// means the client cannot keep up: drop it (close done + remove) so the slow
// connection cannot stall the fan-out to the rest. Deleting during range is
// safe in Go.
func (h *sseHub) broadcast(f sseFrame) {
	h.mu.Lock()
	defer h.mu.Unlock()
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
// changed ones, then drain new llmlog rows since the cursor as individual
// llmcall events. Stops on lifecycle cancel (shutdown) or one tick after the
// last subscriber leaves.
func (h *sseHub) runLoop() {
	h.status.setBroadcasting(true)
	defer h.status.setBroadcasting(false)
	defer func() {
		h.mu.Lock()
		h.running = false
		h.mu.Unlock()
	}()

	cfg := h.cfg.Snapshot()
	tick := cfg.Events.TickInterval
	if tick <= 0 {
		tick = 5 * time.Second
	}
	t := time.NewTicker(tick)
	defer t.Stop()

	var lastStatus, lastBackends []byte
	// New llmlog rows are streamed forward only — the table fetches its history
	// over HTTP, so the cursor starts at "now" and never replays the backlog.
	llmCursor := time.Now().UTC()

	for {
		select {
		case <-h.life.Done():
			return
		case <-t.C:
			if h.subCount() == 0 {
				return // last subscriber left — stop (no idle queue scans)
			}

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

			rows, next := h.llmcalls(opCtx, llmCursor, cfg.LLMLog.MaxLimit)
			cancel()
			llmCursor = next
			for i := range rows {
				if data, err := json.Marshal(rows[i]); err == nil {
					h.broadcast(sseFrame{name: "llmcall", data: data})
				}
			}
		}
	}
}

// fetchLLMCalls returns the telemetry rows newer than cursor (oldest first) and
// the advanced cursor. It reuses llmlogEntry + normalizeLLMError so the SSE
// stream and GET /api/llmlog share one row shape and one error-normalization
// (class + ≤256-char detail) — and the same body-free SELECT list: llmlogEntry
// has no request_*/response_content fields, so a body leak is structurally
// impossible regardless of the query.
func fetchLLMCalls(ctx context.Context, pool *pgxpool.Pool, cursor time.Time, limit int) ([]llmlogEntry, time.Time) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := pool.Query(ctx, `
		SELECT id::text, created_at, pipeline, model,
		       COALESCE(backend_name, host) AS backend,
		       duration_ms, error, prompt_tokens, completion_tokens, cost_usd
		FROM context_llm_log
		WHERE created_at > $1
		ORDER BY created_at ASC
		LIMIT $2`, cursor, limit)
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
		if e.CreatedAt.After(next) {
			next = e.CreatedAt
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
		llmcalls: func(ctx context.Context, cursor time.Time, limit int) ([]llmlogEntry, time.Time) {
			return fetchLLMCalls(ctx, pool, cursor, limit)
		},
		authenticate: func(ctx context.Context, key string) (*auth.AuthResult, error) {
			return auth.Authenticate(ctx, pool, key)
		},
		subs: map[*sseSub]struct{}{},
	}}
}

// HandleEvents serves the admin SSE stream. Admin-gated upstream (RequireAdmin)
// — same payload as /api/status. Flow: cap-check → commit stream header →
// initial full status + backends → fan-out diffs / pings / periodic re-auth
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

	cfg := h.hub.cfg.Snapshot()
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

	key := apiKeyFromRequest(r)
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
			if sw.ping() != nil {
				return
			}
		case <-reauthT.C:
			res, err := h.hub.authenticate(r.Context(), key)
			if err != nil || res == nil || !res.IsValid || !res.IsAdmin {
				_ = sw.event("error", "", []byte(`{"error":"session revoked"}`))
				return
			}
		}
	}
}
