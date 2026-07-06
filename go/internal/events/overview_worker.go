// Wave E-A (plan-inference-scheduler design/05 §4.7): the scheduler routes
// the overview rebuild through a worker CHILD PROCESS (the hidden
// `ctxd overview-rebuild-worker` subcommand of the own binary) so the
// minutes-long single-threaded Louvain compute stops living inside ctxd.
// This file owns the daemon side of the boundary: spawn, IPC, and the
// in-process fallback when the child cannot be STARTED.
//
// E-A is result-neutral by contract: fixed seeds + ordered loads make the
// worker's partition identical to the in-process one (pinned by the overview
// worker integration roundtrip). Wave E-B adds the OS layer on top — nice/
// ionice on the spawn and the hard kill on rebuild_timeout that structurally
// retires the documented in-process Modularize goroutine leak.
package events

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"

	"github.com/GottZ/ctx/internal/overview"
)

// workerStderrTailMax caps how much child stderr rides along in errors/logs —
// enough for the worker's slog JSON diagnostics, never an unbounded blob.
const workerStderrTailMax = 4096

// SetOverviewWorkerArgv installs the argv the overview rebuild spawns as its
// worker child process (production: {os.Executable(), overview.WorkerCommand},
// wired in cmd/ctxd). MUST be called at boot, before Run — the field is
// written without synchronization, relying on the boot happens-before (the
// SetBlocktypeRegistry pattern). nil (the zero value) keeps every rebuild
// in-process: the pre-E-A behaviour, and the target the not-startable
// fallback degrades to.
func (s *Scheduler) SetOverviewWorkerArgv(argv []string) {
	s.overviewWorkerArgv = argv
}

// executeOverviewRebuild runs one rebuild, through the worker child process when
// one is wired and startable, in-process otherwise.
//
// Fallback semantics (E-A contract): the in-process path is the fallback for
// a worker that cannot be STARTED (missing binary, exec denied, argv rot) —
// WARN + identical result via overview.Rebuild. A worker that started and
// then FAILED is a rebuild failure like any other and is NOT retried
// in-process: the failure (bad options, DB down, timeout kill) would recur
// there and double the Louvain bill for nothing.
func (s *Scheduler) executeOverviewRebuild(ctx context.Context, opts overview.Options) (overview.Stats, error) {
	argv := s.overviewWorkerArgv
	if len(argv) == 0 {
		return overview.Rebuild(ctx, s.pool, opts)
	}
	stats, started, err := rebuildViaWorker(ctx, argv, opts)
	if err != nil && !started {
		slog.Warn("scheduler: overview worker not startable — falling back to in-process rebuild",
			"error", err, "argv0", argv[0])
		return overview.Rebuild(ctx, s.pool, opts)
	}
	return stats, err
}

// rebuildViaWorker spawns one worker child, feeds it the Options JSON on
// stdin and decodes the Stats JSON from its stdout. started reports whether
// the child process came up — the fallback discriminator: false means the
// spawn itself failed (caller may fall back), true means the rebuild ran and
// its outcome is authoritative.
//
// The child runs under ctx (exec.CommandContext): the caller's
// rebuild_timeout kills it instead of orphaning it — where the in-process
// path leaks an unstoppable Modularize goroutine, the process boundary is
// already hard-cancelable. E-B builds on exactly this edge (nice/ionice +
// the kill/rollback probes); Wait() reaps the child on every path, so no
// zombie survives here either.
func rebuildViaWorker(ctx context.Context, argv []string, opts overview.Options) (overview.Stats, bool, error) {
	var input bytes.Buffer
	if err := overview.EncodeWorkerOptions(&input, opts); err != nil {
		return overview.Stats{}, false, err
	}

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // argv is boot-wired (os.Executable + shared const), never request input
	cmd.Stdin = &input
	var stdout, stderrBuf bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderrBuf
	// Env deliberately not set: the child inherits the daemon's environment,
	// which carries the CONTEXT_DB_* DSN the worker rebuilds against
	// (design/05 §4.7: "DB via bestehende Env-DSN").

	if err := cmd.Start(); err != nil {
		return overview.Stats{}, false, fmt.Errorf("starting overview worker: %w", err)
	}
	if err := cmd.Wait(); err != nil {
		return overview.Stats{}, true, fmt.Errorf("overview worker failed: %w (stderr: %s)", err, stderrTail(&stderrBuf))
	}
	stats, err := overview.DecodeWorkerStats(&stdout)
	if err != nil {
		return overview.Stats{}, true, fmt.Errorf("overview worker succeeded but stats are unreadable: %w (stderr: %s)", err, stderrTail(&stderrBuf))
	}
	if stderrBuf.Len() > 0 {
		slog.Debug("scheduler: overview worker stderr", "stderr", stderrTail(&stderrBuf))
	}
	return stats, true, nil
}

// stderrTail returns the last workerStderrTailMax bytes of the child's stderr
// — the end carries the failure, the head just boot noise.
func stderrTail(buf *bytes.Buffer) string {
	b := buf.Bytes()
	if len(b) > workerStderrTailMax {
		b = b[len(b)-workerStderrTailMax:]
	}
	return string(bytes.TrimSpace(b))
}
