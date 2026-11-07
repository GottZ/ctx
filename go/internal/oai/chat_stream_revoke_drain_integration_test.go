//go:build integration && linux

package oai_test

// The revocation of a stream that drains (A03-W10 landing, after W01-8): a
// stream whose upstream has ended — its lease free, nothing settling — and
// whose client still reads the rest from the relay buffer is ended by the
// door's in-flight registry like a running one. The relay's writer stops at
// the run's end (its stop channel is the request context the registry
// cancels), the queued chunks are dropped, and the route writes the error
// frame invalid_api_key as the last frame. The consumer's L1 slot, held for
// the drain since hardening 1 (Fremd-Review 2 F3b), ends exactly once there.
//
//	go test -tags=integration -p 1 -count=1 -run 'TestRevokeEndsADrainingStream' -v ./internal/oai/

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/keyquota"
	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/GottZ/ctx/internal/oai"
)

// drainFrames is the drain gate's stream: 8 content chunks of 32 KiB, all in
// the 2 MiB relay buffer once the upstream ended; the client's 4 KiB
// buffers against the route's 4 KiB send buffer keep the writer on the
// second chunk while the client does not read.
const drainFrames = 8

// drainRevokeBound is "at once" for the error frame after the revocation
// while the client reads (index Q19: 50 ms); W01-8 measured 2–6 ms on a
// running stream. Under -race it is logged, not judged.
const drainRevokeBound = 50 * time.Millisecond

// TestRevokeEndsADrainingStream is the landing gate "revocation during a
// drain": a real ctxm_ key behind the real model-key door and its registry,
// the meter's L1 slot of the consumer at max_concurrency 1, the production
// in-flight budget (256 MiB) and relay buffer (2 MiB).
//
//  1. The upstream delivers its 8 chunks at once and ends; the client reads
//     the first one and stops: the lease is free (held 0, settling 0) while
//     the stream drains, the reservation has not ended (F3b: no end while a
//     client drains), the budget holds the ring, a second stream of the
//     consumer is 429 concurrency_limit_exceeded, the registry holds the
//     draining stream.
//  2. The key is revoked the way model-key-revoke does it (the row's
//     revoked_at, then the registry's CancelKey) — one running request — and
//     the client reads on: at most the chunk in the writer's hand arrives,
//     then the error frame invalid_api_key as the last data event, no
//     [DONE], fewer than the 8 chunks; within 50 ms of the revocation.
//  3. The reservation ended exactly once (settle: the upstream's usage), the
//     meter admits one and refuses a second for concurrency (the slot free
//     once), the in-flight budget back to 0 (the ring unbooked once — a
//     Release is sealed, a second one would not show, a missing one would),
//     the registry empty with one request ended by it, the lease still free,
//     the upstream never aborted (it had ended), and the key's next request
//     is a 401 at the door.
func TestRevokeEndsADrainingStream(t *testing.T) {
	e := newReauthEnvWith(t, bigFrames(drainFrames, 32<<10, 0), 4, reauthProdAuth.InflightSweepInterval)
	m := &endMeter{rigMeter: newRigMeter(l1One, true)}
	cm := &consumerMeter{endMeter: m}
	e.rig.deps.Limiter = cm
	const budget = 268435456 // gateway.inflight_budget's default
	e.rig.setSettings(func(s *oai.Settings) { s.InflightBudget = budget })
	s := *e.rig.settings.Load()
	ring := max(s.StreamRelayBufferBytes, s.MaxStreamFrameBytes+oai.RelayRecordHeader)
	host := e.up.srv.URL

	c := startDrainClient(t, e.routes(), e.bearer()...)
	if c.resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", c.resp.StatusCode)
	}
	if data, err := c.event(); err != nil || !strings.Contains(data, `"content":"xxx`) {
		t.Fatalf("first event %.80q, %v — want the first content chunk", data, err)
	}

	// 1. The drain.
	if !e.ended.wait(5*time.Second) || e.ended.canceled {
		t.Fatalf("the upstream did not run to its end (ended %v, aborted %v)", !e.ended.at.IsZero(), e.ended.canceled)
	}
	if !waitHeld(e.rig.disp, host, 0, 2500*time.Millisecond) {
		held, _ := heldOn(e.rig.disp, host)
		t.Fatalf("lease held %d 2.5 s after the upstream's end — the lease ends with the upstream", held)
	}
	if st := settlingOn(e.rig, host); st != 0 {
		t.Fatalf("%d leases settling after a clean upstream end, want 0", st)
	}
	time.Sleep(50 * time.Millisecond)
	if n := endCount(m); n != 0 {
		t.Fatalf("ends %q while the client drains, want none — the slot ends at the drain's end (F3b)", m.endsOf())
	}
	inUse := e.rig.budget.InUse()
	if inUse < ring {
		t.Fatalf("budget in use %d B while the stream drains, want ≥ the ring %d B", inUse, ring)
	}
	second := postStream(context.Background(), t, c.srv, streamBody("chat"), e.bearer()...)
	checkL1Refusal(t, "second stream of the consumer while the first drains", second)
	if n := endCount(m); n != 0 {
		t.Fatalf("ends %q after the refused second stream, want none", m.endsOf())
	}
	if !waitFor(time.Second, func() bool { return e.door.Stats().Inflight == 1 }) {
		t.Fatalf("registry holds %d running requests while the stream drains, want 1", e.door.Stats().Inflight)
	}

	// 2. The revocation.
	e.revoke(t)
	revokedAt := time.Now()
	n := e.door.Inflight().CancelKey(e.keyID)
	var datas []string
	var errAt time.Time
	var readErr error
	for {
		data, err := c.event()
		if err != nil {
			readErr = err
			break
		}
		datas = append(datas, data)
		if strings.Contains(data, `"error"`) {
			errAt = time.Now()
		}
	}
	after, dones := 0, 0
	for _, d := range datas {
		switch {
		case d == "[DONE]":
			dones++
		case strings.Contains(d, `"content":"xxx`):
			after++
		}
	}
	last := ""
	if len(datas) > 0 {
		last = datas[len(datas)-1]
	}
	took := time.Duration(-1) // -1: no error frame
	if !errAt.IsZero() {
		took = errAt.Sub(revokedAt)
	}
	t.Logf("drain: budget %d B (ring %d B); revocation (CancelKey %d) → %d more content chunks, error frame after %v, %d [DONE], end %v; last %.120q",
		inUse, ring, n, after, took.Round(10*time.Microsecond), dones, readErr, last)
	if n != 1 {
		t.Errorf("CancelKey counted %d running requests, want 1 (the draining stream)", n)
	}
	if !errors.Is(readErr, io.EOF) {
		t.Errorf("the stream ended with %v, want a clean end of the body", readErr)
	}
	if r5Code(last) != oai.CodeInvalidAPIKey || dones != 0 {
		t.Fatalf("last data event %.200q, [DONE] %d — want the error frame invalid_api_key last, no [DONE]", last, dones)
	}
	if after > 1 || 1+after >= drainFrames {
		t.Errorf("%d content chunks after the revocation (%d of %d in all) — want at most the one in the writer's hand: the drain ends with the run", after, 1+after, drainFrames)
	}
	if took > drainRevokeBound {
		if raceEnabled {
			t.Logf("error frame %v after the revocation under -race (bound %v logged, not judged)", took, drainRevokeBound)
		} else {
			t.Errorf("error frame %v after the revocation, want ≤ %v", took, drainRevokeBound)
		}
	}

	// 3. The books.
	if !waitFor(2*time.Second, func() bool { return endCount(m) >= 1 }) {
		t.Fatalf("the reservation never ended (ends %q)", m.endsOf())
	}
	time.Sleep(150 * time.Millisecond) // a second end of the same request would land here
	if ends := m.endsOf(); ends != "settle" {
		t.Errorf("ends %q, want exactly one settle (the upstream's usage, booked at the drain's end)", ends)
	}
	if meter, ok := drainMeterSlot(m, cm.ca.Load()); !ok {
		t.Errorf("at the meter %s — want the first allowed and the second refused for concurrency (the slot free once)", meter)
	}
	if !waitFor(2*time.Second, func() bool { return e.rig.budget.InUse() == 0 }) {
		t.Errorf("budget in use %d B after the stream, want 0 (the ring unbooked)", e.rig.budget.InUse())
	}
	if !waitFor(time.Second, func() bool { return e.door.Stats().Inflight == 0 }) || e.door.Stats().InflightCancelled != 1 {
		st := e.door.Stats()
		t.Errorf("registry after the stream: %d running, %d ended by it — want 0 and 1", st.Inflight, st.InflightCancelled)
	}
	if held, _ := heldOn(e.rig.disp, host); held != 0 || settlingOn(e.rig, host) != 0 || e.ended.canceled {
		t.Errorf("after the revocation: lease held %d, settling %d, upstream aborted %v — want 0, 0, false (the lease ended with the upstream)",
			held, settlingOn(e.rig, host), e.ended.canceled)
	}
	if b := postStream(context.Background(), t, c.srv, streamBody("chat"), e.bearer()...); b.status != http.StatusUnauthorized {
		t.Errorf("next request with the revoked key: %d, want 401 at the door", b.status)
	}
}

