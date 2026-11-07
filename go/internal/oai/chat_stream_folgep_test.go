package oai_test

// The stream half on FOLGE-P's byte ledger (W02-08 rebase round 2, part A,
// review 4 §11): (A1) serve hands the stream its form and its booking in one
// UpstreamSpec — the upstream is asked for a stream with usage, and the
// attempt's upstream body is booked against the request share before it is
// built; (A3) the budget's refusal of a stream attempt's upstream body ends
// the walk as an admission outcome, as in the non-stream attempt: 503
// server_overloaded before the commit, no failover, no wire call, the meter
// reservation aborted exactly once; (A4) the same refusal after the grace
// commit is the stream's error frame — never a second status (Z2).

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/oai"
)

// jsonStateBytesPerLevel is what the route books per nesting level before
// the JSON check (FOLGE-P: jsonDepthStateBytes; oai.JSONStateReserve is it
// times the decoder's 10 001 levels).
const jsonStateBytesPerLevel = oai.JSONStateReserve / 10001

// TestStreamSpecCarriesStreamAndReserve (A1): during a stream the in-flight
// budget holds the request share as the ledger has it — 3 × the body, grown
// by what the request booked past it: the read, the JSON check's nesting
// state and the attempt's upstream body (reserveUpstream) — plus the
// stream's answer share 2 × max_stream_frame_bytes; the upstream got
// "stream":true with usage asked for. Red: the spec without Reserve (the
// upstream body unbooked: 3 × the body + 2 frames) or without Stream (a
// non-stream upstream request).
func TestStreamSpecCarriesStreamAndReserve(t *testing.T) {
	hold := make(chan struct{})
	entered := make(chan struct{}, 1)
	u := newUpstream(t, "")
	u.setRespond(sseRespond([]sseStep{{hold: hold}, {data: sglangChunk("hello", "stop")}, {data: "[DONE]"}}, entered, nil))
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings)
	frame := rig.settings.Load().MaxStreamFrameBytes
	body := streamBody("chat", `"stop":["`+ledgerMark+`"]`)
	srv := serveHTTP(t, rig.router)
	done := make(chan streamAnswer, 1)
	go func() { done <- postStream(context.Background(), t, srv, body) }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		close(hold)
		t.Fatal("the upstream saw no request")
	}
	inUse := rig.budget.InUse()
	close(hold)
	a := <-done
	raws := ownRaws(u)
	if a.status != http.StatusOK || a.dones() != 1 || len(raws) != 1 {
		t.Fatalf("answer %d, dones %d, upstream requests %d: %q", a.status, a.dones(), len(raws), a.raw)
	}
	var up map[string]json.RawMessage
	if err := json.Unmarshal(raws[0], &up); err != nil {
		t.Fatal(err)
	}
	if string(up["stream"]) != "true" || string(up["stream_options"]) != `{"include_usage":true}` {
		t.Errorf("upstream stream %s, stream_options %s — want a stream with usage asked for", up["stream"], up["stream_options"])
	}
	// The upstream body is booked at the size llm's encoder allocates for it:
	// the bytes sent plus at most a few bytes of its size bound (a comma
	// counted after the last member) — upstreamSlack.
	const upstreamSlack = 8
	b, depth := int64(len(body)), oai.JSONDepth([]byte(body))
	spent := b + depth*jsonStateBytesPerLevel + int64(len(raws[0]))
	want := max(3*b, spent) + 2*frame
	t.Logf("body %d B, depth %d, upstream body %d B: ledger %d (3 × body %d), in use %d, want %d … %d", b, depth, len(raws[0]), spent, 3*b, inUse, want, want+upstreamSlack)
	if inUse < want || inUse > want+upstreamSlack {
		t.Errorf("in-flight budget in use during the stream: %d bytes, want max(3 × %d, %d read + %d × %d nesting + %d upstream) + 2 × %d frame = %d (+ ≤ %d)",
			inUse, b, b, depth, jsonStateBytesPerLevel, len(raws[0]), frame, want, upstreamSlack)
	}
	if !waitFor(2*time.Second, func() bool { return rig.budget.InUse() == 0 }) {
		t.Errorf("budget in use after the stream: %d, want 0", rig.budget.InUse())
	}
}

// bigFrameRow is a row whose upstream frame (its extra_body) the budget of
// the stream tests below cannot book.
func bigFrameRow(u *upstream, name string, prio int) backends.Backend {
	b := u.row(name, backends.TrustFull, prio)
	b.ExtraBody = map[string]any{"vendor_blob": strings.Repeat("z", 64<<10)}
	return b
}

// streamBudget is a budget that holds a small stream request's door share,
// its answer share and a small upstream body (4 KiB of slack), not a
// bigFrameRow's.
func streamBudget(rig *chatRig, body string) {
	rig.setSettings(func(s *oai.Settings) { s.InflightBudget = 3*int64(len(body)) + 2*s.MaxStreamFrameBytes + 4<<10 })
}

