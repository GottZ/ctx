package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestDigestTimeoutSaysWhatItCost — the review of #530 (note 4): a digest
// the call's --timeout cut said only that the request failed. The server
// ends the digest with its call, so the run wrote no topic map; the error
// says that and names --timeout.
func TestDigestTimeoutSaysWhatItCost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	defer srv.Close()
	c := NewClientWithTimeout(Config{BaseURL: srv.URL, Key: "k"}, 0, maxResponseSize)
	digest := digestCmd(func() (*Client, error) { return c, nil })
	digest.SetArgs([]string{"--timeout", "100ms"})
	digest.SetOut(io.Discard)
	digest.SetErr(io.Discard)
	done := make(chan error, 1)
	go func() { done <- digest.Execute() }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "--timeout") || !strings.Contains(err.Error(), "no topic map") {
			t.Errorf("error %v, want one that names --timeout and says no topic map was written", err)
		}
	case <-time.After(5 * time.Second):
		srv.CloseClientConnections()
		t.Fatal("the call did not end at its --timeout")
	}
}

// TestDigestTimeoutAfterTheAnswerBegan (issue #519, review of PR #553): the
// server answers a digest once its run has ended, so a call the --timeout
// cuts while it reads the answer came after the run — it must not say the
// run wrote no topic map.
func TestDigestTimeoutAfterTheAnswerBegan(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success":true,`))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	c := NewClientWithTimeout(Config{BaseURL: srv.URL, Key: "k"}, 0, maxResponseSize)
	digest := digestCmd(func() (*Client, error) { return c, nil })
	digest.SetArgs([]string{"--timeout", "100ms"})
	digest.SetOut(io.Discard)
	digest.SetErr(io.Discard)
	done := make(chan error, 1)
	go func() { done <- digest.Execute() }()
	select {
	case err := <-done:
		if err == nil || strings.Contains(err.Error(), "no topic map") || !strings.Contains(err.Error(), "run had ended") || !strings.Contains(err.Error(), "--timeout") {
			t.Errorf("error %v, want one that says the run had ended and names --timeout, not that no topic map was written", err)
		}
	case <-time.After(5 * time.Second):
		srv.CloseClientConnections()
		t.Fatal("the call did not end at its --timeout")
	}
}
