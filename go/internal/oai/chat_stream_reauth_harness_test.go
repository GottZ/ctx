package oai_test

// The revocation harness of the routes (W01-8, masterplan K22): a stand-in
// for the model-key door's in-flight registry (handler.InflightRegistry),
// which ends the request context it hands to the route with
// auth.ErrCredentialRevoked once the request's key stops authenticating.
// Separate from chat_stream_harness_test.go, which compiles against the
// base too (the red runs of the relay gates).

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/GottZ/ctx/internal/auth"
)

// revoker registers every request it serves the way the door's registry
// does: one cancelable request context per request (context.WithCancelCause
// in the door's stage 8), handed to the route; revoke ends every running one
// with auth.ErrCredentialRevoked. A request leaves at its end, its context
// canceled with context.Canceled.
type revoker struct {
	mu      sync.Mutex
	next    uint64
	entries map[uint64]*revokerEntry
	running atomic.Int64 // requests inside wrap right now (leak checks)
}

type revokerEntry struct {
	cancel context.CancelCauseFunc
	ended  bool // canceled with ErrCredentialRevoked already
}

func newRevoker() *revoker { return &revoker{entries: map[uint64]*revokerEntry{}} }

// wrap serves h with the registered request context.
func (rv *revoker) wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancelCause(r.Context())
		rv.mu.Lock()
		rv.next++
		id := rv.next
		rv.entries[id] = &revokerEntry{cancel: cancel}
		rv.mu.Unlock()
		rv.running.Add(1)
		defer func() {
			rv.mu.Lock()
			delete(rv.entries, id)
			rv.mu.Unlock()
			rv.running.Add(-1)
			cancel(context.Canceled)
		}()
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}

// revoke ends every running request not ended yet with
// auth.ErrCredentialRevoked and reports how many.
func (rv *revoker) revoke() int {
	rv.mu.Lock()
	defer rv.mu.Unlock()
	n := 0
	for _, e := range rv.entries {
		if !e.ended {
			e.ended = true
			e.cancel(auth.ErrCredentialRevoked)
			n++
		}
	}
	return n
}
