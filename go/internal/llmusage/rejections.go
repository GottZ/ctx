package llmusage

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/GottZ/ctx/internal/pgxdb"
)

// RejectionRow is one cell of the refusal ledger as the flusher hands it
// over: an hour, its dimensions and the counts since the last flush (cmd/ctxd
// adapts keyquota.RejectionDelta to it — this package stays a leaf).
type RejectionRow struct {
	// Bucket is the UTC hour of the refusals.
	Bucket time.Time
	// ConsumerID "" is NULL (a refusal before the model-key door accepted).
	ConsumerID, Op, Alias, Class, Target string
	Reason, Detail                       string
	N, WaitMsSum, WaitN                  int64
}

// flushRejectionsSQL is the one statement of a flush: every row of the
// batch as one additive upsert (design/06 §3.4: n = n + EXCLUDED.n,
// wait_ms_sum, wait_n). The conflict target is the unique index of 167,
// whose NULLS NOT DISTINCT makes the instance-wide rows (consumer_id NULL)
// conflict like any other.
const flushRejectionsSQL = `INSERT INTO context_llm_rejections_hourly AS t
       (bucket_start, consumer_id, op, model_alias, dispatch_class, target, reason, detail,
        n, wait_ms_sum, wait_n, updated_at)
SELECT u.b, NULLIF(u.c, '')::uuid, u.o, u.a, u.k, u.g, u.r, u.d, u.n, u.ws, u.wn, now()
  FROM unnest($1::timestamptz[], $2::text[], $3::text[], $4::text[], $5::text[], $6::text[],
              $7::text[], $8::text[], $9::bigint[], $10::bigint[], $11::bigint[])
       AS u(b, c, o, a, k, g, r, d, n, ws, wn)
ON CONFLICT (bucket_start, consumer_id, op, model_alias, dispatch_class, target, reason, detail)
DO UPDATE SET n           = t.n + EXCLUDED.n,
              wait_ms_sum = t.wait_ms_sum + EXCLUDED.wait_ms_sum,
              wait_n      = t.wait_n + EXCLUDED.wait_n,
              updated_at  = now()`

// FlushRejections writes rows to context_llm_rejections_hourly in ONE
// statement — the table's only writer (design/06 §4.2 "Rejection-Flusher").
// Rows of the same cell are summed first (an upsert cannot touch one row
// twice in a statement); rows without a refusal are skipped. No rows, no
// statement. The ledger already holds only counted reasons and UUID
// consumers (keyquota.Ledger.Note); the CHECK of 167 refuses anything else,
// and then the statement fails as a whole and returns the error — the
// caller decides what becomes of the counts (cmd/ctxd hands them back to
// the ledger).
func FlushRejections(ctx context.Context, q pgxdb.Execer, rows []RejectionRow) error {
	rows = mergeRejectionRows(rows)
	if len(rows) == 0 {
		return nil
	}
	n := len(rows)
	var (
		buckets                                   = make([]time.Time, n)
		consumers, ops, aliases, classes, targets = make([]string, n), make([]string, n), make([]string, n), make([]string, n), make([]string, n)
		reasons, details                          = make([]string, n), make([]string, n)
		counts, waitMs, waitN                     = make([]int64, n), make([]int64, n), make([]int64, n)
	)
	for i, r := range rows {
		buckets[i] = r.Bucket.UTC()
		consumers[i], ops[i], aliases[i], classes[i], targets[i] = r.ConsumerID, r.Op, r.Alias, r.Class, r.Target
		reasons[i], details[i] = r.Reason, r.Detail
		counts[i], waitMs[i], waitN[i] = r.N, r.WaitMsSum, r.WaitN
	}
	if _, err := q.Exec(ctx, flushRejectionsSQL,
		buckets, consumers, ops, aliases, classes, targets, reasons, details, counts, waitMs, waitN); err != nil {
		return fmt.Errorf("llmusage: flush %d rejection row(s): %w", n, err)
	}
	return nil
}

// mergeRejectionRows sums the rows of one cell and drops rows that count
// nothing; the result is ordered by cell.
func mergeRejectionRows(rows []RejectionRow) []RejectionRow {
	out := make([]RejectionRow, 0, len(rows))
	for _, r := range rows {
		if r.N <= 0 {
			continue
		}
		r.Bucket = r.Bucket.UTC()
		out = append(out, r)
	}
	slices.SortFunc(out, compareRejectionCell)
	merged := out[:0]
	for _, r := range out {
		if k := len(merged) - 1; k >= 0 && compareRejectionCell(merged[k], r) == 0 {
			merged[k].N += r.N
			merged[k].WaitMsSum += r.WaitMsSum
			merged[k].WaitN += r.WaitN
			continue
		}
		merged = append(merged, r)
	}
	return merged
}

func compareRejectionCell(a, b RejectionRow) int {
	return cmp.Or(a.Bucket.Compare(b.Bucket), cmp.Compare(a.ConsumerID, b.ConsumerID), cmp.Compare(a.Op, b.Op),
		cmp.Compare(a.Alias, b.Alias), cmp.Compare(a.Class, b.Class), cmp.Compare(a.Target, b.Target),
		cmp.Compare(a.Reason, b.Reason), cmp.Compare(a.Detail, b.Detail))
}
