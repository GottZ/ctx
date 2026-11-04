// Package fedhop is the hop contract between ctx instances (design/04 §4.1):
// the identity of this instance (this file), the hop header grammar and the
// loop test of a parsed chain (grammar.go), the hop token (token.go), the
// monotony rules of the hop headers (merge.go), the stamp of outgoing wire
// calls with its context carriers (stamp.go) and the evaluation of a peer's
// answer (observe.go). Nothing outside the boot calls it yet: the wire sites
// (F-W4), the Tier-F context (F-W6), the loop guard of incoming requests
// (F-W8) and the learned state (F-W11) build on it. It is a leaf on
// purpose: llm, embed, rerank, backends, httpx and handler import it, so it
// imports nothing of ctx and nothing third-party — the standard library only
// (.golangci.yml rule fedhop-leaf).
//
// Identity is the instance id plus the HMAC key of the hop tokens, read from
// context_instance (migration 163) by store.LoadInstanceIdentity at boot. It
// flows in the request context: the boot sets the default once
// (SetDefaultIdentity) for the detached scheduler contexts, and the root
// middleware (IdentityMiddleware) puts the router's identity into every
// request, so two routers in one test process carry two identities and every
// outgoing call stamps with the identity of the router it serves.
//
// The key never renders. Every formatting path of fmt (each verb, through the
// Formatter), log/slog (LogValue) and encoding/json (an unexported field)
// prints the id only. The key itself lives in a closure: fmt prints an
// unexported field of an enclosing struct by reflection, without calling any
// method of it, and under a wrong verb (%s of the struct) it prints the
// target of a pointer in full — a func value it prints as an address under
// every verb, and nothing reaches the captured bytes.
package fedhop

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync/atomic"
)

// Identity is who this ctx instance is: ID is context_instance.instance_id (a
// UUIDv4, visible to server-admins), key the 32-byte instance_secret, which
// never leaves the process. The zero Identity has neither — what a context
// without identity and without boot default yields (a miswired test; the boot
// refuses to start without one).
//
// Identity is not comparable (the key is a func): compare identities by ID.
// Hold it as a named field, never embedded: an embedding struct inherits
// Format, String and LogValue and then renders as the id alone, hiding its
// other fields.
type Identity struct {
	ID  string
	key func() []byte
}

// NewIdentity builds the identity of instance id with the given key. The key
// is copied: a later write to the caller's slice does not reach it.
func NewIdentity(id string, secret []byte) Identity {
	k := bytes.Clone(secret)
	return Identity{ID: id, key: func() []byte { return k }}
}

// String returns the instance id, never the key.
func (i Identity) String() string { return i.ID }

// GoString renders %#v as the id alone.
func (i Identity) GoString() string { return "fedhop.Identity{ID:" + strconv.Quote(i.ID) + "}" }

// LogValue renders the identity in log/slog as its id.
func (i Identity) LogValue() slog.Value { return slog.StringValue(i.ID) }

// Format renders every fmt verb as the id: %#v as GoString, %q quoted, any
// other verb plain. Without it %d or %x of the struct would print its fields.
func (i Identity) Format(f fmt.State, verb rune) {
	switch {
	case verb == 'v' && f.Flag('#'):
		_, _ = io.WriteString(f, i.GoString())
	case verb == 'q':
		_, _ = io.WriteString(f, strconv.Quote(i.ID))
	default:
		_, _ = io.WriteString(f, i.ID)
	}
}

// defaultIdentity is the identity of a context that carries none.
var defaultIdentity atomic.Pointer[Identity]

// SetDefaultIdentity installs the identity of contexts that carry none — the
// detached contexts of the scheduler. The boot calls it once, right after the
// identity was loaded and before the scheduler and the HTTP server start.
func SetDefaultIdentity(i Identity) { defaultIdentity.Store(&i) }

type identityKey struct{}

// WithIdentity returns ctx carrying i. A zero i is carried as well: the
// context then reads the zero Identity even over a boot default, and Stamp
// refuses to mark its calls (ERROR and federation_stamp_without_identity) —
// the explicit "no identity" of a test. IdentityMiddleware is the door that
// skips a zero identity.
func WithIdentity(ctx context.Context, i Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, i)
}

// IdentityFrom returns the identity ctx carries, else the boot default, else
// the zero Identity.
func IdentityFrom(ctx context.Context) Identity {
	if i, ok := ctx.Value(identityKey{}).(Identity); ok {
		return i
	}
	if p := defaultIdentity.Load(); p != nil {
		return *p
	}
	return Identity{}
}

// IdentityMiddleware puts i into the context of every request — the first
// root middleware of a router. It changes nothing else. A zero i (a router
// built before any identity existed, as most tests do) puts nothing there, so
// the requests read the boot default instead of freezing the zero Identity.
func IdentityMiddleware(i Identity) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if i.ID == "" {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), i)))
		})
	}
}
