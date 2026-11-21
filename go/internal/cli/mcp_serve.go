package cli

// mcp_serve.go — `ctx mcp serve` (#409): the CLI as a local MCP server over
// stdio that forwards to <CTX_BASE_URL>/mcp with the key of the CLI
// configuration.
//
// The proxy registers no tool of its own. It is an SDK server whose receiving
// middleware answers tools/list and tools/call from the remote server through
// an SDK client session, so the tool set, every description and schema, the
// server instructions and every refusal are the remote's — a tool added on the
// server appears here without a CLI release. Forwarding JSON-RPC frames by hand
// was the alternative; it would have meant re-implementing the Streamable HTTP
// client (JSON and SSE answers, the protocol-version header, sessions) that the
// SDK already ships, and answering initialize remotely, which fails the whole
// local session when the server is down at start.
//
// State is one remote session, held for the life of the process and replaced
// after a failure: nothing in the proxy grows with the number of tools or with
// the time it runs. With --allow-files the proxy adds two parameters to the
// mirrored schemas (mcp_serve_files.go); without it the arguments pass through
// byte for byte.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
)

// mcpServeConnectWait bounds opening the session to the remote: one
// server/discover or initialize exchange, which a ctx server answers in
// milliseconds. A server that accepts the connection and never answers would
// otherwise hold the connect — and every request waiting for it — until the
// local client gives up. The wait also bounds the local initialize, which
// carries the remote's instructions: an MCP client gives a stdio server a
// limited time to answer it, and a remote that misses this wait costs the
// session its instructions, not its connection. Tool calls themselves are not
// bounded here (a query runs as long as the server's keepalive holds it).
const mcpServeConnectWait = 10 * time.Second

// errMCPKeyRejected is what a 401 from the remote becomes (mcpKeyTransport), so
// the answer can name the cause instead of the SDK's bare status text.
var errMCPKeyRejected = errors.New("the server refused the API key of the ctx configuration (HTTP 401)")

// The codes the SDK gives its own transport failures (go-sdk v1.8.0
// internal/jsonrpc2/wire.go: ErrClientClosing, ErrServerClosing, ErrRejected).
// They arrive as JSON-RPC errors like the server's answers do, but no server
// sent them: a connection refused is ErrRejected wrapping the dial error.
const (
	sdkCodeClientClosing = -32003
	sdkCodeServerClosing = -32004
	sdkCodeRejected      = -32005
)

// errSDKRejected matches the SDK's ErrRejected anywhere in a chain
// (jsonrpc.Error.Is compares codes): the request did not get through, and the
// connection is intact.
var errSDKRejected = &jsonrpc.Error{Code: sdkCodeRejected}

// remoteAnswered returns the JSON-RPC error the remote server answered with,
// and false when err is not such an answer (a transport failure of the SDK, a
// cancellation, anything else). A rejected HTTP answer that carries a JSON-RPC
// error is wrapped by the SDK with the server's error first, so errors.As finds
// that one.
func remoteAnswered(err error) (*jsonrpc.Error, bool) {
	var wire *jsonrpc.Error
	if !errors.As(err, &wire) {
		return nil, false
	}
	switch wire.Code {
	case sdkCodeClientClosing, sdkCodeServerClosing, sdkCodeRejected:
		return nil, false
	}
	return wire, true
}

// MCPProxy is the server behind `ctx mcp serve`.
type MCPProxy struct {
	endpoint string       // <CTX_BASE_URL>/mcp
	http     *http.Client // carries the key as X-Context-Key, never in a URL
	cfgErr   error        // the configuration could not be loaded: every forwarded request answers it
	files    *mcpFileRoots
	server   *mcp.Server

	connectWait time.Duration // mcpServeConnectWait; a field so tests can shorten it

	// connecting is a one-slot token held while a request looks up or opens the
	// session: a request waiting behind a connect that hangs gives up with its
	// own context instead of piling up behind a mutex.
	connecting chan struct{}
	mu         sync.Mutex
	session    *mcp.ClientSession // nil until the first forwarded request, and after a broken connection
}

