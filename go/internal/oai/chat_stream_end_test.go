package oai_test

// Two corrections of the stream writer after Fremd-Review 2 of W02-08:
// L-1 — after the commit a refusal whose writer wrote nothing sends no frame
// (no empty data event before the real error frame), and an ended stream
// takes no second error frame and no [DONE]; L-2 — once the first data
// frame has committed the stream, the keepalive counts from that frame,
// not from entry + the commit grace.

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/oai"
)

// TestStreamCommittedWriterWindow is L-1, the committed twin of
// TestStreamWriterDomain/shutdown_in_the_writer_window: the stream has
// committed by its grace, row 1 fails, the failover acquire is refused and
// the shutdown lands in the admission writer's window (the writer, seeing
// the canceled run, writes nothing). The client reads exactly one data
// event — the error frame internal_error — and no [DONE]. Red: the empty
// capture written as a frame (`data: ` before the error frame; the Go SDK
// fails with "unexpected end of JSON input" instead of the error).
func TestStreamCommittedWriterWindow(t *testing.T) {
	fastPhaseB(t, 100*time.Millisecond)
	u1 := newUpstream(t, "")
	var failedAt atomic.Int64
	u1.setRespond(status502(400*time.Millisecond, &failedAt))
	u2 := newUpstream(t, "")
	u2.setRespond(sseRespond(streamShapes[0].steps(), nil, nil))
	life, stop := context.WithCancel(context.Background())
	defer stop()
	var calls atomic.Int64
	rig := newChatRig(t, []backends.Backend{u1.row("one", backends.TrustFull, 20), u2.row("two", backends.TrustFull, 10)}, 2, 1,
		withStreamSettings, withWait(100*time.Millisecond), withStream(0, 10*time.Second, 5*time.Second), func(r *chatRig) {
			r.deps.Lifecycle = life
			r.deps.Planner = shWindowPlanner{AdmissionPlanner: r.planner, stop: stop, calls: &calls}
		})
	srv := serveHTTP(t, rig.router)
	defer occupyExternal(t, rig.disp, u2.srv.URL)()
	a := postStream(context.Background(), t, srv, streamBody("chat"))
	t.Logf("status %d, writer calls %d, wire %q", a.status, calls.Load(), a.raw)
	if a.status != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("status %d, writer calls %d — want a committed stream whose failover refusal reached the writer once", a.status, calls.Load())
	}
	if n := strings.Count(a.raw, "data:"); n != 1 || strings.Contains(a.raw, "data: \n") {
		t.Errorf("%d data events (wire %q), want exactly one — the error frame, no empty event before it", n, a.raw)
	}
	if code, last := a.errorFrame(t); code != oai.CodeInternalError || !last || a.dones() != 0 {
		t.Errorf("error frame %q (last %v, [DONE] %d), want the shutdown's internal_error as the last frame", code, last, a.dones())
	}
}

// TestStreamKeepaliveAfterFirstFrame is L-2 (design/02 §4.7: a keepalive
// after every keepalive_interval of silence once committed): the first data
// frame commits the stream long before entry + grace (production grace
// 40 s), then the upstream is silent for 1 s at a keepalive interval of
// 200 ms ⇒ keepalives in that silence, the first one an interval after the
// frame. Red: the timer still waits for entry + grace (no keepalive).
func TestStreamKeepaliveAfterFirstFrame(t *testing.T) {
	u := newUpstream(t, "")
	u.setRespond(sseRespond([]sseStep{{data: sglangChunk("a", "")}, {wait: time.Second, data: sglangChunk(" end", "stop")}, {data: "[DONE]"}}, nil, nil))
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 30)}, 4, 1, withStreamSettings,
		withStream(40*time.Second, 200*time.Millisecond, 5*time.Second))
	a := postStream(context.Background(), t, serveHTTP(t, rig.router), streamBody("chat"))
	var first, ka time.Time
	n := 0
	for _, e := range a.events {
		switch {
		case e.data != "" && first.IsZero():
			first = e.at
		case e.comment == "keepalive" && !first.IsZero():
			if ka.IsZero() {
				ka = e.at
			}
			n++
		}
	}
	gap := time.Duration(-1) // -1: no keepalive after the frame
	if !ka.IsZero() {
		gap = ka.Sub(first)
	}
	t.Logf("grace 40 s, keepalive 200 ms, 1 s of silence after the first frame: %d keepalives, the first %v after the frame", n, gap.Round(time.Millisecond))
	if a.status != http.StatusOK || a.dones() != 1 {
		t.Fatalf("status %d, [DONE] %d", a.status, a.dones())
	}
	if n < 3 || gap > 200*time.Millisecond+300*time.Millisecond {
		t.Errorf("%d keepalives in 1 s of silence (interval 200 ms), the first %v after the frame — want ≥ 3, the first ≈ 200 ms after it", n, gap)
	}
}
