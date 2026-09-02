//go:build windows

package goldbench

import (
	"os"

	"golang.org/x/sys/windows"
)

// openNoFollow has no O_NOFOLLOW equivalent in the os.OpenFile flag space on
// Windows (reparse-point handling is a CreateFile attribute os.OpenFile does
// not expose), so the symlink guard the unix build enforces at open time is
// not available here. goldbench is a developer tool that reads and writes
// its own dump directory; the trade-off is documented rather than emulated.
const openNoFollow = 0

// lockDumpExclusive takes a non-blocking exclusive LockFileEx over the whole
// file — the Windows counterpart of flock(LOCK_EX|LOCK_NB). The lock is
// released when the handle is closed, exactly like the flock it mirrors.
func lockDumpExclusive(f *os.File) error {
	ol := new(windows.Overlapped)
	return windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, ^uint32(0), ^uint32(0), ol)
}
