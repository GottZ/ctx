package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"sort"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/embed"
	"github.com/GottZ/ctx/internal/embedmigration"
	"github.com/jackc/pgx/v5"
	pgvec "github.com/pgvector/pgvector-go"
)

// Achse 04 W04-5 (design/04-reembed-migration.md §4.6/§4.7): the verify
// gate of the re-embed migration statemachine. Triggered by the W04-4 arm's
// cycle when the row sits in `verifying` without a verdict for the current
// watermark; the arm keeps DRAINING while the gate runs (§4.1 drain
// semantics — the gate is a parallel one-shot, never a work stop).
//
// The five §4.7 stages and their execution shape here:
//
//  1. Vollständigkeit — the §4.7 SQL verbatim (watermark-scoped, NOT-EXISTS
//     exception for infinity memos of THIS migration) plus the finite-memo
//     rest count, the named skip list and the visibility_loss numbers.
//  2. Integrität — dims/norm/model of every migrated row, FOLDED into the
//     stage-4 table pass (Ein-Pass-Doktrin: embedding_next is unindexed
//     fp32 TOAST, ~41 GB at 10M — one sequential pass total, never two).
//  3. Index-Bau — CONCURRENTLY HNSW on embedding_next (canonical live
//     parameters m=16/ef_construction=128, Mig 115) + the three sister
//     partial indexes of the Attnum class (§4.8). Executed LAST: a CIC over
//     a bestand that stage 1/2 already proved defective would be hours of
//     wasted I/O at 10M.
//  4. Qualität — DEGRADED to Overlap@k (recall_check / Achse 01 does not
//     exist yet); informative, never red. Hook: runVerifyQualityStage.
//  5. Guard-Stichprobe (§4.6, E-04-4) — cosine distribution of known
//     non-duplicates in the NEW space vs. the strictest live guard
//     thresholds; report-only.
//
// Verdicts: red → CAS `verifying → paused` with the finding in
// verify_report + last_error — NEVER auto-abort (§4.1: the operator decides
// between retry, threshold change and abort). Green → verify_report stored,
// the row STAYS `verifying`; cutover is the operator confirm (W04-6/W04-7).

const (
	verifyGreen       = "green"
	verifyRed         = "red"
	verifySkipped     = "skipped"
	verifyInformative = "informative"

	// verifyMinNorm/verifyMaxNorm mirror the Go embed quality gate
	// (embed.go minNorm/maxNorm) — §4.7 Stufe 2: this re-check exists to
	// catch STORE corruption behind that gate, so the bounds must stay in
	// lockstep with it.
	verifyMinNorm = 0.99
	verifyMaxNorm = 1.01

	// Report list caps: counts in verify_report are always exact; only the
	// NAMED lists are bounded so the JSONB row never bloats into megabytes
	// at 10M-scale defect counts.
	verifyOffenderListCap    = 50
	verifyMissingListCap     = 50
	verifySkipListCapDefault = 1000

	// verifyGuardReservoirCap hard-caps the Stufe-5 reservoir regardless of
	// config: pair count grows quadratically (n·(n-1)/2 dot products à 1024
	// dims) — 4096 vectors ≈ 8.4M pairs ≈ seconds of CPU, a safe ceiling
	// even for generous verify_sample_n values.
	verifyGuardReservoirCap = 4096
)

// verifyNextHNSWIndexName / DDL: §4.7 Stufe 3. Parameters from the
// CANONICAL live recommendation (m=16, ef_construction=128 — migration 115
// codified the live state; NOT 001's stale 64). CONCURRENTLY + INVALID
// recovery via ensureConcurrentIndexValid; an existing VALID index is
// reused, never rebuilt (§4.7 idempotent re-run). Build-memory note
// (§6.1): the HNSW graph (~2.8 kB/vector → ~2.8 GB @1M, ~28 GB @10M) must
// fit maintenance_work_mem for the fast build path — the verify runbook
// sets a session-level mwm per scale step; graph > available RAM is a
// DECLARED limit of this design (handed to Achse 06).
const (
	verifyNextHNSWIndexName = "idx_embedding_next_hnsw"
	verifyNextHNSWIndexDDL  = `CREATE INDEX CONCURRENTLY IF NOT EXISTS ` + verifyNextHNSWIndexName + `
		ON context_blocks USING hnsw ((embedding_next::halfvec(1024)) halfvec_cosine_ops)
		WITH (m = 16, ef_construction = 128)`
)

