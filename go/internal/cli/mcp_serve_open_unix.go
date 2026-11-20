//go:build unix

package cli

import "syscall"

// mcpOpenNonblock opens a content_file of `ctx mcp serve --allow-files`
// without blocking: opening a FIFO for reading returns at once, and the opened
// file's type decides. On a regular file the flag changes nothing.
const mcpOpenNonblock = syscall.O_NONBLOCK
