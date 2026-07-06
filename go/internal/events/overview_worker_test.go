package events

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/overview"
)

// helperEnv routes TestOverviewWorkerHelperProcess into a worker stand-in
// when the tests below re-exec the test binary (the os/exec helper-process
// pattern). Unset in a normal run — the helper then skips.
const helperEnv = "CTX_TEST_OVERVIEW_WORKER_HELPER"

// hangPidFileEnv tells the "hang" helper where to advertise its pid — the
// E-B kill probe needs it to assert the child is DEAD (not just abandoned)
// after the context kill.
const hangPidFileEnv = "CTX_TEST_OVERVIEW_WORKER_PIDFILE"

// workerHelperArgv re-execs THIS test binary constrained to the helper test —
// a real child process with real stdin/stdout pipes, no DB and no ctxd binary
// needed.
func workerHelperArgv() []string {
	return []string{os.Args[0], "-test.run=^TestOverviewWorkerHelperProcess$"}
}

// TestOverviewWorkerHelperProcess is not a test: it is the child-process body
// the spawn tests exec. mode "echo" speaks the real E-A protocol (decodes
// Options from stdin, answers with a Stats derived from them, so the test can
// prove the values crossed BOTH pipes); mode "fail" simulates a worker that
// started and then died. os.Exit keeps the test framework's "PASS" off the
// stats stdout.
func TestOverviewWorkerHelperProcess(t *testing.T) {
	switch os.Getenv(helperEnv) {
	case "":
		t.Skip("helper process body — only meaningful when spawned by the worker-path tests")
	case "echo":
		opts, err := overview.DecodeWorkerOptions(os.Stdin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "helper: %v\n", err)
			os.Exit(1)
		}
		_ = overview.EncodeWorkerStats(os.Stdout, overview.Stats{
			NodeCount:    opts.MaxNodes,
			ClusterCount: len(opts.VisibleTypes),
			Modularity:   opts.Resolution,
		})
		os.Exit(0)
	case "fail":
		fmt.Fprintln(os.Stderr, "helper: deliberate worker failure")
		os.Exit(1)
	case "hang":
		// E-B kill-probe body: a worst-case child that ignores SIGTERM (the
		// real worker mid-Louvain handles no signals either), advertises its
		// pid, then hangs forever — only SIGKILL ends it.
		signal.Ignore(syscall.SIGTERM)
		if pf := os.Getenv(hangPidFileEnv); pf != "" {
			_ = os.WriteFile(pf, []byte(strconv.Itoa(os.Getpid())), 0o600)
		}
		select {}
	}
	os.Exit(2)
}

// TestRunOverviewRebuild_WorkerSpawnRoundtrip proves the daemon-side spawn
// mechanics end to end against a real child process: Options cross stdin,
// Stats cross stdout, and the worker result is returned verbatim — without
// the pool ever being touched (s.pool is nil; any in-process rebuild would
// error on the empty allowlists instead of echoing).
func TestRunOverviewRebuild_WorkerSpawnRoundtrip(t *testing.T) {
	t.Setenv(helperEnv, "echo")
	s := &Scheduler{}
	s.SetOverviewWorkerArgv(workerHelperArgv())

	opts := overview.Options{
		Resolution:    1.5,
		VisibleTypes:  []string{"knowledge", "issue"},
		OverviewTypes: []string{"knowledge"},
		MaxNodes:      4242,
	}
	stats, err := s.executeOverviewRebuild(context.Background(), opts)
	if err != nil {
		t.Fatalf("worker roundtrip: %v", err)
	}
	if stats.NodeCount != 4242 || stats.ClusterCount != 2 || stats.Modularity != 1.5 {
		t.Fatalf("stats did not cross the process boundary intact: %+v (want NodeCount=4242 ClusterCount=2 Modularity=1.5)", stats)
	}
}

// TestRunOverviewRebuild_FallsBackWhenNotStartable is the E-A fallback gate:
// a worker argv that cannot be STARTED degrades to the in-process path with a
// WARN. Reaching overview.Rebuild is observable without a DB: the empty type
// allowlists produce its distinctive wiring error BEFORE any pool use (a nil
// pool would panic past that point — a fallback that skipped validation could
// not sneak through).
func TestRunOverviewRebuild_FallsBackWhenNotStartable(t *testing.T) {
	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	defer slog.SetDefault(prev)

	s := &Scheduler{}
	s.SetOverviewWorkerArgv([]string{"/nonexistent/ctx-overview-worker-e-a"})

	_, err := s.executeOverviewRebuild(context.Background(), overview.Options{})
	if err == nil || !strings.Contains(err.Error(), "empty type allowlist") {
		t.Fatalf("in-process fallback not reached — want the Rebuild allowlist error, got: %v", err)
	}
	if !strings.Contains(logBuf.String(), "not startable") {
		t.Fatalf("fallback ran without the WARN log; captured: %s", logBuf.String())
	}
}

