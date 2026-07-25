package rrf

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Stage 2 of the cardinality estimation (design/02-strategy-selektor.md
// §4.3b): a TTL-refreshed pg_stats snapshot, held in an atomic.Pointer and
// filled from ONE catalog read.
//
// Why this stage may be cheap AND wrong: it only ever gates the GREY branch,
// whose effect is a scan BUDGET that is its own ceiling — a misclassification
// costs at most GreyScanTuples tuple visits. Both error directions are
// consequence-bounded: overestimate → plain ann (Ist behaviour), underestimate
// → grey budget on a large scope, capped by the budget itself. The EXACT
// branch never reads this stage; it is gated by the bounded probe, which is
// exact up to its cap and has zero staleness (§4.3a).
//
// The snapshot is ONE process-wide cache for all tenants. That is not a
// tenancy leak: it holds scope NAMES × frequencies from the shared planner
// catalog — the same rows every DB user sees — and no tenant datum, no block
// id, no content (§5.5).

// defaultStatsTTL is the fallback refresh interval when the policy carries a
// non-positive StatsTTL. Not a §5.4 clamp (no warn, no policy mutation): a
// zero TTL would mean "refresh on every search AND consider every snapshot
// older than 0 stale", i.e. a permanently unusable stage. The value mirrors
// the shipped default of retrieval.selector.stats_ttl.
const defaultStatsTTL = 60 * time.Second

// statsStaleFactor is the §4.3b hard staleness bound: a snapshot older than
// this multiple of the TTL counts as unusable (reason stats_stale → plain
// ann), even though a refresh was attempted. It is the visible consequence of
// a persistently failing catalog read — the cache keeps serving the last
// snapshot across a transient failure, but never indefinitely.
const statsStaleFactor = 10

// estimateCeil caps the returned estimate. reltuples is a float4 and can carry
// astronomic values on a corrupted/absurd catalog; the estimate is compared
// against GreyMax (an int) and logged, so it must not overflow int on any
// platform.
const estimateCeil = math.MaxInt32

// scopeStatsRow is the raw §4.3b catalog row, untouched by interpretation —
// exactly the four values the SQL selects, in PG's own types (float4 arrays
// and scalars). Splitting the READ from the INTERPRETATION is what makes the
// edge-case handling (gate G3) unit-testable without a database.
type scopeStatsRow struct {
	mcVals    []string  // most_common_vals::text::text[] (NULL → nil)
	mcFreqs   []float32 // most_common_freqs (parallel to mcVals)
	nDistinct float32   // pg_stats.n_distinct — negative values are RATIOS
	reltuples float32   // pg_class.reltuples — negative means "never analysed"
}

// statsReader performs the catalog read. Injected for the same reason as
// selectorProbe: the snapshot logic and every §4.3b edge case stay DB-free
// testable. found=false means the column has no pg_stats row at all (never
// analysed, or no statistics for this attribute).
type statsReader func(ctx context.Context) (row scopeStatsRow, found bool, err error)

// scopeStatsSQL is the §4.3b catalog read, verbatim.
//
// Schema qualification and the OID join are mandatory, not cosmetic:
// pg_class.relname is not unique across namespaces — this database shares its
// cluster with n8n and TimescaleDB creates internal schemata, so a
// same-named relation elsewhere would silently duplicate the row or supply
// foreign reltuples. n_distinct is selected because the residual formula
// needs it.
const scopeStatsSQL = `
SELECT s.most_common_vals::text::text[], s.most_common_freqs, s.n_distinct, c.reltuples
FROM pg_stats s
JOIN pg_class c ON c.oid = 'public.context_blocks'::regclass
WHERE s.schemaname = 'public' AND s.tablename = 'context_blocks' AND s.attname = 'scope'`

// poolStatsReader binds the catalog read to a pool.
func poolStatsReader(pool *pgxpool.Pool) statsReader {
	return func(ctx context.Context) (scopeStatsRow, bool, error) {
		var row scopeStatsRow
		err := pool.QueryRow(ctx, scopeStatsSQL).
			Scan(&row.mcVals, &row.mcFreqs, &row.nDistinct, &row.reltuples)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return row, false, nil
			}
			return row, false, err
		}
		return row, true, nil
	}
}

