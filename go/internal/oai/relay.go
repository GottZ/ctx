package oai

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"time"
)

// The stream relay (wave A03-W10, design/03 §4.10 no. 1, I-03-7): the bounded
// buffer between a /v1 stream's upstream reader and its client writer. The
// lease of a stream ends at the upstream's end, never at the client's drain:
//
//   - the reader is the attempt on the handler's goroutine, under the lease —
//     it reads the upstream frame by frame (llm.ChatRawStream), rewrites each
//     chunk and books its usage and runes there (rewriteChunk, unchanged), and
//     puts the rewritten chunk into the buffer instead of writing it to the
//     client. When the upstream ends, ChatRawStream returns and the walker
//     releases the lease — however far behind the client is;
//   - the writer is one goroutine per stream: it takes the chunks in order
//     and writes them through the stream writer (sse.go frame, the SSE line
//     and the client's write deadline unchanged), while the reader goes on.
//
// The buffer sits after the rewrite on purpose: what the request books (the
// usage chunk, the estimate's runes) and a chunk the relay cannot render are
// the upstream's, settled when the upstream ends — a client that leaves
// during the drain changes neither the meter nor the attempt's outcome.
//
// Bounds (gateway.stream_relay_buffer_bytes, stream_min_drain_rate,
// stream_min_drain_window — A-E2 no. 2): the buffer is one allocation of the
// setting's size per stream, booked in the request's ledger before it is
// made (startRelay); a chunk that does not fit is an overflow: the run ends
// (errRelayOverflow — the upstream is aborted, the lease settles), the
// writer drops what is queued and the route ends the stream with an error
// frame. The writer cuts a client that drains slower than the rate over the
// window while chunks wait for it (drainMeter → the stream writer's write
// deadline), which complements client_write_window, the bound of a single
// write.

// errRelayOverflow is the run's cause when a chunk did not fit the relay
// buffer: the client reads slower than the backend writes, by more than the
// buffer holds. It wraps context.Canceled — the attempt ends as a
// cancellation, never as a health fault of a healthy row.
var errRelayOverflow = fmt.Errorf("oai: the stream's client fell behind by more than gateway.stream_relay_buffer_bytes: %w", context.Canceled)

// errRelayPanic is the run's cause when the relay's writer panicked: the
// stream is broken, the upstream is aborted, the handler raises the panic
// again (wait). It wraps context.Canceled like errTimerPanic.
var errRelayPanic = fmt.Errorf("oai: the stream relay's writer panicked: %w", context.Canceled)

// errRelayClosed is a put after the reader closed the relay — a route bug.
var errRelayClosed = errors.New("oai: stream relay used after its close")

// relayOverflowMessage is the error frame of an overflow.
const relayOverflowMessage = "The client read the stream slower than the model wrote it; the stream was cut."

// relayPanic is a panic of the writer goroutine, raised again on the handler's
// (wait) so that it reaches oai.Recover like a panic of the handler.
type relayPanic struct {
	value any
	stack []byte
}

func (p *relayPanic) Error() string {
	return fmt.Sprintf("oai: the stream relay's writer panicked: %v\n%s", p.value, p.stack)
}

// Unwrap keeps errors.Is on an error value (http.ErrAbortHandler).
func (p *relayPanic) Unwrap() error {
	err, _ := p.value.(error)
	return err
}

