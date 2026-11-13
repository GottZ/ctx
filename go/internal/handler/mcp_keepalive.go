package handler

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// The /mcp keepalive: the MCP twin of queryHeartbeat (query.go).
//
// The SDK handler answers a tool call with ONE buffered application/json body
// (JSONResponse, mcp.go), and the query tool delegates into a responseRecorder
// whose Flush and SetWriteDeadline are no-ops. So while `query` runs, no byte
// reaches the socket and nothing moves http.Server's absolute WriteTimeout
// (main.go: 120s): an MCP query whose pipeline runs past that is computed,
// logged as a 200 and never delivered, and a reverse proxy with a shorter read
// timeout gives up even earlier.
//
// withMCPKeepalive hands the query tool a handle through the request context.
// While the tool runs, every heartbeatInterval the keepalive
//
//   - commits `200 application/json` on its first tick,
//   - writes one space and flushes (leading whitespace is valid JSON per
//     RFC 8259, the same trick HandleQuery relies on),
//   - rolls the connection write deadline one heartbeatWriteWindow ahead (the
//     mechanism of queryHeartbeat and sseWriteWindow in events.go).
//
// It runs from the moment the tool starts, so it also covers what the REST
// heartbeat does not: the phase before the RRF search and synthesize:false.
//
// One response has one cadence. The arms are counted, and a single ticker
// serves them all: a JSON-RPC batch with two query calls (the SDK still takes
// batches on protocol 2025-03-26 and earlier, and runs their calls
// concurrently) gets one space per interval, not two.
//
// What stays untouched, by construction:
//
//   - Every request that does not reach the query tool (get, search, an
//     unknown tool, invalid params, a protocol error) is never armed. The
//     wrapper only forwards, so status, headers and body are the SDK's own.
//   - A query that finishes before the first tick has had no byte written by
//     the keepalive either, so the SDK still picks the status, including the
//     SEP-2575 override (go-sdk mcp/streamable.go, deliverLocked: 404 for
//     MethodNotFound, 400 for InvalidParams on protocol >= 2026-07-28).
//
// The one thing the early 200 costs: once a tick has gone out, the status line
// is on the wire, and a later WriteHeader from the SDK cannot replace it. That
// needs a JSON-RPC protocol error as the answer to a query call that has
// already run longer than heartbeatInterval. mcpQueryHandler reports its
// failures as tool results (isError), never as such an error, so today no
// path produces it; if one ever does, the wrapper drops the late status, logs
// it, and the body still carries the JSON-RPC error.
type mcpKeepalive struct {
	w   http.ResponseWriter
	rc  *http.ResponseController
	ctx context.Context // the request context: canceled when the client is gone

	done chan struct{} // closed by close(): ends the ticker at once

	mu        sync.Mutex
	closed    bool // next.ServeHTTP has returned: w must not be touched again
	committed bool // a status line is out, ours or the SDK's
	ticked    bool // the status line that is out is our early 200
	payload   bool // the SDK has started its body: no whitespace after that
	armed     int  // query calls running right now
	ticking   bool // the ticker goroutine is live and has not given up
}

type mcpKeepaliveKey struct{}

// withMCPKeepalive wraps the MCP SDK handler. The wrapper is closed under its
// mutex once next.ServeHTTP has returned, because writing to a ResponseWriter
// after its handler returned is not allowed and the SDK runs tool handlers on
// a goroutine of their own. go-sdk v1.8.0 in stateless mode happens to keep
// ServeHTTP open until the tool has returned (its deferred session.Close
// waits for in-flight calls), even when the client is gone; that is its
// behavior, not a contract, so the guard does not lean on it. A client that
// went away is caught by the request context on the next tick.
func withMCPKeepalive(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		k := &mcpKeepalive{
			w:    w,
			rc:   http.NewResponseController(w),
			ctx:  r.Context(),
			done: make(chan struct{}),
		}
		defer k.close()
		next.ServeHTTP(k, r.WithContext(context.WithValue(r.Context(), mcpKeepaliveKey{}, k)))
	})
}

// armMCPKeepalive starts the keepalive for the tool call running under ctx and
// returns its stop function, which must run before the tool returns. Outside
// /mcp (a direct handler call, the chat delegation) there is no keepalive in
// the context and both are no-ops.
func armMCPKeepalive(ctx context.Context) (stop func()) {
	k, _ := ctx.Value(mcpKeepaliveKey{}).(*mcpKeepalive)
	if k == nil {
		return func() {}
	}
	return k.arm()
}

