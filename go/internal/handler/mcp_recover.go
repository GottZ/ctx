package handler

import (
	"context"
	"log/slog"
	"runtime/debug"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpInternalErrorMsg is the client-facing text of a recovered panic on /mcp:
// the opaque wording writeInternal and the classInternal rejections already
// put on the wire. The panic value and the stack go to the log only.
const mcpInternalErrorMsg = "Internal server error"

// mcpRecover is the /mcp twin of Recovery (middleware.go). newMCPServer
// installs it as the server's outermost receiving middleware.
//
// Recovery guards the HTTP goroutine, and an MCP request does not run there:
// go-sdk v1.8.0 hands every incoming message to a goroutine of its own
// (internal/jsonrpc2/conn.go, handleAsync) and recovers nothing on it, so a
// panic in a tool handler — or anywhere below one, the delegated /api/query
// pipeline included — ended the process for every tenant (#194). A receiving
// middleware runs on exactly that goroutine and wraps every method the server
// dispatches: each tool registered now or later, however it is registered,
// and resource, prompt and notification handlers should any ever exist. One
// guard on the server instead of one per handler. It guards that goroutine
// only: a goroutine the pipeline detaches for itself (the fire-and-forget
// access log of a query, the attribution write of blob_link) is outside it,
// as it is outside Recovery on REST.
//
// After a panic:
//
//   - the log carries what Recovery logs — the panic value, the stack, the
//     request id — plus the MCP method and the tool name, at ERROR. The
//     server is stateless (no MCP session id); the request id ties the line
//     to the request's access log line.
//   - a tools/call is answered the way the tools answer every other
//     server-side fault: a tool result with isError, the opaque text and the
//     code "internal" (classInternal). The code matters: on store, blob_store
//     and blob_link an uncoded isError means "staged", which a crash is not.
//   - any other method gets the JSON-RPC internal error with the same text
//     (for a notification the SDK drops it: a notification has no reply).
//   - the handler returns normally, so the SDK finishes the request the way it
//     finishes a failed one: the response is written, the request context is
//     canceled, which ends the body heartbeat a delegated query may have
//     started (startHeartbeat, query.go). What the request held through
//     defers — the query tool's keepalive arm, the admission slot of
//     WithScheduler, a pool connection — was released while the panic
//     unwound.
func mcpRecover(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (result mcp.Result, err error) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			call, isToolCall := req.(*mcp.CallToolRequest)
			tool := ""
			if isToolCall && call != nil && call.Params != nil {
				tool = call.Params.Name
			}
			slog.Error("mcp: panic recovered",
				"error", rec,
				"stack", string(debug.Stack()),
				"mcp_method", method,
				"tool", tool,
				"request_id", RequestIDFromContext(ctx),
			)
			if isToolCall {
				result, err = classInternal.errResult(mcpInternalErrorMsg), nil
				return
			}
			result, err = nil, &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: mcpInternalErrorMsg}
		}()
		return next(ctx, method, req)
	}
}

// recoverMCPKeepalive is the keepalive ticker's guard (mcpKeepalive.run). It
// must be the deferred function itself: recover only stops a panic when the
// deferred call makes it directly. ctx is the request context, for the id.
func recoverMCPKeepalive(ctx context.Context) {
	if rec := recover(); rec != nil {
		slog.Error("mcp keepalive: panic recovered",
			"error", rec,
			"stack", string(debug.Stack()),
			"request_id", RequestIDFromContext(ctx),
		)
	}
}
