package handler

// Issue #453: the 499 line of a request whose client left names the status
// the handler wrote as handler_status. A handler that wrote nothing — a route
// that answers a gone client with nothing, the MCP SDK when the client of a
// tools/call left — had the record's start value 200 there, the same line as
// a handler that answered 200. It now says 0: nothing was written.

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// abortWith serves h behind RequestID and Logger and gives up on one request
// (abortClient); it returns when the request log line is written.
func abortWith(t *testing.T, h http.HandlerFunc, started <-chan struct{}) {
	t.Helper()
	srv, done := abortServer(t, h, nil)
	abortClient(t, srv.URL+"/api/probe", started)
	waitDone(t, done)
}

// abortClient sends one request to url and cancels its client once started
// is closed. A stream may have sent its headers by then: the client then
// closes the body it holds.
func abortClient(t *testing.T, url string, started <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(`{}`))
	type answer struct {
		resp *http.Response
		err  error
	}
	got := make(chan answer, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		got <- answer{resp, err}
	}()
	<-started
	cancel()
	if a := <-got; a.err == nil {
		a.resp.Body.Close()
	}
}

// waitForTheClient reads the body, reports through started and blocks until
// the client left (bounded, so a broken premise fails instead of hanging).
func waitForTheClient(r *http.Request, started chan<- struct{}) {
	_, _ = io.Copy(io.Discard, r.Body)
	close(started)
	select {
	case <-r.Context().Done():
	case <-time.After(15 * time.Second):
	}
}

func TestRequestLogHandlerStatusSaysWhatTheHandlerWrote(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(w http.ResponseWriter)
		want  int64
	}{
		{"nothing written", func(http.ResponseWriter) {}, 0},
		{"a status written", func(w http.ResponseWriter) { w.WriteHeader(http.StatusAccepted) }, http.StatusAccepted},
		{"a body without a status (the implicit 200)", func(w http.ResponseWriter) { _, _ = w.Write([]byte("{}")) }, http.StatusOK},
		{"a flush without a status (the implicit 200)", func(w http.ResponseWriter) { _ = http.NewResponseController(w).Flush() }, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := installRejectLogCapture(t)
			started := make(chan struct{})
			abortWith(t, func(w http.ResponseWriter, r *http.Request) {
				waitForTheClient(r, started)
				tc.write(w)
			}, started)
			line := requestLogOf(t, logs)
			if got := statusOf(t, line); got != clientClosedStatus {
				t.Fatalf("status = %d, want %d", got, clientClosedStatus)
			}
			if hs, ok := line.attrs["handler_status"].(int64); !ok || hs != tc.want {
				t.Errorf("handler_status = %v, want %d", line.attrs["handler_status"], tc.want)
			}
		})
	}
}

// TestRequestLogHandlerStatusBeforeTheClientLeft — a status the handler wrote
// before its client left (a stream's 200) stays the handler_status; the
// handler writes nothing more after.
func TestRequestLogHandlerStatusBeforeTheClientLeft(t *testing.T) {
	logs := installRejectLogCapture(t)
	started := make(chan struct{})
	abortWith(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = http.NewResponseController(w).Flush()
		waitForTheClient(r, started)
	}, started)
	if hs, _ := requestLogOf(t, logs).attrs["handler_status"].(int64); hs != http.StatusOK {
		t.Errorf("handler_status = %v, want 200", hs)
	}
}

// TestRequestLogLiveRequestWithoutAWriteStays200 — a live request whose
// handler wrote nothing got net/http's implicit 200: its line is unchanged.
func TestRequestLogLiveRequestWithoutAWriteStays200(t *testing.T) {
	logs := installRejectLogCapture(t)
	srv, done := abortServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), nil)
	resp, err := http.Get(srv.URL + "/api/probe")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	waitDone(t, done)
	line := requestLogOf(t, logs)
	if got := statusOf(t, line); got != http.StatusOK || resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d (client got %d), want 200", got, resp.StatusCode)
	}
	if _, ok := line.attrs["handler_status"]; ok {
		t.Errorf("a live request carries handler_status: %+v", line.attrs)
	}
}