// NewMCPProxy builds the proxy. cfgErr is the error loading the CLI
// configuration failed with: the proxy still starts and every forwarded request
// answers it, so the client shows the cause instead of a server that exits.
// allowFiles are the --allow-files directories; an entry that is not a
// directory is an error here, before any request is served.
func NewMCPProxy(cfg Config, cfgErr error, allowFiles []string) (*MCPProxy, error) {
	p := &MCPProxy{cfgErr: cfgErr, connecting: make(chan struct{}, 1), connectWait: mcpServeConnectWait}
	if cfgErr == nil {
		p.endpoint = strings.TrimSuffix(cfg.BaseURL, "/") + "/mcp"
		keyed, err := newKeyTransport(cfg.BaseURL, cfg.Key, http.DefaultTransport)
		if err != nil {
			p.cfgErr = fmt.Errorf("CTX_BASE_URL is not a usable URL: %w", err)
		}
		// No overall timeout: a query call runs as long as the server's
		// keepalive holds it open, and the local client cancels a call it gives
		// up on. Dial and TLS limits are http.DefaultTransport's; opening the
		// session is bounded by connectWait.
		p.http = &http.Client{Transport: mcpKeyTransport{keyed: keyed}}
	}
	if len(allowFiles) > 0 {
		roots, err := newMCPFileRoots(allowFiles)
		if err != nil {
			return nil, err
		}
		p.files = roots
	}
	p.server = mcp.NewServer(&mcp.Implementation{Name: "ctx", Version: Version}, &mcp.ServerOptions{
		// Tools only, and no list_changed: the proxy relays no notification
		// from the remote, and a client re-lists when it reconnects.
		Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}},
	})
	p.server.AddReceivingMiddleware(p.forward)
	return p, nil
}

// Server is the local MCP server; run it on a transport (stdio for the
// command, an in-memory pair in tests).
func (p *MCPProxy) Server() *mcp.Server { return p.server }

// Close ends the remote session, if one is open, and releases the
// --allow-files directories.
func (p *MCPProxy) Close() error {
	if p.files != nil {
		p.files.close()
	}
	p.mu.Lock()
	cs := p.session
	p.session = nil
	p.mu.Unlock()
	if cs == nil {
		return nil
	}
	return cs.Close()
}

// forward is the receiving middleware: tools/list and tools/call go to the
// remote, initialize and server/discover take the remote's instructions, every
// other method (ping, logging) the SDK answers locally.
func (p *MCPProxy) forward(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		switch r := req.(type) {
		case *mcp.ListToolsRequest:
			return p.listTools(ctx, r)
		case *mcp.CallToolRequest:
			return p.callTool(ctx, r)
		}
		res, err := next(ctx, method, req)
		switch r := res.(type) {
		case *mcp.InitializeResult:
			r.Instructions = p.instructions(ctx)
		case *mcp.DiscoverResult:
			r.Instructions = p.instructions(ctx)
		}
		return res, err
	}
}

// instructions are the remote's server instructions, or "" when the remote
// cannot be reached, at most connectWait (the cause goes to stderr, which an
// MCP client keeps out of the protocol).
func (p *MCPProxy) instructions(ctx context.Context) string {
	cs, err := p.remote(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v; initialize answered without the server instructions\n", p.answer(ctx, nil, err))
		return ""
	}
	if res := cs.InitializeResult(); res != nil {
		return res.Instructions
	}
	return ""
}

func (p *MCPProxy) listTools(ctx context.Context, req *mcp.ListToolsRequest) (mcp.Result, error) {
	cs, err := p.remote(ctx)
	if err != nil {
		return nil, protocolError(p.answer(ctx, nil, err))
	}
	params := &mcp.ListToolsParams{}
	if req.Params != nil {
		params.Cursor = req.Params.Cursor
	}
	res, err := cs.ListTools(ctx, params)
	if err != nil {
		return nil, protocolError(p.answer(ctx, cs, err))
	}
	if p.files == nil {
		return res, nil
	}
	return p.files.withFileParams(res), nil
}

