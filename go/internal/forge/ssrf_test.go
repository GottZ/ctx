// H13c — negative probe for the claimed capability boundary "outgoing fetches
// NEVER reach private addresses", consumer 1 of 3: the forge sync client
// (design/04 §4.8 table row 3; github.go:46 builds the production transport).
//
// internal/ssrfguard has unit tests for IsDeniedIP and for the guarded dialer
// itself. What was missing is the probe that the guard actually HANGS in the
// client this package hands out: NewGitHubClient could regress to a plain
// &http.Client{} (or to http.DefaultTransport) and every existing test would
// stay green — github_test.go deliberately builds its clients with srv.Client()
// to reach its loopback fixtures.
//
// The probe drives the PUBLIC entry point (ListIssuesSince) against a LIVE
// loopback server that would answer 200 with a valid issue page, and asserts
// (a) the guard's marker error and (b) zero requests observed. The control
// half fetches the same target with a plain client and gets the 200 — so the
// refusal is provably the guard, not mere unreachability.
//
// Mutation that turns this RED (documented, test-local): swap the wired client
// for srv.Client() in the guarded half ⇒ the dial succeeds ⇒ hits == 1.
package forge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestProductionClientRefusesLoopbackDial(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("ETag", `"etag-ssrf"`)
		_, _ = w.Write([]byte(`[{"number":1,"title":"reachable","state":"open"}]`))
	}))
	defer srv.Close()

	repo := RepoRef{Owner: "o", Repo: "r", APIBase: srv.URL}

	// --- guarded half: the client this package hands to production ---
	c, ok := NewGitHubClient("tok").(*githubClient)
	if !ok {
		t.Fatalf("NewGitHubClient returned %T, want *githubClient", NewGitHubClient("tok"))
	}
	_, err := c.ListIssuesSince(context.Background(), repo, time.Time{}, "")
	if err == nil {
		t.Fatal("the production forge client reached a loopback api_base — ssrfguard is not wired into NewGitHubClient")
	}
	if !strings.Contains(err.Error(), "SSRF guard") {
		t.Fatalf("forge fetch failed for the wrong reason (want the ssrfguard marker): %v", err)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("the loopback target saw %d requests — the dial was not aborted before the bytes flowed", n)
	}

	// --- control half: the very same target IS reachable without the guard ---
	plain := &githubClient{token: "tok", http: srv.Client()}
	page, cerr := plain.ListIssuesSince(context.Background(), repo, time.Time{}, "")
	if cerr != nil {
		t.Fatalf("control fetch failed — the fixture is unreachable, so the probe above proves nothing: %v", cerr)
	}
	if len(page.Issues) != 1 {
		t.Fatalf("control fetch returned %d issues, want 1", len(page.Issues))
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("control hits = %d, want exactly 1 (the guarded half must have contributed none)", n)
	}
}
