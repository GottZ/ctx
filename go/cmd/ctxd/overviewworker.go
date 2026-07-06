// Hidden overview-rebuild worker mode (wave E-A, plan-inference-scheduler
// design/05 §4.7): `ctxd overview-rebuild-worker` — the -health /
// -secret-decrypt precedent. The ctxd scheduler spawns this subcommand of
// its OWN binary as a child process so the minutes-long single-threaded
// Louvain compute runs behind a process boundary (E-B adds nice/ionice and
// the timeout kill on top).
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
	"github.com/GottZ/ctx/internal/store"
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

	// Env-DSN config (design/05 §4.7): validate like the daemon boot does —
	// the env is inherited from a daemon that already fail-fasted on real
	// config errors, so errors here mean a genuinely broken spawn context.
	cc, issues := config.FromEnv()
	issues = append(issues, config.Validate(cc)...)
	if config.HasErrors(issues) {
		for _, is := range issues {
			if is.Severity == config.SeverityError {
				_, _ = fmt.Fprintf(stderr, "overview-worker: config: %s: %s\n", is.Field, is.Msg)
			}
		}
		return 1
	}

	// SIGINT/SIGTERM abort the rebuild; the parent's timeout arrives as a
	// hard kill (exec.CommandContext), which the persist tx survives as a
	// clean rollback — E-B pins that with its own probes.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	pool, err := store.NewPool(ctx, cc.DSN())
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "overview-worker: db pool: %v\n", err)
		return 1
	}
	defer pool.Close()

	if err := overview.RunWorker(ctx, pool, opts, stdout); err != nil {
		_, _ = fmt.Fprintf(stderr, "overview-worker: %v\n", err)
		return 1
	}
	return 0
}
