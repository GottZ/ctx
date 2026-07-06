// OS-level self-deprioritization of the overview-rebuild worker (wave E-B,
// plan-inference-scheduler design/05 §4.7: the child runs "unter nice 19
// (+ ionice -c3, wo verfügbar)"). The mechanism is SELF-deprioritization at
// worker start rather than external nice/ionice wrapper binaries: the
// container image guarantees no coreutils, and lowering one's own priority
// needs no privilege — setpriority(2) downward is always permitted (RLIMIT_NICE
// only gates raising) and IOPRIO_CLASS_IDLE is unprivileged since Linux 2.6.25.
// Best-effort by contract ("wo verfügbar"): failures are WARNed by the caller
// and never abort the rebuild — a full-priority worker still beats no worker.

package main

import "golang.org/x/sys/unix"

// ioprio_set(2)/ioprio_get(2) plumbing — x/sys/unix ships the per-arch syscall
// numbers (SYS_IOPRIO_*) but no wrapper functions, so the constants live here.
const (
	ioprioWhoProcess = 1  // IOPRIO_WHO_PROCESS; who == 0 targets the calling process
	ioprioClassIdle  = 3  // IOPRIO_CLASS_IDLE: I/O only when the disk is otherwise idle
	ioprioClassShift = 13 // IOPRIO_PRIO_VALUE(class, data) = class<<13 | data
)

// deprioritizeSelf drops the calling process to nice 19 and the idle I/O
// scheduling class (E-B). The two failures are returned separately so the
// caller can WARN with precision; both are nil on the happy path. Never
// fatal by contract — an unusual sandbox (e.g. a seccomp filter denying
// ioprio_set) must not kill the rebuild.
func deprioritizeSelf() (niceErr, ioErr error) {
	niceErr = unix.Setpriority(unix.PRIO_PROCESS, 0, 19)
	if _, _, errno := unix.Syscall(unix.SYS_IOPRIO_SET, ioprioWhoProcess, 0, ioprioClassIdle<<ioprioClassShift); errno != 0 {
		ioErr = errno
	}
	return niceErr, ioErr
}

// ioprioGet reads a process' I/O priority value (pid 0 = calling process) —
// the verification counterpart for the E-B priority probe (/proc exposes nice
// but has no ioprio file, so the probe reads it back through the syscall; on
// a child it needs only a matching UID).
func ioprioGet(pid int) (int, error) {
	prio, _, errno := unix.Syscall(unix.SYS_IOPRIO_GET, ioprioWhoProcess, uintptr(pid), 0)
	if errno != 0 {
		return 0, errno
	}
	return int(prio), nil
}
