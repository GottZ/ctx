// Package recall holds the measurement machinery for Achse 01 (recall_check):
// ANN-vs-exact recall probes, scope-stratified, that quantify how close the
// production HNSW index tracks a brute-force reference. W01-1 delivers only
// the persistence layer (this file) — the probe mechanics that populate
// context_recall_runs land in W01-2, the scheduler arm in W01-3/W01-4.
//
// Source: https://github.com/GottZ/ctx
package recall

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Run mirrors one row of context_recall_runs (migration 110). Pointer fields
// carry the column's NULL semantics: Scope is NULL for the pseudo-stratum
// "all", and the four *_ms/recall_* metrics are NULL whenever Valid is false
// (an aborted or plan-assertion-violating probe has nothing to report).
type Run struct {
	ID             string    // set by the DB default (uuidv7()); empty on Insert input
	RanAt          time.Time // set by the DB default (now()); zero on Insert input, filled by LatestByStratum for the status age_ms (design/01 §4.4)
	RunGroup       string    // uuid — one scheduler run = one group across all strata
	Stratum        string    // "small" | "medium" | "large" | "all"
	Scope          *string
	CorpusEmbedded int
	K              int16
	NQueries       int16
	QuerySource    string // "log" | "loo" | "mixed"
	EfSearch       int
	IterativeScan  string // "off" | "strict_order" | "relaxed_order"
	Valid          bool
	RecallAvg      *float64
	RecallMin      *float64
	AnnMsP50       *float64
	AnnMsP95       *float64
	ExactMsP50     *float64
	Meta           map[string]any
}

// metaAllowlist is the fail-closed leak guard on Run.Meta: the table must
// never carry query texts, block IDs, or vectors (design/01 §Leak-Schutz),
// so Insert rejects any key outside this exact set before the write. Adding
// a key is a deliberate design decision — extend the allowlist, the
// migration-110 comment block, AND the corresponding unit test together,
// never ad hoc.
var metaAllowlist = map[string]struct{}{
	"pgvector_version":  {},
	"pg_version":        {},
	"index_reloptions":  {},
	"embed_model":       {},
	"invalid_reason":    {},
	"budget_exhausted":  {},
	"strata_bounds":     {},
	"epsilon":           {},
	"n_eff_min":         {},
	"exact_touch_bytes": {},
	"buffercache_delta": {},
	"scope_changed":     {},
}

// maxMetaStringLen bounds string values in Meta — long enough for any of the
// allowlisted scalars (version strings, reloptions, short reason codes), far
// too short to smuggle a query text or a serialized block list.
const maxMetaStringLen = 256

// validateMeta enforces the leak guard: every key must be in metaAllowlist,
// and every value must be a scalar (string/bool/number) — no arrays, maps,
// or nil containers that could hide structured payloads. Split out from
// Insert so it is unit-testable without a database.
func validateMeta(meta map[string]any) error {
	for k, v := range meta {
		if _, ok := metaAllowlist[k]; !ok {
			return fmt.Errorf("recall: meta key %q is not in the allowlist", k)
		}
		switch val := v.(type) {
		case string:
			if len(val) > maxMetaStringLen {
				return fmt.Errorf("recall: meta key %q: string value exceeds %d chars", k, maxMetaStringLen)
			}
		case bool, float64, float32, int, int8, int16, int32, int64,
			uint, uint8, uint16, uint32, uint64:
			// scalar, always fine
		default:
			return fmt.Errorf("recall: meta key %q: value type %T is not a scalar (arrays/maps/nil are rejected)", k, v)
		}
	}
	return nil
}

