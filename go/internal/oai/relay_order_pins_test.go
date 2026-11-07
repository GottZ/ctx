package oai_test

// The order pins of the stream relay (A03-W10 hardening 1; Fremd-Review 2
// P-1, R13): what the relay books before it allocates, and what of it is
// left once the handler returned. A booking that slides behind the
// allocation it covers changes no answer — the request is 503
// server_overloaded either way — so the pin measures what the refused
// request allocated.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/oai"
)

// TestRelayRingIsBookedBeforeItIsMade (P-1, mutant Y1): the relay buffer is
// booked in the request's ledger before it is made (startRelay: cover, then
// make). With an 8 MiB buffer and a budget that holds the stream's shares
// but only half the buffer, the ring's booking is refused: 503
// server_overloaded before the wire, and the request allocates less than
// half the buffer. With the budget that holds the buffer too the same
// request is served and allocates the whole ring — the measurement sees it.
// Red with the allocation before the booking (Y1): the refused request
// allocates the 8 MiB ring.
func TestRelayRingIsBookedBeforeItIsMade(t *testing.T) {
	const buffer = 8 << 20
	u := newUpstream(t, "")
	u.setRespond(sseRespond(streamShapes[0].steps(), nil, nil))
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings,
		withRelay(buffer, 1024, 30*time.Second))
	srv := serveHTTP(t, rig.router)
	body := streamBody("chat")
	post := func() streamAnswer { return postStream(context.Background(), t, srv, body) }

	streamBudget(rig, body) // the shares, 4 KiB of slack and the buffer
	full, served := allocOf(post)
	if served.status != http.StatusOK || served.dones() != 1 {
		t.Fatalf("with the buffer in the budget: %d dones %d %q — want 200 (the control)", served.status, served.dones(), served.raw)
	}
	rig.setSettings(func(s *oai.Settings) { s.InflightBudget -= buffer / 2 })
	hits := len(ownRaws(u))
	refusedAlloc, refused := allocOf(post)
	var e struct {
		Error struct{ Code string } `json:"error"`
	}
	_ = json.Unmarshal([]byte(refused.raw), &e)
	t.Logf("buffer %d B: served allocates %d B, refused at the ring's booking allocates %d B (%d %s); wire hits during the refusals %d",
		buffer, full, refusedAlloc, refused.status, e.Error.Code, len(ownRaws(u))-hits)
	if refused.status != http.StatusServiceUnavailable || e.Error.Code != oai.CodeServerOverloaded {
		t.Fatalf("half the buffer short: %d %q — want 503 server_overloaded (the ring's booking refused)", refused.status, refused.raw)
	}
	if n := len(ownRaws(u)) - hits; n != 0 {
		t.Fatalf("%d wire calls during the refusals, want 0 (the buffer is booked before the wire)", n)
	}
	if full < buffer {
		t.Fatalf("the served request allocated %d B, want ≥ the %d B ring — the measurement does not see the ring", full, buffer)
	}
	if refusedAlloc >= buffer/2 {
		t.Fatalf("the request refused at the ring's booking allocated %d B, want < %d — the ring was made before its booking", refusedAlloc, buffer/2)
	}
}
