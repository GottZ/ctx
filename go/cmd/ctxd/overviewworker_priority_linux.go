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

import (
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// ioprio_set(2)/ioprio_get(2) plumbing — x/sys/unix ships the per-arch syscall
// numbers (SYS_IOPRIO_*) but no wrapper functions, so the constants live here.
const (
	ioprioWhoProcess = 1  // IOPRIO_WHO_PROCESS; who == a thread ID (0 = calling thread)
	ioprioClassIdle  = 3  // IOPRIO_CLASS_IDLE: I/O only when the disk is otherwise idle
	ioprioClassShift = 13 // IOPRIO_PRIO_VALUE(class, data) = class<<13 | data
)

// deprioritizeSelf drops the WHOLE process to nice 19 and the idle I/O
// scheduling class (E-B). Under NPTL both setpriority(2) with who=0 and
// ioprio_set(2) with who=0 act on the CALLING THREAD only — the nice value is
// per-thread on Linux. The first cut relied on who=0 and therefore reniced
// whichever runtime M happened to run the goroutine, while every other thread
// (including the heavy rebuild work and the main thread that /proc/<pid>/stat
// reports) kept nice 0. The E-B priority probe caught exactly this as its
// "flake": /proc showed nice 0 whenever the call landed off the main thread
// (empirically confirmed 2026-07-20 — a locked non-main thread renicing
// "itself" leaves the main thread at 0).
//
// So: walk /proc/self/task and apply BOTH settings to every thread. New
// threads inherit nice and ioprio from their creator, so once a full pass
// covers all live threads, later Ms start deprioritized too. Threads can
// appear mid-walk (their creators are covered, but a bounded re-scan closes
// the window where a thread was cloned by a not-yet-covered sibling). The two
// failures are returned separately so the caller can WARN with precision;
// both are nil on the happy path. Never fatal by contract — an unusual
// sandbox (e.g. a seccomp filter denying ioprio_set) must not kill the
// rebuild.
func deprioritizeSelf() (niceErr, ioErr error) {
	seen := make(map[int]bool)
	for range 3 {
		entries, err := os.ReadDir("/proc/self/task")
		if err != nil {
			// No /proc (exotic sandbox): fall back to calling-thread scope —
			// strictly better than nothing, matches the best-effort contract.
			if e := unix.Setpriority(unix.PRIO_PROCESS, 0, 19); niceErr == nil {
				niceErr = e
			}
			if _, _, errno := unix.Syscall(unix.SYS_IOPRIO_SET, ioprioWhoProcess, 0, ioprioClassIdle<<ioprioClassShift); errno != 0 && ioErr == nil {
				ioErr = errno
			}
			return niceErr, ioErr
		}
		grew := false
		for _, e := range entries {
			tid, err := strconv.Atoi(e.Name())
			if err != nil || seen[tid] {
				continue
			}
			seen[tid] = true
			grew = true
			// A tid may exit between readdir and the calls (ESRCH) — that is
			// not a failure, the thread is simply gone.
			if err := unix.Setpriority(unix.PRIO_PROCESS, tid, 19); err != nil && err != unix.ESRCH && niceErr == nil {
				niceErr = err
			}
			if _, _, errno := unix.Syscall(unix.SYS_IOPRIO_SET, ioprioWhoProcess, uintptr(tid), ioprioClassIdle<<ioprioClassShift); errno != 0 && errno != unix.ESRCH && ioErr == nil {
				ioErr = errno
			}
		}
		if !grew {
			break
		}
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
