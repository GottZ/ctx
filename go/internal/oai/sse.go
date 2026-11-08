package oai

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"sync"
	"time"
)

// The SSE relay writer of a streamed chat completion (design/02 §4.7, wave
// W02-08). Its own writer, not the house sseWriter: that one always writes
// an "event:" line, and the OpenAI SDKs read every event not named thread.*
// as a data chunk — a named event would reach client code as a chunk without
// choices. This writer writes only "data: <json>", the comment keepalive
// ": keepalive" (ignored by every SSE client) and "data: [DONE]" once.
//
// The three phases (§4.7, masterplan K7):
//
//	A  before the First acquire returned: nothing — no status, no byte, no
//	   keepalive (a refusal is a real 429 with Retry-After);
//	B  admitted, before the first data frame: nothing until the first frame
//	   or until entry + grace_eff, whichever comes first — then the status,
//	   the headers and a keepalive; never before the admission (arm is called
//	   by the attempt, after the acquire returned);
//	C  committed: frames, a keepalive after keepalive_interval of silence,
//	   an error frame (never [DONE] after it) on a failure.
//
// Every write rolls the connection's write deadline by client_write_window
// (D4): a client that stops reading fails the write, the run ends (broken)
// and the upstream call with it (D3), which frees the lease.

// StreamCommitDistance and StreamCommitGraceCeiling are the clamp of the
// commit grace (design/02 §4.7: grace_eff = clamp(gateway.stream_commit_grace,
// MaxWait + 5 s, 50 s)). Mirrors of config's gatewayCommitDistance and
// gatewayCommitGraceCeiling (V39/V43), pinned equal by
// config.TestGatewayCommitClampMirrors: internal/oai cannot import config.
// The distance keeps the commit above the longest wait in the queue, so a
// refusal or an expired wait budget is always a status, never an in-band
// frame; the ceiling keeps the first byte under a typical 60 s proxy idle
// timeout.
const (
	StreamCommitDistance     = 5 * time.Second
	StreamCommitGraceCeiling = 50 * time.Second
)

// graceWarnEvery throttles the WARN of a clamped grace (lead decision
// O-08-4 (b)): one line per change of the inputs, else at most one per
// window — one line per stream would flood the log at the target scale
// (thousands of streams per hour), and the settings write already logged the
// value.
const graceWarnEvery = time.Minute

// keepaliveLine is the keepalive comment (design/02 §4.7).
var keepaliveLine = []byte(": keepalive\n\n")

// doneLine ends a successful stream, exactly once.
var doneLine = []byte("data: [DONE]\n\n")

// errClientGone is the run's cause when a write to the client failed — it
// stopped reading (the write deadline expired) or the connection broke. It
// wraps context.Canceled: the attempt that relayed the frame ends as a
// cancellation, never as a health fault of a healthy row (Fremd-Review 1
// F-3 of W02-07).
var errClientGone = fmt.Errorf("oai: the client stopped receiving the stream: %w", context.Canceled)

// errTimerPanic is the writer's failure once its timer caught a panic: every
// later write fails, the run ends like after a failed write, and the route
// raises the panic again on the handler's goroutine (rethrow).
var errTimerPanic = fmt.Errorf("oai: the stream timer panicked: %w", context.Canceled)

// timerPanic is a panic of the stream's timer goroutine, raised again on the
// handler's (rethrow) so that it reaches oai.Recover like a panic of the
// handler (Delta-Review 4 F4-2): the value and the timer's stack — the one
// oai.Recover logs is the handler's.
type timerPanic struct {
	value any
	stack []byte
}

func (p *timerPanic) Error() string {
	return fmt.Sprintf("oai: the stream timer panicked: %v\n%s", p.value, p.stack)
}

// Unwrap keeps errors.Is on an error value: a timer that panicked with
// http.ErrAbortHandler stays an abort for oai.Recover.
func (p *timerPanic) Unwrap() error {
	err, _ := p.value.(error)
	return err
}

// commitDistance is the distance in use; tests shorten it
// (export_stream_test.go) so a phase B runs in milliseconds.
var commitDistance = StreamCommitDistance

