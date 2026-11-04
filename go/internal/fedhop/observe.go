package fedhop

// The answer side of a wire call on the sender (design/04 §4.4): Observe
// reads the response headers of a peer and reports what it learned to the
// Observer of the context. It only reads and only excludes — learned values
// are unauthenticated, they never admit anything. The learned state itself
// (Pool, self-loop exclusion, reset on reload) is F-W11's.

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
)

// Observer is the learning sink of the backend pool, context-bound with a
// boot default like the Identity.
type Observer interface {
	// NotePeerInstance: the row answered with Ctx-Instance.
	NotePeerInstance(backendID, instance string)
	// MarkSelfLoop: the row leads back to this instance.
	MarkSelfLoop(backendID string)
}

// ErrEmbedSpaceMismatch: a row with a pinned embed space answered without
// echoing it (design/04 §4.11) — the vector is from another space.
var ErrEmbedSpaceMismatch = errors.New("fedhop: peer embed space mismatch")

type (
	observerKey struct{}
	observerBox struct{ o Observer }
)

var defaultObserver atomic.Pointer[observerBox]

// SetDefaultObserver installs the observer of contexts that carry none —
// the boot sets the backend pool.
func SetDefaultObserver(o Observer) { defaultObserver.Store(&observerBox{o: o}) }

// WithObserver returns ctx carrying o (two routers in one test process, two
// pools).
func WithObserver(ctx context.Context, o Observer) context.Context {
	return context.WithValue(ctx, observerKey{}, observerBox{o: o})
}

func observerFrom(ctx context.Context) Observer {
	if b, ok := ctx.Value(observerKey{}).(observerBox); ok {
		return b.o
	}
	if p := defaultObserver.Load(); p != nil {
		return p.o
	}
	return nil
}

// Observe evaluates the answer of one wire call to target t, on every tier
// (a row registered as generic that answers as a ctx instance must be
// noticed too):
//
//   - Ctx-Instance (one canonical UUID) ⇒ NotePeerInstance; the own instance
//     id ⇒ MarkSelfLoop;
//   - status 508 with an own Ctx-Loop-Token ⇒ MarkSelfLoop — the row leads
//     back here, whatever kind of consumer this instance is on the other side;
//   - a 2xx answer to a target with a pinned embed space that does not echo
//     it in Ctx-Embed-Space ⇒ ErrEmbedSpaceMismatch (an error status is
//     classified by its status, not here).
//
// Unreadable header values are ignored.
func Observe(ctx context.Context, t Target, status int, resp http.Header) error {
	if obs := observerFrom(ctx); obs != nil {
		id := IdentityFrom(ctx)
		self := false
		if inst, ok := single(resp, HeaderInstance); ok && validUUID(inst) {
			obs.NotePeerInstance(t.BackendID, inst)
			self = inst == id.ID
		}
		if status == http.StatusLoopDetected {
			if tok, ok := single(resp, HeaderLoopToken); ok && id.IsOwnToken(tok) {
				self = true
			}
		}
		if self {
			obs.MarkSelfLoop(t.BackendID)
		}
	}
	if t.EmbedSpace != "" && status >= 200 && status < 300 {
		// An unreadable pin matches nothing: Stamp did not send it, so the
		// peer filtered nothing, and an echo of the same garbage proves nothing.
		if got, ok := single(resp, HeaderEmbedSpace); !ok || got != t.EmbedSpace || !validEmbedSpace(got) {
			return ErrEmbedSpaceMismatch
		}
	}
	return nil
}
