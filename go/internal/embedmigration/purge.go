package embedmigration

import (
	"context"
	"errors"
	"fmt"
)

// ErrPurgeActiveMigration is returned by Purge while a non-terminal
// migration exists: purging embedding_next under a live worker would
// destroy in-flight work the statemachine still accounts for (design §4.10
// — purge is the "abort --purge" follow-up, an operator action AFTER the
// migration went terminal, never a concurrent one).
var ErrPurgeActiveMigration = errors.New("embedmigration: purge: a non-terminal migration exists — purge is only valid after abort")

// purgeDefaultBatchSize bounds one purge UPDATE. Column-wise nulling of
// embedding_next touches every leftover row (a 10M-touch in the worst case,
// §4.10) — batching keeps each transaction short so autovacuum and
// concurrent writers never sit behind one multi-minute row-lock avalanche.
const purgeDefaultBatchSize = 5000

// Purge nulls embedding_next/embed_model_next in batches until no leftover
// row remains and returns the total number of cleared rows (design §4.10
// point 3: the `migration purge` command Create's ErrRestEmbeddingNextData
// hint points to). Fail-closed: refuses while any non-terminal migration
// exists. Pass the POOL as q — each batch UPDATE is its own implicit
// transaction; running Purge inside one caller tx would defeat the batching
// (every batch's row locks would be held until the outer commit).
func Purge(ctx context.Context, q Querier, batchSize int) (int64, error) {
	if batchSize <= 0 {
		batchSize = purgeDefaultBatchSize
	}
	active, err := Active(ctx, q)
	if err != nil {
		return 0, err
	}
	if active != nil {
		return 0, fmt.Errorf("%w (id %s, status %s)", ErrPurgeActiveMigration, active.ID, active.Status)
	}

	var total int64
	for {
		tag, err := q.Exec(ctx,
			`UPDATE context_blocks
			 SET embedding_next = NULL, embed_model_next = NULL
			 WHERE id IN (SELECT id FROM context_blocks
			              WHERE embedding_next IS NOT NULL LIMIT $1)`,
			batchSize,
		)
		if err != nil {
			return total, fmt.Errorf("embedmigration: purge batch: %w", err)
		}
		n := tag.RowsAffected()
		total += n
		if n == 0 {
			return total, nil
		}
	}
}
