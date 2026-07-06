package events

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/overview"
)

// helperEnv routes TestOverviewWorkerHelperProcess into a worker stand-in
// when the tests below re-exec the test binary (the os/exec helper-process
// pattern). Unset in a normal run — the helper then skips.
const helperEnv = "CTX_TEST_OVERVIEW_WORKER_HELPER"

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
