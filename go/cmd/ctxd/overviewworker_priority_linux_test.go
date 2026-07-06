package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/overview"
)

// priorityHelperEnv routes TestOverviewWorkerPriorityHelperProcess into the
// helper body when the /proc probe below re-execs the test binary. Unset in a
// normal run — the helper then skips.
const priorityHelperEnv = "CTX_TEST_WORKER_PRIORITY_HELPER"

// TestOverviewWorkerPriorityHelperProcess is not a test: it is the child body
// of the E-B /proc probe. It routes through the REAL production entry —
// dispatchOverviewWorkerMode keys on os.Args[1], deprioritizes, then blocks in
// the worker's options decode while the parent holds stdin open (and os.Exits
// on EOF). Running the entry itself, not deprioritizeSelf directly, is what
// makes this a CALL-PATH probe: dropping the deprioritizeSelf call from the
// entry turns the parent's /proc assert red, not just breaking the function.
func TestOverviewWorkerPriorityHelperProcess(t *testing.T) {
	if os.Getenv(priorityHelperEnv) == "" {
		t.Skip("helper process body — only meaningful when spawned by the priority probe")
	}
	os.Args = []string{os.Args[0], overview.WorkerCommand}
	dispatchOverviewWorkerMode()
	fmt.Println("dispatch returned") // unreachable on the worker path (os.Exit)
	os.Exit(3)
}

// procNice reads the nice value of pid from /proc/<pid>/stat. The comm field
// (2) may contain spaces, so parsing anchors on the LAST ')': after it the
// fields are state(3) … priority(18) nice(19) — nice is index 16 of the rest.
func procNice(t *testing.T, pid int) int {
	t.Helper()
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		t.Skipf("environment without readable /proc/<pid>/stat: %v", err)
	}
	rest := string(raw)
	if i := strings.LastIndexByte(rest, ')'); i >= 0 {
		rest = rest[i+1:]
	}
	fields := strings.Fields(rest)
	if len(fields) < 17 {
		t.Fatalf("unparseable /proc/%d/stat tail: %q", pid, rest)
	}
	nice, err := strconv.Atoi(fields[16])
	if err != nil {
		t.Fatalf("nice field of /proc/%d/stat is not a number: %v", pid, err)
	}
	return nice
}

// TestOverviewWorkerSelfDeprioritization_Proc is the E-B priority gate
// (design/05 §4.7: "nice-Level im Kind per /proc verifiziert"): a child that
// enters through the production worker dispatch must show nice 19 in
// /proc/<pid>/stat and the idle I/O class via an ioprio_get readback on its
// pid. The child blocks in the options decode while stdin stays open —
// deprioritizeSelf runs strictly before that read, so polling /proc up to a
// deadline is race-free against everything but a missing call.
//
// The nice assert is HARD — lowering priority needs no privilege and cannot
// legitimately fail. The ioprio assert is soft-skipped only when the
// ioprio_get syscall itself is denied (an exotic sandbox), matching the
// production WARN-not-fatal contract.
func TestOverviewWorkerSelfDeprioritization_Proc(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestOverviewWorkerPriorityHelperProcess$") //nolint:gosec // re-exec of the own test binary
	cmd.Env = append(os.Environ(), priorityHelperEnv+"=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting helper: %v", err)
	}
	defer func() {
		_ = stdin.Close()
		_ = cmd.Wait() // decode EOF ⇒ worker exit 1; the wait only reaps
	}()

	// Both values are polled up to the deadline: the child sets nice first
	// and ioprio a beat later, so a single read right after the nice flip
	// can still see the default I/O class (a real race, seen once under
	// full-suite load). Only a value that never arrives fails.
	deadline := time.Now().Add(5 * time.Second)
	nice := procNice(t, cmd.Process.Pid)
	for nice != 19 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		nice = procNice(t, cmd.Process.Pid)
	}
	if nice != 19 {
		t.Fatalf("worker child runs at nice %d, want 19 — deprioritizeSelf missing from the dispatch entry, or broken", nice)
	}

	want := ioprioClassIdle << ioprioClassShift
	ioprio, err := ioprioGet(cmd.Process.Pid)
	for err == nil && ioprio != want && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		ioprio, err = ioprioGet(cmd.Process.Pid)
	}
	if err != nil {
		t.Skipf("environment denies ioprio_get on the child — nice 19 verified, ioprio unverifiable here: %v", err)
	}
	if ioprio != want {
		t.Fatalf("worker child ioprio = %d, want %d (idle class)", ioprio, want)
	}
}
