package handler

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GottZ/ctx/internal/config"
)

// handshakeVersions are the two ways a client learns the instructions: a
// client of protocol 2026-07-28 or later (the SDK default) asks
// server/discover, an older one — Claude Code today — sends initialize. ""
// keeps the client's default.
var handshakeVersions = []string{"", "2025-06-18"}

// connectProbe connects an in-memory client of the given protocol version to
// server.
func connectProbe(t *testing.T, server *mcp.Server, version string) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	serverT, clientT := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "instructions-probe", Version: "0"}, nil).Connect(ctx, clientT,
		&mcp.ClientSessionOptions{ProtocolVersion: version})
	if err != nil {
		t.Fatalf("client connect (protocol %q): %v", version, err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// TestMCPInstructionsFrameCtxAsMemory is the contract for the one text the
// server sends to every connecting agent at initialize when nobody configured
// one: the wording ctx ships (#315, arm C of the measurement on the pull
// request). The agent has read no README; this string is all it knows about
// ctx. It must say that ctx is the agent's memory, name the lightweight
// lookup first, and say where a saved fact's source goes. Read over a real
// client/server handshake, not from the source, so the test fails if the
// string stops reaching the client.
func TestMCPInstructionsFrameCtxAsMemory(t *testing.T) {
	const want = "ctx is your memory across sessions. Before you answer from memory or start work, look it up: 'search' for a lightweight lookup, 'query' for a question that needs a synthesized answer. When you learn something that should outlast this session, save it with 'store' and put its source into metadata.source."
	for _, version := range handshakeVersions {
		got := connectProbe(t, newMCPServer(MCPConfig{}), version).InitializeResult().Instructions
		t.Logf("instructions as received by the client (protocol %q, %d chars): %s", version, len(got), got)
		if got != want {
			t.Fatalf("protocol %q: instructions = %q, want the shipped wording %q", version, got, want)
		}
		if strings.Index(got, "'search'") > strings.Index(got, "'query'") {
			t.Errorf("instructions name 'query' before 'search'; the lightweight lookup comes first")
		}
	}
}

// TestMCPWriteToolsNameTheSourceKey: the store and update schemas say that a
// saved fact's source belongs in metadata.source (#315). The schema is the
// one place a client that never forwards server instructions (Hermes) still
// sees it, so the key must be in the tool list, not only in the instructions.
func TestMCPWriteToolsNameTheSourceKey(t *testing.T) {
	cs := connectProbe(t, newMCPServer(MCPConfig{}), "")
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	seen := map[string]bool{}
	for _, tool := range res.Tools {
		if tool.Name != "store" && tool.Name != "update" {
			continue
		}
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("%s schema: %v", tool.Name, err)
		}
		var schema struct {
			Properties map[string]struct {
				Description string `json:"description"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("%s schema: %v", tool.Name, err)
		}
		seen[tool.Name] = true
		if d := schema.Properties["metadata"].Description; !strings.Contains(d, "metadata.source") {
			t.Errorf("%s: metadata description %q does not name metadata.source", tool.Name, d)
		}
	}
	if !seen["store"] || !seen["update"] {
		t.Fatalf("tools/list lacks store or update: %v", seen)
	}
}

// swapConfig is a ConfigStore whose one generation a test replaces, the way a
// settings reload replaces the live store's.
type swapConfig struct{ c atomic.Pointer[config.Config] }

func (s *swapConfig) Snapshot() *config.Config                          { return s.c.Load() }
func (s *swapConfig) SnapshotForRequest(context.Context) *config.Config { return s.c.Load() }
func (s *swapConfig) SnapshotForTenant(context.Context, string) *config.Config {
	return s.c.Load()
}

// TestMCPInstructionsOverStatelessHTTP reads the instructions through the
// production /mcp handler (stateless Streamable HTTP, built once) while the
// config generation changes underneath it: each new connection's initialize
// carries the text of the generation current at that moment (#315).
func TestMCPInstructionsOverStatelessHTTP(t *testing.T) {
	cfg := &swapConfig{}
	base := config.Defaults()
	cfg.c.Store(base)
	srv := httptest.NewServer(NewMCPHandler(MCPConfig{Cfg: cfg}))
	defer srv.Close()
	read := func() string {
		t.Helper()
		var texts []string
		for _, version := range handshakeVersions {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			cs, err := mcp.NewClient(&mcp.Implementation{Name: "instructions-probe", Version: "0"}, nil).Connect(ctx,
				&mcp.StreamableClientTransport{Endpoint: srv.URL, DisableStandaloneSSE: true, MaxRetries: -1},
				&mcp.ClientSessionOptions{ProtocolVersion: version})
			if err != nil {
				cancel()
				t.Fatalf("connect (protocol %q): %v", version, err)
			}
			texts = append(texts, cs.InitializeResult().Instructions)
			_ = cs.Close()
			cancel()
		}
		if texts[0] != texts[1] {
			t.Fatalf("server/discover answered %q, initialize %q — one source, one text", texts[0], texts[1])
		}
		return texts[0]
	}
	set := func(enabled bool, text string) {
		c := *base
		c.MCP.InstructionsEnabled, c.MCP.Instructions = enabled, text
		cfg.c.Store(&c)
	}
	shipped := config.ShippedMCPInstructions()

	if got := read(); got != shipped[len(shipped)-1].Text {
		t.Errorf("default: instructions = %q, want the newest shipped wording", got)
	}
	set(true, "Team rules:\n- look it up first")
	if got := read(); got != "Team rules:\n- look it up first" {
		t.Errorf("custom: instructions = %q, want the custom text", got)
	}
	set(true, shipped[0].Text)
	if got := read(); got != shipped[len(shipped)-1].Text {
		t.Errorf("stored old wording: instructions = %q, want the newest shipped wording", got)
	}
	set(false, "Team rules")
	if got := read(); got != "" {
		t.Errorf("off: instructions = %q, want none", got)
	}
}

// panicConfig is a ConfigStore whose snapshot read panics — the overlay build
// behind SnapshotForRequest runs a DB read and config.Build on a cold tenant
// cache, on the SDK's own goroutine.
type panicConfig struct{}

func (panicConfig) Snapshot() *config.Config { panic("instructions probe: snapshot") }
func (panicConfig) SnapshotForRequest(context.Context) *config.Config {
	panic("instructions probe: snapshot")
}
func (panicConfig) SnapshotForTenant(context.Context, string) *config.Config {
	panic("instructions probe: snapshot")
}

// TestMCPInstructionsPanicIsRecovered: the instructions middleware runs inside
// the panic guard (mcp_recover.go), so a panic while it reads the settings
// fails that one handshake instead of ending the process for every tenant.
func TestMCPInstructionsPanicIsRecovered(t *testing.T) {
	server := newMCPServer(MCPConfig{Cfg: panicConfig{}})
	for _, version := range handshakeVersions {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		serverT, clientT := mcp.NewInMemoryTransports()
		ss, err := server.Connect(ctx, serverT, nil)
		if err != nil {
			cancel()
			t.Fatalf("server connect: %v", err)
		}
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "instructions-probe", Version: "0"}, nil).Connect(ctx, clientT,
			&mcp.ClientSessionOptions{ProtocolVersion: version})
		if err == nil {
			_ = cs.Close()
			t.Errorf("protocol %q: handshake succeeded although the settings read panicked", version)
		} else if !strings.Contains(err.Error(), mcpInternalErrorMsg) {
			t.Errorf("protocol %q: handshake error = %v, want the guard's %q", version, err, mcpInternalErrorMsg)
		}
		_ = ss.Close()
		cancel()
	}
}