// arm counts one more running call and starts the ticker unless one is live.
// The count is guarded by the keepalive mutex, so once the last stop() has
// returned no tick touches the writer: the SDK writes its body only after
// that.
func (k *mcpKeepalive) arm() (stop func()) {
	k.mu.Lock()
	k.armed++
	start := !k.ticking && !k.closed
	if start {
		k.ticking = true
	}
	k.mu.Unlock()
	if start {
		// Both package vars are read here, on the caller's goroutine, not
		// per tick.
		go k.run(heartbeatInterval, heartbeatWriteWindow)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			k.mu.Lock()
			k.armed--
			k.mu.Unlock()
		})
	}
}

// run is the ticker goroutine. It ends at once when the handler has returned,
// and otherwise on the first tick that gives up (see tick). A client that is
// gone ends it at most one interval after the disconnect, and without a write.
//
// It is a goroutine of ctx's own, outside both the HTTP Recovery and the SDK
// guard (mcpRecover), so it carries its own: a panic in a tick is logged and
// ends the ticker, never the process. tick's defers have released the mutex
// and cleared ticking by then, so a later arm() starts a fresh ticker.
func (k *mcpKeepalive) run(interval, window time.Duration) {
	defer recoverMCPKeepalive(k.ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-k.done:
			return
		case <-t.C:
			if !k.tick(window) {
				return
			}
		}
	}
}

// tick writes one keepalive byte. It reports false when the ticker is to end.
// Six conditions end it before anything reaches the writer: the handler has
// returned, no call is armed any more, the SDK has started its body, the
// client is gone, the SDK has chosen a content type other than JSON, or the
// SDK has committed a status of its own. A failed write of the byte ends it as
// well. It clears ticking under the same lock, so an arm() that comes later
// starts a new ticker instead of relying on this one.
//
// The header map is read and written here without the SDK's own lock. That is
// safe because the SDK sets its response headers before it dispatches the
// request, and touches them again only to answer it, which happens after the
// tool handlers returned and therefore after the last stop().
func (k *mcpKeepalive) tick(window time.Duration) (alive bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	defer func() {
		if !alive {
			k.ticking = false
		}
	}()
	if k.closed || k.armed == 0 || k.payload || k.ctx.Err() != nil {
		return false
	}
	first := !k.committed
	if first {
		h := k.w.Header()
		// The SDK has already chosen the content type. A space is only a
		// valid keepalive in front of a JSON body, never inside an SSE stream.
		if ct := h.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
			return false
		}
		h.Set("Content-Type", "application/json")
		k.w.WriteHeader(http.StatusOK)
		k.committed, k.ticked = true, true
	} else if !k.ticked {
		// The SDK committed a status of its own: the response is its now.
		return false
	}
	deadlineErr := k.rc.SetWriteDeadline(time.Now().Add(window))
	if _, err := io.WriteString(k.w, " "); err != nil {
		return false
	}
	flushErr := k.rc.Flush()
	if first {
		// Same alarms as startHeartbeat: loud once, instead of silent
		// buffering or a silent cut at the server WriteTimeout.
		if flushErr != nil {
			slog.Warn("mcp keepalive: response writer not flushable, keepalive ineffective",
				"error", flushErr)
		}
		if deadlineErr != nil {
			slog.Warn("mcp keepalive: cannot extend write deadline, server WriteTimeout caps long responses",
				"error", deadlineErr)
		}
	}
	return true
}

func (k *mcpKeepalive) close() {
	k.mu.Lock()
	k.closed = true
	k.mu.Unlock()
	close(k.done)
}

// The http.ResponseWriter the SDK sees. Every method that reaches the
// connection takes the mutex, so SDK writes and ticks never interleave.

func (k *mcpKeepalive) Header() http.Header { return k.w.Header() }

func (k *mcpKeepalive) WriteHeader(code int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closed {
		return
	}
	if k.ticked {
		if code != http.StatusOK {
			slog.Warn("mcp keepalive: status arrived after the early 200, dropped",
				"status", code)
		}
		return
	}
	k.committed = true
	k.w.WriteHeader(code)
}

func (k *mcpKeepalive) Write(b []byte) (int, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closed {
		return 0, http.ErrHandlerTimeout
	}
	k.committed, k.payload = true, true
	return k.w.Write(b)
}

// FlushError is what http.ResponseController looks for first, so a flush by
// the SDK is serialized with the ticks as well.
func (k *mcpKeepalive) FlushError() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closed {
		return http.ErrHandlerTimeout
	}
	return k.rc.Flush()
}

func (k *mcpKeepalive) Flush() { _ = k.FlushError() }

func (k *mcpKeepalive) SetWriteDeadline(t time.Time) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closed {
		return http.ErrHandlerTimeout
	}
	return k.rc.SetWriteDeadline(t)
}

// Unwrap keeps the rest of http.ResponseController (read deadline, hijack,
// full duplex) reachable through the wrapper.
func (k *mcpKeepalive) Unwrap() http.ResponseWriter { return k.w }