// TestStreamUpstreamPastTheBudgetEndsTheWalk (A3): the first row's upstream
// body does not fit the budget — a stream answers 503 server_overloaded with
// Retry-After 1 before its commit, the walk ends there (no failover to the
// second row, which would fit), no wire call, no row, the meter reservation
// aborted exactly once, and no health report against the row — the
// budget's refusal is no fault of the backend. Red: the stream attempt
// passes ErrBudgetExhausted on unwrapped — the walker counts it as the row's
// failure and reports it to the pool.
func TestStreamUpstreamPastTheBudgetEndsTheWalk(t *testing.T) {
	big, small := newUpstream(t, ""), newUpstream(t, "")
	big.setRespond(sseRespond(streamShapes[0].steps(), nil, nil))
	small.setRespond(sseRespond(streamShapes[0].steps(), nil, nil))
	rig := newChatRig(t, []backends.Backend{bigFrameRow(big, "big", 20), small.row("small", backends.TrustFull, 10)}, 4, 1, withStreamSettings)
	body := streamBody("chat", `"stop":["`+ledgerMark+`"]`)
	streamBudget(rig, body)
	a := postStream(context.Background(), t, serveHTTP(t, rig.router), body)
	var e struct {
		Error struct{ Code string } `json:"error"`
	}
	_ = json.Unmarshal([]byte(a.raw), &e)
	t.Logf("answer %d %s; wire hits big %d, small %d; limiter settle/keep/abort/reject %d/%d/%d/%d; rows %d",
		a.status, e.Error.Code, len(ownRaws(big)), len(ownRaws(small)), rig.limiter.settles.Load(), rig.limiter.keeps.Load(),
		rig.limiter.aborts.Load(), rig.limiter.rejects.Load(), len(rig.rows.all()))
	if a.status != http.StatusServiceUnavailable || e.Error.Code != oai.CodeServerOverloaded || a.header.Get("Retry-After") != "1" {
		t.Fatalf("answer %d (Retry-After %q): %q — want 503 server_overloaded with Retry-After 1 before the commit", a.status, a.header.Get("Retry-After"), a.raw)
	}
	if strings.Contains(a.header.Get("Content-Type"), "text/event-stream") {
		t.Errorf("Content-Type %q: the refusal came after a commit", a.header.Get("Content-Type"))
	}
	if n, m := len(ownRaws(big)), len(ownRaws(small)); n != 0 || m != 0 {
		t.Errorf("wire hits: big row %d, small row %d — want none (the walk ends at the refusal, no failover)", n, m)
	}
	l := rig.limiter
	if l.aborts.Load() != 1 || l.settles.Load()+l.keeps.Load()+l.rejects.Load() != 0 {
		t.Errorf("limiter settle/keep/abort/reject %d/%d/%d/%d, want exactly one abort", l.settles.Load(), l.keeps.Load(), l.aborts.Load(), l.rejects.Load())
	}
	if n := len(rig.rows.all()); n != 0 {
		t.Errorf("%d llmlog rows, want 0 (no wire call)", n)
	}
	for _, s := range rig.pool.Status() {
		if s.ConsecutiveFails != 0 || s.LastErrorClass != "" || s.EffectiveState != "active" {
			t.Errorf("row %s got a health report: %d consecutive fails, last class %q, state %q", s.Name, s.ConsecutiveFails, s.LastErrorClass, s.EffectiveState)
		}
	}
	if !waitFor(2*time.Second, func() bool { return rig.budget.InUse() == 0 }) {
		t.Errorf("budget in use after the request: %d, want 0", rig.budget.InUse())
	}
}

// TestStreamUpstreamPastTheBudgetAfterTheCommit (A4): the first row fails
// after the grace commit, the failover row's upstream body does not fit the
// budget — the stream ends with the server_overloaded error frame as its
// last data event, no second status and no bare error body in the stream;
// the reservation keeps its charge (the first row had the request). Red: the
// budget's 503 written to the response itself after the commit.
func TestStreamUpstreamPastTheBudgetAfterTheCommit(t *testing.T) {
	fastPhaseB(t, 100*time.Millisecond)
	u1, u2 := newUpstream(t, ""), newUpstream(t, "")
	var failedAt atomic.Int64
	u1.setRespond(status502(400*time.Millisecond, &failedAt))
	u2.setRespond(sseRespond(streamShapes[0].steps(), nil, nil))
	rig, m := streamMeterRig(t, []backends.Backend{u1.row("one", backends.TrustFull, 20), bigFrameRow(u2, "two", 10)}, 4, 1,
		withWait(100*time.Millisecond), withStream(0, 10*time.Second, 5*time.Second))
	body := streamBody("chat", `"stop":["`+ledgerMark+`"]`)
	streamBudget(rig, body)
	a := postStream(context.Background(), t, serveHTTP(t, rig.router), body)
	code, last := a.errorFrame(t)
	if !waitEnded(m) {
		t.Fatal("the reservation never ended")
	}
	held := m.held(t, rig)
	t.Logf("grace commit, 502, failover past the budget: answer %d, frame %q (last %v); wire hits %d/%d; ends %s; the bucket holds %d tokens",
		a.status, code, last, len(ownRaws(u1)), len(ownRaws(u2)), m.endsOf(), held)
	if a.status != http.StatusOK || code != oai.CodeServerOverloaded || !last {
		t.Fatalf("answer %d, frame %q (last %v): %q — want the server_overloaded frame after the grace commit", a.status, code, last, a.raw)
	}
	for _, line := range strings.Split(a.raw, "\n") {
		if strings.Contains(line, `"error"`) && !strings.HasPrefix(line, "data: ") {
			t.Errorf("a bare error body in the stream (a second answer after the commit): %q", line)
		}
	}
	if n, k := len(ownRaws(u1)), len(ownRaws(u2)); n != 1 || k != 0 {
		t.Errorf("wire hits: first row %d, failover row %d — want 1 and 0", n, k)
	}
	if held != wireReservation || m.endsOf() != "keep" {
		t.Errorf("the bucket holds %d tokens (ends %s), want the reservation %d kept", held, m.endsOf(), wireReservation)
	}
	if !waitFor(2*time.Second, func() bool { return rig.budget.InUse() == 0 }) {
		t.Errorf("budget in use after the request: %d, want 0", rig.budget.InUse())
	}
}
