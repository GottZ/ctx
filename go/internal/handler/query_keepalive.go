package handler

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// The /api/query keepalive (issue #220; the MCP twin is mcp_keepalive.go).
//
// HandleQuery answers through a queryKeepalive from the moment its body is
// parsed. It keeps a long request alive on two levels:
//
//   - The write deadline, for every request. http.Server's WriteTimeout
//     (main.go: 120s) is an ABSOLUTE deadline fixed when the request was read.
//     A request still working past it was computed, logged as a 200 and never
//     delivered: the CPU synthesis fallback first (the old body heartbeat
//     rolled the deadline from the search on), then the retrieval-only path
//     and the phase before the search, which no heartbeat covered (#220).
//     Every heartbeatInterval the ticker rolls the deadline one
//     heartbeatWriteWindow ahead WITHOUT writing a byte, so status, headers
//     and body stay exactly what they were.
//   - Keepalive bytes. A reverse proxy's read timeout is reset only by bytes:
//     1xx responses do not survive nginx (proxy_buffering swallows them, the
//     body is lost — measured), a flushed body byte does. With the bytes on,
//     each tick also writes one space and flushes, and the first one commits
//     `200 application/json`; leading whitespace is valid JSON (RFC 8259), so
//     the final body still decodes. The synthesis path switches them on with
//     commit() once its search has succeeded (X4, F3-P2: with pool chains any
//     synthesis can outlast a 60s proxy). A request with `keepalive: true` has
//     them from its start, whatever `synthesize` is: the first space goes out
//     after one interval, so a request that answers sooner keeps its real
//     status and a byte-identical body.
//
// Once a keepalive 200 is on the wire a later status cannot replace it: a
// failure answered after that is the 200 whose body carries the error (`error`
// or `error_code`, the body the status would have carried). That is why the
// bytes before the search are opt-in: every refusal of the request path (quota,
// embed, grants, search) happens before it.
//
// The handler reaches the writer through this type only, and every method that
// touches the connection takes the mutex, so handler writes and ticks never
// interleave. The handler's headers live in a map of their own and are copied
// out when the handler commits its status: the tick's first commit sets the
// Content-Type on the real header map, and the two goroutines never write one
// map.
type queryKeepalive struct {
	w   http.ResponseWriter
	rc  *http.ResponseController
	ctx context.Context // the request context: the ticker ends with it
	hdr http.Header     // the handler's headers, copied out by its own commit

	done chan struct{} // closed by close(): ends the ticker at once

	mu        sync.Mutex
	closed    bool // HandleQuery has returned: w must not be touched again
	committed bool // a status line is out, the keepalive's or the handler's
	ticked    bool // the status line that is out is the keepalive's 200
	payload   bool // the handler has started its body: no space after that
	bytes     bool // ticks write a space, not only roll the deadline
	warned    bool // the deadline roll failed once and said so
}

// armQueryKeepalive wraps w and starts the ticker. bytes arms the keepalive
// bytes from the start (`keepalive: true`). The caller answers through the
// returned writer and calls close before it returns.
func armQueryKeepalive(ctx context.Context, w http.ResponseWriter, bytes bool) *queryKeepalive {
	k := &queryKeepalive{
		w:     w,
		rc:    http.NewResponseController(w),
		ctx:   ctx,
		hdr:   make(http.Header),
		done:  make(chan struct{}),
		bytes: bytes,
	}
	// Both package vars are read here, on the caller's goroutine, not per tick.
	go k.run(heartbeatInterval, heartbeatWriteWindow)
	return k
}

// heartbeatInterval is the keepalive cadence — well under a typical 60s proxy
// read timeout. A package var so tests can shrink it.
var heartbeatInterval = 25 * time.Second

// heartbeatWriteWindow is the rolling write-deadline budget per tick: 90s =
// ~3 missed ticks before the server gives up on a dead connection.
var heartbeatWriteWindow = 90 * time.Second

