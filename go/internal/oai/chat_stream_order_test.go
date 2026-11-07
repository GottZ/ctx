package oai_test

// The place of the stream frame in serve (W02-08 rebase round 2, part A1;
// lead review L-1): c.frame(req.stream) runs right after the top-level parse,
// before resolve and parsePolicy — so (1) a non-streamed request's grant
// read already runs under its request deadline (frame sets it on the
// context), and (2) a refused stream request is recorded as a stream. Moved
// behind parsePolicy, both would change without another gate noticing.

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/oai"
)

// TestStreamFrameBeforeTheResolve: (1) with a request deadline of 300 ms
// the entry's grant read of a non-streamed request sees a deadline at most
// that far (the door's own 1-s deadline would be later); (2) a stream request
// for an unknown model is 404 model_not_found and its access record says
// stream.
func TestStreamFrameBeforeTheResolve(t *testing.T) {
	t.Run("non-stream: the grant read under the request deadline", func(t *testing.T) {
		var mu sync.Mutex
		var left []time.Duration
		u := newUpstream(t, answerSGLang)
		rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withDeps(func(d *oai.Deps) {
			inner := d.HasGrant
			d.HasGrant = func(ctx context.Context, consumerID, aliasID string) (bool, error) {
				if dl, ok := ctx.Deadline(); ok {
					mu.Lock()
					left = append(left, time.Until(dl))
					mu.Unlock()
				}
				return inner(ctx, consumerID, aliasID)
			}
		}))
		rig.setSettings(func(s *oai.Settings) { s.RequestDeadline = 300 * time.Millisecond })
		ca := urgentApp()
		ca.AllModels = false
		rig.ca.Store(ca)
		rig.setGrant(chatAliases()[0].id, true)
		a := rig.post(t, chatBody("chat"))
		mu.Lock()
		defer mu.Unlock()
		t.Logf("answer %d; deadlines left at the grant reads: %v", a.status, left)
		if len(left) == 0 {
			t.Fatalf("answer %d: no grant read with a deadline", a.status)
		}
		if left[0] > 300*time.Millisecond {
			t.Errorf("the entry's grant read had %v left, want at most the request deadline 300 ms (the stream frame runs before resolve)", left[0])
		}
	})
	t.Run("stream: a refused request is recorded as a stream", func(t *testing.T) {
		u := newUpstream(t, "")
		rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings)
		a := postStream(context.Background(), t, serveHTTP(t, rig.router), streamBody("nosuchmodel"))
		var rec *oai.AccessRecord
		select {
		case rec = <-rig.recs:
		case <-time.After(3 * time.Second):
			t.Fatal("no access record")
		}
		t.Logf("answer %d, access record stream %v reason %q", a.status, rec.Stream, rec.Reason)
		if a.status != http.StatusNotFound || !rec.Stream {
			t.Errorf("answer %d, access record stream %v — want 404 and a record that says stream", a.status, rec.Stream)
		}
	})
}
