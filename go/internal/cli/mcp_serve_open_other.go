//go:build !unix

package cli

// mcpOpenNonblock is zero where opening a file cannot block on a peer the way
// a Unix FIFO does (mcp_serve_open_unix.go).
const mcpOpenNonblock = 0
