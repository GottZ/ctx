package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/GottZ/ctx/internal/pgxdb"
)

// StatusClientClosedRequest is the status the request log gives a request
// whose client closed the connection before the handler was done (issue
// #417) — nginx's 499 for the same case. It is a log status only: nothing
// reaches a client that is gone, and no handler writes it.
const StatusClientClosedRequest = 499

// requestLogKey carries Logger's *responseWriter through the request context,
// so a handler's error line can ask whether the request's client is gone.
type requestLogKey struct{}

// loggedRequest returns Logger's record of the request ctx belongs to, nil
// for a context that never passed Logger.
func loggedRequest(ctx context.Context) *responseWriter {
	if ctx == nil {
		return nil
	}
	rw, _ := ctx.Value(requestLogKey{}).(*responseWriter)
	return rw
}

// loggedResponse walks a handler's writer chain down to Logger's record, for
// the writers that have no context at hand (writeJSON and the store-error
// writers). A writer in between that does not unwrap ends the walk with nil.
func loggedResponse(w http.ResponseWriter) *responseWriter {
	for w != nil {
		if rw, ok := w.(*responseWriter); ok {
			return rw
		}
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return nil
		}
		w = u.Unwrap()
	}
	return nil
}

// clientGaveUp is the one rule (issue #417): a failure is the client's
// leaving, not a server fault, when the request's own context ended because
// its client went away (responseWriter.clientGone) AND the failure is that
// cancellation (it wraps context.Canceled). Anything else — a failure that
// is no cancellation, a deadline, a cancellation while the client still
// waits, a request outside Logger — stays a fault.
func clientGaveUp(rw *responseWriter, err error) bool {
	return rw != nil && err != nil && errors.Is(err, context.Canceled) && rw.clientGone()
}

// logRequestError writes a handler's error line for a failure of a call that
// ran on ctx; it takes slog.Error's arguments and writes the record
// slog.Error(msg, args...) writes — at ERROR — unless the client gave up
// (clientGaveUp) on the failure named by the "error" argument and ctx itself
// ended with the request: then the same record at INFO with client_gone=true.
// A line without an "error" argument stays ERROR, and so does the failure of
// a call on a context detached from the request (context.WithoutCancel): it
// never ends with the request.
func logRequestError(ctx context.Context, msg string, args ...any) {
	gone := ctx != nil && ctx.Err() != nil && clientGaveUp(loggedRequest(ctx), errorArg(args))
	logFailure(gone, msg, args)
}

// logResponseError is logRequestError for a writer that has the response but
// no context: the request is found through w's chain.
func logResponseError(w http.ResponseWriter, msg string, args ...any) {
	logFailure(clientGaveUp(loggedResponse(w), errorArg(args)), msg, args)
}

// logEncodeError writes the line of a response body that could not be
// written: ERROR, unless the client is gone and the failure is the write to
// it — a JSON marshal error, or a body the response may not carry, is a
// server fault whatever the client does.
func logEncodeError(w http.ResponseWriter, msg string, args ...any) {
	var (
		unsupportedValue *json.UnsupportedValueError
		unsupportedType  *json.UnsupportedTypeError
		marshaler        *json.MarshalerError
	)
	err := errorArg(args)
	serverFault := errors.As(err, &unsupportedValue) || errors.As(err, &unsupportedType) || errors.As(err, &marshaler) ||
		errors.Is(err, http.ErrBodyNotAllowed) || errors.Is(err, http.ErrHijacked) || errors.Is(err, http.ErrContentLength)
	rw := loggedResponse(w)
	logFailure(err != nil && !serverFault && rw != nil && rw.clientGone(), msg, args)
}

// errorArg returns the error value of the "error" key in slog-style
// arguments (key-value pairs or slog.Attr), nil when there is none.
func errorArg(args []any) error {
	for i := 0; i < len(args); i++ {
		switch a := args[i].(type) {
		case slog.Attr:
			if a.Key == "error" {
				if e, ok := a.Value.Any().(error); ok {
					return e
				}
			}
		case string:
			if i+1 >= len(args) {
				return nil
			}
			if a == "error" {
				if e, ok := args[i+1].(error); ok {
					return e
				}
			}
			i++
		}
	}
	return nil
}

func logFailure(gone bool, msg string, args []any) {
	if gone {
		slog.Info(msg, append(append(make([]any, 0, len(args)+2), args...), "client_gone", true)...)
		return
	}
	slog.Error(msg, args...)
}

// Write outcomes a failed write states in its line (issue #417): the client
// of a cancelled write does not learn what happened, the operator reads it.
const (
	writeRolledBack = "rolled_back" // the transaction ended without a commit
	writeCommitted  = "committed"   // the write committed; a later step failed
	// writeUnknown: the COMMIT was sent and its answer never arrived (the
	// request was cancelled or the connection broke during it) — the
	// database may have committed the transaction.
	writeUnknown = "unknown"
)

// writeOutcome states the outcome of a failed write bracket (pgxdb.Write,
// WriteOpts) whose Stages name the commit: unknown when the COMMIT was sent
// and its answer never came back (no PostgreSQL error answer, not the
// rollback answer pgx reports as ErrTxCommitRollback, not refused before
// sending — pgconn.SafeToRetry), rolled back for everything else (a
// statement inside the transaction failed, the database refused or rolled
// back the commit, or the commit never left). For an unlabeled bracket the commit is not
// recognisable and the answer would claim too much — use it only on labeled
// ones.
func writeOutcome(err error) string {
	var pgErr *pgconn.PgError
	if pgxdb.FailedAtCommit(err) && !errors.As(err, &pgErr) && !errors.Is(err, pgx.ErrTxCommitRollback) && !pgconn.SafeToRetry(err) {
		return writeUnknown
	}
	return writeRolledBack
}
