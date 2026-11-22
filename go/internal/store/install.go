package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/GottZ/ctx/internal/pgxdb"
)

// LoadDreamInstallDefault reads context_install.dream_default (migration 192,
// issue #492): the value dream.enabled takes on this database when
// CTX_DREAM_ENABLED is not set — true on a database whose first migration run
// installed it, false on one an upgrade reached. A missing row reads false,
// the built-in default: nothing in ctx deletes the row, and a database
// without it is no installation the migration recorded as new.
func LoadDreamInstallDefault(ctx context.Context, q pgxdb.Rower) (bool, error) {
	var on bool
	err := q.QueryRow(ctx, `SELECT dream_default FROM context_install WHERE singleton`).Scan(&on)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: load the installation's dream default: %w", err)
	}
	return on, nil
}
