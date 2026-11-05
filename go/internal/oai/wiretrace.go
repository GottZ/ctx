package oai

import (
	"net/http/httptrace"
	"sync/atomic"
)

// wireContactTrace is the meter's wire-contact trace of one attempt (W06-10
// hardening 3, H3-1; Fremd-Review 4 F4-1, F4-2, F4-7), shared by the chat
// and the embeddings route: it marks wired where a byte of the request can
// leave for a backend, in any try — it rides on the request context, and
// req.Clone carries it into httpx.DoRetryOnce's retry — instead of reading
// it off the error of the last try. conn records a connection in hand.
//
//   - WroteHeaderField, WroteHeaders, WroteRequest (any Err — a partly
//     written body reached the backend): the request is being written on an
//     established connection. HTTP/1.1 writes every request byte inside
//     Request.write, which calls WroteHeaderField after the request line
//     and the Host line, and WroteRequest when it returns, failed writes
//     included; HTTP/2 calls WroteHeaderField while it encodes the HEADERS
//     frame, before any of it is written. A dial, a refused or timed-out
//     connect and a failed TLS handshake reach none of them. Before an error
//     returns, HTTP/1.1 waits for its write loop, HTTP/2 for the stream's
//     writer unless the context ended (the caller's context rule), so the
//     mark is visible when the wire call returns.
//   - GotFirstResponseByte: the backend answered — wire contact by
//     definition; the hook runs before net/http hands the response over
//     (Fremd-Review 5 F5-3: an HTTP/1.1 backend that answers before it reads
//     can return the response before the write loop reaches the first write
//     hook).
//
// The caller's context rule: an attempt that ended by its context with a
// connection in hand (conn) and an error counts as wire contact — HTTP/2
// returns on the context without waiting for the stream's writer.
func wireContactTrace(wired, conn *atomic.Bool) *httptrace.ClientTrace {
	mark := func() { wired.Store(true) }
	return &httptrace.ClientTrace{
		GotConn:              func(httptrace.GotConnInfo) { conn.Store(true) },
		WroteHeaderField:     func(string, []string) { mark() },
		WroteHeaders:         mark,
		WroteRequest:         func(httptrace.WroteRequestInfo) { mark() },
		GotFirstResponseByte: mark,
	}
}
