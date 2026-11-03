package oai

import (
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
)

// EnabledGate is stage 1 of the gateway stack (design/02 §4.2, masterplan
// K4): with gateway.enabled=false every request gets NotFound — the same
// bytes for any method, path, credential or body — before any credential is
// looked at. A switched-off server does not reveal whether a key is valid,
// and an anonymous flood costs it no auth query. The flag is read per
// request (hot). A nil getter is a wiring bug and panics at construction.
func EnabledGate(get func() Settings) func(http.Handler) http.Handler {
	if get == nil {
		panic("oai.EnabledGate: nil settings getter")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !get().Enabled {
				NotFound(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Recover is the gateway's own recovery (R-02-14, masterplan K5): a panic
// under /v1 or /v2 is an A-internal fault and answers WriteInternal — 503 in
// the envelope with x-should-retry: false — where the global house Recovery
// would send a plain-text 500 that no SDK parses and that stops a peer ctx's
// chain. It sits inside the global chain, so the global Recovery never sees
// a gateway panic.
//
// Before the commit, the 503 carries only the headers the response had when
// Recover was entered (request id, security headers): whatever the panicking
// chain set — a Retry-After, rate-limit headers, a Content-Length — belongs
// to an answer that will never be sent.
//
// Once the response is committed (status line out, e.g. a stream in flight)
// nothing valid can follow, and ending it normally would hand the client a
// truncated body that reads like a complete one; the response is aborted
// instead (abortCommitted), so the client sees a failed read.
//
// http.ErrAbortHandler is no special case on this surface (Fremd-Review 2,
// FR2-1): re-panicked, it would reach the global house Recovery, which
// catches every panic — before the commit as a plain-text 500 (K5: never a
// 500 from inside ctx), after it as "internal server error" appended to the
// stream and a clean end. So a deliberate abort is handled like any panic:
// the internal 503 before the commit, abortCommitted after it. It is logged
// as a warning without a stack, because the handler asked for it.
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cw := &commitWatch{ResponseWriter: w}
		entry := w.Header().Clone()
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				slog.Warn("gateway: handler aborted the response",
					"method", r.Method,
					"path", r.URL.Path,
					"committed", cw.committed,
					"request_id", w.Header().Get("X-Request-ID"),
				)
			} else {
				slog.Error("gateway: panic recovered",
					"error", rec,
					"stack", string(debug.Stack()),
					"method", r.Method,
					"path", r.URL.Path,
					"committed", cw.committed,
					"request_id", w.Header().Get("X-Request-ID"),
				)
			}
			if cw.committed {
				abortCommitted(w)
				return
			}
			h := w.Header()
			clear(h)
			for k, v := range entry {
				h[k] = v
			}
			WriteInternal(w)
		}()
		next.ServeHTTP(cw, r)
	})
}

// abortCommitted ends a committed response so that the client sees an error,
// not a short success: on HTTP/1 the connection is taken over and closed
// (the body ends without its terminating chunk — the client's read fails);
// the global chain lets the takeover through (handler.Logger's writer
// unwraps). Where the connection cannot be taken over (a recorder; HTTP/2,
// which ctxd does not serve today — plain ListenAndServe, no TLS),
// http.ErrAbortHandler is raised as the last resort; net/http would reset
// the stream, but the house Recovery outside still swallows it (handover
// W02-08).
func abortCommitted(w http.ResponseWriter) {
	if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
		_ = conn.Close()
		return
	}
	panic(http.ErrAbortHandler)
}

// commitWatch records whether the response has been committed. Unwrap keeps
// http.ResponseController (flush, deadlines) working through it.
type commitWatch struct {
	http.ResponseWriter
	committed bool
}

func (c *commitWatch) WriteHeader(code int) {
	if code >= http.StatusOK {
		c.committed = true
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *commitWatch) Write(p []byte) (int, error) {
	c.committed = true
	return c.ResponseWriter.Write(p)
}

func (c *commitWatch) Flush() {
	c.committed = true
	_ = http.NewResponseController(c.ResponseWriter).Flush()
}

func (c *commitWatch) Unwrap() http.ResponseWriter { return c.ResponseWriter }
