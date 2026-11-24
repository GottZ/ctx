package handler

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpNULExempt names, per tool, the argument whose NUL is a legitimate value:
// blob_store's text is stored as bytes (bytea), as its file is.
var mcpNULExempt = map[string]string{
	"blob_store": "text",
}

// withNULRefusal refuses a call whose arguments carry a NUL character
// (issue #519) before the tool runs: the tool error of the invalid_body class
// naming the argument ("items[0]: content" in a batch), no line, no
// statement. It is the MCP side of the NUL guard (nul_guard.go): /mcp
// answers in JSON-RPC, so the refusal is a tool result, not an HTTP 400.
func withNULRefusal[In any](tool string, h mcp.ToolHandlerFor[In, any]) mcp.ToolHandlerFor[In, any] {
	exempt := mcpNULExempt[tool]
	skip := func(p string) bool { return exempt != "" && p == exempt }
	return func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		if req != nil && req.Params != nil {
			if path, found := jsonNULPath(req.Params.Arguments, skip); found {
				if path == "" {
					path = "arguments"
				}
				return classInvalidBody.errResult(path + " " + nulFieldMessage), nil, nil
			}
		}
		return h(ctx, req, in)
	}
}