func (p *MCPProxy) callTool(ctx context.Context, req *mcp.CallToolRequest) (mcp.Result, error) {
	cs, err := p.remote(ctx)
	if err != nil {
		return toolFailure(p.answer(ctx, nil, err))
	}
	name, args := req.Params.Name, req.Params.Arguments
	var out *mcpOutputTarget
	if p.files != nil {
		var refusal string
		args, out, refusal, err = p.files.prepareCall(ctx, cs, name, args)
		if err != nil {
			return toolFailure(p.answer(ctx, cs, err))
		}
		if refusal != "" {
			return refusalResult(refusal), nil
		}
	}
	params := &mcp.CallToolParams{Name: name}
	if len(args) > 0 {
		params.Arguments = args
	}
	res, err := cs.CallTool(ctx, params)
	if err != nil {
		return toolFailure(p.answer(ctx, cs, err))
	}
	if out != nil {
		return out.answer(res), nil
	}
	return res, nil
}

// remote is the session to the remote /mcp, opened on first use and after a
// failure dropped it. One session at a time; concurrent first calls wait for
// the one that connects, which ends after connectWait at the latest.
func (p *MCPProxy) remote(ctx context.Context) (*mcp.ClientSession, error) {
	if p.cfgErr != nil {
		return nil, p.cfgErr
	}
	select {
	case p.connecting <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-p.connecting }()
	p.mu.Lock()
	cs := p.session
	p.mu.Unlock()
	if cs != nil {
		return cs, nil
	}
	cs, err := p.connect(ctx)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.session = cs
	p.mu.Unlock()
	return cs, nil
}

// connect opens a session to the remote and answers within connectWait. When
// the deadline cuts the handshake, the SDK sends the server a cancellation
// notice and its cleanup waits for that notice (up to five seconds against a
// server that does not answer, go-sdk mcp/transport.go notifyCancellationTimeout);
// that cleanup finishes in the background, so the caller gets its answer and
// the connect token is free when the wait is over. A session the handshake
// still produces after that is closed, not kept.
func (p *MCPProxy) connect(ctx context.Context) (*mcp.ClientSession, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "ctx mcp serve", Version: Version}, nil)
	// The session outlives this deadline: the SDK detaches the connection
	// from the context Connect gets.
	connectCtx, cancel := context.WithTimeout(ctx, p.connectWait)
	defer cancel()
	type connected struct {
		cs  *mcp.ClientSession
		err error
	}
	done := make(chan connected, 1)
	go func() {
		cs, err := client.Connect(connectCtx, &mcp.StreamableClientTransport{
			Endpoint:   p.endpoint,
			HTTPClient: p.http,
			// The server is stateless and answers GET with 405; nothing comes
			// on a standalone stream that the proxy would relay.
			DisableStandaloneSSE: true,
		}, nil)
		done <- connected{cs, err}
	}()
	select {
	case r := <-done:
		if r.err == nil {
			return r.cs, nil
		}
		return nil, p.connectFailure(ctx, connectCtx, r.err)
	case <-connectCtx.Done():
	}
	select {
	case r := <-done: // finished together with the deadline
		if r.err == nil {
			return r.cs, nil
		}
	default:
		go func() {
			if r := <-done; r.cs != nil {
				_ = r.cs.Close()
			}
		}()
	}
	return nil, p.connectFailure(ctx, connectCtx, connectCtx.Err())
}

// connectFailure names a connect the wait ended; a cancelled request stays a
// cancellation, and any other failure keeps its own cause.
func (p *MCPProxy) connectFailure(ctx, connectCtx context.Context, err error) error {
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(connectCtx.Err(), context.DeadlineExceeded):
		return fmt.Errorf("no MCP session within %s: the server took the connection and did not answer", p.connectWait)
	default:
		return err
	}
}