// verifySisterIndexes are the _next twins of the three partial indexes
// whose predicates bind to the physical embedding column (§4.7 Stufe 3 /
// §4.8 Attnum class): after the cutover's column RENAME the old exemplars
// follow embedding_old, so without these pre-built sisters the backfill
// peek, the dream queue scan and the guard pending scan would all
// seq-scan at 10M (and `cleanup` would silently drop the only carriers).
// The swap tx (W04-6) renames them canonically.
var verifySisterIndexes = []struct{ name, ddl string }{
	// Twin of idx_embedding_pending (migration 109): the regular backfill
	// pending predicate on the post-cutover serving column.
	{"idx_embedding_pending_next", `CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_embedding_pending_next
		ON context_blocks (created_at)
		WHERE embedding_next IS NULL AND NOT is_archived`},
	// Twin of idx_dream_pending (016_dream.sql:11-13), embedding →
	// embedding_next.
	{"idx_dream_pending_next", `CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_dream_pending_next
		ON context_blocks (dream_checked_at ASC NULLS FIRST, quality_score ASC)
		WHERE NOT is_archived AND embedding_next IS NOT NULL`},
	// Twin of idx_guard_pending (074_guard_check_type_policy.sql:68-72),
	// embedding → embedding_next.
	{"idx_guard_pending_next", `CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_guard_pending_next
		ON context_blocks (created_at ASC)
		WHERE NOT is_archived
		  AND (metadata->>'guard_checked_at') IS NULL
		  AND embedding_next IS NOT NULL`},
}

// verifyCompletenessWhere is the §4.7 Stufe-1 predicate, defined ONCE
// (design §3.3 doctrine) and shared by the verify count, the missing-id
// sample, and the arm's running→verifying entry probe (watermark-less
// variant = the CURRENT pending set). POSITIONAL CONTRACT: $1 = migration
// id, $2 = watermark (only with withWatermark). The NOT-EXISTS exception
// covers exactly the infinity memos of THIS migration — without it a
// single declared skip (oversize/sensitivity_ineligible) kept the gate
// permanently red (review finding; the W04-5 test pins it as a negative
// probe). Blocks under a FINITE backoff memo still count as pending.
func verifyCompletenessWhere(withWatermark bool) string {
	sql := `embedding IS NOT NULL AND embedding_next IS NULL AND NOT is_archived
	AND NOT EXISTS (
		SELECT 1 FROM context_embed_failures f
		WHERE f.block_id = context_blocks.id
		  AND f.migration_id = $1::uuid
		  AND f.next_attempt_at = 'infinity')`
	if withWatermark {
		sql += `
	AND created_at < $2`
	}
	return sql
}

// The verify_report shape (JSONB, five named sections §4.7) follows.

type verifyReport struct {
	Result       string                    `json:"result"`
	StartedAt    time.Time                 `json:"started_at"`
	FinishedAt   time.Time                 `json:"finished_at"`
	Watermark    time.Time                 `json:"watermark"`
	ToModel      string                    `json:"to_model"`
	Completeness verifyCompletenessSection `json:"completeness"`
	Integrity    verifyIntegritySection    `json:"integrity"`
	Index        verifyIndexSection        `json:"index"`
	Quality      verifyQualitySection      `json:"quality"`
	Guard        verifyGuardSection        `json:"guard"`
}

type verifySkipEntry struct {
	BlockID string `json:"block_id"`
	Class   string `json:"class"`
}

type verifyCompletenessSection struct {
	Result        string   `json:"result"`
	MissingBlocks int64    `json:"missing_blocks"`
	MissingSample []string `json:"missing_sample,omitempty"`
	FiniteMemos   int64    `json:"finite_memos"`
	// SkipCount is exact; Skips is the NAMED list (§4.7 Stufe 1
	// "Skip-Liste namentlich"), capped — SkipsTruncated flags the cap.
	SkipCount      int64             `json:"skip_count"`
	Skips          []verifySkipEntry `json:"skips,omitempty"`
	SkipsTruncated bool              `json:"skips_truncated,omitempty"`
	// VisibilityLoss = blocks that lose their serving vector at the swap
	// (§4.8 Post-Cutover-Restmengen): skips that still carry an old-space
	// vector + archived rows with a vector. Informative for the operator
	// confirm — never a red condition (the loss is a DECLARED consequence).
	VisibilityLoss         int64 `json:"visibility_loss"`
	VisibilityLossSkips    int64 `json:"visibility_loss_skips"`
	VisibilityLossArchived int64 `json:"visibility_loss_archived"`
}

type verifyOffender struct {
	BlockID string `json:"block_id"`
	Kind    string `json:"kind"` // dims | norm | model
	Detail  string `json:"detail"`
}

type verifyIntegritySection struct {
	Result          string           `json:"result"`
	RowsChecked     int64            `json:"rows_checked"`
	DimsViolations  int64            `json:"dims_violations"`
	NormViolations  int64            `json:"norm_violations"`
	ModelViolations int64            `json:"model_violations"`
	Offenders       []verifyOffender `json:"offenders,omitempty"`
}

type verifyIndexState struct {
	Name   string `json:"name"`
	Valid  bool   `json:"valid"`
	Reused bool   `json:"reused"`
}

