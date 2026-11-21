package handler

// Issue #453 point 2: a client that gave up while its request waited for an
// admission slot left a server that had no room for it — an operator signal,
// not a quiet INFO. The handler's line and the request log say so at WARN
// with waited_for; every client-gone line carries the request's duration. A
// client that left while nothing waited stays INFO.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/dispatch"
)

const waitProbeMsg = "probe: admission failed"

// admissionHandler acquires an interactive slot on origin the way the wire
// walks do (the error wrapped in a *dispatch.AdmissionError) and reports the
// failure through the package's internal-error writer. before runs between
// reading the body and the acquire.
func admissionHandler(d *dispatch.Dispatcher, origin string, before func(r *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		before(r)
		ctx := withAuthResult(r.Context(), testAuthResult())
		lease, _, err := d.Acquire(ctx, dispatch.Request{Target: dispatch.Target{Origin: origin}, Class: dispatch.ClassInteractive, Role: "probe"})
		if err == nil {
			lease.Release()
			writeJSON(w, http.StatusOK, map[string]any{"success": true})
			return
		}
		internalError(w, ctx, waitProbeMsg, fmt.Errorf("probe: %w", &dispatch.AdmissionError{Err: err, Host: origin}))
	}
}

// cancelWhen sends a request to url and cancels it once ready returns.
func cancelWhen(t *testing.T, url string, ready func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(`{}`))
	errc := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
		errc <- err
	}()
	ready()
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("client: want its own cancellation, got %v", err)
	}
}

// TestClientAbortWhileWaitingForAdmissionIsWarn — the only slot is held; the
// request queues; its client leaves.
func TestClientAbortWhileWaitingForAdmissionIsWarn(t *testing.T) {
	installRequestPrincipalHook(t)
	d := newGateDispatcher(t)
	logs := installRejectLogCapture(t)
	srv, done := abortServer(t, admissionHandler(d, gateHeldOrigin, func(*http.Request) {}), nil)

	begin := time.Now()
	cancelWhen(t, srv.URL+"/api/query", func() {
		deadline := time.Now().Add(10 * time.Second)
		for targetSnap(d, gateHeldOrigin).Interactive.Waiting != 1 {
			if time.Now().After(deadline) {
				t.Fatal("the request never queued")
			}
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(20 * time.Millisecond)
	})
	waitDone(t, done)
	waited := time.Since(begin).Milliseconds()

	if errs := errorLines(logs); len(errs) != 0 {
		t.Errorf("a client that gave up produced ERROR lines: %+v", errs)
	}
	recs := logs.withMsg(waitProbeMsg)
	if len(recs) != 1 {
		t.Fatalf("want one handler line, got %+v", logs.all())
	}
	line := recs[0]
	if line.level != slog.LevelWarn || line.attrs["client_gone"] != true || line.attrs["waited_for"] != "admission" {
		t.Errorf("handler line = %+v, want WARN with client_gone=true and waited_for=admission", line)
	}
	if ms, ok := line.attrs["duration_ms"].(int64); !ok || ms < 20 || ms > waited {
		t.Errorf("handler line duration_ms = %v, want the request's duration (>= 20 ms, <= %d)", line.attrs["duration_ms"], waited)
	}
	req := requestLogOf(t, logs)
	if statusOf(t, req) != clientClosedStatus || req.level != slog.LevelWarn || req.attrs["waited_for"] != "admission" {
		t.Errorf("request log = %+v, want 499 at WARN with waited_for=admission", req)
	}
}

// TestClientAbortWithoutAWaitStaysInfo — the client left before the request
// reached the admission door: nothing waited, the lines stay INFO, without
// waited_for, and still carry the duration.
func TestClientAbortWithoutAWaitStaysInfo(t *testing.T) {
	installRequestPrincipalHook(t)
	d := newGateDispatcher(t)
	logs := installRejectLogCapture(t)
	started := make(chan struct{})
	srv, done := abortServer(t, admissionHandler(d, gateFreeOrigin, func(r *http.Request) {
		close(started)
		<-r.Context().Done()
	}), nil)

	cancelWhen(t, srv.URL+"/api/query", func() { <-started })
	waitDone(t, done)

	if errs := errorLines(logs); len(errs) != 0 {
		t.Errorf("a client that gave up produced ERROR lines: %+v", errs)
	}
	recs := logs.withMsg(waitProbeMsg)
	if len(recs) != 1 {
		t.Fatalf("want one handler line, got %+v", logs.all())
	}
	line := recs[0]
	if line.level != slog.LevelInfo || line.attrs["client_gone"] != true || line.attrs["waited_for"] != nil {
		t.Errorf("handler line = %+v, want INFO with client_gone=true and no waited_for", line)
	}
	if _, ok := line.attrs["duration_ms"].(int64); !ok {
		t.Errorf("handler line has no duration_ms: %+v", line.attrs)
	}
	req := requestLogOf(t, logs)
	if statusOf(t, req) != clientClosedStatus || req.level != slog.LevelInfo || req.attrs["waited_for"] != nil {
		t.Errorf("request log = %+v, want 499 at INFO without waited_for", req)
	}
}
