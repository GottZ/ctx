package oai_test

// The consumer the re-authentication hands back (W02-08 hardening after
// Fremd-Review 2, M-1), on the fake door: the period resolves the alias
// with it, and so does the check right before a non-stream write. The real
// door and database: TestReauthFreshConsumerDB.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/oai"
)

// TestReauthHandsBackTheConsumer: (a) the door reads the consumer without
// all_models and without a grant ⇒ the committed stream ends with
// model_not_found within the period; (b) non-stream: the period hands back
// the consumer without all_models while its grant still holds (the request
// runs on), the grant is withdrawn after that period and the answer comes
// before the next ⇒ the check before the write resolves with the consumer
// of the period: 404 model_not_found, never a 200. Red: the period keeps
// the entry's consumer ((a) runs to its end), the check before the write
// uses the entry's consumer ((b) answers 200).
func TestReauthHandsBackTheConsumer(t *testing.T) {
	const period = 300 * time.Millisecond
	withdrawn := func() *reauthAnswer {
		ca := urgentApp()
		ca.AllModels = false
		return &reauthAnswer{ok: true, ca: ca}
	}
	t.Run("stream: all_models withdrawn", func(t *testing.T) {
		hold := make(chan struct{})
		defer close(hold)
		f := &reauthFake{}
		f.set(*withdrawn())
		rig, u, end := reauthRig(t, f, []sseStep{{data: sglangChunk("hello", "")}, {hold: hold}})
		t.Cleanup(oai.SetReauthEvery(period))
		ls, each := openAndRead(t, context.Background(), serveHTTP(t, rig.router), streamBody("chat"))
		defer func() { _ = ls.Close() }()
		if _, ok := nextData(each, 5*time.Second); !ok {
			t.Fatal("no frame")
		}
		at := time.Now()
		e, ok := nextData(each, period+3*time.Second)
		if !ok || !strings.Contains(e.data, `"code":"model_not_found"`) {
			t.Fatalf("frame %q (got one: %v), want model_not_found: the period resolves with the consumer the door reads now", e.data, ok)
		}
		if took := e.at.Sub(at); took > period+500*time.Millisecond {
			t.Errorf("frame %v after the commit, want within one period", took)
		}
		if !end.wait(time.Second) || !waitHeld(rig.disp, u.srv.URL, 0, time.Second) {
			t.Error("the upstream was not aborted or the lease not freed")
		}
	})
	t.Run("non-stream: the check before the write", func(t *testing.T) {
		t.Cleanup(oai.SetReauthEvery(period))
		f := &reauthFake{}
		f.set(*withdrawn())
		u := newUpstream(t, answerSGLang)
		b := newBlocker()
		defer b.open()
		u.setRespond(b.respond(answerSGLang))
		rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withReauth(f))
		rig.setGrant(idChat, true) // the entry's consumer holds all_models and a grant
		done := make(chan chatAnswer, 1)
		go func() { done <- rig.post(t, chatBody("chat")) }()
		waitEntered(t, b, 1)
		if !waitFor(period+2*time.Second, func() bool { return f.calls.Load() >= 1 }) {
			t.Fatal("no re-auth")
		}
		time.Sleep(20 * time.Millisecond) // the period's grant read is done
		rig.setGrant(idChat, false)
		b.open()
		a := <-done
		code, _, _ := a.envelope(t)
		t.Logf("answer %d %s after %d re-auths", a.status, code, f.calls.Load())
		if a.status != http.StatusNotFound || code != oai.CodeModelNotFound {
			t.Fatalf("answer %d %s, want 404 model_not_found: the check before the write resolves with the consumer of the last period", a.status, a.body)
		}
	})
}

// TestGrantReadDeadlinePin: every grant read of the alias resolution is
// bounded by the door's own database deadline, 1 s (handler
// modelAuthDBDeadline; lead decision of the hardening: a named constant,
// not a setting). The watch tests use the same value (grantDeadline).
func TestGrantReadDeadlinePin(t *testing.T) {
	if oai.GrantReadDeadline != time.Second || grantDeadline != oai.GrantReadDeadline {
		t.Fatalf("grant read deadline %v (tests %v), want the door's 1 s", oai.GrantReadDeadline, grantDeadline)
	}
}
