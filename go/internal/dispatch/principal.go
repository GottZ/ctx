package dispatch

import "context"

// principalHook resolves the caller's Principal from a request context. It is
// the ONE binding seam of the B8 class authorization (Vorhaben E wave MW4,
// design/03 §4.1.1): Request carries NO principal parameter, so a STORED
// AuthResult in a variable is structurally inert — only a context that went
// through the authenticated request path grants interactive. dispatch is a
// leaf package (stdlib only) and cannot read the handler ctx key itself, so
// the handler layer supplies a cycle-free wrapper around
// AuthResultFromContext (precedent: config.SetRequestScopeHook).
var principalHook func(context.Context) Principal

// SetPrincipalHook installs the request→principal resolver. Like
// config.SetRequestScopeHook it is set ONCE at boot — here BEFORE the
// scheduler goroutines spawn, because Acquire (unlike the HTTP-only
// request-scope hooks) also runs on detached background contexts; the
// unsynchronized write rides the boot happens-before. A nil hook (pre-boot,
// tests) yields the zero Principal: every interactive acquire then runs into
// the B8 downgrade — fail-closed toward the protected good, never fail-open
// into the interactive queue.
func SetPrincipalHook(h func(context.Context) Principal) { principalHook = h }

// principalFromContext derives the admission principal of one Acquire.
// Detached background contexts (lifecycleCtx, scheduler arms, dream/audit/
// backfill) carry no AuthResult and resolve to the zero Principal — which
// makes them background in the structural sense of design/03 §4.1.1.
func principalFromContext(ctx context.Context) Principal {
	if principalHook == nil {
		return Principal{}
	}
	return principalHook(ctx)
}
