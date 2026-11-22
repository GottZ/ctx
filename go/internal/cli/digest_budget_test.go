// Issue #505: `ctx digest` ran under the main client's 120 s budget, and since
// #490 the digest ends with its client — a digest that took longer never
// finished from the CLI. Like `ctx manage purge`, the digest call now waits
// for the answer (digestCallBudget); every other call keeps the client's
// budget, and the shared client keeps it too.
//
//	go test -short ./internal/cli/ -run TestDigestWaitsPastTheClientBudget -count=1 -v
package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDigestWaitsPastTheClientBudget(t *testing.T) {
	const answerAfter = 400 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-time.After(answerAfter):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"action":"digest","scope":"private","title":"topic-map-private","blockCount":3}`))
	}))
	defer srv.Close()

	// A client whose budget is a quarter of the server's answer time stands in
	// for the 120 s main client and a digest that runs longer.
	short := NewClientWithTimeout(Config{BaseURL: srv.URL, Key: "k"}, answerAfter/4, maxResponseSize)
	get := func() (*Client, error) { return short, nil }
	quiet := func(cmd interface {
		SetArgs([]string)
		SetOut(io.Writer)
		SetErr(io.Writer)
	}, args ...string) {
		cmd.SetArgs(args)
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
	}

	stats := statsCmd(get)
	quiet(stats)
	if err := stats.Execute(); err == nil {
		t.Fatalf("stats answered within a budget shorter than the server's answer time — the fixture proves nothing")
	}
	digest := digestCmd(get)
	quiet(digest)
	if err := digest.Execute(); err != nil {
		t.Errorf("digest: %v — the call gave up before the server answered", err)
	}
	if got := short.HTTPClient.Timeout; got != answerAfter/4 {
		t.Errorf("the digest changed the shared client's budget to %v, want it untouched", got)
	}
}
