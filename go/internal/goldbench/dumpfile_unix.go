//go:build unix

package goldbench

import (
	"os"
	"syscall"
)

// openNoFollow is OR-ed into every dump-file open: never follow a symlink at
// the dump path (the file carries full-text prompts, i.e. private block
// content, and a planted link could redirect them).
const openNoFollow = syscall.O_NOFOLLOW

// openAppend makes every write land at end-of-file regardless of the offset.
const openAppend = os.O_APPEND

// lockDumpExclusive takes a non-blocking exclusive flock(2) on the open dump
// file so a second driver on the same file fails fast (ErrDumpLocked) instead
// of interleaving records.
func lockDumpExclusive(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}