type verifyIndexSection struct {
	Result  string             `json:"result"` // green | red | skipped
	Detail  string             `json:"detail,omitempty"`
	Indexes []verifyIndexState `json:"indexes,omitempty"`
}

type verifyQueryOverlap struct {
	BlockID string  `json:"block_id"`
	Overlap float64 `json:"overlap"`
}

type verifyQualitySection struct {
	Result      string               `json:"result"` // informative | skipped
	Mode        string               `json:"mode"`
	K           int                  `json:"k,omitempty"`
	Samples     int                  `json:"samples,omitempty"`
	MeanOverlap float64              `json:"mean_overlap"`
	MinOverlap  float64              `json:"min_overlap"`
	PerQuery    []verifyQueryOverlap `json:"per_query,omitempty"`
	Note        string               `json:"note"`
}

type verifyGuardSection struct {
	Result             string  `json:"result"` // informative | skipped
	SampleSize         int     `json:"sample_size"`
	Pairs              int64   `json:"pairs"`
	ThresholdDuplicate float64 `json:"threshold_duplicate"`
	ThresholdReview    float64 `json:"threshold_review"`
	MaxCosine          float64 `json:"max_cosine"`
	P99Cosine          float64 `json:"p99_cosine"`
	P95Cosine          float64 `json:"p95_cosine"`
	AboveDuplicate     int64   `json:"above_duplicate"`
	AboveReview        int64   `json:"above_review"`
}

// maybeStartEmbedVerify starts the one-shot verify runner unless one is
// already active (single-flight CAS). Fire-and-forget: the calling cycle
// returns to draining immediately (§4.1); on 10M the gate runs for hours
// (CIC build) while the arm keeps working. embedVerifySync (tests) runs it
// inline for deterministic assertions. A run that errors out leaves
// verify_report NULL — the next verifying-tick re-triggers it, so
// transient DB errors self-heal without dedicated retry code.
func (s *Scheduler) maybeStartEmbedVerify(ctx context.Context, mig *embedmigration.Migration, cfg *config.Config) {
	if !s.embedVerifyActive.CompareAndSwap(false, true) {
		return
	}
	run := func() {
		defer s.embedVerifyActive.Store(false)
		defer guardPanic("embed verify")
		if err := s.runEmbedVerify(ctx, mig, cfg); err != nil {
			slog.Error("scheduler: embed verify run failed — no verdict stored, next cycle retries",
				"migration_id", mig.ID, "error", err)
		}
	}
	if s.embedVerifySync {
		run()
		return
	}
	go run()
}

// runEmbedVerify executes the five §4.7 stages and lands the verdict.
// Stage order deviates from the §4.7 numbering ON PURPOSE: the index build
// (Stufe 3) runs LAST so a red completeness/integrity verdict never pays
// an hours-long CIC first; the report keeps all five named sections either
// way (a red run marks the index section "skipped").
func (s *Scheduler) runEmbedVerify(ctx context.Context, mig *embedmigration.Migration, cfg *config.Config) error {
	if mig.VerifyStartedAt == nil {
		return fmt.Errorf("embed-verify: migration %s in verifying without watermark", mig.ID)
	}
	wm := *mig.VerifyStartedAt
	rep := &verifyReport{StartedAt: time.Now().UTC(), Watermark: wm, ToModel: mig.ToModel}
	slog.Info("scheduler: embed verify started", "migration_id", mig.ID, "watermark", wm)

	comp, err := s.verifyCompleteness(ctx, mig, wm, cfg)
	if err != nil {
		return err
	}
	rep.Completeness = comp

	queries, err := s.verifySampleQueries(ctx, wm, cfg)
	if err != nil {
		return err
	}
	pass, err := s.runVerifyScanPass(ctx, mig, wm, cfg, queries)
	if err != nil {
		return err
	}
	rep.Integrity = pass.integritySection()
	rep.Quality = s.runVerifyQualityStage(cfg, queries)
	rep.Guard = s.verifyGuardStage(cfg, pass.reservoir)

	if rep.Completeness.Result == verifyRed || rep.Integrity.Result == verifyRed {
		rep.Index = verifyIndexSection{Result: verifySkipped,
			Detail: "index build skipped: earlier stage red (a CONCURRENTLY HNSW build over a defective bestand would be hours of wasted I/O at 10M)"}
		return s.finishVerifyRed(ctx, mig, rep)
	}

	idxSection, err := s.verifyEnsureIndexes(ctx)
	rep.Index = idxSection
	if err != nil {
		rep.Index.Result = verifyRed
		rep.Index.Detail = err.Error()
		return s.finishVerifyRed(ctx, mig, rep)
	}

	rep.Result = verifyGreen
	rep.FinishedAt = time.Now().UTC()
	return s.finishVerifyGreen(ctx, mig, rep)
}