// answer turns a failed forwarded request into what the local client gets.
//
//   - A JSON-RPC error is the remote server's own answer (an unknown tool,
//     invalid parameters): returned as it is, so a refusal reads here in the
//     words it reads against /mcp. The session stays.
//   - A cancelled request (the local client gave up) stays a cancellation.
//   - Anything else is a failure to reach the server or to use the session,
//     and the answer names the cause. A failure the SDK marks as rejected by
//     the transport (a dial error, a 401, a 429 or 5xx it takes as transient)
//     leaves its connection intact, and the session stays. Any other failure
//     broke the connection (an answer that is no JSON-RPC, a 403 or 404 in
//     front of the server, a stateful server's lost session): the session is
//     dropped, and the next request opens a new one.
func (p *MCPProxy) answer(ctx context.Context, cs *mcp.ClientSession, err error) error {
	if wire, ok := remoteAnswered(err); ok {
		return wire
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if cs != nil && !errors.Is(err, errSDKRejected) {
		p.drop(cs)
	}
	switch {
	case p.cfgErr != nil:
		return mcpFailure("ctx mcp serve: no usable ctx configuration: " + err.Error())
	case errors.Is(err, errMCPKeyRejected):
		return mcpFailure(fmt.Sprintf("ctx mcp serve: %s: %s", p.endpoint, errMCPKeyRejected))
	default:
		return mcpFailure(fmt.Sprintf("ctx mcp serve: cannot use the server at %s: %s", p.endpoint, err))
	}
}

// mcpFailure is a failure the proxy names in its own words. A string, not a
// wrapped error, on purpose: the SDK's chain carries its transport codes as
// JSON-RPC errors, and toolFailure must not take the failure for the server's
// answer.
type mcpFailure string

func (f mcpFailure) Error() string { return string(f) }

// drop forgets a broken session. It is closed in the background: closing
// waits for the session's other calls, and the failure answer must not.
func (p *MCPProxy) drop(cs *mcp.ClientSession) {
	p.mu.Lock()
	if p.session == cs {
		p.session = nil
	}
	p.mu.Unlock()
	go func() { _ = cs.Close() }()
}

// protocolError gives a failure the proxy names itself the JSON-RPC code of an
// internal error; the server's own JSON-RPC errors and cancellations pass as
// they are.
func protocolError(err error) error {
	var failure mcpFailure
	if errors.As(err, &failure) {
		return &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: failure.Error()}
	}
	return err
}

// toolFailure answers a tools/call that did not reach the server. A JSON-RPC
// error and a cancellation stay protocol errors; every other cause becomes an
// error result, which a client shows the model rather than hiding it as a
// transport fault.
func toolFailure(err error) (mcp.Result, error) {
	var wire *jsonrpc.Error
	if errors.As(err, &wire) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil, err
	}
	return refusalResult(err.Error()), nil
}

func refusalResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}, IsError: true}
}

// mcpKeyTransport sends the requests to the remote through keyTransport — the
// key of the CLI configuration, only to the origin of CTX_BASE_URL, a redirect
// followed only within it, exactly as for every other keyed request of the
// CLI (keytransport.go) — and turns a 401 into errMCPKeyRejected.
type mcpKeyTransport struct {
	keyed keyTransport
}

func (t mcpKeyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.keyed.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		_ = resp.Body.Close()
		return nil, errMCPKeyRejected
	}
	return resp, nil
}

func mcpServeCmd(getClient func() (*Client, error)) *cobra.Command {
	var allowFiles []string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Serve the server's MCP tools over stdio, for a local MCP client",
		Long: `Speaks MCP on stdin/stdout and forwards every tool call to <CTX_BASE_URL>/mcp
with the key of the CLI configuration. Tools, descriptions and server
instructions are the server's. stdout carries protocol frames only.

--allow-files DIR (repeatable) adds content_file to every tool that takes a
content string (the file's text is sent as content) and output_file to get
(the block's content is written to the file). Both are confined to the given
directories after symlink resolution; without the flag neither is offered.`,
		Example: `  claude mcp add ctx -- ctx mcp serve
  claude mcp add ctx -- ctx mcp serve --allow-files /home/me/notes`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var cfg Config
			c, cfgErr := getClient()
			if cfgErr == nil {
				cfg = Config{BaseURL: c.BaseURL, Key: c.Key}
			}
			p, err := NewMCPProxy(cfg, cfgErr, allowFiles)
			if err != nil {
				return err
			}
			defer func() { _ = p.Close() }()
			return p.Server().Run(cmd.Context(), &mcp.StdioTransport{})
		},
	}
	cmd.Flags().StringArrayVar(&allowFiles, "allow-files", nil,
		"directory whose files the tools may read (content_file) and write (output_file); repeatable, confined after symlink resolution")
	return cmd
}
