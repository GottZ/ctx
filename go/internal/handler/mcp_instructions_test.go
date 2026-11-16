package handler

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestMCPInstructionsFrameCtxAsMemory is the contract for the one text the
// server sends to every connecting agent at initialize. The agent has read no
// README; this string is all it knows about ctx. It must say that ctx is the
// agent's memory, when to ask it, and that a saved fact carries its source.
// Read over a real client/server handshake, not from the source constant, so
// the test fails if the string stops reaching the client.
func TestMCPInstructionsFrameCtxAsMemory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	server := newMCPServer(MCPConfig{})
	serverT, clientT := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer func() { _ = ss.Close() }()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "instructions-probe", Version: "0"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer func() { _ = cs.Close() }()

	got := cs.InitializeResult().Instructions
	t.Logf("instructions as received by the client (%d chars): %s", len(got), got)

	for _, want := range []string{"your memory", "before you answer", "'query'", "'store'", "source"} {
		if !strings.Contains(strings.ToLower(got), want) {
			t.Errorf("instructions lack %q", want)
		}
	}
	if strings.Contains(got, "knowledge store with hybrid retrieval") {
		t.Errorf("instructions still describe ctx as a tool, not as the agent's memory")
	}
}