// run is the ticker goroutine. It ends at once when the handler has returned
// or the client is gone, and otherwise on the first tick that gives up.
func (k *queryKeepalive) run(interval, window time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-k.done:
			return
		case <-k.ctx.Done():
			return
		case <-t.C:
			if !k.tick(window) {
				return
			}
		}
	}
}

// tick rolls the write deadline and, with the bytes on, writes one space. It
// reports false when the ticker is to end: the handler has returned or started
// its body, the client is gone, the handler committed a status of its own, or
// the space could not be written.
func (k *queryKeepalive) tick(window time.Duration) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closed || k.payload || k.ctx.Err() != nil {
		return false
	}
	if err := k.rc.SetWriteDeadline(time.Now().Add(window)); err != nil && !k.warned {
		// Loud once, instead of a silent cut at the server WriteTimeout.
		k.warned = true
		slog.Warn("query keepalive: cannot extend write deadline, server WriteTimeout caps long responses",
			"error", err)
	}
	if !k.bytes {
		return true
	}
	first := !k.committed
	if first {
		k.commitLocked()
	} else if !k.ticked {
		return false // the handler's own status is out: the response is its now
	}
	if _, err := io.WriteString(k.w, " "); err != nil {
		return false
	}
	if err := k.rc.Flush(); err != nil && first {
		k.flushFailed(err)
	}
	return true
}

// commit puts the 200 on the wire now and switches the keepalive bytes on: the
// synthesis path, from the moment its search has succeeded. A response whose
// status is already out keeps it.
func (k *queryKeepalive) commit() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.bytes = true
	if k.committed || k.closed {
		return
	}
	k.commitLocked()
	if err := k.rc.Flush(); err != nil {
		k.flushFailed(err)
	}
	if err := k.rc.SetWriteDeadline(time.Now().Add(heartbeatWriteWindow)); err != nil {
		slog.Warn("query keepalive: cannot extend write deadline, server WriteTimeout caps long responses",
			"error", err)
	}
}

// commitLocked writes the keepalive's 200. Caller holds the mutex.
func (k *queryKeepalive) commitLocked() {
	k.w.Header().Set("Content-Type", "application/json")
	k.w.WriteHeader(http.StatusOK)
	k.committed, k.ticked = true, true
}

// flushFailed reports a writer that cannot stream: likely a middleware
// ResponseWriter wrapper without Unwrap(), so the controller cannot reach the
// real Flusher. The spaces then buffer to the end of the response and a
// reverse proxy still times out — loud here instead of silent buffering.
func (k *queryKeepalive) flushFailed(err error) {
	slog.Warn("query keepalive: response writer not flushable, keepalive ineffective", "error", err)
}

// close ends the keepalive. After it returns no tick touches the writer.
func (k *queryKeepalive) close() {
	k.mu.Lock()
	k.closed = true
	k.mu.Unlock()
	close(k.done)
}

// The http.ResponseWriter the handler sees.

func (k *queryKeepalive) Header() http.Header { return k.hdr }

// WriteHeader commits the handler's status, unless the keepalive's 200 is
// already out: then the status is dropped and the body that follows carries the
// answer.
func (k *queryKeepalive) WriteHeader(code int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closed || k.ticked {
		return
	}
	k.commitHeaderLocked()
	k.w.WriteHeader(code)
}

func (k *queryKeepalive) Write(b []byte) (int, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closed {
		return 0, http.ErrHandlerTimeout
	}
	k.commitHeaderLocked()
	k.payload = true
	return k.w.Write(b)
}

// commitHeaderLocked copies the handler's headers out before its first status
// or byte. Caller holds the mutex.
func (k *queryKeepalive) commitHeaderLocked() {
	if k.committed {
		return
	}
	dst := k.w.Header()
	for key, vals := range k.hdr {
		dst[key] = vals
	}
	k.committed = true
}
