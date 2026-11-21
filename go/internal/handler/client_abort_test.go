package handler

// Issue #417: a request the client gave up on is no server fault. The request
// log line carries 499 (the status nginx logs for it; nothing reaches the
// client), the handler's own error line stays below ERROR — and everything
// the server ends itself (its own deadline, a cancelled context of its own,
// a failure on a live request, a shutdown) stays an ERROR with the status the
// handler wrote.
//
// These tests drive the production middleware (RequestID, Logger) on a real
// httptest server whose client gives up, and on a request whose context is
// already cancelled when it reaches the handler.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// clientClosedStatus is the status of a request the client closed, written
// as the number on purpose: the log's readers match the number, not a Go name.
const clientClosedStatus = 499

// abortProbeMsg is the error line the probe handlers write through the
// package's internal-error writer.
const abortProbeMsg = "probe: work failed"

// requestLogOf returns the one "http request" line of the captured records.
func requestLogOf(t *testing.T, c *rejectLogCapture) rejectLogRecord {
	t.Helper()
	recs := c.withMsg("http request")
	if len(recs) != 1 {
		t.Fatalf("want exactly one request log line, got %d: %+v", len(recs), c.all())
	}
	return recs[0]
}

// errorLines returns every captured record at ERROR or above.
func errorLines(c *rejectLogCapture) []rejectLogRecord {
	var out []rejectLogRecord
	for _, r := range c.all() {
		if r.level >= slog.LevelError {
			out = append(out, r)
		}
	}
	return out
}

// statusOf reads the status attribute of a request log line as an int.
func statusOf(t *testing.T, r rejectLogRecord) int {
	t.Helper()
	switch v := r.attrs["status"].(type) {
	case int64:
		return int(v)
	case int:
		return v
	}
	t.Fatalf("request log line has no int status: %+v", r.attrs)
	return 0
}

// abortServer serves h behind the production RequestID and Logger stages and
// reports through done when the request log line is written (Logger
// returned). cfg may adjust the server before it starts.
func abortServer(t *testing.T, h http.Handler, cfg func(*http.Server)) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	done := make(chan struct{}, 4)
	chain := RequestID(Logger(h))
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { done <- struct{}{} }()
		chain.ServeHTTP(w, r)
	}))
	if cfg != nil {
		cfg(srv.Config)
	}
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, done
}

func waitDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the request never finished on the server")
	}
}

// waitingHandler reads the body like every route does (net/http watches the
// connection for a client that leaves only once the body is consumed), then
// blocks until the request context ends and fails the way a handler fails
// whose store call ran on that context: the call's error wraps the
// context's error, and the handler answers through internalError. The wait
// is bounded so a broken premise fails the test instead of hanging it.
func waitingHandler(started chan<- struct{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		select {
		case <-r.Context().Done():
		case <-time.After(15 * time.Second):
		}
		internalError(w, r.Context(), abortProbeMsg, fmt.Errorf("store: probe: %w", r.Context().Err()))
	}
}

// TestClientAbortIsLogged499 — the client gives up while the handler waits on
// its request context: the request log says 499 and nothing is an ERROR.
func TestClientAbortIsLogged499(t *testing.T) {
	logs := installRejectLogCapture(t)
	started := make(chan struct{})
	srv, done := abortServer(t, waitingHandler(started), nil)

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/api/manage", strings.NewReader(`{"action":"stats"}`))
	errc := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
		errc <- err
	}()
	<-started
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("client: want its own cancellation, got %v", err)
	}
	waitDone(t, done)

	line := requestLogOf(t, logs)
	if got := statusOf(t, line); got != clientClosedStatus {
		t.Errorf("request log status = %d, want %d (the client closed the request)", got, clientClosedStatus)
	}
	if errs := errorLines(logs); len(errs) != 0 {
		t.Errorf("a client that gave up produced ERROR lines: %+v", errs)
	}
	if recs := logs.withMsg(abortProbeMsg); len(recs) != 1 || recs[0].level >= slog.LevelError {
		t.Errorf("the handler's line must stay, below ERROR: %+v", recs)
	}
}

// TestClientAbortPreCancelledContext — the same verdict for a request whose
// context is already cancelled when it reaches the handler.
func TestClientAbortPreCancelledContext(t *testing.T) {
	logs := installRejectLogCapture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/api/manage", strings.NewReader(`{"action":"stats"}`)).WithContext(ctx)
	rec := httptest.NewRecorder()
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		internalError(w, r.Context(), abortProbeMsg, fmt.Errorf("store: probe: %w", r.Context().Err()))
	})
	RequestID(Logger(h)).ServeHTTP(rec, req)

	if got := statusOf(t, requestLogOf(t, logs)); got != clientClosedStatus {
		t.Errorf("request log status = %d, want %d", got, clientClosedStatus)
	}
	if errs := errorLines(logs); len(errs) != 0 {
		t.Errorf("a cancelled request produced ERROR lines: %+v", errs)
	}
}

