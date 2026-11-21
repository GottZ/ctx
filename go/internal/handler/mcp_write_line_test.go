package handler

// Issue #453: every MCP tool without ReadOnlyHint writes one server line per
// call that reaches it ("mcp tool write"); a read-only tool writes none. The
// census walks the production tool set — a tool registered later, however it
// is registered, is in it — and calls each tool with arguments its schema
// accepts.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// schemaArgs builds arguments the tool's input schema accepts: each required
// property with a plain value of its type.
func schemaArgs(t *testing.T, tool *mcp.Tool) map[string]any {
	t.Helper()
	schema, ok := tool.InputSchema.(map[string]any)
	if !ok {
		t.Fatalf("tool %s: input schema %T, want a JSON object", tool.Name, tool.InputSchema)
	}
	props, _ := schema["properties"].(map[string]any)
	required, _ := schema["required"].([]any)
	args := map[string]any{}
	for _, r := range required {
		name, _ := r.(string)
		prop, _ := props[name].(map[string]any)
		typ, _ := prop["type"].(string)
		if types, ok := prop["type"].([]any); ok {
			for _, ty := range types {
				if s, _ := ty.(string); s != "null" {
					typ = s
					break
				}
			}
		}
		switch typ {
		case "string":
			args[name] = "x"
		case "array":
			args[name] = []any{}
		case "object":
			args[name] = map[string]any{}
		case "integer", "number":
			args[name] = 1
		case "boolean":
			args[name] = false
		default:
			t.Fatalf("tool %s: required property %s has type %v", tool.Name, name, prop["type"])
		}
	}
	return args
}

func TestMCPEveryWriteToolWritesItsLine(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// The query tool delegates; a stub answers it so the read-only side runs.
	stub := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"answer": "", "sources": []any{}})
	})
	server := newMCPServer(MCPConfig{QueryHandler: stub})
	serverT, clientT := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer func() { _ = ss.Close() }()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "i453-census", Version: "0"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer func() { _ = cs.Close() }()

	var writes, reads []string
	for tool, err := range cs.Tools(ctx, nil) {
		if err != nil {
			t.Fatalf("tools/list: %v", err)
		}
		readOnly := tool.Annotations != nil && tool.Annotations.ReadOnlyHint
		logs := installRejectLogCapture(t)
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: tool.Name, Arguments: schemaArgs(t, tool)})
		if err != nil {
			t.Fatalf("%s: %v", tool.Name, err)
		}
		if res.IsError && slices.ContainsFunc(res.Content, func(c mcp.Content) bool {
			tc, ok := c.(*mcp.TextContent)
			return ok && strings.HasPrefix(tc.Text, `validating "arguments"`)
		}) {
			t.Fatalf("%s: the schema refused the census arguments: %+v", tool.Name, res.Content)
		}
		lines := logs.withMsg(toolWriteLineMsg)
		if readOnly {
			reads = append(reads, tool.Name)
			if len(lines) != 0 {
				t.Errorf("read-only tool %s wrote %d tool lines: %+v", tool.Name, len(lines), lines)
			}
			continue
		}
		writes = append(writes, tool.Name)
		if len(lines) != 1 || lines[0].attrs["tool"] != tool.Name || lines[0].attrs["write_outcome"] != writeNone {
			t.Errorf("write tool %s: tool lines %+v, want one with tool=%s and write_outcome=none (nothing was written)", tool.Name, lines, tool.Name)
		}
	}
	t.Logf("%d write tools %v, %d read-only %v", len(writes), writes, len(reads), reads)
	if len(writes)+len(reads) != 17 || len(writes) != 11 {
		t.Errorf("%d tools (%d write), want the 17 production tools with 11 write tools — a changed tool set updates this census", len(writes)+len(reads), len(writes))
	}
}

// TestStatementOutcome — what a failed write without a labeled commit states.
func TestStatementOutcome(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"the database answered with an error", fmt.Errorf("stage: %w", &pgconn.PgError{Code: "23505"}), writeRolledBack},
		{"no row to write", fmt.Errorf("link: %w", pgx.ErrNoRows), writeRolledBack},
		{"never sent", fmt.Errorf("stage: %w", safeToRetryErr{}), writeRolledBack},
		{"sent, no answer", fmt.Errorf("stage: %w", errors.New("read tcp: connection reset by peer")), writeUnknown},
		{"sent, cancelled", fmt.Errorf("stage: %w", context.Canceled), writeUnknown},
	} {
		if got := statementOutcome(tc.err); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestToolWriteNotesOutsideACall — the notes are no-ops outside a write tool
// call (the cores REST shares call them), and inside one they keep what the
// tool noted.
func TestToolWriteNotesOutsideACall(t *testing.T) {
	ctx := context.Background()
	noteToolWrite(ctx, nil)
	noteToolStaged(ctx, errors.New("x"), false)
	noteToolBlock(ctx, "b")
	noteToolCount(ctx, 3)
	if toolWriteOf(ctx) != nil {
		t.Fatal("a record outside a call")
	}
	tw := &toolWrite{tool: "store"}
	ctx = context.WithValue(ctx, toolWriteKey{}, tw)
	noteToolWrite(ctx, nil)
	noteToolBlock(ctx, "b1")
	if tw.outcome != writeCommitted || tw.blockID != "b1" || tw.staged {
		t.Errorf("record %+v, want committed b1 not staged", tw)
	}
	noteToolStaged(ctx, fmt.Errorf("stage: %w", &pgconn.PgError{Code: "P0001"}), false)
	if tw.outcome != writeRolledBack || !tw.staged {
		t.Errorf("record %+v, want rolled_back staged", tw)
	}
}
