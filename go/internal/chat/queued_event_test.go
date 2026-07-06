package chat

// MW8 (D3-W4, §4.4 V2b / C6): the stream path emits a periodic "queued"
// keepalive event while a stream acquire waits in the dispatcher queue, so a
// long wait behind a foreign lease survives a reverse-proxy idle timeout
// instead of dying byte-less. And the rejection saturation event is generic —
// no backend name (a C2 topology signal for a saturation event).

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/store"
)

func (s *eventSink) has(typ string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.events {
		if e == typ {
			return true
		}
	}
	return false
}

// TestRunStreamEmitsQueuedEventWhileWaiting: a stream acquire forced to queue
// (single slot held) emits at least one "queued" keepalive before admission,
// carrying no target/backend. The RED PROBE (reverting acquireQueued to a bare
// blocking Acquire) makes this fail — proving the gate can see the silent
// death the event prevents.
func TestRunStreamEmitsQueuedEventWhileWaiting(t *testing.T) {
	orig := queuedEventInterval
	queuedEventInterval = 15 * time.Millisecond
	t.Cleanup(func() { queuedEventInterval = orig })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, sseDelta("hi")+sseTail)
	}))
	defer srv.Close()

	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	origin, err := dispatch.NormalizeOrigin(srv.URL)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	d.UpdatePolicy(dispatch.Policy{Targets: map[string]dispatch.TargetPolicy{origin: {Slots: 1}}})
	adm := interactiveStreamAdmission(t, d)

	// Hold the only slot so the stream acquire must queue.
	holder, _, err := d.Acquire(context.Background(), dispatch.Request{
		Target: dispatch.Target{Origin: srv.URL}, Class: dispatch.ClassInteractive, Role: "chat",
	})
	if err != nil {
		t.Fatalf("holder acquire: %v", err)
	}

	e := streamEngine()
	sink := newEventSink()
	outcome := make(chan streamOutcome, 1)
	go func() {
		outcome <- e.runStream(context.Background(), []backends.Backend{streamBackend("b", srv.URL)},
			[]llm.ChatMsg{{Role: "user", Content: "hi"}}, false, 0, 1, "sess", "key-s", backends.SensPublic, adm, sink)
	}()

	// Wait until the stream acquire is queued, then let a couple keepalive
	// ticks fire before releasing the slot.
	waiting := func() bool {
		for _, ts := range d.Snapshot().Targets {
			if ts.Origin == origin && ts.Interactive.Waiting == 1 {
				return true
			}
		}
		return false
	}
	deadline := time.Now().Add(5 * time.Second)
	for !waiting() {
		if time.Now().After(deadline) {
			t.Fatal("stream acquire never queued")
		}
		time.Sleep(2 * time.Millisecond)
	}
	time.Sleep(60 * time.Millisecond) // ~4 keepalive intervals
	holder.Release()

	so := <-outcome
	if !so.served || so.err != nil {
		t.Fatalf("outcome = served %v err %v", so.served, so.err)
	}
	if !sink.has("queued") {
		t.Fatal("no queued keepalive event during the wait — a proxy idle-kill would die silently (C6)")
	}
	// The queued event must be generic: no target/backend leak (C2/B6).
	q := sink.last("queued")
	if q == nil {
		t.Fatal("queued event carried no data")
	}
	for _, k := range []string{"backend", "origin", "host", "target"} {
		if _, ok := q[k]; ok {
			t.Fatalf("queued event leaks %q: %v", k, q)
		}
	}
}

// TestSaturatedEventCarriesNoBackend: a stream rejection maps to a generic
// saturation event — retryable:true (distinct from the terminal
// no_eligible_backend), and WITHOUT the backend name (a saturation event's
// target is a C2 foreign-load oracle). RED PROBE: re-adding the backend field
// makes this fail.
func TestSaturatedEventCarriesNoBackend(t *testing.T) {
	e := streamEngine()
	sink := newEventSink()
	so := streamOutcome{
		backend: streamBackend("secret-gpu", "http://gpu:8089"),
		err:     &llm.AdmissionError{Err: dispatch.ErrTargetSaturated, Host: "http://gpu:8089"},
	}
	if err := e.finishUnserved(context.Background(), &store.ChatSession{}, so, backends.SensPublic, time.Now(), 1, sink); err != nil {
		t.Fatalf("finishUnserved: %v", err)
	}
	ev := sink.last("error")
	if ev == nil {
		t.Fatal("no error event emitted")
	}
	if ev["code"] != "saturated" || ev["retryable"] != true {
		t.Fatalf("event = %v, want code saturated retryable true", ev)
	}
	if _, ok := ev["backend"]; ok {
		t.Fatalf("saturated event leaks the backend name (C2): %v", ev)
	}
	if ev["code"] == "no_eligible_backend" {
		t.Fatal("saturated must stay distinct from the terminal no_eligible_backend")
	}
}