// effectiveGrace clamps the configured commit grace to [maxWait + distance,
// ceiling]; clamped reports whether the configured value lay outside.
func effectiveGrace(configured, maxWait time.Duration) (grace time.Duration, clamped bool) {
	grace = max(configured, maxWait+commitDistance)
	grace = min(grace, StreamCommitGraceCeiling)
	return grace, grace != configured
}

// graceWarner is the throttle state of the clamp WARN.
var graceWarner struct {
	mu                  sync.Mutex
	at                  time.Time
	configured, maxWait time.Duration
}

// warnClampedGrace logs a clamped grace: at once when the inputs changed,
// else at most once per graceWarnEvery.
func warnClampedGrace(configured, maxWait, grace time.Duration) {
	now := time.Now()
	graceWarner.mu.Lock()
	same := graceWarner.configured == configured && graceWarner.maxWait == maxWait
	if same && now.Sub(graceWarner.at) < graceWarnEvery {
		graceWarner.mu.Unlock()
		return
	}
	graceWarner.at, graceWarner.configured, graceWarner.maxWait = now, configured, maxWait
	graceWarner.mu.Unlock()
	slog.Warn("gateway: stream commit grace clamped (gateway.stream_commit_grace outside [dispatch.external_max_wait + 5s, 50s]; throttled to one line per minute)",
		"configured", configured, "external_max_wait", maxWait, "effective", grace)
}

// streamState is the writer's phase as far as bytes are concerned.
type streamState uint8

const (
	streamOpen      streamState = iota // nothing written: a status can still answer
	streamCommitted                    // 200 and the SSE headers are out
	streamEnded                        // [DONE] or an error frame is out
)

// streamWriter is the relay writer of one stream. Every write holds mu: the
// handler goroutine relays frames, the timer goroutine commits by the grace
// and writes keepalives.
type streamWriter struct {
	w         http.ResponseWriter
	rc        *http.ResponseController
	keepalive time.Duration
	window    time.Duration
	// broken ends the request's run with errClientGone (D4 → D3); called
	// outside mu.
	broken func(error)
	// headers are the response headers of the commit beyond the SSE ones —
	// the meter's x-ratelimit-* of the admission (W06-10 lead decision M2
	// (c): on the successful answer, before its first byte); read under mu
	// when the commit writes the status, on the handler's goroutine or the
	// timer's. nil adds none.
	headers func() http.Header

	mu        sync.Mutex
	state     streamState
	frozen    bool        // the route owns the outcome: the timer writes nothing more
	err       error       // the first failed write
	panicked  *timerPanic // a panic the timer caught, for rethrow
	timer     *time.Timer
	capAt     time.Time // the relay's drain cap of the write deadline (capWrites; zero: none)
	last      time.Time // the last write
	frames    int       // data frames written
	first     time.Time // the first data frame
	noFlushed bool      // the "not flushable" WARN was logged
}

func newStreamWriter(w http.ResponseWriter, s Settings, broken func(error), headers func() http.Header) *streamWriter {
	return &streamWriter{w: w, rc: http.NewResponseController(w), keepalive: s.KeepaliveInterval,
		window: s.ClientWriteWindow, broken: broken, headers: headers}
}

// arm starts phase B: the commit at the latest at `at`, keepalives after it.
// The attempt calls it after its acquire returned — never earlier — so the
// commit is max(admission, entry + grace_eff) by construction. Idempotent.
func (s *streamWriter) arm(at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.frozen || s.timer != nil {
		return
	}
	s.timer = time.AfterFunc(max(time.Until(at), 0), s.tick)
}

// tick is the timer: the grace commit (status, headers, a keepalive), then
// a keepalive after keepalive_interval of silence.
func (s *streamWriter) tick() {
	if err := s.tickLocked(); err != nil {
		s.broken(err)
	}
}

