//go:build integration

package main

// Integration pin of the fail-closed limiter case at the real router (W02-07
// Härtung H-5, taken over from the Fremd-Review 2 probe; turned around by
// W06-10). Until W06-10 the production router had no Limiter at all and this
// test pinned that every chat request was refused. W06-10 wires the meter
// (TestV1ChatMeterOnTheRealRouter: the same production router now serves and
// meters), and this pin keeps the fail-closed case it was written for (its
// own header: "a Limiter that is nil again must still refuse"): the SAME
// production router, its chat route rebuilt from gatewayDeps with the
// Limiter set to nil again, refuses fail-closed — 503 service_unavailable
// with the largest Retry-After step of dispatch.external_retry_after_steps
// and x-should-retry: true, before any engine contact and without a
// gateway-chat row; GET /v1/models is not affected.

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"testing"

	"github.com/GottZ/ctx/internal/oai"
)

func TestV1ChatLimiterMissingOnTheRealRouter(t *testing.T) {
	var r *v1MeterRig
	// The route seam of buildRouter: the chat route as production builds it
	// (gatewayDeps over the router's own dispatcher and meter), the Limiter
	// put back to nil — the wiring defect this pin is about.
	wrap := func(routes []modelRoute) []modelRoute {
		out := append([]modelRoute(nil), routes...)
		for i, rt := range out {
			if rt.method == http.MethodPost && rt.pattern == "/chat/completions" {
				out[i].handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					d := r.deps
					d.Limiter = nil
					oai.ChatCompletions(d).ServeHTTP(w, req)
				})
			}
		}
		return out
	}
	r = newV1MeterRig(t, map[string]string{}, wrap)
	if r.deps.Limiter == nil {
		t.Fatal("the production deps carry no Limiter — the pin would not put a wired limiter back to nil")
	}

	steps := r.disp.ExternalRetryAfterSteps()
	if len(steps) == 0 {
		t.Fatal("the dispatcher has no external Retry-After steps")
	}
	wantRA := strconv.Itoa(int(math.Ceil(steps[len(steps)-1].Seconds())))
	for i := range 3 {
		st, h, b := r.do(http.MethodPost, "/v1/chat/completions", `{"model":"`+v1ChatAlias+`","messages":[{"role":"user","content":"hi"}]}`)
		var env struct {
			Error struct {
				Code *string `json:"code"`
				Type string  `json:"type"`
			} `json:"error"`
		}
		if err := json.Unmarshal(b, &env); err != nil || env.Error.Code == nil {
			t.Fatalf("request %d: %d %s, want the 503 service_unavailable envelope", i, st, b)
		}
		if st != http.StatusServiceUnavailable || *env.Error.Code != "service_unavailable" || env.Error.Type != "service_unavailable_error" {
			t.Fatalf("request %d: %d %s, want 503 service_unavailable (Limiter nil)", i, st, b)
		}
		if got := h.Get("Retry-After"); got != wantRA {
			t.Fatalf("request %d: Retry-After = %q, want the largest step %s of %v", i, got, wantRA, steps)
		}
		if got := h.Get("x-should-retry"); got != "true" {
			t.Fatalf("request %d: x-should-retry = %q, want true", i, got)
		}
	}
	if st, _, b := r.do(http.MethodGet, "/v1/models", ""); st != http.StatusOK || !bytes.Contains(b, []byte(`"`+v1ChatAlias+`"`)) {
		t.Fatalf("GET /v1/models without a Limiter: %d %s, want 200 listing %s", st, b, v1ChatAlias)
	}
	if n := r.engine.hits.Load(); n != 0 {
		t.Fatalf("engine contacts %d, want 0 (refused before the planner)", n)
	}
	if n := r.chatRows(t); n != 0 {
		t.Fatalf("gateway-chat rows %d, want 0", n)
	}
}
