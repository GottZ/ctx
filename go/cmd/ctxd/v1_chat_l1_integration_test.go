//go:build integration

package main

// W06-11 over the PRODUCTION router (buildRouter WITHOUT a route wrap): L1 of
// the per-consumer meter gatewayDeps wires (design/06 §4.1 step 9, §4.2,
// masterplan gate "3. Parallelanfrage ⇒ 429"). The fixture is the meter rig
// of W06-10 (newV1MeterRig): a throwaway database, an urgent-lane app
// consumer, one fake engine that holds every answer, the real dispatcher and
// planner; the concurrency limit comes from the environment like in
// production (registry default 2, CTX_GATEWAY_CONSUMER_CONCURRENCY_DEFAULT).
//
//	go test -tags=integration -p 1 -count=1 -run 'TestV1ChatConcurrency' -v ./cmd/ctxd/

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// concurrencyRefusal reports what is wrong with a as the L1 refusal: 429
// rate_limit_error concurrency_limit_exceeded, Retry-After 1, retry-after-ms
// 1000, x-should-retry true ("" = nothing).
func concurrencyRefusal(a v1MeterAnswer) string {
	var env struct {
		Error struct {
			Type    string  `json:"type"`
			Code    *string `json:"code"`
			Message string  `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(a.body, &env); err != nil || env.Error.Code == nil {
		return "no envelope with a code"
	}
	if a.status != http.StatusTooManyRequests || env.Error.Type != "rate_limit_error" || *env.Error.Code != "concurrency_limit_exceeded" || env.Error.Message == "" {
		return "not 429 rate_limit_error concurrency_limit_exceeded"
	}
	if a.header.Get("Retry-After") != "1" || a.header.Get("retry-after-ms") != "1000" || a.header.Get("x-should-retry") != "true" {
		return "headers not Retry-After 1, retry-after-ms 1000, x-should-retry true"
	}
	return ""
}

// parallelChat sends n requests at once; each is held by the engine.
func (r *v1MeterRig) parallelChat(n int) <-chan v1MeterAnswer {
	body := `{"model":"` + v1ChatAlias + `","messages":[{"role":"user","content":"hi"}]}`
	out := make(chan v1MeterAnswer, n)
	for range n {
		go func() {
			st, h, b := r.do(http.MethodPost, "/v1/chat/completions", body)
			out <- v1MeterAnswer{st, h.Clone(), b}
		}()
	}
	return out
}

// waitHits waits until the engine saw n requests (they hold their L1 slots).
func (r *v1MeterRig) waitHits(t *testing.T, n int64) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); r.engine.hits.Load() < n; {
		if time.Now().After(deadline) {
			t.Fatalf("the engine saw %d requests, want %d", r.engine.hits.Load(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestV1ChatConcurrencyOnTheRealRouter is the masterplan gate "3.
// Parallelanfrage ⇒ 429" (briefing W06-11): the engine holds every answer
// 2 s, the concurrency limit is the registry default 2 — of three parallel
// requests of one consumer two are served and one is 429
// concurrency_limit_exceeded with Retry-After 1; after a running one ended,
// the next is served.
func TestV1ChatConcurrencyOnTheRealRouter(t *testing.T) {
	r := newV1MeterRig(t, map[string]string{}, nil)
	r.engine.hold.Store(int64(2 * time.Second))
	running := r.parallelChat(2)
	r.waitHits(t, 2)
	third := <-r.parallelChat(1)
	got := []v1MeterAnswer{<-running, <-running}
	fourth := <-r.parallelChat(1)
	rows := r.chatRows(t)
	for i, a := range got {
		t.Logf("parallel request %d: %s", i+1, a)
	}
	t.Logf("third, parallel: %s", third)
	t.Logf("fourth, after the first two ended: %s", fourth)
	t.Logf("engine hits %d, gateway-chat rows %d", r.engine.hits.Load(), rows)
	for i, a := range got {
		if a.status != http.StatusOK {
			t.Errorf("parallel request %d: %s, want 200", i+1, a)
		}
	}
	if why := concurrencyRefusal(third); why != "" {
		t.Errorf("third, parallel: %s — %s", third, why)
	}
	if fourth.status != http.StatusOK {
		t.Errorf("fourth, after the first two ended: %s, want 200", fourth)
	}
	if n := r.engine.hits.Load(); n != 3 {
		t.Errorf("engine hits %d, want 3 (the refused request never reached it)", n)
	}
	if rows != 3 {
		t.Errorf("gateway-chat rows %d, want 3", rows)
	}
}

// TestV1ChatConcurrencyOneOnTheRealRouter is the gate "max_concurrency = 1":
// with CTX_GATEWAY_CONSUMER_CONCURRENCY_DEFAULT=1 the second parallel request
// is 429 concurrency_limit_exceeded; after the first ended the next is served.
func TestV1ChatConcurrencyOneOnTheRealRouter(t *testing.T) {
	r := newV1MeterRig(t, map[string]string{"CTX_GATEWAY_CONSUMER_CONCURRENCY_DEFAULT": "1"}, nil)
	r.engine.hold.Store(int64(time.Second))
	first := r.parallelChat(1)
	r.waitHits(t, 1)
	second := <-r.parallelChat(1)
	a := <-first
	next := <-r.parallelChat(1)
	t.Logf("first: %s", a)
	t.Logf("second, parallel: %s", second)
	t.Logf("next, after the first ended: %s", next)
	if a.status != http.StatusOK || next.status != http.StatusOK {
		t.Errorf("first %d, next %d, want 200 and 200", a.status, next.status)
	}
	if why := concurrencyRefusal(second); why != "" {
		t.Errorf("second, parallel: %s — %s", second, why)
	}
	if n := r.engine.hits.Load(); n != 2 {
		t.Errorf("engine hits %d, want 2", n)
	}
}