// verifyCompleteness is Stufe 1: the watermark-scoped §4.7 count (== 0),
// the finite-memo rest (== 0), the named skip list and the
// visibility_loss numbers. All SQL-side: the counts ride
// idx_embedding_next_pending (the pending set is small once the arm
// drained into verifying) and the memo table; only the archived-with-
// vector count is a heap pass without TOAST detoast — a one-off per
// verify, not part of the vector I/O budget.
func (s *Scheduler) verifyCompleteness(ctx context.Context, mig *embedmigration.Migration, wm time.Time, cfg *config.Config) (verifyCompletenessSection, error) {
	sec := verifyCompletenessSection{Result: verifyGreen}

	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM context_blocks WHERE `+verifyCompletenessWhere(true),
		mig.ID, wm).Scan(&sec.MissingBlocks); err != nil {
		return sec, fmt.Errorf("embed-verify: completeness count: %w", err)
	}
	if sec.MissingBlocks > 0 {
		rows, err := s.pool.Query(ctx,
			`SELECT id::text FROM context_blocks WHERE `+verifyCompletenessWhere(true)+`
			 ORDER BY created_at LIMIT $3`, mig.ID, wm, verifyMissingListCap)
		if err != nil {
			return sec, fmt.Errorf("embed-verify: missing sample: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return sec, fmt.Errorf("embed-verify: missing sample row: %w", err)
			}
			sec.MissingSample = append(sec.MissingSample, id)
		}
		if err := rows.Err(); err != nil {
			return sec, fmt.Errorf("embed-verify: missing sample iterate: %w", err)
		}
	}

	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM context_embed_failures
		 WHERE migration_id = $1::uuid AND next_attempt_at < 'infinity'`,
		mig.ID).Scan(&sec.FiniteMemos); err != nil {
		return sec, fmt.Errorf("embed-verify: finite memo count: %w", err)
	}

	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM context_embed_failures
		 WHERE migration_id = $1::uuid AND next_attempt_at = 'infinity'`,
		mig.ID).Scan(&sec.SkipCount); err != nil {
		return sec, fmt.Errorf("embed-verify: skip count: %w", err)
	}
	skipListCap := cfg.EmbedMigration.VerifySampleN
	if skipListCap <= 0 {
		skipListCap = verifySkipListCapDefault
	}
	if sec.SkipCount > 0 {
		rows, err := s.pool.Query(ctx,
			`SELECT block_id::text, last_class FROM context_embed_failures
			 WHERE migration_id = $1::uuid AND next_attempt_at = 'infinity'
			 ORDER BY first_seen LIMIT $2`, mig.ID, skipListCap)
		if err != nil {
			return sec, fmt.Errorf("embed-verify: skip list: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var e verifySkipEntry
			if err := rows.Scan(&e.BlockID, &e.Class); err != nil {
				return sec, fmt.Errorf("embed-verify: skip list row: %w", err)
			}
			sec.Skips = append(sec.Skips, e)
		}
		if err := rows.Err(); err != nil {
			return sec, fmt.Errorf("embed-verify: skip list iterate: %w", err)
		}
		sec.SkipsTruncated = sec.SkipCount > int64(len(sec.Skips))
	}

	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM context_embed_failures f
		 JOIN context_blocks cb ON cb.id = f.block_id
		 WHERE f.migration_id = $1::uuid AND f.next_attempt_at = 'infinity'
		   AND cb.embedding IS NOT NULL`,
		mig.ID).Scan(&sec.VisibilityLossSkips); err != nil {
		return sec, fmt.Errorf("embed-verify: visibility loss (skips): %w", err)
	}
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM context_blocks WHERE is_archived AND embedding IS NOT NULL`,
	).Scan(&sec.VisibilityLossArchived); err != nil {
		return sec, fmt.Errorf("embed-verify: visibility loss (archived): %w", err)
	}
	sec.VisibilityLoss = sec.VisibilityLossSkips + sec.VisibilityLossArchived

	if sec.MissingBlocks > 0 || sec.FiniteMemos > 0 {
		sec.Result = verifyRed
	}
	return sec, nil
}

// verifyQuerySample is one degraded-Stufe-4 sample query: a migrated block
// serving as its own query in BOTH spaces (oldVec = embedding, newVec =
// embedding_next — both exact, zero wire calls), plus its two top-k
// neighborhoods accumulated by the scan pass.
type verifyQuerySample struct {
	id     string
	oldVec []float32
	newVec []float32
	oldTop topK
	newTop topK
}

// verifySampleQueries draws the Stufe-4 sample deterministically
// (md5-ordered — a pseudo-random but reproducible draw; ORDER BY created_at
// would cluster on the oldest corner of the corpus). Source note: the
// design names context_embed_cache top-hit_count as a query source, but
// that table stores only text_preview (026) — re-embedding a TRUNCATED
// preview with to_model would measure the truncation artifact, not the
// space; blocks-as-queries keeps both query vectors exact. The REAL query
// sample (context_access_log.query_text) belongs to the Achse-01
// recall_check (see runVerifyQualityStage hook).
func (s *Scheduler) verifySampleQueries(ctx context.Context, wm time.Time, cfg *config.Config) ([]verifyQuerySample, error) {
	n := cfg.EmbedMigration.VerifyOverlapSamples
	k := cfg.EmbedMigration.VerifyOverlapK
	if n <= 0 || k <= 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id::text, embedding, embedding_next FROM context_blocks
		 WHERE created_at < $1 AND embedding_next IS NOT NULL
		   AND embedding IS NOT NULL AND NOT is_archived
		 ORDER BY md5(id::text)
		 LIMIT $2`, wm, n)
	if err != nil {
		return nil, fmt.Errorf("embed-verify: sample queries: %w", err)
	}
	defer rows.Close()
	var out []verifyQuerySample
	for rows.Next() {
		var q verifyQuerySample
		var oldV, newV pgvec.Vector
		if err := rows.Scan(&q.id, &oldV, &newV); err != nil {
			return nil, fmt.Errorf("embed-verify: sample query row: %w", err)
		}
		q.oldVec, q.newVec = oldV.Slice(), newV.Slice()
		q.oldTop, q.newTop = newTopK(k), newTopK(k)
		out = append(out, q)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("embed-verify: sample queries iterate: %w", err)
	}
	return out, nil
}

