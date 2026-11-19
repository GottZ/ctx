package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// UpsertBlockTxForTest runs UpsertBlock's write inside a transaction the test
// holds open (issue #281): the gate in package store_test needs a first create
// of an identity that is written but not yet committed while a second writer's
// pre-read runs, the interleaving UpsertBlock's own transaction cannot be
// paused in. Production never calls it.
func UpsertBlockTxForTest(ctx context.Context, tx pgx.Tx, category, title, content, scope string) (*Block, error) {
	return prepareUpsert(category, title, content, nil, nil, scope, false, SensitivityWrite{}, "").exec(ctx, tx)
}

// UpsertDerivedTxForTest is UpsertBlockTxForTest for a derived writer: the
// metadata, the sensitivity intent (the badge) and the type of the write. The
// provenance gate's race test needs a derivative's first create that is
// written but not yet committed while a second writer runs. Production never
// calls it.
func UpsertDerivedTxForTest(ctx context.Context, tx pgx.Tx, category, title, content string, metadata map[string]any, scope string, sens SensitivityWrite, typeName string) (*Block, error) {
	return prepareUpsert(category, title, content, nil, metadata, scope, true, sens, typeName).exec(ctx, tx)
}
