// Issue #555, point 3: `ctx tenant delete` waits for the server's answer
// without the client's 120 s budget — the delete runs to its end on the
// server whatever the client does — and when the request ends before the
// answer, the error says that the delete goes on and the tenant is locked out
// meanwhile.
//
//	go test -short ./internal/cli/ -run TestTenantDelete -count=1 -v
package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func runTenantDelete(get func() (*Client, error), id string) error {
	cmd := tenantDeleteCmd(get)
	cmd.SetArgs([]string{id})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	return cmd.Execute()
}

func TestTenantDeleteWaitsPastTheClientBudget(t *testing.T) {
	const answerAfter = 400 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-time.After(answerAfter):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"deleted":"01a2105d-0000-7000-8000-000000000555"}`))
	}))
	defer srv.Close()
	// A budget a quarter of the server's answer time stands in for the 120 s
	// main client and a delete of minutes.
	short := NewClientWithTimeout(Config{BaseURL: srv.URL, Key: "k"}, answerAfter/4, maxResponseSize)
	if _, err := short.Post("manage", map[string]any{"action": "tenant-list"}); err == nil {
		t.Fatal("a call answered within a budget shorter than the server's answer time — the fixture proves nothing")
	}
	if err := runTenantDelete(func() (*Client, error) { return short, nil }, "01a2105d-0000-7000-8000-000000000555"); err != nil {
		t.Errorf("tenant delete: %v — the call gave up before the server answered", err)
	}
	if got := short.HTTPClient.Timeout; got != answerAfter/4 {
		t.Errorf("the delete changed the shared client's budget to %v, want it untouched", got)
	}
}

func TestTenantDeleteSaysTheDeleteGoesOnWhenTheRequestEnds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The connection ends before an answer: a proxy that timed out, a
		// network that dropped it.
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("no hijacker")
			return
		}
		c, _, err := hj.Hijack()
		if err == nil {
			_ = c.Close()
		}
	}))
	defer srv.Close()
	c := NewClientWithTimeout(Config{BaseURL: srv.URL, Key: "k"}, 5*time.Second, maxResponseSize)
	err := runTenantDelete(func() (*Client, error) { return c, nil }, "01a2105d-0000-7000-8000-000000000555")
	if err == nil {
		t.Fatal("a request that ended without an answer reported success")
	}
	t.Logf("error: %v", err)
	for _, want := range []string{"the delete goes on at the server", "offboarding", "run the delete again"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error of a request that ended before the delete's answer does not say %q: %v", want, err)
		}
	}
}
