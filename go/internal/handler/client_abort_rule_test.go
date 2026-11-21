package handler

// Issue #417, the edges of the rule: what a gone client does NOT turn into an
// INFO line, and the response writers' own line.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/GottZ/ctx/internal/pgxdb"
	"github.com/GottZ/ctx/internal/store"
)

// TestClientGoneKeepsOtherFailuresError — the client is gone, but the failure
// a handler reports is no cancellation of the request (a real store error,
// or a call on a context detached from the request): the line stays ERROR.
func TestClientGoneKeepsOtherFailuresError(t *testing.T) {
	for _, tc := range []struct {
		name string
		log  func(w http.ResponseWriter, r *http.Request)
	}{
		{"failure that is no cancellation", func(_ http.ResponseWriter, r *http.Request) {
			logRequestError(r.Context(), abortProbeMsg, "error", errors.New("store: probe: relation does not exist"))
		}},
		{"cancellation reported on a detached context", func(_ http.ResponseWriter, r *http.Request) {
			logRequestError(context.WithoutCancel(r.Context()), abortProbeMsg, "error", fmt.Errorf("store: probe: %w", context.Canceled))
		}},
		{"writer form, failure that is no cancellation", func(w http.ResponseWriter, _ *http.Request) {
			logResponseError(w, abortProbeMsg, "error", errors.New("store: probe: relation does not exist"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := installRejectLogCapture(t)
			started := make(chan struct{})
			srv, done := abortServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(started)
				select {
				case <-r.Context().Done():
				case <-time.After(15 * time.Second):
				}
				tc.log(w, r)
				writeInternal(w)
			}), nil)
			ctx, cancel := context.WithCancel(context.Background())
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/manage", nil)
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
			<-errc
			waitDone(t, done)
			if got := statusOf(t, requestLogOf(t, logs)); got != clientClosedStatus {
				t.Errorf("request log status = %d, want %d (the client did go away)", got, clientClosedStatus)
			}
			if recs := logs.withMsg(abortProbeMsg); len(recs) != 1 || recs[0].level != slog.LevelError {
				t.Errorf("the line must stay ERROR: %+v", recs)
			}
		})
	}
}

// TestDeadlineOnTheRequestContextIsNoClientAbort — a deadline a stage in
// front of Logger puts on the request's own context is the server's: the
// request keeps the handler's status.
func TestDeadlineOnTheRequestContextIsNoClientAbort(t *testing.T) {
	logs := installRejectLogCapture(t)
	h := Logger(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		internalError(w, r.Context(), abortProbeMsg, fmt.Errorf("store: probe: %w", r.Context().Err()))
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/manage", nil).WithContext(ctx)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got := statusOf(t, requestLogOf(t, logs)); got != http.StatusInternalServerError {
		t.Errorf("request log status = %d, want 500", got)
	}
	if recs := logs.withMsg(abortProbeMsg); len(recs) != 1 || recs[0].level != slog.LevelError {
		t.Errorf("a deadline must stay an ERROR line: %+v", recs)
	}
}

// TestCustomCauseOnTheRequestContextIsNoClientAbort — a cancellation with a
// cause of its own is a stage's decision, not net/http's plain cancellation
// of a client that left.
func TestCustomCauseOnTheRequestContextIsNoClientAbort(t *testing.T) {
	logs := installRejectLogCapture(t)
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errors.New("probe: revoked"))
	req := httptest.NewRequest(http.MethodGet, "/api/manage", nil).WithContext(ctx)
	Logger(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		internalError(w, r.Context(), abortProbeMsg, fmt.Errorf("store: probe: %w", r.Context().Err()))
	})).ServeHTTP(httptest.NewRecorder(), req)
	if got := statusOf(t, requestLogOf(t, logs)); got != http.StatusInternalServerError {
		t.Errorf("request log status = %d, want 500", got)
	}
	if recs := logs.withMsg(abortProbeMsg); len(recs) != 1 || recs[0].level != slog.LevelError {
		t.Errorf("a cancellation with its own cause must stay an ERROR line: %+v", recs)
	}
}

