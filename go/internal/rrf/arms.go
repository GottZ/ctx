package rrf

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Querier is the minimum statement surface the retrieval calls need. Both
// *pgxpool.Pool and pgx.Tx implicitly satisfy it, which is the entire point:
// the SAME statement text runs either on the pool (autocommit, the live path)
// or inside a caller-owned transaction (the B-W2 measurement seam), without a
// second code path and without the rrf package learning about transactions.
//
// The pattern is guard.guardPool (internal/guard/guard.go:74-80) one level
// narrower — read-only, so no Exec.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// ArmRow is one ctx_rrf_arms output row (migration 137, ninth column added in
// 142): the raw per-arm ranks of one candidate plus the two multiplicative
// factors ctx_rrf folds into its score. A nil rank means the block is NOT in
// that arm — which is exactly the information an offline weight sweep needs, so
// the pointers are deliberate and NOT flattened to 0 (rank 0 does not exist; 1
// is the best rank).
//
// CosSim is nil for a candidate only the lexical arms found (the E-M6 rescue
// clause in 137). MassFactor/TypeFactor arrive already COALESCEd, the same way
// the fusion consumes them.
//
// TypeName is the block's registry type (M-W1, migration 142). It is NOT
// redundant with TypeFactor: the factor is not injective — two damped types may
// carry the same value and every undamped type carries 1.0 — so a dump can
// reconstruct a per-type damping sweep from the NAME and cannot from the
// factor. It arrives non-empty from 142 onwards; the empty string means the row
// came out of a dump written BEFORE 142 (the field simply was not there), not
// that a block has no type.
//
// No content, no title, no scope: the function projects identity, numbers and
// one registry classification (137 header, "WARUM KEINE INHALTE IM RETURN"; 142
// header for why a type name does not move that line), and this struct keeps
// that promise on the Go side.
type ArmRow struct {
	ID           string   `json:"id"`
	RankSemantic *int     `json:"rank_semantic"`
	RankFTSDe    *int     `json:"rank_fts_de"`
	RankFTSEn    *int     `json:"rank_fts_en"`
	RankTrigram  *int     `json:"rank_trigram"`
	CosSim       *float64 `json:"cos_sim"`
	MassFactor   float64  `json:"mass_factor"`
	TypeFactor   float64  `json:"type_factor"`
	TypeName     string   `json:"type_name"`
}

// armsQuery calls ctx_rrf_arms over the SAME 18-position argument surface as
// ctx_rrf (migration 137 keeps positions 1-18 byte-identical for exactly this
// reason). Positions 19-23 (the four arm caps + the trigram threshold) are
// deliberately NOT passed: their SQL defaults are the literals ctx_rrf uses,
// so the default call is deckungsgleich with the live fusion. A sweep over the
// caps is a later wave and gets its own parameters then.
const armsQuery = `SELECT id, rank_semantic, rank_fts_de, rank_fts_en, rank_trigram, cos_sim, mass_factor, type_factor, type_name
		 FROM ctx_rrf_arms($1, $2, $3, $4::text[], $5, $6::text[], $7, $8, $9, $10::text[], $11::text[], $12::float8[], $13::text[], $14::text[], $15::uuid[], $16::text, $17::int, $18::int)`

// ArmRanksTx runs ctx_rrf_arms for a search that has ALREADY run through
// SearchTx on the same Querier, and returns the per-arm ranks of every
// candidate the four arms produced (no fusion, no truncation — 137 takes
// p_limit and ignores it).
//
// dec is the decision SearchTx returned, INCLUDING the exact_cap_hit
// degradation: runSelected rewrites dec.Mode to ann when the in-body cap guard
// fired, so mapping dec through selectorSQLArgs here reproduces the arguments
// the live statement actually ran with, not the ones it was first asked to run
// with. That is the difference between a measurement and a plausible-looking
// number.
//
// The caller MUST hand in a Querier that is the same transaction SearchTx ran
// on. Two autocommit statements would see two snapshots AND two GUC states
// (the ann arm's SET LOCAL hnsw.* lives until end of transaction, not end of
// function — 137 header), so the arms would describe a candidate space the
// live call never had.
func ArmRanksTx(ctx context.Context, q Querier, dec SelectorDecision, policy SelectorPolicy, embedding []float32, query, querySpaced string, scopes []string, category *string, tags []string, limit int, temporal string, queryOR string, visibleTypes []string, dampedTypes []string, dampedFactors []float64, categoriesExclude []string, typesExclude []string, grantedBlockIDs []string) ([]ArmRow, error) {
	args, err := rrfBaseArgs(embedding, query, querySpaced, scopes, category, tags, clampSearchLimit(limit),
		temporal, queryOR, visibleTypes, dampedTypes, dampedFactors, categoriesExclude, typesExclude, grantedBlockIDs)
	if err != nil {
		return nil, err
	}
	mode, scanTuples, exactCap := selectorSQLArgs(dec, policy)
	args = append(args, mode, scanTuples, exactCap)
	return queryRRFArms(ctx, q, args)
}

// SelectorSQLArgs is the exported, typed view of the Gen-15 selector arguments
// a decision maps onto (§4.6). It exists for the B-W2 debug seam, which has to
// REPORT the strategy a measured call actually ran with — the handler must not
// re-derive that mapping and risk describing something other than what SQL saw.
//
// nil means "SQL default" (the parameter was not passed), which is what the
// `any` form encodes internally. Clamping is idempotent and silent for an
// in-range policy; an out-of-range policy warns once more per measurement
// request, which is the honest price for not caching the mapping.
func SelectorSQLArgs(dec SelectorDecision, policy SelectorPolicy) (mode string, scanTuples, exactCap *int) {
	m, st, ec := selectorSQLArgs(dec, policy)
	if v, ok := st.(int); ok {
		scanTuples = &v
	}
	if v, ok := ec.(int); ok {
		exactCap = &v
	}
	return m, scanTuples, exactCap
}

// queryRRFArms runs one ctx_rrf_arms statement and scans its rows — the
// queryRRF sibling, same shape, different projection.
func queryRRFArms(ctx context.Context, q Querier, args []any) ([]ArmRow, error) {
	rows, err := q.Query(ctx, armsQuery, args...)
	if err != nil {
		return nil, fmt.Errorf("rrf: query ctx_rrf_arms: %w", err)
	}
	defer rows.Close()

	var out []ArmRow
	for rows.Next() {
		var r ArmRow
		if err := rows.Scan(&r.ID, &r.RankSemantic, &r.RankFTSDe, &r.RankFTSEn, &r.RankTrigram,
			&r.CosSim, &r.MassFactor, &r.TypeFactor, &r.TypeName); err != nil {
			return nil, fmt.Errorf("rrf: scan arm row: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rrf: arm rows iteration: %w", err)
	}
	return out, nil
}
