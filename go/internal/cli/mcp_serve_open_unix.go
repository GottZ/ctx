//go:build unix

package cli

import "syscall"

// mcpOpenNonblock opens the files of `ctx mcp serve --allow-files` without
// blocking: opening a FIFO for reading or writing returns at once (or fails),
// and the opened file's type decides. On a regular file the flag changes
// nothing.
const mcpOpenNonblock = syscall.O_NONBLOCK