// TestResponseWriteToAGoneClient — writeJSON's own line: a body the gone
// client can no longer take is INFO; a value JSON cannot encode and a body
// the status may not carry are server faults whatever the client does.
func TestResponseWriteToAGoneClient(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		data      any
		wantLevel slog.Level
	}{
		{"write to the closed connection", http.StatusOK, map[string]string{"blob": strings.Repeat("x", 8<<20)}, slog.LevelInfo},
		{"value JSON cannot encode", http.StatusOK, map[string]float64{"x": math.NaN()}, slog.LevelError},
		{"body on a status without one", http.StatusNotModified, map[string]string{"x": "y"}, slog.LevelError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := installRejectLogCapture(t)
			started := make(chan struct{})
			srv, done := abortServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(started)
				select {
				case <-r.Context().Done():
				case <-time.After(15 * time.Second):
				}
				time.Sleep(50 * time.Millisecond) // let the peer's close settle
				writeJSON(w, tc.status, tc.data)
			}), nil)
			ctx, cancel := context.WithCancel(context.Background())
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/search", nil)
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
			<-errc
			waitDone(t, done)
			recs := logs.withMsg("failed to encode JSON response")
			if len(recs) != 1 {
				t.Fatalf("want one encode line, got %+v", logs.all())
			}
			if recs[0].level != tc.wantLevel {
				t.Errorf("encode line level = %v, want %v (%+v)", recs[0].level, tc.wantLevel, recs[0].attrs)
			}
		})
	}
}

// TestStreamClosedByClientIsLogged499 — a stream (SSE, a keepalive query)
// commits its 200 at once and ends because its client closes it: the line
// says 499 and keeps the 200 the handler sent as handler_status.
func TestStreamClosedByClientIsLogged499(t *testing.T) {
	logs := installRejectLogCapture(t)
	started := make(chan struct{})
	srv, done := abortServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_ = http.NewResponseController(w).Flush()
		close(started)
		select {
		case <-r.Context().Done():
		case <-time.After(15 * time.Second):
		}
	}), nil)
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	cancel()
	resp.Body.Close()
	waitDone(t, done)
	line := requestLogOf(t, logs)
	if got := statusOf(t, line); got != clientClosedStatus {
		t.Errorf("request log status = %d, want %d", got, clientClosedStatus)
	}
	if hs, _ := line.attrs["handler_status"].(int64); hs != http.StatusOK {
		t.Errorf("handler_status = %v, want 200", line.attrs["handler_status"])
	}
}

// TestSlowRequestBodyIsNoClientAbort — a body that arrives slower than
// http.Server.ReadTimeout fails its read with a timeout, and net/http cancels
// the request context for it as for a client that left. The client is still
// there and gets the handler's answer: the line keeps that status.
func TestSlowRequestBodyIsNoClientAbort(t *testing.T) {
	logs := installRejectLogCapture(t)
	readErr := make(chan error, 1)
	srv, done := abortServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		readErr <- err
		if r.Context().Err() == nil {
			t.Errorf("precondition: the timed-out body read did not end the request context (read err %v)", err)
		}
		logRequestError(r.Context(), abortProbeMsg, "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "invalid request body"})
	}), func(s *http.Server) { s.ReadTimeout = 300 * time.Millisecond })

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "POST /api/store HTTP/1.1\r\nHost: probe\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{\"ca"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("the waiting client got no answer: %v", err)
	}
	resp.Body.Close()
	waitDone(t, done)
	if err := <-readErr; !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("precondition: body read error = %v, want the read deadline", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("client got %d, want 400", resp.StatusCode)
	}
	if got := statusOf(t, requestLogOf(t, logs)); got != http.StatusBadRequest {
		t.Errorf("request log status = %d, want 400 (the server's read deadline, the client still waited)", got)
	}
}

// TestFlushPastTheWriteDeadlineIsNoClientAbort — a stream's heartbeat: a
// small write stays in the buffer and only the flush meets the connection,
// past the server's write deadline. That flush ends the request context; it
// stays the server's deadline.
func TestFlushPastTheWriteDeadlineIsNoClientAbort(t *testing.T) {
	logs := installRejectLogCapture(t)
	srv, done := abortServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond) // past the server's WriteTimeout
		if _, err := w.Write([]byte(" ")); err != nil {
			t.Errorf("precondition: the one-byte write reached the connection: %v", err)
		}
		if err := http.NewResponseController(w).Flush(); err == nil {
			t.Error("precondition: the flush past the write deadline succeeded")
		}
		internalError(w, r.Context(), abortProbeMsg, fmt.Errorf("store: probe: %w", r.Context().Err()))
	}), func(s *http.Server) { s.WriteTimeout = 50 * time.Millisecond })
	resp, err := http.Get(srv.URL + "/api/query")
	if err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	waitDone(t, done)
	if got := statusOf(t, requestLogOf(t, logs)); got == clientClosedStatus {
		t.Error("request log status = 499 for a flush that failed on the server's write deadline")
	}
	if recs := logs.withMsg(abortProbeMsg); len(recs) != 1 || recs[0].level != slog.LevelError {
		t.Errorf("the server's write deadline must stay an ERROR line: %+v", recs)
	}
}

