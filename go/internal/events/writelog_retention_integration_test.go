//go:build integration

package events

// Issue #277: the write-log retention is a line of the six-hour janitor
// bundle. The probe drives runSixHourJanitor — the method the ticker runs —
// not the arm, so a retention that is built but never fired fails here.
//
//	go test -tags=integration ./internal/events/ -run TestSixHourJanitorWriteLogRetention -count=1 -v

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestSixHourJanitorWriteLogRetention(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	// Guard-shaped audit rows of three ages (no write path stamps a past
	// created_at, so the ages are set here).
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_write_log (decision, similarity, scope, block_title, block_category, metadata, created_at)
		SELECT 'clean', 0.4, 'private', 'r' || g, 'decisions', '{}'::jsonb, now() - make_interval(days => d)
		  FROM generate_series(1, 20) g, (VALUES (0), (5), (40)) a(d)`); err != nil {
		t.Fatalf("seed write log: %v", err)
	}
	count := func() (young, mid, old int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT
			count(*) FILTER (WHERE created_at > now() - interval '1 day'),
			count(*) FILTER (WHERE created_at BETWEEN now() - interval '6 days' AND now() - interval '4 days'),
			count(*) FILTER (WHERE created_at < now() - interval '30 days')
			FROM context_write_log`).Scan(&young, &mid, &old); err != nil {
			t.Fatalf("count: %v", err)
		}
		return
	}

	for _, tc := range []struct {
		days            int
		young, mid, old int
	}{
		{0, 20, 20, 20},  // the default: an upgrade deletes nothing
		{30, 20, 20, 0},  // past 30 days only
		{2, 20, 0, 0},    // past the floor
		{0, 20, 0, 0},    // back to 0: nothing more
	} {
		c := &config.Config{}
		c.Scheduler.WriteLogRetentionDays = tc.days
		dfScheduler(pool, c, nil).runSixHourJanitor(ctx)
		y, m, o := count()
		if y != tc.young || m != tc.mid || o != tc.old {
			t.Fatalf("after a janitor run with retention %d: %d/%d/%d rows (1 day / 5 days / 40 days old), want %d/%d/%d",
				tc.days, y, m, o, tc.young, tc.mid, tc.old)
		}
	}
}