// TestServerFailureOnLiveRequestStaysError — a failure on a request whose
// client still waits keeps its ERROR line and its 500, also when the error is
// a cancellation of a context the server owns (not the request's).
func TestServerFailureOnLiveRequestStaysError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  func() error
	}{
		{"plain failure", func() error { return errors.New("store: probe: relation does not exist") }},
		{"cancelled server-owned context", func() error {
			own, cancel := context.WithCancel(context.Background())
			cancel()
			return fmt.Errorf("store: probe: %w", own.Err())
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := installRejectLogCapture(t)
			srv, done := abortServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				internalError(w, r.Context(), abortProbeMsg, tc.err())
			}), nil)
			resp, err := http.Post(srv.URL+"/api/manage", "application/json", strings.NewReader(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			waitDone(t, done)
			if resp.StatusCode != http.StatusInternalServerError {
				t.Errorf("client got %d, want 500", resp.StatusCode)
			}
			if got := statusOf(t, requestLogOf(t, logs)); got != http.StatusInternalServerError {
				t.Errorf("request log status = %d, want 500", got)
			}
			if recs := logs.withMsg(abortProbeMsg); len(recs) != 1 || recs[0].level != slog.LevelError {
				t.Errorf("the failure must stay one ERROR line: %+v", recs)
			}
		})
	}
}

// TestServerTimeoutStaysError — deadlines the server sets itself are no
// client abort: a handler's own WithTimeout child, http.TimeoutHandler, and
// the connection's write deadline (http.Server.WriteTimeout), which cancels
// the request context through a failed write.
func TestServerTimeoutStaysError(t *testing.T) {
	t.Run("handler deadline child", func(t *testing.T) {
		logs := installRejectLogCapture(t)
		srv, done := abortServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), 20*time.Millisecond)
			defer cancel()
			<-ctx.Done()
			internalError(w, ctx, abortProbeMsg, fmt.Errorf("store: probe: %w", ctx.Err()))
		}), nil)
		resp, err := http.Get(srv.URL + "/api/manage")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		waitDone(t, done)
		if got := statusOf(t, requestLogOf(t, logs)); got != http.StatusInternalServerError {
			t.Errorf("request log status = %d, want 500", got)
		}
		if recs := logs.withMsg(abortProbeMsg); len(recs) != 1 || recs[0].level != slog.LevelError {
			t.Errorf("a server deadline must stay an ERROR line: %+v", recs)
		}
	})

	t.Run("http.TimeoutHandler", func(t *testing.T) {
		logs := installRejectLogCapture(t)
		inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
			internalError(w, r.Context(), abortProbeMsg, fmt.Errorf("store: probe: %w", r.Context().Err()))
		})
		var wg sync.WaitGroup
		wg.Add(1)
		srv, done := abortServer(t, http.TimeoutHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer wg.Done()
			inner(w, r)
		}), 20*time.Millisecond, "timeout"), nil)
		resp, err := http.Get(srv.URL + "/api/manage")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		waitDone(t, done)
		wg.Wait()
		if got := statusOf(t, requestLogOf(t, logs)); got != http.StatusServiceUnavailable {
			t.Errorf("request log status = %d, want 503 (TimeoutHandler's own answer)", got)
		}
		if recs := logs.withMsg(abortProbeMsg); len(recs) != 1 || recs[0].level != slog.LevelError {
			t.Errorf("a server deadline must stay an ERROR line: %+v", recs)
		}
	})

	t.Run("server write deadline", func(t *testing.T) {
		logs := installRejectLogCapture(t)
		flushed := make(chan error, 1)
		srv, done := abortServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(150 * time.Millisecond) // past the server's WriteTimeout
			_, _ = w.Write([]byte(strings.Repeat("x", 64<<10)))
			ferr := http.NewResponseController(w).Flush()
			flushed <- ferr
			if r.Context().Err() == nil {
				t.Errorf("precondition: the failed write did not end the request context (flush err %v)", ferr)
			}
			internalError(w, r.Context(), abortProbeMsg, fmt.Errorf("store: probe: %w", r.Context().Err()))
		}), func(s *http.Server) { s.WriteTimeout = 50 * time.Millisecond })
		resp, err := http.Get(srv.URL + "/api/manage")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		waitDone(t, done)
		if ferr := <-flushed; ferr == nil {
			t.Fatal("precondition: the flush past the write deadline succeeded")
		}
		if got := statusOf(t, requestLogOf(t, logs)); got == clientClosedStatus {
			t.Errorf("request log status = 499 for the server's own write deadline")
		}
		if recs := logs.withMsg(abortProbeMsg); len(recs) != 1 || recs[0].level != slog.LevelError {
			t.Errorf("the server's write deadline must stay an ERROR line: %+v", recs)
		}
	})
}

// TestShutdownIsNoClientAbort — http.Server.Shutdown does not end the
// context of a request in flight, so a handler that fails during the
// teardown (its pool closes under it) keeps ERROR and 500.
func TestShutdownIsNoClientAbort(t *testing.T) {
	logs := installRejectLogCapture(t)
	started, release := make(chan struct{}), make(chan struct{})
	srv, done := abortServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		if err := r.Context().Err(); err != nil {
			t.Errorf("Shutdown ended the request context: %v", err)
		}
		internalError(w, r.Context(), abortProbeMsg, errors.New("store: probe: closed pool"))
	}), nil)
	got := make(chan int, 1)
	go func() {
		resp, err := http.Get(srv.URL + "/api/manage")
		if err != nil {
			got <- 0
			return
		}
		resp.Body.Close()
		got <- resp.StatusCode
	}()
	<-started
	sctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_ = srv.Config.Shutdown(sctx) // returns at its deadline: the request is still active
	close(release)
	waitDone(t, done)
	if code := <-got; code != http.StatusInternalServerError {
		t.Errorf("client got %d, want 500", code)
	}
	if st := statusOf(t, requestLogOf(t, logs)); st != http.StatusInternalServerError {
		t.Errorf("request log status = %d, want 500", st)
	}
	if recs := logs.withMsg(abortProbeMsg); len(recs) != 1 || recs[0].level != slog.LevelError {
		t.Errorf("a failure during shutdown must stay an ERROR line: %+v", recs)
	}
}
