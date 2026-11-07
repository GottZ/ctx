package main

import (
	"net/http"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/GottZ/ctx/internal/federation"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/oai"
)

// modelGroupDeps is what one model group (/v1, /v2) is built from
// (masterplan K23, design/02 K-02-2). newRouter mounts /v1 (W02-05) and
// /v2 (W02-10) through mountModelGroup from the process's ONE modelGateway
// (modelGateway.group); the door gate (internal/handler/v1door) runs over
// that mount. Mounting a second group marks the router's ingress again —
// the marks are idempotent (fedhop.Ingress).
type modelGroupDeps struct {
	// access is the OUTERMOST stage (W06-9, design/06 §4.6): the access log
	// of the model groups — one line per success, refusals counted per
	// (consumer, reason) and logged at most once a minute. Ahead of gate
	// and door, so the flag's 404 and the door's 401 are counted too; one
	// per process, shared by /v1 and /v2 like the door.
	access *handler.GatewayAccessLog
	// gate runs BEFORE the door (K4): oai.EnabledGate over the gateway
	// getter — gateway.enabled off ⇒ the same 404 on every path, no key
	// oracle, no database query.
	gate func(http.Handler) http.Handler
	// door is the ONE model-key door of the process: /v1 and /v2 share its
	// failure lock, probe bucket and DB semaphore, so two groups never hold
	// twice the database share.
	door *handler.ModelDoor
	// ingress is the federation-ingress registration of the router the group
	// is mounted on (F-W8, design/04 §4.6): mountModelGroup marks it, the
	// boot assert and the gateway.enabled settings write read it. The
	// router's own — NewRouter's, never a process-wide one.
	ingress *fedhop.Ingress
	// maxDepth is the federation.max_depth getter of the group's LoopGuard
	// (federationMaxDepthGetter). The guard itself is no dependency, nor is
	// the PeerIngress behind it (F-W9): both are fixed in the group between
	// the door and inner (K14), so no mount can leave them out.
	maxDepth func() int
	// inner are the stages after the door, the LoopGuard and the
	// PeerIngress, in order (body cap, in-flight budget; design/02 §4.2,
	// W02-05ff.).
	inner []func(http.Handler) http.Handler
	// routes are the group's endpoints; the handlers come from here, so
	// the door gate can put a probe behind every route (K-02-2).
	routes []modelRoute
}

// modelRoute is one endpoint of a model group, relative to its prefix.
type modelRoute struct {
	method, pattern string
	handler         http.Handler
}

// mountModelGroup mounts prefix as a sub-router whose every request — a
// registered route, an unknown path, a wrong method, OPTIONS — passes
// access log → recovery → gate → door → LoopGuard → PeerIngress → inner
// stages before anything answers (design/01 §4.2, B4; design/06 §4.6;
// masterplan K14: EnabledGate → ModelAuth → LoopGuard → PeerIngress → body
// cap/parse). Four stages are fixed by construction, no mount can leave them
// out:
//
//   - oai.Recover right inside the access log: a panic anywhere behind it is
//     the 503 internal_error of the envelope (K5, R-02-14), counted by the
//     access log as a refusal (W06-9 handover), never the plain-text 500 of
//     the house Recovery outside;
//   - the LoopGuard, answering in the OpenAI envelope;
//   - the PeerIngress (F-W9) behind it: it reads the door's consumer, cleans
//     the inbound federation state and resolves the request's policy; once
//     the group is mounted, the router's ingress is registered with both
//     marks (design/04 §4.6 "Registrierung als Mechanismus");
//   - the catch-alls (K-02-2, owner 02): an unknown path is oai.NotFound and
//     a wrong method oai.MethodNotAllowed with Allow — both behind gate and
//     door like every route, so nothing under the prefix falls through to
//     the root's SPA fallback (chi hands a sub-router without its own
//     handler the parent's).
//
// A missing access log, gate, door, ingress or depth getter is a wiring bug and
// panics at mount time.
func mountModelGroup(r chi.Router, prefix string, deps modelGroupDeps) {
	if deps.access == nil || deps.gate == nil || deps.door == nil || deps.ingress == nil || deps.maxDepth == nil {
		panic("mountModelGroup " + prefix + ": the access log, the gate, the model-key door, the ingress registration and the federation.max_depth getter are required")
	}
	loopGuard := handler.LoopGuard(handler.LoopGuardConfig{MaxDepth: deps.maxDepth, Envelope: handler.LoopGuardOpenAI})
	peerIngress := federation.PeerIngress(federation.PeerIngressConfig{Consumer: handler.ConsumerAuthFromContext})
	r.Route(prefix, func(g chi.Router) {
		g.Use(deps.access.Handler, oai.Recover, deps.gate, deps.door.Handler, loopGuard, peerIngress)
		g.Use(deps.inner...)
		g.NotFound(oai.NotFound)
		g.MethodNotAllowed(methodNotAllowed(g, routeMethods(deps.routes)))
		for _, rt := range deps.routes {
			g.Method(rt.method, rt.pattern, rt.handler)
		}
	})
	deps.ingress.MarkLoopGuard()
	deps.ingress.MarkPeerIngress()
}

// routeMethods are the distinct methods of routes, sorted.
func routeMethods(routes []modelRoute) []string {
	var out []string
	for _, rt := range routes {
		if !slices.Contains(out, rt.method) {
			out = append(out, rt.method)
		}
	}
	slices.Sort(out)
	return out
}

// methodNotAllowed is the group's 405: Allow lists every method of the group
// that has a route for the request's path within the group (RFC 9110
// §15.5.6 — chi sets no Allow for a custom handler), then
// oai.MethodNotAllowed answers. The path is the one the group routes on
// (chi's RoutePath of the sub-router), so an escaped wildcard is matched the
// same way the route itself would be.
func methodNotAllowed(g chi.Router, methods []string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := "/"
		if rctx := chi.RouteContext(r.Context()); rctx != nil && rctx.RoutePath != "" {
			path = rctx.RoutePath
		}
		var allow []string
		for _, m := range methods {
			if g.Match(chi.NewRouteContext(), m, path) {
				allow = append(allow, m)
			}
		}
		if len(allow) > 0 {
			w.Header().Set("Allow", strings.Join(allow, ", "))
		}
		oai.MethodNotAllowed(w, r)
	}
}
