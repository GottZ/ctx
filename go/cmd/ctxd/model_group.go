package main

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/GottZ/ctx/internal/handler"
)

// modelGroupDeps is what one model group (/v1, /v2) is built from
// (masterplan K23, design/02 K-02-2). Not mounted in W01-6; W02-05 mounts
// /v1 (and W02-10 /v2) through mountModelGroup and runs the door gate
// (internal/handler/v1door) over the real mount.
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
	// inner are the stages after the door, in order (body cap, in-flight
	// budget, hop gate; design/02 §4.2, W02-05ff.).
	inner []func(http.Handler) http.Handler
	// routes are the group's endpoints; the handlers come from here, so
	// the door gate can put a probe behind every route (K-02-2).
	routes []modelRoute
	// notFound and methodNotAllowed are the group's catch-alls (owner 02,
	// K-02-2: oai.NotFound / oai.MethodNotAllowed with Allow); nil keeps
	// chi's. They run behind gate and door like every route of the group.
	notFound, methodNotAllowed http.HandlerFunc
}

// modelRoute is one endpoint of a model group, relative to its prefix.
type modelRoute struct {
	method, pattern string
	handler         http.Handler
}

// mountModelGroup mounts prefix as a sub-router whose every request — a
// registered route, an unknown path, a wrong method, OPTIONS — passes
// access log → gate → door → inner stages before anything answers
// (design/01 §4.2, B4; design/06 §4.6). A missing access log, gate or door
// is a wiring bug and panics at mount time.
func mountModelGroup(r chi.Router, prefix string, deps modelGroupDeps) {
	if deps.access == nil || deps.gate == nil || deps.door == nil {
		panic("mountModelGroup " + prefix + ": the access log, the gate and the model-key door are required")
	}
	r.Route(prefix, func(g chi.Router) {
		g.Use(deps.access.Handler, deps.gate, deps.door.Handler)
		g.Use(deps.inner...)
		if deps.notFound != nil {
			g.NotFound(deps.notFound)
		}
		if deps.methodNotAllowed != nil {
			g.MethodNotAllowed(deps.methodNotAllowed)
		}
		for _, rt := range deps.routes {
			g.Method(rt.method, rt.pattern, rt.handler)
		}
	})
}
