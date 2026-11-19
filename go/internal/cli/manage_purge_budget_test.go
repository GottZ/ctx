// Issue #277: a confirmed `ctx manage purge` runs to its end on the server
// whatever the client does (the run is detached from the request), and at the
// 1M-block scale it takes minutes. With the main client's 120 s budget the
// CLI gave up and printed a timeout while the run went on; its deleted counts
// never reached the caller. The purge call now waits for the answer; every
// other manage call keeps the client's budget.
//
//	go test -short ./internal/cli/ -run TestManagePurgeWaitsPastTheClientBudget -count=1 -v
package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestManagePurgeWaitsPastTheClientBudget(t *testing.T) {
	const answerAfter = 400 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-time.After(answerAfter):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"action":"purge","success":true,"dry_run":false,"deleted":{"blocks":3,"dream_links":0,"blobs":0}}`))
	}))
	defer srv.Close()

	// A client whose budget is a quarter of the server's answer time stands in
	// for the 120 s main client and a run of minutes.
	short := NewClientWithTimeout(Config{BaseURL: srv.URL, Key: "k"}, answerAfter/4, maxResponseSize)
	get := func() (*Client, error) { return short, nil }

	run := func(args ...string) error {
		cmd := manageCmd(get)
		cmd.SetArgs(args)
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		return cmd.Execute()
	}

	if err := run("stats"); err == nil {
		t.Fatalf("manage stats answered within a budget shorter than the server's answer time — the fixture proves nothing")
	}
	if err := run("purge", "", `{"scope":"acme:import","confirm_blocks":3}`); err != nil {
		t.Errorf("manage purge: %v — the call gave up before the server answered", err)
	}
	if got := short.HTTPClient.Timeout; got != answerAfter/4 {
		t.Errorf("the purge changed the shared client's budget to %v, want it untouched", got)
	}
}
