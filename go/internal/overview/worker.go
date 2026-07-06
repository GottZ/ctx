// Worker IPC for wave E-A (plan-inference-scheduler design/05 §4.7): the
// Louvain rebuild — minutes of single-threaded gonum CPU at the target scale —
// moves out of the ctxd process behind a process boundary. The ctx binary
// grows a hidden subcommand (`ctxd overview-rebuild-worker`, cmd/ctxd) that
// the scheduler spawns as a child process (internal/events); this file is the
// protocol both sides speak.
//
// Protocol: the parent writes ONE Options JSON document to the child's stdin;
// the child runs Rebuild against the env-DSN database and writes ONE Stats
// JSON document to stdout. The exit code carries success (0 = Stats on stdout
// are valid, including Skipped runs; ≠0 = failure, diagnostics on stderr).
// The wire format is the exported Go field names of Options/Stats — both ends
// are the same binary by construction, so no versioned envelope is needed.
//
// Result neutrality across the boundary rests on the existing determinism
// axes (fixed Louvain seeds, ORDER BY loads, sorted edge insertion): the same
// DB input yields the same partition whether clustered in-process or in the
// worker — pinned by the worker integration roundtrip test.
//
// Both decoders are STRICT (DisallowUnknownFields): an unknown field means
// protocol drift (e.g. a mixed-version binary window) and must fail loudly —
// the daemon then falls back to the in-process path instead of clustering
// with silently dropped options.
package overview

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5/pgxpool"
)

// WorkerCommand is the argv[1] literal that routes the ctx binary into the
// hidden overview-rebuild worker mode (cmd/ctxd main dispatch, the -health /
// -secret-decrypt precedent). Shared constant so spawner and dispatcher can
// never drift apart.
const WorkerCommand = "overview-rebuild-worker"

// EncodeWorkerOptions writes the parent→child options document.
func EncodeWorkerOptions(w io.Writer, o Options) error {
	if err := json.NewEncoder(w).Encode(o); err != nil {
		return fmt.Errorf("encoding worker options: %w", err)
	}
	return nil
}

// DecodeWorkerOptions reads the parent→child options document (strict).
func DecodeWorkerOptions(r io.Reader) (Options, error) {
	var o Options
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&o); err != nil {
		return Options{}, fmt.Errorf("decoding worker options: %w", err)
	}
	return o, nil
}

// EncodeWorkerStats writes the child→parent result document.
func EncodeWorkerStats(w io.Writer, s Stats) error {
	if err := json.NewEncoder(w).Encode(s); err != nil {
		return fmt.Errorf("encoding worker stats: %w", err)
	}
	return nil
}

// DecodeWorkerStats reads the child→parent result document (strict).
func DecodeWorkerStats(r io.Reader) (Stats, error) {
	var s Stats
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return Stats{}, fmt.Errorf("decoding worker stats: %w", err)
	}
	return s, nil
}

// RunWorker is the child-process core: one rebuild with pre-decoded options,
// Stats JSON to stdout. Options decoding deliberately does NOT live here —
// cmd/ctxd decodes BEFORE it builds config or pool, so broken options JSON
// exits without a single DB connection (the "no mutation" half of the E-A
// gate is structural). Factored into the overview package so the integration
// roundtrip test can drive the exact production compute+persist+encode path
// against a real schema without forking a process.
func RunWorker(ctx context.Context, pool *pgxpool.Pool, opts Options, stdout io.Writer) error {
	stats, err := Rebuild(ctx, pool, opts)
	if err != nil {
		return err
	}
	return EncodeWorkerStats(stdout, stats)
}
