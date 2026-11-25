package store

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// fullGraphSeedIndexReadySQL reads whether idx_context_scope_active stands
// as the baseline builds it (valid, ready, the deparsed definition of
// migration 113), the index every arm of fullGraphSeedMergeSQL walks
// (issue #536). Without it an arm has no path to its scope's newest blocks
// that needs no sort, and with full sorts off it walked idx_context_created
// through the corpus, one walk per arm: at 1 000 000 rows 52 596 buffers for
// a read scope of 300 behind newer blocks (16 on the planned form), 367 540
// (3.3 s) for 20 small scopes. The schema-contract manifest reports such a
// database; until the index stands FullGraph reads every seed on the planned
// form.
const fullGraphSeedIndexReadySQL = `SELECT EXISTS (SELECT 1 FROM pg_index
	WHERE indexrelid = to_regclass('public.idx_context_scope_active') AND indisvalid AND indisready
	  AND pg_get_indexdef(indexrelid) = 'CREATE INDEX idx_context_scope_active ON public.context_blocks USING btree (scope, created_at DESC) WHERE (NOT is_archived)')`

// fullGraphSeedIndexReading is one reading of fullGraphSeedIndexReadySQL.
type fullGraphSeedIndexReading struct {
	ready bool
	at    time.Time
}

// fullGraphSeedIndexReadings holds the last reading per database
// (databaseKey), trusted for listIndexProbeInterval as the readings of the
// indexes of 195 are.
var fullGraphSeedIndexReadings = struct {
	sync.Mutex
	m map[string]fullGraphSeedIndexReading
}{m: map[string]fullGraphSeedIndexReading{}}

// fullGraphSeedIndexReady reports whether idx_context_scope_active stands,
// from the process's reading of the last listIndexProbeInterval for the
// database behind pool, else from a new reading.
func fullGraphSeedIndexReady(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	key, keep := databaseKey(pool)
	if keep {
		fullGraphSeedIndexReadings.Lock()
		r, ok := fullGraphSeedIndexReadings.m[key]
		fullGraphSeedIndexReadings.Unlock()
		if ok && time.Since(r.at) < listIndexProbeInterval {
			return r.ready, nil
		}
	}
	var ready bool
	if err := pool.QueryRow(ctx, fullGraphSeedIndexReadySQL).Scan(&ready); err != nil {
		return false, fmt.Errorf("store: graph seed index probe: %w", err)
	}
	if keep {
		fullGraphSeedIndexReadings.Lock()
		fullGraphSeedIndexReadings.m[key] = fullGraphSeedIndexReading{ready: ready, at: time.Now()}
		fullGraphSeedIndexReadings.Unlock()
	}
	return ready, nil
}

// indexProbeLogged is when indexProbeFailed last wrote its line (unix nanos).
var indexProbeLogged atomic.Int64

// indexProbeFailed decides what a failed index probe of a read means (issue
// #536, review of the pull request): the probes of the issue search and of
// FullGraph's seed only choose between two forms of a statement that return
// the same rows, so a probe that fails for a reason of its own — a role that
// may not read pg_index, a timeout on the catalog — does not fail the read:
// nil, and the caller takes the form that needs no index reading (the
// statement before #536). The line is written once per
// listIndexProbeInterval. A cancelled or expired request stays an error; err
// nil stays nil.
func indexProbeFailed(ctx context.Context, err error, what string) error {
	if err == nil || ctx.Err() != nil {
		return err
	}
	now := time.Now().UnixNano()
	if last := indexProbeLogged.Load(); now-last >= int64(listIndexProbeInterval) && indexProbeLogged.CompareAndSwap(last, now) {
		slog.Warn("store: index probe failed; the read goes on with the form that needs no index reading", "read", what, "error", err)
	}
	return nil
}