// streamRelay is the buffer of one stream: a ring of fixed size holding
// records [length uint32 LE][chunk], contiguous each — a record that does not
// fit at the end starts again at offset 0 (the end's rest stays unused until
// the reader passes it). One reader (put, close), one writer goroutine.
//
// A chunk larger than the whole ring (a rewrite grew it: json.Marshal escapes
// <, > and & six-fold, Fremd-Review 1 F1) is no sign of a slow client: it
// waits by reference in the one oversize slot, in its place in the order,
// and the writer takes it when the records before it are written. A second
// oversize chunk while one still waits is an overflow — then the client is
// behind by at least that chunk.
type streamRelay struct {
	write  func([]byte) error // the stream writer's frame
	stop   <-chan struct{}    // the run's end: the writer drops what is queued
	broken func(error)        // ends the run (a panic of the writer)
	drain  drainMeter
	capAt  func(time.Time) // the stream writer's write-deadline cap (zero: none)

	mu      sync.Mutex
	ring    []byte
	r, w    int  // the next record to take, the next free byte
	end     int  // wrapped: the end of the data above w
	wrapped bool // records lie in [r, end) and [0, w)
	n       int  // records in the ring
	closed  bool // the reader is done: no record follows
	err     error
	more    chan struct{} // a record, the close or the error (capacity 1)
	done    chan struct{} // closed when the writer ended
	pan     *relayPanic

	// big is the oversize chunk waiting by reference (nil: none), bigAt its
	// place: the number of records put before it; puts and takes count every
	// record put and taken, handBig marks the one in the writer's hand.
	big         []byte
	bigAt       uint64
	puts, takes uint64
	handBig     bool

	// until is the attempt's deadline (bound; zero: none): no write ends
	// after it, the drain included — a stream lasts at most its attempt's
	// timeout, as before the relay (Fremd-Review 1 F3 (a)).
	until time.Time
}

// newStreamRelay starts the writer of a relay over ring (made by the caller
// after its booking).
func newStreamRelay(ring []byte, write func([]byte) error, stop <-chan struct{}, broken func(error), d drainMeter, capAt func(time.Time)) *streamRelay {
	rl := &streamRelay{write: write, stop: stop, broken: broken, drain: d, capAt: capAt, ring: ring,
		more: make(chan struct{}, 1), done: make(chan struct{})}
	go rl.run()
	return rl
}

// put queues one chunk (the reader). It never waits for the client: nil when
// the chunk is queued, errRelayOverflow when it does not fit, the writer's
// error once the writer failed (the client is gone). An oversize chunk is
// kept by reference until it is written: the caller must not reuse it
// (rewriteChunk returns a fresh slice per chunk).
func (rl *streamRelay) put(chunk []byte) error {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if rl.err != nil {
		return rl.err
	}
	if rl.closed {
		return errRelayClosed
	}
	need := RelayRecordHeader + len(chunk)
	if need > len(rl.ring) {
		if rl.big != nil {
			return errRelayOverflow
		}
		rl.big, rl.bigAt = chunk, rl.puts
		rl.puts++
		rl.signal()
		return nil
	}
	at, ok := rl.placeLocked(need)
	if !ok {
		return errRelayOverflow
	}
	rl.puts++
	binary.LittleEndian.PutUint32(rl.ring[at:], uint32(len(chunk))) //nolint:gosec // G115: a chunk fits the ring, whose size is an int setting far below 4 GiB (V38/V39)
	copy(rl.ring[at+RelayRecordHeader:], chunk)
	rl.n++
	rl.signal()
	return nil
}

// placeLocked finds room for need contiguous bytes and claims it (w moves).
func (rl *streamRelay) placeLocked(need int) (int, bool) {
	size := len(rl.ring)
	if rl.n == 0 {
		rl.r, rl.w, rl.end, rl.wrapped = 0, 0, 0, false
	}
	switch {
	case !rl.wrapped && size-rl.w >= need:
		at := rl.w
		rl.w += need
		return at, true
	case !rl.wrapped && rl.r >= need:
		// Start again at 0; the data above ends at the old w.
		rl.end, rl.wrapped = rl.w, true
		rl.w = need
		return 0, true
	case rl.wrapped && rl.r-rl.w >= need:
		at := rl.w
		rl.w += need
		return at, true
	}
	return 0, false
}

// close tells the writer that no record follows (the reader is done).
func (rl *streamRelay) close() {
	rl.mu.Lock()
	rl.closed = true
	rl.signal()
	rl.mu.Unlock()
}

// fail ends the relay with err (the first error stays): the writer drops what
// is queued, the reader's next put returns it.
func (rl *streamRelay) fail(err error) {
	rl.mu.Lock()
	if rl.err == nil {
		rl.err = err
	}
	rl.signal()
	rl.mu.Unlock()
}