// tickLocked is tick's section under mu. Every section of the writer
// releases mu by defer, also when a writer of the chain or the header
// source panics inside it (Delta-Review 4 F4-2): the route's close() takes
// mu to freeze the writer. The timer's goroutine has no oai.Recover, so a
// panic here is caught: kept for rethrow (the handler raises it again after
// freeze, oai.Recover answers it), the writer failed with errTimerPanic and
// the run ended like after a failed write (tick calls broken).
func (s *streamWriter) tickLocked() (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer func() {
		if v := recover(); v != nil {
			s.panicked = &timerPanic{value: v, stack: debug.Stack()}
			s.err, err = errTimerPanic, errTimerPanic
		}
	}()
	if s.frozen || s.err != nil || s.state == streamEnded {
		return nil
	}
	switch {
	case s.state == streamOpen:
		if err = s.commitLocked(); err == nil {
			err = s.writeLocked(keepaliveLine)
		}
	case s.keepalive > 0 && time.Since(s.last) >= s.keepalive:
		err = s.writeLocked(keepaliveLine)
	}
	if err == nil && s.keepalive > 0 {
		next := s.keepalive - time.Since(s.last)
		if next <= 0 {
			next = s.keepalive
		}
		s.timer.Reset(next)
	}
	return err
}

// frameLine writes one data frame (committing first in phase B): line is
// the frame's whole SSE line — "data: ", the chunk, the blank line — as the
// rewrite wrote it (rewriteChunkInto, FOLGE-S), written as it is. A failed
// write ends the run (D4) and comes back as errClientGone for the relay.
func (s *streamWriter) frameLine(line []byte) error {
	gone, err := s.frameLocked(line)
	if err != nil {
		s.broken(err)
		return errClientGone
	}
	if gone {
		return errClientGone
	}
	return nil
}

// frameLocked is frameLine's section under mu, released by defer: a panic of a
// writer of the chain (Write, Flush, WriteHeader) or of the header source
// runs on — on the relay's writer goroutine since A03-W10, which raises it
// again on the handler's (relay.go) — to the route's close() — which takes
// mu — and to oai.Recover (F4-2). gone: an earlier write had failed, or the
// route owns the outcome (frozen: no frame after freeze, Fremd-Review 1 F2).
func (s *streamWriter) frameLocked(line []byte) (gone bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil || s.frozen {
		return true, nil
	}
	commit := s.state == streamOpen
	if commit {
		if err = s.commitLocked(); err != nil {
			return false, err
		}
	}
	if err = s.writeLocked(line); err != nil {
		return false, err
	}
	if s.frames == 0 {
		s.first = s.last
	}
	s.frames++
	if commit && s.timer != nil && !s.frozen && s.keepalive > 0 {
		// Committed by this frame, before entry + grace: the keepalive
		// counts from this frame, not from the grace (Fremd-Review 2 L-2).
		s.timer.Reset(s.keepalive)
	}
	return false, nil
}

// freeze hands the outcome to the route: the timer writes nothing more.
// It reports whether the stream is committed (then every answer is a frame).
func (s *streamWriter) freeze() (committed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frozen = true
	if s.timer != nil {
		s.timer.Stop()
	}
	return s.state != streamOpen
}

// rethrow raises a panic the timer caught on the calling goroutine — the
// handler's, after freeze — so that it reaches oai.Recover like a panic of
// the handler and the route's close() ends the reservation (F4-2). Without
// one it does nothing.
func (s *streamWriter) rethrow() {
	s.mu.Lock()
	p := s.panicked
	s.panicked = nil
	s.mu.Unlock()
	if p != nil {
		panic(p)
	}
}

// committed reports whether the status is out.
func (s *streamWriter) committed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state != streamOpen
}

// failed reports whether a write to the client failed.
func (s *streamWriter) failed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err != nil
}

// firstFrame is the time of the first data frame (zero: none was written).
func (s *streamWriter) firstFrame() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.first
}

// done ends a successful stream with [DONE] (committing first when no
// frame was written — an upstream that answered nothing but its end). On an
// ended stream it writes nothing.
func (s *streamWriter) done() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if s.state == streamEnded {
		return nil // a stream ends once (Fremd-Review 2 L-1)
	}
	if s.state == streamOpen {
		if err := s.commitLocked(); err != nil {
			return err
		}
	}
	err := s.writeLocked(doneLine)
	s.state = streamEnded
	return err
}