// consumerMeter is the end meter that keeps the consumer the door handed the
// route (its ID is the database's, not the rig's).
type consumerMeter struct {
	*endMeter
	ca atomic.Pointer[auth.ConsumerAuth]
}

func (m *consumerMeter) Admit(ctx context.Context, ca *auth.ConsumerAuth, a modelalias.Alias, est oai.Estimate) (oai.LimitReservation, *oai.LimitRefusal) {
	m.ca.Store(ca)
	return m.endMeter.Admit(ctx, ca, a, est)
}

// drainMeterSlot is r5MeterSlot on the door's consumer: one direct admission
// on its chat key passes, a second one beside it is refused for concurrency.
func drainMeterSlot(m *endMeter, ca *auth.ConsumerAuth) (string, bool) {
	if ca == nil {
		return "no admission seen", false
	}
	s := keyquota.Subject{ConsumerID: ca.ConsumerID, TenantID: ca.TenantID, Op: keyquota.OpChat, Lane: keyquota.LaneUrgent}
	r1, d1 := m.m.Admit(s, m.lim, keyquota.Estimate{ClientMax: true})
	r2, d2 := m.m.Admit(s, m.lim, keyquota.Estimate{ClientMax: true})
	if r1 != nil {
		r1.Abort()
	}
	if r2 != nil {
		r2.Abort()
	}
	ok := d1.Allowed && !d2.Allowed && d2.Reason == keyquota.ReasonConcurrency
	return fmt.Sprintf("consumer %s: first allowed=%v, second allowed=%v reason=%s", ca.ConsumerID, d1.Allowed, d2.Allowed, d2.Reason), ok
}

// settlingOn is the number of host's leases that settle (A03-W10).
func settlingOn(r *chatRig, host string) int {
	for _, ts := range r.disp.Snapshot().Targets {
		if ts.Origin == host {
			return ts.Settling
		}
	}
	return 0
}