// signal wakes the writer (mu held; never blocks).
func (rl *streamRelay) signal() {
	select {
	case rl.more <- struct{}{}:
	default:
	}
}

// wait closes the relay and returns once its writer ended — every queued
// chunk written, or the writer failed or stopped — and raises a panic of the
// writer on the calling goroutine (the handler's).
func (rl *streamRelay) wait() {
	rl.close()
	<-rl.done
	rl.mu.Lock()
	p := rl.pan
	rl.pan = nil
	rl.mu.Unlock()
	if p != nil {
		panic(p)
	}
}

// bound sets the attempt's deadline — every attempt calls it with its own
// (a failover attempt's is later).
func (rl *streamRelay) bound(t time.Time) {
	rl.mu.Lock()
	rl.until = t
	rl.mu.Unlock()
}

// capFor is the write-deadline cap of a write of k bytes: the drain meter's,
// never past the attempt's deadline.
func (rl *streamRelay) capFor(now time.Time, k int) time.Time {
	dl := rl.drain.deadline(now, k)
	rl.mu.Lock()
	u := rl.until
	rl.mu.Unlock()
	if !u.IsZero() && (dl.IsZero() || u.Before(dl)) {
		return u
	}
	return dl
}

// join closes the relay and returns once its writer ended, without raising
// its panic — close()'s half (Fremd-Review 1 F2): on a path that did not
// reach wait (a panic of the reader) nothing may write to the response after
// the handler returned. The caller failed the relay first, so the writer
// takes no further record.
func (rl *streamRelay) join() {
	rl.close()
	<-rl.done
}

// panicked reports whether the writer panicked (wait raises it).
func (rl *streamRelay) panicked() bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return rl.pan != nil
}

// run is the writer goroutine.
func (rl *streamRelay) run() {
	defer close(rl.done)
	defer func() {
		if v := recover(); v != nil {
			p := &relayPanic{value: v, stack: debug.Stack()}
			rl.mu.Lock()
			rl.pan = p
			rl.mu.Unlock()
			rl.fail(errRelayPanic)
			if rl.broken != nil {
				rl.broken(errRelayPanic) // the upstream must not run on for a broken stream
			}
		}
	}()
	for {
		rec, ok := rl.next()
		if !ok {
			return
		}
		if rl.capAt != nil {
			rl.capAt(rl.capFor(time.Now(), len(rec)))
		}
		err := rl.write(rec)
		rl.drain.wrote(time.Now(), len(rec))
		rl.release(len(rec))
		if err != nil {
			rl.fail(err)
			return
		}
	}
}

// next is the writer's next record (a slice of the ring, valid until release),
// waiting for one; false once the relay is closed and empty, failed, or the
// run ended. While the writer waits the client is not behind: the drain
// window starts again with the next record.
func (rl *streamRelay) next() ([]byte, bool) {
	for {
		rl.mu.Lock()
		switch {
		case rl.err != nil:
			rl.mu.Unlock()
			return nil, false
		case rl.big != nil && rl.takes == rl.bigAt:
			rec := rl.big
			rl.big, rl.handBig = nil, true
			rl.mu.Unlock()
			select {
			case <-rl.stop:
				rl.fail(context.Canceled)
				return nil, false
			default:
			}
			return rec, true
		case rl.n > 0:
			if rl.wrapped && rl.r == rl.end {
				rl.r, rl.end, rl.wrapped = 0, 0, false
			}
			l := int(binary.LittleEndian.Uint32(rl.ring[rl.r:]))
			start := rl.r + RelayRecordHeader
			rec := rl.ring[start : start+l : start+l]
			rl.mu.Unlock()
			select {
			case <-rl.stop:
				rl.fail(context.Canceled)
				return nil, false
			default:
			}
			return rec, true
		case rl.closed:
			rl.mu.Unlock()
			return nil, false
		}
		u := rl.until
		rl.mu.Unlock()
		rl.drain.idle()
		if rl.capAt != nil {
			rl.capAt(u) // no drain window while the writer waits; the attempt's deadline stays
		}
		select {
		case <-rl.more:
		case <-rl.stop:
			rl.fail(context.Canceled)
			return nil, false
		}
	}
}

