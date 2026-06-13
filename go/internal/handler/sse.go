package handler

// Reusable SSE plumbing for the F6 web-chat token stream (design 06 §3.5/§3.6,
// G37). The sseWriter itself lives in events.go — G34 placed it there as the
// "K8 reuse vehicle" for exactly this path (events.go header). This file adds
// the two pieces the chat stream needs on top of it:
//
//   - comment(): a generic SSE comment line (": <text>"), shared by the events
//     ping and the chat idle heartbeat.
//   - chatSink: adapts the sseWriter onto chat.Sink so the headless engine
//     (internal/chat) streams a turn to one HTTP connection, plus the idle
//     heartbeat that keeps the fronting proxy's read timeout from firing during
//     a long tool call / prompt-processing pause (no event flows then).

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"
)

var (
	// chatHeartbeatGap is the idle threshold: once no event has flowed for this
	// long, the next heartbeat tick writes a ": hb" comment. It MUST stay under
	// the fronting proxy's read timeout (nginx proxy_read_timeout ~60s) — three
	// chat pauses sit above 60s without any event (tool exec, 30k-token prompt
	// processing, the --parallel-1 slot wait, §3.5). Package var so tests shrink
	// it.
	chatHeartbeatGap = 15 * time.Second
	// chatHeartbeatTick is how often runHeartbeat re-checks the idle gap. Finer
	// than the gap so the ": hb" lands close to gap, not a tick late.
	chatHeartbeatTick = 5 * time.Second
)

// comment writes one SSE comment line (": <text>"). The client ignores comments
// (eventsource-parser drops them natively); the write resets the fronting
// proxy's read timeout and rolls the connection write deadline one window ahead,
// exactly like a real event. Mutex-serialized with event()/ping().
func (sw *sseWriter) comment(text string) error {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	_ = sw.rollDeadline()
	if _, err := io.WriteString(sw.w, ": "+text+"\n\n"); err != nil {
		return err
	}
	return sw.rc.Flush()
}

// chatSink adapts the reusable sseWriter onto chat.Sink: the headless engine
// emits a turn's events (session / backend / delta / tool_call / tool_result /
// usage / done / error) through Event, which frames them as SSE.
//
// The 200 stream header is committed LAZILY on the first event, not at
// construction: RunTurn claims the session busy_until first and returns
// ErrTurnBusy WITHOUT emitting any event when the session is already running a
// turn (engine doc). Deferring the header lets the handler still answer a clean
// 409 JSON in that case — committed() reports whether the stream has started.
//
// It also tracks the last write so the idle heartbeat fires only during real
// silence. Every framed write goes through the sseWriter mutex, so the heartbeat
// goroutine and the engine's Event calls on the same connection are serialized.
type chatSink struct {
	w http.ResponseWriter

	mu        sync.Mutex
	sw        *sseWriter // nil until the first event commits the stream header
	lastEvent time.Time
}

func newChatSink(w http.ResponseWriter) *chatSink {
	return &chatSink{w: w, lastEvent: time.Now()}
}

// Event implements chat.Sink. The engine hands plain map/struct payloads that
// always marshal; a marshal failure is laundered to an error frame rather than
// emitting broken JSON or aborting the turn silently. The empty id matches the
// chat protocol (events use ids; chat frames do not, §3.5).
func (s *chatSink) Event(typ string, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		typ, b = "error", []byte(`{"code":"encode_failed","retryable":false}`)
	}
	sw := s.commit()
	return sw.event(typ, "", b)
}

// commit returns the sseWriter, committing the 200 stream header on first use,
// and stamps the write time. Holds the mutex only across the lazy init + stamp,
// never across the actual frame write (so a long write cannot block heartbeat).
func (s *chatSink) commit() *sseWriter {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sw == nil {
		s.w.Header().Set("Content-Type", "text/event-stream")
		s.w.Header().Set("X-Accel-Buffering", "no")
		s.w.WriteHeader(http.StatusOK)
		s.sw = newSSEWriter(s.w)
	}
	s.lastEvent = time.Now()
	return s.sw
}

// committed reports whether the stream header has been sent. The handler reads
// it after RunTurn to decide between a JSON error (header not yet sent) and an
// already-streaming connection that can only carry an error event.
func (s *chatSink) committed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sw != nil
}

// heartbeat writes a ": hb" keepalive iff the stream has started AND has been
// idle longer than gap. Before the first event (no header yet) it is a no-op.
// Returns the sseWriter error (client gone) so runHeartbeat can stop.
func (s *chatSink) heartbeat(gap time.Duration) error {
	s.mu.Lock()
	sw := s.sw
	idle := time.Since(s.lastEvent)
	if sw == nil || idle < gap {
		s.mu.Unlock()
		return nil
	}
	s.lastEvent = time.Now()
	s.mu.Unlock()
	return sw.comment("hb")
}

// runHeartbeat drives the idle keepalive until ctx is done (the handler cancels
// it the moment RunTurn returns). It ticks at chatHeartbeatTick and writes a
// comment only after chatHeartbeatGap of event silence. A write error (client
// vanished) stops it — the turn's own Event calls then fail too and unwind
// RunTurn. Run in its own goroutine by the handler.
func (s *chatSink) runHeartbeat(ctx context.Context) {
	t := time.NewTicker(chatHeartbeatTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if s.heartbeat(chatHeartbeatGap) != nil {
				return
			}
		}
	}
}
