// Issue #505: `ctx digest` ran under the main client's 120 s budget, and since
// #490 the digest ends with its client — a digest that took longer never
// finished from the CLI. The digest call now has a budget of its own
// (digestCallBudget, 30 minutes): far above the client's, and it ends — a
// digest that hangs does not hold the CLI forever. Every other call keeps the
// client's budget, and the shared client keeps it too.
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

// TestDigestHasABudgetOfItsOwn — the digest call is not unbounded: a digest
// that hangs (on a lock, say) would hold the CLI forever. Its budget is
// generous — far above what a digest takes at the target scale — and ends.
func TestDigestHasABudgetOfItsOwn(t *testing.T) {
	if digestCallBudget < 30*time.Minute {
		t.Errorf("digestCallBudget = %v, want 30 minutes (0 waits without end)", digestCallBudget)
	}
}

// TestDigestEndsAtItsBudget — a digest whose answer does not come: the call
// gives up at digestCallBudget, and the server sees its request end (the
// digest ends with its client, issue #490).
func TestDigestEndsAtItsBudget(t *testing.T) {
	prev := digestCallBudget
	digestCallBudget = 200 * time.Millisecond
	t.Cleanup(func() { digestCallBudget = prev })
	ended := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
		close(ended)
	}))
	defer srv.Close()
	c := NewClientWithTimeout(Config{BaseURL: srv.URL, Key: "k"}, 0, maxResponseSize)
	digest := digestCmd(func() (*Client, error) { return c, nil })
	digest.SetOut(io.Discard)
	digest.SetErr(io.Discard)
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- digest.Execute() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("digest succeeded without an answer")
		}
		t.Logf("the digest gave up after %v: %v", time.Since(start).Round(time.Millisecond), err)
	case <-time.After(5 * time.Second):
		srv.CloseClientConnections() // so the deferred Close does not wait for the call
		t.Fatal("the digest call is still waiting 5 s into a 200 ms budget")
	}
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Error("the server did not see the request end")
	}
}
