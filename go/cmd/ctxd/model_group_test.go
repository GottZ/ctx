package main

// F-W8 (design/04 §4.6, §7.2 no. 1): mountModelGroup registers the router's
// federation ingress when it mounts a group — the guard is fixed in the
// group, so "a model group exists on this router" and "its ingress
// protection is mounted" are the same statement. The registration is the
// router's own: another router's Ingress stays unregistered.

import (
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/oai"
)

func TestModelGroupRegistersItsRoutersIngress(t *testing.T) {
	mine, other := &fedhop.Ingress{}, &fedhop.Ingress{}
	if mine.Registered() {
		t.Fatal("registered before any mount")
	}
	mountModelGroup(chi.NewRouter(), "/v1", modelGroupDeps{
		access: testAccessLog(), gate: func(h http.Handler) http.Handler { return h }, door: &handler.ModelDoor{},
		ingress: mine, maxDepth: func() int { return 3 },
	})
	if !mine.Registered() {
		t.Error("mountModelGroup did not register the router's ingress")
	}
	if other.Registered() {
		t.Error("mounting one router's group registered another router's ingress")
	}
}

// testAccessLog is a model-group access log with empty settings — the
// outermost stage every mountModelGroup needs (W06-9), for tests whose subject
// is not the log.
func testAccessLog() *handler.GatewayAccessLog {
	return handler.NewGatewayAccessLog(handler.GatewayAccessLogConfig{Settings: func() oai.Settings { return oai.Settings{} }})
}