// TestRunOverviewRebuild_WorkerFailureIsNoFallback pins the fallback
// DISCRIMINATOR: a worker that started and then failed is a rebuild failure,
// never silently retried in-process (the failure would recur and double the
// Louvain bill). The empty allowlists would make an accidental in-process run
// unmistakable.
func TestRunOverviewRebuild_WorkerFailureIsNoFallback(t *testing.T) {
	t.Setenv(helperEnv, "fail")
	s := &Scheduler{}
	s.SetOverviewWorkerArgv(workerHelperArgv())

	_, err := s.executeOverviewRebuild(context.Background(), overview.Options{})
	if err == nil {
		t.Fatal("started-then-failed worker returned nil error")
	}
	if strings.Contains(err.Error(), "empty type allowlist") {
		t.Fatalf("in-process fallback ran after a STARTED worker failed: %v", err)
	}
	if !strings.Contains(err.Error(), "deliberate worker failure") {
		t.Fatalf("worker stderr missing from the failure: %v", err)
	}
}

// TestRebuildViaWorker_TimeoutKillsHangingChild is the E-B kill gate
// (design/05 §4.7: "Kind tot binnen Frist, kein Zombie, Loop lebt"): a
// SIGTERM-deaf, forever-hanging child is SIGKILLed when the rebuild context
// ends (CommandContext Cancel = Process.Kill — no ignorable grace signal),
// Wait returns promptly (reaping the child: no zombie), and the spawn
// mechanics stay usable for the next rebuild (the scheduler loop lives).
func TestRebuildViaWorker_TimeoutKillsHangingChild(t *testing.T) {
	t.Setenv(helperEnv, "hang")
	pidFile := filepath.Join(t.TempDir(), "worker.pid")
	t.Setenv(hangPidFileEnv, pidFile)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type result struct {
		started bool
		err     error
	}
	resCh := make(chan result, 1)
	go func() {
		_, started, err := rebuildViaWorker(ctx, workerHelperArgv(), overview.Options{})
		resCh <- result{started, err}
	}()

	// The pid file is the sync point: once it exists the child is up and
	// provably inside its hang (it writes the file right before select{}).
	var pid int
	for deadline := time.Now().Add(10 * time.Second); ; {
		if b, err := os.ReadFile(pidFile); err == nil && len(b) > 0 {
			if p, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				pid = p
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("hanging child never advertised its pid")
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel() // the rebuild_timeout stand-in (same mechanism: ctx end)

	select {
	case res := <-resCh:
		if !res.started {
			t.Fatal("kill path misreported started=false — the caller would wrongly fall back in-process")
		}
		if res.err == nil || !strings.Contains(res.err.Error(), "killed") {
			t.Fatalf("want a signal: killed failure, got: %v", res.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Wait did not return within 3s of ctx cancel — SIGTERM-deaf child not SIGKILLed?")
	}

	// No zombie, no survivor: Wait reaped the child, so its pid is GONE
	// (a zombie would still answer kill(pid, 0) with success).
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("child pid %d still exists after Wait returned (zombie/orphan): kill(pid,0) = %v", pid, err)
	}

	// Loop lives: the very next spawn over the same mechanics succeeds.
	t.Setenv(helperEnv, "echo")
	s := &Scheduler{}
	s.SetOverviewWorkerArgv(workerHelperArgv())
	stats, err := s.executeOverviewRebuild(context.Background(), overview.Options{MaxNodes: 7})
	if err != nil || stats.NodeCount != 7 {
		t.Fatalf("spawn after kill broken (loop would be dead): stats=%+v err=%v", stats, err)
	}
}

// TestRunOverviewRebuild_NilArgvStaysInProcess pins the zero value: without
// SetOverviewWorkerArgv the rebuild goes straight in-process — the pre-E-A
// behaviour every existing scheduler test (and the boot-wiring failure path)
// relies on. No spawn attempt ⇒ no WARN.
func TestRunOverviewRebuild_NilArgvStaysInProcess(t *testing.T) {
	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	defer slog.SetDefault(prev)

	s := &Scheduler{}
	_, err := s.executeOverviewRebuild(context.Background(), overview.Options{})
	if err == nil || !strings.Contains(err.Error(), "empty type allowlist") {
		t.Fatalf("in-process path not reached with nil argv: %v", err)
	}
	if strings.Contains(logBuf.String(), "not startable") {
		t.Fatal("nil argv must not log a spawn-fallback WARN")
	}
}