// statsSnapshot is the INTERPRETED catalog state: per-MCV-scope row estimates
// plus the residual estimate that every non-listed scope gets. An invalid
// snapshot (valid=false) is kept rather than discarded — it records that the
// read happened and failed its validity checks, so the cache does not hammer
// the catalog once per search.
type statsSnapshot struct {
	// owner is the pool the snapshot was read from. A process holds exactly
	// one pool in production, so this never changes there; in tests every
	// case runs its own container, and without this guard a snapshot from
	// database A would silently answer questions about database B.
	owner     any
	fetchedAt time.Time
	valid     bool
	mcv       map[string]float64 // scope → estimated live rows
	residual  float64            // estimated live rows of a non-MCV scope
	// hasResidual is false when the residual denominator (normalised
	// n_distinct − |MCV|) is not positive. Then, and only then, a scope
	// OUTSIDE the MCV list is unanswerable — see estimate.
	hasResidual bool
}

// parseScopeStats maps a raw catalog row onto a snapshot, applying every
// §4.3b validity rule. Every rejection lands on valid=false, which the
// dispatch turns into reason stats_stale → plain ann (the Ist path) — the
// safe direction by construction.
//
// Rejection rules, in order:
//
//  1. reltuples < 0: PG14+ writes -1 for "never analysed" (fresh database
//     before the first autovacuum, and the state right after a pg_restore
//     that carried no planner statistics). Any estimate derived from it
//     would be fiction.
//  2. mcVals/mcFreqs length mismatch: the catalog does not produce this;
//     if it ever does, the frequency-to-scope mapping is meaningless.
//  3. normalised n_distinct <= |MCV| (residual denominator <= 0). This also
//     absorbs n_distinct = 0 ("unknown").
//
// DEVIATION from §4.3b, deliberate: the design lists rule 3 as a rejection of
// the whole SNAPSHOT. Applied literally it disables stage 2 in exactly the
// state migration 117 is built to produce. Measured (W02-4 gate G4): a
// 150-scope corpus at statistics target 1000 reports n_distinct = -0.25 over
// 600 rows — normalised 150 — with all 150 scopes in the MCV list, i.e.
// n_distinct == |MCV| whenever the target succeeds in covering the corpus.
// The rule's PURPOSE is to protect the residual division, and that division
// only ever runs for a scope OUTSIDE the MCV list. So rule 3 is enforced
// exactly there (hasResidual, see estimate): an MCV scope keeps its exact
// frequency-derived estimate, a non-MCV scope without a usable denominator
// yields stats_stale → plain ann. Neither a fabricated residual nor a
// division by a non-positive denominator can occur — the safety property the
// rule encodes is intact, its collateral damage is not.
//
// The NEGATIVE n_distinct is NOT a rejection — it is the ratio form ANALYZE
// chooses when the distinct count grows with the table (the multi-tenant
// case, and empirically already the Ist shape). It is normalised via
// |n_distinct| × reltuples first.
func parseScopeStats(row scopeStatsRow, owner any, at time.Time) *statsSnapshot {
	snap := &statsSnapshot{owner: owner, fetchedAt: at}

	reltuples := float64(row.reltuples)
	if reltuples < 0 {
		slog.Debug("rrf selector: pg_stats snapshot unusable, never analysed", "reltuples", reltuples)
		return snap
	}
	if len(row.mcVals) != len(row.mcFreqs) {
		slog.Warn("rrf selector: pg_stats snapshot unusable, MCV arrays disagree",
			"vals", len(row.mcVals), "freqs", len(row.mcFreqs))
		return snap
	}

	snap.mcv = make(map[string]float64, len(row.mcVals))
	var freqSum float64
	for i, v := range row.mcVals {
		f := float64(row.mcFreqs[i])
		freqSum += f
		snap.mcv[v] = f * reltuples
	}
	snap.valid = true

	nDistinct := float64(row.nDistinct)
	if nDistinct < 0 {
		nDistinct = -nDistinct * reltuples
	}
	unlisted := nDistinct - float64(len(row.mcVals))
	if unlisted <= 0 {
		slog.Debug("rrf selector: pg_stats snapshot without a residual, MCV-only",
			"n_distinct_normalised", nDistinct, "mcv", len(row.mcVals))
		return snap
	}

	// Residual (§4.3b): the rows NOT covered by the MCV list, spread evenly
	// over the distinct values that are not in it. Float noise can push
	// freqSum marginally past 1 — clamp at 0 instead of producing a negative
	// estimate (which would make every unlisted scope look empty).
	residual := reltuples * (1 - freqSum) / unlisted
	if residual < 0 {
		residual = 0
	}
	snap.residual = residual
	snap.hasResidual = true
	return snap
}