// errorFrame ends a committed stream with the error envelope as its last
// data frame (design/02 §4.6: no [DONE] after it). envelope is the JSON the
// envelope writer produced; a trailing newline is dropped. On an ended
// stream it writes nothing (Fremd-Review 2 L-1).
func (s *streamWriter) errorFrame(envelope []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if s.state == streamEnded {
		return nil
	}
	line := append(append([]byte("data: "), bytes.TrimRight(envelope, "\n")...), "\n\n"...)
	err := s.writeLocked(line)
	s.state = streamEnded
	return err
}

// commitLocked sends the status and the SSE headers — the one commit point,
// called by tick (the grace commit), frame and done. Nothing of an upstream
// response is ever copied: the headers are the ones the stack set (request
// id, security headers, the ingress's own Ctx-Instance), the header source
// (the admission's x-ratelimit-*: the state when the stream commits; what
// the request books after it — a failover refused, an error frame, a cut
// stream — no header can show any more) plus these three.
func (s *streamWriter) commitLocked() error {
	h := s.w.Header()
	if s.headers != nil {
		for k, v := range s.headers() {
			h[k] = v
		}
	}
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	h.Del("Content-Length")
	s.roll()
	s.w.WriteHeader(http.StatusOK)
	s.state = streamCommitted
	if err := s.flushLocked(); err != nil {
		s.err = err
		return err
	}
	return nil
}

// writeLocked writes b under a fresh write deadline and flushes it.
func (s *streamWriter) writeLocked(b []byte) error {
	s.roll()
	if _, err := s.w.Write(b); err != nil {
		s.err = err
		return err
	}
	if err := s.flushLocked(); err != nil {
		s.err = err
		return err
	}
	s.last = time.Now()
	return nil
}

// flushLocked pushes the written bytes to the client. A writer that cannot
// flush is logged once (the stream then reaches the client in the server's
// buffer sizes) and is no failure.
func (s *streamWriter) flushLocked() error {
	err := s.rc.Flush()
	if errors.Is(err, http.ErrNotSupported) {
		if !s.noFlushed {
			s.noFlushed = true
			slog.Warn("gateway: stream response writer cannot flush; frames are buffered")
		}
		return nil
	}
	return err
}

// roll moves the write deadline one client_write_window ahead (D4), never
// past the relay's drain cap (capWrites, A03-W10); a writer without deadline
// support (a test recorder) ignores it. The settings keep the window and the
// keepalive interval >= 1 s (V38); a zero value here (a hand-built Settings)
// rolls nothing and sends no keepalive rather than failing every write or
// spinning the timer.
func (s *streamWriter) roll() {
	var dl time.Time
	if s.window > 0 {
		dl = time.Now().Add(s.window)
	}
	if !s.capAt.IsZero() && (dl.IsZero() || s.capAt.Before(dl)) {
		dl = s.capAt
	}
	if !dl.IsZero() {
		_ = s.rc.SetWriteDeadline(dl)
	}
}

// capWrites caps the write deadline of every later write at t (zero: no cap)
// — the relay's minimum drain rate (A03-W10, drainMeter): a client that
// drains slower than gateway.stream_min_drain_rate over the window while
// chunks wait for it fails the write like one that stopped reading (D4).
func (s *streamWriter) capWrites(t time.Time) {
	s.mu.Lock()
	s.capAt = t
	s.mu.Unlock()
}

// frameCapture is the response writer a refusal is written into after the
// commit: its body — the error envelope — becomes the stream's error frame,
// its status and headers are dropped (a committed stream has sent its own).
// The refusal helpers stay the one source of each code's envelope.
type frameCapture struct {
	h   http.Header
	buf bytes.Buffer
}

func (f *frameCapture) Header() http.Header {
	if f.h == nil {
		f.h = http.Header{}
	}
	return f.h
}

func (f *frameCapture) Write(p []byte) (int, error) { return f.buf.Write(p) }

func (f *frameCapture) WriteHeader(int) {}
