package handler

import (
	"context"
	"net/http"
)

// The query tool ends with its client (#195).
//
// go-sdk v1.8.0 does not end a tool call when the client goes away. In
// stateless mode every POST gets a jsonrpc2 connection over the request
// context, and that connection hides the context's cancellation from the
// handlers (internal/jsonrpc2/conn.go, notDone); the deferred session.Close
// in serveStateless (mcp/streamable.go) then waits for them. A query whose
// client had disconnected ran retrieval, rerank and synthesis to the end, on
// the operator's backends and the tenant's quota, for an answer nobody
// received.
//
// The SDK's StreamableHTTPOptions.PropagateRequestCancellation is not the
// fix. It applies to protocol 2026-07-28 and later only, so a client on the
// default protocol keeps the hidden cancellation. And it applies to every
// handler at once, the write tools included: there a cancel between the
// committed upsert and its booking (mcpStoreHandler: store.UpsertBlock, then
// store.LogAccess) leaves a written block without the row the write rate
// limit counts.
//
// Values do pass the SDK, so the request context travels as one, and the
// query tool, the tool that reaches the LLM and embedding backends and the
// tenant quota, binds its delegation to it. The REST route /api/query has
// always ended with its client this way; the delegation now does too.

type mcpRequestContextKey struct{}

// withMCPRequestContext hands the HTTP request context to the tool handlers.
func withMCPRequestContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), mcpRequestContextKey{}, r.Context())))
	})
}

// mcpRequestBoundContext returns ctx, canceled as well when the HTTP request
// that carries the call ends: net/http cancels it when it sees the client
// close the connection, and when the request is over. release must run when
// the call returns. Without a request context in ctx (a handler called
// directly) ctx comes back unchanged.
func mcpRequestBoundContext(ctx context.Context) (bound context.Context, release func()) {
	reqCtx, _ := ctx.Value(mcpRequestContextKey{}).(context.Context)
	if reqCtx == nil {
		return ctx, func() {}
	}
	bound, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(reqCtx, cancel)
	return bound, func() {
		stop()
		cancel()
	}
}
