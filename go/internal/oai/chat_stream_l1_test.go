package oai_test

// Part C of the W02-08 rebase round 2: L1 of the per-consumer meter (W06-11)
// on stream: true. A stream holds its consumer's concurrency slot from the
// meter's admission to its end — while it waits, while it relays, until its
// reservation ends — so one more is 429 concurrency_limit_exceeded before
// any byte of a stream; every exit of the stream path ends the reservation
// exactly once, and with it the slot (keyquota frees the slot in each of
// Settle, Keep, Abort and Reject).

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/keyquota"
	"github.com/GottZ/ctx/internal/oai"
)

// l1One are the limits of a consumer with one open request at a time.
var l1One = keyquota.Limits{RPM: 1000, TPM: 1 << 30, CompletionReserve: 1024, Concurrency: 1}

// l1Engine is a fake streaming engine that acts on the request's message:
// "ok" streams to [DONE]; "hold" streams one chunk and holds until release
// is closed (or the request ends), then [DONE]; "refuse" answers 400;
// "cut" streams one chunk and holds until the request ends; "nostatus"
// reads the request and holds without a status line until it ends.
func l1Engine(t *testing.T, release <-chan struct{}) *upstream {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Messages []struct{ Content string } `json:"messages"`
		}
		_ = json.Unmarshal(raw, &body)
		what := ""
		if len(body.Messages) == 1 {
			what = body.Messages[0].Content
		}
		switch what {
		case "refuse":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"engine refuses"}}`)
			return
		case "nostatus":
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: "+sglangChunk("hello", "")+"\n\n")
		w.(http.Flusher).Flush()
		switch what {
		case "hold":
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		case "cut":
			<-r.Context().Done()
			return
		}
		_, _ = io.WriteString(w, "data: "+sglangChunk(" end", "stop")+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return &upstream{srv: srv}
}

// l1Body is a stream request whose message tells l1Engine what to do.
func l1Body(what string) string {
	return `{"model":"chat","stream":true,"messages":[{"role":"user","content":"` + what + `"}]}`
}

// endCount is the number of reservations m ended so far.
func endCount(m *endMeter) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.ends)
}

// checkL1Refusal checks a 429 concurrency_limit_exceeded as W06-11 writes
// it: Retry-After 1, retry-after-ms 1000, x-should-retry true, a JSON body
// and none of the six x-ratelimit-* headers — never a stream.
func checkL1Refusal(t *testing.T, what string, a streamAnswer) {
	t.Helper()
	var e struct {
		Error struct{ Code string } `json:"error"`
	}
	_ = json.Unmarshal([]byte(a.raw), &e)
	if a.status != http.StatusTooManyRequests || e.Error.Code != oai.CodeConcurrencyLimitExceeded {
		t.Fatalf("%s: %d %q — want 429 concurrency_limit_exceeded", what, a.status, a.raw)
	}
	h := a.header
	if h.Get("Retry-After") != "1" || h.Get("retry-after-ms") != "1000" || h.Get("x-should-retry") != "true" {
		t.Errorf("%s: Retry-After %q, retry-after-ms %q, x-should-retry %q — want 1, 1000, true", what, h.Get("Retry-After"), h.Get("retry-after-ms"), h.Get("x-should-retry"))
	}
	if ct := h.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("%s: Content-Type %q, want application/json (no stream before the admission)", what, ct)
	}
	for _, k := range []string{"x-ratelimit-limit-requests", "x-ratelimit-remaining-requests", "x-ratelimit-reset-requests",
		"x-ratelimit-limit-tokens", "x-ratelimit-remaining-tokens", "x-ratelimit-reset-tokens"} {
		if v := h.Get(k); v != "" {
			t.Errorf("%s: carries %s %q, want none of the six on a refusal", what, k, v)
		}
	}
}

// TestL1StreamConcurrency: with max_concurrency 1 a running stream holds
// the slot — a second stream of the same consumer is 429
// concurrency_limit_exceeded before any byte of a stream and never reaches
// the engine; once the first ended (its reservation ended) the next one is
// served; a stream its client abandons after the commit gives the slot
// back. Over real sockets.
func TestL1StreamConcurrency(t *testing.T) {
	release := make(chan struct{})
	u := l1Engine(t, release)
	m := &endMeter{rigMeter: newRigMeter(l1One, true)}
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings,
		withDeps(func(d *oai.Deps) { d.Limiter = m }))
	srv := serveHTTP(t, rig.router)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ls, each := openAndRead(t, ctx, srv, l1Body("hold"))
	if _, ok := nextData(each, 5*time.Second); !ok {
		close(release)
		t.Fatal("the first stream relayed no frame")
	}
	second := postStream(context.Background(), t, srv, l1Body("ok"))
	checkL1Refusal(t, "second stream while the first runs", second)
	close(release)
	for {
		e, ok := nextData(each, 5*time.Second)
		if !ok {
			t.Fatal("the first stream did not end with [DONE]")
		}
		if e.data == "[DONE]" {
			break
		}
	}
	_ = ls.Close()
	if !waitFor(3*time.Second, func() bool { return endCount(m) == 1 }) {
		t.Fatalf("ends %q, want the first stream's one end", m.endsOf())
	}
	third := postStream(context.Background(), t, srv, l1Body("ok"))
	t.Logf("second %d, first ended %s, third %d dones %d", second.status, m.endsOf(), third.status, third.dones())
	if third.status != http.StatusOK || third.dones() != 1 {
		t.Errorf("third stream after the first ended: %d dones %d — want the slot back", third.status, third.dones())
	}
	// A committed stream its client abandons: the slot is held while it
	// runs and comes back with its end.
	ctx4, cancel4 := context.WithCancel(context.Background())
	ls4, each4 := openAndRead(t, ctx4, srv, l1Body("cut"))
	if _, ok := nextData(each4, 5*time.Second); !ok {
		cancel4()
		t.Fatal("the fourth stream relayed no frame")
	}
	checkL1Refusal(t, "a stream beside the committed fourth", postStream(context.Background(), t, srv, l1Body("ok")))
	ends := endCount(m)
	cancel4()
	_ = ls4.Close()
	if !waitFor(3*time.Second, func() bool { return endCount(m) == ends+1 }) {
		t.Fatalf("ends %q: the abandoned stream's reservation did not end", m.endsOf())
	}
	fifth := postStream(context.Background(), t, srv, l1Body("ok"))
	t.Logf("fourth abandoned after its first frame: ends %s, fifth %d dones %d", m.endsOf(), fifth.status, fifth.dones())
	if fifth.status != http.StatusOK || fifth.dones() != 1 {
		t.Errorf("a stream after the abandoned one: %d dones %d — want the slot back", fifth.status, fifth.dones())
	}
}

// TestL1StreamExitsFreeTheSlotOnce: every exit of the stream path ends its
// reservation exactly once and with it the slot — after the exit a holding
// stream gets the one slot (freed: not 429) and a probe beside it is 429
// (freed once: the count did not go below zero).
func TestL1StreamExitsFreeTheSlotOnce(t *testing.T) {
	for _, tc := range []struct {
		name, what string
		end        string // how the exit's reservation ends
		status     int    // the exit's status
		cut        bool   // the client goes away after the first frame
		setup      func(t *testing.T, rig *chatRig, u *upstream) (after func())
	}{
		{name: "served to [DONE]", what: "ok", end: "settle", status: http.StatusOK},
		{name: "the backend refuses before the commit", what: "refuse", end: "keep", status: http.StatusBadRequest},
		{name: "client gone after the first frame", what: "cut", end: "settle", status: http.StatusOK, cut: true},
		{name: "client gone before the backend's status", what: "nostatus", end: "keep", cut: true},
		{name: "the budget refuses the upstream body", what: "ok", end: "abort", status: http.StatusServiceUnavailable,
			setup: func(t *testing.T, rig *chatRig, _ *upstream) func() {
				body := l1Body("ok")
				rig.setSettings(func(s *oai.Settings) { s.InflightBudget = 3*int64(len(body)) + 2*s.MaxStreamFrameBytes })
				return func() { rig.setSettings(func(s *oai.Settings) { s.InflightBudget = 1 << 30 }) }
			}},
		{name: "the first acquire refused (phase A)", what: "ok", end: "reject", status: http.StatusTooManyRequests,
			setup: func(t *testing.T, rig *chatRig, u *upstream) func() {
				return occupyExternal(t, rig.disp, u.srv.URL)
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release := make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			u := l1Engine(t, release)
			row := u.row("spark", backends.TrustFull, 10)
			if tc.end == "abort" {
				row.ExtraBody = map[string]any{"vendor_blob": strings.Repeat("z", 64<<10)}
			}
			m := &endMeter{rigMeter: newRigMeter(l1One, true)}
			slots := 4
			if tc.end == "reject" {
				slots = 2 // one external slot: the occupied one fills it
			}
			rig := newChatRig(t, []backends.Backend{row}, slots, 1, withStreamSettings, withWait(0),
				withDeps(func(d *oai.Deps) { d.Limiter = m }))
			srv := serveHTTP(t, rig.router)
			after := func() {}
			if tc.setup != nil {
				after = tc.setup(t, rig, u)
			}
			if tc.cut {
				ctx, cancel := context.WithCancel(context.Background())
				ls, each := openAndRead(t, ctx, srv, l1Body(tc.what))
				if tc.what == "cut" {
					if _, ok := nextData(each, 5*time.Second); !ok {
						cancel()
						t.Fatal("no frame before the cut")
					}
				} else {
					time.Sleep(200 * time.Millisecond) // the engine holds before its status
				}
				cancel()
				_ = ls.Close()
			} else {
				a := postStream(context.Background(), t, srv, l1Body(tc.what))
				if a.status != tc.status {
					t.Fatalf("the exit answered %d, want %d: %q", a.status, tc.status, a.raw)
				}
			}
			if !waitFor(3*time.Second, func() bool { return endCount(m) == 1 }) {
				t.Fatalf("ends %q after the exit, want one", m.endsOf())
			}
			after()
			if got := m.endsOf(); got != tc.end {
				t.Errorf("the exit's reservation ended %q, want %q", got, tc.end)
			}
			// The slot is free: a holding stream gets it.
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ls, each := openAndRead(t, ctx, srv, l1Body("hold"))
			if _, ok := nextData(each, 5*time.Second); !ok {
				t.Fatalf("after the exit a stream got no frame (ends %q): the slot was not freed", m.endsOf())
			}
			// Freed once: the probe beside it is refused.
			probe := postStream(context.Background(), t, srv, l1Body("ok"))
			checkL1Refusal(t, "probe beside the holding stream", probe)
			close(release)
			_ = ls.Close()
		})
	}
}