// release frees the record of l bytes the writer took.
func (rl *streamRelay) release(l int) {
	rl.mu.Lock()
	rl.takes++
	if rl.handBig {
		rl.handBig = false
		rl.mu.Unlock()
		return
	}
	rl.r += RelayRecordHeader + l
	rl.n--
	switch {
	case rl.n == 0:
		rl.r, rl.w, rl.end, rl.wrapped = 0, 0, 0, false
	case rl.wrapped && rl.r == rl.end:
		// The records above are all taken: the next one is at 0, and the
		// free space is no longer only [w, r).
		rl.r, rl.end, rl.wrapped = 0, 0, false
	}
	rl.mu.Unlock()
}

// drainMeter is the minimum drain rate of a stream's client (design/03 §4.10
// no. 1): while chunks wait for it, the client must have taken at least rate
// bytes per second since its window began, counted when a write ends — a
// write is never cut before one window has passed, and a large chunk gets
// the time its bytes take at the rate. A window that passed resets it; the
// writer waiting for the upstream (no chunk queued) clears it. rate or window
// ≤ 0 (a hand-built Settings; V38 keeps both ≥ 1) disables it.
type drainMeter struct {
	rate    int64
	window  time.Duration
	start   time.Time
	drained int64
}

// deadline is the latest a write of k more bytes may end (zero: no cap).
func (d *drainMeter) deadline(now time.Time, k int) time.Time {
	if d.rate <= 0 || d.window <= 0 {
		return time.Time{}
	}
	if d.start.IsZero() {
		d.start, d.drained = now, 0
	}
	need := time.Duration(float64(d.drained+int64(k)) / float64(d.rate) * float64(time.Second))
	return d.start.Add(max(d.window, need))
}

// wrote counts k bytes the client took; a passed window starts a new one.
func (d *drainMeter) wrote(now time.Time, k int) {
	if d.start.IsZero() {
		return
	}
	d.drained += int64(k)
	if now.Sub(d.start) >= d.window {
		d.start, d.drained = now, 0
	}
}

// idle clears the window: the writer waits for the upstream.
func (d *drainMeter) idle() { d.start, d.drained = time.Time{}, 0 }

// RelayRecordHeader is the size of the length header the relay buffer stores
// in front of every chunk (a little-endian uint32) — the bytes one chunk
// takes in the buffer beyond its own. config's V39 reads it: the buffer must
// hold one frame at gateway.max_stream_frame_bytes plus this header.
const RelayRecordHeader = 4

// startRelay books and makes the stream's relay buffer once per request (the
// first attempt; a failover attempt — only ever before the first chunk —
// reuses it) and starts its writer. The buffer is booked in the request's
// ledger before it is made (FOLGE-P: every term before its allocation) and
// made once, never grown; a refusal is ErrBudgetExhausted — the attempt ends
// before the wire like a refused upstream body.
func (c *chatCall) startRelay() error {
	if c.relay != nil {
		return nil
	}
	size := relayBufferBytes(c.s)
	if !c.cover(size) {
		return ErrBudgetExhausted
	}
	c.relay = newStreamRelay(make([]byte, size), c.sse.frame, c.ctx.Done(), c.cancel,
		drainMeter{rate: c.s.StreamMinDrainRate, window: c.s.StreamMinDrainWindow}, c.sse.capWrites)
	return nil
}

// relayBufferBytes is the relay buffer of one stream:
// gateway.stream_relay_buffer_bytes, at least one frame at the frame cap plus
// its header — V39 keeps the setting there; a hand-built smaller value (a
// test's Settings) is raised to it.
func relayBufferBytes(s Settings) int64 {
	return max(s.StreamRelayBufferBytes, s.MaxStreamFrameBytes+RelayRecordHeader)
}
