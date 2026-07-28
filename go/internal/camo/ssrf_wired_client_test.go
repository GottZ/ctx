// H13c — negative probe for "outgoing fetches NEVER reach private addresses",
// consumer 2 of 3: the Camo image proxy (design/04 §4.8 table row 3;
// fetch.go:45 builds the production client over ssrfguard.GuardedDialer).
//
// camo_test.go already refuses loopback/private targets via Fetch — but it
// asserts only the LAUNDERED status 502. Fetch collapses "guard refused",
// "connection refused", "DNS failure" and "redirect policy" into that one
// status, so those tests cannot tell a working guard from an unreachable
// port: pointed at a DEAD loopback port they stay green with no guard at all.
//
// This probe closes that gap on two axes: the target is a LIVE loopback server
// that would serve a valid PNG, and the assertion is made on the wired client
// itself, where the ssrfguard marker still exists before Fetch launders it.
//
// Mutation that turns this RED (documented, test-local): build the fetcher
// with newFetcher(DefaultMaxBytes, &net.Dialer{}) — an unguarded dialer ⇒ the
// PNG comes back and hits == 1.
package camo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestProductionFetcherRefusesLiveLoopbackTarget(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngBytes)
	}))
	defer srv.Close()
	target := srv.URL + "/internal.png"

	f := NewFetcher(DefaultMaxBytes)

	// --- policy surface: the caller sees a laundered 502, no image ---
	res, err := f.Fetch(context.Background(), target)
	if err == nil {
		t.Fatalf("the production fetcher delivered %d bytes from a loopback upstream — ssrfguard is not wired into NewFetcher", len(res.Body))
	}
	assertStatus(t, err, http.StatusBadGateway)
	if n := hits.Load(); n != 0 {
		t.Fatalf("the loopback upstream saw %d requests — the dial was not aborted before the bytes flowed", n)
	}

	// --- wiring surface: the marker survives on the client itself ---
	// Fetch maps every transport failure to 502, so the status above cannot
	// distinguish the guard from a dead port. The wired client can.
	req, rerr := http.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
	if rerr != nil {
		t.Fatalf("build probe request: %v", rerr)
	}
	resp, derr := f.client.Do(req)
	if derr == nil {
		resp.Body.Close() //nolint:errcheck
		t.Fatal("the wired camo client connected to loopback — the Control hook is missing")
	}
	if !strings.Contains(derr.Error(), "SSRF guard") {
		t.Fatalf("camo dial failed for the wrong reason (want the ssrfguard marker): %v", derr)
	}

	// --- control half: the very same target IS reachable without the guard ---
	cresp, cerr := srv.Client().Get(target) //nolint:noctx // reachability control, no deadline needed
	if cerr != nil {
		t.Fatalf("control fetch failed — the fixture is unreachable, so the probes above prove nothing: %v", cerr)
	}
	defer cresp.Body.Close() //nolint:errcheck
	if cresp.StatusCode != http.StatusOK {
		t.Fatalf("control status = %d, want 200", cresp.StatusCode)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("control hits = %d, want exactly 1 (the guarded halves must have contributed none)", n)
	}
}
