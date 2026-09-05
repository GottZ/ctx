// Hidden overview-rebuild worker mode (waves E-A/E-B, plan-inference-scheduler
// design/05 §4.7): `ctxd overview-rebuild-worker` — the -health /
// -secret-decrypt precedent. The ctxd scheduler spawns this subcommand of
// its OWN binary as a child process so the minutes-long single-threaded
// Louvain compute runs behind a process boundary. E-B is the OS layer on that
// boundary: the worker self-deprioritizes to nice 19 + idle I/O class at
// entry (deprioritizeSelf), and the parent's rebuild_timeout arrives as a
// SIGKILL (events.rebuildViaWorker) that the persist tx survives as a clean
// rollback.
//
// Contract (overview.WorkerCommand IPC): ONE Options JSON document on stdin,
// ONE Stats JSON document on stdout, exit code carries success; the database
// comes from the inherited env DSN (CONTEXT_DB_*), never from argv. All
// diagnostics go to stderr — the parent captures and relays them.

package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/events"
	"github.com/GottZ/ctx/internal/overview"
	"github.com/GottZ/ctx/internal/toolboot"
)

// dispatchOverviewWorkerMode routes `ctxd overview-rebuild-worker` into the
// worker and NEVER RETURNS in that case (os.Exit — the -health precedent);
// any other argv is a no-op. slog goes to stderr so worker diagnostics reach
// the parent's capture. Extracted from main (the
// normalizeInterruptedSyncsBoot pattern) to keep it under the cyclop budget.
func dispatchOverviewWorkerMode() {
	if len(os.Args) > 1 && os.Args[1] == overview.WorkerCommand {
		slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelInfo,
		})))
		// E-B (design/05 §4.7): the worker deprioritizes ITSELF (nice 19 +
		// idle I/O class) instead of being spawned through external nice/
		// ionice binaries — none are guaranteed in the container image, and
		// lowering own priority needs no privilege. Failures WARN and never
		// abort: a full-priority rebuild beats none. Lives HERE (process
		// entry), not in runOverviewWorker — the in-process unit tests call
		// that function directly and must not renice the test binary.
		if niceErr, ioErr := deprioritizeSelf(); niceErr != nil || ioErr != nil {
			slog.Warn("overview-worker: OS deprioritization incomplete — continuing at current priority",
				"nice_error", niceErr, "ioprio_error", ioErr)
		}
		// S7b (Achse 04, SP-8): das Kind macht sich zum bevorzugten OOM-Opfer
		// und deckelt seinen eigenen Heap.
		//
		// Beides HIER, am Prozess-Eintritt, und nicht in runOverviewWorker:
		// die In-Process-Unit-Tests rufen jene Funktion direkt auf und dürfen
		// weder das Testbinary deckeln noch es zum OOM-Opfer erklären —
		// dieselbe Grenze, die deprioritizeSelf schon zieht.
		//
		// Die Reihenfolge ist load-bearing: oom_score_adj VOR dem
		// Speicherlimit. Reisst die cgroup, WÄHREND das Limit gesetzt wird,
		// soll der Kill bereits das Kind treffen und nicht den Daemon.
		if err := preferSelfForOOMKill(); err != nil {
			slog.Warn("overview-worker: oom_score_adj not writable — a cgroup OOM may hit the daemon instead of this child",
				"error", err)
		}
		limit, err := overview.WorkerMemLimitBytes()
		if err != nil {
			slog.Warn("overview-worker: memory budget ignored", "error", err)
		}
		if limit > 0 {
			overview.ApplyWorkerMemLimit(limit)
			slog.Info("overview-worker: self memory limit applied", "limit_bytes", limit)
		}
		os.Exit(runOverviewWorker(os.Stdin, os.Stdout, os.Stderr))
	}
}

// wireOverviewWorkerArgv installs the E-A worker argv (the own binary's
// hidden subcommand) on the scheduler — boot happens-before like
// SetBlocktypeRegistry. When the executable path cannot be resolved the argv
// stays nil and every rebuild stays in-process: the documented fallback,
// WARN-loud. Extracted from main for the cyclop budget.
func wireOverviewWorkerArgv(scheduler *events.Scheduler) {
	exe, err := os.Executable()
	if err != nil {
		slog.Warn("scheduler: overview worker disabled — cannot resolve own executable, rebuilds stay in-process", "error", err)
		return
	}
	scheduler.SetOverviewWorkerArgv([]string{exe, overview.WorkerCommand})
}

// runOverviewWorker implements the worker mode. Returns the process exit code.
//
// Order is load-bearing (the E-A negative gate): the options decode happens
// BEFORE any config load or pool creation, so broken options JSON exits 1
// without a single DB connection — "no DB mutation" is structural, not
// behavioural. Migrations do NOT run here: the spawning daemon migrated the
// schema at boot; the worker is always its contemporary (same binary).
func runOverviewWorker(stdin io.Reader, stdout, stderr io.Writer) int {
	opts, err := overview.DecodeWorkerOptions(stdin)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "overview-worker: options: %v\n", err)
		return 1
	}

	// SIGINT/SIGTERM abort the rebuild; the parent's timeout arrives as a
	// hard kill (exec.CommandContext), which the persist tx survives as a
	// clean rollback — E-B pins that with its own probes.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Env-DSN config (design/05 §4.7, contract G14 via internal/toolboot):
	// validate like the daemon boot does — the env is inherited from a daemon
	// that already fail-fasted on real config errors, so errors here mean a
	// genuinely broken spawn context. Only errors are worth a line, and only
	// when they are what ends this process; the daemon has already seen and
	// logged whatever warnings this env carries.
	sess, ok := toolboot.Open(ctx, func(issues []config.Issue, aborting bool) {
		if !aborting {
			return
		}
		for _, is := range issues {
			if is.Severity == config.SeverityError {
				_, _ = fmt.Fprintf(stderr, "overview-worker: config: %s: %s\n", is.Field, is.Msg)
			}
		}
	}, func(err error) {
		_, _ = fmt.Fprintf(stderr, "overview-worker: db pool: %v\n", err)
	})
	if !ok {
		return 1
	}
	defer sess.Stop()

	if err := overview.RunWorker(ctx, sess.Pool, opts, stdout); err != nil {
		_, _ = fmt.Fprintf(stderr, "overview-worker: %v\n", err)
		return 1
	}
	return 0
}
