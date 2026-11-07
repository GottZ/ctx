package usageevents_test

// W06-18 S2 hardening (Fremd-Review S2 F7/P3): a failed INSERT releases the
// event's key, so a later note of the same key is written instead of being
// deduplicated away for the rest of the window — without a database: the
// Execer fails its first statement.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/usageevents"
	"github.com/jackc/pgx/v5/pgconn"
)

type flakyExecer struct {
	mu    sync.Mutex
	calls int
}

func (f *flakyExecer) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls == 1 {
		return pgconn.CommandTag{}, errors.New("connection reset")
	}
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}

func TestRecorderRetriesAfterAFailedInsert(t *testing.T) {
	db := &flakyExecer{}
	day := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	r := usageevents.NewRecorder(db, func() time.Time { return day.Add(10 * time.Hour) })
	e := usageevents.Event{Kind: usageevents.KindSoftLimit, ConsumerID: "c1", TenantID: "t1", Op: "chat",
		Dimension: "daily_tokens", Level: "consumer", WindowStart: day, Used: 900, Limit: 1000}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r.Note(e)
	_ = r.Flush(ctx)
	r.Note(e)
	if err := r.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	s := r.Stats()
	db.mu.Lock()
	calls := db.calls
	db.mu.Unlock()
	if calls != 2 || s.Failed != 1 || s.Written != 1 {
		t.Errorf("W06-18 gate (release after a failed INSERT): %d statements, stats %+v — want 2 statements, Failed 1, Written 1 (the second note retries)", calls, s)
	}
}