// estimate sums the snapshot over the requested scopes (§4.3b): MCV scopes
// contribute frequency × reltuples, every other scope the residual.
//
// FLOOR ExactMax+1: the caller only reaches this stage AFTER the bounded
// probe counted more than ExactMax rows across exactly these scopes — the
// scope set is PROVEN bigger than that, whatever the sampled statistics
// claim. The floor is applied to the SUM, not per scope: the probe proved the
// size of the set, not of each member.
//
// Duplicate scopes in the input are summed as given — deduplication is the
// caller's business (the read-scope resolution never produces duplicates).
func (s *statsSnapshot) estimate(scopes []string, exactMax int) (int, bool) {
	if s == nil || !s.valid {
		return 0, false
	}
	var est float64
	for _, sc := range scopes {
		if rows, ok := s.mcv[sc]; ok {
			est += rows
			continue
		}
		if !s.hasResidual {
			// §4.3b rule 3, applied where it bites: no usable residual
			// denominator AND a scope outside the MCV list → no estimate at
			// all (stats_stale → plain ann), rather than an invented one.
			return 0, false
		}
		est += s.residual
	}
	if est > estimateCeil {
		est = estimateCeil
	}
	n := int(math.Round(est))
	if floor := exactMax + 1; n < floor {
		return floor, true
	}
	return n, true
}

// statsCache is the process-wide snapshot holder. Reads are lock-free
// (atomic.Pointer); a refresh is single-flighted with TryLock, so a second
// concurrent search never QUEUES behind another's catalog roundtrip — it
// serves the slightly older snapshot instead. For a consequence-bounded
// budget heuristic that trade is free; blocking a query path on a lock is not.
type statsCache struct {
	mu   sync.Mutex
	snap atomic.Pointer[statsSnapshot]
}

// scopeStatsCache is THE snapshot for this process (§5.5: scope names ×
// frequencies, no tenant state — one cache is correct, not a shortcut).
var scopeStatsCache statsCache

// estimate is the statsEstimator entry point: refresh if the snapshot is
// missing, foreign, or older than the TTL, then answer from it.
//
// Failure handling: a failing refresh keeps the previous snapshot (a transient
// catalog error must not cost the strategy) but does not reset its age — the
// statsStaleFactor bound eventually turns a persistently failing refresh into
// stats_stale, i.e. into the Ist path. Same degradation doctrine as the probe
// (§5.3, N5 muster).
func (c *statsCache) estimate(ctx context.Context, owner any, read statsReader, scopes []string, exactMax int, ttl time.Duration) (int, bool) {
	if ttl <= 0 {
		ttl = defaultStatsTTL
	}
	now := time.Now()

	snap := c.snap.Load()
	if snap == nil || snap.owner != owner || now.Sub(snap.fetchedAt) > ttl {
		c.refresh(ctx, owner, read)
		snap = c.snap.Load()
	}
	if snap == nil || snap.owner != owner {
		return 0, false
	}
	if age := time.Since(snap.fetchedAt); age > statsStaleFactor*ttl {
		slog.Warn("rrf selector: pg_stats snapshot beyond the staleness bound, falling back to ann",
			"age_s", age.Seconds(), "ttl_s", ttl.Seconds())
		return 0, false
	}
	return snap.estimate(scopes, exactMax)
}

// refresh performs at most one catalog read at a time (TryLock); concurrent
// callers return immediately and use whatever is currently published.
func (c *statsCache) refresh(ctx context.Context, owner any, read statsReader) {
	if !c.mu.TryLock() {
		return
	}
	defer c.mu.Unlock()

	row, found, err := read(ctx)
	if err != nil {
		slog.Warn("rrf selector: pg_stats catalog read failed, keeping the previous snapshot",
			"error", err)
		return
	}
	if !found {
		// No stats row for context_blocks.scope at all — an empty snapshot,
		// deliberately PUBLISHED (not skipped) so the next search does not
		// re-read the catalog before the TTL elapses.
		c.snap.Store(&statsSnapshot{owner: owner, fetchedAt: time.Now()})
		return
	}
	c.snap.Store(parseScopeStats(row, owner, time.Now()))
}