// TestWriteDeadlineAfterTheClientLeftStaysAbort — the client left first; a
// write that fails on the write deadline afterwards does not turn the
// request into the server's fault.
func TestWriteDeadlineAfterTheClientLeftStaysAbort(t *testing.T) {
	logs := installRejectLogCapture(t)
	started := make(chan struct{})
	srv, done := abortServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
		case <-time.After(15 * time.Second):
		}
		time.Sleep(150 * time.Millisecond) // past the server's WriteTimeout
		_, _ = w.Write([]byte(strings.Repeat("x", 64<<10)))
		_ = http.NewResponseController(w).Flush()
		internalError(w, r.Context(), abortProbeMsg, fmt.Errorf("store: probe: %w", r.Context().Err()))
	}), func(s *http.Server) { s.WriteTimeout = 100 * time.Millisecond })
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/manage", nil)
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
	<-errc
	waitDone(t, done)
	if got := statusOf(t, requestLogOf(t, logs)); got != clientClosedStatus {
		t.Errorf("request log status = %d, want %d (the client left before the deadline)", got, clientClosedStatus)
	}
	if errs := errorLines(logs); len(errs) != 0 {
		t.Errorf("a client that gave up produced ERROR lines: %+v", errs)
	}
}

// outcomeTx / outcomeDB drive a real pgxdb bracket to the commit.
type outcomeTx struct {
	pgx.Tx
	commitErr error
}

func (t *outcomeTx) Commit(context.Context) error   { return t.commitErr }
func (t *outcomeTx) Rollback(context.Context) error { return nil }

type outcomeDB struct{ tx *outcomeTx }

func (d outcomeDB) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) { return d.tx, nil }

type safeToRetryErr struct{}

func (safeToRetryErr) Error() string     { return "probe: commit never sent" }
func (safeToRetryErr) SafeToRetry() bool { return true }

// connClosedErr is the shape of pgx's lock error on a closed connection:
// safe to retry, pgconn.ErrConnClosed inside — what pgx v5 answers for a
// COMMIT whose connection broke while it waited (issue #483).
type connClosedErr struct{}

func (connClosedErr) Error() string     { return "conn closed" }
func (connClosedErr) SafeToRetry() bool { return true }
func (connClosedErr) Unwrap() error     { return pgconn.ErrConnClosed }

// TestWriteOutcome — what a failed labeled write bracket states.
func TestWriteOutcome(t *testing.T) {
	bracket := func(fnErr, commitErr error) error {
		return pgxdb.Write(context.Background(), outcomeDB{tx: &outcomeTx{commitErr: commitErr}}, pgxdb.At("store: upsert block"),
			func(pgx.Tx) error { return fnErr })
	}
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"cancelled inside the transaction", bracket(fmt.Errorf("probe: %w", context.Canceled), nil), writeRolledBack},
		{"COMMIT cut off on the wire", bracket(nil, fmt.Errorf("probe: timeout: %w", context.Canceled)), writeUnknown},
		{"COMMIT answered as rollback", bracket(nil, pgx.ErrTxCommitRollback), writeRolledBack},
		{"COMMIT refused by the database", bracket(nil, &pgconn.PgError{Code: "40001"}), writeRolledBack},
		{"COMMIT never sent", bracket(nil, safeToRetryErr{}), writeRolledBack},
		{"COMMIT whose connection broke while it waited", bracket(nil, connClosedErr{}), writeUnknown},
		{"connection closed inside the transaction", bracket(connClosedErr{}, nil), writeRolledBack},
	} {
		if got := writeOutcome(tc.err); got != tc.want {
			t.Errorf("%s: writeOutcome(%v) = %q, want %q", tc.name, tc.err, got, tc.want)
		}
	}
}

// TestPurgeOutcome — a purge that failed after earlier batches removed
// something says committed; one that removed nothing says what its failing
// statement left behind (issue #483).
func TestPurgeOutcome(t *testing.T) {
	cut := fmt.Errorf("store: purge blobs: %w", errors.New("read tcp: connection reset by peer"))
	for _, tc := range []struct {
		name string
		done *store.PurgeDeleted
		err  error
		want string
	}{
		{"blocks removed before the failure", &store.PurgeDeleted{Blocks: 2}, cut, writeCommitted},
		{"only blobs removed before the failure", &store.PurgeDeleted{Blobs: 1}, cut, writeCommitted},
		{"nothing removed, the statement was cut", &store.PurgeDeleted{}, cut, writeUnknown},
		{"refused before the first delete", nil, &pgconn.PgError{Code: "57014"}, writeRolledBack},
	} {
		if got := purgeOutcome(tc.done, tc.err); got != tc.want {
			t.Errorf("%s: purgeOutcome = %q, want %q", tc.name, got, tc.want)
		}
	}
}