// verifyPassStats accumulates the single table pass: full-coverage
// integrity counters (Stufe 2), the per-query top-k heaps (Stufe 4) and
// the guard reservoir (Stufe 5).
type verifyPassStats struct {
	rows      int64
	seen      int64 // rows offered to the reservoir (valid-dims only)
	dims      int64
	norm      int64
	model     int64
	offenders []verifyOffender
	reservoir [][]float32
}

func (p *verifyPassStats) addOffender(id, kind, detail string) {
	if len(p.offenders) >= verifyOffenderListCap {
		return
	}
	p.offenders = append(p.offenders, verifyOffender{BlockID: id, Kind: kind, Detail: detail})
}

func (p *verifyPassStats) integritySection() verifyIntegritySection {
	sec := verifyIntegritySection{
		Result:          verifyGreen,
		RowsChecked:     p.rows,
		DimsViolations:  p.dims,
		NormViolations:  p.norm,
		ModelViolations: p.model,
		Offenders:       p.offenders,
	}
	if p.dims+p.norm+p.model > 0 {
		sec.Result = verifyRed
	}
	return sec
}

// runVerifyScanPass is THE one table pass (§4.7 Stufe 2/4 Ein-Pass-
// Doktrin): every migrated row up to the watermark is read exactly once —
// the fp32 TOAST detoast of both vector columns is the dominant cost (~4 GB
// @1M, ~41 GB @10M per column, sequential) and everything computed per row
// (dims/norm/model integrity, distances to all sample queries in both
// spaces, reservoir membership) rides that single read. Batch-kNN: per row
// one dot product against each of the N query vectors per space with
// in-memory top-k heaps — never N × full scan.
//
// Integrity runs FULL-coverage instead of the design's N=1000 sample: the
// pass already pays the detoast for the kNN, a fused multiply-add loop per
// row is free by comparison, and full coverage is strictly stronger — a
// sampled check would re-open exactly the planted-defect class the RED
// gates prove (a defect outside the sample window).
func (s *Scheduler) runVerifyScanPass(ctx context.Context, mig *embedmigration.Migration, wm time.Time, cfg *config.Config, queries []verifyQuerySample) (*verifyPassStats, error) {
	stats := &verifyPassStats{}
	resCap := cfg.EmbedMigration.VerifySampleN
	if resCap > verifyGuardReservoirCap {
		resCap = verifyGuardReservoirCap
	}
	// Deterministic reservoir RNG: reproducible verify runs; sampling, not
	// cryptography.
	rng := rand.New(rand.NewPCG(0x04c7, 0x2026)) //nolint:gosec // statistical sampling, not security material

	rows, err := s.pool.Query(ctx,
		`SELECT id::text, embed_model_next, embedding, embedding_next
		 FROM context_blocks
		 WHERE created_at < $1 AND embedding_next IS NOT NULL AND NOT is_archived`, wm)
	if err != nil {
		return nil, fmt.Errorf("embed-verify: scan pass: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id string
		var model *string
		var oldV, nextV *pgvec.Vector
		if err := rows.Scan(&id, &model, &oldV, &nextV); err != nil {
			return nil, fmt.Errorf("embed-verify: scan pass row: %w", err)
		}
		stats.rows++
		var next []float32
		if nextV != nil {
			next = nextV.Slice()
		}

		// Stufe 2 folded: dims, L2 norm, model — every row.
		if len(next) != embed.TargetDims {
			stats.dims++
			stats.addOffender(id, "dims", fmt.Sprintf("vector_dims=%d, want %d", len(next), embed.TargetDims))
		} else if n := l2norm(next); n < verifyMinNorm || n > verifyMaxNorm {
			stats.norm++
			stats.addOffender(id, "norm", fmt.Sprintf("l2_norm=%.4f outside [%.2f,%.2f]", n, verifyMinNorm, verifyMaxNorm))
		}
		if model == nil || *model != mig.ToModel {
			got := "<null>"
			if model != nil {
				got = *model
			}
			stats.model++
			stats.addOffender(id, "model", fmt.Sprintf("embed_model_next=%q, want %q", got, mig.ToModel))
		}

		if len(next) != embed.TargetDims {
			continue // dims defect: unusable for distance work
		}

		// Stufe 4 contributions: one dot per query per space.
		for i := range queries {
			q := &queries[i]
			if q.id == id {
				continue // self-match excluded from both neighborhoods
			}
			if oldV != nil {
				if o := oldV.Slice(); len(o) == embed.TargetDims {
					q.oldTop.offer(id, dot32(o, q.oldVec))
				}
			}
			q.newTop.offer(id, dot32(next, q.newVec))
		}

		// Stufe 5 reservoir (uniform without knowing the total up front).
		if resCap > 0 {
			stats.seen++
			if len(stats.reservoir) < resCap {
				stats.reservoir = append(stats.reservoir, next)
			} else if j := rng.Int64N(stats.seen); j < int64(resCap) {
				stats.reservoir[j] = next
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("embed-verify: scan pass iterate: %w", err)
	}
	return stats, nil
}

// runVerifyQualityStage is the Stufe-4 producer — TODAY the degraded
// Overlap@k metric (informative, never red): per sample query the exact
// top-k neighborhood in the old space vs. the new space, overlap =
// |old ∩ new| / k. It measures neighborhood preservation across the model
// change, not retrieval quality against ground truth.
//
// ACHSE-01 HOOK (the documented interface point): once recall_check
// exists, it replaces THIS function's body (and the query source moves
// from blocks-as-queries to context_access_log.query_text embedded with
// to_model). Contract for the replacement: consume the verify config +
// watermark scope, produce a verifyQualitySection — Result "informative"
// stays report-only, Result "red"/"green" (with recall@k thresholds from
// embed_migration.verify_* config) is picked up by runEmbedVerify's
// existing red path WITHOUT further wiring, because the orchestrator
// treats every section Result uniformly.
func (s *Scheduler) runVerifyQualityStage(cfg *config.Config, queries []verifyQuerySample) verifyQualitySection {
	k := cfg.EmbedMigration.VerifyOverlapK
	if len(queries) == 0 || k <= 0 {
		return verifyQualitySection{Result: verifySkipped, Mode: "overlap_degraded",
			Note: "disabled (verify_overlap_samples/verify_overlap_k <= 0) or no migrated blocks to sample"}
	}
	sec := verifyQualitySection{
		Result: verifyInformative, Mode: "overlap_degraded",
		K: k, Samples: len(queries), MinOverlap: 1,
		Note: "degraded Overlap@k, blocks-as-queries, exact single-pass batch-kNN in both spaces; " +
			"replaced by the Achse-01 recall_check (see runVerifyQualityStage hook)",
	}
	var sum float64
	counted := 0
	for i := range queries {
		q := &queries[i]
		denom := min(k, min(len(q.oldTop.ids), len(q.newTop.ids)))
		if denom == 0 {
			continue
		}
		inOld := make(map[string]struct{}, len(q.oldTop.ids))
		for _, id := range q.oldTop.ids {
			inOld[id] = struct{}{}
		}
		hits := 0
		for _, id := range q.newTop.ids {
			if _, ok := inOld[id]; ok {
				hits++
			}
		}
		ov := float64(hits) / float64(denom)
		sum += ov
		counted++
		if ov < sec.MinOverlap {
			sec.MinOverlap = ov
		}
		sec.PerQuery = append(sec.PerQuery, verifyQueryOverlap{BlockID: q.id, Overlap: ov})
	}
	if counted == 0 {
		sec.Result = verifySkipped
		sec.MinOverlap = 0
		sec.Note = "no query had neighbors in both spaces (corpus too small for k)"
		return sec
	}
	sec.MeanOverlap = sum / float64(counted)
	return sec
}

// verifyGuardStage is Stufe 5 (§4.6, E-04-4, report-only): the cosine
// distribution of known non-duplicates in the NEW space against the
// strictest live guard thresholds. "Known non-duplicates" = pairs of
// coexisting live blocks — the old-space guard let them coexist, so any
// pair crossing p_threshold_duplicate in the new space signals that the
// thresholds need recalibration BEFORE the cutover. Cosine == dot product:
// the integrity stage enforces unit norm (±1%) on every vector entering
// the reservoir's pass.
func (s *Scheduler) verifyGuardStage(cfg *config.Config, reservoir [][]float32) verifyGuardSection {
	dup, review := s.guardThresholdBounds()
	sec := verifyGuardSection{
		Result: verifyInformative, SampleSize: len(reservoir),
		ThresholdDuplicate: dup, ThresholdReview: review,
	}
	if cfg.EmbedMigration.VerifySampleN <= 0 || len(reservoir) < 2 {
		sec.Result = verifySkipped
		return sec
	}
	cos := make([]float64, 0, len(reservoir)*(len(reservoir)-1)/2)
	for i := 0; i < len(reservoir); i++ {
		for j := i + 1; j < len(reservoir); j++ {
			cos = append(cos, dot32(reservoir[i], reservoir[j]))
		}
	}
	sort.Float64s(cos)
	n := len(cos)
	sec.Pairs = int64(n)
	sec.MaxCosine = cos[n-1]
	sec.P99Cosine = cos[pctIdx(n, 0.99)]
	sec.P95Cosine = cos[pctIdx(n, 0.95)]
	for i := n - 1; i >= 0; i-- {
		if cos[i] >= review {
			sec.AboveReview++
			if cos[i] >= dup {
				sec.AboveDuplicate++
			}
			continue
		}
		break // sorted ascending: everything below is below both thresholds
	}
	return sec
}

// guardThresholdBounds resolves the strictest (lowest) guard thresholds
// across all guard-checked block types — the most sensitive live policy is
// the right comparison line for a report that exists to catch threshold
// drift early. Falls back to the blocktype seed defaults when no registry
// is wired (tests, pre-boot).
func (s *Scheduler) guardThresholdBounds() (dup, review float64) {
	dup, review = blocktype.DefaultThresholdDuplicate, blocktype.DefaultThresholdReview
	if s.blocktypes == nil {
		return dup, review
	}
	set := s.blocktypes.Snapshot()
	for _, t := range set.GuardCheckTypes() {
		d, r := set.GuardThresholds(t)
		if d < dup {
			dup = d
		}
		if r < review {
			review = r
		}
	}
	return dup, review
}

// verifyEnsureIndexes is Stufe 3: HNSW + the three sister partial indexes,
// all CONCURRENTLY with INVALID recovery (shared ensureConcurrentIndexValid
// helper — the W04-4 CIC lifecycle doctrine). A VALID exemplar is reused
// (Reused=true), which is what makes verify re-runs idempotent: a
// paused→resume→verifying round-trip never rebuilds a finished 10M HNSW.
func (s *Scheduler) verifyEnsureIndexes(ctx context.Context) (verifyIndexSection, error) {
	sec := verifyIndexSection{Result: verifyGreen}
	builds := make([]struct{ name, ddl string }, 0, 1+len(verifySisterIndexes))
	builds = append(builds, struct{ name, ddl string }{verifyNextHNSWIndexName, verifyNextHNSWIndexDDL})
	builds = append(builds, verifySisterIndexes...)
	for _, b := range builds {
		reused, err := s.ensureConcurrentIndexValid(ctx, b.name, b.ddl)
		if err != nil {
			return sec, err
		}
		sec.Indexes = append(sec.Indexes, verifyIndexState{Name: b.name, Valid: true, Reused: reused})
	}
	return sec, nil
}

// finishVerifyGreen stores the green report; the row STAYS `verifying` —
// the cutover is the operator confirm (W04-6/W04-7, E-04-2). The UPDATE is
// conditional on status='verifying': a concurrent operator transition
// (pause/abort) wins and the verdict is discarded — a later re-entry
// re-runs the gate against a fresh watermark.
func (s *Scheduler) finishVerifyGreen(ctx context.Context, mig *embedmigration.Migration, rep *verifyReport) error {
	body, err := json.Marshal(rep)
	if err != nil {
		return fmt.Errorf("embed-verify: marshal report: %w", err)
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE context_embed_migrations SET verify_report = $2
		 WHERE id = $1::uuid AND status = 'verifying'`,
		mig.ID, string(body))
	if err != nil {
		return fmt.Errorf("embed-verify: store green report: %w", err)
	}
	if tag.RowsAffected() == 0 {
		slog.Info("scheduler: embed verify green verdict discarded — row left verifying concurrently",
			"migration_id", mig.ID)
		return nil
	}
	slog.Info("scheduler: embed verify GREEN — awaiting operator confirm (W04-6)", "migration_id", mig.ID)
	return nil
}

// finishVerifyRed stores the red report and CAS-pauses the migration in
// ONE tx (§4.7: rot → paused with the finding in verify_report AND
// last_error — NEVER auto-abort; the operator decides between fixing the
// fehlmenge, adjusting thresholds/config and aborting). A lost CAS race
// (operator moved the row mid-verify) rolls the report back too — the
// verdict belonged to a state that no longer exists.
func (s *Scheduler) finishVerifyRed(ctx context.Context, mig *embedmigration.Migration, rep *verifyReport) error {
	rep.Result = verifyRed
	if rep.FinishedAt.IsZero() {
		rep.FinishedAt = time.Now().UTC()
	}
	summary := verifyRedSummary(rep)
	body, err := json.Marshal(rep)
	if err != nil {
		return fmt.Errorf("embed-verify: marshal red report: %w", err)
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("embed-verify: begin red tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op
	if _, err := tx.Exec(ctx,
		`UPDATE context_embed_migrations SET verify_report = $2
		 WHERE id = $1::uuid AND status = 'verifying'`,
		mig.ID, string(body)); err != nil {
		return fmt.Errorf("embed-verify: store red report: %w", err)
	}
	err = embedmigration.Transition(ctx, tx, mig.ID,
		embedmigration.StatusVerifying, embedmigration.StatusPaused,
		embedmigration.WithLastError(summary))
	if errors.Is(err, embedmigration.ErrTransitionRaceLost) {
		slog.Warn("scheduler: embed verify red verdict discarded — row left verifying concurrently",
			"migration_id", mig.ID)
		return nil // deferred rollback drops the report write too
	}
	if err != nil {
		return fmt.Errorf("embed-verify: pause transition: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("embed-verify: commit red verdict: %w", err)
	}
	slog.Warn("scheduler: embed verify RED — migration paused", "migration_id", mig.ID, "detail", summary)
	return nil
}

// verifyRedSummary builds the last_error line for a red verdict: defect
// classes + counts, no block content (the column contract mirrors
// context_embed_failures.last_error — normalized, ≤500 chars).
func verifyRedSummary(rep *verifyReport) string {
	var parts []string
	if c := rep.Completeness; c.Result == verifyRed {
		if c.MissingBlocks > 0 {
			parts = append(parts, fmt.Sprintf("completeness: %d block(s) before watermark without embedding_next and without skip memo", c.MissingBlocks))
		}
		if c.FiniteMemos > 0 {
			parts = append(parts, fmt.Sprintf("completeness: %d unresolved finite failure memo(s)", c.FiniteMemos))
		}
	}
	if i := rep.Integrity; i.Result == verifyRed {
		if i.DimsViolations > 0 {
			parts = append(parts, fmt.Sprintf("integrity: %d dims violation(s)", i.DimsViolations))
		}
		if i.NormViolations > 0 {
			parts = append(parts, fmt.Sprintf("integrity: %d norm violation(s)", i.NormViolations))
		}
		if i.ModelViolations > 0 {
			parts = append(parts, fmt.Sprintf("integrity: %d embed_model_next mismatch(es)", i.ModelViolations))
		}
	}
	if rep.Index.Result == verifyRed {
		parts = append(parts, "index: "+rep.Index.Detail)
	}
	if len(parts) == 0 {
		parts = append(parts, "unspecified red section")
	}
	msg := "verify red: " + strings.Join(parts, "; ")
	if runes := []rune(msg); len(runes) > 500 {
		msg = string(runes[:500])
	}
	return msg
}

// Small numeric helpers follow.

// topK keeps the k highest-similarity ids seen so far (sims descending).
// offer is O(1) for the common no-beat case and an O(k) memmove on insert
// — with k ≤ ~50 the heaps are the cheap side of the pass; the dot
// products are the expensive side.
type topK struct {
	k    int
	ids  []string
	sims []float64
}

func newTopK(k int) topK { return topK{k: k} }

func (h *topK) offer(id string, sim float64) {
	if h.k <= 0 {
		return
	}
	if len(h.sims) == h.k && sim <= h.sims[h.k-1] {
		return
	}
	lo, hi := 0, len(h.sims)
	for lo < hi {
		m := (lo + hi) / 2
		if h.sims[m] >= sim {
			lo = m + 1
		} else {
			hi = m
		}
	}
	h.ids = append(h.ids, "")
	h.sims = append(h.sims, 0)
	copy(h.ids[lo+1:], h.ids[lo:])
	copy(h.sims[lo+1:], h.sims[lo:])
	h.ids[lo] = id
	h.sims[lo] = sim
	if len(h.sims) > h.k {
		h.ids = h.ids[:h.k]
		h.sims = h.sims[:h.k]
	}
}

// dot32 is the float64-accumulated dot product of two float32 vectors
// (equal length enforced by the callers' dims checks; a mismatch yields 0).
func dot32(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}
	var s float64
	for i := range a {
		s += float64(a[i]) * float64(b[i])
	}
	return s
}

func l2norm(v []float32) float64 {
	var s float64
	for _, x := range v {
		s += float64(x) * float64(x)
	}
	return math.Sqrt(s)
}

// pctIdx maps a percentile onto a sorted-slice index (nearest-rank on the
// ascending slice).
func pctIdx(n int, p float64) int {
	i := int(p * float64(n-1))
	if i < 0 {
		i = 0
	}
	if i >= n {
		i = n - 1
	}
	return i
}
