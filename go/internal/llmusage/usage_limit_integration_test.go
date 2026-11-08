//go:build integration

// W06-16 S2 hardening (Fremd-Review S2 F1): one series answer holds at most
// MaxPoints points (series × buckets) — 21 consumers × 1 008 hourly buckets
// (42 days) are refused with ErrTooLarge, two of them are answered.
//
//	go test -tags=integration -p 1 -count=1 -run TestReadSeriesPointLimit -v ./internal/llmusage/
package llmusage_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/llmusage"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestReadSeriesPointLimit(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	_, ids := seedTenantConsumers(ctx, t, pool, "w0616-limit", 21)
	now := time.Now().UTC()
	from := now.Truncate(time.Hour).Add(-1007 * time.Hour)
	for _, id := range ids {
		usageRollupRow(ctx, t, pool, id, from, 1, 1, 1, 1)
	}
	cover, err := llmusage.ReadCover(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	q, err := llmusage.Align(llmusage.SeriesQuery{From: from, To: now, Bucket: llmusage.BucketHour, Group: llmusage.GroupConsumer, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := llmusage.ReadSeries(ctx, pool, q, cover); !errors.Is(err, llmusage.ErrTooLarge) {
		t.Errorf("W06-16 gate (point limit): 21 series × 1008 buckets gives %v, want ErrTooLarge (MaxPoints %d)", err, llmusage.MaxPoints)
	}
	q.ConsumerIDs = ids[:2]
	if res, err := llmusage.ReadSeries(ctx, pool, q, cover); err != nil || len(res.Series) != 2 {
		t.Errorf("W06-16 gate (point limit): two consumers give %d series, %v — want 2, nil", len(res.Series), err)
	}
}