// Querier is the minimal interface Insert needs — satisfied by both
// *pgxpool.Pool and pgx.Tx, mirroring store.execQuerier's shape without
// importing the store package (avoids a cross-package import cycle: store
// may one day want to call into recall for scheduler wiring).
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// RowsQuerier is the minimal interface LatestByStratum needs — satisfied by
// both *pgxpool.Pool and pgx.Tx.
type RowsQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Insert writes one recall-run row. It validates Meta against the allowlist
// (fail-closed — rejected keys/values abort before any DB round-trip, no
// partial write) and lets the DB fill ID and ran_at via their defaults.
func Insert(ctx context.Context, q Querier, run Run) error {
	if err := validateMeta(run.Meta); err != nil {
		return err
	}
	meta := run.Meta
	if meta == nil {
		meta = map[string]any{}
	}
	_, err := q.Exec(ctx,
		`INSERT INTO context_recall_runs
		    (run_group, stratum, scope, corpus_embedded, k, n_queries,
		     query_source, ef_search, iterative_scan, valid,
		     recall_avg, recall_min, ann_ms_p50, ann_ms_p95, exact_ms_p50, meta)
		 VALUES
		    ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16::jsonb)`,
		run.RunGroup, run.Stratum, run.Scope, run.CorpusEmbedded, run.K, run.NQueries,
		run.QuerySource, run.EfSearch, run.IterativeScan, run.Valid,
		run.RecallAvg, run.RecallMin, run.AnnMsP50, run.AnnMsP95, run.ExactMsP50, meta,
	)
	if err != nil {
		return fmt.Errorf("recall: insert run: %w", err)
	}
	return nil
}

// DeleteOlderThan removes recall-run rows older than retentionDays (§3.3, the
// W01-3 janitor line). retentionDays <= 0 is a no-op (kept forever) — the same
// opt-out convention as the other janitor retentions. Returns the deleted row
// count. context_recall_runs is aggregate-only, so this is a plain bounded
// DELETE, never a hypertable drop.
func DeleteOlderThan(ctx context.Context, q Querier, retentionDays int) (int64, error) {
	if retentionDays <= 0 {
		return 0, nil
	}
	tag, err := q.Exec(ctx,
		`DELETE FROM context_recall_runs WHERE ran_at < now() - make_interval(days => $1)`,
		retentionDays)
	if err != nil {
		return 0, fmt.Errorf("recall: retention delete: %w", err)
	}
	return tag.RowsAffected(), nil
}

// LatestByStratum returns the newest row per (stratum, scope, k), newest
// group first, capped at limit. DISTINCT ON rides idx_recall_runs_stratum
// (stratum, scope, k, ran_at DESC) directly — no separate window-function
// pass needed.
func LatestByStratum(ctx context.Context, q RowsQuerier, limit int) ([]Run, error) {
	rows, err := q.Query(ctx,
		`SELECT id, ran_at, run_group, stratum, scope, corpus_embedded, k, n_queries,
		        query_source, ef_search, iterative_scan, valid,
		        recall_avg, recall_min, ann_ms_p50, ann_ms_p95, exact_ms_p50, meta
		 FROM (
		    SELECT DISTINCT ON (stratum, scope, k)
		        id, run_group, stratum, scope, corpus_embedded, k, n_queries,
		        query_source, ef_search, iterative_scan, valid,
		        recall_avg, recall_min, ann_ms_p50, ann_ms_p95, exact_ms_p50, meta,
		        ran_at
		    FROM context_recall_runs
		    ORDER BY stratum, scope, k, ran_at DESC
		 ) latest
		 ORDER BY ran_at DESC
		 LIMIT $1`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("recall: latest by stratum: %w", err)
	}
	defer rows.Close()

	var out []Run
	for rows.Next() {
		var r Run
		if err := rows.Scan(
			&r.ID, &r.RanAt, &r.RunGroup, &r.Stratum, &r.Scope, &r.CorpusEmbedded, &r.K, &r.NQueries,
			&r.QuerySource, &r.EfSearch, &r.IterativeScan, &r.Valid,
			&r.RecallAvg, &r.RecallMin, &r.AnnMsP50, &r.AnnMsP95, &r.ExactMsP50, &r.Meta,
		); err != nil {
			return nil, fmt.Errorf("recall: scan run: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("recall: rows: %w", err)
	}
	return out, nil
}
