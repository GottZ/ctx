package handler

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The server line of an MCP write tool (issue #453).
//
// A /mcp POST carries one tools/call (stateless Streamable HTTP, a JSON
// answer), and the request logger writes one "http request" line for it —
// status 200 whatever the tool did, since the SDK answers a tool's failure
// inside the JSON-RPC body. The write tools answered their outcome to the
// client only. Every call that reaches a tool without ReadOnlyHint now writes
// one line, "mcp tool write", at INFO when the tool returns: the tool, the
// block (or blob) it wrote or the number of items of a batch, the write's
// outcome, the call's duration, the request id that joins it to the request
// line, and client_gone=true when the client had left by then. Arguments,
// titles and content never go into it.
//
// "Client gone" is read from the request, not from the tool's context:
// go-sdk v1.8.0 hides the request's cancellation from every tool handler
// (mcp_cancel.go) and only the query tool binds itself to it again, so a
// write tool runs to its end whether or not its client is still there. The
// line asks the request logger's record (responseWriter.clientGone), which
// travels in the context's values; the SDK's handler waits for the tool, so
// the record is still open when the line is written. A client that leaves
// after the tool returned and before the SDK wrote the answer is seen only
// by the request line (499).
//
// A call the SDK refuses before the tool runs — arguments that fail the
// schema, an unknown tool — reaches no tool and writes no tool line; nothing
// was written.

// toolWriteLineMsg is the message of the line.
const toolWriteLineMsg = "mcp tool write"

// writeNone is the outcome of a write tool call that wrote nothing: it was
// refused before its write, found nothing to change, or never reached it.
const writeNone = "none"

// toolWrite is the record of one write tool call: what the tool noted while
// it ran. Only the tool's own goroutine touches it.
type toolWrite struct {
	tool    string
	start   time.Time
	blockID string
	blobID  string
	count   int
	outcome string
	staged  bool
}

type toolWriteKey struct{}

// toolWriteOf returns the record of the write tool call ctx belongs to, nil
// outside one (a REST request, a tool handler called directly).
func toolWriteOf(ctx context.Context) *toolWrite {
	tw, _ := ctx.Value(toolWriteKey{}).(*toolWrite)
	return tw
}

// noteToolWrite records the end of the call's write: committed for a nil
// err, else what writeOutcome states — so err must come from a labeled
// bracket (pgxdb.Write with Stages). A no-op outside a write tool call, so a
// core both transports share notes for both.
func noteToolWrite(ctx context.Context, err error) {
	noteToolOutcome(ctx, err, writeOutcome)
}

// noteToolStatement is noteToolWrite for a write that is one statement
// outside a transaction bracket (statementOutcome).
func noteToolStatement(ctx context.Context, err error) {
	noteToolOutcome(ctx, err, statementOutcome)
}

// noteToolStaged records that the call's write is a staged card (a
// confirm_writes key): the block itself is untouched. err is the card's
// write; labeled says whether it ran in a labeled bracket.
func noteToolStaged(ctx context.Context, err error, labeled bool) {
	tw := toolWriteOf(ctx)
	if tw == nil {
		return
	}
	tw.staged = true
	if labeled {
		noteToolWrite(ctx, err)
		return
	}
	noteToolStatement(ctx, err)
}

func noteToolOutcome(ctx context.Context, err error, failed func(error) string) {
	tw := toolWriteOf(ctx)
	if tw == nil {
		return
	}
	if err == nil {
		tw.outcome = writeCommitted
		return
	}
	tw.outcome = failed(err)
}

// noteToolBlock names the block the call wrote or meant to write.
func noteToolBlock(ctx context.Context, id string) {
	if tw := toolWriteOf(ctx); tw != nil {
		tw.blockID = id
	}
}

// noteToolBlob names the blob the call wrote or meant to write.
func noteToolBlob(ctx context.Context, id string) {
	if tw := toolWriteOf(ctx); tw != nil {
		tw.blobID = id
	}
}

// noteToolCount names the number of items (blocks, ids) of a batch call.
func noteToolCount(ctx context.Context, n int) {
	if tw := toolWriteOf(ctx); tw != nil {
		tw.count = n
	}
}

// log writes the call's line.
func (tw *toolWrite) log(ctx context.Context) {
	outcome := tw.outcome
	if outcome == "" {
		outcome = writeNone
	}
	args := make([]any, 0, 16)
	args = append(args, "tool", tw.tool)
	if tw.blockID != "" {
		args = append(args, "block_id", tw.blockID)
	}
	if tw.blobID != "" {
		args = append(args, "blob_id", tw.blobID)
	}
	if tw.count > 0 {
		args = append(args, "count", tw.count)
	}
	args = append(args, "write_outcome", outcome)
	if tw.staged {
		args = append(args, "staged", true)
	}
	args = append(args, "duration_ms", time.Since(tw.start).Milliseconds(), "request_id", RequestIDFromContext(ctx))
	if rw := loggedRequest(ctx); rw != nil && rw.clientGone() {
		args = append(args, "client_gone", true)
	}
	slog.Info(toolWriteLineMsg, args...)
}

// addMCPTool registers a tool: mcp.AddTool, and for a tool without
// ReadOnlyHint the handler writes the call's server line. Every tool of the
// server is registered through it, so a new write tool gets its line from
// its annotations.
func addMCPTool[In any](server *mcp.Server, t *mcp.Tool, h mcp.ToolHandlerFor[In, any]) {
	if t.Annotations == nil || !t.Annotations.ReadOnlyHint {
		h = withToolWriteLine(t.Name, h)
	}
	mcp.AddTool(server, t, h)
}

// withToolWriteLine wraps a write tool's handler with its line. The line is
// written when the handler returns — also when it panics (the deferred line
// runs while the panic unwinds to mcpRecover), with what the tool noted up to
// then.
func withToolWriteLine[In any](name string, h mcp.ToolHandlerFor[In, any]) mcp.ToolHandlerFor[In, any] {
	return func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		tw := &toolWrite{tool: name, start: time.Now()}
		ctx = context.WithValue(ctx, toolWriteKey{}, tw)
		defer tw.log(ctx)
		return h(ctx, req, in)
	}
}

// statementOutcome states the outcome of a failed write whose commit is not
// recognisable (writeOutcome needs a labeled bracket): one statement outside
// a transaction (it commits on its own), or a bracket without a commit label
// (the issue writes). Rolled back when the database answered with an error
// (a statement or the COMMIT refused — pgx.ErrTxCommitRollback for a
// transaction that had failed), the statement found no row to write, no
// connection was made (pgconn.ConnectError), or nothing left
// (pgconn.SafeToRetry); unknown otherwise — it was sent and its answer never
// came, or came and could not be read, and the database may have committed
// it. What cannot be told apart from that stays unknown, the safe side: a
// context that ended before the pool handed out a connection (pgx reports
// the bare cancellation), a closed pool, an error of the caller's own between
// two statements of an unlabeled bracket.
func statementOutcome(err error) string {
	var (
		pgErr      *pgconn.PgError
		connectErr *pgconn.ConnectError
	)
	if errors.As(err, &pgErr) || errors.As(err, &connectErr) || errors.Is(err, pgx.ErrNoRows) ||
		errors.Is(err, pgx.ErrTxCommitRollback) || pgconn.SafeToRetry(err) {
		return writeRolledBack
	}
	return writeUnknown
}
