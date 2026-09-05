package recall

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Test seams for the W01-2 gates. ProbeWithGUCs lets gate (a) run a probe
// WITHOUT the GUC forcing to prove the plan assertion fails closed on the
// as-is planner state; BeginLegTx lets the READ-ONLY gate (§5.4) assert
// SQLSTATE 25006 on a write inside a leg transaction.

func ProbeWithGUCs(ctx context.Context, pool *pgxpool.Pool, spec ProbeSpec, exactSet, annSet []string) (ProbeResult, error) {
	return probeWithGUCs(ctx, pool, spec, exactSet, annSet)
}

func ExactGUCs(timeout time.Duration) []string { return exactGUCs(timeout) }
func BeginLegTx(ctx context.Context, pool *pgxpool.Pool, gucs []string) (pgx.Tx, error) {
	return beginLegTx(ctx, pool, gucs)
}

// ComputeRecall exposes the arithmetic for the unit tests (ties, n<k, eps).
func ComputeRecall(exact, ann []LegRow, k int, eps float64) (float64, int) {
	er := make([]legRow, len(exact))
	for i, r := range exact {
		er[i] = legRow(r)
	}
	ar := make([]legRow, len(ann))
	for i, r := range ann {
		ar[i] = legRow(r)
	}
	return computeRecall(er, ar, k, eps)
}

// LegRow mirrors the unexported legRow for test inputs.
type LegRow struct {
	ID   string
	Dist float64
}

func Percentile(xs []float64, p float64) float64 { return percentile(xs, p) }

const (
	ExactLegSQLForTest = exactLegSQL
	AnnLegSQLForTest   = annLegSQL
)
